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

// Area is a configured service area: a named set of H3 cells that the refresh loop
// keeps fresh once the area is enabled. It is the persistent, operator-managed unit
// that replaced the prototype's single config-file area — areas live in the store,
// are disabled by default, and only enter the working set when enabled.
//
// The canonical geometry is the explicit Cells set. An area may be seeded from an
// uploaded GeoJSON polygon (polyfilled to cells) and then refined by adding or
// removing individual cells, so after the first manual edit only Cells describes the
// truth; GeoJSON is retained as provenance. Resolution fixes the H3 resolution of
// every cell; MaxRadiusMeters is the per-origin travel-radius bound (§7), expressed
// in meters, used to build the directed pair set from Cells. MaxRadiusMeters == 0 is
// the full-mesh sentinel: every in-area cell is paired with every other.
//
// WarmStrategy and CoreRadiusMeters govern how much of that bound is kept fresh
// eagerly. eager pins the whole bound; lazy pins nothing (demand only); hybrid pins
// everything within CoreRadiusMeters and demand-fills the tail out to MaxRadiusMeters.
type Area struct {
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Name             string
	WarmStrategy     WarmStrategy
	GeoJSON          []byte // raw uploaded polygon for provenance; nil when built purely from manual cells
	Cells            []H3Cell
	ID               AreaID
	Resolution       int
	MaxRadiusMeters  float64
	CoreRadiusMeters float64
	Enabled          bool
}

// RoutedArea is the result of resolving a query coordinate to the enabled service
// area that contains it: the area's id, the H3 resolution to key lookups at, and the
// area's outer travel bound. The read path uses it to attribute cache hits and
// demand-fills to the right area partition and to decide whether a demand-fill falls
// within the bound (and may be cached) or beyond it (answered but never cached). See
// the AreaRouter seam in package query. MaxRadiusMeters == 0 means unbounded (full
// mesh): every in-area query is cacheable.
type RoutedArea struct {
	ID              AreaID
	Resolution      int
	MaxRadiusMeters float64
}
