package query_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/query"
	memstore "github.com/primandproper/beeline/internal/store/memory"
	"github.com/primandproper/beeline/internal/telemetry"

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
	targetTTL  time.Duration
	present    bool
}

func (f fakeRouter) Locate(beeline.LatLng) (beeline.RoutedArea, bool) {
	if !f.present {
		return beeline.RoutedArea{}, false
	}

	return beeline.RoutedArea{
		ID:        f.id,
		Layers:    []beeline.RoutedLayer{{Resolution: f.resolution, MaxRadiusMeters: f.maxRadius}},
		TargetTTL: f.targetTTL,
	}, true
}

const testArea = beeline.AreaID(1)

// fixedResolver serves every area (and out-of-area, id 0) through one engine, so the
// read-path tests exercise a single provider without a control coordinator.
type fixedResolver struct{ engine beeline.RoutingEngine }

func (r fixedResolver) EngineFor(beeline.AreaID) beeline.RoutingEngine { return r.engine }

func newHandler(t *testing.T, resolution int, ttl time.Duration) (*query.Handler, *memstore.Store, *memindex.Index) {
	t.Helper()

	store := memstore.New()
	index := memindex.New(ttl, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	router := fakeRouter{id: testArea, resolution: resolution, targetTTL: ttl, present: true}

	return query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, nil), store, index
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
		Key:      key,
		Duration: 42, Distance: 420, ComputedAt: time.Now(),
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
	handler := query.NewHandler(store, index, fixedResolver{engine: engine}, fakeRouter{present: false}, nil, nil)

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
	handler := query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, nil)

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
	handler := query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, nil)

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

// countingEngine wraps a RoutingEngine and counts Table calls, so table tests can
// assert the batch path groups misses into at most one engine call per source.
type countingEngine struct {
	inner beeline.RoutingEngine
	calls int
}

func (c *countingEngine) Table(ctx context.Context, req beeline.TableRequest) (beeline.TableResponse, error) {
	c.calls++

	return c.inner.Table(ctx, req)
}

func (c *countingEngine) Capabilities() beeline.Capabilities { return c.inner.Capabilities() }

// coordRouter routes exact coordinates to preconfigured areas, so a single table can
// span several areas at different resolutions. An absent coordinate is out of area.
type coordRouter struct {
	areas map[beeline.LatLng]beeline.RoutedArea
}

func (c coordRouter) Locate(p beeline.LatLng) (beeline.RoutedArea, bool) {
	a, ok := c.areas[p]

	return a, ok
}

func newTableHandler(t *testing.T, resolution int, ttl time.Duration, maxRadius float64) (*query.Handler, *memstore.Store, *memindex.Index, *countingEngine) {
	t.Helper()

	store := memstore.New()
	index := memindex.New(ttl, nil)
	engine := &countingEngine{inner: haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)}
	router := fakeRouter{id: testArea, resolution: resolution, maxRadius: maxRadius, targetTTL: ttl, present: true}

	return query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, nil), store, index, engine
}

// wide table coordinates: sources and destinations far enough apart that every cell is
// a distinct-cell cache lookup at resolution 9.
var (
	tableSources = []beeline.LatLng{{Lat: 37.7749, Lng: -122.4194}, {Lat: 37.7770, Lng: -122.4194}}
	tableDests   = []beeline.LatLng{{Lat: 37.7949, Lng: -122.4194}, {Lat: 37.7989, Lng: -122.4194}, {Lat: 37.8029, Lng: -122.4194}}
)

