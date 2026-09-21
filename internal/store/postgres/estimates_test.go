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

// The Store contract itself lives in conformance_test.go, which runs the shared
// storetest suite — round trips, miss handling, chunk alignment, Delete and
// DeleteArea are all covered there for every backend. What remains here is the
// one behavior that is specifically Postgres's: it is the clock authority, so it
// ignores the entry's ComputedAt and stamps its own. The shared suite can only
// assert that *some* conformant timestamp comes back, since a client-stamping
// backend is equally correct.

func testKey(area beeline.AreaID, origin, dest beeline.H3Cell) beeline.PairKey {
	return beeline.PairKey{Area: area, Origin: origin, Dest: dest, Profile: "car", Res: 8}
}

func TestEstimateStoreServerStampsComputedAt(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := postgres.NewEstimateStore(pgtest.Open(t))

	key := testKey(1, 0x8828308281fffff, 0x8828308283fffff)

	// A zero ComputedAt on the way in: whatever comes back is the database's.
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:      key,
		Duration: 120, Distance: 1500,
	}}))

	got, err := store.BatchGet(ctx, []beeline.PairKey{key})
	require.NoError(t, err)
	require.NotNil(t, got[0])
	assert.WithinDuration(t, time.Now(), got[0].ComputedAt, time.Minute,
		"ComputedAt must be server-stamped at Put, not taken from the entry")

	// An overwrite re-stamps, so a refreshed pair reads as freshly computed.
	first := got[0].ComputedAt
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:      key,
		Duration: 60, Distance: 900,
	}}))
	got, err = store.BatchGet(ctx, []beeline.PairKey{key})
	require.NoError(t, err)
	require.NotNil(t, got[0])
	assert.InDelta(t, 60, got[0].Duration, 1e-9)
	assert.False(t, got[0].ComputedAt.Before(first), "overwrite must not regress the stamp")
}
