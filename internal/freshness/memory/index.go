// Package memory is an in-memory beeline.FreshnessIndex: the leased-queue
// coordination from §8 (Claim with a visibility timeout, idempotent
// MarkComputed) plus the freshness contract from §3 (debt, oldest age, achieved
// vs required throughput), all in process memory.
//
// It is a **test double, not a deployable backend.** Scheduling is exactly the
// state that cannot live per-head — two heads with private queues would lease the
// same pair to two followers — so the CLI always uses
// internal/freshness/postgres. This implementation exists because the packages
// that consume the index (control, httpapi, query, refresh) need to schedule
// against something without a database, and because its injected clock lets tests
// run inside testing/synctest bubbles, which no SQL backend can offer.
//
// It is pinned to the real index by internal/freshness/freshnesstest, which both
// implementations run.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/primitives-go/v2/clock"
)

// entry is the per-pair scheduling state.
type entry struct {
	computedAt time.Time // zero value means never computed
	leaseUntil time.Time // a key is claimable when now >= leaseUntil
	lastAccess time.Time // last time a query touched this pair (Access/Bump); drives decay
	bumped     bool      // demand-driven priority (stale-while-revalidate)
	pinned     bool      // eager-core pair; never evicted by the demand-decay sweep
}

// baseline is the per-area achieved-throughput baseline: how many refreshes have
// landed for this area and since when. It is reset each time the area is (re)seeded
// so the console's load progress for a freshly enabled area reads from zero, the
// per-area analog of the old global reseed behavior.
type baseline struct {
	started       time.Time
	computedTotal int64
}

// areaFreshness is one area's freshness contract, configured per area by the control
// plane. Zero values fall back to the index-wide defaults (the New targetTTL, and the
// lease passed to Claim), so an area with no override behaves as before.
type areaFreshness struct {
	targetTTL time.Duration
	lease     time.Duration
}

// Index tracks staleness for every pair in the working set and hands the stalest
// due pairs to workers under a lease.
type Index struct {
	started       time.Time
	clock         clock.Clock
	entries       map[beeline.PairKey]*entry
	baselines     map[beeline.AreaID]*baseline
	areaFresh     map[beeline.AreaID]areaFreshness
	targetTTL     time.Duration
	computedTotal int64
	mu            sync.Mutex
}

// New builds an index with the given target TTL. Pass nil for the wall clock;
// tests run it under testing/synctest, where the wall clock reads bubble time.
func New(targetTTL time.Duration, clk clock.Clock) *Index {
	if clk == nil {
		clk = clock.NewClock()
	}

	return &Index{
		clock:     clk,
		entries:   make(map[beeline.PairKey]*entry),
		baselines: make(map[beeline.AreaID]*baseline),
		areaFresh: make(map[beeline.AreaID]areaFreshness),
		targetTTL: targetTTL,
		started:   clk.Now(),
	}
}

// SetAreaFreshness records an area's per-area freshness contract: the target TTL used
// to decide staleness for its pairs and the lease duration applied when its pairs are
// claimed. A non-positive value for either falls back to the index-wide default. The
// control plane calls it on enable/seed/convergence; Unseed drops it.
func (i *Index) SetAreaFreshness(_ context.Context, area beeline.AreaID, targetTTL, lease time.Duration) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	i.areaFresh[area] = areaFreshness{targetTTL: targetTTL, lease: lease}

	return nil
}

// ttlFor returns the target TTL governing an area's pairs: its per-area override when
// set, else the index-wide default. Callers hold i.mu.
func (i *Index) ttlFor(area beeline.AreaID) time.Duration {
	if f, ok := i.areaFresh[area]; ok && f.targetTTL > 0 {
		return f.targetTTL
	}

	return i.targetTTL
}

// leaseFor returns the lease duration for an area's claims: its per-area override when
// set, else fallback (the value the worker passed to Claim). Callers hold i.mu.
func (i *Index) leaseFor(area beeline.AreaID, fallback time.Duration) time.Duration {
	if f, ok := i.areaFresh[area]; ok && f.lease > 0 {
		return f.lease
	}

	return fallback
}

// Seed adds keys to the working set as never-computed (maximally stale) and pinned,
// so the eager core they represent is never evicted by the demand-decay sweep.
// Existing keys are left untouched so re-seeding is idempotent (a demand pair that was
// later promoted into the seed set keeps its state). The achieved-throughput baseline
// for every area present in the batch is reset to now, so a freshly enabled (or
// re-converged) area's load progress reads from zero.
func (i *Index) Seed(_ context.Context, keys []beeline.PairKey) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()
	for pos := range keys {
		if _, ok := i.entries[keys[pos]]; !ok {
			i.entries[keys[pos]] = &entry{pinned: true}
		}
		i.baselines[keys[pos].Area] = &baseline{started: now}
	}

	return nil
}