func TestTableFillComputesCachesAndGroups(t *testing.T) {
	t.Parallel()

	// Unbounded area: every within-area miss is cacheable.
	handler, store, _, engine := newTableHandler(t, 9, time.Minute, 0)

	result, err := handler.Table(context.Background(), &query.TableQuery{
		Sources:      tableSources,
		Destinations: tableDests,
		Profile:      "car",
		Fill:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, 6, result.Misses, "nothing was seeded, so every cell misses")
	assert.Equal(t, 6, result.Filled, "every miss is demand-filled")
	assert.Zero(t, result.Hits)
	assert.Equal(t, 6, store.Len(), "every within-bound miss is cached")
	assert.Equal(t, len(tableSources), engine.calls, "misses group into one 1×K call per source")

	for i := range result.Cells {
		for j := range result.Cells[i] {
			cell := result.Cells[i][j]
			assert.True(t, cell.Present, "filled cell must be present")
			assert.Equal(t, query.SourceDemand, cell.Source)
			assert.Positive(t, cell.Estimate.Distance)
		}
	}
}

func TestTableCacheHitsNeverCallEngine(t *testing.T) {
	t.Parallel()

	handler, store, _, engine := newTableHandler(t, 9, time.Minute, 0)
	ctx := context.Background()

	// Seed every cell so the whole grid is a hit.
	for i := range tableSources {
		oCell, err := beeline.CellAt(tableSources[i], 9)
		require.NoError(t, err)
		for j := range tableDests {
			dCell, dErr := beeline.CellAt(tableDests[j], 9)
			require.NoError(t, dErr)
			key := beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9}
			require.NoError(t, store.Put(ctx, []beeline.Entry{{
				Key:      key,
				Duration: 42, Distance: 420, ComputedAt: time.Now(),
			}}))
		}
	}

	result, err := handler.Table(ctx, &query.TableQuery{
		Sources:      tableSources,
		Destinations: tableDests,
		Profile:      "car",
		Fill:         true, // fill is irrelevant when everything hits
	})
	require.NoError(t, err)

	assert.Equal(t, 6, result.Hits)
	assert.Zero(t, result.Misses)
	assert.Zero(t, engine.calls, "a fully-cached table touches no engine")
	assert.InDelta(t, 42, result.Cells[0][0].Estimate.Duration, 1e-9)
}

func TestTableCacheOnlyLeavesMissesAbsent(t *testing.T) {
	t.Parallel()

	handler, store, _, engine := newTableHandler(t, 9, time.Minute, 0)
	ctx := context.Background()

	// Seed only cell (0,0); the rest miss.
	oCell, err := beeline.CellAt(tableSources[0], 9)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(tableDests[0], 9)
	require.NoError(t, err)
	seeded := beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9}
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:      seeded,
		Duration: 42, Distance: 420, ComputedAt: time.Now(),
	}}))

	result, err := handler.Table(ctx, &query.TableQuery{
		Sources:      tableSources,
		Destinations: tableDests,
		Profile:      "car",
		Fill:         false, // cache-only
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Hits)
	assert.Equal(t, 5, result.Misses)
	assert.Zero(t, result.Filled, "cache-only fills nothing")
	assert.Zero(t, engine.calls, "cache-only never calls the engine")
	assert.Equal(t, 1, store.Len(), "cache-only writes nothing new")

	assert.True(t, result.Cells[0][0].Present, "the seeded cell is present")
	assert.False(t, result.Cells[1][2].Present, "an unfilled miss is absent")
}

func TestTableSkipOmitsCells(t *testing.T) {
	t.Parallel()

	handler, store, _, engine := newTableHandler(t, 9, time.Minute, 0)
	ctx := context.Background()

	// Seed cell (0,0) so it hits; skip (0,1).
	oCell, err := beeline.CellAt(tableSources[0], 9)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(tableDests[0], 9)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:      beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9},
		Duration: 42, Distance: 420, ComputedAt: time.Now(),
	}}))

	result, err := handler.Table(ctx, &query.TableQuery{
		Sources:      tableSources[:1],
		Destinations: tableDests[:2],
		Profile:      "car",
		Skip:         map[[2]int]struct{}{{0, 1}: {}},
		Fill:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.Skipped)
	assert.Equal(t, 1, result.Hits)
	assert.Zero(t, result.Misses)
	assert.Zero(t, engine.calls, "the only non-hit cell was skipped")
	assert.True(t, result.Cells[0][0].Present)
	assert.False(t, result.Cells[0][1].Present, "a skipped cell is absent")
}

