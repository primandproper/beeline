// Package query is the read path (§9). It resolves an origin/destination coordinate
// pair to a cached scalar estimate in a single keyed lookup, applying the accuracy
// rules the design calls out: same-cell correction, stale-while-revalidate, and
// demand-fill on a miss. It never returns route geometry — scalars only (§2).
package query

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/geo"

	"github.com/primandproper/platform-go/v4/observability/logging"
)

// AreaRouter resolves a coordinate to the enabled service area that contains it. The
// control-plane Coordinator implements it; the read path consults it per query to pick
// which area partition (and resolution) a lookup keys against.
type AreaRouter interface {
	Locate(p beeline.LatLng) (beeline.RoutedArea, bool)
}

// Source describes how an estimate was produced, for observability on the read path.
type Source string

const (
	// SourceCache means the value came from the store (a precomputed hit).
	SourceCache Source = "cache"
	// SourceSameCell means origin and destination share a cell and the value was
	// corrected with a direct engine call on the true coordinates (§9).
	SourceSameCell Source = "same-cell"
	// SourceDemand means the pair was missing and computed synchronously on read.
	SourceDemand Source = "demand"
)

// Result is the answer returned to a caller.
type Result struct {
	ComputedAt time.Time
	Source     Source
	Estimate   beeline.Estimate
	Stale      bool
}

// Handler answers estimate queries against the store, index, and engine. It consults
// an AreaRouter per query to decide which service area (and resolution) a coordinate
// belongs to, so multiple areas at different resolutions share one read path.
type Handler struct {
	store     beeline.Store
	index     beeline.FreshnessIndex
	engine    beeline.RoutingEngine
	router    AreaRouter
	logger    logging.Logger
	targetTTL time.Duration
}

// NewHandler builds a read-path handler. A nil logger is replaced with a noop.
func NewHandler(store beeline.Store, index beeline.FreshnessIndex, engine beeline.RoutingEngine, router AreaRouter, logger logging.Logger, targetTTL time.Duration) *Handler {
	return &Handler{
		store:     store,
		index:     index,
		engine:    engine,
		router:    router,
		logger:    logging.EnsureLogger(logger),
		targetTTL: targetTTL,
	}
}

// Estimate answers a single origin→destination query for a profile. It routes by the
// origin: if the origin falls in an enabled area, the lookup keys against that area's
// partition and resolution (cache hit, same-cell correction, or demand-fill). A
// coordinate outside every enabled area is still answered — computed directly on the
// true endpoints — but not cached or bumped, since it belongs to no area partition.
func (h *Handler) Estimate(ctx context.Context, origin, dest beeline.LatLng, profile beeline.Profile) (Result, error) {
	routed, ok := h.router.Locate(origin)
	if !ok {
		// Outside every enabled area: answer directly, cache nothing.
		return h.compute(ctx, origin, dest, profile, SourceDemand)
	}

	resolution := routed.Resolution

	originCell, err := beeline.CellAt(origin, resolution)
	if err != nil {
		return Result{}, err
	}

	destCell, err := beeline.CellAt(dest, resolution)
	if err != nil {
		return Result{}, err
	}

	// Same-cell collapse: center-to-center distance is ~0, wrong for a real trip.
	// Compute directly on the true coordinates instead of trusting the cache (§9).
	if originCell == destCell {
		return h.compute(ctx, origin, dest, profile, SourceSameCell)
	}

	key := beeline.PairKey{Area: routed.ID, Origin: originCell, Dest: destCell, Profile: profile, Res: resolution}

	got, err := h.store.BatchGet(ctx, []beeline.PairKey{key})
	if err != nil {
		return Result{}, err
	}

	if len(got) > 0 && got[0] != nil {
		stored := got[0]
		stale := time.Since(stored.ComputedAt) >= h.targetTTL
		if stale {
			// Return the stale value immediately; bump its refresh priority so the
			// budget flows to pairs people actually query (§3). A bump failure is
			// non-fatal — the value is still correct — so log and carry on.
			if bumpErr := h.index.Bump(ctx, []beeline.PairKey{key}); bumpErr != nil {
				h.logger.Error("bumping stale pair", bumpErr)
			}
		} else if accessErr := h.index.Access(ctx, []beeline.PairKey{key}); accessErr != nil {
			// A fresh hit is still an access: record it so an actively-queried pair
			// survives the demand-decay sweep. Non-fatal — the value is still correct.
			h.logger.Error("recording fresh-hit access", accessErr)
		}

		return Result{
			Estimate:   stored.Estimate,
			ComputedAt: stored.ComputedAt,
			Stale:      stale,
			Source:     SourceCache,
		}, nil
	}

	// Miss: compute on the true endpoints now. Cache it only if the destination falls
	// within the area's travel bound; beyond the bound the pair is answered but never
	// tracked or stored, the same treatment as an out-of-area coordinate (§ warm-set).
	res, err := h.compute(ctx, origin, dest, profile, SourceDemand)
	if err != nil {
		return Result{}, err
	}

	if !withinBound(routed.MaxRadiusMeters, origin, dest) {
		return res, nil
	}

	if putErr := h.store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: res.Estimate, ComputedAt: res.ComputedAt},
	}}); putErr != nil {
		h.logger.Error("caching demand-filled estimate", putErr)
	}
	// Track the pair before marking it computed: MarkComputed ignores unknown keys, so
	// a demand-filled pair in a lazy/hybrid-tail area must be added to the working set
	// (as an unpinned demand entry) first, or it would be stored but never refreshed.
	if accessErr := h.index.Access(ctx, []beeline.PairKey{key}); accessErr != nil {
		h.logger.Error("tracking demand-filled estimate", accessErr)
	}
	if markErr := h.index.MarkComputed(ctx, []beeline.PairKey{key}, res.ComputedAt); markErr != nil {
		h.logger.Error("marking demand-filled estimate computed", markErr)
	}

	return res, nil
}

// withinBound reports whether a demand-fill to dest falls inside the area's travel
// bound and may therefore be cached. A maxRadiusMeters of 0 is the full-mesh sentinel:
// unbounded, so every in-area query is cacheable. The distance is measured on the true
// query coordinates with the same great-circle metric the tessellator uses to size the
// bound, so the read path and the precompute agree on what "within radius" means.
func withinBound(maxRadiusMeters float64, origin, dest beeline.LatLng) bool {
	if maxRadiusMeters == 0 {
		return true
	}

	return geo.Haversine(origin, dest) <= maxRadiusMeters
}

// compute runs a 1×1 engine call on the true coordinates and packages the result.
func (h *Handler) compute(ctx context.Context, origin, dest beeline.LatLng, profile beeline.Profile, source Source) (Result, error) {
	resp, err := h.engine.Table(ctx, beeline.TableRequest{
		Sources:      []beeline.LatLng{origin},
		Destinations: []beeline.LatLng{dest},
		Profile:      profile,
		Want:         beeline.AnnotateDuration | beeline.AnnotateDistance,
	})
	if err != nil {
		return Result{}, err
	}

	est := beeline.Estimate{}
	if len(resp.Duration) > 0 && len(resp.Duration[0]) > 0 {
		est.Duration = resp.Duration[0][0]
	}
	if len(resp.Distance) > 0 && len(resp.Distance[0]) > 0 {
		est.Distance = resp.Distance[0][0]
	}

	return Result{
		Estimate:   est,
		ComputedAt: time.Now(),
		Stale:      false,
		Source:     source,
	}, nil
}
