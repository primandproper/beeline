package control_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/engine/registry"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	memstore "github.com/primandproper/beeline/internal/store/memory"
	areasqlite "github.com/primandproper/beeline/internal/store/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/h3-go/v4"
)

const (
	testRes      = 8
	sfLat        = 37.7749
	sfLng        = -122.4194
	testTTL      = 60_000_000_000 // 1 minute in nanoseconds
	testProvider = "osrm-test"    // the named provider newHarness registers alongside the default
)

// harness bundles a coordinator with the concrete seams it drives, so tests can assert
// against the shared index/store as well as the persisted repo.
type harness struct {
	repo  *areasqlite.Repository
	index *memindex.Index
	store *memstore.Store
	coord *control.Coordinator
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	db, err := areasqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	repo := areasqlite.NewRepository(db, nil)
	index := memindex.New(testTTL, nil)
	store := memstore.New()

	// A registry with the built-in default plus one named OSRM provider, so tests can
	// exercise per-area provider selection and unknown-provider rejection.
	providers, err := registry.Build(
		map[string]config.ProviderConfig{
			testProvider: {Type: config.ProviderTypeOSRM, BaseURL: "http://osrm-test:5000", MaxTableSize: 10000},
		},
		map[beeline.Profile]float64{"car": 10},
		config.EngineLatencyConfig{},
	)
	require.NoError(t, err)

	coord := control.New(repo, index, store, []beeline.Profile{"car"}, providers, config.DefaultProviderName, control.FreshnessDefaults{
		TargetTTL:     testTTL,
		LeaseDuration: 15 * time.Second,
		SweepInterval: time.Second,
	})

	return &harness{repo: repo, index: index, store: store, coord: coord}
}

// diskCells returns the res-8 GridDisk of radius r around San Francisco.
func diskCells(t *testing.T, r int) []beeline.H3Cell {
	t.Helper()

	center, err := beeline.CellAt(beeline.LatLng{Lat: sfLat, Lng: sfLng}, testRes)
	require.NoError(t, err)
	cells, err := h3.GridDisk(center, r)
	require.NoError(t, err)

	return cells
}

// geoJSONForCells renders a cell set's exact outline as a GeoJSON MultiPolygon.
// Polyfilling it back at the cells' resolution reproduces the set (every original
// cell's center is inside the outline; every other cell's is outside), so tests can
// reason about a GeoJSON-canonical area in terms of a known cell disk.
func geoJSONForCells(t *testing.T, cells []beeline.H3Cell) []byte {
	t.Helper()

	polys, err := h3.CellsToMultiPolygon(cells)
	require.NoError(t, err)
	require.NotEmpty(t, polys)

	ring := func(loop h3.GeoLoop) [][]float64 {
		out := make([][]float64, 0, len(loop)+1)
		for _, v := range loop {
			out = append(out, []float64{v.Lng, v.Lat}) // GeoJSON is [lng,lat]
		}

		return append(out, out[0]) // close the ring
	}

	multi := make([][][][]float64, 0, len(polys))
	for i := range polys {
		poly := [][][]float64{ring(polys[i].GeoLoop)}
		for _, hole := range polys[i].Holes {
			poly = append(poly, ring(hole))
		}
		multi = append(multi, poly)
	}

	raw, err := json.Marshal(map[string]any{"type": "MultiPolygon", "coordinates": multi})
	require.NoError(t, err)

	return raw
}

// diskGeoJSON is the geometry whose res-8 polyfill is the r-ring SF disk.
func diskGeoJSON(t *testing.T, r int) []byte {
	t.Helper()

	return geoJSONForCells(t, diskCells(t, r))
}

// layers1 is the common one-layer list at the test resolution.
func layers1(maxRadius, coreRadius float64) []beeline.Layer {
	return []beeline.Layer{{Resolution: testRes, MaxRadiusMeters: maxRadius, CoreRadiusMeters: coreRadius}}
}

// resolutions projects a layer list to its resolution order.
func resolutions(layers []beeline.Layer) []int {
	out := make([]int, 0, len(layers))
	for i := range layers {
		out = append(out, layers[i].Resolution)
	}

	return out
}

func createArea(t *testing.T, h *harness, diskRings int) beeline.Area {
	t.Helper()

	area, err := h.coord.Create(context.Background(), &control.CreateAreaInput{
		Name:    "test",
		GeoJSON: diskGeoJSON(t, diskRings),
		Layers:  layers1(1500, 0),
	})
	require.NoError(t, err)

	return area
}

