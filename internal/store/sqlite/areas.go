package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/store/sqlite/generated"

	"github.com/uber/h3-go/v4"
)

// ErrNotFound is returned when an area id does not exist.
var ErrNotFound = errors.New("sqlite: area not found")

// timeFormat is the on-disk representation of created_at/updated_at.
const timeFormat = time.RFC3339Nano

// Repository is the SQLite-backed area store. It wraps the sqlc-generated Querier and
// converts between generated rows and the beeline.Area domain type. Multi-statement
// writes (an area plus its cells) run in a transaction.
type Repository struct {
	db      *sql.DB
	queries generated.Querier
	now     func() time.Time
}

// NewRepository builds a Repository over an already-open, migrated database (see
// Open). The clock is injectable for tests; pass nil for the wall clock.
func NewRepository(db *sql.DB, clock func() time.Time) *Repository {
	if clock == nil {
		clock = time.Now
	}

	return &Repository{db: db, queries: generated.New(), now: clock}
}

// Create inserts an area and its cells in one transaction, stamping created/updated
// times, and returns the stored area with its assigned ID. The caller controls the
// Enabled flag (the control plane creates areas disabled).
func (r *Repository) Create(ctx context.Context, a *beeline.Area) (beeline.Area, error) {
	now := r.now().UTC()
	a.CreatedAt = now
	a.UpdatedAt = now

	err := r.inTx(ctx, func(tx *sql.Tx) error {
		id, createErr := r.queries.CreateArea(ctx, tx, &generated.CreateAreaParams{
			Name:         a.Name,
			Resolution:   int64(a.Resolution),
			RadiusMeters: a.MaxRadiusMeters,
			Geojson:      geojsonParam(a.GeoJSON),
			Enabled:      boolToInt(a.Enabled),
			CreatedAt:    now.Format(timeFormat),
			UpdatedAt:    now.Format(timeFormat),
		})
		if createErr != nil {
			return fmt.Errorf("sqlite: inserting area: %w", createErr)
		}
		a.ID = beeline.AreaID(id)

		return r.insertCells(ctx, tx, a.ID, a.Cells)
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return *a, nil
}

// Get returns one area with its cells hydrated, or ErrNotFound.
func (r *Repository) Get(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	row, err := r.queries.GetArea(ctx, r.db, int64(id))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return beeline.Area{}, ErrNotFound
		}

		return beeline.Area{}, fmt.Errorf("sqlite: getting area %d: %w", id, err)
	}

	cells, err := r.cellsFor(ctx, r.db, id)
	if err != nil {
		return beeline.Area{}, err
	}

	return convertArea(row, cells)
}

// List returns every area with its cells hydrated, ordered by id.
func (r *Repository) List(ctx context.Context) ([]beeline.Area, error) {
	rows, err := r.queries.ListAreas(ctx, r.db)
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing areas: %w", err)
	}

	areas := make([]beeline.Area, 0, len(rows))
	for _, row := range rows {
		cells, cellsErr := r.cellsFor(ctx, r.db, beeline.AreaID(row.ID))
		if cellsErr != nil {
			return nil, cellsErr
		}

		area, convErr := convertArea(row, cells)
		if convErr != nil {
			return nil, convErr
		}
		areas = append(areas, area)
	}

	return areas, nil
}

// Update rewrites an area's mutable fields and replaces its cell set, in one
// transaction. It bumps updated_at.
func (r *Repository) Update(ctx context.Context, a *beeline.Area) error {
	now := r.now().UTC()

	return r.inTx(ctx, func(tx *sql.Tx) error {
		if updErr := r.queries.UpdateArea(ctx, tx, &generated.UpdateAreaParams{
			ID:           int64(a.ID),
			Name:         a.Name,
			Resolution:   int64(a.Resolution),
			RadiusMeters: a.MaxRadiusMeters,
			Geojson:      geojsonParam(a.GeoJSON),
			UpdatedAt:    now.Format(timeFormat),
		}); updErr != nil {
			return fmt.Errorf("sqlite: updating area %d: %w", a.ID, updErr)
		}

		if delErr := r.queries.DeleteAreaCells(ctx, tx, int64(a.ID)); delErr != nil {
			return fmt.Errorf("sqlite: clearing cells for area %d: %w", a.ID, delErr)
		}

		return r.insertCells(ctx, tx, a.ID, a.Cells)
	})
}

// Delete removes an area; its cells cascade.
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
		UpdatedAt: r.now().UTC().Format(timeFormat),
	}); err != nil {
		return fmt.Errorf("sqlite: setting enabled for area %d: %w", id, err)
	}

	return nil
}

// AddCells adds cells to an area's set (idempotent — existing cells are ignored).
func (r *Repository) AddCells(ctx context.Context, id beeline.AreaID, cells []beeline.H3Cell) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		return r.insertCells(ctx, tx, id, cells)
	})
}

// RemoveCells removes cells from an area's set (missing cells are ignored).
func (r *Repository) RemoveCells(ctx context.Context, id beeline.AreaID, cells []beeline.H3Cell) error {
	return r.inTx(ctx, func(tx *sql.Tx) error {
		for _, c := range cells {
			if err := r.queries.RemoveAreaCell(ctx, tx, &generated.RemoveAreaCellParams{
				AreaID: int64(id),
				Cell:   c.String(),
			}); err != nil {
				return fmt.Errorf("sqlite: removing cell %s from area %d: %w", c, id, err)
			}
		}

		return nil
	})
}

// insertCells inserts each cell for an area within the given executor.
func (r *Repository) insertCells(ctx context.Context, tx generated.DBTX, id beeline.AreaID, cells []beeline.H3Cell) error {
	for _, c := range cells {
		if err := r.queries.AddAreaCell(ctx, tx, &generated.AddAreaCellParams{
			AreaID: int64(id),
			Cell:   c.String(),
		}); err != nil {
			return fmt.Errorf("sqlite: adding cell %s to area %d: %w", c, id, err)
		}
	}

	return nil
}

// cellsFor loads an area's cell set, parsing the stored hex strings back to cells.
func (r *Repository) cellsFor(ctx context.Context, db generated.DBTX, id beeline.AreaID) ([]beeline.H3Cell, error) {
	rows, err := r.queries.ListAreaCells(ctx, db, int64(id))
	if err != nil {
		return nil, fmt.Errorf("sqlite: listing cells for area %d: %w", id, err)
	}

	cells := make([]beeline.H3Cell, 0, len(rows))
	for _, s := range rows {
		cell := h3.CellFromString(s)
		if !cell.IsValid() {
			return nil, fmt.Errorf("sqlite: area %d has invalid stored cell %q", id, s)
		}
		cells = append(cells, cell)
	}

	return cells, nil
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

// convertArea maps a generated row plus its cell set into a beeline.Area.
func convertArea(row *generated.Areas, cells []beeline.H3Cell) (beeline.Area, error) {
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
		Resolution:      int(row.Resolution),
		MaxRadiusMeters: row.RadiusMeters,
		Cells:           cells,
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

// boolToInt maps a bool to SQLite's 0/1 integer boolean.
func boolToInt(b bool) int64 {
	if b {
		return 1
	}

	return 0
}
