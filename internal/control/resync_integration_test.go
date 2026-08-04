package control_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/engine/registry"
	pgfresh "github.com/primandproper/beeline/internal/freshness/postgres"
	pgstore "github.com/primandproper/beeline/internal/store/postgres"
	"github.com/primandproper/beeline/internal/store/postgres/pgtest"

	"github.com/primandproper/platform-go/v9/database"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// downtownGeoJSON is the demo's Downtown Austin polygon; downtownPoint sits
// inside it.
const downtownGeoJSON = `{"type":"Polygon","coordinates":[[
  [-97.7562560714113, 30.2666553730407],
  [-97.749885173004, 30.283821900995793],
  [-97.73046473182343, 30.27869488249841],
  [-97.73766667762762, 30.257482414343396],
  [-97.7562560714113, 30.2666553730407]]]}`

var downtownPoint = beeline.LatLng{Lat: 30.27, Lng: -97.745}

// newPgCoordinator wires a full Coordinator over the shared pool — the same
// composition serve.go builds in distributed mode. Two of these over one pool
// model two heads.
func newPgCoordinator(t *testing.T, db database.Client) *control.Coordinator {
	t.Helper()

	c, _ := newPgHead(t, db)

	return c
}

// newPgHead is newPgCoordinator plus the head's index handle, for tests that
// drive claims/marks directly.
func newPgHead(t *testing.T, db database.Client) (*control.Coordinator, *pgfresh.Index) {
	t.Helper()

	pool, err := pgstore.Pool(db)
	require.NoError(t, err)

	locker, err := pgstore.NewAdvisoryLocker(db, nil, nil, nil)
	require.NoError(t, err)

	speeds := map[string]float64{"car": 13.9}
	idx, err := pgfresh.New(pool, &pgfresh.Config{TargetTTL: time.Minute}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, idx.Close(context.Background())) })

	c, err := control.New(&control.Config{
		Areas:     pgstore.NewRepository(pool, nil),
		Providers: pgstore.NewRepository(pool, nil),
		Index:     idx,
		Store:     pgstore.NewEstimateStore(pool),
		Locker:    locker,
		BuildEngine: func(spec *beeline.ProviderSpec) (beeline.RoutingEngine, error) {
			return registry.BuildEngine(spec, map[beeline.Profile]float64{"car": 13.9})
		},
		Speeds:          speeds,
		DefaultProvider: beeline.DefaultProviderName,
		Builtins:        registry.BuiltinSpecs(false, 0, 0),
		Defaults: control.FreshnessDefaults{
			TargetTTL:     time.Minute,
			LeaseDuration: 30 * time.Second,
			SweepInterval: 30 * time.Second,
		},
	})
	require.NoError(t, err)
	require.NoError(t, c.InitProviders(context.Background(), nil))

	return c, idx
}

// TestBootPreservesSharedStateAcrossHeadRestarts drives multi-head boot: after
// head A enabled an area and real work landed, freshly booting heads must
// observe the populated shared index and only project — the working set is not
// re-pinned and the throughput baseline is not reset — even when two heads
// boot at once (serialized by the boot-seed lock).
func TestBootPreservesSharedStateAcrossHeadRestarts(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := pgtest.OpenClient(t)
	headA, idxA := newPgHead(t, db)

	area, err := headA.Create(ctx, &control.CreateAreaInput{
		Name:    "downtown",
		GeoJSON: []byte(downtownGeoJSON),
		Layers:  []beeline.Layer{{Resolution: 9}},
	})
	require.NoError(t, err)
	_, err = headA.Enable(ctx, area.ID)
	require.NoError(t, err)

	claimed, err := idxA.Claim(ctx, 50, time.Minute)
	require.NoError(t, err)
	require.NotEmpty(t, claimed)
	require.NoError(t, idxA.MarkComputed(ctx, claimed, time.Now()))

	before, err := headA.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	require.Positive(t, before.AchievedThroughput, "work landed against the enable-time baseline")

	headB := newPgCoordinator(t, db)
	headC := newPgCoordinator(t, db)

	var wg sync.WaitGroup
	for _, head := range []*control.Coordinator{headB, headC} {
		wg.Go(func() {
			assert.NoError(t, head.ResumeEnabled(ctx))
		})
	}
	wg.Wait()

	after, err := headB.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Equal(t, before.WorkingSet, after.WorkingSet, "booting heads must not grow or shrink the working set")
	assert.Positive(t, after.AchievedThroughput, "booting heads must not reset the throughput baseline")

	for _, head := range []*control.Coordinator{headB, headC} {
		routed, ok := head.Locate(downtownPoint)
		require.True(t, ok, "every booted head routes the enabled area")
		assert.Equal(t, area.ID, routed.ID)
	}
}

