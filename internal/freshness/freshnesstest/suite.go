// Package freshnesstest is the conformance suite for FreshnessIndex
// implementations: one set of behavioral tests both the in-memory index and the
// Postgres index must pass.
//
// Its purpose narrowed when single-node mode was deleted. It is no longer keeping
// two shipping backends aligned — Postgres is the only one that ships. It now
// keeps the **test double honest about the real one**: the in-memory index is what
// control, httpapi, query and refresh schedule against in their own tests, so any
// behavior it gets wrong silently weakens every one of those suites. That is the
// only thing justifying the double's continued existence, and this suite is what
// makes the justification true rather than hopeful.
//
// The suite runs in real time (the Postgres clock cannot be injected), so it uses
// short-but-generous windows: hundreds of milliseconds for TTL/lease expiries,
// with waits at 2–3× the window.
package freshnesstest

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Index is the full surface the suite exercises: the scheduling interface plus
// the control plane's per-area seam.
type Index interface {
	beeline.FreshnessIndex
	control.AreaIndex
}

// Factory builds a fresh, empty index with the given index-wide target TTL.
// Implementations register their own cleanup on tb.
type Factory func(tb testing.TB, targetTTL time.Duration) Index

// key builds a suite pair key. Suite tests always use distinct areas per
// subtest, so shared backends (one Postgres schema per test binary run) don't
// cross-contaminate.
func key(area beeline.AreaID, origin, dest int) beeline.PairKey {
	return beeline.PairKey{
		Profile: "car",
		Area:    area,
		Origin:  beeline.H3Cell(origin),
		Dest:    beeline.H3Cell(dest),
		Res:     8,
	}
}

// keySet turns claimed keys into a set for order-insensitive assertions (the
// Postgres index's RETURNING order is unspecified).
func keySet(keys []beeline.PairKey) map[beeline.PairKey]bool {
	set := make(map[beeline.PairKey]bool, len(keys))
	for pos := range keys {
		set[keys[pos]] = true
	}

	return set
}