func TestTableSameCellComputesLiveAndDoesNotCache(t *testing.T) {
	t.Parallel()

	// Coarse resolution so origin and dest share a cell.
	handler, store, _, _ := newTableHandler(t, 5, time.Minute, 0)

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7758, Lng: -122.4194} // ~100 m north

	oCell, err := beeline.CellAt(origin, 5)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(dest, 5)
	require.NoError(t, err)
	require.Equal(t, oCell, dCell, "precondition: points share a cell")

	result, err := handler.Table(context.Background(), &query.TableQuery{
		Sources:      []beeline.LatLng{origin},
		Destinations: []beeline.LatLng{dest},
		Profile:      "car",
		Fill:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.SameCell)
	assert.Zero(t, store.Len(), "a same-cell correction is never cached")
	cell := result.Cells[0][0]
	assert.True(t, cell.Present)
	assert.Equal(t, query.SourceSameCell, cell.Source)
	assert.Positive(t, cell.Estimate.Distance, "correction must not collapse to zero")
}

func TestTableSameCellAbsentWhenCacheOnly(t *testing.T) {
	t.Parallel()

	handler, _, _, engine := newTableHandler(t, 5, time.Minute, 0)

	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7758, Lng: -122.4194}

	result, err := handler.Table(context.Background(), &query.TableQuery{
		Sources:      []beeline.LatLng{origin},
		Destinations: []beeline.LatLng{dest},
		Profile:      "car",
		Fill:         false,
	})
	require.NoError(t, err)

	assert.Equal(t, 1, result.SameCell)
	assert.Zero(t, engine.calls, "cache-only never computes a same-cell correction")
	assert.False(t, result.Cells[0][0].Present, "same-cell needs a live compute, absent under cache-only")
}

func TestTableOutOfAreaComputesButDoesNotCache(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := &countingEngine{inner: haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)}
	handler := query.NewHandler(store, index, fixedResolver{engine: engine}, fakeRouter{present: false}, nil, nil)

	result, err := handler.Table(context.Background(), &query.TableQuery{
		Sources:      tableSources[:1],
		Destinations: tableDests[:2],
		Profile:      "car",
		Fill:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, result.OutOfArea)
	assert.Zero(t, result.Misses, "out-of-area cells are not cache lookups")
	assert.Zero(t, store.Len(), "out-of-area answers are never cached")
	assert.Equal(t, 1, engine.calls, "both out-of-area dests share one source, one call")
	assert.True(t, result.Cells[0][0].Present)
	assert.Equal(t, query.SourceDemand, result.Cells[0][0].Source)
}

func TestTableBeyondBoundComputesButDoesNotCache(t *testing.T) {
	t.Parallel()

	// A tight 500 m bound: the ~2.2 km trips below fall outside it.
	handler, store, _, _ := newTableHandler(t, 9, time.Minute, 500)

	result, err := handler.Table(context.Background(), &query.TableQuery{
		Sources:      tableSources[:1],
		Destinations: tableDests[:2],
		Profile:      "car",
		Fill:         true,
	})
	require.NoError(t, err)

	assert.Equal(t, 2, result.Misses)
	assert.Equal(t, 2, result.Filled, "beyond-bound misses are still answered")
	assert.Zero(t, store.Len(), "beyond-bound misses must not be cached")
	assert.True(t, result.Cells[0][0].Present)
}

