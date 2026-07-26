package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/sqlite/generated"

	"github.com/primandproper/platform-go/v7/clock"
)

// ErrNotFound is returned when an area id does not exist.
var ErrNotFound = errors.New("sqlite: area not found")

// timeFormat is the on-disk representation of created_at/updated_at.
const timeFormat = time.RFC3339Nano

// Repository is the SQLite-backed area store. It wraps the sqlc-generated Querier and
// converts between generated rows and the beeline.Area domain type. Multi-statement
// writes (an area plus its layers) run in a transaction.
type Repository struct {
	db      *sql.DB
	queries generated.Querier
	clock   clock.Clock
}

// NewRepository builds a Repository over an already-open, migrated database (see
// Open). Pass nil for the wall clock.
func NewRepository(db *sql.DB, clk clock.Clock) *Repository {
	if clk == nil {
		clk = clock.NewClock()
	}

	return &Repository{db: db, queries: generated.New(), clock: clk}
}

// Create inserts an area and its layers in one transaction, stamping created/updated
// times, and returns the stored area with its assigned ID. The caller controls the
// Enabled flag (the control plane creates areas disabled).
func (r *Repository) Create(ctx context.Context, a *beeline.Area) (beeline.Area, error) {
	now := r.clock.Now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now

	err := r.inTx(ctx, func(tx *sql.Tx) error {
		id, createErr := r.queries.CreateArea(ctx, tx, &generated.CreateAreaParams{
			Name:                 a.Name,
			WarmStrategy:         string(a.WarmStrategy),
			DemandIdleTtlSeconds: durationSeconds(a.DemandIdleTTL),
			TargetTtlSeconds:     durationSeconds(a.TargetTTL),
			LeaseDurationSeconds: durationSeconds(a.LeaseDuration),
			SweepIntervalSeconds: durationSeconds(a.SweepInterval),
			RoutingProvider:      a.RoutingProvider,
			Geojson:              geojsonParam(a.GeoJSON),
			Enabled:              boolToInt(a.Enabled),
			CreatedAt:            now.Format(timeFormat),
			UpdatedAt:            now.Format(timeFormat),
		})
		if createErr != nil {
			return fmt.Errorf("sqlite: inserting area: %w", createErr)
		}
		a.ID = beeline.AreaID(id)

		return r.insertLayers(ctx, tx, a.ID, a.Layers)
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return *a, nil
}

// Get returns one area with its layers hydrated, or ErrNotFound.
func (r *Repository) Get(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	row, err := r.queries.GetArea(ctx, r.db, int64(id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return beeline.Area{}, ErrNotFound
		}

		return beeline.Area{}, fmt.Errorf("sqlite: getting area %d: %w", id, err)
	}

	layers, err := r.layersFor(ctx, r.db, id)
	if err != nil {
		return beeline.Area{}, err
	}

	return convertArea(row, layers)
}

// List returns every area with its layers hydrated, ordered by id.
func (r *Repository) List(ctx context.Context) ([]beeline.Area, error) {
	rows, err := r.queries.ListAreas(ctx, r.db)
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing areas: %w", err)
	}

	areas := make([]beeline.Area, 0, len(rows))
	for _, row := range rows {
		layers, layersErr := r.layersFor(ctx, r.db, beeline.AreaID(row.ID))
		if layersErr != nil {
			return nil, layersErr
		}

		area, convErr := convertArea(row, layers)
		if convErr != nil {
			return nil, convErr
		}
		areas = append(areas, area)
	}

	return areas, nil
}

// Update rewrites an area's mutable fields and replaces its layer list, in one
// transaction. It bumps updated_at.
func (r *Repository) Update(ctx context.Context, a *beeline.Area) error {
	now := r.clock.Now().UTC()

	return r.inTx(ctx, func(tx *sql.Tx) error {
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
			UpdatedAt:            now.Format(timeFormat),
		}); updErr != nil {
			return fmt.Errorf("sqlite: updating area %d: %w", a.ID, updErr)
		}

		if delErr := r.queries.DeleteAreaLayers(ctx, tx, int64(a.ID)); delErr != nil {
			return fmt.Errorf("sqlite: clearing layers for area %d: %w", a.ID, delErr)
		}

		return r.insertLayers(ctx, tx, a.ID, a.Layers)
	})
}