// Run exercises the shared FreshnessIndex contract against the factory's
// implementation.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	ctx := context.Background()

	t.Run("seed then claim then mark lifecycle", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		keys := []beeline.PairKey{key(101, 1, 2), key(101, 1, 3), key(101, 2, 3)}
		require.NoError(t, idx.Seed(ctx, keys))

		claimed, err := idx.Claim(ctx, 2, time.Minute)
		require.NoError(t, err)
		require.Len(t, claimed, 2, "never-computed pairs are due")

		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		rest, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		require.Len(t, rest, 1, "only the unclaimed pair remains due (TTL is an hour)")
		assert.False(t, keySet(claimed)[rest[0]], "the remaining pair is the never-claimed one")

		stats, err := idx.DebtForArea(ctx, 101)
		require.NoError(t, err)
		assert.Equal(t, 3, stats.WorkingSet)
		assert.Equal(t, 1, stats.Debt, "two computed, one never-computed")
	})

	t.Run("a lease hides pairs until it expires", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{key(102, 1, 2)}))

		claimed, err := idx.Claim(ctx, 10, 400*time.Millisecond)
		require.NoError(t, err)
		require.Len(t, claimed, 1)

		again, err := idx.Claim(ctx, 10, 400*time.Millisecond)
		require.NoError(t, err)
		assert.Empty(t, again, "a leased pair is not claimable")

		time.Sleep(time.Second)

		expired, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Len(t, expired, 1, "an expired lease returns the pair to the queue")
	})

	t.Run("bumped pairs claim first", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Millisecond)

		fresh := key(103, 1, 2)
		bumped := key(103, 2, 3)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{fresh, bumped}))
		require.NoError(t, idx.MarkComputed(ctx, []beeline.PairKey{fresh, bumped}, time.Now()))
		time.Sleep(50 * time.Millisecond) // both now past the 1ms TTL — equally stale
		require.NoError(t, idx.Bump(ctx, []beeline.PairKey{bumped}))

		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		assert.Equal(t, bumped, claimed[0], "the demand-bumped pair outranks equally stale pairs")
	})

	t.Run("computed pairs come due again after the TTL", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, 500*time.Millisecond)

		k := key(104, 1, 2)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))
		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		immediately, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Empty(t, immediately, "freshly computed pairs are not due")

		time.Sleep(1200 * time.Millisecond)

		due, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Len(t, due, 1, "past the TTL the pair is due again")
	})

	t.Run("access then immediate mark lands on the tracked pair", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		// The demand-fill commit's exact call order: Access introduces the pair,
		// MarkComputed (possibly microseconds later) must stamp that same pair —
		// even when the implementation buffers access stamps.
		k := key(105, 1, 2)
		require.NoError(t, idx.Access(ctx, []beeline.PairKey{k}))
		require.NoError(t, idx.MarkComputed(ctx, []beeline.PairKey{k}, time.Now()))

		stats, err := idx.DebtForArea(ctx, 105)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.WorkingSet, "the accessed pair joined the working set")
		assert.Equal(t, 0, stats.Debt, "and it is computed, not owed")

		claimed, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Empty(t, claimed, "a computed demand pair inside its TTL is not due")
	})

	t.Run("sweep evicts cold demand but never pinned or leased pairs", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		pinned := key(106, 1, 2)
		cold := key(106, 2, 3)
		leased := key(106, 3, 4)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{pinned}))
		require.NoError(t, idx.Access(ctx, []beeline.PairKey{cold}))
		require.NoError(t, idx.Bump(ctx, []beeline.PairKey{leased}))
		time.Sleep(300 * time.Millisecond) // let buffered access stamps land

		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.Equal(t, []beeline.PairKey{leased}, claimed, "the bumped pair claims first and is now leased")

		removed, err := idx.SweepArea(ctx, 106, time.Now().Add(time.Minute))
		require.NoError(t, err)
		assert.Equal(t, map[beeline.PairKey]bool{cold: true}, keySet(removed),
			"only the cold unpinned, unleased demand pair is evicted")

		stats, err := idx.DebtForArea(ctx, 106)
		require.NoError(t, err)
		assert.Equal(t, 2, stats.WorkingSet, "pinned + leased survive")
	})

	t.Run("a recently accessed demand pair survives the sweep", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		warm := key(107, 1, 2)
		require.NoError(t, idx.Access(ctx, []beeline.PairKey{warm}))
		time.Sleep(300 * time.Millisecond) // let buffered access stamps land

		removed, err := idx.SweepArea(ctx, 107, time.Now().Add(-time.Minute))
		require.NoError(t, err)
		assert.Empty(t, removed, "accessed after the cutoff, so it stays")
	})

	t.Run("unseed drops one area and leaves others alone", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{key(108, 1, 2), key(109, 1, 2)}))
		require.NoError(t, idx.Unseed(ctx, 108))

		gone, err := idx.DebtForArea(ctx, 108)
		require.NoError(t, err)
		assert.Zero(t, gone.WorkingSet)

		kept, err := idx.DebtForArea(ctx, 109)
		require.NoError(t, err)
		assert.Equal(t, 1, kept.WorkingSet)
	})

	t.Run("a per-area TTL override wins over the index default", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		k := key(110, 1, 2)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))
		require.NoError(t, idx.SetAreaFreshness(ctx, 110, 400*time.Millisecond, 0))

		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		time.Sleep(time.Second)

		due, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Len(t, due, 1, "the 400ms area TTL made it due despite the 1h default")
	})

	t.Run("bumping the same pair twice in one batch is not an error", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Millisecond)

		// Callers legitimately repeat a key inside one batch: a /table grid whose
		// coordinates collapse onto one H3 cell pair, or a warm-set feed naming a
		// pair twice. The map-based memory index never noticed; the Postgres index
		// used to reject the WHOLE statement with SQLSTATE 21000 ("ON CONFLICT DO
		// UPDATE command cannot affect row a second time"), silently discarding
		// every other pair's demand signal along with the duplicate.
		dup := key(117, 1, 2)
		other := key(117, 3, 4)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{dup, other}))
		require.NoError(t, idx.MarkComputed(ctx, []beeline.PairKey{dup, other}, time.Now()))
		time.Sleep(50 * time.Millisecond) // both past the 1ms TTL — equally stale

		require.NoError(t, idx.Bump(ctx, []beeline.PairKey{dup, other, dup, dup}),
			"a duplicated key in one batch must not fail the batch")

		claimed, err := idx.Claim(ctx, 2, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, keySet([]beeline.PairKey{dup, other}), keySet(claimed),
			"the duplicate's neighbors keep their bump")
	})

	t.Run("bumping only unknown duplicate keys still tracks them once", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		// The same duplicate hazard on the insert side: an unseeded key repeated in
		// one batch has no existing row to conflict with the first time and one the
		// second time, all inside a single statement.
		k := key(118, 1, 2)
		require.NoError(t, idx.Bump(ctx, []beeline.PairKey{k, k}))

		claimed, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Equal(t, []beeline.PairKey{k}, claimed,
			"a repeated unknown key joins the working set exactly once")
	})

	t.Run("re-registering an unchanged area contract preserves freshness", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		// Every head calls SetAreaFreshness per enabled area at boot and again on
		// every config-poll convergence, almost always with the values already
		// stored. That repeat has to be a genuine no-op: it must not disturb
		// freshness, and (on the Postgres index) it must not rewrite every computed
		// row in the area, which is what used to deadlock against the follower
		// pool's concurrent MarkComputed writes and crashloop the booting head.
		k := key(119, 1, 2)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))
		require.NoError(t, idx.SetAreaFreshness(ctx, 119, time.Hour, 0))

		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		require.NoError(t, idx.SetAreaFreshness(ctx, 119, time.Hour, 0), "re-registering is idempotent")

		due, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Empty(t, due, "an unchanged contract leaves the computed pair fresh")
	})

	t.Run("shortening an area TTL re-derives already-computed pairs", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		// The flip side of the no-op above: when the contract really does change,
		// pairs computed under the old TTL must come due against the new one
		// without waiting to be recomputed first.
		//
		// The new TTL is deliberately sub-second. A whole-second value would pass
		// even against a backend that truncates the override to an integer and
		// falls back to its index-wide default, which is precisely the bug the
		// Postgres re-derive shipped with.
		k := key(120, 1, 2)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))
		require.NoError(t, idx.SetAreaFreshness(ctx, 120, time.Hour, 0))

		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.Len(t, claimed, 1)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		require.NoError(t, idx.SetAreaFreshness(ctx, 120, 300*time.Millisecond, 0))
		time.Sleep(900 * time.Millisecond)

		due, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Len(t, due, 1, "the shortened 300ms TTL made the already-computed pair due")
	})

	t.Run("invalidate requeues computed pairs", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		k := key(111, 1, 2)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))
		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		require.NoError(t, idx.Invalidate(ctx, beeline.Selector{OlderThan: time.Now().Add(time.Hour)}))

		due, err := idx.Claim(ctx, 10, time.Minute)
		require.NoError(t, err)
		assert.Len(t, due, 1, "invalidated pairs are maximally stale again")

		stats, err := idx.DebtForArea(ctx, 111)
		require.NoError(t, err)
		assert.Equal(t, 1, stats.Debt)
	})

	t.Run("marking unknown keys is a no-op", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		require.NoError(t, idx.MarkComputed(ctx, []beeline.PairKey{key(112, 1, 2)}, time.Now()))

		stats, err := idx.DebtForArea(ctx, 112)
		require.NoError(t, err)
		assert.Zero(t, stats.WorkingSet, "an expired-lease straggler cannot re-add swept pairs")
	})

	t.Run("debt aggregates across areas and throughput moves", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{key(113, 1, 2), key(113, 1, 3), key(114, 1, 2)}))

		claimed, err := idx.Claim(ctx, 100, time.Minute)
		require.NoError(t, err)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		stats, err := idx.Debt(ctx)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, stats.WorkingSet, 3, "shared backends may hold other subtests' areas too")
		assert.Positive(t, stats.RequiredThroughput)
		assert.Positive(t, stats.AchievedThroughput, "marks advanced the global counter")

		area, err := idx.DebtForArea(ctx, 113)
		require.NoError(t, err)
		assert.Equal(t, 2, area.WorkingSet)
		assert.Zero(t, area.Debt)
		assert.Positive(t, area.AchievedThroughput)
	})

	t.Run("re-seeding resets an area's throughput baseline", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		k := key(115, 1, 2)
		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))
		claimed, err := idx.Claim(ctx, 1, time.Minute)
		require.NoError(t, err)
		require.NoError(t, idx.MarkComputed(ctx, claimed, time.Now()))

		before, err := idx.DebtForArea(ctx, 115)
		require.NoError(t, err)
		require.Positive(t, before.AchievedThroughput)

		require.NoError(t, idx.Seed(ctx, []beeline.PairKey{k}))

		after, err := idx.DebtForArea(ctx, 115)
		require.NoError(t, err)
		assert.Zero(t, after.AchievedThroughput, "a re-enabled area's progress reads from zero")
		assert.Equal(t, 1, after.WorkingSet, "re-seeding an existing key does not duplicate it")
	})

	t.Run("cell states roll up per origin", func(t *testing.T) {
		t.Parallel()
		idx := factory(t, time.Hour)

		keys := []beeline.PairKey{key(116, 1, 2), key(116, 1, 3), key(116, 2, 3)}
		require.NoError(t, idx.Seed(ctx, keys))
		require.NoError(t, idx.MarkComputed(ctx, []beeline.PairKey{keys[0]}, time.Now()))

		states, err := idx.CellStatesForArea(ctx, 116)
		require.NoError(t, err)
		require.Len(t, states, 2, "two distinct origins")

		byOrigin := make(map[beeline.H3Cell]beeline.CellState, len(states))
		for pos := range states {
			byOrigin[states[pos].Origin] = states[pos]
		}
		assert.Equal(t, 2, byOrigin[beeline.H3Cell(1)].Total)
		assert.Equal(t, 1, byOrigin[beeline.H3Cell(1)].Fresh, "one of origin 1's pairs is computed and fresh")
		assert.Equal(t, 1, byOrigin[beeline.H3Cell(2)].Total)
		assert.Equal(t, 0, byOrigin[beeline.H3Cell(2)].Fresh)
	})
}