// createAreaWith creates an area with an explicit warm strategy, layers, and bounds.
func createAreaWith(t *testing.T, h *harness, in *control.CreateAreaInput) beeline.Area {
	t.Helper()

	area, err := h.coord.Create(context.Background(), in)
	require.NoError(t, err)

	return area
}

func TestLazyAreaSeedsNothingButRoutes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "lazy", GeoJSON: diskGeoJSON(t, 2), Layers: layers1(3000, 0),
		WarmStrategy: beeline.WarmLazy,
	})
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	debt, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet, "a lazy area seeds no pairs eagerly")

	// It still routes reads, carrying the bound so the read path can demand-fill.
	routed, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	require.True(t, ok)
	assert.Equal(t, area.ID, routed.ID)
	assert.InDelta(t, 3000, routed.ReadLayer().MaxRadiusMeters, 1e-9)
}

func TestHybridSeedsCoreNotFullBound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	geo := diskGeoJSON(t, 5) // a roomy area so the radius, not the area, clips

	eager := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "eager", GeoJSON: geo, Layers: layers1(6000, 0),
		WarmStrategy: beeline.WarmEager,
	})
	_, err := h.coord.Enable(ctx, eager.ID)
	require.NoError(t, err)

	hybrid := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "hybrid", GeoJSON: geo, Layers: layers1(6000, 1500),
		WarmStrategy: beeline.WarmHybrid,
	})
	_, err = h.coord.Enable(ctx, hybrid.ID)
	require.NoError(t, err)

	eagerDebt, err := h.index.DebtForArea(ctx, eager.ID)
	require.NoError(t, err)
	hybridDebt, err := h.index.DebtForArea(ctx, hybrid.ID)
	require.NoError(t, err)

	assert.Positive(t, hybridDebt.WorkingSet, "hybrid pins its core")
	assert.Less(t, hybridDebt.WorkingSet, eagerDebt.WorkingSet,
		"hybrid's core (1.5km) is a subset of eager's full bound (6km)")
}

func TestMultiLayerEnableSeedsEveryLayer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	// Layers supplied coarsest-first on purpose: the coordinator must sort them
	// finest→coarsest. The geometry is a ~9 km res-8 disk, roomy enough that its
	// res-7 polyfill is non-empty too.
	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name:    "multi",
		GeoJSON: diskGeoJSON(t, 4),
		Layers: []beeline.Layer{
			{Resolution: 7, MinDistanceMeters: 2000, MaxRadiusMeters: 6000},
			{Resolution: testRes, MinDistanceMeters: 0, MaxRadiusMeters: 3000},
		},
	})
	assert.Equal(t, []int{8, 7}, resolutions(area.Layers), "layers are stored finest→coarsest")

	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	// Both layers' origin cells are seeded, distinguishable by cell resolution.
	states, err := h.index.CellStatesForArea(ctx, area.ID)
	require.NoError(t, err)
	seededRes := map[int]int{}
	for _, s := range states {
		seededRes[s.Origin.Resolution()]++
	}
	assert.Positive(t, seededRes[8], "the finest layer seeds origin cells")
	assert.Positive(t, seededRes[7], "the coarse layer seeds origin cells too")

	// Locate routes at the finest layer and carries the whole list, finest first.
	routed, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	require.True(t, ok)
	require.Len(t, routed.Layers, 2)
	assert.Equal(t, 8, routed.Layers[0].Resolution)
	assert.Equal(t, 7, routed.Layers[1].Resolution)
	assert.InDelta(t, 2000, routed.Layers[1].MinDistanceMeters, 1e-9)
	assert.Equal(t, 8, routed.ReadLayer().Resolution)

	// Disable tears down every layer's pairs.
	_, err = h.coord.Disable(ctx, area.ID)
	require.NoError(t, err)
	debt, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet, "disabling clears all layers")
}

