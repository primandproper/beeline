// Package control is the runtime control plane for service areas. It turns the
// persistent set of operator-configured areas into live refresh work: areas are
// created disabled, and only when enabled does the Coordinator tessellate their cell
// set into pairs and seed them into the shared freshness index for the refresh pool
// to burn down. Disabling an area removes its pairs and cached estimates again.
//
// A Coordinator owns the mutable enabled-set and drives three narrow seams: the areas
// repository (persistence), the freshness index (per-area Seed/Unseed), and the hot
// store (per-area delete). It also serves the read path's point-in-area routing
// (Locate). Everything is serialized so an area's persisted state, its presence in
// the index/store, and the in-memory routing snapshot never drift apart.
package control

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/tessellate"
)

// AreasRepository persists service-area definitions. The SQLite store satisfies it;
// it is declared here (consumer-side) so the control plane does not depend on a
// concrete database package.
type AreasRepository interface {
	Create(ctx context.Context, a *beeline.Area) (beeline.Area, error)
	List(ctx context.Context) ([]beeline.Area, error)
	Get(ctx context.Context, id beeline.AreaID) (beeline.Area, error)
	Update(ctx context.Context, a *beeline.Area) error
	Delete(ctx context.Context, id beeline.AreaID) error
	SetEnabled(ctx context.Context, id beeline.AreaID, enabled bool) error
	AddCells(ctx context.Context, id beeline.AreaID, cells []beeline.H3Cell) error
	RemoveCells(ctx context.Context, id beeline.AreaID, cells []beeline.H3Cell) error
}

// AreaIndex is the freshness-index seam: seed an area's pairs, remove them, and
// report per-area freshness. The in-memory index satisfies it.
type AreaIndex interface {
	Seed(ctx context.Context, keys []beeline.PairKey) error
	Unseed(ctx context.Context, area beeline.AreaID) error
	CellStatesForArea(ctx context.Context, area beeline.AreaID) ([]beeline.CellState, error)
	DebtForArea(ctx context.Context, area beeline.AreaID) (beeline.DebtStats, error)
}

// AreaStore is the hot-store seam: drop an area's cached estimates on disable.
type AreaStore interface {
	DeleteArea(ctx context.Context, area beeline.AreaID) error
}

// CreateAreaInput describes a new area. Exactly one geometry source is used: if
// GeoJSON is non-empty it is polyfilled to the cell set (and kept as provenance);
// otherwise Cells is taken verbatim (possibly empty, to be filled in by hand later).
type CreateAreaInput struct {
	Name            string
	GeoJSON         []byte
	Cells           []beeline.H3Cell
	Resolution      int
	MaxRadiusMeters float64
}

// UpdateAreaInput carries the mutable metadata of an area. Changing Resolution is
// only allowed when the area has a GeoJSON provenance to re-polyfill from.
type UpdateAreaInput struct {
	Name            string
	Resolution      int
	MaxRadiusMeters float64
}

// enabledArea is the in-memory routing snapshot for one enabled area: its resolution
// and cell membership set, consulted by Locate on the read path.
type enabledArea struct {
	cells      map[beeline.H3Cell]struct{}
	resolution int
}

// Coordinator serializes area lifecycle operations over the repository, index, and
// store, and answers point-in-area routing for the read path.
type Coordinator struct {
	repo     AreasRepository
	index    AreaIndex
	store    AreaStore
	enabled  map[beeline.AreaID]*enabledArea
	profiles []beeline.Profile
	mu       sync.RWMutex
}

// New builds a Coordinator over the given seams. No areas are seeded yet — call
// ResumeEnabled at boot to seed the ones already enabled.
func New(repo AreasRepository, index AreaIndex, store AreaStore, profiles []beeline.Profile) *Coordinator {
	return &Coordinator{
		repo:     repo,
		index:    index,
		store:    store,
		profiles: profiles,
		enabled:  make(map[beeline.AreaID]*enabledArea),
	}
}

