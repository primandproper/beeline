package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memory "github.com/primandproper/beeline/internal/freshness/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// areaKeys returns pairs tagged with the given area id.
func areaKeys(area beeline.AreaID) []beeline.PairKey {
	return []beeline.PairKey{
		{Area: area, Origin: 1, Dest: 2, Profile: "car", Res: 8},
		{Area: area, Origin: 1, Dest: 3, Profile: "car", Res: 8},
		{Area: area, Origin: 2, Dest: 3, Profile: "car", Res: 8},
	}
}

func TestIndexUnseedRemovesOnlyOneArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx := memory.New(time.Minute, nil)

	require.NoError(t, idx.Seed(ctx, areaKeys(1)))
	require.NoError(t, idx.Seed(ctx, areaKeys(2)))

	all, err := idx.Debt(ctx)
	require.NoError(t, err)
	assert.Equal(t, 6, all.WorkingSet)

	require.NoError(t, idx.Unseed(ctx, 1))

	// Area 1 is gone; area 2 is intact.
	one, err := idx.DebtForArea(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, one.WorkingSet)

	two, err := idx.DebtForArea(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, two.WorkingSet)

	all, err = idx.Debt(ctx)
	require.NoError(t, err)
	assert.Equal(t, 3, all.WorkingSet)
}

func TestIndexDebtForAreaBaselineIsPerArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Minute, clk.now)

	require.NoError(t, idx.Seed(ctx, areaKeys(1)))
	require.NoError(t, idx.Seed(ctx, areaKeys(2)))

	// Compute all of area 1's pairs; leave area 2 untouched.
	require.NoError(t, idx.MarkComputed(ctx, areaKeys(1), clk.now()))
	clk.advance(3 * time.Second)

	one, err := idx.DebtForArea(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 0, one.Debt, "area 1 fully computed")
	assert.InDelta(t, 1.0, one.AchievedThroughput, 1e-9, "3 refreshes over 3s = 1/s")

	two, err := idx.DebtForArea(ctx, 2)
	require.NoError(t, err)
	assert.Equal(t, 3, two.Debt, "area 2 never computed")
	assert.Equal(t, 0.0, two.AchievedThroughput, "area 2 has its own baseline at zero")
}

func TestIndexReseedingAreaResetsBaseline(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	clk := &clock{t: time.Unix(1_700_000_000, 0)}
	idx := memory.New(time.Minute, clk.now)

	require.NoError(t, idx.Seed(ctx, areaKeys(1)))
	require.NoError(t, idx.MarkComputed(ctx, areaKeys(1), clk.now()))
	clk.advance(time.Second)

	// Converge the area: unseed then re-seed. Progress must read from zero again.
	require.NoError(t, idx.Unseed(ctx, 1))
	require.NoError(t, idx.Seed(ctx, areaKeys(1)))

	stats, err := idx.DebtForArea(ctx, 1)
	require.NoError(t, err)
	assert.Equal(t, 3, stats.Debt, "re-seeded pairs are never-computed")
	assert.Equal(t, 0.0, stats.AchievedThroughput, "baseline reset on re-seed")
}

func TestIndexCellStatesForAreaScopes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	idx := memory.New(time.Minute, nil)

	require.NoError(t, idx.Seed(ctx, areaKeys(1)))
	require.NoError(t, idx.Seed(ctx, areaKeys(2)))

	states, err := idx.CellStatesForArea(ctx, 1)
	require.NoError(t, err)

	// Only area 1's origins (1 and 2) appear, with area 1's totals.
	byOrigin := make(map[beeline.H3Cell]beeline.CellState, len(states))
	for _, s := range states {
		byOrigin[s.Origin] = s
	}
	require.Len(t, states, 2)
	assert.Equal(t, 2, byOrigin[1].Total)
	assert.Equal(t, 1, byOrigin[2].Total)
}