// TestTwoHeadsConvergeOnAreaLifecycle drives the cross-head convergence
// contract: an Enable handled by head A becomes routable on head B after B's
// resync (the watcher's job), and a Disable stops routing on B the same way —
// without B ever touching the shared index seed.
func TestTwoHeadsConvergeOnAreaLifecycle(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := pgtest.OpenClient(t)
	headA := newPgCoordinator(t, db)
	headB := newPgCoordinator(t, db)

	area, err := headA.Create(ctx, &control.CreateAreaInput{
		Name:    "downtown",
		GeoJSON: []byte(downtownGeoJSON),
		Layers:  []beeline.Layer{{Resolution: 9}},
	})
	require.NoError(t, err)

	_, err = headA.Enable(ctx, area.ID)
	require.NoError(t, err)

	_, routed := headB.Locate(downtownPoint)
	assert.False(t, routed, "head B has not resynced yet")

	require.NoError(t, headB.ResyncAreas(ctx))

	routedArea, routed := headB.Locate(downtownPoint)
	require.True(t, routed, "after resync head B routes the area head A enabled")
	assert.Equal(t, area.ID, routedArea.ID)
	assert.True(t, headB.AreaEnabled(area.ID))

	// The shared index was seeded exactly once (by head A): the area's debt
	// reads the same from both heads and the resync did not reset the baseline.
	statsA, err := headA.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	statsB, err := headB.DebtForArea(ctx, area.ID)
	require.NoError(t, err)
	assert.Equal(t, statsA.WorkingSet, statsB.WorkingSet)
	assert.Positive(t, statsB.WorkingSet, "head A's Enable seeded the shared index")

	_, err = headA.Disable(ctx, area.ID)
	require.NoError(t, err)
	require.NoError(t, headB.ResyncAreas(ctx))

	_, routed = headB.Locate(downtownPoint)
	assert.False(t, routed, "after resync head B stopped routing the disabled area")
	assert.False(t, headB.AreaEnabled(area.ID))
}

// TestTwoHeadsConvergeOnProviders drives the provider half: a PutProvider on
// head A changes the shared registry and catalog hash; head B converges after
// ResyncProviders, and a delete converges the same way.
func TestTwoHeadsConvergeOnProviders(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := pgtest.OpenClient(t)
	headA := newPgCoordinator(t, db)
	headB := newPgCoordinator(t, db)

	require.Equal(t, headA.ProvidersHash(), headB.ProvidersHash(), "fresh heads agree on the built-in catalog")

	_, err := headA.PutProvider(ctx, &beeline.ProviderSpec{
		Name:    "osrm-test",
		Type:    beeline.ProviderTypeOSRM,
		BaseURL: "http://osrm.invalid:5000",
	})
	require.NoError(t, err)
	assert.NotEqual(t, headA.ProvidersHash(), headB.ProvidersHash(), "head B still holds the old catalog")

	require.NoError(t, headB.ResyncProviders(ctx))
	assert.Equal(t, headA.ProvidersHash(), headB.ProvidersHash(), "resync converged the catalog hash")

	require.NoError(t, headA.DeleteProvider(ctx, "osrm-test"))
	require.NoError(t, headB.ResyncProviders(ctx))
	assert.Equal(t, headA.ProvidersHash(), headB.ProvidersHash(), "delete converged too")
}

// TestConfigGenerationsMoveOnMutation pins the watcher's poll signal: every
// control-plane mutation bumps its kind's generation in the same transaction.
func TestConfigGenerationsMoveOnMutation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	db := pgtest.OpenClient(t)
	head := newPgCoordinator(t, db)

	pool, err := pgstore.Pool(db)
	require.NoError(t, err)
	repo := pgstore.NewRepository(pool, nil)

	before, err := repo.ConfigGenerations(ctx)
	require.NoError(t, err)

	area, err := head.Create(ctx, &control.CreateAreaInput{
		Name:    "downtown",
		GeoJSON: []byte(downtownGeoJSON),
		Layers:  []beeline.Layer{{Resolution: 9}},
	})
	require.NoError(t, err)
	_, err = head.Enable(ctx, area.ID)
	require.NoError(t, err)

	after, err := repo.ConfigGenerations(ctx)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, after[pgstore.ConfigKindAreas], before[pgstore.ConfigKindAreas]+2,
		"create and enable each bumped the areas generation")
	assert.Equal(t, before[pgstore.ConfigKindProviders], after[pgstore.ConfigKindProviders],
		"area mutations do not tick the providers channel")
}
