package memory

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"sort"
	"sync"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/primitives-go/v2/clock"
)

// ErrNotFound is returned when an area id does not exist. Like the persistent
// stores' equivalents it wraps beeline.ErrNotFound, so the HTTP layer's 404
// mapping works identically against the fake.
var ErrNotFound = fmt.Errorf("memory: area %w", beeline.ErrNotFound)

// Repository is an in-memory control-plane store: areas with their precision
// layers, plus the operator-defined provider registry. It implements
// control.AreasRepository and control.ProvidersRepository.
//
// It is a test double, not a deployable backend — nothing persists past the
// process. Its reason to exist is that the packages above the control plane
// (control, httpapi) need a repository to build fixtures on without a database.
// It is pinned to the same behavior as the real stores by
// internal/control/repositorytest, which is the only thing that makes a
// hand-written fake trustworthy: every deviation it might drift into is a
// deviation the suite fails on.
//
// The zero value is not usable; call NewRepository.
type Repository struct {
	areas     map[beeline.AreaID]beeline.Area
	providers map[string]beeline.ProviderSpec
	clock     clock.Clock
	nextID    beeline.AreaID
	mu        sync.RWMutex
}

// NewRepository returns an empty repository. A nil clock means the wall clock.
func NewRepository(clk clock.Clock) *Repository {
	if clk == nil {
		clk = clock.NewClock()
	}

	return &Repository{
		areas:     make(map[beeline.AreaID]beeline.Area),
		providers: make(map[string]beeline.ProviderSpec),
		clock:     clk,
		nextID:    1,
	}
}

// Create stores an area under a freshly assigned id and returns it. The caller
// controls the Enabled flag, matching the persistent stores (the control plane
// creates areas disabled).
func (r *Repository) Create(_ context.Context, a *beeline.Area) (beeline.Area, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	now := r.clock.Now().UTC()

	stored := cloneArea(a)
	stored.ID = r.nextID
	stored.CreatedAt = now
	stored.UpdatedAt = now
	sortLayers(stored.Layers)

	r.nextID++
	r.areas[stored.ID] = stored

	return cloneArea(&stored), nil
}

// Get returns one area with its layers hydrated finest-first, or ErrNotFound.
func (r *Repository) Get(_ context.Context, id beeline.AreaID) (beeline.Area, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	stored, ok := r.areas[id]
	if !ok {
		return beeline.Area{}, ErrNotFound
	}

	return cloneArea(&stored), nil
}

// List returns every area ordered by id, layers hydrated.
func (r *Repository) List(_ context.Context) ([]beeline.Area, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ids := make([]beeline.AreaID, 0, len(r.areas))
	for id := range r.areas {
		ids = append(ids, id)
	}
	slices.Sort(ids)

	out := make([]beeline.Area, 0, len(ids))
	for _, id := range ids {
		stored := r.areas[id]
		out = append(out, cloneArea(&stored))
	}

	return out, nil
}

// Update replaces an area's mutable fields and its whole layer set. It
// deliberately does not touch Enabled — that is SetEnabled's business, and the
// persistent stores' UPDATE statements leave the column alone, so an Update
// carrying a stale flag must not re-disable a live area.
//
// Updating an id that does not exist fails. The SQL stores get there via a
// foreign-key violation — Update replaces the layer set, and layer rows for a
// missing area have nothing to reference — so returning nil here would make the
// fake accept a write the real stores reject.
func (r *Repository) Update(_ context.Context, a *beeline.Area) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.areas[a.ID]
	if !ok {
		return fmt.Errorf("memory: updating area %d: %w", a.ID, ErrNotFound)
	}

	updated := cloneArea(a)
	updated.ID = stored.ID
	updated.CreatedAt = stored.CreatedAt
	updated.Enabled = stored.Enabled
	updated.UpdatedAt = r.clock.Now().UTC()
	sortLayers(updated.Layers)

	r.areas[a.ID] = updated

	return nil
}

// Delete removes an area; its layers go with it.
func (r *Repository) Delete(_ context.Context, id beeline.AreaID) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.areas, id)

	return nil
}

// SetEnabled flips an area's enabled flag and bumps updated_at.
func (r *Repository) SetEnabled(_ context.Context, id beeline.AreaID, enabled bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	stored, ok := r.areas[id]
	if !ok {
		return nil
	}

	stored.Enabled = enabled
	stored.UpdatedAt = r.clock.Now().UTC()
	r.areas[id] = stored

	return nil
}

// ListProviders returns the operator-defined provider specs ordered by name.
// Built-ins are synthesized by the control plane and never stored here.
func (r *Repository) ListProviders(_ context.Context) ([]beeline.ProviderSpec, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	names := make([]string, 0, len(r.providers))
	for name := range r.providers {
		names = append(names, name)
	}
	slices.Sort(names)

	out := make([]beeline.ProviderSpec, 0, len(names))
	for _, name := range names {
		spec := r.providers[name]
		out = append(out, cloneSpec(&spec))
	}

	return out, nil
}

// UpsertProvider inserts a spec or replaces the one under the same name. The
// replacement is wholesale, not a field-wise merge.
func (r *Repository) UpsertProvider(_ context.Context, spec *beeline.ProviderSpec) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.providers[spec.Name] = cloneSpec(spec)

	return nil
}

// DeleteProvider removes a spec by name; an absent name is a no-op. Whether a
// missing provider is an error is the control plane's call, not the store's.
func (r *Repository) DeleteProvider(_ context.Context, name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.providers, name)

	return nil
}

// cloneArea deep-copies an area. Every reference field has to be copied on the
// way in and on the way out: a real store serializes through a database, so a
// caller can neither mutate stored state after handing it over nor by writing to
// something it read back. A fake that shared its slices and maps would quietly
// license a bug class that only appears against Postgres.
func cloneArea(a *beeline.Area) beeline.Area {
	out := *a
	out.GeoJSON = slices.Clone(a.GeoJSON)
	out.Layers = slices.Clone(a.Layers)

	return out
}

// cloneSpec deep-copies a provider spec, including its profile map.
func cloneSpec(spec *beeline.ProviderSpec) beeline.ProviderSpec {
	out := *spec
	if spec.Profiles != nil {
		out.Profiles = maps.Clone(spec.Profiles)
	}

	return out
}

// sortLayers puts layers finest-first — highest H3 resolution first — the
// ordering invariant Area.Finest and the read path depend on, and the one the
// persistent stores get from ORDER BY resolution DESC.
func sortLayers(layers []beeline.Layer) {
	sort.SliceStable(layers, func(i, j int) bool {
		return layers[i].Resolution > layers[j].Resolution
	})
}