// Delete removes an area; its layers cascade.
func (r *Repository) Delete(ctx context.Context, id beeline.AreaID) error {
	if err := r.queries.DeleteArea(ctx, r.db, int64(id)); err != nil {
		return fmt.Errorf("sqlite: deleting area %d: %w", id, err)
	}

	return nil
}

// SetEnabled flips an area's enabled flag and bumps updated_at.
func (r *Repository) SetEnabled(ctx context.Context, id beeline.AreaID, enabled bool) error {
	if err := r.queries.SetAreaEnabled(ctx, r.db, &generated.SetAreaEnabledParams{
		ID:        int64(id),
		Enabled:   boolToInt(enabled),
		UpdatedAt: r.clock.Now().UTC().Format(timeFormat),
	}); err != nil {
		return fmt.Errorf("sqlite: setting enabled for area %d: %w", id, err)
	}

	return nil
}

// insertLayers inserts each layer for an area within the given executor. The
// (area_id, resolution) primary key rejects duplicate resolutions at the storage
// layer; the control plane validates them before it gets here.
func (r *Repository) insertLayers(ctx context.Context, tx generated.DBTX, id beeline.AreaID, layers []beeline.Layer) error {
	for i := range layers {
		if err := r.queries.InsertAreaLayer(ctx, tx, &generated.InsertAreaLayerParams{
			AreaID:            int64(id),
			Resolution:        int64(layers[i].Resolution),
			MinDistanceMeters: layers[i].MinDistanceMeters,
			MaxRadiusMeters:   layers[i].MaxRadiusMeters,
			CoreRadiusMeters:  layers[i].CoreRadiusMeters,
		}); err != nil {
			return fmt.Errorf("sqlite: adding res-%d layer to area %d: %w", layers[i].Resolution, id, err)
		}
	}

	return nil
}

// layersFor loads an area's layer list, already ordered finest→coarsest by the
// query (resolution descending — the domain ordering invariant).
func (r *Repository) layersFor(ctx context.Context, db generated.DBTX, id beeline.AreaID) ([]beeline.Layer, error) {
	rows, err := r.queries.ListAreaLayers(ctx, db, int64(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing layers for area %d: %w", id, err)
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

// inTx runs fn inside a transaction, committing on success and rolling back on error.
func (r *Repository) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlite: beginning transaction: %w", err)
	}

	if err = fn(tx); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return errors.Join(err, rbErr)
		}

		return err
	}

	if err = tx.Commit(); err != nil {
		return fmt.Errorf("sqlite: committing transaction: %w", err)
	}

	return nil
}

// convertArea maps a generated row plus its layer list into a beeline.Area.
func convertArea(row *generated.Areas, layers []beeline.Layer) (beeline.Area, error) {
	created, err := time.Parse(timeFormat, row.CreatedAt)
	if err != nil {
		return beeline.Area{}, fmt.Errorf("sqlite: parsing created_at for area %d: %w", row.ID, err)
	}

	updated, err := time.Parse(timeFormat, row.UpdatedAt)
	if err != nil {
		return beeline.Area{}, fmt.Errorf("sqlite: parsing updated_at for area %d: %w", row.ID, err)
	}

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
		Enabled:         row.Enabled != 0,
		CreatedAt:       created,
		UpdatedAt:       updated,
	}, nil
}

// geojsonParam converts raw GeoJSON bytes to the nullable string column value.
func geojsonParam(raw []byte) *string {
	if len(raw) == 0 {
		return nil
	}
	s := string(raw)

	return &s
}

// durationSeconds rounds a per-area duration (a TTL, lease, or sweep interval) down to
// whole seconds for storage. Sub-second precision is meaningless for freshness windows
// measured in seconds-to-hours.
func durationSeconds(d time.Duration) int64 {
	return int64(d / time.Second)
}

// boolToInt maps a bool to SQLite's 0/1 integer boolean.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}