// Access records that a pair was queried (demand-fill or a fresh cache hit): it adds
// unknown keys to the working set as unpinned demand entries and stamps every key's
// last-access time, so actively-queried pairs stay alive against Sweep. Unlike Bump it
// does not raise refresh priority — a fresh-but-queried pair should not be re-refreshed
// early. It never resets a per-area baseline, so demand traffic does not disturb the
// area's throughput accounting.
func (i *Index) Access(_ context.Context, keys []beeline.PairKey) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()
	for pos := range keys {
		e, ok := i.entries[keys[pos]]
		if !ok {
			e = &entry{}
			i.entries[keys[pos]] = e
		}
		e.lastAccess = now
	}

	return nil
}

// Unseed removes every pair belonging to one service area and drops its throughput
// baseline. The control plane calls it when an area is disabled (or before
// re-seeding on a geometry change), leaving other areas' working sets untouched.
func (i *Index) Unseed(_ context.Context, area beeline.AreaID) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	for k := range i.entries {
		if k.Area == area {
			delete(i.entries, k)
		}
	}
	delete(i.baselines, area)
	delete(i.areaFresh, area)

	return nil
}

// SweepArea evicts one area's cold demand pairs: every unpinned entry whose last
// access predates cutoff and that is not currently leased (being refreshed) is removed
// from the working set, and its key is returned so the caller can drop it from the hot
// store too. Pinned (eager-core) pairs are never swept. This is the mechanism behind
// per-area demand decay: cost tracks real usage instead of ratcheting up forever.
func (i *Index) SweepArea(_ context.Context, area beeline.AreaID, cutoff time.Time) ([]beeline.PairKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()

	var removed []beeline.PairKey
	for k, e := range i.entries {
		if k.Area != area || e.pinned {
			continue
		}
		if now.Before(e.leaseUntil) {
			continue // a worker holds this pair; let the refresh finish
		}
		if e.lastAccess.Before(cutoff) {
			removed = append(removed, k)
			delete(i.entries, k)
		}
	}

	return removed, nil
}

// CellStates rolls the working set up per origin cell for the progress map: for
// each origin, how many outgoing pairs exist and how many are currently fresh
// (computed and within the target TTL), plus the oldest age among its computed
// pairs. The result is unordered.
func (i *Index) CellStates(_ context.Context) ([]beeline.CellState, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()

	type rollup struct {
		total     int
		fresh     int
		oldestAge time.Duration
	}

	byOrigin := make(map[beeline.H3Cell]*rollup)
	for key, e := range i.entries {
		r, ok := byOrigin[key.Origin]
		if !ok {
			r = &rollup{}
			byOrigin[key.Origin] = r
		}

		r.total++
		if e.computedAt.IsZero() {
			continue
		}

		age := now.Sub(e.computedAt)
		if age > r.oldestAge {
			r.oldestAge = age
		}
		if age < i.ttlFor(key.Area) {
			r.fresh++
		}
	}

	states := make([]beeline.CellState, 0, len(byOrigin))
	for origin, r := range byOrigin {
		states = append(states, beeline.CellState{
			Origin:           origin,
			Total:            r.total,
			Fresh:            r.fresh,
			OldestAgeSeconds: r.oldestAge.Seconds(),
		})
	}

	return states, nil
}

// CellStatesForArea is CellStates scoped to one service area: only pairs whose
// Area matches are rolled up, so the console can paint one area's progress map at a
// time on the shared index. The result is unordered.
func (i *Index) CellStatesForArea(_ context.Context, area beeline.AreaID) ([]beeline.CellState, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()

	type rollup struct {
		total     int
		fresh     int
		oldestAge time.Duration
	}

	ttl := i.ttlFor(area)

	byOrigin := make(map[beeline.H3Cell]*rollup)
	for key, e := range i.entries {
		if key.Area != area {
			continue
		}

		r, ok := byOrigin[key.Origin]
		if !ok {
			r = &rollup{}
			byOrigin[key.Origin] = r
		}

		r.total++
		if e.computedAt.IsZero() {
			continue
		}

		age := now.Sub(e.computedAt)
		if age > r.oldestAge {
			r.oldestAge = age
		}
		if age < ttl {
			r.fresh++
		}
	}

	states := make([]beeline.CellState, 0, len(byOrigin))
	for origin, r := range byOrigin {
		states = append(states, beeline.CellState{
			Origin:           origin,
			Total:            r.total,
			Fresh:            r.fresh,
			OldestAgeSeconds: r.oldestAge.Seconds(),
		})
	}

	return states, nil
}

