package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/control"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	"github.com/primandproper/beeline/internal/engine/registry"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/query"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/primandproper/platform-go/v10/observability/logging"
	metricsnoop "github.com/primandproper/platform-go/v10/observability/metrics/noop"
	tracingnoop "github.com/primandproper/platform-go/v10/observability/tracing/noop"
	chibackend "github.com/primandproper/platform-go/v10/routing/backends/chi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/uber/h3-go/v4"
)

// oneArea routes every coordinate into a single unbounded area at resolution 9.
type oneArea struct{}

func (oneArea) Locate(beeline.LatLng) (beeline.RoutedArea, bool) {
	return beeline.RoutedArea{ID: 1, Layers: []beeline.RoutedLayer{{Resolution: 9}}, TargetTTL: time.Minute}, true
}

// oneEngine serves every area through one engine.
type oneEngine struct{ engine beeline.RoutingEngine }

func (o oneEngine) EngineFor(beeline.AreaID) beeline.RoutingEngine { return o.engine }

// tableEnvelope mirrors the /table response wire shape for decoding in tests.
type tableEnvelope struct {
	Profile   string       `json:"profile"`
	Durations [][]*float64 `json:"durations"`
	Distances [][]*float64 `json:"distances"`
	Meta      struct {
		Cells     int `json:"cells"`
		Skipped   int `json:"skipped"`
		Hits      int `json:"hits"`
		Misses    int `json:"misses"`
		Filled    int `json:"filled"`
		SameCell  int `json:"sameCell"`
		OutOfArea int `json:"outOfArea"`
	} `json:"meta"`
}

func newTestRouter(t *testing.T) (http.Handler, *memstore.Store) {
	t.Helper()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	handler := query.NewHandler(store, index, oneEngine{engine: engine}, oneArea{}, nil, nil)

	router := httpapi.NewRouter(
		logging.EnsureLogger(nil),
		tracingnoop.NewTracerProvider(),
		metricsnoop.NewMetricsProvider(),
		&chibackend.Config{ServiceName: "test"},
	)
	httpapi.Register(router, &httpapi.Deps{Handler: handler, Store: store, DefaultProfile: "car"})

	return router.Handler(), store
}

func postTable(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/table", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestTableEndpointDenseMatricesAndSkip(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)

	// A 1×2 grid, demand-filling the first cell and skipping the second.
	body := `{
		"profile": "car",
		"sources": ["37.7749,-122.4194"],
		"destinations": ["37.7949,-122.4194", "37.7989,-122.4194"],
		"skip": [[0,1]]
	}`
	rec := postTable(t, h, body)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))

	var env tableEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))

	assert.Equal(t, "car", env.Profile)
	require.Len(t, env.Durations, 1)
	require.Len(t, env.Durations[0], 2)

	assert.NotNil(t, env.Durations[0][0], "the filled cell has a duration")
	assert.Positive(t, *env.Durations[0][0])
	assert.NotNil(t, env.Distances[0][0])
	assert.Nil(t, env.Durations[0][1], "the skipped cell is null")
	assert.Nil(t, env.Distances[0][1])

	assert.Equal(t, 2, env.Meta.Cells)
	assert.Equal(t, 1, env.Meta.Skipped)
	assert.Equal(t, 1, env.Meta.Misses)
	assert.Equal(t, 1, env.Meta.Filled)
}

func TestTableEndpointCacheOnlyLeavesMissesNull(t *testing.T) {
	t.Parallel()

	h, _ := newTestRouter(t)

	body := `{
		"sources": ["37.7749,-122.4194"],
		"destinations": ["37.7949,-122.4194"],
		"fill": false
	}`
	rec := postTable(t, h, body)
	require.Equal(t, http.StatusOK, rec.Code)

	var env tableEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))

	assert.Nil(t, env.Durations[0][0], "a cache-only miss is null")
	assert.Equal(t, 1, env.Meta.Misses)
	assert.Zero(t, env.Meta.Filled, "cache-only fills nothing")
}

