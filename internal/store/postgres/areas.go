package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/postgres/generated"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrNotFound is returned when an area id does not exist. The message ends in
// "not found" — the HTTP layer's not-found mapping matches on that.
var ErrNotFound = errors.New("postgres: area not found")

// Config-version kinds: the convergence channels heads poll (see
// config_version in migrations). Every mutating repository method bumps its
// kind's generation in the same transaction as the write, so a poll can never
// observe the generation ahead of the data.
const (
	// ConfigKindAreas ticks on any area mutation (create/update/delete/enable).
	ConfigKindAreas = "areas"
	// ConfigKindProviders ticks on provider upserts and deletes.
	ConfigKindProviders = "providers"
)

// Repository is the Postgres-backed control-plane store shared by every head:
// area and provider definitions plus the config_version convergence signal.
// It is the distributed counterpart of the SQLite repository and satisfies the
// same control.AreasRepository/ProvidersRepository seams.
type Repository struct {
	pool    *pgxpool.Pool
	queries generated.Querier
	now     func() time.Time
}

// NewRepository builds a Repository over an opened pool. The clock is
// injectable for tests; pass nil for the wall clock. (Config timestamps are
// display metadata — coordination timestamps come from the database clock.)
func NewRepository(pool *pgxpool.Pool, clock func() time.Time) *Repository {
	if clock == nil {
		clock = time.Now
	}

	return &Repository{pool: pool, queries: generated.New(), now: clock}
}

// Create inserts an area and its layers in one transaction and bumps the areas
// generation. The caller controls the Enabled flag (areas are created
// disabled).
func (r *Repository) Create(ctx context.Context, a *beeline.Area) (beeline.Area, error) {
	now := r.now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now

	err := r.inTx(ctx, func(tx pgx.Tx) error {
		id, createErr := r.queries.CreateArea(ctx, tx, &generated.CreateAreaParams{
			Name:                 a.Name,
			WarmStrategy:         string(a.WarmStrategy),
			DemandIdleTtlSeconds: durationSeconds(a.DemandIdleTTL),
			TargetTtlSeconds:     durationSeconds(a.TargetTTL),
			LeaseDurationSeconds: durationSeconds(a.LeaseDuration),
			SweepIntervalSeconds: durationSeconds(a.SweepInterval),
			RoutingProvider:      a.RoutingProvider,
			Geojson:              geojsonParam(a.GeoJSON),
			Enabled:              a.Enabled,
			CreatedAt:            timestamptz(now),
			UpdatedAt:            timestamptz(now),
		})
		if createErr != nil {
			return fmt.Errorf("postgres: inserting area: %w", createErr)
		}
		a.ID = beeline.AreaID(id)

		if layersErr := r.insertLayers(ctx, tx, a.ID, a.Layers); layersErr != nil {
			return layersErr
		}

		return r.bump(ctx, tx, ConfigKindAreas)
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return *a, nil
}

// Get returns one area with its layers hydrated, or ErrNotFound.
func (r *Repository) Get(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	row, err := r.queries.GetArea(ctx, r.pool, int64(id))
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return beeline.Area{}, ErrNotFound
		}

		return beeline.Area{}, fmt.Errorf("postgres: getting area %d: %w", id, err)
	}

	layers, err := r.layersFor(ctx, r.pool, id)
	if err != nil {
		return beeline.Area{}, err
	}

	return convertAreaRow(row, layers), nil
}

// List returns every area with its layers hydrated, ordered by id.
func (r *Repository) List(ctx context.Context) ([]beeline.Area, error) {
	rows, err := r.queries.ListAreas(ctx, r.pool)
	if err != nil {
		return nil, fmt.Errorf("postgres: listing areas: %w", err)
	}

	areas := make([]beeline.Area, 0, len(rows))
	for _, row := range rows {
		layers, layersErr := r.layersFor(ctx, r.pool, beeline.AreaID(row.ID))
		if layersErr != nil {
			return nil, layersErr
		}
		areas = append(areas, convertAreaRow((*generated.GetAreaRow)(row), layers))
	}

	return areas, nil
}

// Update rewrites an area's mutable fields and replaces its layer list in one
// transaction, bumping updated_at and the areas generation.
func (r *Repository) Update(ctx context.Context, a *beeline.Area) error {
	now := r.now().UTC()

	return r.inTx(ctx, func(tx pgx.Tx) error {
		if updErr := r.queries.UpdateArea(ctx, tx, &generated.UpdateAreaParams{
			ID:                   int64(a.ID),
			Name:                 a.Name,
			WarmStrategy:         string(a.WarmStrategy),
			DemandIdleTtlSeconds: durationSeconds(a.DemandIdleTTL),
			TargetTtlSeconds:     durationSeconds(a.TargetTTL),
			LeaseDurationSeconds: durationSeconds(a.LeaseDuration),
			SweepIntervalSeconds: durationSeconds(a.SweepInterval),
			RoutingProvider:      a.RoutingProvider,
			Geojson:              geojsonParam(a.GeoJSON),
			UpdatedAt:            timestamptz(now),
		}); updErr != nil {
			return fmt.Errorf("postgres: updating area %d: %w", a.ID, updErr)
		}

		if delErr := r.queries.DeleteAreaLayers(ctx, tx, int64(a.ID)); delErr != nil {
			return fmt.Errorf("postgres: clearing layers for area %d: %w", a.ID, delErr)
		}
		if layersErr := r.insertLayers(ctx, tx, a.ID, a.Layers); layersErr != nil {
			return layersErr
		}

		return r.bump(ctx, tx, ConfigKindAreas)
	})
}

