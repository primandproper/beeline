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

	"github.com/primandproper/primitives-go/v2/distributedlock"
	"github.com/primandproper/primitives-go/v2/distributedlock/noop"
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

// ProvidersRepository persists operator-defined routing-provider specs — the
// control-plane half of the registry (built-ins are synthesized, never stored).
// The SQLite store satisfies it.
type ProvidersRepository interface {
	ListProviders(ctx context.Context) ([]beeline.ProviderSpec, error)
	UpsertProvider(ctx context.Context, spec *beeline.ProviderSpec) error
	DeleteProvider(ctx context.Context, name string) error
}

// EngineBuilder constructs a routing engine from one provider spec. The CLI wires
// it to the engine registry (closing over the profile speed map), so the control
// plane can rebuild engines on provider mutations without depending on concrete
// engine packages.
type EngineBuilder func(spec *beeline.ProviderSpec) (beeline.RoutingEngine, error)

// AreaIndex is the freshness-index seam: seed an area's pairs, remove them, sweep its
// cold demand pairs, and report per-area freshness. The in-memory index satisfies it.
type AreaIndex interface {
	Seed(ctx context.Context, keys []beeline.PairKey) error
	// Bump raises pairs' refresh priority (adding unknown keys as unpinned demand
	// entries), the decayable half of the model-driven warm feed.
	Bump(ctx context.Context, keys []beeline.PairKey) error
	Unseed(ctx context.Context, area beeline.AreaID) error
	// Invalidate re-enqueues the computed pairs a selector matches, returning how
	// many. The coordinator scopes it to one area (and optionally one of that
	// area's precision layers) for operator-driven cache invalidation.
	Invalidate(ctx context.Context, sel beeline.Selector) (int, error)
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

// Locker serializes one area's lifecycle mutations across processes. A single
// node needs no cross-process lock (the Coordinator's own mutex serializes
// everything), so the default is a no-op; distributed mode injects a
// Postgres advisory-lock implementation so two heads cannot interleave, say,
// an Enable and a Disable of the same area between repository write and index
// seed.
type Locker interface {
	WithAreaLock(ctx context.Context, id beeline.AreaID, fn func(ctx context.Context) error) error
}

// noopLocker is the single-node default: it delegates to primitives-go's no-op
// scoped locker, which runs fn unguarded. The area id is irrelevant to it, so
// the key is a constant.
type noopLocker struct {
	scoped distributedlock.ScopedLocker
}

func newNoopLocker() noopLocker {
	return noopLocker{scoped: noop.NewScopedLocker()}
}

func (l noopLocker) WithAreaLock(ctx context.Context, _ beeline.AreaID, fn func(ctx context.Context) error) error {
	return l.scoped.WithLock(ctx, lockNameSingleNode, fn)
}

// lockNameSingleNode is the key the no-op locker records; nothing contends on it.
const lockNameSingleNode = "beeline:single-node"

// BootSeedLocker is the optional Locker extension for multi-head boot:
// ResumeEnabled runs under this lock when the locker provides it, so heads
// booting simultaneously seed the shared index one at a time (the first
// populates it, the rest observe populated areas and only project).
type BootSeedLocker interface {
	WithBootSeedLock(ctx context.Context, fn func(ctx context.Context) error) error
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
	provider      string
	layers        []enabledLayer
	demandIdleTTL time.Duration
	targetTTL     time.Duration
	sweepInterval time.Duration
}

// Coordinator serializes area lifecycle operations over the repository, index, and
// store, and answers point-in-area routing for the read path. It owns the routing
// provider registry: built-in specs are synthesized at construction, operator-defined
// specs live in the providers repository (mutated through Put/DeleteProvider), and
// every spec is resolved to a concrete engine through the injected builder. Each
// enabled area resolves its named provider to an engine at seed time, and EngineFor
// hands that engine to the refresh pool and read path so different areas route
// through different providers. The full registry — specs plus the profile speed map —
// is also published as a content-hashed catalog (Catalog/ProvidersHash) that
// followers sync their own engines from.
type Coordinator struct {
	repo            AreasRepository
	providersRepo   ProvidersRepository
	index           AreaIndex
	store           AreaStore
	locker          Locker
	buildEngine     EngineBuilder
	providers       map[string]beeline.RoutingEngine
	builtinSpecs    map[string]beeline.ProviderSpec
	dbSpecs         map[string]beeline.ProviderSpec
	speeds          map[string]float64
	enabled         map[beeline.AreaID]*enabledArea
	lastSwept       map[beeline.AreaID]time.Time
	catalog         beeline.ProviderCatalog
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

// Config wires a Coordinator's seams and registry inputs. Locker is optional:
// nil means single-node (no cross-process serialization needed).
type Config struct {
	Areas           AreasRepository
	Providers       ProvidersRepository
	Index           AreaIndex
	Store           AreaStore
	Locker          Locker
	BuildEngine     EngineBuilder
	Speeds          map[string]float64
	DefaultProvider string
	Builtins        []beeline.ProviderSpec
	Defaults        FreshnessDefaults
}

// New builds a Coordinator and constructs the built-in provider engines. The
// operator-defined half of the registry is loaded by InitProviders; areas are seeded
// by ResumeEnabled. Call both at boot, in that order.
func New(cfg *Config) (*Coordinator, error) {
	profiles := make([]beeline.Profile, 0, len(cfg.Speeds))
	for name := range cfg.Speeds {
		profiles = append(profiles, beeline.Profile(name))
	}
	slices.Sort(profiles)

	locker := cfg.Locker
	if locker == nil {
		locker = newNoopLocker()
	}

	c := &Coordinator{
		repo:            cfg.Areas,
		providersRepo:   cfg.Providers,
		index:           cfg.Index,
		store:           cfg.Store,
		locker:          locker,
		buildEngine:     cfg.BuildEngine,
		speeds:          cfg.Speeds,
		profiles:        profiles,
		defaultProvider: cfg.DefaultProvider,
		defaults:        cfg.Defaults,
		providers:       make(map[string]beeline.RoutingEngine, len(cfg.Builtins)),
		builtinSpecs:    make(map[string]beeline.ProviderSpec, len(cfg.Builtins)),
		dbSpecs:         make(map[string]beeline.ProviderSpec),
		enabled:         make(map[beeline.AreaID]*enabledArea),
		lastSwept:       make(map[beeline.AreaID]time.Time),
	}

	for i := range cfg.Builtins {
		spec := cfg.Builtins[i]
		engine, err := cfg.BuildEngine(&spec)
		if err != nil {
			return nil, fmt.Errorf("control: building built-in provider %q: %w", spec.Name, err)
		}
		c.builtinSpecs[spec.Name] = spec
		c.providers[spec.Name] = engine
	}
	if _, ok := c.providers[cfg.DefaultProvider]; !ok {
		return nil, fmt.Errorf("control: default provider %q is not among the built-ins", cfg.DefaultProvider)
	}
	if err := c.rebuildCatalogLocked(); err != nil {
		return nil, err
	}

	return c, nil
}

// InitProviders loads the operator-defined provider registry from the repository and
// builds its engines. When the table is empty and seed entries are supplied (the
// legacy matrix.providers file config), they are imported first — a one-time
// migration, after which the database is authoritative and the file block is inert.
// Call it at boot before ResumeEnabled so resumed areas resolve their providers.
func (c *Coordinator) InitProviders(ctx context.Context, seed []beeline.ProviderSpec) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	specs, err := c.providersRepo.ListProviders(ctx)
	if err != nil {
		return err
	}

	if len(specs) == 0 && len(seed) > 0 {
		for i := range seed {
			if reserveErr := c.checkNameFreeLocked(seed[i].Name); reserveErr != nil {
				return fmt.Errorf("control: seeding provider from file config: %w", reserveErr)
			}
			if upErr := c.providersRepo.UpsertProvider(ctx, &seed[i]); upErr != nil {
				return upErr
			}
		}
		specs = seed
	}

	for i := range specs {
		spec := specs[i]
		engine, buildErr := c.buildEngine(&spec)
		if buildErr != nil {
			return fmt.Errorf("control: building provider %q: %w", spec.Name, buildErr)
		}
		c.dbSpecs[spec.Name] = spec
		c.providers[spec.Name] = engine
	}

	return c.rebuildCatalogLocked()
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

	var out beeline.Area
	err := c.locker.WithAreaLock(ctx, id, func(ctx context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()

		area, err := c.repo.Get(ctx, id)
		if err != nil {
			return err
		}

		sample, err := tessellate.SamplePoint(area.GeoJSON)
		if err != nil {
			return err
		}
		layers, err := normalizeAndValidateLayers(in.Layers, strategy, sample)
		if err != nil {
			return err
		}
		targetTTL, lease, sweep, err := c.resolveFreshness(in.TargetTTL, in.LeaseDuration, in.SweepInterval)
		if err != nil {
			return err
		}

		area.Name = in.Name
		area.Layers = layers
		area.WarmStrategy = strategy
		area.RoutingProvider = provider
		area.DemandIdleTTL = in.DemandIdleTTL
		area.TargetTTL = targetTTL
		area.LeaseDuration = lease
		area.SweepInterval = sweep

		out, err = c.persistAndConvergeLocked(ctx, &area)

		return err
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return out, nil
}

// SetGeoJSON replaces an area's canonical geometry from an uploaded polygon. The
// existing layer list is re-validated against the new geometry (the radius floors
// depend on where on the globe the sample cell lands); the layers' cell sets are
// re-derived at seed time. An enabled area is re-converged.
func (c *Coordinator) SetGeoJSON(ctx context.Context, id beeline.AreaID, raw []byte) (beeline.Area, error) {
	var out beeline.Area
	err := c.locker.WithAreaLock(ctx, id, func(ctx context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()

		area, err := c.repo.Get(ctx, id)
		if err != nil {
			return err
		}

		sample, err := tessellate.SamplePoint(raw)
		if err != nil {
			return err
		}
		if _, err = normalizeAndValidateLayers(area.Layers, area.WarmStrategy, sample); err != nil {
			return err
		}
		area.GeoJSON = raw

		out, err = c.persistAndConvergeLocked(ctx, &area)

		return err
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return out, nil
}

// Enable seeds an area's pairs into the shared index and marks it enabled, so the
// refresh pool begins keeping it fresh. Enabling an already-enabled area is a no-op
// re-seed.
func (c *Coordinator) Enable(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	var out beeline.Area
	err := c.locker.WithAreaLock(ctx, id, func(ctx context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()

		area, err := c.repo.Get(ctx, id)
		if err != nil {
			return err
		}

		if err = c.seedLocked(ctx, &area); err != nil {
			return err
		}

		if err = c.repo.SetEnabled(ctx, id, true); err != nil {
			// Roll back the seed so persisted state and the index don't diverge.
			delete(c.enabled, id)
			return errors.Join(err, c.index.Unseed(ctx, id))
		}

		area.Enabled = true
		out = area

		return nil
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return out, nil
}

// Disable removes an area's pairs and cached estimates and marks it disabled. The
// refresh pool stops working it. Disabling an already-disabled area is a no-op.
func (c *Coordinator) Disable(ctx context.Context, id beeline.AreaID) (beeline.Area, error) {
	var out beeline.Area
	err := c.locker.WithAreaLock(ctx, id, func(ctx context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()

		if err := c.unseedLocked(ctx, id); err != nil {
			return err
		}

		if err := c.repo.SetEnabled(ctx, id, false); err != nil {
			return err
		}

		area, err := c.repo.Get(ctx, id)
		if err != nil {
			return err
		}
		out = area

		return nil
	})
	if err != nil {
		return beeline.Area{}, err
	}

	return out, nil
}

// Delete removes an area entirely, first tearing down its working set if enabled.
func (c *Coordinator) Delete(ctx context.Context, id beeline.AreaID) error {
	return c.locker.WithAreaLock(ctx, id, func(ctx context.Context) error {
		c.mu.Lock()
		defer c.mu.Unlock()

		if err := c.unseedLocked(ctx, id); err != nil {
			return err
		}

		return c.repo.Delete(ctx, id)
	})
}

// ResumeEnabled brings every already-enabled area back into service at boot.
// Single-node, the in-memory index is empty after a restart, so every enabled
// area is seeded from scratch (today's behavior). Distributed, the shared index
// usually already holds the working set: an area whose pairs survive is only
// projected (snapshot + freshness contract), preserving its throughput baseline
// across head restarts, and only an area with no tracked pairs is seeded. Heads
// booting simultaneously serialize on the boot-seed lock when the locker
// provides one, so exactly one seeds and the rest observe its work.
func (c *Coordinator) ResumeEnabled(ctx context.Context) error {
	if bootLocker, ok := c.locker.(BootSeedLocker); ok {
		return bootLocker.WithBootSeedLock(ctx, c.resumeEnabled)
	}

	return c.resumeEnabled(ctx)
}

// resumeEnabled is ResumeEnabled's body, run under the boot-seed lock when one
// exists.
func (c *Coordinator) resumeEnabled(ctx context.Context) error {
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

		stats, statsErr := c.index.DebtForArea(ctx, areas[i].ID)
		if statsErr != nil {
			return statsErr
		}
		if stats.WorkingSet > 0 {
			// Another head (or a prior run of this one) already seeded this
			// area into the shared index; just project it.
			if projErr := c.projectAreaLocked(ctx, &areas[i]); projErr != nil {
				return projErr
			}
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

// ProviderNameFor returns the name of the routing provider an enabled area resolves
// through, or the default provider's name for an unknown/zero id. Follower processes
// use it (via the /_work_/claim response) to pick the matching engine from their own
// registry, mirroring what EngineFor resolves in-process.
func (c *Coordinator) ProviderNameFor(id beeline.AreaID) string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	if ea, ok := c.enabled[id]; ok && ea.provider != "" {
		return ea.provider
	}

	return c.defaultProvider
}

// AreaEnabled reports whether the area is currently enabled. The work-submit path
// uses it to drop results for areas disabled after the claim was handed out, so a
// late submit cannot resurrect estimates the disable already purged from the store.
func (c *Coordinator) AreaEnabled(id beeline.AreaID) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()

	_, ok := c.enabled[id]

	return ok
}

// ErrProviderInUse marks a provider deletion rejected because an area still names
// it; ErrProviderReserved marks a mutation of a built-in name. Both map to 400 at
// the HTTP layer.
var (
	ErrProviderInUse    = errors.New("control: provider is referenced by an area")
	ErrProviderReserved = errors.New("control: provider name is reserved for a built-in engine")
)

// ErrProviderNotFound marks a delete of a provider that does not exist. It wraps
// beeline.ErrNotFound so the HTTP layer's 404 mapping applies via errors.Is —
// this used to rely on the message ending in "not found", which made the status
// code a property of the wording.
var ErrProviderNotFound = fmt.Errorf("control: provider %w", beeline.ErrNotFound)

// ProviderInfo is one registry entry for the control-plane surface: the full spec
// plus whether it is a synthesized built-in (immutable through the API).
type ProviderInfo struct {
	Spec    beeline.ProviderSpec
	Builtin bool
}

// ListProviderInfos returns every registered provider — the default first, then the
// rest ascending by name — so the operator console can render the registry and its
// picker from live state.
func (c *Coordinator) ListProviderInfos() []ProviderInfo {
	c.mu.RLock()
	defer c.mu.RUnlock()

	names := make([]string, 0, len(c.providers))
	for name := range c.providers {
		if name != c.defaultProvider {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	names = append([]string{c.defaultProvider}, names...)

	infos := make([]ProviderInfo, 0, len(names))
	for _, name := range names {
		if spec, ok := c.builtinSpecs[name]; ok {
			infos = append(infos, ProviderInfo{Spec: spec, Builtin: true})
			continue
		}
		infos = append(infos, ProviderInfo{Spec: c.dbSpecs[name]})
	}

	return infos
}

// PutProvider creates or replaces one operator-defined provider: validate, build the
// engine (so a broken spec is rejected before anything persists), persist, register,
// and re-point any enabled area already routing through that name at the new engine —
// updating a provider (say, an OSRM base URL) takes effect without re-enabling areas.
// The catalog hash changes with it, so followers converge on their next claim.
func (c *Coordinator) PutProvider(ctx context.Context, spec *beeline.ProviderSpec) (ProviderInfo, error) {
	if err := spec.Validate(); err != nil {
		return ProviderInfo{}, fmt.Errorf("control: %w", err)
	}
	if spec.Type == beeline.ProviderTypeLatentHaversine {
		return ProviderInfo{}, fmt.Errorf("control: provider type %q is synthesized from engine-latency config and cannot be created", spec.Type)
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if err := c.checkNameFreeLocked(spec.Name); err != nil {
		return ProviderInfo{}, err
	}

	engine, err := c.buildEngine(spec)
	if err != nil {
		return ProviderInfo{}, fmt.Errorf("control: building provider %q: %w", spec.Name, err)
	}
	if err = c.providersRepo.UpsertProvider(ctx, spec); err != nil {
		return ProviderInfo{}, err
	}

	c.dbSpecs[spec.Name] = *spec
	c.providers[spec.Name] = engine
	for _, ea := range c.enabled {
		if ea.provider == spec.Name {
			ea.engine = engine
		}
	}

	if err = c.rebuildCatalogLocked(); err != nil {
		return ProviderInfo{}, err
	}

	return ProviderInfo{Spec: *spec}, nil
}

// DeleteProvider removes one operator-defined provider. Built-ins are reserved, and
// a provider still named by any area — enabled or not, since a disabled area may be
// re-enabled later — cannot be removed until those areas are repointed.
func (c *Coordinator) DeleteProvider(ctx context.Context, name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, ok := c.builtinSpecs[name]; ok {
		return fmt.Errorf("%w: %q", ErrProviderReserved, name)
	}
	if _, ok := c.dbSpecs[name]; !ok {
		return fmt.Errorf("%w: %q", ErrProviderNotFound, name)
	}

	areas, err := c.repo.List(ctx)
	if err != nil {
		return err
	}
	for i := range areas {
		if areas[i].RoutingProvider == name {
			return fmt.Errorf("%w: area %d (%s) routes through %q", ErrProviderInUse, areas[i].ID, areas[i].Name, name)
		}
	}

	if err = c.providersRepo.DeleteProvider(ctx, name); err != nil {
		return err
	}
	delete(c.dbSpecs, name)
	delete(c.providers, name)

	return c.rebuildCatalogLocked()
}

// Catalog returns the current provider catalog — every spec plus the profile speed
// map, stamped with its content hash. It is the /_work_/providers payload.
func (c *Coordinator) Catalog() beeline.ProviderCatalog {
	c.mu.RLock()
	defer c.mu.RUnlock()

	out := c.catalog
	out.Providers = slices.Clone(c.catalog.Providers)

	return out
}

// ProvidersHash returns the catalog's content hash — the change signal stamped into
// every /_work_/claim response.
func (c *Coordinator) ProvidersHash() string {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.catalog.Hash
}

// checkNameFreeLocked rejects operator use of a built-in provider name. Callers
// hold c.mu.
func (c *Coordinator) checkNameFreeLocked(name string) error {
	if _, ok := c.builtinSpecs[name]; ok {
		return fmt.Errorf("%w: %q", ErrProviderReserved, name)
	}

	return nil
}

// rebuildCatalogLocked recomputes the published catalog and its content hash from
// the current registry: built-ins first (default engine leading), then the
// operator-defined specs ascending by name. Callers hold c.mu (or own the
// still-unshared Coordinator during New).
func (c *Coordinator) rebuildCatalogLocked() error {
	specs := make([]beeline.ProviderSpec, 0, len(c.builtinSpecs)+len(c.dbSpecs))
	if spec, ok := c.builtinSpecs[c.defaultProvider]; ok {
		specs = append(specs, spec)
	}
	builtinNames := make([]string, 0, len(c.builtinSpecs))
	for name := range c.builtinSpecs {
		if name != c.defaultProvider {
			builtinNames = append(builtinNames, name)
		}
	}
	slices.Sort(builtinNames)
	for _, name := range builtinNames {
		specs = append(specs, c.builtinSpecs[name])
	}

	dbNames := make([]string, 0, len(c.dbSpecs))
	for name := range c.dbSpecs {
		dbNames = append(dbNames, name)
	}
	slices.Sort(dbNames)
	for _, name := range dbNames {
		specs = append(specs, c.dbSpecs[name])
	}

	catalog := beeline.ProviderCatalog{Speeds: c.speeds, Providers: specs}
	hash, err := catalog.ComputeHash()
	if err != nil {
		return fmt.Errorf("control: %w", err)
	}
	catalog.Hash = hash
	c.catalog = catalog

	return nil
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

// validateProvider rejects a provider name that is not in the registry, so an area
// cannot reference an engine that does not exist. It takes the read lock: the
// registry is mutable and callers (Create/Update) run before the area lock is held.
func (c *Coordinator) validateProvider(name string) error {
	c.mu.RLock()
	defer c.mu.RUnlock()

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

// ErrInvalidInvalidation marks an invalidation request the coordinator rejected: an
// area that is not enabled (its pairs are not in the index at all), a resolution the
// area has no precision layer for, or an unknown profile. It maps to 400 at the HTTP
// edge. Naming a layer that does not exist is an error rather than a silent
// zero-row no-op precisely because the operator's next move depends on the answer.
var ErrInvalidInvalidation = errors.New("control: invalid invalidation request")

// InvalidateInput scopes an invalidation within one area. Both fields are
// optional and narrow the selection: the zero value invalidates every cached pair
// of the area, across all its layers and profiles.
type InvalidateInput struct {
	// Resolution names one of the area's precision layers. Nil means every layer.
	// A resolution the area has no layer for is rejected.
	Resolution *int
	// Profile names one routing profile. Empty means every profile.
	Profile beeline.Profile
}

// Invalidate re-enqueues an enabled area's cached pairs for refresh, optionally
// scoped to one precision layer and/or profile, and reports how many pairs it
// moved. It is the operator's answer to "the routing data behind this layer
// changed": the layer's pairs go to the front of the refresh queue (ahead of every
// merely-stale pair, behind live demand bumps) and the pool recomputes them at the
// area's normal throughput.
//
// It deliberately does not touch the hot store, so invalidating a million-pair layer
// spends refresh throughput, not read latency, and /_ops_/freshness shows the debt
// spike and burn-down as it happens. The tradeoff is that the invalidation is
// invisible on the read path: reads keep serving the pre-invalidation value for as
// long as the burn-down takes, and report it as fresh, because staleness there is
// measured from the stored estimate's own age against the area TTL. A layer whose
// cached values must not be served again wants disable/enable, which drops the
// estimates outright.
//
// Only enabled areas can be invalidated: a disabled area has no pairs in the index
// to re-enqueue, and enabling it seeds them as never-computed anyway.
func (c *Coordinator) Invalidate(ctx context.Context, id beeline.AreaID, in *InvalidateInput) (int, error) {
	if in == nil {
		in = &InvalidateInput{}
	}

	c.mu.RLock()
	ea, ok := c.enabled[id]
	if !ok {
		c.mu.RUnlock()

		return 0, fmt.Errorf("%w: area %d is not enabled", ErrInvalidInvalidation, id)
	}
	resolutions := make([]int, 0, len(ea.layers))
	for i := range ea.layers {
		resolutions = append(resolutions, ea.layers[i].resolution)
	}
	c.mu.RUnlock()

	if in.Resolution != nil && !slices.Contains(resolutions, *in.Resolution) {
		return 0, fmt.Errorf("%w: area %d has no precision layer at resolution %d (layers: %v)",
			ErrInvalidInvalidation, id, *in.Resolution, resolutions)
	}
	if in.Profile != "" && !slices.Contains(c.profiles, in.Profile) {
		return 0, fmt.Errorf("%w: unknown profile %q", ErrInvalidInvalidation, in.Profile)
	}

	// Outside the lock, like WarmPairs: the index serializes on its own, and a
	// concurrent disable at worst re-enqueues pairs its Unseed then removes.
	return c.index.Invalidate(ctx, beeline.Selector{
		Area:    id,
		Res:     in.Resolution,
		Profile: in.Profile,
	})
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
	ea, layerCells, err := c.buildProjectionLocked(area)
	if err != nil {
		return err
	}

	var pairs []beeline.PairKey
	for i := range area.Layers {
		eager, eagerErr := c.eagerSeedPairs(area, area.Layers[i], layerCells[i])
		if eagerErr != nil {
			return eagerErr
		}
		pairs = append(pairs, eager...)
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

	c.enabled[area.ID] = ea

	return nil
}

// projectAreaLocked rebuilds one area's in-process routing snapshot without
// touching the shared index's pair set: polyfill, engine resolution, and the
// freshness-contract registration (idempotent in the index). It is the
// non-mutating-head half of seedLocked — a head converging on another head's
// Enable projects the area so Locate/EngineFor/AreaEnabled answer for it, while
// the head that handled the Enable already seeded the shared index. Callers
// hold c.mu.
func (c *Coordinator) projectAreaLocked(ctx context.Context, area *beeline.Area) error {
	ea, _, err := c.buildProjectionLocked(area)
	if err != nil {
		return err
	}
	if err = c.index.SetAreaFreshness(ctx, area.ID, area.TargetTTL, area.LeaseDuration); err != nil {
		return err
	}

	c.enabled[area.ID] = ea

	return nil
}

// buildProjectionLocked polyfills each of the area's layers from its GeoJSON
// and assembles the routing snapshot, returning the per-layer cell slices for
// eager-pair derivation. It mutates nothing. A provider name missing from the
// registry (a cross-head resync racing a provider delete) falls back to the
// default engine rather than leaving the area unroutable. Callers hold c.mu.
func (c *Coordinator) buildProjectionLocked(area *beeline.Area) (*enabledArea, [][]beeline.H3Cell, error) {
	layerCells := make([][]beeline.H3Cell, 0, len(area.Layers))
	layers := make([]enabledLayer, 0, len(area.Layers))
	for i := range area.Layers {
		layer := area.Layers[i]
		cells, err := tessellate.CellsFromGeoJSON(area.GeoJSON, layer.Resolution)
		if err != nil {
			return nil, nil, err
		}
		layerCells = append(layerCells, cells)

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

	provider := c.normalizeProvider(area.RoutingProvider)
	engine, ok := c.providers[provider]
	if !ok {
		engine = c.providers[c.defaultProvider]
	}

	return &enabledArea{
		layers:        layers,
		engine:        engine,
		provider:      provider,
		demandIdleTTL: area.DemandIdleTTL,
		targetTTL:     area.TargetTTL,
		sweepInterval: area.SweepInterval,
	}, layerCells, nil
}

// ResyncAreas converges this head's routing snapshot with the shared area
// registry: areas enabled elsewhere are projected in, areas disabled or
// deleted elsewhere are dropped. It deliberately never seeds or unseeds the
// shared index/store — the head that handled the mutation already did, and
// doing it again from every head would reset baselines and re-purge stores.
// The config watcher calls it when the areas generation moves.
func (c *Coordinator) ResyncAreas(ctx context.Context) error {
	areas, err := c.repo.List(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	enabled := make(map[beeline.AreaID]bool, len(areas))
	for i := range areas {
		if !areas[i].Enabled {
			continue
		}
		enabled[areas[i].ID] = true
		if projErr := c.projectAreaLocked(ctx, &areas[i]); projErr != nil {
			return projErr
		}
	}

	for id := range c.enabled {
		if !enabled[id] {
			delete(c.enabled, id)
			delete(c.lastSwept, id)
		}
	}

	return nil
}

// ResyncProviders converges this head's provider registry with the shared one:
// operator specs are re-listed, engines rebuilt, enabled areas re-pointed, and
// the catalog hash recomputed — the same effect PutProvider/DeleteProvider have
// on the head that handled the call. The config watcher calls it when the
// providers generation moves.
func (c *Coordinator) ResyncProviders(ctx context.Context) error {
	specs, err := c.providersRepo.ListProviders(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	providers := make(map[string]beeline.RoutingEngine, len(c.builtinSpecs)+len(specs))
	for name := range c.builtinSpecs {
		providers[name] = c.providers[name]
	}

	dbSpecs := make(map[string]beeline.ProviderSpec, len(specs))
	for i := range specs {
		engine, buildErr := c.buildEngine(&specs[i])
		if buildErr != nil {
			return fmt.Errorf("control: rebuilding provider %q: %w", specs[i].Name, buildErr)
		}
		dbSpecs[specs[i].Name] = specs[i]
		providers[specs[i].Name] = engine
	}

	c.dbSpecs = dbSpecs
	c.providers = providers
	for _, ea := range c.enabled {
		if engine, ok := c.providers[ea.provider]; ok {
			ea.engine = engine
		} else {
			ea.engine = c.providers[c.defaultProvider]
		}
	}

	return c.rebuildCatalogLocked()
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
