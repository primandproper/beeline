package httpapi_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/query"
	memstore "github.com/primandproper/beeline/internal/store/memory"

	"github.com/primandproper/platform-go/v4/observability/logging"
	metricsnoop "github.com/primandproper/platform-go/v4/observability/metrics/noop"
	tracingnoop "github.com/primandproper/platform-go/v4/observability/tracing/noop"
	chirouter "github.com/primandproper/platform-go/v4/routing/chi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// oneArea routes every coordinate into a single unbounded area at resolution 9.
type oneArea struct{}

func (oneArea) Locate(beeline.LatLng) (beeline.RoutedArea, bool) {
	return beeline.RoutedArea{ID: 1, Resolution: 9, TargetTTL: time.Minute}, true
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

func newTestRouter(t *testing.T) http.Handler {
	t.Helper()

	store := memstore.New()
	index := memindex.New(time.Minute, nil)
	engine := haversineengine.New(map[beeline.Profile]float64{"car": 10}, 0)
	handler := query.NewHandler(store, index, oneEngine{engine: engine}, oneArea{}, nil)

	router := chirouter.NewRouter(
		logging.EnsureLogger(nil),
		tracingnoop.NewTracerProvider(),
		metricsnoop.NewMetricsProvider(),
		&chirouter.Config{ServiceName: "test"},
	)
	httpapi.Register(router, &httpapi.Deps{Handler: handler, DefaultProfile: "car"})

	return router.Handler()
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

	h := newTestRouter(t)

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

	h := newTestRouter(t)

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

			rec := postTable(t, newTestRouter(t), body)
			assert.Equal(t, http.StatusBadRequest, rec.Code, "expected 400 for %s", name)
		})
	}
}
