package control_test

import (
	"context"
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

func createArea(t *testing.T, h *harness, cells []beeline.H3Cell) beeline.Area {
	t.Helper()

	area, err := h.coord.Create(context.Background(), &control.CreateAreaInput{
		Name:            "test",
		Resolution:      testRes,
		MaxRadiusMeters: 1500,
		Cells:           cells,
	})
	require.NoError(t, err)

	return area
}

// createAreaWith creates an area with an explicit warm strategy and bounds.
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
		Name: "lazy", Resolution: testRes, MaxRadiusMeters: 3000,
		WarmStrategy: beeline.WarmLazy, Cells: diskCells(t, 2),
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
	assert.InDelta(t, 3000, routed.MaxRadiusMeters, 1e-9)
}

func TestHybridSeedsCoreNotFullBound(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	cells := diskCells(t, 5) // a roomy area so the radius, not the area, clips

	eager := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "eager", Resolution: testRes, MaxRadiusMeters: 6000,
		WarmStrategy: beeline.WarmEager, Cells: cells,
	})
	_, err := h.coord.Enable(ctx, eager.ID)
	require.NoError(t, err)

	hybrid := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "hybrid", Resolution: testRes, MaxRadiusMeters: 6000, CoreRadiusMeters: 1500,
		WarmStrategy: beeline.WarmHybrid, Cells: cells,
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

func TestSweepExpiredEvictsColdDemandFromIndexAndStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	cells := diskCells(t, 2)
	area := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "lazy", Resolution: testRes, MaxRadiusMeters: 3000,
		WarmStrategy: beeline.WarmLazy, DemandIdleTTL: time.Hour, Cells: cells,
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

	// An eager area: its whole bound is pinned, so nothing ever decays.
	eager := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "eager", Resolution: testRes, MaxRadiusMeters: 3000,
		WarmStrategy: beeline.WarmEager, DemandIdleTTL: time.Hour, Cells: cells,
	})
	_, err := h.coord.Enable(ctx, eager.ID)
	require.NoError(t, err)
	eagerBefore, err := h.index.DebtForArea(ctx, eager.ID)
	require.NoError(t, err)

	// A lazy area with decay disabled (TTL 0): demand pairs live until disable.
	lazy := createAreaWith(t, h, &control.CreateAreaInput{
		Name: "lazy", Resolution: testRes, MaxRadiusMeters: 3000,
		WarmStrategy: beeline.WarmLazy, DemandIdleTTL: 0, Cells: cells,
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

func TestValidationRejectsBadWarmConfig(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)
	cells := diskCells(t, 1)

	t.Run("full mesh must be eager", func(t *testing.T) {
		t.Parallel()

		_, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "fm", Resolution: testRes, MaxRadiusMeters: 0,
			WarmStrategy: beeline.WarmLazy, Cells: cells,
		})
		assert.Error(t, err, "full-mesh (max 0) with a non-eager strategy is rejected")
	})

	t.Run("bounded radius below the neighbor floor is rejected", func(t *testing.T) {
		t.Parallel()

		// 500m at res 8 reaches no neighbor cell (cells are ~900m apart), so every
		// origin would pair only with itself — the degenerate case the floor forbids.
		_, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "too-tight", Resolution: testRes, MaxRadiusMeters: 500,
			WarmStrategy: beeline.WarmEager, Cells: cells,
		})
		assert.Error(t, err, "a max radius below the res-8 neighbor floor is rejected")
	})

	t.Run("core may not exceed max", func(t *testing.T) {
		t.Parallel()

		_, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "big-core", Resolution: testRes, MaxRadiusMeters: 1000, CoreRadiusMeters: 5000,
			WarmStrategy: beeline.WarmHybrid, Cells: cells,
		})
		assert.Error(t, err, "core radius greater than max radius is rejected")
	})

	t.Run("unknown strategy is rejected", func(t *testing.T) {
		t.Parallel()

		_, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "weird", Resolution: testRes, MaxRadiusMeters: 1000,
			WarmStrategy: beeline.WarmStrategy("aggressive"), Cells: cells,
		})
		assert.Error(t, err)
	})

	t.Run("empty strategy defaults to eager and is accepted", func(t *testing.T) {
		t.Parallel()

		area, err := h.coord.Create(ctx, &control.CreateAreaInput{
			Name: "default", Resolution: testRes, MaxRadiusMeters: 1000, Cells: cells,
		})
		require.NoError(t, err)
		assert.Equal(t, beeline.WarmEager, area.WarmStrategy)
	})
}

