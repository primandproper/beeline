// Package control is the runtime control plane for the service area. The prototype
// tessellates and seeds once at startup; this package makes that operation
// repeatable at runtime so an operator (via the web UI, §frontend) can redraw the
// boundary or change the tessellation scheme and watch the cache reload.
//
// A Coordinator owns the current Area and the three mutable seams it drives on an
// Apply: it re-tessellates the pair set, clears the hot store, replaces the
// freshness index's working set, and repoints the read path at the new resolution.
// The seams are narrow interfaces so the coordinator stays testable without the
// concrete in-memory store/index.
package control

import (
	"context"
	"fmt"
	"sync"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/tessellate"
)

// Reseeder is the freshness index seam: replace the working set atomically and
// report per-cell freshness for the progress map.
type Reseeder interface {
	Reseed(ctx context.Context, keys []beeline.PairKey) error
	CellStates(ctx context.Context) ([]beeline.CellState, error)
}

// Cache is the hot store seam: drop the estimates that belonged to the old area.
type Cache interface {
	Reset()
}

// Resolver is the read-path seam: repoint lookups at the new H3 resolution.
type Resolver interface {
	SetResolution(resolution int)
}

// Area is the mutable tessellation spec: the service-area center, its H3
// resolution, the rings around the center that define the area, and the
// travel-radius bound materialized per origin (§7). It is the JSON body of the
// /_config_/area endpoints.
type Area struct {
	Lat         float64 `json:"lat"`
	Lng         float64 `json:"lng"`
	Resolution  int     `json:"resolution"`
	AreaRings   int     `json:"areaRings"`
	RadiusRings int     `json:"radiusRings"`
}

// Summary is the result of an Apply (and of reading the current area): the area
// echoed back, plus the derived pair-set size and the profiles in force.
//
// MaskResolution is the resolution the road mask is built at, or nil when no mask
// is loaded. The console compares it to the selected resolution to warn when a
// finer zoom prunes only approximately (see tessellate.Mask.Allows).
type Summary struct {
	MaskResolution *int     `json:"maskResolution,omitempty"`
	Profiles       []string `json:"profiles"`
	Area
	Cells int `json:"cells"`
	Pairs int `json:"pairs"`
}

// Coordinator serializes runtime re-tessellation over the mutable seams.
type Coordinator struct {
	index    Reseeder
	store    Cache
	query    Resolver
	mask     *tessellate.Mask
	profiles []beeline.Profile
	mu       sync.RWMutex
	area     Area
	cells    int
	pairs    int
}

// New builds a Coordinator over the given seams and profile set. mask is the
// optional road-mask (§7) applied on every re-tessellation — at any resolution,
// via the H3 hierarchy (see tessellate.Mask.Allows); pass nil to disable
// road-aware pruning. The area is not applied yet; call Apply (typically with the
// config's initial area) to seed.
func New(index Reseeder, store Cache, query Resolver, profiles []beeline.Profile, mask *tessellate.Mask) *Coordinator {
	return &Coordinator{
		index:    index,
		store:    store,
		query:    query,
		profiles: profiles,
		mask:     mask,
	}
}

// validate rejects an area the tessellator or config layer would refuse, so the
// caller gets a 400 rather than a confusing downstream error.
func (c *Coordinator) validate(a Area) error {
	if a.Resolution < 0 || a.Resolution > 15 {
		return fmt.Errorf("resolution %d out of range [0,15]", a.Resolution)
	}
	if a.AreaRings < 0 {
		return fmt.Errorf("area rings %d must be >= 0", a.AreaRings)
	}
	if a.RadiusRings < 1 {
		return fmt.Errorf("radius rings %d must be >= 1", a.RadiusRings)
	}
	if a.Lat < -90 || a.Lat > 90 {
		return fmt.Errorf("latitude %v out of range [-90,90]", a.Lat)
	}
	if a.Lng < -180 || a.Lng > 180 {
		return fmt.Errorf("longitude %v out of range [-180,180]", a.Lng)
	}

	return nil
}

// Apply re-tessellates for the new area and swaps it in: clear the store, replace
// the index working set, and repoint the read path. Concurrent Applys serialize.
// On a validation or tessellation error the current area is left untouched.
func (c *Coordinator) Apply(ctx context.Context, a Area) (Summary, error) {
	if err := c.validate(a); err != nil {
		return Summary{}, err
	}

	seed, err := tessellate.Seed(tessellate.Area{
		Center:      beeline.LatLng{Lat: a.Lat, Lng: a.Lng},
		Resolution:  a.Resolution,
		AreaRings:   a.AreaRings,
		RadiusRings: a.RadiusRings,
	}, c.profiles, c.mask)
	if err != nil {
		return Summary{}, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Clear the store before swapping the index so a worker that computed against
	// the previous area can't leave a fresh-looking entry under the new working set.
	c.store.Reset()

	if err = c.index.Reseed(ctx, seed.Pairs); err != nil {
		return Summary{}, err
	}

	c.query.SetResolution(a.Resolution)

	c.area = a
	c.cells = len(seed.Cells)
	c.pairs = len(seed.Pairs)

	return c.summaryLocked(), nil
}

// Current returns the area in force and its derived sizes.
func (c *Coordinator) Current() Summary {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.summaryLocked()
}

// CellStates proxies the index's per-cell rollup for the progress map.
func (c *Coordinator) CellStates(ctx context.Context) ([]beeline.CellState, error) {
	return c.index.CellStates(ctx)
}

// summaryLocked builds a Summary from current state. Callers hold c.mu.
func (c *Coordinator) summaryLocked() Summary {
	profiles := make([]string, len(c.profiles))
	for i := range c.profiles {
		profiles[i] = string(c.profiles[i])
	}

	var maskResolution *int
	if c.mask != nil {
		res := c.mask.Resolution
		maskResolution = &res
	}

	return Summary{
		Area:           c.area,
		Profiles:       profiles,
		MaskResolution: maskResolution,
		Cells:          c.cells,
		Pairs:          c.pairs,
	}
}