func TestSweepExpiredEvictsColdDemandFromIndexAndStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	cells := diskCells(t, 2)
	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "lazy", GeoJSON: diskGeoJSON(t, 2), Layers: layers1(3000, 0),
		WarmStrategy: beeline.WarmLazy, DemandIdleTTL: time.Hour,
	})
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	// Simulate a demand-fill: the read path tracks the pair (Access) and caches it.
	key := beeline.PairKey{Area: area.ID, Origin: cells[0], Dest: cells[1], Profile: "car", Res: testRes}
	require.NoError(t, h.index.Access(ctx, []beeline.PairKey{key}))
	require.NoError(t, h.store.Put(ctx, []beeline.Entry{{
		Key:    key,
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1, Distance: 2}},
	}}))

	debt, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	require.Equal(t, 1, debt.WorkingSet, "the demand pair is tracked")
	require.Equal(t, 1, h.store.Len())

	// Within the TTL nothing is swept.
	swept, err := h.coord.SweepDue(ctx, time.Now())
	require.NoError(t, err)
	assert.Zero(t, swept, "a freshly-accessed pair is not cold yet")
	assert.Equal(t, 1, h.store.Len())

	// Past the TTL the pair is evicted from both the index and the hot store.
	swept, err = h.coord.SweepDue(ctx, time.Now().Add(2*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, swept, "the cold demand pair is swept")

	debt, err = h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet, "the pair left the working set")
	assert.Zero(t, h.store.Len(), "the pair left the hot store")
}

func TestSweepExpiredSkipsDecayDisabledAndPinned(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	cells := diskCells(t, 2)
	geo := diskGeoJSON(t, 2)

	// An eager area: its whole bound is pinned, so nothing ever decays.
	eager := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "eager", GeoJSON: geo, Layers: layers1(3000, 0),
		WarmStrategy: beeline.WarmEager, DemandIdleTTL: time.Hour,
	})
	_, err := h.coord.Enable(ctx, eager.ID)
	require.NoError(t, err)
	eagerBefore, err := h.index.DebtForArea(ctx, eager.ID)
	require.NoError(t, err)

	// A lazy area with decay disabled (TTL 0): demand pairs live until disable.
	lazy := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "lazy", GeoJSON: geo, Layers: layers1(3000, 0),
		WarmStrategy: beeline.WarmLazy, DemandIdleTTL: 0,
	})
	_, err = h.coord.Enable(ctx, lazy.ID)
	require.NoError(t, err)
	key := beeline.PairKey{Area: lazy.ID, Origin: cells[0], Dest: cells[1], Profile: "car", Res: testRes}
	require.NoError(t, h.index.Access(ctx, []beeline.PairKey{key}))

	// Even far in the future, nothing is swept.
	swept, err := h.coord.SweepDue(ctx, time.Now().Add(9000*time.Hour))
	require.NoError(t, err)
	assert.Zero(t, swept, "pinned pairs and decay-disabled areas are never swept")

	eagerAfter, err := h.index.DebtForArea(ctx, eager.ID)
	require.NoError(t, err)
	assert.Equal(t, eagerBefore.WorkingSet, eagerAfter.WorkingSet, "eager core intact")

	lazyDebt, err := h.index.DebtForArea(ctx, lazy.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, lazyDebt.WorkingSet, "decay-disabled demand pair survives")
}

func TestCreateRequiresGeoJSON(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.coord.Create(context.Background(), &control.CreateAreaInput{
		Name: "no-geometry", Layers: layers1(1500, 0),
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "geojson")
}

func TestLayerValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	geo := diskGeoJSON(t, 1)

	cases := []struct {
		name     string
		strategy beeline.WarmStrategy
		layers   []beeline.Layer
	}{
		{name: "no layers", layers: nil},
		{name: "duplicate resolutions", layers: []beeline.Layer{
			{Resolution: testRes, MaxRadiusMeters: 1500},
			{Resolution: testRes, MaxRadiusMeters: 3000},
		}},
		{name: "resolution out of range", layers: []beeline.Layer{
			{Resolution: 16, MaxRadiusMeters: 1500},
		}},
		{name: "negative min distance", layers: []beeline.Layer{
			{Resolution: testRes, MinDistanceMeters: -1, MaxRadiusMeters: 1500},
		}},
		{name: "finest layer with nonzero min distance", layers: []beeline.Layer{
			{Resolution: testRes, MinDistanceMeters: 100, MaxRadiusMeters: 1500},
		}},
		{name: "min distance decreasing toward coarser", layers: []beeline.Layer{
			{Resolution: 8, MinDistanceMeters: 0, MaxRadiusMeters: 1500},
			{Resolution: 7, MinDistanceMeters: 5000, MaxRadiusMeters: 6000},
			{Resolution: 6, MinDistanceMeters: 2000, MaxRadiusMeters: 12000},
		}},
		{name: "negative max radius", layers: []beeline.Layer{
			{Resolution: testRes, MaxRadiusMeters: -1},
		}},
		{name: "full-mesh layer with non-eager strategy", strategy: beeline.WarmLazy, layers: []beeline.Layer{
			{Resolution: testRes, MaxRadiusMeters: 0},
		}},
		{name: "core exceeds max", strategy: beeline.WarmHybrid, layers: []beeline.Layer{
			{Resolution: testRes, MaxRadiusMeters: 1000, CoreRadiusMeters: 5000},
		}},
		// 500m at res 8 reaches no neighbor cell (cells are ~900m apart), so every
		// origin would pair only with itself — the degenerate case the floor forbids.
		{name: "bounded radius below the neighbor floor", layers: []beeline.Layer{
			{Resolution: testRes, MaxRadiusMeters: 500},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := h.coord.Create(ctx, &control.CreateAreaInput{
				Name: "bad", GeoJSON: geo, Layers: tc.layers, WarmStrategy: tc.strategy,
			})
			assert.Error(t, err)
		})
	}

	t.Run("unknown strategy is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "weird", GeoJSON: geo, Layers: layers1(1000, 0),
			WarmStrategy: beeline.WarmStrategy("aggressive"),
		})
		assert.Error(t, err)
	})

	t.Run("empty strategy defaults to eager and is accepted", func(t *testing.T) {
		t.Parallel()

		area, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "default", GeoJSON: geo, Layers: layers1(1000, 0),
		})
		require.NoError(t, err)
		assert.Equal(t, beeline.WarmEager, area.WarmStrategy)
	})
}

