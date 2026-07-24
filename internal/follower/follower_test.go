package follower_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	"github.com/primandproper/beeline/internal/engine/registry"
	"github.com/primandproper/beeline/internal/follower"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/refresh"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/primandproper/platform-go/v4/observability/logging"
	metricsnoop "github.com/primandproper/platform-go/v4/observability/metrics/noop"
	tracingnoop "github.com/primandproper/platform-go/v4/observability/tracing/noop"
	chirouter "github.com/primandproper/platform-go/v4/routing/chi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testFallback() beeline.RoutingEngine {
	return haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
}

func newFollowerWith(t *testing.T, leaderURL string, fallback beeline.RoutingEngine) *follower.Follower {
	t.Helper()

	f, err := follower.New(follower.Config{
		LeaderURL:    leaderURL,
		Fallback:     fallback,
		BuildEngines: registry.BuildAll,
	}, nil)
	require.NoError(t, err)

	return f
}

func newFollower(t *testing.T, leaderURL string) *follower.Follower {
	t.Helper()

	return newFollowerWith(t, leaderURL, testFallback())
}

func TestNewValidates(t *testing.T) {
	t.Parallel()

	_, err := follower.New(follower.Config{Fallback: testFallback(), BuildEngines: registry.BuildAll}, nil)
	require.Error(t, err, "a leader URL is required")

	_, err = follower.New(follower.Config{LeaderURL: "http://localhost:1", BuildEngines: registry.BuildAll}, nil)
	require.Error(t, err, "a fallback engine is required")

	_, err = follower.New(follower.Config{LeaderURL: "http://localhost:1", Fallback: testFallback()}, nil)
	require.Error(t, err, "an engines builder is required")
}

func TestClaimParsesPairsAndProviderMetadata(t *testing.T) {
	t.Parallel()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)
	dest, err := beeline.CellAt(beeline.LatLng{Lat: 37.70, Lng: -122.4194}, 9)
	require.NoError(t, err)

	// A hash-less leader (pre-catalog protocol): no sync is attempted.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/_work_/claim", r.URL.Path)
		var req map[string]int
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		assert.Equal(t, 16, req["batchSize"])
		assert.Equal(t, 45, req["leaseSeconds"])

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"leaseSeconds": 45,
			"pairs": []map[string]any{{
				"profile": "car", "origin": origin.String(), "dest": dest.String(), "area": 7, "res": 9,
			}},
			"areas": map[string]any{"7": map[string]string{"routingProvider": "special"}},
		}))
	}))
	defer srv.Close()

	fallback := testFallback()
	f := newFollowerWith(t, srv.URL, fallback)

	keys, err := f.Claim(context.Background(), 16, 45*time.Second)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, beeline.PairKey{Area: 7, Origin: origin, Dest: dest, Profile: "car", Res: 9}, keys[0])

	// Area 7 names a provider this follower lacks → fallback engine.
	assert.Same(t, fallback, f.EngineFor(7), "unknown provider falls back to the default engine")
	assert.Same(t, fallback, f.EngineFor(99), "never-claimed area uses the default engine")
}

// catalogLeader stubs a leader whose claim responses carry the given provider-
// catalog hash and whose /_work_/providers serves the catalog. fetches counts
// catalog requests.
func catalogLeader(t *testing.T, origin, dest beeline.H3Cell, hash *atomic.Value, fetches *atomic.Int64) *httptest.Server {
	t.Helper()

	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		currentHash, _ := hash.Load().(string)
		switch r.URL.Path {
		case "/_work_/claim":
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"leaseSeconds":  30,
				"providersHash": currentHash,
				"pairs": []map[string]any{{
					"profile": "car", "origin": origin.String(), "dest": dest.String(), "area": 7, "res": 9,
				}},
				"areas": map[string]any{"7": map[string]string{"routingProvider": "osrm-x"}},
			}))
		case "/_work_/providers":
			fetches.Add(1)
			require.NoError(t, json.NewEncoder(w).Encode(beeline.ProviderCatalog{
				Hash:   currentHash,
				Speeds: map[string]float64{"car": 10},
				Providers: []beeline.ProviderSpec{
					{Name: beeline.DefaultProviderName, Type: beeline.ProviderTypeHaversine},
					// haversine-typed but named osrm-x: the max table size is a
					// marker the test reads back through Capabilities.
					{Name: "osrm-x", Type: beeline.ProviderTypeHaversine, MaxTableSize: 123},
				},
			}))
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestClaimSyncsProviderCatalogOnHashChange(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)
	dest, err := beeline.CellAt(beeline.LatLng{Lat: 37.70, Lng: -122.4194}, 9)
	require.NoError(t, err)

	var hash atomic.Value
	hash.Store("v1")
	var fetches atomic.Int64
	srv := catalogLeader(t, origin, dest, &hash, &fetches)
	defer srv.Close()

	f := newFollower(t, srv.URL)

	_, err = f.Claim(ctx, 4, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), fetches.Load(), "an unseen hash triggers a catalog fetch")
	assert.Equal(t, 123, f.EngineFor(7).Capabilities().MaxTableSize,
		"the claimed area resolves through the synced engine")

	_, err = f.Claim(ctx, 4, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(1), fetches.Load(), "a matching hash does not refetch")

	hash.Store("v2")
	_, err = f.Claim(ctx, 4, time.Minute)
	require.NoError(t, err)
	assert.Equal(t, int64(2), fetches.Load(), "a changed hash refetches the catalog")
}

