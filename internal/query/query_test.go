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

// fakeRouter routes every coordinate into one area at a fixed resolution and travel
// bound, unless present is false, in which case every coordinate is out of area. A
// zero maxRadius is the full-mesh sentinel (unbounded), matching the domain contract.
type fakeRouter struct {
	id         beeline.AreaID
	resolution int
	maxRadius  float64
	present    bool
}

func (f fakeRouter) Locate(beeline.LatLng) (beeline.RoutedArea, bool) {
	if !f.present {
		return beeline.RoutedArea{}, false
	}

	return beeline.RoutedArea{ID: f.id, Resolution: f.resolution, MaxRadiusMeters: f.maxRadius}, true
}

const testArea = beeline.AreaID(1)

func newHandler(t *testing.T, resolution int, ttl time.Duration) (*query.Handler, *memstore.Store, *memindex.Index) {
	t.Helper()

	store := memstore.New()
	index := memindex.New(ttl, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	router := fakeRouter{id: testArea, resolution: resolution, present: true}

	return query.NewHandler(store, index, engine, router, nil, ttl), store, index
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

	key := beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9}
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

func TestEstimateOutOfAreaComputesButDoesNotCache(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	handler := query.NewHandler(store, index, engine, fakeRouter{present: false}, nil, time.Minute)

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194}

	res, err := handler.Estimate(context.Background(), origin, dest, "car")
	require.NoError(t, err)

	assert.Equal(t, query.SourceDemand, res.Source)
	assert.Positive(t, res.Estimate.Distance, "a coordinate outside every area is still answered")
	assert.Zero(t, store.Len(), "an out-of-area answer is not cached")
}

func TestEstimateBeyondBoundComputesButDoesNotCache(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	// A tight 500 m bound: the ~2.2 km trip below falls outside it.
	router := fakeRouter{id: testArea, resolution: 9, maxRadius: 500, present: true}
	handler := query.NewHandler(store, index, engine, router, nil, time.Minute)

	ctx := context.Background()
	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194} // ~2.2 km north, beyond the bound

	res, err := handler.Estimate(ctx, origin, dest, "car")
	require.NoError(t, err)

	assert.Equal(t, query.SourceDemand, res.Source)
	assert.Positive(t, res.Estimate.Distance, "a beyond-bound trip is still answered")
	assert.Zero(t, store.Len(), "a beyond-bound answer must not be cached")

	debt, err := index.Debt(ctx)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet, "a beyond-bound answer must not enter the working set")
}

func TestEstimateWithinBoundDemandFillIsTracked(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	// A generous 10 km bound easily contains the ~2.2 km trip.
	router := fakeRouter{id: testArea, resolution: 9, maxRadius: 10000, present: true}
	handler := query.NewHandler(store, index, engine, router, nil, time.Minute)

	ctx := context.Background()
	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194}

	res, err := handler.Estimate(ctx, origin, dest, "car")
	require.NoError(t, err)
	assert.Equal(t, query.SourceDemand, res.Source)
	assert.Equal(t, 1, store.Len(), "a within-bound miss is cached")

	// The demand-filled pair must be tracked in the working set (an unseeded lazy-area
	// pair that MarkComputed alone would have dropped), and it is marked fresh.
	debt, err := index.Debt(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, debt.WorkingSet, "a within-bound miss enters the working set")
	assert.Zero(t, debt.Debt, "the demand-filled pair is fresh, not debt")
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

	key := beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9}
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
