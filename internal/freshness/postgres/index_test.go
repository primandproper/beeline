package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/freshness/freshnesstest"
	pgfresh "github.com/primandproper/beeline/internal/freshness/postgres"
	"github.com/primandproper/beeline/internal/store/postgres/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newIndex opens a schema-isolated index for one test. Stats caching is off
// (tests read immediately after writing) and the access flush is fast so
// suite waits stay short.
func newIndex(tb testing.TB, targetTTL time.Duration) *pgfresh.Index {
	tb.Helper()

	idx, err := pgfresh.New(pgtest.Open(tb), &pgfresh.Config{
		TargetTTL:           targetTTL,
		StatsCacheTTL:       0,
		AccessFlushInterval: 50 * time.Millisecond,
	}, nil)
	require.NoError(tb, err)
	tb.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		require.NoError(tb, idx.Close(ctx))
	})

	return idx
}

// TestConformance runs the shared behavioral suite (the same one the memory
// index runs) against the Postgres index.
func TestConformance(t *testing.T) {
	t.Parallel()

	freshnesstest.Run(t, func(tb testing.TB, targetTTL time.Duration) freshnesstest.Index {
		tb.Helper()
		return newIndex(tb, targetTTL)
	})
}

// TestConcurrentClaimsDoNotOverlap is the pg-specific property the memory
// index gets from its mutex: two racing claimers (two heads, or a head and a
// follower) must never lease the same pair.
func TestConcurrentClaimsDoNotOverlap(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx := newIndex(t, time.Hour)

	keys := make([]beeline.PairKey, 200)
	for i := range keys {
		keys[i] = beeline.PairKey{
			Profile: "car", Area: 200, Res: 8,
			Origin: beeline.H3Cell(i + 1), Dest: beeline.H3Cell(i + 2),
		}
	}
	require.NoError(t, idx.Seed(ctx, keys))

	const claimers = 8
	results := make(chan []beeline.PairKey, claimers)
	for range claimers {
		go func() {
			claimed, err := idx.Claim(ctx, 50, time.Minute)
			assert.NoError(t, err)
			results <- claimed
		}()
	}

	seen := make(map[beeline.PairKey]bool)
	total := 0
	for range claimers {
		for _, k := range <-results {
			require.False(t, seen[k], "pair %v was claimed twice", k)
			seen[k] = true
			total++
		}
	}
	assert.Equal(t, len(keys), total, "every pair claimed exactly once across racing claimers")
}

// TestConcurrentBumpsDoNotDeadlock is the regression test for the outage this
// batcher was written for. The read path bumps on every stale cache hit, so
// heavy overlapping traffic used to put one INSERT … ON CONFLICT DO UPDATE per
// in-flight request on the same rows; batches that reached those rows in
// different orders deadlocked (SQLSTATE 40P01), and the failed bumps piled up
// until they held every pool connection and starved unrelated queries.
//
// Overlapping key sets are the point: without merged, order-stable writes this
// deadlocks within a few rounds.
func TestConcurrentBumpsDoNotDeadlock(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx := newIndex(t, time.Hour)

	// Each batch is a different *window* of a shared pool of pairs — overlapping
	// its neighbors, but agreeing with none of them on membership or size. That
	// difference is what reproduces the outage: distinct key sets give the
	// planner no reason to reach the shared rows in the same relative order, so
	// two batches take the same two locks in opposite orders and cycle. (Batches
	// that are mere rotations of one identical set do NOT reproduce it — the
	// statement's DISTINCT normalizes them into the same order by accident.)
	const (
		bumpers = 16
		rounds  = 12
		pool    = 60
	)
	key := func(n int) beeline.PairKey {
		return beeline.PairKey{
			Profile: "car", Area: 202, Res: 8,
			Origin: beeline.H3Cell(n%pool + 1), Dest: beeline.H3Cell(n%pool + 2),
		}
	}

	errs := make(chan error, bumpers)
	for b := range bumpers {
		go func() {
			for r := range rounds {
				batch := make([]beeline.PairKey, 15+(b*3+r)%20)
				for i := range batch {
					batch[i] = key(b*7 + r*3 + i)
				}
				if err := idx.Bump(ctx, batch); err != nil {
					errs <- err

					return
				}
			}
			errs <- nil
		}()
	}
	for range bumpers {
		require.NoError(t, <-errs, "concurrent overlapping bumps must not deadlock")
	}

	// Read-your-write survives the merge: Bump returns only once its own keys
	// have landed, so every bumped pair is claimable the moment the last
	// bumper returns.
	claimed, err := idx.Claim(ctx, pool*2, time.Minute)
	require.NoError(t, err)
	assert.Len(t, claimed, pool, "every distinct bumped pair joined the working set exactly once")
}

// TestStatsCacheServesWithinTTL verifies the per-head memoization that keeps
// console polling cheap: within the TTL, reads come from the memo.
func TestStatsCacheServesWithinTTL(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx, err := pgfresh.New(pgtest.Open(t), &pgfresh.Config{
		TargetTTL:     time.Hour,
		StatsCacheTTL: time.Minute,
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, idx.Close(context.Background())) })

	require.NoError(t, idx.Seed(ctx, []beeline.PairKey{{Profile: "car", Area: 201, Origin: 1, Dest: 2, Res: 8}}))

	first, err := idx.DebtForArea(ctx, 201)
	require.NoError(t, err)
	require.Equal(t, 1, first.WorkingSet)

	require.NoError(t, idx.Seed(ctx, []beeline.PairKey{{Profile: "car", Area: 201, Origin: 2, Dest: 3, Res: 8}}))

	cached, err := idx.DebtForArea(ctx, 201)
	require.NoError(t, err)
	assert.Equal(t, 1, cached.WorkingSet, "within the cache TTL the memoized rollup is served")
}
