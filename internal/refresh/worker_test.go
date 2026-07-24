package refresh_test

import (
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