// Delete removes an area (layers cascade) and bumps the areas generation.
func (r *Repository) Delete(ctx context.Context, id beeline.AreaID) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		if err := r.queries.DeleteArea(ctx, tx, int64(id)); err != nil {
			return fmt.Errorf("postgres: deleting area %d: %w", id, err)
		}

		return r.bump(ctx, tx, ConfigKindAreas)
	})
}

// SetEnabled flips an area's enabled flag, bumping updated_at and the areas
// generation — the signal that makes an Enable on one head visible to all.
func (r *Repository) SetEnabled(ctx context.Context, id beeline.AreaID, enabled bool) error {
	return r.inTx(ctx, func(tx pgx.Tx) error {
		if err := r.queries.SetAreaEnabled(ctx, tx, &generated.SetAreaEnabledParams{
			ID:        int64(id),
			Enabled:   enabled,
			UpdatedAt: timestamptz(r.now().UTC()),
		}); err != nil {
			return fmt.Errorf("postgres: setting enabled for area %d: %w", id, err)
		}

		return r.bump(ctx, tx, ConfigKindAreas)
	})
}

// ConfigGenerations returns the current generation per config kind — the
// watcher's poll target.
func (r *Repository) ConfigGenerations(ctx context.Context) (map[string]int64, error) {
	rows, err := r.queries.GetConfigGenerations(ctx, r.pool)
	if err != nil {
		return nil, fmt.Errorf("postgres: reading config generations: %w", err)
	}

	generations := make(map[string]int64, len(rows))
	for _, row := range rows {
		generations[row.Kind] = row.Generation
	}

	return generations, nil
}

// bump advances one config kind's generation inside the caller's transaction.
func (r *Repository) bump(ctx context.Context, tx pgx.Tx, kind string) error {
	if err := r.queries.BumpConfigGeneration(ctx, tx, kind); err != nil {
		return fmt.Errorf("postgres: bumping %s generation: %w", kind, err)
	}

	return nil
}

// insertLayers inserts each layer for an area within the given transaction.
func (r *Repository) insertLayers(ctx context.Context, tx pgx.Tx, id beeline.AreaID, layers []beeline.Layer) error {
	for i := range layers {
		if err := r.queries.InsertAreaLayer(ctx, tx, &generated.InsertAreaLayerParams{
			AreaID:            int64(id),
			Resolution:        int64(layers[i].Resolution),
			MinDistanceMeters: layers[i].MinDistanceMeters,
			MaxRadiusMeters:   layers[i].MaxRadiusMeters,
			CoreRadiusMeters:  layers[i].CoreRadiusMeters,
		}); err != nil {
			return fmt.Errorf("postgres: adding res-%d layer to area %d: %w", layers[i].Resolution, id, err)
		}
	}

	return nil
}

// layersFor loads an area's layer list, ordered finest→coarsest by the query.
func (r *Repository) layersFor(ctx context.Context, db generated.DBTX, id beeline.AreaID) ([]beeline.Layer, error) {
	rows, err := r.queries.ListAreaLayers(ctx, db, int64(id))
	if err != nil {
		return nil, fmt.Errorf("postgres: listing layers for area %d: %w", id, err)
	}

	layers := make([]beeline.Layer, 0, len(rows))
	for _, row := range rows {
		layers = append(layers, beeline.Layer{
			Resolution:        int(row.Resolution),
			MinDistanceMeters: row.MinDistanceMeters,
			MaxRadiusMeters:   row.MaxRadiusMeters,
			CoreRadiusMeters:  row.CoreRadiusMeters,
		})
	}

	return layers, nil
}

// inTx runs fn inside a transaction, committing on success.
func (r *Repository) inTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := r.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: beginning transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	if err = fn(tx); err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: committing transaction: %w", err)
	}

	return nil
}

// convertAreaRow maps a generated row plus its layer list into a beeline.Area.
func convertAreaRow(row *generated.GetAreaRow, layers []beeline.Layer) beeline.Area {
	var geojson []byte
	if row.Geojson != nil {
		geojson = []byte(*row.Geojson)
	}

	return beeline.Area{
		ID:              beeline.AreaID(row.ID),
		Name:            row.Name,
		WarmStrategy:    beeline.WarmStrategy(row.WarmStrategy),
		RoutingProvider: row.RoutingProvider,
		DemandIdleTTL:   time.Duration(row.DemandIdleTtlSeconds) * time.Second,
		TargetTTL:       time.Duration(row.TargetTtlSeconds) * time.Second,
		LeaseDuration:   time.Duration(row.LeaseDurationSeconds) * time.Second,
		SweepInterval:   time.Duration(row.SweepIntervalSeconds) * time.Second,
		Layers:          layers,
		GeoJSON:         geojson,
		Enabled:         row.Enabled,
		CreatedAt:       row.CreatedAt.Time,
		UpdatedAt:       row.UpdatedAt.Time,
	}
}

// geojsonParam converts raw GeoJSON bytes to the nullable text column value.
func geojsonParam(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)

	return &s
}

// durationSeconds rounds a per-area duration down to whole seconds for storage.
func durationSeconds(d time.Duration) int64 {
	return int64(d / time.Second)
}

// timestamptz wraps a time for the generated pgtype parameter shape.
func timestamptz(t time.Time) pgtype.Timestamptz {
	return pgtype.Timestamptz{Time: t, Valid: true}
}