func TestTableSpansMultipleAreas(t *testing.T) {
	t.Parallel()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := &countingEngine{inner: haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)}

	const areaA, areaB = beeline.AreaID(1), beeline.AreaID(2)
	srcA := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	srcB := beeline.LatLng{Lat: 40.7128, Lng: -74.0060}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194} // near srcA; far from srcB

	router := coordRouter{areas: map[beeline.LatLng]beeline.RoutedArea{
		srcA: {ID: areaA, Layers: []beeline.RoutedLayer{{Resolution: 9}}, TargetTTL: time.Minute},
		srcB: {ID: areaB, Layers: []beeline.RoutedLayer{{Resolution: 7}}, TargetTTL: time.Minute},
	}}
	handler := query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, nil)
	ctx := context.Background()

	// Seed the srcA→dest cell in area A's partition only.
	oCellA, err := beeline.CellAt(srcA, 9)
	require.NoError(t, err)
	dCellA, err := beeline.CellAt(dest, 9)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:      beeline.PairKey{Area: areaA, Origin: oCellA, Dest: dCellA, Profile: "car", Res: 9},
		Duration: 42, Distance: 420, ComputedAt: time.Now(),
	}}))

	result, err := handler.Table(ctx, &query.TableQuery{
		Sources:      []beeline.LatLng{srcA, srcB},
		Destinations: []beeline.LatLng{dest},
		Profile:      "car",
		Fill:         false,
	})
	require.NoError(t, err)

	// srcA keys against area A (seeded → hit); srcB keys against area B's own
	// partition/resolution, where nothing is seeded → miss, absent under cache-only.
	assert.Equal(t, 1, result.Hits)
	assert.Equal(t, 1, result.Misses)
	assert.InDelta(t, 42, result.Cells[0][0].Estimate.Duration, 1e-9, "srcA hit its area-A cache entry")
	assert.False(t, result.Cells[1][0].Present, "srcB found nothing in its area-B partition")
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
		Key:      key,
		Duration: 42, Distance: 420, ComputedAt: time.Now().Add(-2 * time.Minute),
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

// captureRecorder collects telemetry events for tee assertions. The handler calls
// it synchronously, but a mutex keeps it honest under -race regardless.
type captureRecorder struct {
	events []telemetry.FetchEvent
	mu     sync.Mutex
}

func (c *captureRecorder) Record(ev *telemetry.FetchEvent) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.events = append(c.events, *ev)
}

func (c *captureRecorder) snapshot() []telemetry.FetchEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]telemetry.FetchEvent(nil), c.events...)
}

// The telemetry package duplicates the query Source strings (query imports
// telemetry, so they cannot be shared); this pins the two sets together.
func TestTelemetrySourceConstantsMatch(t *testing.T) {
	t.Parallel()

	assert.Equal(t, string(query.SourceCache), telemetry.SourceCache)
	assert.Equal(t, string(query.SourceSameCell), telemetry.SourceSameCell)
	assert.Equal(t, string(query.SourceDemand), telemetry.SourceDemand)
}

func TestEstimateTelemetryEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	origin := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	dest := beeline.LatLng{Lat: 37.7949, Lng: -122.4194} // ~2.2 km north → distinct cell at res 9

	oCell, err := beeline.CellAt(origin, 9)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(dest, 9)
	require.NoError(t, err)
	wantKey := beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9}

	build := func(router query.AreaRouter) (*query.Handler, *memstore.Store, *captureRecorder) {
		store := memstore.New()
		index := memindex.New(time.Minute, nil)
		engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
		rec := &captureRecorder{}

		return query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, rec), store, rec
	}
	inArea := fakeRouter{id: testArea, resolution: 9, targetTTL: time.Minute, present: true}

	t.Run("fresh hit", func(t *testing.T) {
		t.Parallel()

		handler, store, rec := build(inArea)
		require.NoError(t, store.Put(ctx, []beeline.Entry{{
			Key:      wantKey,
			Duration: 42, Distance: 420, ComputedAt: time.Now(),
		}}))

		_, estErr := handler.Estimate(ctx, origin, dest, "car")
		require.NoError(t, estErr)

		events := rec.snapshot()
		require.Len(t, events, 1)
		assert.Equal(t, telemetry.SourceCache, events[0].Source)
		assert.False(t, events[0].Stale)
		assert.Equal(t, wantKey, events[0].Key)
		assert.False(t, events[0].At.IsZero())
	})

	t.Run("stale hit", func(t *testing.T) {
		t.Parallel()

		handler, store, rec := build(inArea)
		require.NoError(t, store.Put(ctx, []beeline.Entry{{
			Key:      wantKey,
			Duration: 42, Distance: 420, ComputedAt: time.Now().Add(-2 * time.Minute),
		}}))

		_, estErr := handler.Estimate(ctx, origin, dest, "car")
		require.NoError(t, estErr)

		events := rec.snapshot()
		require.Len(t, events, 1)
		assert.Equal(t, telemetry.SourceCache, events[0].Source)
		assert.True(t, events[0].Stale)
	})

	t.Run("demand fill", func(t *testing.T) {
		t.Parallel()

		handler, _, rec := build(inArea)

		_, estErr := handler.Estimate(ctx, origin, dest, "car")
		require.NoError(t, estErr)

		events := rec.snapshot()
		require.Len(t, events, 1)
		assert.Equal(t, telemetry.SourceDemand, events[0].Source)
		assert.Equal(t, wantKey, events[0].Key)
	})

	t.Run("beyond-bound demand is still recorded", func(t *testing.T) {
		t.Parallel()

		handler, store, rec := build(fakeRouter{id: testArea, resolution: 9, maxRadius: 500, targetTTL: time.Minute, present: true})

		_, estErr := handler.Estimate(ctx, origin, dest, "car")
		require.NoError(t, estErr)

		assert.Zero(t, store.Len(), "beyond the bound nothing is cached")
		events := rec.snapshot()
		require.Len(t, events, 1, "but the demand is real and recorded")
		assert.Equal(t, telemetry.SourceDemand, events[0].Source)
	})

	t.Run("same cell", func(t *testing.T) {
		t.Parallel()

		handler, _, rec := build(fakeRouter{id: testArea, resolution: 5, targetTTL: time.Minute, present: true})
		nearDest := beeline.LatLng{Lat: 37.7758, Lng: -122.4194} // ~100 m: same res-5 cell

		cell, cellErr := beeline.CellAt(origin, 5)
		require.NoError(t, cellErr)

		_, estErr := handler.Estimate(ctx, origin, nearDest, "car")
		require.NoError(t, estErr)

		events := rec.snapshot()
		require.Len(t, events, 1)
		assert.Equal(t, telemetry.SourceSameCell, events[0].Source)
		assert.Equal(t, cell, events[0].Key.Origin)
		assert.Equal(t, cell, events[0].Key.Dest, "a same-cell event carries the shared cell twice")
		assert.Equal(t, testArea, events[0].Key.Area)
		assert.Equal(t, 5, events[0].Key.Res)
	})

	t.Run("out of area records nothing", func(t *testing.T) {
		t.Parallel()

		handler, _, rec := build(fakeRouter{present: false})

		_, estErr := handler.Estimate(ctx, origin, dest, "car")
		require.NoError(t, estErr)

		assert.Empty(t, rec.snapshot(), "no area, no resolution, no event")
	})
}

func TestTableTelemetryEvents(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	router := fakeRouter{id: testArea, resolution: 9, targetTTL: time.Minute, present: true}
	rec := &captureRecorder{}
	handler := query.NewHandler(store, index, fixedResolver{engine: engine}, router, nil, rec)

	// Pre-cache source0→dest0 so the grid mixes a hit with filled misses.
	oCell, err := beeline.CellAt(tableSources[0], 9)
	require.NoError(t, err)
	dCell, err := beeline.CellAt(tableDests[0], 9)
	require.NoError(t, err)
	require.NoError(t, store.Put(ctx, []beeline.Entry{{
		Key:      beeline.PairKey{Area: testArea, Origin: oCell, Dest: dCell, Profile: "car", Res: 9},
		Duration: 42, Distance: 420, ComputedAt: time.Now(),
	}}))

	result, err := handler.Table(ctx, &query.TableQuery{
		Sources:      tableSources,
		Destinations: tableDests,
		Profile:      "car",
		Fill:         true,
		Skip:         map[[2]int]struct{}{{1, 2}: {}},
	})
	require.NoError(t, err)
	require.Equal(t, 1, result.Hits)
	require.Equal(t, 1, result.Skipped)

	events := rec.snapshot()
	// 2×3 grid minus one skipped cell: one hit + four filled misses, every one recorded.
	require.Len(t, events, 5)

	bySource := map[string]int{}
	for _, ev := range events {
		bySource[ev.Source]++
		assert.Equal(t, testArea, ev.Key.Area)
		assert.Equal(t, 9, ev.Key.Res)
	}
	assert.Equal(t, map[string]int{telemetry.SourceCache: 1, telemetry.SourceDemand: 4}, bySource)
}
