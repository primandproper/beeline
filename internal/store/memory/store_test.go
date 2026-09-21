package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memory "github.com/primandproper/beeline/internal/store/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The Store contract itself lives in conformance_test.go, which runs the shared
// storetest suite. What remains here is the memory store's own surface: Len and
// Reset are not part of beeline.Store, so no shared suite can cover them.

func put(t *testing.T, store *memory.Store, area beeline.AreaID, origin beeline.H3Cell) {
	t.Helper()

	require.NoError(t, store.Put(context.Background(), []beeline.Entry{{
		Key:      beeline.PairKey{Area: area, Origin: origin, Dest: origin + 1, Profile: "car", Res: 8},
		Duration: 1, Distance: 1, ComputedAt: time.Now(),
	}}))
}

func TestStoreLenTracksStoredPairs(t *testing.T) {
	t.Parallel()

	store := memory.New()
	assert.Equal(t, 0, store.Len(), "a fresh store is empty")

	put(t, store, 1, 10)
	put(t, store, 1, 11)
	put(t, store, 2, 20)
	assert.Equal(t, 3, store.Len())

	// Re-putting a key already present must not inflate the count.
	put(t, store, 1, 10)
	assert.Equal(t, 3, store.Len(), "an overwrite is not a new pair")

	require.NoError(t, store.DeleteArea(context.Background(), 1))
	assert.Equal(t, 1, store.Len(), "only area 2's estimate remains")
}

func TestStoreResetDropsEverything(t *testing.T) {
	t.Parallel()

	store := memory.New()
	put(t, store, 1, 10)
	put(t, store, 2, 20)
	require.Equal(t, 2, store.Len())

	store.Reset()

	assert.Equal(t, 0, store.Len(), "a re-tessellation drops every cached estimate")

	got, err := store.BatchGet(context.Background(), []beeline.PairKey{
		{Area: 1, Origin: 10, Dest: 11, Profile: "car", Res: 8},
	})
	require.NoError(t, err)
	assert.Nil(t, got[0], "and the dropped keys read as misses")
}
