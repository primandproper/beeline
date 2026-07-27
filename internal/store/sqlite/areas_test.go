package sqlite_test

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/sqlite"

	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newRepo opens a migrated, private in-memory database and wraps it. It takes
// testing.TB so the conformance suite's factory can share it.
func newRepo(tb testing.TB) *sqlite.Repository {
	tb.Helper()

	db, err := sqlite.Open(":memory:")
	require.NoError(tb, err)
	tb.Cleanup(func() { _ = db.Close() })

	return sqlite.NewRepository(db, nil)
}

func TestRepositoryCreateAndGet(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	layers := []beeline.Layer{
		{Resolution: 8, MinDistanceMeters: 0, MaxRadiusMeters: 1500.5, CoreRadiusMeters: 600},
		{Resolution: 6, MinDistanceMeters: 2000, MaxRadiusMeters: 30000, CoreRadiusMeters: 0},
	}
	created, err := repo.Create(ctx, &beeline.Area{
		Name:            "downtown",
		WarmStrategy:    beeline.WarmHybrid,
		RoutingProvider: "osrm-west",
		DemandIdleTTL:   90 * time.Minute,
		TargetTTL:       45 * time.Second,
		LeaseDuration:   20 * time.Second,
		SweepInterval:   30 * time.Second,
		Layers:          layers,
		GeoJSON:         []byte(`{"type":"Polygon","coordinates":[]}`),
		Enabled:         false,
	})
	require.NoError(t, err)
	assert.NotZero(t, created.ID, "an id is assigned")
	assert.False(t, created.CreatedAt.IsZero())

	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "downtown", got.Name)
	assert.Equal(t, beeline.WarmHybrid, got.WarmStrategy)
	assert.Equal(t, "osrm-west", got.RoutingProvider)
	assert.Equal(t, 90*time.Minute, got.DemandIdleTTL)
	assert.Equal(t, 45*time.Second, got.TargetTTL)
	assert.Equal(t, 20*time.Second, got.LeaseDuration)
	assert.Equal(t, 30*time.Second, got.SweepInterval)
	assert.False(t, got.Enabled)
	assert.Equal(t, layers, got.Layers, "layers round-trip in finest→coarsest order")
	assert.JSONEq(t, `{"type":"Polygon","coordinates":[]}`, string(got.GeoJSON))
}

func TestRepositoryLayersOrderedFinestFirst(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	// Insert coarsest-first; the hydrated list must come back resolution-descending.
	created, err := repo.Create(ctx, &beeline.Area{Name: "x", Layers: []beeline.Layer{
		{Resolution: 5, MaxRadiusMeters: 50000},
		{Resolution: 9, MaxRadiusMeters: 1000},
		{Resolution: 7, MaxRadiusMeters: 10000},
	}})
	require.NoError(t, err)

	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	require.Len(t, got.Layers, 3)
	assert.Equal(t, []int{9, 7, 5}, []int{got.Layers[0].Resolution, got.Layers[1].Resolution, got.Layers[2].Resolution})
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

	_, err = repo.Create(ctx, &beeline.Area{Name: "a", Layers: []beeline.Layer{{Resolution: 8, MaxRadiusMeters: 900}}})
	require.NoError(t, err)
	_, err = repo.Create(ctx, &beeline.Area{Name: "b", Layers: []beeline.Layer{{Resolution: 9, MaxRadiusMeters: 1500.5}}})
	require.NoError(t, err)

	all, err := repo.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, "a", all[0].Name)
	assert.Equal(t, "b", all[1].Name)
	assert.Equal(t, 8, all[0].Finest().Resolution)
	assert.Equal(t, 9, all[1].Finest().Resolution)
}

func TestRepositorySetEnabled(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	created, err := repo.Create(ctx, &beeline.Area{Name: "x", Layers: []beeline.Layer{{Resolution: 8, MaxRadiusMeters: 900}}})
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

func TestRepositoryUpdateReplacesLayers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	created, err := repo.Create(ctx, &beeline.Area{Name: "old", Layers: []beeline.Layer{
		{Resolution: 8, MaxRadiusMeters: 900},
		{Resolution: 6, MaxRadiusMeters: 20000},
	}})
	require.NoError(t, err)

	created.Name = "new"
	created.Layers = []beeline.Layer{{Resolution: 9, MinDistanceMeters: 0, MaxRadiusMeters: 2500}}
	created.GeoJSON = nil
	require.NoError(t, repo.Update(ctx, &created))

	got, err := repo.Get(ctx, created.ID)
	require.NoError(t, err)
	assert.Equal(t, "new", got.Name)
	assert.Equal(t, created.Layers, got.Layers, "layers are replaced, not merged")
	assert.Nil(t, got.GeoJSON)
}