// List returns every configured area.
func (c *Coordinator) List(ctx context.Context) ([]beeline.Area, error) {
	return c.repo.List(ctx)
}

// Get returns one configured area.
func (c *Coordinator) Get(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	return c.repo.Get(ctx, id)
}

// Create validates and persists a new area, always disabled. It resolves the cell set
// from GeoJSON (polyfill) when provided, else from the supplied cells. It does not
// seed the index — an area does no refresh work until enabled.
func (c *Coordinator) Create(ctx context.Context, in *CreateAreaInput) (beeline.Area, error) {
	if err := validateAreaFields(in.Name, in.Resolution, in.MaxRadiusMeters); err != nil {
		return beeline.Area{}, err
	}

	cells := in.Cells
	if len(in.GeoJSON) > 0 {
		polyfilled, err := tessellate.CellsFromGeoJSON(in.GeoJSON, in.Resolution)
		if err != nil {
			return beeline.Area{}, err
		}
		cells = polyfilled
	} else if err := validateCellResolution(cells, in.Resolution); err != nil {
		return beeline.Area{}, err
	}

	return c.repo.Create(ctx, &beeline.Area{
		Name:            in.Name,
		Resolution:      in.Resolution,
		MaxRadiusMeters: in.MaxRadiusMeters,
		Cells:           cells,
		GeoJSON:         in.GeoJSON,
		Enabled:         false,
	})
}

