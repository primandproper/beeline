package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/postgres"
	"github.com/primandproper/beeline/internal/store/postgres/pgtest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey(area beeline.AreaID, origin, dest beeline.H3Cell) beeline.PairKey {
	return beeline.PairKey{Area: area, Origin: origin, Dest: dest, Profile: "car", Res: 8}
}

func TestEstimateStoreRoundTrip(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := postgres.NewEstimateStore(pgtest.Open(t))

	key := testKey(1, 0x8828308281fffff, 0x8828308283fffff)
	miss := testKey(1, 0x8828308285fffff, 0x8828308287fffff)

	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 120, Distance: 1500}},
	}}))

	got, err := store.BatchGet(ctx, []beeline.PairKey{key, miss})
	require.NoError(t, err)
	require.Len(t, got, 2)

	require.NotNil(t, got[0])
	assert.InDelta(t, 120, got[0].Duration, 1e-9)
	assert.InDelta(t, 1500, got[0].Distance, 1e-9)
	assert.WithinDuration(t, time.Now(), got[0].ComputedAt, time.Minute,
		"ComputedAt must be server-stamped at Put, not taken from the entry")
	assert.Nil(t, got[1], "miss should be a nil element")

	// Overwrite refreshes the value and the server-side stamp.
	first := got[0].ComputedAt
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 60, Distance: 900}},
	}}))
	got, err = store.BatchGet(ctx, []beeline.PairKey{key})
	require.NoError(t, err)
	require.NotNil(t, got[0])
	assert.InDelta(t, 60, got[0].Duration, 1e-9)
	assert.False(t, got[0].ComputedAt.Before(first), "overwrite must not regress the stamp")
}

func TestEstimateStoreDelete(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := postgres.NewEstimateStore(pgtest.Open(t))

	keep := testKey(1, 10, 11)
	drop := testKey(1, 20, 21)
	entries := []beeline.Entry{
		{Key: keep, Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
		{Key: drop, Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 2, Distance: 2}}},
	}
	require.NoError(t, store.Put(ctx, entries))

	require.NoError(t, store.Delete(ctx, []beeline.PairKey{drop, testKey(9, 1, 2)}),
		"deleting absent keys must be a no-op")

	got, err := store.BatchGet(ctx, []beeline.PairKey{keep, drop})
	require.NoError(t, err)
	assert.NotNil(t, got[0])
	assert.Nil(t, got[1])
}

func TestEstimateStoreDeleteArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := postgres.NewEstimateStore(pgtest.Open(t))

	require.NoError(t, store.Put(ctx, []beeline.Entry{
		{Key: testKey(1, 10, 11), Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
		{Key: testKey(1, 12, 13), Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
		{Key: testKey(2, 20, 21), Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1, Distance: 1}}},
	}))

	require.NoError(t, store.DeleteArea(ctx, 1))

	got, err := store.BatchGet(ctx, []beeline.PairKey{testKey(1, 10, 11), testKey(1, 12, 13), testKey(2, 20, 21)})
	require.NoError(t, err)
	assert.Nil(t, got[0])
	assert.Nil(t, got[1])
	assert.NotNil(t, got[2], "other areas' estimates must survive")
}

func TestEstimateStoreBatchGetAlignmentAcrossChunks(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := postgres.NewEstimateStore(pgtest.Open(t))

	// Enough keys to span multiple 20k chunks, with every third key a miss, so
	// a misaligned chunk offset would show up immediately.
	const n = 45_000
	entries := make([]beeline.Entry, 0, n)
	keys := make([]beeline.PairKey, 0, n)
	for i := range n {
		k := testKey(3, beeline.H3Cell(i), beeline.H3Cell(i+1))
		keys = append(keys, k)
		if i%3 != 0 {
			entries = append(entries, beeline.Entry{
				Key:    k,
				Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: float64(i), Distance: float64(i)}},
			})
		}
	}
	require.NoError(t, store.Put(ctx, entries))

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
