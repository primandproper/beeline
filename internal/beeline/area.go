package beeline

import "time"

// WarmStrategy selects how much of an area's bounded pair set is kept fresh eagerly
// versus filled on demand from observed queries (§ warm-set design). It is stored as
// a plain string (SQLite TEXT / JSON) so a typed constant round-trips unchanged.
type WarmStrategy string

const (
	// WarmEager pins the entire MaxRadiusMeters bound fresh from enable time. This is
	// the original behavior and the default, so existing areas are unaffected.
	WarmEager WarmStrategy = "eager"
	// WarmLazy seeds nothing up front; the working set grows purely from demand-filled
	// queries within the bound.
	WarmLazy WarmStrategy = "lazy"
	// WarmHybrid pins the dense near field (within CoreRadiusMeters) and fills the
	// sparse tail out to MaxRadiusMeters on demand.
	WarmHybrid WarmStrategy = "hybrid"
)

// Valid reports whether w is one of the known strategies.
func (w WarmStrategy) Valid() bool {
	switch w {
	case WarmEager, WarmLazy, WarmHybrid:
		return true
	default:
		return false
	}
}

// Layer is one precision level of a service area, in the DoorDash
// fast-travel-estimates sense: the area's geometry is polyfilled to H3 cells at
// Resolution and that cell set gets its own precomputed, cached pair set,
// independent of the area's other layers (PairKey.Res keeps them apart in the
// shared store/index).
//
// MinDistanceMeters records the trip distance from which this layer is meant to
// serve — future distance-based layer selection in the read path; today it is
// stored and reported but never consulted (reads always use the finest layer).
//
// MaxRadiusMeters is this layer's per-origin travel-radius bound (§7), used to
// build the directed pair set from the layer's cells; 0 is the full-mesh
// sentinel: every in-layer cell is paired with every other. CoreRadiusMeters is
// the near field the hybrid warm strategy pins eagerly within that bound.
type Layer struct {
	Resolution        int
	MinDistanceMeters float64
	MaxRadiusMeters   float64
	CoreRadiusMeters  float64
}

// Area is a configured service area: a GeoJSON polygon plus the precision layers
// it is precomputed at. It is the persistent, operator-managed unit — areas live
// in the store, are disabled by default, and only enter the working set when
// enabled.
//
// The canonical geometry is GeoJSON, required at creation. Cells are derived
// data: each layer's cell set is re-polyfilled from the polygon at that layer's
// resolution whenever the area is seeded (enable, boot resume, geometry change),
// so nothing cell-shaped is persisted. Layers is ordered finest→coarsest
// (descending resolution); the finest layer keys the read path and defines
// containment.
//
// WarmStrategy governs how much of each layer's bound is kept fresh eagerly.
// eager pins the whole bound; lazy pins nothing (demand only); hybrid pins
// everything within the layer's CoreRadiusMeters and demand-fills the tail out
// to its MaxRadiusMeters.
//
// DemandIdleTTL is how long a demand-filled (unpinned) pair survives without being
// queried before the decay sweep evicts it; 0 disables decay (demand pairs live until
// the area is disabled). Eager-core pairs are never evicted regardless.
//
// TargetTTL, LeaseDuration, and SweepInterval are this area's freshness contract,
// configured per area rather than globally: TargetTTL is the age at which a pair is
// stale (and drives the required refresh throughput); LeaseDuration is the visibility
// timeout a worker holds on a claimed pair; SweepInterval is the cadence of this area's
// demand-decay janitor. New areas inherit the global config values as defaults.
//
// RoutingProvider names the engine this area is served by, referencing an entry in
// the globally configured provider registry (§ per-area providers). The empty string
// means the built-in default (haversine), so areas created before providers existed
// keep routing through the in-process engine unchanged.
type Area struct {
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Name            string
	WarmStrategy    WarmStrategy
	RoutingProvider string
	GeoJSON         []byte
	Layers          []Layer
	DemandIdleTTL   time.Duration
	TargetTTL       time.Duration
	LeaseDuration   time.Duration
	SweepInterval   time.Duration
	ID              AreaID
	Enabled         bool
}

// Finest is the area's highest-resolution layer — Layers[0] under the
// finest→coarsest ordering invariant. It keys the read path and defines area
// containment. The zero Layer is returned for an (invalid) layerless area.
func (a *Area) Finest() Layer {
	if len(a.Layers) == 0 {
		return Layer{}
	}

	return a.Layers[0]
}

// RoutedLayer is one precision layer of a routed area as the read path sees it:
// the resolution to key lookups at, the travel bound that decides whether a
// demand-fill may be cached (0 = unbounded full mesh), and the future selection
// threshold. It mirrors Layer minus the warm-only CoreRadiusMeters.
type RoutedLayer struct {
	Resolution        int
	MinDistanceMeters float64
	MaxRadiusMeters   float64
}

// RoutedArea is the result of resolving a query coordinate to the enabled service
// area that contains it: the area's id and its layers, finest→coarsest. The read
// path uses it to attribute cache hits and demand-fills to the right area
// partition and to decide whether a demand-fill falls within the bound (and may
// be cached) or beyond it (answered but never cached). See the AreaRouter seam in
// package query. Today reads use only ReadLayer(); the full list is carried so
// distance-based layer fallthrough can land without changing this seam.
type RoutedArea struct {
	Layers    []RoutedLayer
	TargetTTL time.Duration
	ID        AreaID
}

// ReadLayer is the layer the read path keys lookups at: the finest one
// (Layers[0]). Distance-based selection across the rest of the list is
// deliberately not implemented yet. The zero RoutedLayer is returned for an
// (invalid) layerless routed area.
func (r RoutedArea) ReadLayer() RoutedLayer {
	if len(r.Layers) == 0 {
		return RoutedLayer{}
	}

	return r.Layers[0]
}