func TestCreateIsDisabledAndNotSeeded(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, diskCells(t, 1))
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

	area := createArea(t, h, diskCells(t, 1))
	enabled, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)
	assert.True(t, enabled.Enabled)

	debt, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Positive(t, debt.WorkingSet, "enabling seeds the area's pairs")

	routed, ok := h.coord.Locate(beeline.LatLng{Lat: sfLat, Lng: sfLng})
	require.True(t, ok, "an enabled area routes a contained point")
	assert.Equal(t, area.ID, routed.ID)
	assert.Equal(t, testRes, routed.Resolution)
}

func TestDisableUnseedsAndClearsStore(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, diskCells(t, 1))
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

func TestAddCellsConvergesEnabledArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, diskCells(t, 1))
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	before, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)

	// Grow to a 2-ring; the outer ring adds cells (and pairs).
	two := diskCells(t, 2)
	updated, err := h.coord.AddCells(ctx, area.ID, two)
	require.NoError(t, err)
	assert.Len(t, updated.Cells, len(two))

	after, err := h.index.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Greater(t, after.WorkingSet, before.WorkingSet, "added cells grow the working set")

	// A point in a newly added outer cell now routes into the area.
	oneSet := make(map[beeline.H3Cell]struct{})
	for _, c := range diskCells(t, 1) {
		oneSet[c] = struct{}{}
	}
	var outer beeline.H3Cell
	for _, c := range two {
		if _, ok := oneSet[c]; !ok {
			outer = c
			break
		}
	}
	require.NotZero(t, outer)
	center, err := beeline.Center(outer)
	require.NoError(t, err)
	_, ok := h.coord.Locate(center)
	assert.True(t, ok, "a newly added cell routes reads")
}

func TestDeleteRemovesEverything(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	area := createArea(t, h, diskCells(t, 1))
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

	enabledArea := createArea(t, h, diskCells(t, 1))
	_, err := h.coord.Enable(ctx, enabledArea.ID)
	require.NoError(t, err)

	// A second, disabled area sharing the same repo.
	_, err = h.coord.Create(ctx, &control.CreateAreaInput{
		Name: "disabled", Resolution: testRes, MaxRadiusMeters: 1500, Cells: diskCells(t, 1),
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

func TestCreateRejectsMixedResolutionCells(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newHarness(t)

	// A res-9 cell supplied for a res-8 area must be rejected.
	center9, err := beeline.CellAt(beeline.LatLng{Lat: sfLat, Lng: sfLng}, 9)
	require.NoError(t, err)

	_, err = h.coord.Create(ctx, &control.CreateAreaInput{
		Name: "bad", Resolution: testRes, MaxRadiusMeters: 1500, Cells: []beeline.H3Cell{center9},
	})
	assert.Error(t, err)
}

func TestCreateDefaultsProviderToHaversine(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	// No RoutingProvider set → normalized and persisted as the built-in default.
	area := createArea(t, h, diskCells(t, 1))
	assert.Equal(t, config.DefaultProviderName, area.RoutingProvider)

	got, err := h.coord.Get(context.Background(), area.ID)
	require.NoError(t, err)
	assert.Equal(t, config.DefaultProviderName, got.RoutingProvider)
}

func TestCreateRejectsUnknownProvider(t *testing.T) {
	t.Parallel()

	h := newHarness(t)

	_, err := h.coord.Create(context.Background(), &control.CreateAreaInput{
		Name: "bad", Resolution: testRes, MaxRadiusMeters: 1500,
		Cells: diskCells(t, 1), RoutingProvider: "nope",
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
		Name: "osrm", Resolution: testRes, MaxRadiusMeters: 1500,
		Cells: diskCells(t, 1), RoutingProvider: testProvider,
	})
	require.NoError(t, err)
	_, err = h.coord.Enable(ctx, osrmArea.ID)
	require.NoError(t, err)

	// A second area on the built-in default (unbounded table size).
	defaultArea := createArea(t, h, diskCells(t, 1))
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

	area := createArea(t, h, diskCells(t, 1))
	_, err := h.coord.Enable(ctx, area.ID)
	require.NoError(t, err)
	require.Equal(t, 0, h.coord.EngineFor(area.ID).Capabilities().MaxTableSize)

	updated, err := h.coord.Update(ctx, area.ID, &control.UpdateAreaInput{
		Name: area.Name, Resolution: area.Resolution, MaxRadiusMeters: area.MaxRadiusMeters,
		RoutingProvider: testProvider,
	})
	require.NoError(t, err)
	assert.Equal(t, testProvider, updated.RoutingProvider)
	assert.Equal(t, 10000, h.coord.EngineFor(area.ID).Capabilities().MaxTableSize,
		"re-converging the enabled area picks up the new provider's engine")
}

func TestProviderNamesListsDefaultFirst(t *testing.T) {
	t.Parallel()

	names := newHarness(t).coord.ProviderNames()
	require.NotEmpty(t, names)
	assert.Equal(t, config.DefaultProviderName, names[0], "the default is listed first")
	assert.Contains(t, names, testProvider)
}