// due reports whether a pair should be refreshed now: not currently leased, and
// either never computed, demand-bumped, or older than its area's target TTL.
func (i *Index) due(e *entry, now time.Time, area beeline.AreaID) bool {
	if now.Before(e.leaseUntil) {
		return false
	}
	if e.computedAt.IsZero() || e.bumped {
		return true
	}

	return now.Sub(e.computedAt) >= i.ttlFor(area)
}

// claimCandidate pairs a due key with its entry while Claim ranks it.
type claimCandidate struct {
	e   *entry
	key beeline.PairKey
}

// claimBefore reports whether a should be refreshed before b: demand-bumped keys
// first, then never-computed, then oldest-computed. Equal-priority keys are ordered
// by origin (then dest, for determinism) so a claimed batch clusters into few dense
// origin-centric 1×K table requests (§6) instead of scattering across as many
// origins as pairs — against a network-bound engine that is the difference between
// one round-trip per ~K pairs and one per pair. MarkComputed stamps a whole batch
// with one timestamp, so batches recomputed later still tie on age and keep
// clustering by origin.
func claimBefore(a, b claimCandidate) bool {
	if a.e.bumped != b.e.bumped {
		return a.e.bumped
	}
	na, nb := a.e.computedAt.IsZero(), b.e.computedAt.IsZero()
	if na != nb {
		return na
	}
	if !a.e.computedAt.Equal(b.e.computedAt) {
		return a.e.computedAt.Before(b.e.computedAt)
	}
	if a.key.Origin != b.key.Origin {
		return a.key.Origin < b.key.Origin
	}

	return a.key.Dest < b.key.Dest
}

// Claim leases up to limit of the stalest due keys (claimBefore order) for the
// given visibility timeout. Selection is a bounded worst-at-root heap — O(n·log
// limit) over the due set, not a full O(n·log n) sort — because Claim holds the
// index mutex and is called concurrently by every worker (local and follower):
// against a large cold working set, sorting millions of candidates per claim
// would serialize the whole pool behind the sort.
func (i *Index) Claim(_ context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()

	// top holds the best `limit` candidates seen so far as a binary heap whose
	// root is the WORST of them, so each further candidate is one comparison
	// against the root to accept or reject. limit <= 0 keeps everything.
	var top []claimCandidate
	if limit > 0 {
		top = make([]claimCandidate, 0, limit)
	}

	// siftDown restores the worst-at-root property from position pos.
	siftDown := func(pos int) {
		for {
			worst := pos
			if l := 2*pos + 1; l < len(top) && claimBefore(top[worst], top[l]) {
				worst = l
			}
			if r := 2*pos + 2; r < len(top) && claimBefore(top[worst], top[r]) {
				worst = r
			}
			if worst == pos {
				return
			}
			top[pos], top[worst] = top[worst], top[pos]
			pos = worst
		}
	}

	for k, e := range i.entries {
		if !i.due(e, now, k.Area) {
			continue
		}
		c := claimCandidate{key: k, e: e}
		switch {
		case limit <= 0 || len(top) < limit:
			top = append(top, c)
			if limit > 0 && len(top) == limit {
				for pos := len(top)/2 - 1; pos >= 0; pos-- {
					siftDown(pos)
				}
			}
		case claimBefore(c, top[0]):
			top[0] = c
			siftDown(0)
		}
	}

	// Order the survivors for the caller: the batch arrives origin-clustered.
	sort.Slice(top, func(a, b int) bool { return claimBefore(top[a], top[b]) })

	claimed := make([]beeline.PairKey, len(top))
	for idx := range top {
		top[idx].e.leaseUntil = now.Add(i.leaseFor(top[idx].key.Area, lease))
		claimed[idx] = top[idx].key
	}

	return claimed, nil
}

// MarkComputed records a successful refresh: stamps ComputedAt, releases the lease,
// clears the demand bump, and advances the throughput counter. Unknown keys are
// ignored (a lease may have expired and the pair been invalidated meanwhile).
func (i *Index) MarkComputed(_ context.Context, keys []beeline.PairKey, at time.Time) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	for pos := range keys {
		e, ok := i.entries[keys[pos]]
		if !ok {
			continue
		}

		e.computedAt = at
		e.leaseUntil = time.Time{}
		e.bumped = false
		i.computedTotal++
		// A lazy area is never Seed-ed with keys, so it has no baseline until its first
		// demand-filled pair is computed; establish one now so its achieved-throughput
		// accounting starts here rather than staying blank forever.
		b, has := i.baselines[keys[pos].Area]
		if !has {
			b = &baseline{started: i.clock.Now()}
			i.baselines[keys[pos].Area] = b
		}
		b.computedTotal++
	}

	return nil
}

