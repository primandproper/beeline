// Package control is the runtime control plane for service areas. It turns the
// persistent set of operator-configured areas into live refresh work: areas are
// created disabled, and only when enabled does the Coordinator polyfill each of the
// area's layers from its GeoJSON geometry, tessellate the layers into pairs, and
// seed them into the shared freshness index for the refresh pool to burn down.
// Disabling an area removes its pairs and cached estimates again.
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
}

// AreaIndex is the freshness-index seam: seed an area's pairs, remove them, sweep its
// cold demand pairs, and report per-area freshness. The in-memory index satisfies it.
type AreaIndex interface {
	Seed(ctx context.Context, keys []beeline.PairKey) error
	// Bump raises pairs' refresh priority (adding unknown keys as unpinned demand
	// entries), the decayable half of the model-driven warm feed.
	Bump(ctx context.Context, keys []beeline.PairKey) error
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

// CreateAreaInput describes a new area. GeoJSON is the canonical, required
// geometry: every layer's cell set is derived from it by polyfill at seed time,
// so nothing cell-shaped is supplied here. Layers may arrive in any order; the
// coordinator sorts them finest→coarsest before validating and persisting.
type CreateAreaInput struct {
	Name            string
	WarmStrategy    beeline.WarmStrategy
	RoutingProvider string
	GeoJSON         []byte
	Layers          []beeline.Layer
	DemandIdleTTL   time.Duration
	TargetTTL       time.Duration
	LeaseDuration   time.Duration
	SweepInterval   time.Duration
}

// UpdateAreaInput carries the mutable metadata of an area. Layers replaces the
// whole layer list (validated against the area's existing GeoJSON geometry);
// geometry itself changes through SetGeoJSON.
type UpdateAreaInput struct {
	Name            string
	WarmStrategy    beeline.WarmStrategy
	RoutingProvider string
	Layers          []beeline.Layer
	DemandIdleTTL   time.Duration
	TargetTTL       time.Duration
	LeaseDuration   time.Duration
	SweepInterval   time.Duration
}

// enabledLayer is one layer of an enabled area's routing snapshot: the cell
// membership set polyfilled at seed time, and the per-layer knobs the read path
// needs (resolution to key lookups, bound to gate demand-fill caching, min
// distance for future selection). The finest layer (index 0) defines containment.
type enabledLayer struct {
	cells             map[beeline.H3Cell]struct{}
	resolution        int
	minDistanceMeters float64
	maxRadiusMeters   float64
}

// enabledArea is the in-memory routing snapshot for one enabled area: its layers
// (finest→coarsest, mirroring Area.Layers) and the routing engine it is served
// by, consulted by Locate and EngineFor on the read/refresh paths. Layer bounds
// are carried here so the read path can decide, without a store round-trip,
// whether a demand-fill falls within the cacheable radius; the engine is resolved
// once at seed time so per-pair routing is a map lookup, not a registry walk.
type enabledArea struct {
	engine        beeline.RoutingEngine
	layers        []enabledLayer
	demandIdleTTL time.Duration
	targetTTL     time.Duration
	sweepInterval time.Duration
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

// Create validates and persists a new area, always disabled. GeoJSON is required —
// it is the canonical geometry every layer polyfills from — but it is not polyfilled
// here: cells are derived at seed time, so creation stays cheap and an area does no
// refresh work until enabled.
func (c *Coordinator) Create(ctx context.Context, in *CreateAreaInput) (beeline.Area, error) {
	strategy := normalizeStrategy(in.WarmStrategy)
	provider := c.normalizeProvider(in.RoutingProvider)
	if err := validateAreaFields(in.Name, strategy, in.DemandIdleTTL); err != nil {
		return beeline.Area{}, err
	}
	if err := c.validateProvider(provider); err != nil {
		return beeline.Area{}, err
	}
	if len(in.GeoJSON) == 0 {
		return beeline.Area{}, errors.New("control: geojson geometry is required")
	}

	sample, err := tessellate.SamplePoint(in.GeoJSON)
	if err != nil {
		return beeline.Area{}, err
	}
	layers, err := normalizeAndValidateLayers(in.Layers, strategy, sample)
	if err != nil {
		return beeline.Area{}, err
	}
	targetTTL, lease, sweep, err := c.resolveFreshness(in.TargetTTL, in.LeaseDuration, in.SweepInterval)
	if err != nil {
		return beeline.Area{}, err
	}

	return c.repo.Create(ctx, &beeline.Area{
		Name:            in.Name,
		WarmStrategy:    strategy,
		RoutingProvider: provider,
		DemandIdleTTL:   in.DemandIdleTTL,
		TargetTTL:       targetTTL,
		LeaseDuration:   lease,
		SweepInterval:   sweep,
		Layers:          layers,
		GeoJSON:         in.GeoJSON,
		Enabled:         false,
	})
}

// Update rewrites an area's metadata, replacing its whole layer list. The layers are
// validated against the area's existing GeoJSON geometry (geometry itself changes
// through SetGeoJSON). If the area is enabled, its working set is re-converged.
func (c *Coordinator) Update(ctx context.Context, id beeline.AreaID, in *UpdateAreaInput) (beeline.Area, error) {
	strategy := normalizeStrategy(in.WarmStrategy)
	provider := c.normalizeProvider(in.RoutingProvider)
	if err := validateAreaFields(in.Name, strategy, in.DemandIdleTTL); err != nil {
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

	sample, err := tessellate.SamplePoint(area.GeoJSON)
	if err != nil {
		return beeline.Area{}, err
	}
	layers, err := normalizeAndValidateLayers(in.Layers, strategy, sample)
	if err != nil {
		return beeline.Area{}, err
	}
	targetTTL, lease, sweep, err := c.resolveFreshness(in.TargetTTL, in.LeaseDuration, in.SweepInterval)
	if err != nil {
		return beeline.Area{}, err
	}

	area.Name = in.Name
	area.Layers = layers
	area.WarmStrategy = strategy
	area.RoutingProvider = provider
	area.DemandIdleTTL = in.DemandIdleTTL
	area.TargetTTL = targetTTL
	area.LeaseDuration = lease
	area.SweepInterval = sweep

	return c.persistAndConvergeLocked(ctx, &area)
}

// SetGeoJSON replaces an area's canonical geometry from an uploaded polygon. The
// existing layer list is re-validated against the new geometry (the radius floors
// depend on where on the globe the sample cell lands); the layers' cell sets are
// re-derived at seed time. An enabled area is re-converged.
func (c *Coordinator) SetGeoJSON(ctx context.Context, id beeline.AreaID, raw []byte) (beeline.Area, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	area, err := c.repo.Get(ctx, id)
	if err != nil {
		return beeline.Area{}, err
	}

	sample, err := tessellate.SamplePoint(raw)
	if err != nil {
		return beeline.Area{}, err
	}
	if _, err = normalizeAndValidateLayers(area.Layers, area.WarmStrategy, sample); err != nil {
		return beeline.Area{}, err
	}
	area.GeoJSON = raw

	return c.persistAndConvergeLocked(ctx, &area)
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

// Locate resolves a coordinate to the enabled area containing it. Containment is
// membership in the finest layer's cell set, evaluated at the finest layer's
// resolution — coarser layers polyfill the same geometry, so the finest set is the
// most faithful footprint (and for one-layer areas this is exactly the old
// single-resolution behavior). On overlap the lowest area id wins,
// deterministically. It implements the read path's AreaRouter seam.
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
		if len(ea.layers) == 0 {
			continue
		}
		finest := ea.layers[0]
		cell, err := beeline.CellAt(p, finest.resolution)
		if err != nil {
			continue
		}
		if _, ok := finest.cells[cell]; ok {
			routedLayers := make([]beeline.RoutedLayer, len(ea.layers))
			for i := range ea.layers {
				routedLayers[i] = beeline.RoutedLayer{
					Resolution:        ea.layers[i].resolution,
					MinDistanceMeters: ea.layers[i].minDistanceMeters,
					MaxRadiusMeters:   ea.layers[i].maxRadiusMeters,
				}
			}

			return beeline.RoutedArea{
				ID:        id,
				Layers:    routedLayers,
				TargetTTL: ea.targetTTL,
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

// CellStatesForArea proxies the index's per-area rollup for the progress map. The
// rollup spans all of the area's layers (origin cells at every layer resolution
// appear side by side); each CellState's Origin self-encodes its resolution.
func (c *Coordinator) CellStatesForArea(ctx context.Context, id beeline.AreaID) ([]beeline.CellState, error) {
	return c.index.CellStatesForArea(ctx, id)
}

// DebtForArea proxies the index's per-area freshness contract. Freshness accounting
// is per area, not per layer: a multi-layer area's debt and throughput blend all of
// its layers (accepted — the contract is the area's).
func (c *Coordinator) DebtForArea(ctx context.Context, id beeline.AreaID) (beeline.DebtStats, error) {
	return c.index.DebtForArea(ctx, id)
}

// ErrInvalidWarm marks a warm-feed request the coordinator rejected (unknown or
// disabled area, or a pair that does not belong to it), so the HTTP layer can map
// validation failures to 400 without matching error text.
var ErrInvalidWarm = errors.New("control: invalid warm request")

// WarmPairs feeds externally predicted demand — e.g. a model trained on the
// telemetry this service exports — into the freshness index for one enabled area,
// so the refresh pool computes those pairs before they are queried. Every pair must
// belong to the area: keyed at its finest layer's resolution, both cells inside the
// polyfilled cell set, a configured profile, and origin ≠ dest (same-cell trips are
// never cached, so warming one is meaningless); Area is stamped from id. The first
// invalid pair rejects the whole request with ErrInvalidWarm.
//
// pin=false is the recommended mode: Bump adds the pairs as unpinned demand
// entries at top refresh priority, and if the prediction was wrong they decay
// through the area's normal demand sweep. pin=true additionally Seeds them —
// pinned, surviving the sweep until the area is disabled — for pairs a model deems
// perennial. Note that Seed resets the area's achieved-throughput baseline, so
// pinning restarts the /_ops_/freshness throughput readout; repeated seed-mode
// calls also grow the pinned set without bound.
func (c *Coordinator) WarmPairs(ctx context.Context, id beeline.AreaID, pairs []beeline.PairKey, pin bool) (int, error) {
	if len(pairs) == 0 {
		return 0, fmt.Errorf("%w: at least one pair is required", ErrInvalidWarm)
	}

	c.mu.RLock()
	ea, ok := c.enabled[id]
	if !ok || len(ea.layers) == 0 {
		c.mu.RUnlock()

		return 0, fmt.Errorf("%w: area %d is not enabled", ErrInvalidWarm, id)
	}
	finest := ea.layers[0]

	keys := make([]beeline.PairKey, len(pairs))
	for i := range pairs {
		p := pairs[i]
		if err := c.validateWarmPair(&finest, &p); err != nil {
			c.mu.RUnlock()

			return 0, err
		}
		p.Area = id
		keys[i] = p
	}
	c.mu.RUnlock()

	// Outside the lock: Seed/Bump serialize on the index's own lock, and a
	// concurrent disable at worst warms pairs the Unseed immediately removes.
	if pin {
		if err := c.index.Seed(ctx, keys); err != nil {
			return 0, err
		}
	}
	if err := c.index.Bump(ctx, keys); err != nil {
		return 0, err
	}

	return len(keys), nil
}

// validateWarmPair rejects a predicted pair that does not belong to the area's
// finest (read) layer or names an unknown profile.
func (c *Coordinator) validateWarmPair(finest *enabledLayer, p *beeline.PairKey) error {
	if p.Res != finest.resolution {
		return fmt.Errorf("%w: pair resolution %d does not match the area's read layer resolution %d", ErrInvalidWarm, p.Res, finest.resolution)
	}
	if p.Origin == p.Dest {
		return fmt.Errorf("%w: origin and destination cell %s are the same (same-cell trips are never cached)", ErrInvalidWarm, p.Origin)
	}
	if _, ok := finest.cells[p.Origin]; !ok {
		return fmt.Errorf("%w: origin cell %s is outside the area", ErrInvalidWarm, p.Origin)
	}
	if _, ok := finest.cells[p.Dest]; !ok {
		return fmt.Errorf("%w: destination cell %s is outside the area", ErrInvalidWarm, p.Dest)
	}
	if !slices.Contains(c.profiles, p.Profile) {
		return fmt.Errorf("%w: unknown profile %q", ErrInvalidWarm, p.Profile)
	}

	return nil
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

// seedLocked derives each layer's cell set from the area's GeoJSON (polyfilled at
// that layer's resolution), seeds the eager pairs of every layer into the index, and
// records the layered routing snapshot. How much of each layer is seeded depends on
// the area's warm strategy: eager pins the layer's whole MaxRadiusMeters bound, lazy
// seeds nothing (the working set grows purely from demand), and hybrid pins only the
// layer's CoreRadiusMeters near field. In every case the snapshot records every
// layer's full cell set and bound so Locate works and the read path can demand-fill
// the (unseeded) tail. Layers stay distinct downstream via PairKey.Res. Callers hold
// c.mu.
func (c *Coordinator) seedLocked(ctx context.Context, area *beeline.Area) error {
	var pairs []beeline.PairKey
	layers := make([]enabledLayer, 0, len(area.Layers))
	for i := range area.Layers {
		layer := area.Layers[i]
		cells, err := tessellate.CellsFromGeoJSON(area.GeoJSON, layer.Resolution)
		if err != nil {
			return err
		}

		eager, err := c.eagerSeedPairs(area, layer, cells)
		if err != nil {
			return err
		}
		pairs = append(pairs, eager...)

		cellSet := make(map[beeline.H3Cell]struct{}, len(cells))
		for _, cell := range cells {
			cellSet[cell] = struct{}{}
		}
		layers = append(layers, enabledLayer{
			cells:             cellSet,
			resolution:        layer.Resolution,
			minDistanceMeters: layer.MinDistanceMeters,
			maxRadiusMeters:   layer.MaxRadiusMeters,
		})
	}

	if err := c.index.Seed(ctx, pairs); err != nil {
		return err
	}
	// Register this area's freshness contract so the index applies its own target TTL
	// (staleness) and lease (claim visibility) rather than the global defaults. Done for
	// every strategy, including lazy — its demand-filled pairs must honor the same contract.
	if err := c.index.SetAreaFreshness(ctx, area.ID, area.TargetTTL, area.LeaseDuration); err != nil {
		return err
	}

	c.enabled[area.ID] = &enabledArea{
		layers:        layers,
		engine:        c.providers[c.normalizeProvider(area.RoutingProvider)],
		demandIdleTTL: area.DemandIdleTTL,
		targetTTL:     area.TargetTTL,
		sweepInterval: area.SweepInterval,
	}

	return nil
}

// eagerSeedPairs is the set of one layer's pairs to pin fresh at enable time, chosen
// by the area's warm strategy. lazy pins nothing; hybrid pins the layer's core near
// field (and only when a positive core radius is set — a zero core is an empty core,
// not the full-mesh sentinel); eager pins the layer's entire bound (which may itself
// be the full mesh).
func (c *Coordinator) eagerSeedPairs(area *beeline.Area, layer beeline.Layer, cells []beeline.H3Cell) ([]beeline.PairKey, error) {
	switch area.WarmStrategy {
	case beeline.WarmLazy:
		return nil, nil
	case beeline.WarmHybrid:
		if layer.CoreRadiusMeters <= 0 {
			return nil, nil
		}

		return tessellate.PairsFromCells(area.ID, cells, layer.Resolution, layer.CoreRadiusMeters, c.profiles)
	default: // WarmEager
		return tessellate.PairsFromCells(area.ID, cells, layer.Resolution, layer.MaxRadiusMeters, c.profiles)
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

// validateAreaFields rejects area metadata the store would refuse; the layer list
// has its own validator (normalizeAndValidateLayers).
func validateAreaFields(name string, strategy beeline.WarmStrategy, demandIdleTTL time.Duration) error {
	if name == "" {
		return errors.New("control: area name is required")
	}
	if !strategy.Valid() {
		return fmt.Errorf("control: unknown warm strategy %q (want eager, lazy, or hybrid)", strategy)
	}
	if demandIdleTTL < 0 {
		return fmt.Errorf("control: demand idle TTL %v must be >= 0 (0 = decay disabled)", demandIdleTTL)
	}

	return nil
}

// normalizeAndValidateLayers sorts the layer list finest→coarsest (descending
// resolution — the ordering invariant Area.Layers carries everywhere) and validates
// it as a whole:
//
//   - at least one layer; resolutions unique, each in [0,15];
//   - the finest layer serves reads today, so its MinDistanceMeters must be 0, and
//     min distances must not decrease toward coarser layers (a coarser layer serves
//     longer trips — the DoorDash-style selection the field is recorded for);
//   - per layer, a maxRadiusMeters of 0 is the full-mesh sentinel (every in-layer
//     pair) and requires the eager strategy, since there is no bounded tail to fill
//     on demand; any positive value is a travel-radius bound and must clear the
//     layer's neighbor floor (a bound so small it reaches no neighbor cell would
//     degenerate to self-pairs only), with the hybrid core inside it.
//
// sample is a representative in-area coordinate (tessellate.SamplePoint) used to
// derive each layer's floor cell without polyfilling.
func normalizeAndValidateLayers(in []beeline.Layer, strategy beeline.WarmStrategy, sample beeline.LatLng) ([]beeline.Layer, error) {
	if len(in) == 0 {
		return nil, errors.New("control: at least one layer is required")
	}

	layers := slices.Clone(in)
	slices.SortFunc(layers, func(a, b beeline.Layer) int { return b.Resolution - a.Resolution })

	for i := range layers {
		layer := layers[i]
		if layer.Resolution < 0 || layer.Resolution > 15 {
			return nil, fmt.Errorf("control: layer resolution %d out of range [0,15]", layer.Resolution)
		}
		if i > 0 && layer.Resolution == layers[i-1].Resolution {
			return nil, fmt.Errorf("control: duplicate layer resolution %d", layer.Resolution)
		}
		if layer.MinDistanceMeters < 0 {
			return nil, fmt.Errorf("control: layer res %d: min distance meters %.2f must be >= 0", layer.Resolution, layer.MinDistanceMeters)
		}
		if i == 0 && layer.MinDistanceMeters != 0 {
			return nil, fmt.Errorf("control: the finest layer (res %d) serves all reads and must have min distance 0, got %.2f", layer.Resolution, layer.MinDistanceMeters)
		}
		if i > 0 && layer.MinDistanceMeters < layers[i-1].MinDistanceMeters {
			return nil, fmt.Errorf("control: layer res %d: min distance %.2f is below the finer res-%d layer's %.2f (coarser layers serve longer trips)",
				layer.Resolution, layer.MinDistanceMeters, layers[i-1].Resolution, layers[i-1].MinDistanceMeters)
		}
		if layer.MaxRadiusMeters < 0 {
			return nil, fmt.Errorf("control: layer res %d: max radius meters %.2f must be >= 0 (0 = full mesh)", layer.Resolution, layer.MaxRadiusMeters)
		}
		if layer.MaxRadiusMeters == 0 && strategy != beeline.WarmEager {
			return nil, fmt.Errorf("control: full-mesh layer (res %d, maxRadiusMeters 0) must use the eager strategy, got %q", layer.Resolution, strategy)
		}
		if layer.CoreRadiusMeters < 0 {
			return nil, fmt.Errorf("control: layer res %d: core radius meters %.2f must be >= 0", layer.Resolution, layer.CoreRadiusMeters)
		}
		if layer.MaxRadiusMeters > 0 && layer.CoreRadiusMeters > layer.MaxRadiusMeters {
			return nil, fmt.Errorf("control: layer res %d: core radius meters %.2f must be <= max radius meters %.2f",
				layer.Resolution, layer.CoreRadiusMeters, layer.MaxRadiusMeters)
		}
		if err := validateRadiusFloor(layer, sample); err != nil {
			return nil, err
		}
	}

	return layers, nil
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
// cell at the layer's resolution — the degenerate case where every origin pairs only
// with itself (RingsForRadius == 0). A full-mesh layer (maxRadiusMeters == 0) has no
// floor. The floor is measured from the cell containing the geometry's sample point,
// a representative of the cells PairsFromCells will derive its ring count from, so
// acceptance here matches the pair set the tessellator will actually build.
func validateRadiusFloor(layer beeline.Layer, sample beeline.LatLng) error {
	if layer.MaxRadiusMeters <= 0 {
		return nil
	}

	cell, err := beeline.CellAt(sample, layer.Resolution)
	if err != nil {
		return err
	}
	floor, err := tessellate.MinRadiusForNeighbors(cell)
	if err != nil {
		return err
	}
	if layer.MaxRadiusMeters < floor {
		return fmt.Errorf(
			"control: max radius %.0fm is below the res-%d neighbor floor of ~%.0fm; raise it or use 0 (full mesh)",
			layer.MaxRadiusMeters, layer.Resolution, floor,
		)
	}

	return nil
}
