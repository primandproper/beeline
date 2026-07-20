// Package memory is an in-memory beeline.FreshnessIndex: the scheduling brain of
// the prototype. It implements the leased-queue coordination from §8 (Claim with a
// visibility timeout, idempotent MarkComputed) and surfaces the freshness contract
// from §3 (debt, oldest age, achieved vs required throughput). A Postgres index
// using SELECT … FOR UPDATE SKIP LOCKED implements the same interface for scale-out.
package memory

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
)

// entry is the per-pair scheduling state.
type entry struct {
	computedAt time.Time // zero value means never computed
	leaseUntil time.Time // a key is claimable when now >= leaseUntil
	bumped     bool      // demand-driven priority (stale-while-revalidate)
}

// baseline is the per-area achieved-throughput baseline: how many refreshes have
// landed for this area and since when. It is reset each time the area is (re)seeded
// so the console's load progress for a freshly enabled area reads from zero, the
// per-area analog of the old global reseed behavior.
type baseline struct {
	started       time.Time
	computedTotal int64
}

// Index tracks staleness for every pair in the working set and hands the stalest
// due pairs to workers under a lease.
type Index struct {
	started       time.Time
	now           func() time.Time
	entries       map[beeline.PairKey]*entry
	baselines     map[beeline.AreaID]*baseline
	targetTTL     time.Duration
	computedTotal int64
	mu            sync.Mutex
}

// New builds an index with the given target TTL. The clock is injectable so tests
// can advance time deterministically; pass nil to use the wall clock.
func New(targetTTL time.Duration, clock func() time.Time) *Index {
	if clock == nil {
		clock = time.Now
	}

	return &Index{
		now:       clock,
		entries:   make(map[beeline.PairKey]*entry),
		baselines: make(map[beeline.AreaID]*baseline),
		targetTTL: targetTTL,
		started:   clock(),
	}
}

// Seed adds keys to the working set as never-computed (maximally stale). Existing
// keys are left untouched so re-seeding is idempotent. The achieved-throughput
// baseline for every area present in the batch is reset to now, so a freshly enabled
// (or re-converged) area's load progress reads from zero.
func (i *Index) Seed(_ context.Context, keys []beeline.PairKey) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.now()
	for pos := range keys {
		if _, ok := i.entries[keys[pos]]; !ok {
			i.entries[keys[pos]] = &entry{}
		}
		i.baselines[keys[pos].Area] = &baseline{started: now}
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

	return nil
}

// CellStates rolls the working set up per origin cell for the progress map: for
// each origin, how many outgoing pairs exist and how many are currently fresh
// (computed and within the target TTL), plus the oldest age among its computed
// pairs. The result is unordered.
func (i *Index) CellStates(_ context.Context) ([]beeline.CellState, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.now()

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
		if age < i.targetTTL {
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

	now := i.now()

	type rollup struct {
		total     int
		fresh     int
		oldestAge time.Duration
	}

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
		if age < i.targetTTL {
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
// either never computed, demand-bumped, or older than the target TTL.
func (i *Index) due(e *entry, now time.Time) bool {
	if now.Before(e.leaseUntil) {
		return false
	}
	if e.computedAt.IsZero() || e.bumped {
		return true
	}

	return now.Sub(e.computedAt) >= i.targetTTL
}

// Claim leases up to limit of the stalest due keys for the given visibility
// timeout. Bumped and never-computed keys sort ahead of merely-aged ones; among
// the rest, oldest-computed wins.
func (i *Index) Claim(_ context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.now()

	type candidate struct {
		e   *entry
		key beeline.PairKey
	}

	candidates := make([]candidate, 0)
	for k, e := range i.entries {
		if i.due(e, now) {
			candidates = append(candidates, candidate{key: k, e: e})
		}
	}

	sort.Slice(candidates, func(a, b int) bool {
		ea, eb := candidates[a].e, candidates[b].e
		// Priority 1: demand-bumped keys.
		if ea.bumped != eb.bumped {
			return ea.bumped
		}
		// Priority 2: never-computed keys.
		na, nb := ea.computedAt.IsZero(), eb.computedAt.IsZero()
		if na != nb {
			return na
		}
		// Priority 3: oldest computed first.
		return ea.computedAt.Before(eb.computedAt)
	})

	if limit > 0 && len(candidates) > limit {
		candidates = candidates[:limit]
	}

	claimed := make([]beeline.PairKey, len(candidates))
	for idx := range candidates {
		candidates[idx].e.leaseUntil = now.Add(lease)
		claimed[idx] = candidates[idx].key
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
		if b, has := i.baselines[keys[pos].Area]; has {
			b.computedTotal++
		}
	}

	return nil
}

// Bump raises refresh priority for demand-driven pairs (stale-while-revalidate).
// Unknown keys are added to the working set so a queried-but-unseeded pair starts
// getting refreshed.
func (i *Index) Bump(_ context.Context, keys []beeline.PairKey) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	for pos := range keys {
		e, ok := i.entries[keys[pos]]
		if !ok {
			e = &entry{}
			i.entries[keys[pos]] = e
		}
		e.bumped = true
	}

	return nil
}

// Invalidate re-enqueues every computed pair older than sel.OlderThan by clearing
// its ComputedAt, so it sorts to the front of the queue.
func (i *Index) Invalidate(_ context.Context, sel beeline.Selector) error {
	i.mu.Lock()
	defer i.mu.Unlock()

	for _, e := range i.entries {
		if !e.computedAt.IsZero() && e.computedAt.Before(sel.OlderThan) {
			e.computedAt = time.Time{}
		}
	}

	return nil
}

// Debt reports the freshness contract signals: working-set size, count of pairs
// older than the TTL (never-computed pairs count as debt), p100 age among computed
// pairs, and the required vs achieved refresh throughput.
func (i *Index) Debt(_ context.Context) (beeline.DebtStats, error) {
	i.mu.Lock()
	defer i.mu.Unlock()

	now := i.now()

	var (
		debt      int
		oldestAge time.Duration
	)

	for _, e := range i.entries {
		if e.computedAt.IsZero() {
			debt++
			continue
		}

		age := now.Sub(e.computedAt)
		if age > oldestAge {
			oldestAge = age
		}
		if age >= i.targetTTL {
			debt++
		}
	}

	workingSet := len(i.entries)

	var required float64
	if ttl := i.targetTTL.Seconds(); ttl > 0 {
		required = float64(workingSet) / ttl
	}

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

	now := i.now()

	var (
		debt       int
		workingSet int
		oldestAge  time.Duration
	)

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
		if age >= i.targetTTL {
			debt++
		}
	}

	var required float64
	if ttl := i.targetTTL.Seconds(); ttl > 0 {
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
