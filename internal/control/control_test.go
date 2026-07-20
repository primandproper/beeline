package control_test

import (
	"context"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	memstore "github.com/primandproper/beeline/internal/store/memory"
	areasqlite "github.com/primandproper/beeline/internal/store/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/h3-go/v4"
)

const (
	testRes = 8
	sfLat   = 37.7749
	sfLng   = -122.4194
	testTTL = 60_000_000_000 // 1 minute in nanoseconds
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
	coord := control.New(repo, index, store, []beeline.Profile{"car"})

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
	fresh := control.New(h.repo, freshIndex, freshStore, []beeline.Profile{"car"})

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
