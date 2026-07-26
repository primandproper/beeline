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
