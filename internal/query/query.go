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
	"github.com/primandproper/beeline/internal/telemetry"

	"github.com/primandproper/platform-go/v7/observability/logging"
)

// AreaRouter resolves a coordinate to the enabled service area that contains it. The
// control-plane Coordinator implements it; the read path consults it per query to pick
// which area partition (and resolution) a lookup keys against.
type AreaRouter interface {
	Locate(p beeline.LatLng) (beeline.RoutedArea, bool)
}

// FetchRecorder receives one event per in-area estimate the read path serves — the
// demand signal an organization can export to train a model on which pairs get
// queried when. Implementations must never block: the read path calls it inline.
// Out-of-area queries are not recorded (they have no area, no resolution, and cells
// are the only location the telemetry may carry); refresh recomputes never pass
// through here at all, so the record matches the queries-only access signal.
type FetchRecorder interface {
	RecordFetch(ev *telemetry.FetchEvent)
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

// cellClass is how a single origin→destination pair resolves against its area,
// deciding whether it is a keyed cache lookup or a live compute. It is the shared
// vocabulary of the single-pair (Estimate) and batch (Table) read paths.
type cellClass int

const (
	// classCacheLookup: distinct origin/dest cells in an area — build a PairKey and
	// consult the store (a hit, or a demand-fill on a miss).
	classCacheLookup cellClass = iota
	// classSameCell: origin and dest share a cell — never cached; corrected live on
	// the true coordinates (§9).
	classSameCell
	// classOutOfArea: origin belongs to no enabled area — never cached; answered live
	// through the default engine.
	classOutOfArea
)

// pairPlan is the classification of one pair. key is meaningful for
// classCacheLookup and classSameCell (where Origin == Dest == the shared cell, so
// telemetry can identify the fetch); it is zero for classOutOfArea.
type pairPlan struct {
	key   beeline.PairKey
	class cellClass
}

// Handler answers estimate queries against the store, index, and routing engines. It
// consults an AreaRouter per query to decide which service area (and resolution) a
// coordinate belongs to, so multiple areas at different resolutions share one read
// path, and an EngineResolver to pick that area's routing engine, so areas served by
// different providers compute their misses through the right one.
type Handler struct {
	store    beeline.Store
	index    beeline.FreshnessIndex
	resolver beeline.EngineResolver
	router   AreaRouter
	recorder FetchRecorder
	logger   logging.Logger
}

// NewHandler builds a read-path handler. A nil logger is replaced with a noop; a nil
// recorder disables telemetry entirely. Staleness is judged per query against the
// containing area's own target TTL (from the router), so the read path carries no
// global TTL.
func NewHandler(store beeline.Store, index beeline.FreshnessIndex, resolver beeline.EngineResolver, router AreaRouter, logger logging.Logger, recorder FetchRecorder) *Handler {
	return &Handler{
		store:    store,
		index:    index,
		resolver: resolver,
		router:   router,
		recorder: recorder,
		logger:   logging.EnsureLogger(logger),
	}
}

// record tees one served estimate to the telemetry recorder, if one is wired. The
// recorder contract is non-blocking, so this adds nothing observable to a query.
func (h *Handler) record(key beeline.PairKey, source Source, stale bool) {
	if h.recorder == nil {
		return
	}

	h.recorder.RecordFetch(&telemetry.FetchEvent{
		At:     time.Now(),
		Key:    key,
		Source: string(source),
		Stale:  stale,
	})
}

// Estimate answers a single origin→destination query for a profile. It routes by the
// origin: if the origin falls in an enabled area, the lookup keys against that area's
// partition and resolution (cache hit, same-cell correction, or demand-fill). A
// coordinate outside every enabled area is still answered — computed directly on the
// true endpoints — but not cached or bumped, since it belongs to no area partition.
func (h *Handler) Estimate(ctx context.Context, origin, dest beeline.LatLng, profile beeline.Profile) (Result, error) {
	routed, ok := h.router.Locate(origin)

	plan, err := h.classify(routed, ok, origin, dest, profile)
	if err != nil {
		return Result{}, err
	}

	switch plan.class {
	case classOutOfArea:
		// Outside every enabled area: answer directly (default engine), cache nothing.
		return h.compute(ctx, 0, origin, dest, profile, SourceDemand)
	case classSameCell:
		// Center-to-center distance is ~0, wrong for a real trip. Compute directly on
		// the true coordinates instead of trusting the cache (§9).
		res, computeErr := h.compute(ctx, routed.ID, origin, dest, profile, SourceSameCell)
		if computeErr == nil {
			h.record(plan.key, SourceSameCell, false)
		}

		return res, computeErr
	}

	got, err := h.store.BatchGet(ctx, []beeline.PairKey{plan.key})
	if err != nil {
		return Result{}, err
	}

	if len(got) > 0 && got[0] != nil {
		res, stale := cacheHit(got[0], routed.TargetTTL)
		h.record(plan.key, SourceCache, stale)
		if stale {
			// Return the stale value immediately; bump its refresh priority so the
			// budget flows to pairs people actually query (§3). A bump failure is
			// non-fatal — the value is still correct — so log and carry on.
			if bumpErr := h.index.Bump(ctx, []beeline.PairKey{plan.key}); bumpErr != nil {
				h.logger.Error("bumping stale pair", bumpErr)
			}
		} else if accessErr := h.index.Access(ctx, []beeline.PairKey{plan.key}); accessErr != nil {
			// A fresh hit is still an access: record it so an actively-queried pair
			// survives the demand-decay sweep. Non-fatal — the value is still correct.
			h.logger.Error("recording fresh-hit access", accessErr)
		}

		return res, nil
	}

	// Miss: compute on the true endpoints now. Cache it only if the destination falls
	// within the area's travel bound; beyond the bound the pair is answered but never
	// tracked or stored, the same treatment as an out-of-area coordinate (§ warm-set).
	res, err := h.compute(ctx, routed.ID, origin, dest, profile, SourceDemand)
	if err != nil {
		return Result{}, err
	}

	// Beyond-bound demand is still recorded: it is real demand, and the exporter can
	// judge distance offline from the cells — only caching is bounded.
	h.record(plan.key, SourceDemand, false)

	if !withinBound(routed.ReadLayer().MaxRadiusMeters, origin, dest) {
		return res, nil
	}

	h.commitFills(ctx, []beeline.Entry{{
		Key:    plan.key,
		Stored: beeline.Stored{Estimate: res.Estimate, ComputedAt: res.ComputedAt},
	}}, res.ComputedAt)

	return res, nil
}

// classify resolves a single pair against its (already located) area, without
// touching the store: skipped callers aside, every read-path decision starts here so
// the single-pair and batch paths cannot drift on what "same cell" or "out of area"
// means. When located is false the area is ignored and the pair is out-of-area.
func (h *Handler) classify(routed beeline.RoutedArea, located bool, origin, dest beeline.LatLng, profile beeline.Profile) (pairPlan, error) {
	if !located {
		return pairPlan{class: classOutOfArea}, nil
	}

	// Reads deliberately key at the area's finest layer only; distance-based
	// fallthrough across the coarser layers in routed.Layers is not implemented yet.
	layer := routed.ReadLayer()

	originCell, err := beeline.CellAt(origin, layer.Resolution)
	if err != nil {
		return pairPlan{}, err
	}

	destCell, err := beeline.CellAt(dest, layer.Resolution)
	if err != nil {
		return pairPlan{}, err
	}

	key := beeline.PairKey{Area: routed.ID, Origin: originCell, Dest: destCell, Profile: profile, Res: layer.Resolution}

	if originCell == destCell {
		return pairPlan{class: classSameCell, key: key}, nil
	}

	return pairPlan{class: classCacheLookup, key: key}, nil
}

// cacheHit packages a stored value into a cache Result and reports whether it is
// stale against the area's target TTL. Shared so Estimate and Table judge staleness
// identically.
func cacheHit(stored *beeline.Stored, targetTTL time.Duration) (Result, bool) {
	stale := time.Since(stored.ComputedAt) >= targetTTL

	return Result{
		Estimate:   stored.Estimate,
		ComputedAt: stored.ComputedAt,
		Stale:      stale,
		Source:     SourceCache,
	}, stale
}

// commitFills caches a batch of demand-filled pairs and tracks them in the freshness
// index, at the shared fill time. Every write is non-fatal: the caller already holds
// the correct value, so a store/index hiccup is logged, not returned. Access precedes
// MarkComputed for the same reason Estimate does it singly — MarkComputed ignores
// unknown keys, so a lazy/hybrid-tail pair must enter the working set first or it
// would be stored but never refreshed.
func (h *Handler) commitFills(ctx context.Context, entries []beeline.Entry, at time.Time) {
	if len(entries) == 0 {
		return
	}

	keys := make([]beeline.PairKey, len(entries))
	for i := range entries {
		keys[i] = entries[i].Key
	}

	if err := h.store.Put(ctx, entries); err != nil {
		h.logger.Error("caching demand-filled estimates", err)
	}
	if err := h.index.Access(ctx, keys); err != nil {
		h.logger.Error("tracking demand-filled estimates", err)
	}
	if err := h.index.MarkComputed(ctx, keys, at); err != nil {
		h.logger.Error("marking demand-filled estimates computed", err)
	}
}

// TableQuery is a sparse batch of origin→destination queries: the full grid of
// Sources × Destinations minus the cells named in Skip. It is the read-path analog
// of a routing engine's dense table, but keyed against the cache. Fill decides how
// misses are handled (see Table).
type TableQuery struct {
	Skip         map[[2]int]struct{} // grid cells (sourceIdx, destIdx) to omit entirely
	Profile      beeline.Profile
	Sources      []beeline.LatLng
	Destinations []beeline.LatLng
	Fill         bool // demand-fill misses through the engine, or leave them absent
}

// CellResult is the answer for one grid cell. Present is false for a skipped cell, and
// for a cell that needed a live compute (a miss, same-cell, or out-of-area pair) when
// Fill was not requested — the engine is never touched in that case.
type CellResult struct {
	ComputedAt time.Time
	Source     Source
	Estimate   beeline.Estimate
	Stale      bool
	Present    bool
}

// TableResult holds the dense grid of cell answers (Cells[sourceIdx][destIdx]) plus
// counters describing how each cell resolved, for observability.
type TableResult struct {
	Cells [][]CellResult

	Hits      int // cache hits
	Misses    int // cache lookups that missed
	Filled    int // misses that were computed on the engine (Fill only)
	Skipped   int // cells named in Skip
	SameCell  int // origin and dest shared a cell
	OutOfArea int // origin belonged to no enabled area
}

// Table answers a sparse batch of queries in one pass, the batching the single-pair
// Estimate cannot do: every cache lookup across the whole grid is served by a single
// store.BatchGet, and — when Fill is set — the misses (and same-cell/out-of-area
// cells) are computed with at most one dense 1×K engine call per source coordinate,
// grouping like the refresh pool (§6). Each cell obeys the same rules as Estimate:
// same-cell correction, stale-while-revalidate, and demand-fill only within the area
// bound. When Fill is false the engine is never called: misses and cells that would
// need a live compute come back absent (Present false), a pure cache read.
func (h *Handler) Table(ctx context.Context, q *TableQuery) (TableResult, error) {
	rows, cols := len(q.Sources), len(q.Destinations)

	result := TableResult{Cells: make([][]CellResult, rows)}
	for i := range result.Cells {
		result.Cells[i] = make([]CellResult, cols)
	}

	// Locate each source once: all of its cells share the same area and resolution.
	routed := make([]beeline.RoutedArea, rows)
	located := make([]bool, rows)
	for i := range q.Sources {
		routed[i], located[i] = h.router.Locate(q.Sources[i])
	}

	// todo is a cell awaiting a live compute in the fill pass. key is set for a cache
	// miss (classCacheLookup) — the sole class eligible to be cached afterwards — and
	// for a same-cell correction, whose key identifies the fetch to telemetry.
	type todo struct {
		key   beeline.PairKey
		j     int
		class cellClass
	}

	// lookup remembers where a batched cache key came from so its result lands back in
	// the right grid cell.
	type lookup struct {
		key  beeline.PairKey
		i, j int
	}

	var (
		keys    []beeline.PairKey
		lookups []lookup
		pending = make([][]todo, rows) // compute-needed cells, grouped by source index
	)

	// Pass 1: classify every non-skipped cell. Cache lookups are collected for one
	// batched read; same-cell and out-of-area cells go straight to the fill pass.
	for i := range rows {
		for j := range cols {
			if _, skip := q.Skip[[2]int{i, j}]; skip {
				result.Skipped++
				continue
			}

			plan, err := h.classify(routed[i], located[i], q.Sources[i], q.Destinations[j], q.Profile)
			if err != nil {
				return TableResult{}, err
			}

			switch plan.class {
			case classOutOfArea:
				result.OutOfArea++
				pending[i] = append(pending[i], todo{j: j, class: classOutOfArea})
			case classSameCell:
				result.SameCell++
				pending[i] = append(pending[i], todo{j: j, class: classSameCell, key: plan.key})
			case classCacheLookup:
				keys = append(keys, plan.key)
				lookups = append(lookups, lookup{i: i, j: j, key: plan.key})
			}
		}
	}

	// Pass 2: one batched cache read for every lookup across the whole grid. Hits fill
	// their cell now; misses drop into the fill pass. Bump/Access are batched too.
	if len(keys) > 0 {
		got, err := h.store.BatchGet(ctx, keys)
		if err != nil {
			return TableResult{}, err
		}

		var staleKeys, freshKeys []beeline.PairKey
		for idx := range lookups {
			lk := lookups[idx]
			if idx < len(got) && got[idx] != nil {
				res, stale := cacheHit(got[idx], routed[lk.i].TargetTTL)
				result.Hits++
				result.Cells[lk.i][lk.j] = cellFrom(res)
				h.record(lk.key, SourceCache, stale)
				if stale {
					staleKeys = append(staleKeys, lk.key)
				} else {
					freshKeys = append(freshKeys, lk.key)
				}
				continue
			}

			result.Misses++
			pending[lk.i] = append(pending[lk.i], todo{j: lk.j, class: classCacheLookup, key: lk.key})
		}

		if len(staleKeys) > 0 {
			if bumpErr := h.index.Bump(ctx, staleKeys); bumpErr != nil {
				h.logger.Error("bumping stale pairs", bumpErr)
			}
		}
		if len(freshKeys) > 0 {
			if accessErr := h.index.Access(ctx, freshKeys); accessErr != nil {
				h.logger.Error("recording fresh-hit accesses", accessErr)
			}
		}
	}

	if !q.Fill {
		return result, nil
	}

	// Pass 3: compute the misses. One dense 1×K engine call per source coordinate (its
	// true origin, every compute-needed dest), then cache the within-bound misses.
	now := time.Now()

	var fills []beeline.Entry
	for i := range rows {
		if len(pending[i]) == 0 {
			continue
		}

		area := beeline.AreaID(0)
		if located[i] {
			area = routed[i].ID
		}

		dests := make([]beeline.LatLng, len(pending[i]))
		for k := range pending[i] {
			dests[k] = q.Destinations[pending[i][k].j]
		}

		resp, err := h.resolver.EngineFor(area).Table(ctx, beeline.TableRequest{
			Sources:      []beeline.LatLng{q.Sources[i]},
			Destinations: dests,
			Profile:      q.Profile,
			Want:         beeline.AnnotateDuration | beeline.AnnotateDistance,
		})
		if err != nil {
			return TableResult{}, err
		}

		for k := range pending[i] {
			t := pending[i][k]
			est := beeline.Estimate{}
			if len(resp.Duration) > 0 && k < len(resp.Duration[0]) {
				est.Duration = resp.Duration[0][k]
			}
			if len(resp.Distance) > 0 && k < len(resp.Distance[0]) {
				est.Distance = resp.Distance[0][k]
			}

			source := SourceDemand
			if t.class == classSameCell {
				source = SourceSameCell
			}
			result.Cells[i][t.j] = CellResult{Estimate: est, ComputedAt: now, Source: source, Present: true}

			// Same-cell and miss cells are in-area fetches; out-of-area cells carry
			// no key and are not recorded, matching Estimate.
			if t.class != classOutOfArea {
				h.record(t.key, source, false)
			}

			// Only a genuine miss within the area bound is cached; same-cell and
			// out-of-area cells are answered but never stored, matching Estimate.
			if t.class == classCacheLookup {
				result.Filled++
				if withinBound(routed[i].ReadLayer().MaxRadiusMeters, q.Sources[i], q.Destinations[t.j]) {
					fills = append(fills, beeline.Entry{
						Key:    t.key,
						Stored: beeline.Stored{Estimate: est, ComputedAt: now},
					})
				}
			}
		}
	}

	h.commitFills(ctx, fills, now)

	return result, nil
}

// cellFrom projects a single-pair Result onto a grid CellResult (a present cell).
func cellFrom(r Result) CellResult {
	return CellResult{
		Estimate:   r.Estimate,
		ComputedAt: r.ComputedAt,
		Source:     r.Source,
		Stale:      r.Stale,
		Present:    true,
	}
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

// compute runs a 1×1 engine call on the true coordinates and packages the result. The
// area selects the routing engine: an in-area query uses its area's provider, while an
// out-of-area query (area 0) falls back to the default engine via the resolver.
func (h *Handler) compute(ctx context.Context, area beeline.AreaID, origin, dest beeline.LatLng, profile beeline.Profile, source Source) (Result, error) {
	resp, err := h.resolver.EngineFor(area).Table(ctx, beeline.TableRequest{
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
