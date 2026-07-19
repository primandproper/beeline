// Package query is the read path (§9). It resolves an origin/destination coordinate
// pair to a cached scalar estimate in a single keyed lookup, applying the accuracy
// rules the design calls out: same-cell correction, stale-while-revalidate, and
// demand-fill on a miss. It never returns route geometry — scalars only (§2).
package query

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v4/observability/logging"
)

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

// Handler answers estimate queries against the store, index, and engine. The
// resolution is held atomically because the control plane can swap the service
// area (and its H3 resolution) at runtime while reads are in flight.
type Handler struct {
	store      beeline.Store
	index      beeline.FreshnessIndex
	engine     beeline.RoutingEngine
	logger     logging.Logger
	resolution atomic.Int64
	targetTTL  time.Duration
}

// NewHandler builds a read-path handler. A nil logger is replaced with a noop.
func NewHandler(store beeline.Store, index beeline.FreshnessIndex, engine beeline.RoutingEngine, logger logging.Logger, resolution int, targetTTL time.Duration) *Handler {
	h := &Handler{
		store:     store,
		index:     index,
		engine:    engine,
		logger:    logging.EnsureLogger(logger),
		targetTTL: targetTTL,
	}
	h.resolution.Store(int64(resolution))

	return h
}

// SetResolution updates the resolution the read path keys against. Called by the
// control plane after a runtime re-tessellation so lookups target the new cells.
func (h *Handler) SetResolution(resolution int) {
	h.resolution.Store(int64(resolution))
}

// Estimate answers a single origin→destination query for a profile.
func (h *Handler) Estimate(ctx context.Context, origin, dest beeline.LatLng, profile beeline.Profile) (Result, error) {
	resolution := int(h.resolution.Load())

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

	key := beeline.PairKey{Origin: originCell, Dest: destCell, Profile: profile, Res: resolution}

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
		}

		return Result{
			Estimate:   stored.Estimate,
			ComputedAt: stored.ComputedAt,
			Stale:      stale,
			Source:     SourceCache,
		}, nil
	}

	// Miss: compute on the true endpoints now, cache it, and let the index track it.
	res, err := h.compute(ctx, origin, dest, profile, SourceDemand)
	if err != nil {
		return Result{}, err
	}

	if putErr := h.store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: res.Estimate, ComputedAt: res.ComputedAt},
	}}); putErr != nil {
		h.logger.Error("caching demand-filled estimate", putErr)
	}
	if markErr := h.index.MarkComputed(ctx, []beeline.PairKey{key}, res.ComputedAt); markErr != nil {
		h.logger.Error("marking demand-filled estimate computed", markErr)
	}

	return res, nil
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
