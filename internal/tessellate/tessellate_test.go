package tessellate_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/tessellate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSeed(t *testing.T) {
	t.Parallel()

	area := tessellate.Area{
		Center:      beeline.LatLng{Lat: 37.7749, Lng: -122.4194},
		Resolution:  8,
		AreaRings:   2,
		RadiusRings: 1,
	}

	t.Run("covers the expected number of cells", func(t *testing.T) {
		t.Parallel()

		res, err := tessellate.Seed(area, []beeline.Profile{"car"}, nil)
		require.NoError(t, err)

		// GridDisk of radius k covers 3k^2 + 3k + 1 cells; k=2 → 19.
		assert.Len(t, res.Cells, 19)
	})

	t.Run("pairs are clipped to the area and tagged with res and profile", func(t *testing.T) {
		t.Parallel()

		res, err := tessellate.Seed(area, []beeline.Profile{"car", "bike"}, nil)
		require.NoError(t, err)
		require.NotEmpty(t, res.Pairs)

		inArea := make(map[beeline.H3Cell]struct{}, len(res.Cells))
		for _, c := range res.Cells {
			inArea[c] = struct{}{}
		}

		var selfPairs int
		for _, p := range res.Pairs {
			assert.Equal(t, 8, p.Res)
			assert.Contains(t, []beeline.Profile{"car", "bike"}, p.Profile)
			assert.Contains(t, inArea, p.Origin)
			assert.Contains(t, inArea, p.Dest)
			if p.Origin == p.Dest {
				selfPairs++
			}
		}

		// Every origin×profile has a self pair (used to exercise same-cell handling).
		assert.Equal(t, len(res.Cells)*2, selfPairs)
	})

	t.Run("rejects an out-of-range resolution", func(t *testing.T) {
		t.Parallel()

		bad := area
		bad.Resolution = 42
		_, err := tessellate.Seed(bad, []beeline.Profile{"car"}, nil)
		assert.Error(t, err)
	})

	t.Run("a road mask at the area resolution prunes cells outside it", func(t *testing.T) {
		t.Parallel()

		// Build a mask holding only the center cell, then confirm Seed keeps exactly
		// that cell (and, therefore, only its self-pairs).
		full, err := tessellate.Seed(area, []beeline.Profile{"car"}, nil)
		require.NoError(t, err)
		require.Greater(t, len(full.Cells), 1)

		center, err := beeline.CellAt(area.Center, area.Resolution)
		require.NoError(t, err)

		mask := tessellate.NewMask(area.Resolution, []beeline.H3Cell{center})

		masked, err := tessellate.Seed(area, []beeline.Profile{"car"}, mask)
		require.NoError(t, err)
		assert.Equal(t, []beeline.H3Cell{center}, masked.Cells)
		for _, p := range masked.Pairs {
			assert.Equal(t, center, p.Origin)
			assert.Equal(t, center, p.Dest)
		}
	})

	t.Run("a coarser mask prunes finer cells via their parent", func(t *testing.T) {
		t.Parallel()

		// Area at res 9; mask at res 8 holding only the center's res-8 parent. Every
		// kept res-9 cell must descend from that one allowed parent (the console
		// re-tessellating finer than the mask still prunes).
		fine := area
		fine.Resolution = 9

		center9, err := beeline.CellAt(fine.Center, fine.Resolution)
		require.NoError(t, err)
		parent8, err := center9.Parent(8)
		require.NoError(t, err)

		mask := tessellate.NewMask(8, []beeline.H3Cell{parent8})

		full, err := tessellate.Seed(fine, []beeline.Profile{"car"}, nil)
		require.NoError(t, err)
		masked, err := tessellate.Seed(fine, []beeline.Profile{"car"}, mask)
		require.NoError(t, err)

		assert.Less(t, len(masked.Cells), len(full.Cells))
		assert.Contains(t, masked.Cells, center9)
		for _, c := range masked.Cells {
			parent, perr := c.Parent(8)
			require.NoError(t, perr)
			assert.Equal(t, parent8, parent)
		}
	})

	t.Run("a finer mask prunes coarser cells with no road descendant", func(t *testing.T) {
		t.Parallel()

		// Area at res 8; mask at res 9 holding one res-9 child of the center res-8
		// cell. Only the center cell has a road descendant, so only it survives.
		center8, err := beeline.CellAt(area.Center, area.Resolution)
		require.NoError(t, err)
		children9, err := center8.Children(9)
		require.NoError(t, err)

		mask := tessellate.NewMask(9, []beeline.H3Cell{children9[0]})

		masked, err := tessellate.Seed(area, []beeline.Profile{"car"}, mask)
		require.NoError(t, err)
		assert.Equal(t, []beeline.H3Cell{center8}, masked.Cells)
	})
}