// Update rewrites an area's metadata. A resolution change re-polyfills the cell set
// from the area's GeoJSON provenance; changing the resolution of a hand-built area
// (no GeoJSON) is rejected because its cells cannot be reprojected. If the area is
// enabled, its working set is re-converged.
func (c *Coordinator) Update(ctx context.Context, id beeline.AreaID, in UpdateAreaInput) (beeline.Area, error) {
	if err := validateAreaFields(in.Name, in.Resolution, in.MaxRadiusMeters); err != nil {
		return beeline.Area{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	area, err := c.repo.Get(ctx, id)
	if err != nil {
		return beeline.Area{}, err
	}

	if in.Resolution != area.Resolution {
		if len(area.GeoJSON) == 0 {
			return beeline.Area{}, errors.New("control: cannot change resolution of a hand-built area; replace its geometry instead")
		}
		cells, cellsErr := tessellate.CellsFromGeoJSON(area.GeoJSON, in.Resolution)
		if cellsErr != nil {
			return beeline.Area{}, cellsErr
		}
		area.Cells = cells
	}

	area.Name = in.Name
	area.Resolution = in.Resolution
	area.MaxRadiusMeters = in.MaxRadiusMeters

	return c.persistAndConvergeLocked(ctx, &area)
}

// SetGeoJSON replaces an area's geometry from an uploaded polygon, re-polyfilling the
// cell set at the area's resolution and keeping the raw GeoJSON as provenance. An
// enabled area is re-converged.
func (c *Coordinator) SetGeoJSON(ctx context.Context, id beeline.AreaID, raw []byte) (beeline.Area, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	area, err := c.repo.Get(ctx, id)
	if err != nil {
		return beeline.Area{}, err
	}

	cells, err := tessellate.CellsFromGeoJSON(raw, area.Resolution)
	if err != nil {
		return beeline.Area{}, err
	}
	area.Cells = cells
	area.GeoJSON = raw

	return c.persistAndConvergeLocked(ctx, &area)
}

// AddCells adds cells to an area's set (manual refinement). Cells must be at the
// area's resolution. An enabled area is re-converged so the new cells start refreshing.
func (c *Coordinator) AddCells(ctx context.Context, id beeline.AreaID, cells []beeline.H3Cell) (beeline.Area, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	area, err := c.repo.Get(ctx, id)
	if err != nil {
		return beeline.Area{}, err
	}
	if err = validateCellResolution(cells, area.Resolution); err != nil {
		return beeline.Area{}, err
	}
	if err = c.repo.AddCells(ctx, id, cells); err != nil {
		return beeline.Area{}, err
	}

	return c.reloadAndConvergeLocked(ctx, id)
}

// RemoveCells drops cells from an area's set (manual refinement). An enabled area is
// re-converged so the removed cells stop being refreshed and their estimates are cleared.
func (c *Coordinator) RemoveCells(ctx context.Context, id beeline.AreaID, cells []beeline.H3Cell) (beeline.Area, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.repo.RemoveCells(ctx, id, cells); err != nil {
		return beeline.Area{}, err
	}

	return c.reloadAndConvergeLocked(ctx, id)
}

// Enable seeds an area's pairs into the shared index and marks it enabled, so the
// refresh pool begins keeping it fresh. Enabling an already-enabled area is a no-op
// re-seed.
func (c *Coordinator) Enable(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	area, err := c.repo.Get(ctx, id)
	if err != nil {
		return beeline.Area{}, err
	}

	if err = c.seedLocked(ctx, &area); err != nil {
		return beeline.Area{}, err
	}

	if err = c.repo.SetEnabled(ctx, id, true); err != nil {
		// Roll back the seed so persisted state and the index don't diverge.
		delete(c.enabled, id)
		return beeline.Area{}, errors.Join(err, c.index.Unseed(ctx, id))
	}

	area.Enabled = true

	return area, nil
}

// Disable removes an area's pairs and cached estimates and marks it disabled. The
// refresh pool stops working it. Disabling an already-disabled area is a no-op.
func (c *Coordinator) Disable(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.unseedLocked(ctx, id); err != nil {
		return beeline.Area{}, err
	}

	if err := c.repo.SetEnabled(ctx, id, false); err != nil {
		return beeline.Area{}, err
	}

	return c.repo.Get(ctx, id)
}

// Delete removes an area entirely, first tearing down its working set if enabled.
func (c *Coordinator) Delete(ctx context.Context, id beeline.AreaID) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.unseedLocked(ctx, id); err != nil {
		return err
	}

	return c.repo.Delete(ctx, id)
}

// ResumeEnabled seeds every already-enabled area into the index. Call it once at boot
// after opening the store: an empty database seeds nothing and the pool idles.
func (c *Coordinator) ResumeEnabled(ctx context.Context) error {
	areas, err := c.repo.List(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	for i := range areas {
		if !areas[i].Enabled {
			continue
		}
		if seedErr := c.seedLocked(ctx, &areas[i]); seedErr != nil {
			return seedErr
		}
	}

	return nil
}

// Locate resolves a coordinate to the enabled area containing it, evaluated at that
// area's resolution. On overlap the lowest area id wins, deterministically. It
// implements the read path's AreaRouter seam.
func (c *Coordinator) Locate(p beeline.LatLng) (beeline.RoutedArea, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]beeline.AreaID, 0, len(c.enabled))
	for id := range c.enabled {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	for _, id := range ids {
		ea := c.enabled[id]
		cell, err := beeline.CellAt(p, ea.resolution)
		if err != nil {
			continue
		}
		if _, ok := ea.cells[cell]; ok {
			return beeline.RoutedArea{ID: id, Resolution: ea.resolution}, true
		}
	}

	return beeline.RoutedArea{}, false
}

// EnabledAreas returns the ids of the currently enabled areas, ascending.
func (c *Coordinator) EnabledAreas() []beeline.AreaID {
	c.mu.RLock()
	defer c.mu.RUnlock()

	ids := make([]beeline.AreaID, 0, len(c.enabled))
	for id := range c.enabled {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	return ids
}

// CellStatesForArea proxies the index's per-area rollup for the progress map.
func (c *Coordinator) CellStatesForArea(ctx context.Context, id beeline.AreaID) ([]beeline.CellState, error) {
	return c.index.CellStatesForArea(ctx, id)
}

// DebtForArea proxies the index's per-area freshness contract.
func (c *Coordinator) DebtForArea(ctx context.Context, id beeline.AreaID) (beeline.DebtStats, error) {
	return c.index.DebtForArea(ctx, id)
}

// persistAndConvergeLocked writes area and, if it is enabled, re-converges its working
// set. Callers hold c.mu.
func (c *Coordinator) persistAndConvergeLocked(ctx context.Context, area *beeline.Area) (beeline.Area, error) {
	if err := c.repo.Update(ctx, area); err != nil {
		return beeline.Area{}, err
	}

	if _, ok := c.enabled[area.ID]; ok {
		if err := c.convergeLocked(ctx, area); err != nil {
			return beeline.Area{}, err
		}
	}

	return *area, nil
}

// reloadAndConvergeLocked reloads an area from the repo (after a cell edit) and
// re-converges it if enabled. Callers hold c.mu.
func (c *Coordinator) reloadAndConvergeLocked(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	area, err := c.repo.Get(ctx, id)
	if err != nil {
		return beeline.Area{}, err
	}

	if _, ok := c.enabled[id]; ok {
		if convErr := c.convergeLocked(ctx, &area); convErr != nil {
			return beeline.Area{}, convErr
		}
	}

	return area, nil
}

// seedLocked tessellates an area's cells into pairs, seeds the index, and records the
// routing snapshot. Callers hold c.mu.
func (c *Coordinator) seedLocked(ctx context.Context, area *beeline.Area) error {
	pairs, err := tessellate.PairsFromCells(area.ID, area.Cells, area.Resolution, area.MaxRadiusMeters, c.profiles)
	if err != nil {
		return err
	}

	if err = c.index.Seed(ctx, pairs); err != nil {
		return err
	}

	cellSet := make(map[beeline.H3Cell]struct{}, len(area.Cells))
	for _, cell := range area.Cells {
		cellSet[cell] = struct{}{}
	}
	c.enabled[area.ID] = &enabledArea{resolution: area.Resolution, cells: cellSet}

	return nil
}

// unseedLocked removes an area's pairs and cached estimates and forgets its routing
// snapshot. It is a no-op for an area that is not enabled. Callers hold c.mu.
func (c *Coordinator) unseedLocked(ctx context.Context, id beeline.AreaID) error {
	if _, ok := c.enabled[id]; !ok {
		return nil
	}

	if err := c.index.Unseed(ctx, id); err != nil {
		return err
	}
	if err := c.store.DeleteArea(ctx, id); err != nil {
		return err
	}
	delete(c.enabled, id)

	return nil
}

// convergeLocked rebuilds an enabled area's working set from its current cells: tear
// down the old pairs/estimates, then seed the new ones (freshness restarts from zero
// for this area). Callers hold c.mu.
func (c *Coordinator) convergeLocked(ctx context.Context, area *beeline.Area) error {
	if err := c.index.Unseed(ctx, area.ID); err != nil {
		return err
	}
	if err := c.store.DeleteArea(ctx, area.ID); err != nil {
		return err
	}

	return c.seedLocked(ctx, area)
}

// validateAreaFields rejects area metadata the tessellator or store would refuse. A
// maxRadiusMeters of 0 is the full-mesh sentinel (every in-area pair); any positive
// value is a travel-radius bound. Negative bounds are rejected.
func validateAreaFields(name string, resolution int, maxRadiusMeters float64) error {
	if name == "" {
		return errors.New("control: area name is required")
	}
	if resolution < 0 || resolution > 15 {
		return fmt.Errorf("control: resolution %d out of range [0,15]", resolution)
	}
	if maxRadiusMeters < 0 {
		return fmt.Errorf("control: max radius meters %.2f must be >= 0 (0 = full mesh)", maxRadiusMeters)
	}

	return nil
}

// validateCellResolution ensures every cell matches the area resolution, so a
// hand-supplied or added cell set cannot mix resolutions.
func validateCellResolution(cells []beeline.H3Cell, resolution int) error {
	for _, cell := range cells {
		if cell.Resolution() != resolution {
			return fmt.Errorf("control: cell %s is resolution %d, want %d", cell, cell.Resolution(), resolution)
		}
	}

	return nil
}