func TestCreateIsDisabledAndNotSeeded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 1)
	assert.False(t, area.Enabled, "new areas are disabled")

	debt, err := h.index.Debt(ctx)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet, "a disabled area seeds no pairs")

	_, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	assert.False(t, ok, "a disabled area does not route reads")
}

func TestEnableSeedsAndRoutes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 1)
	enabled, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)
	assert.True(t, enabled.Enabled)

	debt, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Positive(t, debt.WorkingSet, "enabling seeds the area's pairs")

	routed, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	require.True(t, ok, "an enabled area routes a contained point")
	assert.Equal(t, area.ID, routed.ID)
	assert.Equal(t, testRes, routed.ReadLayer().Resolution)
}

func TestDisableUnseedsAndClearsStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 1)
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	// Simulate the refresh pool having cached an estimate for this area.
	require.NoError(t, h.store.Put(ctx, []beeline.Entry{{
		Key: beeline.PairKey{Area: area.ID, Origin: 1, Dest: 2, Profile: "car", Res: testRes},
	}}))
	require.Equal(t, 1, h.store.Len())

	_, err = h.coord.Disable(ctx, area.ID)
	require.NoError(t, err)

	debt, err := h.index.Debt(ctx)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet, "disabling removes the area's pairs")
	assert.Zero(t, h.store.Len(), "disabling clears the area's cached estimates")

	_, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	assert.False(t, ok)
}

func TestSetGeoJSONConvergesEnabledArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 1)
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	before, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)

	// Replace the geometry with the 2-ring disk; the outer ring adds cells (and pairs).
	_, err = h.coord.SetGeoJSON(ctx, area.ID, diskGeoJSON(t, 2))
	require.NoError(t, err)

	after, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Greater(t, after.WorkingSet, before.WorkingSet, "the grown geometry grows the working set")

	// A point in a newly covered outer cell now routes into the area.
	oneSet := make(map[beeline.H3Cell]struct{})
	for _, c := range diskCells(t, 1) {
		oneSet[c] = struct{}{}
	}
	var outer beeline.H3Cell
	for _, c := range diskCells(t, 2) {
		if _, ok := oneSet[c]; !ok {
			outer = c
			break
		}
	}
	require.NotZero(t, outer)
	center, err := beeline.Center(outer)
	require.NoError(t, err)
	_, ok := h.coord.Locate(center)
	assert.True(t, ok, "a newly covered cell routes reads")
}

func TestDeleteRemovesEverything(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 1)
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	require.NoError(t, h.coord.Delete(ctx, area.ID))

	_, err = h.coord.Get(ctx, area.ID)
	assert.ErrorIs(t, err, areasqlite.ErrNotFound)

	debt, err := h.index.Debt(ctx)
	require.NoError(t, err)
	assert.Zero(t, debt.WorkingSet)

	_, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	assert.False(t, ok)
}

func TestResumeEnabledSeedsOnlyEnabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	enabledArea := createArea(t, h, 1)
	_, err := h.coord.Enable(ctx, enabledArea.ID)
	require.NoError(t, err)

	// A second, disabled area sharing the same repo.
	_, err = h.coord.Create(ctx, &control.CreateAreaInput{
		Name: "disabled", GeoJSON: diskGeoJSON(t, 1), Layers: layers1(1500, 0),
	})
	require.NoError(t, err)

	// Simulate a restart: a brand-new coordinator with a fresh index/store over the
	// same persisted areas.
	freshIndex := memindex.New(testTTL, nil)
	freshStore := memstore.New()
	freshProviders, err := registry.Build(nil, map[beeline.Profile]float64{"car": 10}, config.EngineLatencyConfig{})
	require.NoError(t, err)
	fresh := control.New(h.repo, freshIndex, freshStore, []beeline.Profile{"car"}, freshProviders, config.DefaultProviderName, control.FreshnessDefaults{
		TargetTTL:     testTTL,
		LeaseDuration: 15 * time.Second,
		SweepInterval: time.Second,
	})

	require.NoError(t, fresh.ResumeEnabled(ctx))

	enabled := fresh.EnabledAreas()
	require.Len(t, enabled, 1, "only the enabled area resumes")
	assert.Equal(t, enabledArea.ID, enabled[0])

	debt, err := freshIndex.DebtForArea(ctx, enabledArea.ID)
	require.NoError(t, err)
	assert.Positive(t, debt.WorkingSet)
}

func TestCreateDefaultsProviderToHaversine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// No RoutingProvider set → normalized and persisted as the built-in default.
	area := createArea(t, h, 1)
	assert.Equal(t, config.DefaultProviderName, area.RoutingProvider)

	got, err := h.coord.Get(context.Background(), area.ID)
	require.NoError(t, err)
	assert.Equal(t, config.DefaultProviderName, got.RoutingProvider)
}

func TestCreateRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.coord.Create(context.Background(), &control.CreateAreaInput{
		Name: "bad", GeoJSON: diskGeoJSON(t, 1), Layers: layers1(1500, 0),
		RoutingProvider: "nope",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown routing provider")
}

func TestEngineForSelectsPerAreaProvider(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	// An area on the named OSRM provider (max table size 10000, per newHarness).
	osrmArea, err := h.coord.Create(ctx, &control.CreateAreaInput{
		Name: "osrm", GeoJSON: diskGeoJSON(t, 1), Layers: layers1(1500, 0),
		RoutingProvider: testProvider,
	})
	require.NoError(t, err)
	_, err = h.coord.Enable(ctx, osrmArea.ID)
	require.NoError(t, err)

	// A second area on the built-in default (unbounded table size).
	defaultArea := createArea(t, h, 1)
	_, err = h.coord.Enable(ctx, defaultArea.ID)
	require.NoError(t, err)

	assert.Equal(t, 10000, h.coord.EngineFor(osrmArea.ID).Capabilities().MaxTableSize,
		"the OSRM area routes through its named provider")
	assert.Equal(t, 0, h.coord.EngineFor(defaultArea.ID).Capabilities().MaxTableSize,
		"the default area routes through the built-in engine")
	assert.Equal(t, 0, h.coord.EngineFor(0).Capabilities().MaxTableSize,
		"an out-of-area query falls back to the default engine")
}

func TestUpdateChangesProvider(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 1)
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)
	require.Equal(t, 0, h.coord.EngineFor(area.ID).Capabilities().MaxTableSize)

	updated, err := h.coord.Update(ctx, area.ID, &control.UpdateAreaInput{
		Name: area.Name, Layers: area.Layers,
		RoutingProvider: testProvider,
	})
	require.NoError(t, err)
	assert.Equal(t, testProvider, updated.RoutingProvider)
	assert.Equal(t, 10000, h.coord.EngineFor(area.ID).Capabilities().MaxTableSize,
		"re-converging the enabled area picks up the new provider's engine")
}

