package sqlite_test

import (
	"context"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/sqlite"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/h3-go/v4"
)

func newRepo(t *testing.T) *sqlite.Repository {
	t.Helper()

	db, err := sqlite.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	return sqlite.NewRepository(db, nil)
}

// cellsAround returns the res-8 disk of radius r around San Francisco.
func cellsAround(t *testing.T, r int) []beeline.H3Cell {
	t.Helper()

	center, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 8)
	require.NoError(t, err)
	cells, err := h3.GridDisk(center, r)
	require.NoError(t, err)

	return cells
}

func TestRepositoryCreateAndGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	cells := cellsAround(t, 1)
	created, err := repo.Create(ctx, &beeline.Area{
		Name:            "downtown",
		Resolution:      8,
		MaxRadiusMeters: 1500.5,
		Cells:           cells,
		GeoJSON:         []byte(`{"type":"Polygon","coordinates":[]}`),
		Enabled:         false,
	})
	require.NoError(t, err)
	assert.NotZero(t, created.ID, "an id is assigned")
	assert.False(t, created.CreatedAt.IsZero())

	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "downtown", got.Name)
	assert.Equal(t, 8, got.Resolution)
	assert.Equal(t, 1500.5, got.MaxRadiusMeters)
	assert.False(t, got.Enabled)
	assert.ElementsMatch(t, cells, got.Cells)
	assert.JSONEq(t, `{"type":"Polygon","coordinates":[]}`, string(got.GeoJSON))
}

func TestRepositoryGetMissing(t *testing.T) {
	t.Parallel()

	_, err := newRepo(t).Get(context.Background(), 999)
	assert.ErrorIs(t, err, sqlite.ErrNotFound)
}

func TestRepositoryList(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	empty, err := repo.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, empty, "a fresh database has no areas")

	_, err = repo.Create(ctx, &beeline.Area{Name: "a", Resolution: 8, MaxRadiusMeters: 900, Cells: cellsAround(t, 1)})
	require.NoError(t, err)
	_, err = repo.Create(ctx, &beeline.Area{Name: "b", Resolution: 9, MaxRadiusMeters: 1500.5, Cells: cellsAround(t, 1)})
	require.NoError(t, err)

	all, err := repo.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "a", all[0].Name)
	assert.Equal(t, "b", all[1].Name)
}

func TestRepositorySetEnabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	created, err := repo.Create(ctx, &beeline.Area{Name: "x", Resolution: 8, MaxRadiusMeters: 900, Cells: cellsAround(t, 1)})
	require.NoError(t, err)
	require.False(t, created.Enabled)

	require.NoError(t, repo.SetEnabled(ctx, created.ID, true))
	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.True(t, got.Enabled)

	require.NoError(t, repo.SetEnabled(ctx, created.ID, false))
	got, err = repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.False(t, got.Enabled)
}

func TestRepositoryAddRemoveCells(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	base := cellsAround(t, 1)
	created, err := repo.Create(ctx, &beeline.Area{Name: "x", Resolution: 8, MaxRadiusMeters: 900, Cells: base})
	require.NoError(t, err)

	extra := cellsAround(t, 2) // superset of base
	require.NoError(t, repo.AddCells(ctx, created.ID, extra))
	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.ElementsMatch(t, extra, got.Cells, "adding is idempotent and unions")

	require.NoError(t, repo.RemoveCells(ctx, created.ID, base))
	got, err = repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Len(t, got.Cells, len(extra)-len(base))
	for _, c := range base {
		assert.NotContains(t, got.Cells, c)
	}
}

func TestRepositoryUpdateReplacesCells(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	created, err := repo.Create(ctx, &beeline.Area{Name: "old", Resolution: 8, MaxRadiusMeters: 900, Cells: cellsAround(t, 2)})
	require.NoError(t, err)

	newCells := cellsAround(t, 1)
	created.Name = "new"
	created.MaxRadiusMeters = 2500
	created.Cells = newCells
	created.GeoJSON = nil
	require.NoError(t, repo.Update(ctx, &created))

	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)
	assert.Equal(t, 2500.0, got.MaxRadiusMeters)
	assert.ElementsMatch(t, newCells, got.Cells, "cells are replaced, not merged")
	assert.Nil(t, got.GeoJSON)
}

func TestRepositoryDeleteCascadesCells(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	created, err := repo.Create(ctx, &beeline.Area{Name: "x", Resolution: 8, MaxRadiusMeters: 900, Cells: cellsAround(t, 2)})
	require.NoError(t, err)

	require.NoError(t, repo.Delete(ctx, created.ID))
	_, err = repo.Get(ctx, created.ID)
	assert.ErrorIs(t, err, sqlite.ErrNotFound)

	// Recreating reuses no stale cells: a brand-new area with the same geometry has
	// exactly its own cells (the cascade removed the old rows).
	again, err := repo.Create(ctx, &beeline.Area{Name: "y", Resolution: 8, MaxRadiusMeters: 900, Cells: cellsAround(t, 1)})
	require.NoError(t, err)
	got, err := repo.Get(ctx, again.ID)
	require.NoError(t, err)
	assert.Len(t, got.Cells, len(cellsAround(t, 1)))
}