// TestErrorBodyIsFlatForEveryFailure pins the one shape this API sends for an
// error: {"error": "<string>"}. The interesting case is the malformed body,
// which the typed router rejects during binding and so never reaches a handler
// or its fail() call. Before routing.WithErrorEncoder that path answered in the
// platform envelope — {"error": {"message": …, "code": …}, "details": …} — so
// "error" was an object on exactly one of this API's error paths, while the
// console and the follower client both read it as a string.
func TestErrorBodyIsFlatForEveryFailure(t *testing.T) {
	t.Parallel()

	cases := map[string]struct {
		body string
		want string
	}{
		"rejected by the router's binding": {
			body: `{"sources":`,
			want: "could not decode request body",
		},
		"rejected by the handler": {
			body: `{"sources": [], "destinations": ["37.7,-122.4"]}`,
			want: "sources and destinations must both be non-empty",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h, _ := newTestRouter(t)
			rec := postTable(t, h, tc.body)
			require.Equal(t, http.StatusBadRequest, rec.Code)

			// Decoding into a string-valued field is the assertion: an envelope
			// body fails here, which is the regression worth catching.
			var got struct {
				Error string `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got),
				"error body must decode with a string error field, got %s", rec.Body.String())
			assert.Equal(t, tc.want, got.Error)
		})
	}
}

func TestTableEndpointRejectsBadInput(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		"malformed json":      `{"sources":`,
		"empty sources":       `{"sources": [], "destinations": ["37.7,-122.4"]}`,
		"bad coordinate":      `{"sources": ["not-a-coord"], "destinations": ["37.7,-122.4"]}`,
		"skip out of range":   `{"sources": ["37.7,-122.4"], "destinations": ["37.8,-122.4"], "skip": [[0,5]]}`,
		"negative skip index": `{"sources": ["37.7,-122.4"], "destinations": ["37.8,-122.4"], "skip": [[-1,0]]}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h, _ := newTestRouter(t)
			rec := postTable(t, h, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s", name)
		})
	}
}

func postPairs(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/_ops_/pairs", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// pairsEnvelope mirrors the /_ops_/pairs response wire shape.
type pairsEnvelope struct {
	Pairs []struct {
		Dest           string  `json:"dest"`
		ComputedAt     string  `json:"computedAt"`
		DurationSec    float64 `json:"durationSec"`
		DistanceMeters float64 `json:"distanceMeters"`
	} `json:"pairs"`
}

func TestPairsEndpointReturnsOnlyCachedAtOwnResolution(t *testing.T) {
	t.Parallel()

	h, store := newTestRouter(t)

	// Two res-7 neighbor cells with a cached estimate — a coarser resolution than
	// the routed area's res 9, unreachable through /estimate or /table, but exactly
	// what the hover probe must see.
	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 7)
	require.NoError(t, err)
	cached, err := beeline.CellAt(beeline.LatLng{Lat: 37.85, Lng: -122.4194}, 7)
	require.NoError(t, err)
	uncached, err := beeline.CellAt(beeline.LatLng{Lat: 37.70, Lng: -122.4194}, 7)
	require.NoError(t, err)

	require.NoError(t, store.Put(context.Background(), []beeline.Entry{{
		Key:    beeline.PairKey{Area: 1, Origin: origin, Dest: cached, Profile: "car", Res: 7},
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 321, Distance: 4200}, ComputedAt: time.Now()},
	}}))

	body := `{"area": 1, "origin": "` + origin.String() + `", "dests": ["` +
		cached.String() + `", "` + uncached.String() + `"]}`
	rec := postPairs(t, h, body)
	require.Equal(t, http.StatusOK, rec.Code)

	var env pairsEnvelope
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env.Pairs, 1, "only the cached pair is returned")
	assert.Equal(t, cached.String(), env.Pairs[0].Dest)
	assert.InDelta(t, 321, env.Pairs[0].DurationSec, 1e-9)
	assert.InDelta(t, 4200, env.Pairs[0].DistanceMeters, 1e-9)
	assert.NotEmpty(t, env.Pairs[0].ComputedAt)
}

