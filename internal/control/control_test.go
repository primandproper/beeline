package control_test

import (
	"context"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeIndex struct {
	reseeded [][]beeline.PairKey
	states   []beeline.CellState
}

func (f *fakeIndex) Reseed(_ context.Context, keys []beeline.PairKey) error {
	f.reseeded = append(f.reseeded, keys)

	return nil
}

func (f *fakeIndex) CellStates(_ context.Context) ([]beeline.CellState, error) {
	return f.states, nil
}

type fakeStore struct{ resets int }

func (f *fakeStore) Reset() { f.resets++ }

type fakeResolver struct {
	res  int
	sets int
}

func (f *fakeResolver) SetResolution(r int) {
	f.res = r
	f.sets++
}

func newCoordinator() (*control.Coordinator, *fakeIndex, *fakeStore, *fakeResolver) {
	idx := &fakeIndex{}
	store := &fakeStore{}
	resolver := &fakeResolver{}
	c := control.New(idx, store, resolver, []beeline.Profile{"car", "bike"}, nil)

	return c, idx, store, resolver
}

func sfArea() control.Area {
	return control.Area{Lat: 37.7749, Lng: -122.4194, Resolution: 8, AreaRings: 1, RadiusRings: 1}
}

func TestApplyDrivesAllSeams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c, idx, store, resolver := newCoordinator()

	summary, err := c.Apply(ctx, sfArea())
	require.NoError(t, err)

	// A single central cell plus its first ring = 7 cells.
	assert.Equal(t, 7, summary.Cells)
	assert.Positive(t, summary.Pairs)
	assert.Equal(t, []string{"car", "bike"}, summary.Profiles)
	assert.Equal(t, 8, summary.Resolution)

	// The store was cleared, the index reseeded with the whole pair set, and the
	// read path repointed at the new resolution.
	assert.Equal(t, 1, store.resets)
	require.Len(t, idx.reseeded, 1)
	assert.Len(t, idx.reseeded[0], summary.Pairs)
	assert.Equal(t, 8, resolver.res)
	assert.Equal(t, 1, resolver.sets)

	// Current reflects the applied area.
	cur := c.Current()
	assert.Equal(t, summary, cur)
}

func TestApplyRejectsInvalidArea(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c, idx, store, resolver := newCoordinator()

	cases := map[string]control.Area{
		"resolution too high": {Lat: 0, Lng: 0, Resolution: 20, AreaRings: 1, RadiusRings: 1},
		"radius rings zero":   {Lat: 0, Lng: 0, Resolution: 8, AreaRings: 1, RadiusRings: 0},
		"latitude range":      {Lat: 200, Lng: 0, Resolution: 8, AreaRings: 1, RadiusRings: 1},
	}

	for name, area := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			_, err := c.Apply(ctx, area)
			require.Error(t, err)
		})
	}

	// A rejected Apply touches none of the seams and leaves the area empty.
	assert.Equal(t, 0, store.resets)
	assert.Empty(t, idx.reseeded)
	assert.Equal(t, 0, resolver.sets)
	assert.Equal(t, control.Summary{Profiles: []string{"car", "bike"}}, c.Current())
}

func TestCellStatesProxiesIndex(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c, idx, _, _ := newCoordinator()
	idx.states = []beeline.CellState{{Origin: 42, Total: 3, Fresh: 2}}

	got, err := c.CellStates(ctx)
	require.NoError(t, err)
	assert.Equal(t, idx.states, got)
}
