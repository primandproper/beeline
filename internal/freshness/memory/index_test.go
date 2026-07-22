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

func TestIndexPerAreaTargetTTLAndLease(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Hour, clk.now) // index-wide default TTL is deliberately long

	shortKey := beeline.PairKey{Area: 1, Origin: 1, Dest: 2, Profile: "car", Res: 8}
	longKey := beeline.PairKey{Area: 2, Origin: 1, Dest: 2, Profile: "car", Res: 8}

	// Area 1: short 30s TTL, long 5m lease. Area 2: long 10m TTL.
	require.NoError(t, idx.SetAreaFreshness(ctx, 1, 30*time.Second, 5*time.Minute))
	require.NoError(t, idx.SetAreaFreshness(ctx, 2, 10*time.Minute, 15*time.Second))
	require.NoError(t, idx.Seed(ctx, []beeline.PairKey{shortKey, longKey}))

	// Compute both now so neither is never-computed, then release the leases.
	claimed, err := idx.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	require.Len(t, claimed, 2)
	require.NoError(t, idx.MarkComputed(ctx, claimed, clk.now()))

	// Past area 1's 30s TTL but well within area 2's 10m TTL: only area 1's pair is stale.
	clk.advance(45 * time.Second)
	due, err := idx.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	assert.Equal(t, []beeline.PairKey{shortKey}, due, "only the short-TTL area's pair is due")

	// DebtForArea judges staleness against each area's own TTL.
	d1, err := idx.DebtForArea(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 1, d1.Debt, "area 1 pair is older than its 30s TTL")
	d2, err := idx.DebtForArea(ctx, 2)
	require.NoError(t, err)
	assert.Zero(t, d2.Debt, "area 2 pair is well within its 10m TTL")

	// The just-claimed area-1 pair holds its area's 5m lease, not the 15s fallback passed
	// to Claim: a minute later it is stale yet still invisible to a re-claim.
	clk.advance(time.Minute)
	again, err := idx.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	assert.Empty(t, again, "the per-area 5m lease keeps the claimed pair invisible")
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

func TestIndexAccessTracksWithoutBumping(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Minute, clk.now)

	ks := keys()
	require.NoError(t, idx.Seed(ctx, ks))
	claimed, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	require.NoError(t, idx.MarkComputed(ctx, claimed, clk.now()))

	// Access an unseeded key: it joins the working set as a never-computed demand entry.
	newKey := beeline.PairKey{Origin: 9, Dest: 10, Profile: "car", Res: 8}
	require.NoError(t, idx.Access(ctx, []beeline.PairKey{newKey}))

	stats, err := idx.Debt(ctx)
	require.NoError(t, err)
	assert.Equal(t, 4, stats.WorkingSet, "Access adds the key to the working set")

	// Accessing an existing fresh key must NOT raise its refresh priority (unlike Bump),
	// so only the never-computed newKey is due.
	require.NoError(t, idx.Access(ctx, []beeline.PairKey{ks[0]}))
	due, err := idx.Claim(ctx, 10, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, []beeline.PairKey{newKey}, due, "Access does not bump a fresh pair")
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
