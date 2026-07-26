package redis_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/redis/redistest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(area beeline.AreaID, origin, dest beeline.H3Cell) beeline.PairKey {
	return beeline.PairKey{Area: area, Origin: origin, Dest: dest, Profile: "car", Res: 8}
}

func TestStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := redistest.Open(t)
	area := redistest.RandomArea(t)

	key := testKey(area, 10, 11)
	miss := testKey(area, 20, 21)
	stamp := time.Now().Truncate(time.Millisecond)

	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{ComputedAt: stamp, Estimate: beeline.Estimate{Duration: 120, Distance: 1500}},
	}}))

	got, err := store.BatchGet(ctx, []beeline.PairKey{key, miss})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.NotNil(t, got[0])
	assert.InDelta(t, 120, got[0].Duration, 1e-9)
	assert.InDelta(t, 1500, got[0].Distance, 1e-9)
	assert.True(t, got[0].ComputedAt.Equal(stamp), "ComputedAt survives the binary round trip to millisecond precision")
	assert.Nil(t, got[1], "miss should be a nil element")
}

func TestStoreDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := redistest.Open(t)
	area := redistest.RandomArea(t)

	keep := testKey(area, 10, 11)
	drop := testKey(area, 20, 21)
	require.NoError(t, store.Put(ctx, []beeline.Entry{
		{Key: keep, Stored: beeline.Stored{ComputedAt: time.Now(), Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
		{Key: drop, Stored: beeline.Stored{ComputedAt: time.Now(), Estimate: beeline.Estimate{Duration: 2, Distance: 2}}},
	}))

	require.NoError(t, store.Delete(ctx, []beeline.PairKey{drop, testKey(area, 90, 91)}),
		"deleting absent keys must be a no-op")

	got, err := store.BatchGet(ctx, []beeline.PairKey{keep, drop})
	require.NoError(t, err)
	assert.NotNil(t, got[0])
	assert.Nil(t, got[1])
}

func TestStoreDeleteArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := redistest.Open(t)
	area, other := redistest.RandomArea(t), redistest.RandomArea(t)

	require.NoError(t, store.Put(ctx, []beeline.Entry{
		{Key: testKey(area, 10, 11), Stored: beeline.Stored{ComputedAt: time.Now(), Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
		{Key: testKey(area, 12, 13), Stored: beeline.Stored{ComputedAt: time.Now(), Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
		{Key: testKey(other, 20, 21), Stored: beeline.Stored{ComputedAt: time.Now(), Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
	}))

	require.NoError(t, store.DeleteArea(ctx, area))

	got, err := store.BatchGet(ctx, []beeline.PairKey{testKey(area, 10, 11), testKey(area, 12, 13), testKey(other, 20, 21)})
	require.NoError(t, err)
	assert.Nil(t, got[0])
	assert.Nil(t, got[1])
	assert.NotNil(t, got[2], "other areas' estimates must survive")
}

func TestStoreBatchGetAlignmentAcrossChunks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := redistest.Open(t)
	area := redistest.RandomArea(t)

	// Spans multiple 5k pipeline chunks with periodic misses, so a chunk-offset
	// bug in reassembly would surface immediately.
	const n = 12_000
	entries := make([]beeline.Entry, 0, n)
	keys := make([]beeline.PairKey, 0, n)
	for i := range n {
		k := testKey(area, beeline.H3Cell(i), beeline.H3Cell(i+1))
		keys = append(keys, k)
		if i%3 != 0 {
			entries = append(entries, beeline.Entry{
				Key:    k,
				Stored: beeline.Stored{ComputedAt: time.Now(), Estimate: beeline.Estimate{Duration: float64(i), Distance: float64(i)}},
			})
		}
	}
	require.NoError(t, store.Put(ctx, entries))
	t.Cleanup(func() { require.NoError(t, store.DeleteArea(context.Background(), area)) })

	got, err := store.BatchGet(ctx, keys)
	require.NoError(t, err)
	require.Len(t, got, n)
	for i := range n {
		if i%3 == 0 {
			require.Nilf(t, got[i], "key %d was never written", i)
			continue
		}
		require.NotNilf(t, got[i], "key %d was written", i)
		require.InDeltaf(t, float64(i), got[i].Duration, 1e-9, "key %d must get its own value back", i)
	}
}
