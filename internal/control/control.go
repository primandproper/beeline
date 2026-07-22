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
	"time"

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

// AreaIndex is the freshness-index seam: seed an area's pairs, remove them, sweep its
// cold demand pairs, and report per-area freshness. The in-memory index satisfies it.
type AreaIndex interface {
	Seed(ctx context.Context, keys []beeline.PairKey) error
	Unseed(ctx context.Context, area beeline.AreaID) error
	SweepArea(ctx context.Context, area beeline.AreaID, cutoff time.Time) ([]beeline.PairKey, error)
	CellStatesForArea(ctx context.Context, area beeline.AreaID) ([]beeline.CellState, error)
	DebtForArea(ctx context.Context, area beeline.AreaID) (beeline.DebtStats, error)
	// SetAreaFreshness registers an area's per-area target TTL and claim lease, so the
	// index applies this area's freshness contract to its pairs rather than the global one.
	SetAreaFreshness(ctx context.Context, area beeline.AreaID, targetTTL, lease time.Duration) error
}

// AreaStore is the hot-store seam: drop an area's cached estimates on disable, and
// drop individual swept keys on demand decay.
type AreaStore interface {
	DeleteArea(ctx context.Context, area beeline.AreaID) error
	Delete(ctx context.Context, keys []beeline.PairKey) error
}

// CreateAreaInput describes a new area. Exactly one geometry source is used: if
// GeoJSON is non-empty it is polyfilled to the cell set (and kept as provenance);
// otherwise Cells is taken verbatim (possibly empty, to be filled in by hand later).
type CreateAreaInput struct {
	Name             string
	WarmStrategy     beeline.WarmStrategy
	RoutingProvider  string
	GeoJSON          []byte
	Cells            []beeline.H3Cell
	DemandIdleTTL    time.Duration
	TargetTTL        time.Duration
	LeaseDuration    time.Duration
	SweepInterval    time.Duration
	Resolution       int
	MaxRadiusMeters  float64
	CoreRadiusMeters float64
}

// UpdateAreaInput carries the mutable metadata of an area. Changing Resolution is
// only allowed when the area has a GeoJSON provenance to re-polyfill from.
type UpdateAreaInput struct {
	Name             string
	WarmStrategy     beeline.WarmStrategy
	RoutingProvider  string
	DemandIdleTTL    time.Duration
	TargetTTL        time.Duration
	LeaseDuration    time.Duration
	SweepInterval    time.Duration
	Resolution       int
	MaxRadiusMeters  float64
	CoreRadiusMeters float64
}

// enabledArea is the in-memory routing snapshot for one enabled area: its resolution,
// cell membership set, outer travel bound, and the routing engine it is served by,
// consulted by Locate and EngineFor on the read/refresh paths. The bound is carried
// here so the read path can decide, without a store round-trip, whether a demand-fill
// falls within the area's cacheable radius; the engine is resolved once at seed time
// so per-pair routing is a map lookup, not a registry walk.
type enabledArea struct {
	cells           map[beeline.H3Cell]struct{}
	engine          beeline.RoutingEngine
	demandIdleTTL   time.Duration
	targetTTL       time.Duration
	sweepInterval   time.Duration
	resolution      int
	maxRadiusMeters float64
}

// Coordinator serializes area lifecycle operations over the repository, index, and
// store, and answers point-in-area routing for the read path. It owns the routing
// provider registry: each enabled area resolves its named provider to a concrete
// engine at seed time, and EngineFor hands that engine to the refresh pool and read
// path so different areas route through different providers.
type Coordinator struct {
	repo            AreasRepository
	index           AreaIndex
	store           AreaStore
	providers       map[string]beeline.RoutingEngine
	enabled         map[beeline.AreaID]*enabledArea
	lastSwept       map[beeline.AreaID]time.Time
	defaultProvider string
	profiles        []beeline.Profile
	defaults        FreshnessDefaults
	mu              sync.RWMutex
}

// FreshnessDefaults are the house freshness values applied to a new or updated area when
// the caller leaves a per-area knob unset (zero). They come from the global matrix config;
// each area then stores and honors its own values, so the global values are only a seed
// for newly-created areas.
type FreshnessDefaults struct {
	TargetTTL     time.Duration
	LeaseDuration time.Duration
	SweepInterval time.Duration
}

