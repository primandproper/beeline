package httpapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/primandproper/beeline/internal/httpapi"

	"github.com/primandproper/platform-go/v10/healthcheck"
	metricsnoop "github.com/primandproper/platform-go/v10/observability/metrics/noop"
	tracingnoop "github.com/primandproper/platform-go/v10/observability/tracing/noop"
	chibackend "github.com/primandproper/platform-go/v10/routing/backends/chi"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubChecker is one named dependency with a fixed verdict.
type stubChecker struct {
	err  error
	name string
}

func (c stubChecker) Name() string                { return c.name }
func (c stubChecker) Check(context.Context) error { return c.err }

// healthHandler mounts the ops routes over a registry carrying checkers.
func healthHandler(t *testing.T, checkers ...healthcheck.Checker) http.Handler {
	t.Helper()

	registry, err := healthcheck.NewRegistry()
	require.NoError(t, err)

	for _, c := range checkers {
		registry.Register(c)
	}

	router := httpapi.NewRouter(nil, tracingnoop.NewTracerProvider(), metricsnoop.NewMetricsProvider(),
		&chibackend.Config{ServiceName: "test", SilenceRouteLogging: true})
	httpapi.Register(router, &httpapi.Deps{Health: registry, DefaultProfile: "car"})
	require.NoError(t, router.Err())

	return router.Handler()
}

func getHealth(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// TestReadyReportsHealthyDependencies pins the success shape: a registry whose
// checkers all pass answers 200 and names each component.
func TestReadyReportsHealthyDependencies(t *testing.T) {
	t.Parallel()

	rec := getHealth(t, healthHandler(t,
		stubChecker{name: "postgres"},
		stubChecker{name: "redis"},
	), "/_ops_/ready")

	require.Equal(t, http.StatusOK, rec.Code)

	var body healthcheck.Result
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, healthcheck.StatusUp, body.Status)
	assert.Equal(t, healthcheck.StatusUp, body.Components["postgres"].Status)
	assert.Equal(t, healthcheck.StatusUp, body.Components["redis"].Status)
}

// TestReadyReportsFailingDependency is the regression this phase exists for: a
// registry with no checkers used to report up unconditionally, so a head with a
// dead database still passed its readiness gate.
func TestReadyReportsFailingDependency(t *testing.T) {
	t.Parallel()

	rec := getHealth(t, healthHandler(t,
		stubChecker{name: "postgres", err: errors.New("connection refused")},
		stubChecker{name: "redis"},
	), "/_ops_/ready")

	require.Equal(t, http.StatusServiceUnavailable, rec.Code)

	var body healthcheck.Result
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.Equal(t, healthcheck.StatusDown, body.Status)
	assert.Equal(t, healthcheck.StatusDown, body.Components["postgres"].Status)
	assert.Equal(t, healthcheck.StatusUp, body.Components["redis"].Status, "a healthy sibling still reports up")
}

// TestLiveIgnoresDependencies keeps liveness a process-up signal: a dead
// dependency must not make an orchestrator restart an otherwise-running head.
func TestLiveIgnoresDependencies(t *testing.T) {
	t.Parallel()

	rec := getHealth(t, healthHandler(t,
		stubChecker{name: "postgres", err: errors.New("connection refused")},
	), "/_ops_/live")

	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"status":"up"}`, rec.Body.String())
}
