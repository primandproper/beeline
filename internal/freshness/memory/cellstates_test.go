package memory_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memory "github.com/primandproper/beeline/internal/freshness/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIndexCellStatesRollup(t *testing.T) {
	t.Parallel()

	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		idx := memory.New(time.Minute, nil)

		// Origin 1 has two outgoing pairs; origin 2 has one.
		require.NoError(t, idx.Seed(ctx, keys()))

		// Compute just one of origin 1's pairs so it is half fresh.
		require.NoError(t, idx.MarkComputed(ctx, []beeline.PairKey{{Origin: 1, Dest: 2, Profile: "car", Res: 8}}, time.Now()))

		states, err := idx.CellStates(ctx)
		require.NoError(t, err)

		byOrigin := make(map[beeline.H3Cell]beeline.CellState, len(states))
		for _, s := range states {
			byOrigin[s.Origin] = s
		}

		require.Contains(t, byOrigin, beeline.H3Cell(1))
		assert.Equal(t, 2, byOrigin[1].Total)
		assert.Equal(t, 1, byOrigin[1].Fresh)

		require.Contains(t, byOrigin, beeline.H3Cell(2))
		assert.Equal(t, 1, byOrigin[2].Total)
		assert.Equal(t, 0, byOrigin[2].Fresh)

		// After the TTL elapses, the computed pair is no longer fresh.
		time.Sleep(2 * time.Minute)
		states, err = idx.CellStates(ctx)
		require.NoError(t, err)
		for _, s := range states {
			if s.Origin == 1 {
				assert.Equal(t, 0, s.Fresh, "aged past TTL is not fresh")
				assert.GreaterOrEqual(t, s.OldestAgeSeconds, 120.0)
			}
		}
	})
}
