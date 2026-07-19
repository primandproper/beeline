// Package tessellate turns a service area into the set of origin→destination pairs
// the freshness index must keep fresh. It stands in for the design's ingestion path
// (§7: polyfill → road-mask → seed pair set). To stay dependency-light the prototype
// seeds the area from a center cell and a ring count rather than a GeoJSON polygon;
// swapping in h3.PolygonToCells over real geometry is the natural extension.
package tessellate

import (
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/uber/h3-go/v4"
)

// Area describes the service area and the pruning that keeps the pair set from
// being quadratic (§7). The area is every cell within AreaRings of the center; for
// each origin, destinations are only those within RadiusRings (the travel-radius
// bound) that are also inside the area.
type Area struct {
	Center      beeline.LatLng
	Resolution  int
	AreaRings   int
	RadiusRings int
}

// Result is the output of tessellation: the cells covering the area and the
// directed pair set (one entry per origin × reachable-destination × profile).
type Result struct {
	Cells []beeline.H3Cell
	Pairs []beeline.PairKey
}

// Seed builds the pair set for the area across the given profiles. Origin-centric:
// each origin contributes a dense ring of nearby destinations (§6), which is the
// shape the refresh worker packs into a single 1×K table request.
//
// mask is the optional road-mask (§7): when non-nil, cells the operator's road
// layer does not cover are dropped before pairs are built, so roadless cells
// (ocean, open water) never enter the working set. The mask answers at any area
// resolution through the H3 hierarchy (see Mask.Allows), so a runtime resolution
// change still prunes; a nil mask keeps every cell.
func Seed(area Area, profiles []beeline.Profile, mask *Mask) (Result, error) {
	if area.Resolution < 0 || area.Resolution > 15 {
		return Result{}, fmt.Errorf("tessellate: resolution %d out of range [0,15]", area.Resolution)
	}
	if len(profiles) == 0 {
		return Result{}, fmt.Errorf("tessellate: at least one profile required")
	}

	center, err := h3.LatLngToCell(h3.NewLatLng(area.Center.Lat, area.Center.Lng), area.Resolution)
	if err != nil {
		return Result{}, fmt.Errorf("tessellate: locating center cell: %w", err)
	}

	cells, err := h3.GridDisk(center, area.AreaRings)
	if err != nil {
		return Result{}, fmt.Errorf("tessellate: covering area: %w", err)
	}

	// Road-aware pruning (§7): keep only cells the mask says have roads. The mask
	// answers at the area's resolution via the H3 hierarchy, so this holds even when
	// the console re-tessellates at a resolution other than the mask's.
	if mask != nil {
		kept := make([]beeline.H3Cell, 0, len(cells))
		for _, c := range cells {
			if mask.Allows(c, area.Resolution) {
				kept = append(kept, c)
			}
		}
		cells = kept
	}

	inArea := make(map[beeline.H3Cell]struct{}, len(cells))
	for _, c := range cells {
		inArea[c] = struct{}{}
	}

	pairs := make([]beeline.PairKey, 0, len(cells)*7*len(profiles))
	for _, origin := range cells {
		neighbors, diskErr := h3.GridDisk(origin, area.RadiusRings)
		if diskErr != nil {
			return Result{}, fmt.Errorf("tessellate: expanding origin %s: %w", origin, diskErr)
		}

		for _, dest := range neighbors {
			if _, ok := inArea[dest]; !ok {
				continue // clip destinations to the service area
			}

			for _, profile := range profiles {
				pairs = append(pairs, beeline.PairKey{
					Origin:  origin,
					Dest:    dest,
					Profile: profile,
					Res:     area.Resolution,
				})
			}
		}
	}

	return Result{Cells: cells, Pairs: pairs}, nil
}