func TestClaimFailsWhenCatalogSyncFails(t *testing.T) {
	t.Parallel()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/_work_/claim":
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
				"leaseSeconds":  30,
				"providersHash": "unfetchable",
				"pairs": []map[string]any{{
					"profile": "car", "origin": origin.String(), "dest": origin.String(), "area": 1, "res": 9,
				}},
			}))
		default:
			http.Error(w, `{"error":"boom"}`, http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	_, err = newFollower(t, srv.URL).Claim(context.Background(), 1, time.Minute)
	require.Error(t, err, "computing with a stale registry is refused; the leases expire back into the queue")
	assert.Contains(t, err.Error(), "provider catalog")
}

func TestClaimRejectsInvalidCells(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"pairs":[{"profile":"car","origin":"nonsense","dest":"junk","area":1,"res":9}]}`))
	}))
	defer srv.Close()

	_, err := newFollower(t, srv.URL).Claim(context.Background(), 1, time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cell")
}

func TestSubmitErrorsOnNon200(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"nope"}`, http.StatusBadRequest)
	}))
	defer srv.Close()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)

	entries := []beeline.Entry{{
		Key:    beeline.PairKey{Area: 1, Origin: origin, Dest: origin, Profile: "car", Res: 9},
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 1}},
	}}
	submitErr := newFollower(t, srv.URL).Submit(context.Background(), entries)
	require.Error(t, submitErr)
	assert.Contains(t, submitErr.Error(), "status 400")
}

func TestClaimErrorsWhenLeaderUnreachable(t *testing.T) {
	t.Parallel()

	// A closed server: connection refused.
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	_, err := newFollower(t, url).Claim(context.Background(), 1, time.Second)
	require.Error(t, err)
}

func TestHealthEndpoints(t *testing.T) {
	t.Parallel()

	leader := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/_ops_/live" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))

	f := newFollower(t, leader.URL)

	router := chirouter.NewRouter(
		logging.EnsureLogger(nil),
		tracingnoop.NewTracerProvider(),
		metricsnoop.NewMetricsProvider(),
		&chirouter.Config{ServiceName: "test"},
	)
	follower.RegisterHealth(router, f, nil)
	h := router.Handler()

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	assert.Equal(t, http.StatusOK, get("/_ops_/live").Code)
	assert.Equal(t, http.StatusOK, get("/_ops_/ready").Code, "leader reachable")

	leader.Close()
	assert.Equal(t, http.StatusServiceUnavailable, get("/_ops_/ready").Code, "leader gone")
}

// TestEndToEndFollowerDrainsLeaderQueue wires the REAL leader handlers (memory
// index+store behind httpapi.Register) into an httptest server and points a real
// Follower + refresh.Pool at it: the full claim→compute→submit protocol, both
// sides production code. This is also the guard against the follower's wire
// structs drifting from httpapi's.
func TestEndToEndFollowerDrainsLeaderQueue(t *testing.T) {
	t.Parallel()

	ctx := t.Context()

	index := memindex.New(time.Minute, nil)
	store := memstore.New()

	router := chirouter.NewRouter(
		logging.EnsureLogger(nil),
		tracingnoop.NewTracerProvider(),
		metricsnoop.NewMetricsProvider(),
		&chirouter.Config{ServiceName: "leader"},
	)
	httpapi.Register(router, &httpapi.Deps{
		Index:          index,
		Store:          store,
		DefaultProfile: "car",
		RefreshBatch:   8,
		LeaseDuration:  30 * time.Second,
	})
	leader := httptest.NewServer(router.Handler())
	defer leader.Close()

	// Seed the leader's queue.
	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)
	pairs := make([]beeline.PairKey, 0, 6)
	for i := range 6 {
		dest, destErr := beeline.CellAt(beeline.LatLng{Lat: 37.70 + float64(i)*0.01, Lng: -122.4194}, 9)
		require.NoError(t, destErr)
		pairs = append(pairs, beeline.PairKey{Area: 1, Origin: origin, Dest: dest, Profile: "car", Res: 9})
	}
	require.NoError(t, index.Seed(ctx, pairs))

	f := newFollower(t, leader.URL)
	pool := refresh.NewPool(f, f, nil, refresh.Config{
		Workers:     2,
		Batch:       4,
		Lease:       time.Minute,
		IdleBackoff: 5 * time.Millisecond,
	})
	go pool.Run(ctx)

	require.Eventually(t, func() bool {
		debt, debtErr := index.Debt(ctx)
		return debtErr == nil && debt.Debt == 0 && store.Len() == len(pairs)
	}, 5*time.Second, 10*time.Millisecond, "the follower drains the leader's queue over HTTP")

	stored, err := store.BatchGet(ctx, pairs)
	require.NoError(t, err)
	for _, s := range stored {
		require.NotNil(t, s)
		assert.Positive(t, s.Duration)
		assert.Positive(t, s.Distance)
	}
}