// Bump raises refresh priority for demand-driven pairs (stale-while-revalidate).
// Unknown keys are added to the working set so a queried-but-unseeded pair starts
// getting refreshed. A bump is also an access, so it stamps last-access time (keeping
// the pair alive against the decay sweep); newly created keys are unpinned demand
// entries.
func (i *Index) Bump(_ context.Context, keys []beeline.PairKey) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()
	for pos := range keys {
		e, ok := i.entries[keys[pos]]
		if !ok {
			e = &entry{}
			i.entries[keys[pos]] = e
		}
		e.bumped = true
		e.lastAccess = now
	}

	return nil
}

// Invalidate re-enqueues every computed pair the selector matches by clearing its
// ComputedAt (so it sorts to the front of the queue) and its lease (so it is
// claimable at once, even if a worker holds it). Cached estimates are untouched;
// only the schedule changes. It returns how many pairs were selected.
func (i *Index) Invalidate(_ context.Context, sel beeline.Selector) (int, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	var n int
	for k, e := range i.entries {
		if !selects(sel, k, e.computedAt) {
			continue
		}
		e.computedAt = time.Time{}
		e.leaseUntil = time.Time{}
		n++
	}

	return n, nil
}

// selects reports whether one pair falls inside a Selector: computed at all, old
// enough if an age bound is set, and inside every scope the selector names.
func selects(sel beeline.Selector, k beeline.PairKey, computedAt time.Time) bool {
	switch {
	case computedAt.IsZero(): // never computed: already maximally stale
		return false
	case !sel.OlderThan.IsZero() && !computedAt.Before(sel.OlderThan):
		return false
	case sel.Area != 0 && k.Area != sel.Area:
		return false
	case sel.Res != nil && k.Res != *sel.Res:
		return false
	case sel.Profile != "" && k.Profile != sel.Profile:
		return false
	default:
		return true
	}
}

// Debt reports the freshness contract signals: working-set size, count of pairs
// older than the TTL (never-computed pairs count as debt), p100 age among computed
// pairs, and the required vs achieved refresh throughput.
func (i *Index) Debt(_ context.Context) (beeline.DebtStats, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()

	var (
		debt      int
		oldestAge time.Duration
		required  float64 // entries/sec needed, summed per-pair since TTLs vary by area
	)

	for k, e := range i.entries {
		if ttl := i.ttlFor(k.Area).Seconds(); ttl > 0 {
			required += 1 / ttl
		}

		if e.computedAt.IsZero() {
			debt++
			continue
		}

		age := now.Sub(e.computedAt)
		if age > oldestAge {
			oldestAge = age
		}
		if age >= i.ttlFor(k.Area) {
			debt++
		}
	}

	workingSet := len(i.entries)

	var achieved float64
	if elapsed := now.Sub(i.started).Seconds(); elapsed > 0 {
		achieved = float64(i.computedTotal) / elapsed
	}

	return beeline.DebtStats{
		WorkingSet:         workingSet,
		Debt:               debt,
		OldestAgeSeconds:   oldestAge.Seconds(),
		RequiredThroughput: required,
		AchievedThroughput: achieved,
	}, nil
}

// DebtForArea reports the freshness contract signals for a single service area:
// working-set size, debt, p100 age, and required vs achieved throughput, all scoped
// to that area. Achieved throughput is measured from the area's own baseline (set
// when it was enabled), so a newly enabled area's progress reads from zero
// independent of other areas.
func (i *Index) DebtForArea(_ context.Context, area beeline.AreaID) (beeline.DebtStats, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.clock.Now()

	var (
		debt       int
		workingSet int
		oldestAge  time.Duration
	)

	areaTTL := i.ttlFor(area)

	for k, e := range i.entries {
		if k.Area != area {
			continue
		}

		workingSet++
		if e.computedAt.IsZero() {
			debt++
			continue
		}

		age := now.Sub(e.computedAt)
		if age > oldestAge {
			oldestAge = age
		}
		if age >= areaTTL {
			debt++
		}
	}

	var required float64
	if ttl := areaTTL.Seconds(); ttl > 0 {
		required = float64(workingSet) / ttl
	}

	var achieved float64
	if b, ok := i.baselines[area]; ok {
		if elapsed := now.Sub(b.started).Seconds(); elapsed > 0 {
			achieved = float64(b.computedTotal) / elapsed
		}
	}

	return beeline.DebtStats{
		WorkingSet:         workingSet,
		Debt:               debt,
		OldestAgeSeconds:   oldestAge.Seconds(),
		RequiredThroughput: required,
		AchievedThroughput: achieved,
	}, nil
}