func TestUpdateReplacesLayerList(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, 2) // one res-8 layer
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	// Grow to two layers; the enabled area re-converges and seeds both.
	updated, err := h.coord.Update(ctx, area.ID, &control.UpdateAreaInput{
		Name: area.Name,
		Layers: []beeline.Layer{
			{Resolution: testRes, MaxRadiusMeters: 3000},
			{Resolution: 7, MinDistanceMeters: 2500, MaxRadiusMeters: 6000},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []int{8, 7}, resolutions(updated.Layers))

	routed, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	require.True(t, ok)
	assert.Len(t, routed.Layers, 2, "the re-converged snapshot carries both layers")
}

func TestProviderNamesListsDefaultFirst(t *testing.T) {
	t.Parallel()

	names := newHarness(t).coord.ProviderNames()
	require.NotEmpty(t, names)
	assert.Equal(t, config.DefaultProviderName, names[0], "the default is listed first")
	assert.Contains(t, names, testProvider)
}

func TestWarmPairsValidation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name:         "warm",
		WarmStrategy: beeline.WarmLazy,
		GeoJSON:      diskGeoJSON(t, 1),
		Layers:       layers1(1500, 0),
	})

	cells := diskCells(t, 1)
	valid := beeline.PairKey{Origin: cells[0], Dest: cells[1], Profile: "car", Res: testRes}

	// Not enabled yet: every warm attempt is rejected.
	_, err := h.coord.WarmPairs(ctx, area.ID, []beeline.PairKey{valid}, false)
	require.ErrorIs(t, err, control.ErrInvalidWarm)

	_, err = h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	outside, err := beeline.CellAt(beeline.LatLng{Lat: 0, Lng: 0}, testRes)
	require.NoError(t, err)

	cases := map[string]beeline.PairKey{
		"wrong resolution":        {Origin: cells[0], Dest: cells[1], Profile: "car", Res: testRes + 1},
		"origin outside the area": {Origin: outside, Dest: cells[1], Profile: "car", Res: testRes},
		"dest outside the area":   {Origin: cells[0], Dest: outside, Profile: "car", Res: testRes},
		"unknown profile":         {Origin: cells[0], Dest: cells[1], Profile: "hovercraft", Res: testRes},
		"origin equals dest":      {Origin: cells[0], Dest: cells[0], Profile: "car", Res: testRes},
	}
	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, warmErr := h.coord.WarmPairs(ctx, area.ID, []beeline.PairKey{valid, bad}, false)
			require.ErrorIs(t, warmErr, control.ErrInvalidWarm, "one bad pair rejects the whole request")
		})
	}

	_, err = h.coord.WarmPairs(ctx, area.ID, nil, false)
	require.ErrorIs(t, err, control.ErrInvalidWarm, "an empty request is rejected")

	_, err = h.coord.WarmPairs(ctx, area.ID+999, []beeline.PairKey{valid}, false)
	require.ErrorIs(t, err, control.ErrInvalidWarm, "an unknown area is rejected")
}

func TestWarmPairsBumpIsClaimableAndDecays(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name:          "warm-bump",
		WarmStrategy:  beeline.WarmLazy,
		GeoJSON:       diskGeoJSON(t, 1),
		Layers:        layers1(1500, 0),
		DemandIdleTTL: time.Minute,
	})
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	cells := diskCells(t, 1)
	pair := beeline.PairKey{Origin: cells[0], Dest: cells[1], Profile: "car", Res: testRes}

	accepted, err := h.coord.WarmPairs(ctx, area.ID, []beeline.PairKey{pair}, false)
	require.NoError(t, err)
	assert.Equal(t, 1, accepted)

	// The Area is stamped by the coordinator: the caller's zero Area still lands in
	// the right partition, at top refresh priority.
	want := pair
	want.Area = area.ID
	claimed, err := h.index.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	assert.Equal(t, []beeline.PairKey{want}, claimed, "a lazy area holds nothing else; the warmed pair is claimable immediately")
	require.NoError(t, h.index.MarkComputed(ctx, claimed, time.Now()))

	// Bump-mode warming is decayable demand: once idle past the cutoff it sweeps away.
	removed, err := h.index.SweepArea(ctx, area.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Equal(t, []beeline.PairKey{want}, removed, "a wrong prediction decays through the normal sweep")
}

func TestWarmPairsSeedModeSurvivesSweep(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name:          "warm-seed",
		WarmStrategy:  beeline.WarmLazy,
		GeoJSON:       diskGeoJSON(t, 1),
		Layers:        layers1(1500, 0),
		DemandIdleTTL: time.Minute,
	})
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	cells := diskCells(t, 1)
	pair := beeline.PairKey{Origin: cells[0], Dest: cells[1], Profile: "car", Res: testRes}

	accepted, err := h.coord.WarmPairs(ctx, area.ID, []beeline.PairKey{pair}, true)
	require.NoError(t, err)
	assert.Equal(t, 1, accepted)

	removed, err := h.index.SweepArea(ctx, area.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, removed, "a pinned (seed-mode) pair survives the demand sweep")

	claimed, err := h.index.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	want := pair
	want.Area = area.ID
	assert.Equal(t, []beeline.PairKey{want}, claimed, "and is claimable at top priority")
}
