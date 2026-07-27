package tessellate_test

import (
	"fmt"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/geo"
	"github.com/primandproper/beeline/internal/tessellate"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/h3-go/v4"
)

func TestPairsFromCellsEmitsSelfPairs(t *testing.T) {
	t.Parallel()

	_, cells := resDisk(t, 8, 2)
	profiles := []beeline.Profile{"car", "bike"}

	pairs, err := tessellate.PairsFromCells(0, cells, 8, 1500, profiles)
	require.NoError(t, err)
	require.NotEmpty(t, pairs)

	var selfPairs int
	for _, p := range pairs {
		if p.Origin == p.Dest {
			selfPairs++
		}
	}

	// Every origin×profile has a self pair (used to exercise same-cell handling).
	assert.Equal(t, len(cells)*len(profiles), selfPairs)
}

// resDisk returns the GridDisk of radius r around San Francisco at the given
// resolution, plus its center cell, for the meters-bound tests.
func resDisk(t *testing.T, res, r int) (center beeline.H3Cell, cells []beeline.H3Cell) {
	t.Helper()

	center, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, res)
	require.NoError(t, err)
	cells, err = h3.GridDisk(center, r)
	require.NoError(t, err)

	return center, cells
}

func TestRingsForRadius(t *testing.T) {
	t.Parallel()

	center, centerErr := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, centerErr)

	t.Run("a non-positive radius is zero rings (origin only)", func(t *testing.T) {
		t.Parallel()

		k, err := tessellate.RingsForRadius(center, 0)
		require.NoError(t, err)
		assert.Equal(t, 0, k)

		k, err = tessellate.RingsForRadius(center, -100)
		require.NoError(t, err)
		assert.Equal(t, 0, k)
	})

	t.Run("ring count grows monotonically with the bound", func(t *testing.T) {
		t.Parallel()

		var prev int
		for _, radius := range []float64{100, 500, 1000, 2000, 4000, 8000} {
			k, err := tessellate.RingsForRadius(center, radius)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, k, prev, "radius %.0f should not shrink the ring count", radius)
			prev = k
		}
		assert.Positive(t, prev, "a multi-km bound must reach beyond the origin cell")
	})

	t.Run("every cell in the derived disk is within the bound", func(t *testing.T) {
		t.Parallel()

		const radius = 2000.0
		k, err := tessellate.RingsForRadius(center, radius)
		require.NoError(t, err)
		require.Positive(t, k)

		originLL, err := beeline.Center(center)
		require.NoError(t, err)

		// The ring at exactly k must have at least one cell within the bound (that is
		// why it was included), and ring k+1's nearest cell must exceed it.
		inner, err := h3.GridDisk(center, k)
		require.NoError(t, err)
		outer, err := h3.GridDisk(center, k+1)
		require.NoError(t, err)

		innerSet := make(map[beeline.H3Cell]struct{}, len(inner))
		for _, c := range inner {
			innerSet[c] = struct{}{}
		}

		var nearestNextRing float64 = -1
		for _, c := range outer {
			if _, ok := innerSet[c]; ok {
				continue
			}
			cc, cErr := beeline.Center(c)
			require.NoError(t, cErr)
			if d := geo.Haversine(originLL, cc); nearestNextRing < 0 || d < nearestNextRing {
				nearestNextRing = d
			}
		}
		assert.Greater(t, nearestNextRing, radius, "the next ring out must be beyond the bound")
	})
}

func TestMinRadiusForNeighbors(t *testing.T) {
	t.Parallel()

	for _, res := range []int{6, 8, 9, 11} {
		t.Run(fmt.Sprintf("res-%d floor is the k=0/k>=1 boundary", res), func(t *testing.T) {
			t.Parallel()

			cell, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, res)
			require.NoError(t, err)

			floor, err := tessellate.MinRadiusForNeighbors(cell)
			require.NoError(t, err)
			require.Positive(t, floor, "a hexagon cell must have a measurable neighbor floor")

			// Just below the floor reaches no neighbor; at the floor it reaches ring 1.
			below, err := tessellate.RingsForRadius(cell, floor*0.99)
			require.NoError(t, err)
			assert.Equal(t, 0, below, "a bound below the floor must yield origin-only self-pairs")

			at, err := tessellate.RingsForRadius(cell, floor)
			require.NoError(t, err)
			assert.GreaterOrEqual(t, at, 1, "a bound at the floor must reach at least ring 1")
		})
	}
}

func TestPairsFromCellsFullMeshSentinel(t *testing.T) {
	t.Parallel()

	_, cells := resDisk(t, 9, 1) // 7 cells
	profiles := []beeline.Profile{"car", "bike"}

	pairs, err := tessellate.PairsFromCells(0, cells, 9, 0, profiles)
	require.NoError(t, err)

	// Full mesh: every origin × every in-area dest × every profile.
	assert.Len(t, pairs, len(cells)*len(cells)*len(profiles))

	// Every pair is in-area, tagged with the resolution and a requested profile.
	inArea := make(map[beeline.H3Cell]struct{}, len(cells))
	for _, c := range cells {
		inArea[c] = struct{}{}
	}
	for _, p := range pairs {
		assert.Equal(t, 9, p.Res)
		assert.Contains(t, inArea, p.Origin)
		assert.Contains(t, inArea, p.Dest)
	}
}

func TestPairsFromCellsBoundedMatchesRingDisk(t *testing.T) {
	t.Parallel()

	const res = 9
	center, cells := resDisk(t, res, 4) // a generous area so the bound, not the area, clips
	const radius = 1500.0

	k, err := tessellate.RingsForRadius(center, radius)
	require.NoError(t, err)
	require.Positive(t, k)

	pairs, err := tessellate.PairsFromCells(0, cells, res, radius, []beeline.Profile{"car"})
	require.NoError(t, err)

	// The center origin's outgoing pairs must be exactly its k-ring disk clipped to
	// the area (which fully contains it here), one per in-area disk cell.
	inArea := make(map[beeline.H3Cell]struct{}, len(cells))
	for _, c := range cells {
		inArea[c] = struct{}{}
	}
	disk, err := h3.GridDisk(center, k)
	require.NoError(t, err)
	var wantFromCenter int
	for _, d := range disk {
		if _, ok := inArea[d]; ok {
			wantFromCenter++
		}
	}

	var gotFromCenter int
	for _, p := range pairs {
		if p.Origin == center {
			gotFromCenter++
		}
	}
	assert.Equal(t, wantFromCenter, gotFromCenter, "center origin pairs with exactly its clipped k-ring disk")
}

func TestPairsFromCellsRejectsNegativeRadius(t *testing.T) {
	t.Parallel()

	_, cells := resDisk(t, 9, 1)
	_, err := tessellate.PairsFromCells(0, cells, 9, -1, []beeline.Profile{"car"})
	assert.Error(t, err)
}