// New builds a Coordinator over the given seams and provider registry. providers maps
// a provider name to its engine and must contain defaultProvider (the engine used by
// areas that name no provider and by out-of-area reads). defaults seeds a new area's
// per-area freshness knobs when the create/update input leaves them unset. No areas are
// seeded yet — call ResumeEnabled at boot to seed the ones already enabled.
func New(
	repo AreasRepository,
	index AreaIndex,
	store AreaStore,
	profiles []beeline.Profile,
	providers map[string]beeline.RoutingEngine,
	defaultProvider string,
	defaults FreshnessDefaults,
) *Coordinator {
	return &Coordinator{
		repo:            repo,
		index:           index,
		store:           store,
		providers:       providers,
		profiles:        profiles,
		defaultProvider: defaultProvider,
		defaults:        defaults,
		enabled:         make(map[beeline.AreaID]*enabledArea),
		lastSwept:       make(map[beeline.AreaID]time.Time),
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
	strategy := normalizeStrategy(in.WarmStrategy)
	provider := c.normalizeProvider(in.RoutingProvider)
	if err := validateAreaFields(in.Name, in.Resolution, in.MaxRadiusMeters, in.CoreRadiusMeters, strategy, in.DemandIdleTTL); err != nil {
		return beeline.Area{}, err
	}
	if err := c.validateProvider(provider); err != nil {
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
	if err := validateRadiusFloor(in.Resolution, in.MaxRadiusMeters, cells); err != nil {
		return beeline.Area{}, err
	}
	targetTTL, lease, sweep, err := c.resolveFreshness(in.TargetTTL, in.LeaseDuration, in.SweepInterval)
	if err != nil {
		return beeline.Area{}, err
	}

	return c.repo.Create(ctx, &beeline.Area{
		Name:             in.Name,
		Resolution:       in.Resolution,
		MaxRadiusMeters:  in.MaxRadiusMeters,
		CoreRadiusMeters: in.CoreRadiusMeters,
		WarmStrategy:     strategy,
		RoutingProvider:  provider,
		DemandIdleTTL:    in.DemandIdleTTL,
		TargetTTL:        targetTTL,
		LeaseDuration:    lease,
		SweepInterval:    sweep,
		Cells:            cells,
		GeoJSON:          in.GeoJSON,
		Enabled:          false,
	})
}

// Update rewrites an area's metadata. A resolution change re-polyfills the cell set
// from the area's GeoJSON provenance; changing the resolution of a hand-built area
// (no GeoJSON) is rejected because its cells cannot be reprojected. If the area is
// enabled, its working set is re-converged.
func (c *Coordinator) Update(ctx context.Context, id beeline.AreaID, in *UpdateAreaInput) (beeline.Area, error) {
	strategy := normalizeStrategy(in.WarmStrategy)
	provider := c.normalizeProvider(in.RoutingProvider)
	if err := validateAreaFields(in.Name, in.Resolution, in.MaxRadiusMeters, in.CoreRadiusMeters, strategy, in.DemandIdleTTL); err != nil {
		return beeline.Area{}, err
	}
	if err := c.validateProvider(provider); err != nil {
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
	if err = validateRadiusFloor(in.Resolution, in.MaxRadiusMeters, area.Cells); err != nil {
		return beeline.Area{}, err
	}
	targetTTL, lease, sweep, err := c.resolveFreshness(in.TargetTTL, in.LeaseDuration, in.SweepInterval)
	if err != nil {
		return beeline.Area{}, err
	}

	area.Name = in.Name
	area.Resolution = in.Resolution
	area.MaxRadiusMeters = in.MaxRadiusMeters
	area.CoreRadiusMeters = in.CoreRadiusMeters
	area.WarmStrategy = strategy
	area.RoutingProvider = provider
	area.DemandIdleTTL = in.DemandIdleTTL
	area.TargetTTL = targetTTL
	area.LeaseDuration = lease
	area.SweepInterval = sweep

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
			return beeline.RoutedArea{
				ID:              id,
				Resolution:      ea.resolution,
				MaxRadiusMeters: ea.maxRadiusMeters,
				TargetTTL:       ea.targetTTL,
			}, true
		}
	}

	return beeline.RoutedArea{}, false
}

// EngineFor returns the routing engine an area is served by, implementing the
// beeline.EngineResolver seam for the refresh pool and read path. An enabled area uses
// the engine resolved from its provider at seed time; an unknown or zero area id (an
// out-of-area read that belongs to no partition) falls back to the default engine, so
// a query outside every area is still answered.
func (c *Coordinator) EngineFor(id beeline.AreaID) beeline.RoutingEngine {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if ea, ok := c.enabled[id]; ok && ea.engine != nil {
		return ea.engine
	}

	return c.providers[c.defaultProvider]
}

// ProviderNames returns the configured provider names (the default first, then the
// rest ascending), so the operator console can populate its provider picker from live
// config rather than a hard-coded list.
func (c *Coordinator) ProviderNames() []string {
	names := make([]string, 0, len(c.providers))
	for name := range c.providers {
		if name != c.defaultProvider {
			names = append(names, name)
		}
	}
	slices.Sort(names)

	return append([]string{c.defaultProvider}, names...)
}

// normalizeProvider maps the empty (unset) provider to the default, preserving today's
// behavior for areas that name no provider. Any other value is returned unchanged for
// validateProvider to accept or reject.
func (c *Coordinator) normalizeProvider(name string) string {
	if name == "" {
		return c.defaultProvider
	}

	return name
}

// validateProvider rejects a provider name that is not in the configured registry, so
// an area cannot reference an engine that does not exist.
func (c *Coordinator) validateProvider(name string) error {
	if _, ok := c.providers[name]; !ok {
		return fmt.Errorf("control: unknown routing provider %q", name)
	}

	return nil
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

// SweepDue evicts cold demand pairs from each enabled area that is due for a sweep on
// its own cadence: an area is swept when at least its SweepInterval has elapsed since it
// was last swept and decay is enabled for it (a positive DemandIdleTTL). For a due area,
// any unpinned pair not queried within its DemandIdleTTL is dropped from both the
// freshness index and the hot store, so cost tracks real usage instead of ratcheting up
// forever. It is serialized under the same lock as enable/disable, so a sweep never races
// an area's teardown. The janitor loop calls it on a fixed base tick; each area's own
// SweepInterval gates how often it is actually swept.
func (c *Coordinator) SweepDue(ctx context.Context, now time.Time) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	swept := 0
	for id, ea := range c.enabled {
		if ea.demandIdleTTL <= 0 {
			continue // decay disabled for this area
		}
		if last, ok := c.lastSwept[id]; ok && now.Sub(last) < ea.sweepInterval {
			continue // not yet due on this area's cadence
		}
		c.lastSwept[id] = now

		removed, err := c.index.SweepArea(ctx, id, now.Add(-ea.demandIdleTTL))
		if err != nil {
			return swept, err
		}
		if len(removed) == 0 {
			continue
		}
		if delErr := c.store.Delete(ctx, removed); delErr != nil {
			return swept, delErr
		}
		swept += len(removed)
	}

	return swept, nil
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

// seedLocked seeds an area's eager pairs into the index and records the routing
// snapshot. How much is seeded depends on the warm strategy: eager pins the whole
// MaxRadiusMeters bound (as before), lazy seeds nothing (the working set grows purely
// from demand), and hybrid pins only the CoreRadiusMeters near field. In every case
// the routing snapshot records the full cell set and bound so Locate works and the
// read path can demand-fill the (unseeded) tail. Callers hold c.mu.
func (c *Coordinator) seedLocked(ctx context.Context, area *beeline.Area) error {
	pairs, err := c.eagerSeedPairs(area)
	if err != nil {
		return err
	}

	if err = c.index.Seed(ctx, pairs); err != nil {
		return err
	}
	// Register this area's freshness contract so the index applies its own target TTL
	// (staleness) and lease (claim visibility) rather than the global defaults. Done for
	// every strategy, including lazy — its demand-filled pairs must honor the same contract.
	if err = c.index.SetAreaFreshness(ctx, area.ID, area.TargetTTL, area.LeaseDuration); err != nil {
		return err
	}

	cellSet := make(map[beeline.H3Cell]struct{}, len(area.Cells))
	for _, cell := range area.Cells {
		cellSet[cell] = struct{}{}
	}
	c.enabled[area.ID] = &enabledArea{
		resolution:      area.Resolution,
		cells:           cellSet,
		engine:          c.providers[c.normalizeProvider(area.RoutingProvider)],
		maxRadiusMeters: area.MaxRadiusMeters,
		demandIdleTTL:   area.DemandIdleTTL,
		targetTTL:       area.TargetTTL,
		sweepInterval:   area.SweepInterval,
	}

	return nil
}

// eagerSeedPairs is the set of pairs to pin fresh at enable time for an area, chosen by
// its warm strategy. lazy pins nothing; hybrid pins the core near field (and only when
// a positive core radius is set — a zero core is an empty core, not the full-mesh
// sentinel); eager pins the entire bound (which may itself be the full mesh).
func (c *Coordinator) eagerSeedPairs(area *beeline.Area) ([]beeline.PairKey, error) {
	switch area.WarmStrategy {
	case beeline.WarmLazy:
		return nil, nil
	case beeline.WarmHybrid:
		if area.CoreRadiusMeters <= 0 {
			return nil, nil
		}

		return tessellate.PairsFromCells(area.ID, area.Cells, area.Resolution, area.CoreRadiusMeters, c.profiles)
	default: // WarmEager
		return tessellate.PairsFromCells(area.ID, area.Cells, area.Resolution, area.MaxRadiusMeters, c.profiles)
	}
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
	delete(c.lastSwept, id)

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

// normalizeStrategy maps the empty (unset) strategy to the eager default, preserving
// today's behavior for callers that don't specify one. Any other value is returned
// unchanged for validateAreaFields to accept or reject.
func normalizeStrategy(s beeline.WarmStrategy) beeline.WarmStrategy {
	if s == "" {
		return beeline.WarmEager
	}

	return s
}

// validateAreaFields rejects area metadata the tessellator or store would refuse. A
// maxRadiusMeters of 0 is the full-mesh sentinel (every in-area pair); any positive
// value is a travel-radius bound. The core radius is only meaningful for the hybrid
// strategy and must lie within [0, max]. A full-mesh area (max == 0) must be eager,
// since there is no bounded tail to fill on demand.
func validateAreaFields(name string, resolution int, maxRadiusMeters, coreRadiusMeters float64, strategy beeline.WarmStrategy, demandIdleTTL time.Duration) error {
	if name == "" {
		return errors.New("control: area name is required")
	}
	if resolution < 0 || resolution > 15 {
		return fmt.Errorf("control: resolution %d out of range [0,15]", resolution)
	}
	if maxRadiusMeters < 0 {
		return fmt.Errorf("control: max radius meters %.2f must be >= 0 (0 = full mesh)", maxRadiusMeters)
	}
	if !strategy.Valid() {
		return fmt.Errorf("control: unknown warm strategy %q (want eager, lazy, or hybrid)", strategy)
	}
	if maxRadiusMeters == 0 && strategy != beeline.WarmEager {
		return fmt.Errorf("control: full-mesh area (maxRadiusMeters 0) must use the eager strategy, got %q", strategy)
	}
	if coreRadiusMeters < 0 {
		return fmt.Errorf("control: core radius meters %.2f must be >= 0", coreRadiusMeters)
	}
	if maxRadiusMeters > 0 && coreRadiusMeters > maxRadiusMeters {
		return fmt.Errorf("control: core radius meters %.2f must be <= max radius meters %.2f", coreRadiusMeters, maxRadiusMeters)
	}
	if demandIdleTTL < 0 {
		return fmt.Errorf("control: demand idle TTL %v must be >= 0 (0 = decay disabled)", demandIdleTTL)
	}

	return nil
}

// resolveFreshness fills any unset (non-positive) per-area freshness knob from the
// coordinator's house defaults, then validates the result. Blank/zero means "use the
// default"; the final values must all be positive, since a zero target TTL, lease, or
// sweep interval has no valid meaning in the freshness contract (unlike DemandIdleTTL,
// where zero disables decay).
func (c *Coordinator) resolveFreshness(targetTTL, lease, sweep time.Duration) (rTTL, rLease, rSweep time.Duration, err error) {
	rTTL, rLease, rSweep = targetTTL, lease, sweep
	if rTTL <= 0 {
		rTTL = c.defaults.TargetTTL
	}
	if rLease <= 0 {
		rLease = c.defaults.LeaseDuration
	}
	if rSweep <= 0 {
		rSweep = c.defaults.SweepInterval
	}
	if rTTL <= 0 || rLease <= 0 || rSweep <= 0 {
		return 0, 0, 0, fmt.Errorf(
			"control: target TTL, lease, and sweep interval must be positive (got %v, %v, %v)",
			rTTL, rLease, rSweep,
		)
	}

	return rTTL, rLease, rSweep, nil
}

// validateRadiusFloor rejects a bounded travel radius so small it reaches no neighbor
// cell at the area's resolution — the degenerate case where every origin pairs only
// with itself (RingsForRadius == 0). A full-mesh area (maxRadiusMeters == 0) has no
// floor; an area with no cells yet is skipped (it has no pairs to build). The floor is
// measured from a representative cell (the same cells[0] PairsFromCells derives its ring
// count from), so acceptance here matches the pair set the tessellator will actually build.
func validateRadiusFloor(resolution int, maxRadiusMeters float64, cells []beeline.H3Cell) error {
	if maxRadiusMeters <= 0 || len(cells) == 0 {
		return nil
	}

	floor, err := tessellate.MinRadiusForNeighbors(cells[0])
	if err != nil {
		return err
	}
	if maxRadiusMeters < floor {
		return fmt.Errorf(
			"control: max radius %.0fm is below the res-%d neighbor floor of ~%.0fm; raise it or use 0 (full mesh)",
			maxRadiusMeters, resolution, floor,
		)
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
