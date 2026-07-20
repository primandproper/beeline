package tessellate

import (
	"encoding/json"
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/uber/h3-go/v4"
)

// CellsFromGeoJSON polyfills a GeoJSON polygon into the set of H3 cells at the given
// resolution (§7). It accepts a bare Geometry, a Feature, or a FeatureCollection, and
// within those a Polygon or MultiPolygon; every polygon is filled and the union
// (deduplicated) is returned.
//
// GeoJSON positions are [longitude, latitude]; h3 wants latitude/longitude, so the
// order is swapped here. Result order is unspecified.
func CellsFromGeoJSON(raw []byte, resolution int) ([]beeline.H3Cell, error) {
	if resolution < 0 || resolution > 15 {
		return nil, fmt.Errorf("tessellate: resolution %d out of range [0,15]", resolution)
	}

	polygons, err := polygonsFromGeoJSON(raw)
	if err != nil {
		return nil, err
	}
	if len(polygons) == 0 {
		return nil, fmt.Errorf("tessellate: geojson contained no Polygon/MultiPolygon geometry")
	}

	seen := make(map[beeline.H3Cell]struct{})
	cells := make([]beeline.H3Cell, 0)
	for i := range polygons {
		filled, fillErr := h3.PolygonToCells(polygons[i], resolution)
		if fillErr != nil {
			return nil, fmt.Errorf("tessellate: polyfilling geojson: %w", fillErr)
		}

		for _, c := range filled {
			if _, dup := seen[c]; dup {
				continue
			}
			seen[c] = struct{}{}
			cells = append(cells, c)
		}
	}

	return cells, nil
}

// geometry is a minimal GeoJSON geometry: a type discriminator plus raw coordinates
// decoded per-type (Polygon: [][]position, MultiPolygon: [][][]position).
type geometry struct {
	Type        string          `json:"type"`
	Coordinates json.RawMessage `json:"coordinates"`
}

type feature struct {
	Type     string   `json:"type"`
	Geometry geometry `json:"geometry"`
}

type featureCollection struct {
	Type     string    `json:"type"`
	Features []feature `json:"features"`
}

// polygonsFromGeoJSON decodes any of the three GeoJSON container shapes down to the
// list of h3.GeoPolygon they contain.
func polygonsFromGeoJSON(raw []byte) ([]h3.GeoPolygon, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return nil, fmt.Errorf("tessellate: decoding geojson: %w", err)
	}

	var geometries []geometry

	switch probe.Type {
	case "FeatureCollection":
		var fc featureCollection
		if err := json.Unmarshal(raw, &fc); err != nil {
			return nil, fmt.Errorf("tessellate: decoding feature collection: %w", err)
		}
		for i := range fc.Features {
			geometries = append(geometries, fc.Features[i].Geometry)
		}
	case "Feature":
		var f feature
		if err := json.Unmarshal(raw, &f); err != nil {
			return nil, fmt.Errorf("tessellate: decoding feature: %w", err)
		}
		geometries = append(geometries, f.Geometry)
	case "Polygon", "MultiPolygon":
		var g geometry
		if err := json.Unmarshal(raw, &g); err != nil {
			return nil, fmt.Errorf("tessellate: decoding geometry: %w", err)
		}
		geometries = append(geometries, g)
	default:
		return nil, fmt.Errorf("tessellate: unsupported geojson type %q", probe.Type)
	}

	var polygons []h3.GeoPolygon
	for i := range geometries {
		polys, err := geometryToPolygons(geometries[i])
		if err != nil {
			return nil, err
		}
		polygons = append(polygons, polys...)
	}

	return polygons, nil
}

// geometryToPolygons turns one geometry into zero or more h3.GeoPolygon. Geometry
// types other than Polygon/MultiPolygon (points, lines) are skipped, not errors, so
// a mixed FeatureCollection still yields its polygons.
func geometryToPolygons(g geometry) ([]h3.GeoPolygon, error) {
	switch g.Type {
	case "Polygon":
		var rings [][][]float64
		if err := json.Unmarshal(g.Coordinates, &rings); err != nil {
			return nil, fmt.Errorf("tessellate: decoding polygon coordinates: %w", err)
		}
		poly, err := ringsToPolygon(rings)
		if err != nil {
			return nil, err
		}

		return []h3.GeoPolygon{poly}, nil
	case "MultiPolygon":
		var multi [][][][]float64
		if err := json.Unmarshal(g.Coordinates, &multi); err != nil {
			return nil, fmt.Errorf("tessellate: decoding multipolygon coordinates: %w", err)
		}
		polys := make([]h3.GeoPolygon, 0, len(multi))
		for i := range multi {
			poly, err := ringsToPolygon(multi[i])
			if err != nil {
				return nil, err
			}
			polys = append(polys, poly)
		}

		return polys, nil
	default:
		return nil, nil // non-areal geometry; ignore
	}
}

// ringsToPolygon converts GeoJSON linear rings (exterior first, holes after) into an
// h3.GeoPolygon. The first ring is the outer boundary; any remaining rings are holes.
func ringsToPolygon(rings [][][]float64) (h3.GeoPolygon, error) {
	if len(rings) == 0 {
		return h3.GeoPolygon{}, fmt.Errorf("tessellate: polygon has no rings")
	}

	outer, err := ringToLoop(rings[0])
	if err != nil {
		return h3.GeoPolygon{}, err
	}

	var holes []h3.GeoLoop
	for i := 1; i < len(rings); i++ {
		hole, holeErr := ringToLoop(rings[i])
		if holeErr != nil {
			return h3.GeoPolygon{}, holeErr
		}
		holes = append(holes, hole)
	}

	return h3.GeoPolygon{GeoLoop: outer, Holes: holes}, nil
}

// ringToLoop converts a GeoJSON linear ring ([[lng,lat],…]) into an h3.GeoLoop
// ([]h3.LatLng). GeoJSON rings repeat the first position as the last to close the
// ring; h3 loops are implicitly closed, so the duplicate trailing vertex is dropped.
func ringToLoop(ring [][]float64) (h3.GeoLoop, error) {
	if len(ring) < 3 {
		return nil, fmt.Errorf("tessellate: ring needs at least 3 positions, got %d", len(ring))
	}

	end := len(ring)
	if first, last := ring[0], ring[end-1]; len(first) >= 2 && len(last) >= 2 &&
		first[0] == last[0] && first[1] == last[1] {
		end-- // drop the closing vertex
	}

	loop := make(h3.GeoLoop, 0, end)
	for i := 0; i < end; i++ {
		pos := ring[i]
		if len(pos) < 2 {
			return nil, fmt.Errorf("tessellate: position needs [lng,lat], got %v", pos)
		}
		loop = append(loop, h3.NewLatLng(pos[1], pos[0])) // GeoJSON is [lng,lat]
	}

	return loop, nil
}
