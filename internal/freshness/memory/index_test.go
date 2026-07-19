package memory_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memory "github.com/primandproper/beeline/internal/freshness/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clock is a test-controlled time source.
type clock struct {
	t  time.Time
	mu sync.Mutex
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

func keys() []beeline.PairKey {
	return []beeline.PairKey{
		{Origin: 1, Dest: 2, Profile: "car", Res: 8},
		{Origin: 1, Dest: 3, Profile: "car", Res: 8},
		{Origin: 2, Dest: 3, Profile: "car", Res: 8},
	}
}

func TestIndexClaimLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Minute, clk.now)

	require.NoError(t, idx.Seed(ctx, keys()))

	// Never-computed keys are all due.
	claimed, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	assert.Len(t, claimed, 3)

	// A second claim before the lease expires finds nothing due.
	again, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	assert.Empty(t, again)

	// After computing, debt is zero.
	require.NoError(t, idx.MarkComputed(ctx, claimed, clk.now()))
	stats, err := idx.Debt(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.WorkingSet)
	assert.Equal(t, 0, stats.Debt)

	// Once the TTL elapses the pairs go stale and become claimable again.
	clk.advance(2 * time.Minute)
	due, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	assert.Len(t, due, 3)

	stats, err = idx.Debt(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Debt)
	assert.GreaterOrEqual(t, stats.OldestAgeSeconds, 120.0)
}

func TestIndexBumpPrioritizesDemand(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Minute, clk.now)

	ks := keys()
	require.NoError(t, idx.Seed(ctx, ks))

	// Compute everything so nothing is naturally due.
	claimed, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, idx.MarkComputed(ctx, claimed, clk.now()))

	// Nothing due yet (all fresh).
	none, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	require.Empty(t, none)

	// Bump one pair: it becomes the only due key.
	require.NoError(t, idx.Bump(ctx, []beeline.PairKey{ks[1]}))
	got, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, ks[1], got[0])
}

func TestIndexInvalidateReenqueues(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Hour, clk.now)

	ks := keys()
	require.NoError(t, idx.Seed(ctx, ks))
	claimed, err := idx.Claim(ctx, 10, time.Second)
	require.NoError(t, err)
	require.NoError(t, idx.MarkComputed(ctx, claimed, clk.now()))

	// Well within the 1h TTL nothing is due...
	clk.advance(5 * time.Minute)
	none, err := idx.Claim(ctx, 10, time.Second)
	require.NoError(t, err)
	require.Empty(t, none)

	// ...until an explicit invalidation of everything computed so far.
	require.NoError(t, idx.Invalidate(ctx, beeline.Selector{OlderThan: clk.now()}))
	due, err := idx.Claim(ctx, 10, time.Second)
	require.NoError(t, err)
	assert.Len(t, due, 3)
}
