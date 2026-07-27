// Package tessellate turns a service area into the set of origin→destination pairs
// the freshness index must keep fresh. It stands in for the design's ingestion path
// (§7: polyfill → seed pair set). An area's canonical geometry is its GeoJSON
// polygon; each of the area's layers polyfills it at that layer's resolution
// (CellsFromGeoJSON) and PairsFromCells derives the directed pair set from the
// resulting cells.
package tessellate

import (
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/geo"

	"github.com/uber/h3-go/v4"
)

// maxRings caps the empirical meters→rings search so a pathological bound can't spin
// the grow loop forever. A res-0 cell spans thousands of km, so even a global bound
// resolves in a handful of rings; this is a safety backstop, not an operational limit.
const maxRings = 10000

// PairsFromCells builds the directed origin→destination pair set for an explicit set
// of area cells, tagging every pair with the given service area. This is the
// area-agnostic half of tessellation: whatever produced the cell set (a GeoJSON
// polyfill, a test's ring disk), the pair set is derived the same way.
//
// Origin-centric (§6): each origin contributes a dense ring of destinations within
// radiusMeters that are also inside the area, which is the shape the refresh worker
// packs into a single 1×K table request. One pair is emitted per
// origin × reachable-destination × profile.
//
// radiusMeters is the travel-radius bound in meters. It is converted once to an H3
// ring count k (the same for every origin at this resolution) via RingsForRadius, and
// each origin's destinations are GridDisk(origin, k) clipped to the area. The special
// value 0 is the full-mesh sentinel: every origin is paired with every in-area cell
// (the GridDisk step is skipped entirely).
func PairsFromCells(area beeline.AreaID, cells []beeline.H3Cell, resolution int, radiusMeters float64, profiles []beeline.Profile) ([]beeline.PairKey, error) {
	if len(profiles) == 0 {
		return nil, fmt.Errorf("tessellate: at least one profile required")
	}
	if radiusMeters < 0 {
		return nil, fmt.Errorf("tessellate: radius meters %.2f must be >= 0", radiusMeters)
	}
	if len(cells) == 0 {
		return nil, nil
	}

	inArea := make(map[beeline.H3Cell]struct{}, len(cells))
	for _, c := range cells {
		inArea[c] = struct{}{}
	}

	if radiusMeters == 0 {
		return fullMeshPairs(area, cells, resolution, profiles), nil
	}

	// The ring count is a function of (resolution, radiusMeters) only, so derive it
	// once from a representative origin and reuse it for every cell (§ Phase 1).
	rings, err := RingsForRadius(cells[0], radiusMeters)
	if err != nil {
		return nil, err
	}

	pairs := make([]beeline.PairKey, 0, len(cells)*(1+3*rings*(rings+1))*len(profiles))
	for _, origin := range cells {
		neighbors, diskErr := h3.GridDisk(origin, rings)
		if diskErr != nil {
			return nil, fmt.Errorf("tessellate: expanding origin %s: %w", origin, diskErr)
		}

		for _, dest := range neighbors {
			if _, ok := inArea[dest]; !ok {
				continue // clip destinations to the service area
			}

			for _, profile := range profiles {
				pairs = append(pairs, beeline.PairKey{
					Area:    area,
					Origin:  origin,
					Dest:    dest,
					Profile: profile,
					Res:     resolution,
				})
			}
		}
	}

	return pairs, nil
}

// fullMeshPairs pairs every origin with every in-area cell (including itself), for
// every profile. This is the MaxRadiusMeters == 0 case: no travel bound, the whole
// area's N² directed pairs. Callers should guard the O(N²) blow-up for large areas.
func fullMeshPairs(area beeline.AreaID, cells []beeline.H3Cell, resolution int, profiles []beeline.Profile) []beeline.PairKey {
	pairs := make([]beeline.PairKey, 0, len(cells)*len(cells)*len(profiles))
	for _, origin := range cells {
		for _, dest := range cells {
			for _, profile := range profiles {
				pairs = append(pairs, beeline.PairKey{
					Area:    area,
					Origin:  origin,
					Dest:    dest,
					Profile: profile,
					Res:     resolution,
				})
			}
		}
	}

	return pairs
}

// MinRadiusForNeighbors returns the smallest travel-radius bound (in meters) that
// still reaches at least the first ring of neighbors around sample — i.e. the distance
// from sample's center to its nearest ring-1 cell. A bound below this value yields
// RingsForRadius == 0, collapsing the pair set to useless origin-only self-pairs, so it
// is the operational floor for a bounded (non-full-mesh) area at sample's resolution.
//
// It measures real great-circle distance (geo.Haversine), the same method RingsForRadius
// uses, so the two agree: a radius >= MinRadiusForNeighbors resolves to k >= 1. The
// result is deterministic for a given (resolution, cell) since cell geometry is fixed.
func MinRadiusForNeighbors(sample beeline.H3Cell) (float64, error) {
	center, err := beeline.Center(sample)
	if err != nil {
		return 0, err
	}

	ring, err := h3.GridDisk(sample, 1)
	if err != nil {
		return 0, fmt.Errorf("tessellate: sizing neighbor floor: %w", err)
	}

	nearest := -1.0
	for _, c := range ring {
		if c == sample {
			continue
		}

		cc, centerErr := beeline.Center(c)
		if centerErr != nil {
			return 0, centerErr
		}
		if d := geo.Haversine(center, cc); nearest < 0 || d < nearest {
			nearest = d
		}
	}

	if nearest < 0 {
		// A pentagon cell can have no ring-1 neighbor to measure; treat as no floor.
		return 0, nil
	}

	return nearest, nil
}

// RingsForRadius returns the H3 GridDisk ring count k that best approximates a metric
// disk of radiusMeters around origin. It grows k outward one ring at a time, measuring
// the actual great-circle distance (geo.Haversine) from origin's center to the nearest
// cell of the candidate ring, and stops at the last ring whose nearest cell is still
// within the bound. Measuring real distance keeps this correct across resolutions and
// avoids brittle per-resolution edge-length constants (§ Phase 1, decision 2).
//
// A non-positive radius yields 0 rings (origin only). The result is deterministic for
// a given (origin resolution, radiusMeters) since cell geometry is fixed.
func RingsForRadius(origin beeline.H3Cell, radiusMeters float64) (int, error) {
	if radiusMeters <= 0 {
		return 0, nil
	}

	center, err := beeline.Center(origin)
	if err != nil {
		return 0, err
	}

	seen := map[beeline.H3Cell]struct{}{origin: {}}
	k := 0
	for k < maxRings {
		next := k + 1
		disk, diskErr := h3.GridDisk(origin, next)
		if diskErr != nil {
			return 0, fmt.Errorf("tessellate: sizing radius at ring %d: %w", next, diskErr)
		}

		nearest := -1.0
		grew := false
		for _, c := range disk {
			if _, ok := seen[c]; ok {
				continue
			}
			grew = true

			cc, centerErr := beeline.Center(c)
			if centerErr != nil {
				return 0, centerErr
			}
			if d := geo.Haversine(center, cc); nearest < 0 || d < nearest {
				nearest = d
			}
		}

		// GridDisk stopped growing (a pentagon can pin the frontier); take what we have.
		if !grew {
			return k, nil
		}
		// The whole next ring is beyond the bound: the current disk covers the radius.
		if nearest > radiusMeters {
			return k, nil
		}

		for _, c := range disk {
			seen[c] = struct{}{}
		}
		k = next
	}

	return k, nil
}
