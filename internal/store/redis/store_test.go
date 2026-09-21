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

// The Store contract itself lives in conformance_test.go, which runs the shared
// storetest suite — round trips, miss handling, chunk alignment, Delete and
// DeleteArea are all covered there for every backend. What remains here is the
// one behavior specific to Redis: its fixed 24-byte value packs ComputedAt into
// a millisecond field, so precision loss is exactly one millisecond and no more.
// The shared suite deliberately tolerates any conformant timestamp, so only this
// test pins the encoding.

func testKey(area beeline.AreaID, origin, dest beeline.H3Cell) beeline.PairKey {
	return beeline.PairKey{Area: area, Origin: origin, Dest: dest, Profile: "car", Res: 8}
}

func TestStoreComputedAtSurvivesToMillisecondPrecision(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := redistest.Open(t)
	area := redistest.RandomArea(t)

	key := testKey(area, 10, 11)

	// Sub-millisecond digits are the part the encoding cannot keep; truncating
	// first makes the assertion exact rather than approximate.
	stamp := time.Now().Truncate(time.Millisecond)
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:        key,
		ComputedAt: stamp, Duration: 120, Distance: 1500,
	}}))

	got, err := store.BatchGet(ctx, []beeline.PairKey{key})
	require.NoError(t, err)
	require.NotNil(t, got[0])
	assert.True(t, got[0].ComputedAt.Equal(stamp),
		"Redis keeps the client's stamp to the millisecond: want %s, got %s", stamp, got[0].ComputedAt)

	// And a sub-millisecond stamp loses at most that millisecond — it is never
	// dropped to zero, which the read path would treat as infinitely stale.
	precise := time.Now().Add(500 * time.Microsecond)
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:        key,
		ComputedAt: precise, Duration: 1, Distance: 1,
	}}))
	got, err = store.BatchGet(ctx, []beeline.PairKey{key})
	require.NoError(t, err)
	require.NotNil(t, got[0])
	assert.WithinDuration(t, precise, got[0].ComputedAt, time.Millisecond,
		"truncation loses at most one millisecond")
}