func TestPairsEndpointRejectsBadInput(t *testing.T) {
	t.Parallel()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 7)
	require.NoError(t, err)
	res9, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)

	cases := map[string]string{
		"malformed json": `{"area":`,
		"missing area":   `{"origin": "` + origin.String() + `", "dests": ["` + origin.String() + `"]}`,
		"empty dests":    `{"area": 1, "origin": "` + origin.String() + `", "dests": []}`,
		"invalid origin": `{"area": 1, "origin": "nope", "dests": ["` + origin.String() + `"]}`,
		"invalid dest":   `{"area": 1, "origin": "` + origin.String() + `", "dests": ["nope"]}`,
		"mixed-res dest": `{"area": 1, "origin": "` + origin.String() + `", "dests": ["` + res9.String() + `"]}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			h, _ := newTestRouter(t)
			rec := postPairs(t, h, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s", name)
		})
	}
}

// warmHarness is a router with a real coordinator behind it (the warm endpoint
// validates pairs against the enabled-area set, which the lighter newTestRouter
// fakes away), plus the shared index so tests can observe the warmed queue.
type warmHarness struct {
	handler http.Handler
	index   *memindex.Index
	areaID  beeline.AreaID
	origin  beeline.H3Cell
	dest    beeline.H3Cell
}

func newWarmHarness(t *testing.T) *warmHarness {
	t.Helper()

	ctx := context.Background()

	repo := memstore.NewRepository(nil)
	index := memindex.New(time.Minute, nil)
	store := memstore.New()

	engineSpeeds := map[beeline.Profile]float64{"car": 10}
	coord, err := control.New(&control.Config{
		Areas:     repo,
		Providers: repo,
		Index:     index,
		Store:     store,
		BuildEngine: func(spec *beeline.ProviderSpec) (beeline.RoutingEngine, error) {
			return registry.BuildEngine(spec, engineSpeeds)
		},
		Speeds:          map[string]float64{"car": 10},
		Builtins:        registry.BuiltinSpecs(false, 0, 0),
		DefaultProvider: config.DefaultProviderName,
		Defaults: control.FreshnessDefaults{
			TargetTTL:     time.Minute,
			LeaseDuration: 15 * time.Second,
			SweepInterval: time.Second,
		},
	})
	require.NoError(t, err)
	require.NoError(t, coord.InitProviders(ctx, nil))

	// A ~5 km square over SF: its res-8 polyfill comfortably contains the center
	// cell and its immediate neighbors.
	geo := `{"type":"Polygon","coordinates":[[[-122.45,37.75],[-122.39,37.75],[-122.39,37.80],[-122.45,37.80],[-122.45,37.75]]]}`
	area, err := coord.Create(ctx, &control.CreateAreaInput{
		Name:         "warm",
		WarmStrategy: beeline.WarmLazy,
		GeoJSON:      []byte(geo),
		Layers:       []beeline.Layer{{Resolution: 8, MaxRadiusMeters: 5000}},
	})
	require.NoError(t, err)
	_, err = coord.Enable(ctx, area.ID)
	require.NoError(t, err)

	center, err := beeline.CellAt(beeline.LatLng{Lat: 37.775, Lng: -122.42}, 8)
	require.NoError(t, err)
	disk, err := h3.GridDisk(center, 1)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(disk), 2)

	handler := query.NewHandler(store, index, coord, coord, nil, nil)
	router := httpapi.NewRouter(
		logging.EnsureLogger(nil),
		tracingnoop.NewTracerProvider(),
		metricsnoop.NewMetricsProvider(),
		&chibackend.Config{ServiceName: "test"},
	)
	httpapi.Register(router, &httpapi.Deps{Handler: handler, Store: store, Index: index, Coordinator: coord, DefaultProfile: "car"})

	return &warmHarness{handler: router.Handler(), index: index, areaID: area.ID, origin: center, dest: disk[1]}
}

func postWarm(t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, "/_ops_/warm", bytes.NewReader([]byte(body)))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

func TestWarmEndpointBumpsPredictedPairs(t *testing.T) {
	t.Parallel()

	h := newWarmHarness(t)

	body := `{"area": ` + strconv.FormatInt(int64(h.areaID), 10) +
		`, "pairs": [{"origin": "` + h.origin.String() + `", "dest": "` + h.dest.String() + `"}]}`
	rec := postWarm(t, h.handler, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Mode     string `json:"mode"`
		Accepted int    `json:"accepted"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Accepted)
	assert.Equal(t, "bump", resp.Mode, "bump is the default mode")

	// The warmed pair is the lazy area's only queue entry, claimable immediately.
	claimed, err := h.index.Claim(context.Background(), 10, 15*time.Second)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, beeline.PairKey{Area: h.areaID, Origin: h.origin, Dest: h.dest, Profile: "car", Res: 8}, claimed[0])
}

