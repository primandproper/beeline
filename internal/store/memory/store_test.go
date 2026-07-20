package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memory "github.com/primandproper/beeline/internal/store/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.New()

	key := beeline.PairKey{Origin: 1, Dest: 2, Profile: "car", Res: 8}
	miss := beeline.PairKey{Origin: 9, Dest: 9, Profile: "car", Res: 8}

	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 120, Distance: 1500}, ComputedAt: time.Now()},
	}}))

	got, err := store.BatchGet(ctx, []beeline.PairKey{key, miss})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.NotNil(t, got[0])
	assert.InDelta(t, 120, got[0].Duration, 1e-9)
	assert.InDelta(t, 1500, got[0].Distance, 1e-9)

	assert.Nil(t, got[1], "miss should be a nil element")
	assert.Equal(t, 1, store.Len())
}

func TestStoreDeleteArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memory.New()

	put := func(area beeline.AreaID, origin beeline.H3Cell) {
		require.NoError(t, store.Put(ctx, []beeline.Entry{{
			Key:    beeline.PairKey{Area: area, Origin: origin, Dest: origin + 1, Profile: "car", Res: 8},
			Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1, Distance: 1}, ComputedAt: time.Now()},
		}}))
	}

	put(1, 10)
	put(1, 11)
	put(2, 20)
	require.Equal(t, 3, store.Len())

	require.NoError(t, store.DeleteArea(ctx, 1))
	assert.Equal(t, 1, store.Len(), "only area 2's estimate remains")

	// Area 2's estimate is still retrievable; area 1's is gone.
	got, err := store.BatchGet(ctx, []beeline.PairKey{
		{Area: 2, Origin: 20, Dest: 21, Profile: "car", Res: 8},
		{Area: 1, Origin: 10, Dest: 11, Profile: "car", Res: 8},
	})
	require.NoError(t, err)
	assert.NotNil(t, got[0])
	assert.Nil(t, got[1])
}
