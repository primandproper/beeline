package refresh_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/refresh"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oneEngine resolves every area to the same engine.
type oneEngine struct{ engine beeline.RoutingEngine }

func (o oneEngine) EngineFor(beeline.AreaID) beeline.RoutingEngine { return o.engine }

// distinctOriginPairs builds n pairs that each have their own origin cell, so the
// pool's (area, origin, profile) grouping yields n single-entry groups and the
// flush boundary is a pure function of SubmitChunk.
func distinctOriginPairs(t *testing.T, n int) []beeline.PairKey {
	t.Helper()

	pairs := make([]beeline.PairKey, 0, n)
	for i := range n {
		origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.70 + float64(i)*0.05, Lng: -122.4194}, 9)
		require.NoError(t, err)
		dest, destErr := beeline.CellAt(beeline.LatLng{Lat: 37.70 + float64(i)*0.05, Lng: -122.3000}, 9)
		require.NoError(t, destErr)
		pairs = append(pairs, beeline.PairKey{
			Area:    1,
			Origin:  origin,
			Dest:    dest,
			Profile: "car",
			Res:     9,
		})
	}

	return pairs
}

// recordingSource hands out one fixed batch, then nothing, and records the keys of
// every Submit it receives. failFrom is the 1-based ordinal of the first Submit to
// reject (0 rejects none).
type recordingSource struct {
	pending  []beeline.PairKey
	submits  [][]beeline.PairKey
	failFrom int
	mu       sync.Mutex
}

func (r *recordingSource) Claim(_ context.Context, limit int, _ time.Duration) ([]beeline.PairKey, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if len(r.pending) == 0 {
		return nil, nil
	}
	if limit > len(r.pending) {
		limit = len(r.pending)
	}
	claimed := r.pending[:limit]
	r.pending = r.pending[limit:]

	return claimed, nil
}

func (r *recordingSource) Submit(_ context.Context, entries []beeline.Entry) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	keys := make([]beeline.PairKey, 0, len(entries))
	for i := range entries {
		keys = append(keys, entries[i].Key)
	}
	r.submits = append(r.submits, keys)

	if r.failFrom > 0 && len(r.submits) >= r.failFrom {
		return errors.New("sink is down")
	}

	return nil
}

func (r *recordingSource) recorded() [][]beeline.PairKey {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([][]beeline.PairKey(nil), r.submits...)
}

// runPool starts a one-worker pool over src and stops it when the test ends.
func runPool(t *testing.T, src refresh.WorkSource, chunk int) {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)

	pool := refresh.NewPool(
		oneEngine{engine: haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)},
		src, nil,
		refresh.Config{
			Workers:     1,
			Batch:       100,
			SubmitChunk: chunk,
			Lease:       time.Minute,
			IdleBackoff: 5 * time.Millisecond,
		},
	)
	go pool.Run(ctx)
}

func TestPoolFlushesInChunks(t *testing.T) {
	t.Parallel()

	pairs := distinctOriginPairs(t, 6)
	src := &recordingSource{pending: pairs}
	runPool(t, src, 2)

	require.Eventually(t, func() bool {
		return len(src.recorded()) == 3
	}, 5*time.Second, 10*time.Millisecond, "six single-entry groups flush as three chunks of two")

	seen := 0
	for _, submit := range src.recorded() {
		assert.Len(t, submit, 2, "every flush carries exactly one chunk")
		seen += len(submit)
	}
	assert.Equal(t, len(pairs), seen, "every claimed pair is submitted exactly once")
}

func TestPoolAbandonsBatchAfterFailedFlush(t *testing.T) {
	t.Parallel()

	src := &recordingSource{pending: distinctOriginPairs(t, 6), failFrom: 2}
	runPool(t, src, 2)

	// The first chunk lands, the second is rejected, and the worker gives up on the
	// claim rather than computing the remaining groups for a sink that just failed.
	require.Eventually(t, func() bool {
		return len(src.recorded()) == 2
	}, 5*time.Second, 10*time.Millisecond, "the pool flushes twice and stops")

	assert.Never(t, func() bool {
		return len(src.recorded()) > 2
	}, 250*time.Millisecond, 25*time.Millisecond, "the rest of the batch is abandoned, not computed")

	assert.Len(t, src.recorded()[0], 2, "the flush that succeeded before the failure stays durable")
}

func TestPoolDefaultsSubmitChunkToAQuarterOfTheBatch(t *testing.T) {
	t.Parallel()

	// runPool claims 100 at a time, so the derived chunk is 25 and a full batch
	// flushes four times. A fixed default equal to the batch size would flush once
	// and quietly disable incremental submission for the common configuration.
	src := &recordingSource{pending: distinctOriginPairs(t, 100)}
	runPool(t, src, 0)

	require.Eventually(t, func() bool {
		return len(src.recorded()) == 4
	}, 10*time.Second, 10*time.Millisecond, "a claimed batch flushes in quarters")

	for _, submit := range src.recorded() {
		assert.Len(t, submit, 25)
	}
}

func TestPoolFlushesShortBatchOnce(t *testing.T) {
	t.Parallel()

	// Fewer entries than a chunk still land — at the end, in one flush.
	src := &recordingSource{pending: distinctOriginPairs(t, 4)}
	runPool(t, src, 0)

	require.Eventually(t, func() bool {
		return len(src.recorded()) == 1
	}, 5*time.Second, 10*time.Millisecond, "one flush covers the whole batch")
	assert.Len(t, src.recorded()[0], 4)
}

func TestPoolComputesSeededPairs(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)

	pairs := testPairs(t, 5)
	require.NoError(t, index.Seed(ctx, pairs))

	pool := refresh.NewPool(oneEngine{engine: engine}, refresh.NewLocalSource(index, store), nil, refresh.Config{
		Workers:     2,
		Batch:       3,
		Lease:       time.Minute,
		IdleBackoff: 5 * time.Millisecond,
	})
	go pool.Run(ctx)

	require.Eventually(t, func() bool {
		debt, err := index.Debt(ctx)
		return err == nil && debt.Debt == 0 && store.Len() == len(pairs)
	}, 5*time.Second, 10*time.Millisecond, "the pool drains the seeded debt")

	stored, err := store.BatchGet(ctx, pairs)
	require.NoError(t, err)
	for _, s := range stored {
		require.NotNil(t, s)
		assert.Positive(t, s.Duration, "haversine over distinct cells yields a positive duration")
		assert.Positive(t, s.Distance)
		assert.False(t, s.ComputedAt.IsZero())
	}
}