func TestWarmEndpointSeedMode(t *testing.T) {
	t.Parallel()

	h := newWarmHarness(t)

	body := `{"area": ` + strconv.FormatInt(int64(h.areaID), 10) + `, "mode": "seed",` +
		` "pairs": [{"origin": "` + h.origin.String() + `", "dest": "` + h.dest.String() + `"}]}`
	rec := postWarm(t, h.handler, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Mode string `json:"mode"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, "seed", resp.Mode)

	// Seed mode pins: a sweep far in the future removes nothing.
	removed, err := h.index.SweepArea(context.Background(), h.areaID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.Empty(t, removed)
}

func TestWarmEndpointRejectsBadInput(t *testing.T) {
	t.Parallel()

	h := newWarmHarness(t)
	goodPair := `{"origin": "` + h.origin.String() + `", "dest": "` + h.dest.String() + `"}`
	area := strconv.FormatInt(int64(h.areaID), 10)

	res9, err := beeline.CellAt(beeline.LatLng{Lat: 37.775, Lng: -122.42}, 9)
	require.NoError(t, err)
	outside, err := beeline.CellAt(beeline.LatLng{Lat: 0, Lng: 0}, 8)
	require.NoError(t, err)

	cases := map[string]string{
		"malformed json":  `{"area":`,
		"missing area":    `{"pairs": [` + goodPair + `]}`,
		"empty pairs":     `{"area": ` + area + `, "pairs": []}`,
		"unknown mode":    `{"area": ` + area + `, "mode": "sear", "pairs": [` + goodPair + `]}`,
		"invalid origin":  `{"area": ` + area + `, "pairs": [{"origin": "nope", "dest": "` + h.dest.String() + `"}]}`,
		"mixed-res pair":  `{"area": ` + area + `, "pairs": [{"origin": "` + h.origin.String() + `", "dest": "` + res9.String() + `"}]}`,
		"unknown area":    `{"area": 999, "pairs": [` + goodPair + `]}`,
		"outside cell":    `{"area": ` + area + `, "pairs": [{"origin": "` + outside.String() + `", "dest": "` + h.dest.String() + `"}]}`,
		"unknown profile": `{"area": ` + area + `, "profile": "hovercraft", "pairs": [` + goodPair + `]}`,
		"same-cell pair":  `{"area": ` + area + `, "pairs": [{"origin": "` + h.origin.String() + `", "dest": "` + h.origin.String() + `"}]}`,
	}

	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := postWarm(t, h.handler, body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s, got %s", name, rec.Body.String())
		})
	}
}

func postInvalidate(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodPost, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// TestInvalidateEndpointRequeuesLayer drives the endpoint end to end over the
// coordinator-backed harness: warm a pair, compute it, then invalidate the layer it
// belongs to and watch it become claimable again. Layer *scoping* across several
// resolutions is pinned in internal/control and the freshness conformance suite;
// what matters here is the wire contract, including that an empty body is the valid
// whole-area form.
func TestInvalidateEndpointRequeuesLayer(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newWarmHarness(t)
	area := strconv.FormatInt(int64(h.areaID), 10)

	body := `{"area": ` + area + `, "pairs": [{"origin": "` + h.origin.String() + `", "dest": "` + h.dest.String() + `"}]}`
	require.Equal(t, http.StatusOK, postWarm(t, h.handler, body).Code)

	claimed, err := h.index.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	require.NoError(t, h.index.MarkComputed(ctx, claimed, time.Now()))

	rec := postInvalidate(t, h.handler, "/_config_/areas/"+area+"/invalidate?resolution=8")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Resolution  *int   `json:"resolution"`
		Profile     string `json:"profile"`
		Area        int64  `json:"area"`
		Invalidated int    `json:"invalidated"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, int64(h.areaID), resp.Area)
	assert.Equal(t, 1, resp.Invalidated)
	require.NotNil(t, resp.Resolution)
	assert.Equal(t, 8, *resp.Resolution, "the response echoes the layer that was hit")
	assert.Empty(t, resp.Profile, "an unscoped profile stays absent")

	due, err := h.index.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	assert.Equal(t, claimed, due, "the invalidated pair is due again")
}

func TestInvalidateEndpointWholeAreaAndScopes(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	h := newWarmHarness(t)
	area := strconv.FormatInt(int64(h.areaID), 10)

	body := `{"area": ` + area + `, "pairs": [{"origin": "` + h.origin.String() + `", "dest": "` + h.dest.String() + `"}]}`
	require.Equal(t, http.StatusOK, postWarm(t, h.handler, body).Code)
	claimed, err := h.index.Claim(ctx, 10, 15*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.index.MarkComputed(ctx, claimed, time.Now()))

	// No query string at all: every layer, every profile, no request body needed.
	rec := postInvalidate(t, h.handler, "/_config_/areas/"+area+"/invalidate")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp struct {
		Resolution  *int `json:"resolution"`
		Invalidated int  `json:"invalidated"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Invalidated)
	assert.Nil(t, resp.Resolution, "an unscoped invalidation reports no resolution")

	// Both scopes together: the area's only registered profile plus its only layer.
	require.NoError(t, h.index.MarkComputed(ctx, claimed, time.Now()))
	rec = postInvalidate(t, h.handler, "/_config_/areas/"+area+"/invalidate?profile=car&resolution=8")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, 1, resp.Invalidated)
}

func TestInvalidateEndpointRejectsBadInput(t *testing.T) {
	t.Parallel()

	h := newWarmHarness(t)
	area := strconv.FormatInt(int64(h.areaID), 10)

	cases := map[string]string{
		"non-numeric area":     "/_config_/areas/nope/invalidate",
		"unknown area":         "/_config_/areas/99999/invalidate",
		"resolution not a int": "/_config_/areas/" + area + "/invalidate?resolution=eight",
		"resolution too large": "/_config_/areas/" + area + "/invalidate?resolution=16",
		"resolution negative":  "/_config_/areas/" + area + "/invalidate?resolution=-1",
		"absent layer":         "/_config_/areas/" + area + "/invalidate?resolution=7",
		"unknown profile":      "/_config_/areas/" + area + "/invalidate?profile=hovercraft",
	}

	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := postInvalidate(t, h.handler, path)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s, got %s", name, rec.Body.String())
		})
	}
}
