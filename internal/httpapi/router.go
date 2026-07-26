package httpapi

import (
	"github.com/primandproper/beeline/version"

	"github.com/primandproper/platform-go/v7/observability/logging"
	"github.com/primandproper/platform-go/v7/observability/metrics"
	"github.com/primandproper/platform-go/v7/observability/tracing"
	"github.com/primandproper/platform-go/v7/routing"
	chibackend "github.com/primandproper/platform-go/v7/routing/backends/chi"
)

// NewRouter builds beeline's router: the chi backend under the typed OpenAPI
// router, encoding through wireEncoder and enveloping disabled so response
// bodies stay this API's bare JSON. Callers register routes, then must check
// router.Err() before serving.
func NewRouter(
	logger logging.Logger,
	tracerProvider tracing.TracerProvider,
	metricsProvider metrics.Provider,
	cfg *chibackend.Config,
	opts ...routing.RouterOption,
) *routing.Router {
	backend := chibackend.NewBackend(logger, tracerProvider, metricsProvider, cfg)
	router := routing.New(backend, newWireEncoder(logger), logger, tracerProvider,
		append([]routing.RouterOption{
			routing.WithTitle("Beeline"),
			routing.WithVersion(version.CommitHash),
			routing.WithDefaultEnvelope(false),
		}, opts...)...)

	// The escape hatch every typed handler's error path depends on; must precede
	// route registration.
	router.Use(wireMiddleware())

	return router
}
