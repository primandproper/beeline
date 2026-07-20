package beeline

import "time"

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
type Area struct {
	CreatedAt       time.Time
	UpdatedAt       time.Time
	Name            string
	GeoJSON         []byte // raw uploaded polygon for provenance; nil when built purely from manual cells
	Cells           []H3Cell
	ID              AreaID
	Resolution      int
	MaxRadiusMeters float64
	Enabled         bool
}

// RoutedArea is the result of resolving a query coordinate to the enabled service
// area that contains it: the area's id and the H3 resolution to key lookups at. The
// read path uses it to attribute cache hits and demand-fills to the right area
// partition (see the AreaRouter seam in package query).
type RoutedArea struct {
	ID         AreaID
	Resolution int
}
