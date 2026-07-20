package tessellate_test

import (
	"fmt"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/tessellate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/h3-go/v4"
)

// squarePolygon returns a GeoJSON Polygon (as a Feature) covering the axis-aligned
// box [minLng,minLat]–[maxLng,maxLat], closed per the GeoJSON spec (first == last).
func squarePolygon(minLng, minLat, maxLng, maxLat float64) string {
	return fmt.Sprintf(`{
      "type": "Feature",
      "properties": {},
      "geometry": {
        "type": "Polygon",
        "coordinates": [[[%[1]f,%[2]f],[%[3]f,%[2]f],[%[3]f,%[4]f],[%[1]f,%[4]f],[%[1]f,%[2]f]]]
      }
    }`, minLng, minLat, maxLng, maxLat)
}

func TestCellsFromGeoJSON(t *testing.T) {
	t.Parallel()

	t.Run("fills a polygon and honors lng,lat ordering", func(t *testing.T) {
		t.Parallel()

		// A ~0.2°×0.2° box around San Francisco. If lat/lng were swapped, the box
		// would land near (−122,37) in the ocean off nowhere and the cells would not
		// contain the SF point.
		raw := squarePolygon(-122.52, 37.70, -122.35, 37.83)

		cells, err := tessellate.CellsFromGeoJSON([]byte(raw), 8)
		require.NoError(t, err)
		require.NotEmpty(t, cells)

		sf, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 8)
		require.NoError(t, err)
		assert.Contains(t, cells, sf, "the SF point's cell must be inside the filled box")

		// Every returned cell is at the requested resolution and inside the box.
		for _, c := range cells {
			assert.Equal(t, 8, c.Resolution())
			center, cerr := beeline.Center(c)
			require.NoError(t, cerr)
			assert.True(t, center.Lat >= 37.70 && center.Lat <= 37.83, "cell center lat within box")
			assert.True(t, center.Lng >= -122.52 && center.Lng <= -122.35, "cell center lng within box")
		}
	})

	t.Run("returns no duplicates", func(t *testing.T) {
		t.Parallel()

		raw := squarePolygon(-122.52, 37.70, -122.35, 37.83)
		cells, err := tessellate.CellsFromGeoJSON([]byte(raw), 8)
		require.NoError(t, err)

		seen := make(map[beeline.H3Cell]struct{}, len(cells))
		for _, c := range cells {
			_, dup := seen[c]
			require.False(t, dup, "cell %s duplicated", c)
			seen[c] = struct{}{}
		}
	})

	t.Run("a hole removes interior cells", func(t *testing.T) {
		t.Parallel()

		outer := squarePolygon(-122.52, 37.70, -122.35, 37.83)
		solid, err := tessellate.CellsFromGeoJSON([]byte(outer), 8)
		require.NoError(t, err)

		// Same outer ring with a large interior hole.
		withHole := `{
          "type": "Polygon",
          "coordinates": [
            [[-122.52,37.70],[-122.35,37.70],[-122.35,37.83],[-122.52,37.83],[-122.52,37.70]],
            [[-122.47,37.74],[-122.40,37.74],[-122.40,37.80],[-122.47,37.80],[-122.47,37.74]]
          ]
        }`
		holed, err := tessellate.CellsFromGeoJSON([]byte(withHole), 8)
		require.NoError(t, err)

		assert.Less(t, len(holed), len(solid), "a hole must drop interior cells")
	})

	t.Run("MultiPolygon unions its polygons", func(t *testing.T) {
		t.Parallel()

		a := `[[[-122.52,37.70],[-122.45,37.70],[-122.45,37.77],[-122.52,37.77],[-122.52,37.70]]]`
		b := `[[[-122.44,37.78],[-122.35,37.78],[-122.35,37.83],[-122.44,37.83],[-122.44,37.78]]]`
		multi := fmt.Sprintf(`{"type":"MultiPolygon","coordinates":[%s,%s]}`, a, b)

		cells, err := tessellate.CellsFromGeoJSON([]byte(multi), 8)
		require.NoError(t, err)
		require.NotEmpty(t, cells)
	})

	t.Run("rejects unsupported and malformed input", func(t *testing.T) {
		t.Parallel()

		_, err := tessellate.CellsFromGeoJSON([]byte(`{"type":"Point","coordinates":[0,0]}`), 8)
		assert.Error(t, err, "a Point has no areal geometry")

		_, err = tessellate.CellsFromGeoJSON([]byte(`not json`), 8)
		assert.Error(t, err)

		_, err = tessellate.CellsFromGeoJSON([]byte(squarePolygon(-122.5, 37.7, -122.4, 37.8)), 42)
		assert.Error(t, err, "out-of-range resolution")
	})
}

// TestPairsFromCells checks the area-tagged pair builder in isolation.
func TestPairsFromCells(t *testing.T) {
	t.Parallel()

	center, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 8)
	require.NoError(t, err)
	cells, err := h3.GridDisk(center, 2)
	require.NoError(t, err)

	pairs, err := tessellate.PairsFromCells(beeline.AreaID(7), cells, 8, 1, []beeline.Profile{"car", "bike"})
	require.NoError(t, err)
	require.NotEmpty(t, pairs)

	inArea := make(map[beeline.H3Cell]struct{}, len(cells))
	for _, c := range cells {
		inArea[c] = struct{}{}
	}
	for _, p := range pairs {
		assert.Equal(t, beeline.AreaID(7), p.Area, "every pair carries the area id")
		assert.Equal(t, 8, p.Res)
		assert.Contains(t, inArea, p.Origin)
		assert.Contains(t, inArea, p.Dest)
	}

	_, err = tessellate.PairsFromCells(1, cells, 8, 1, nil)
	assert.Error(t, err, "at least one profile is required")
}
