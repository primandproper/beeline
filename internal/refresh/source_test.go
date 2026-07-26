package refresh_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/refresh"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testPairs builds n directed pairs in area 1 at resolution 9 around San Francisco.
func testPairs(t *testing.T, n int) []beeline.PairKey {
	t.Helper()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)

	pairs := make([]beeline.PairKey, 0, n)
	for i := range n {
		dest, destErr := beeline.CellAt(beeline.LatLng{Lat: 37.70 + float64(i)*0.01, Lng: -122.4194}, 9)
		require.NoError(t, destErr)
		pairs = append(pairs, beeline.PairKey{
			Area:    1,
			Origin:  origin,
			Dest:    dest,
			Profile: "car",
			Res:     9,
		})
	}

	return pairs
}

func TestLocalSourceClaimAndSubmit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	source := refresh.NewLocalSource(index, store)

	pairs := testPairs(t, 3)
	require.NoError(t, index.Seed(ctx, pairs))

	claimed, err := source.Claim(ctx, 10, time.Minute)
	require.NoError(t, err)
	assert.Len(t, claimed, 3)

	now := time.Now()
	entries := make([]beeline.Entry, 0, len(claimed))
	for _, key := range claimed {
		entries = append(entries, beeline.Entry{
			Key:    key,
			Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 60, Distance: 500}, ComputedAt: now},
		})
	}
	require.NoError(t, source.Submit(ctx, entries))

	stored, err := store.BatchGet(ctx, claimed)
	require.NoError(t, err)
	for _, s := range stored {
		require.NotNil(t, s)
		assert.Equal(t, 60.0, s.Duration)
	}

	debt, err := index.Debt(ctx)
	require.NoError(t, err)
	assert.Zero(t, debt.Debt, "all pairs are fresh after submit")
}

// failingStore rejects every Put so Submit's no-mark-on-store-error contract is
// observable.
type failingStore struct{ memstore.Store }

func (f *failingStore) Put(context.Context, []beeline.Entry) error {
	return errors.New("disk on fire")
}

func TestLocalSourceSubmitStoreErrorLeavesLease(t *testing.T) {
	t.Parallel()

	// Bubble time so the lease can be expired without really sleeping.
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()

		index := memindex.New(time.Minute, nil)
		source := refresh.NewLocalSource(index, &failingStore{})

		pairs := testPairs(t, 1)
		require.NoError(t, index.Seed(ctx, pairs))

		claimed, err := source.Claim(ctx, 10, time.Second)
		require.NoError(t, err)
		require.Len(t, claimed, 1)

		entries := []beeline.Entry{{
			Key:    claimed[0],
			Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1}, ComputedAt: time.Now()},
		}}
		require.Error(t, source.Submit(ctx, entries), "the store failure surfaces")

		// While the lease holds, the pair is not claimable again.
		reclaimed, err := source.Claim(ctx, 10, time.Second)
		require.NoError(t, err)
		assert.Empty(t, reclaimed, "the lease is still held")

		// Once the lease expires it comes back — never marked computed.
		time.Sleep(2 * time.Second)
		reclaimed, err = source.Claim(ctx, 10, time.Second)
		require.NoError(t, err)
		require.Len(t, reclaimed, 1)
		assert.Equal(t, claimed[0], reclaimed[0])
	})
}

func TestLocalSourceSubmitEmptyIsNoop(t *testing.T) {
	t.Parallel()

	source := refresh.NewLocalSource(memindex.New(time.Minute, nil), memstore.New())
	require.NoError(t, source.Submit(context.Background(), nil))
}