func TestRepositoryDeleteCascadesLayers(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	repo := newRepo(t)

	created, err := repo.Create(ctx, &beeline.Area{Name: "x", Layers: []beeline.Layer{{Resolution: 8, MaxRadiusMeters: 900}}})
	require.NoError(t, err)

	require.NoError(t, repo.Delete(ctx, created.ID))
	_, err = repo.Get(ctx, created.ID)
	assert.ErrorIs(t, err, sqlite.ErrNotFound)

	// Recreating inherits no stale layers: the cascade removed the old rows.
	again, err := repo.Create(ctx, &beeline.Area{Name: "y", Layers: []beeline.Layer{{Resolution: 7, MaxRadiusMeters: 5000}}})
	require.NoError(t, err)
	got, err := repo.Get(ctx, again.ID)
	require.NoError(t, err)
	require.Len(t, got.Layers, 1)
	assert.Equal(t, 7, got.Layers[0].Resolution)
}

// TestMigration0004Backfill drives the schema to just before the layers migration,
// plants legacy single-resolution rows the way pre-layer builds wrote them, and
// asserts 0004 converts them: one layer per area carrying the old scalars, the
// area_cells table dropped, and geojson-less areas disabled (they have no geometry
// to derive cells from, so ResumeEnabled must not pick them up).
func TestMigration0004Backfill(t *testing.T) {
	t.Parallel()

	db, err := sql.Open("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	// One connection, like sqlite.Open: a second conn would see a different :memory: db.
	db.SetMaxOpenConns(1)

	provider, err := goose.NewProvider(goose.DialectSQLite3, db, os.DirFS("migrations"))
	require.NoError(t, err)

	ctx := context.Background()
	_, err = provider.UpTo(ctx, 3)
	require.NoError(t, err)

	// Two legacy areas: one hand-built (no geojson) and enabled, one from geojson.
	_, err = db.ExecContext(ctx, `
		INSERT INTO areas (id, name, resolution, radius_meters, warm_strategy, core_radius_meters,
			demand_idle_ttl_seconds, geojson, enabled, created_at, updated_at,
			routing_provider, target_ttl_seconds, lease_duration_seconds, sweep_interval_seconds)
		VALUES
			(1, 'hand-built', 9, 1200.5, 'hybrid', 400, 60, NULL, 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'haversine', 60, 15, 15),
			(2, 'uploaded', 8, 0, 'eager', 0, 0, '{"type":"Polygon","coordinates":[]}', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', 'haversine', 60, 15, 15)`)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO area_cells (area_id, cell) VALUES (1, '8928308280fffff')`)
	require.NoError(t, err)

	_, err = provider.Up(ctx)
	require.NoError(t, err)

	rows, err := db.QueryContext(ctx,
		`SELECT area_id, resolution, min_distance_meters, max_radius_meters, core_radius_meters
		 FROM area_layers ORDER BY area_id`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()

	type layerRow struct {
		areaID               int64
		resolution           int64
		minDist, maxR, coreR float64
	}
	var got []layerRow
	for rows.Next() {
		var lr layerRow
		require.NoError(t, rows.Scan(&lr.areaID, &lr.resolution, &lr.minDist, &lr.maxR, &lr.coreR))
		got = append(got, lr)
	}
	require.NoError(t, rows.Err())
	require.Len(t, got, 2, "each legacy area becomes exactly one layer")
	assert.Equal(t, layerRow{areaID: 1, resolution: 9, minDist: 0, maxR: 1200.5, coreR: 400}, got[0])
	assert.Equal(t, layerRow{areaID: 2, resolution: 8, minDist: 0, maxR: 0, coreR: 0}, got[1])

	// area_cells is gone.
	var n int
	err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='area_cells'`).Scan(&n)
	require.NoError(t, err)
	assert.Zero(t, n, "area_cells table must be dropped")

	// The geojson-less area was force-disabled; the uploaded one stays enabled.
	var enabled1, enabled2 int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT enabled FROM areas WHERE id = 1`).Scan(&enabled1))
	require.NoError(t, db.QueryRowContext(ctx, `SELECT enabled FROM areas WHERE id = 2`).Scan(&enabled2))
	assert.Zero(t, enabled1, "an area without geojson cannot derive cells and must be disabled")
	assert.Equal(t, 1, enabled2)
}
