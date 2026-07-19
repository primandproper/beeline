package query_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/query"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newHandler(t *testing.T, resolution int, ttl time.Duration) (*query.Handler, *memstore.Store, *memindex.Index) {
	t.Helper()

	store := memstore.New()
	index := memindex.New(ttl, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)

	return query.NewHandler(store, index, engine, nil, resolution, ttl), store, index
}

func TestEstimateSameCellCorrection(t *testing.T) {
	t.Parallel()

	// A coarse resolution guarantees two nearby points share a cell.
	handler, _, _ := newHandler(t, 5, time.Minute)

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7758, Lng: -122.4194} // ~100m north

	oCell, err := beeline.CellAt(origin, 5)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(dest, 5)
	require.NoError(t, err)
	require.Equal(t, oCell, dCell, "precondition: points must share a cell")

	res, err := handler.Estimate(context.Background(), origin, dest, "car")
	require.NoError(t, err)

	assert.Equal(t, query.SourceSameCell, res.Source)
	assert.Positive(t, res.Estimate.Distance, "correction must not collapse to zero distance")
	assert.InDelta(t, res.Estimate.Distance/10, res.Estimate.Duration, 1e-6)
}

func TestEstimateCacheHit(t *testing.T) {
	t.Parallel()

	handler, store, _ := newHandler(t, 9, time.Minute)
	ctx := context.Background()

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194} // ~2.2km north → different cell

	oCell, err := beeline.CellAt(origin, 9)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(dest, 9)
	require.NoError(t, err)
	require.NotEqual(t, oCell, dCell)

	key := beeline.PairKey{Origin: oCell, Dest: dCell, Profile: "car", Res: 9}
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 42, Distance: 420}, ComputedAt: time.Now()},
	}}))

	res, err := handler.Estimate(ctx, origin, dest, "car")
	require.NoError(t, err)

	assert.Equal(t, query.SourceCache, res.Source)
	assert.False(t, res.Stale)
	assert.InDelta(t, 42, res.Estimate.Duration, 1e-9)
}

func TestEstimateDemandFillOnMiss(t *testing.T) {
	t.Parallel()

	handler, store, _ := newHandler(t, 9, time.Minute)
	ctx := context.Background()

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194}

	require.Zero(t, store.Len())

	res, err := handler.Estimate(ctx, origin, dest, "car")
	require.NoError(t, err)

	assert.Equal(t, query.SourceDemand, res.Source)
	assert.Positive(t, res.Estimate.Distance)
	assert.Equal(t, 1, store.Len(), "a miss should be cached for next time")
}

func TestEstimateStaleHitBumps(t *testing.T) {
	t.Parallel()

	handler, store, index := newHandler(t, 9, time.Minute)
	ctx := context.Background()

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194}

	oCell, err := beeline.CellAt(origin, 9)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(dest, 9)
	require.NoError(t, err)

	key := beeline.PairKey{Origin: oCell, Dest: dCell, Profile: "car", Res: 9}
	require.NoError(t, index.Seed(ctx, []beeline.PairKey{key}))
	// Mark it computed two minutes ago so it reads as stale against a 1m TTL.
	require.NoError(t, index.MarkComputed(ctx, []beeline.PairKey{key}, time.Now().Add(-2*time.Minute)))
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 42, Distance: 420}, ComputedAt: time.Now().Add(-2 * time.Minute)},
	}}))

	res, err := handler.Estimate(ctx, origin, dest, "car")
	require.NoError(t, err)

	assert.Equal(t, query.SourceCache, res.Source)
	assert.True(t, res.Stale, "value older than the TTL must be reported stale")

	// The stale read should have bumped the pair to the front of the refresh queue.
	claimed, err := index.Claim(ctx, 10, time.Second)
	require.NoError(t, err)
	assert.Contains(t, claimed, key)
}
