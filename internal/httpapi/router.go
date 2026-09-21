package httpapi

import (
	"github.com/primandproper/beeline/version"

	"github.com/primandproper/primitives-go/v2/observability/logging"
	"github.com/primandproper/primitives-go/v2/observability/metrics"
	"github.com/primandproper/primitives-go/v2/observability/tracing"
	"github.com/primandproper/primitives-go/v2/routing"
	chibackend "github.com/primandproper/primitives-go/v2/routing/backends/chi"
)

// NewRouter builds beeline's router: the chi backend under the typed OpenAPI
// router, encoding through wireEncoder and enveloping disabled so response
// bodies stay this API's bare JSON. Callers register routes, then must check
// router.Err() before serving.
func NewRouter(
	logger logging.Logger,
	tracerProvider tracing.Provider,
	metricsProvider metrics.Provider,
	cfg *chibackend.Config,
	opts ...routing.RouterOption,
) *routing.Router {
	backend := chibackend.NewBackend(cfg,
		chibackend.WithLogger(logger),
		chibackend.WithTracerProvider(tracerProvider),
		chibackend.WithMetricsProvider(metricsProvider),
	)
	router := routing.New(backend, newWireEncoder(logger),
		append([]routing.RouterOption{
			routing.WithLogger(logger),
			routing.WithTracerProvider(tracerProvider),
			routing.WithTitle("Beeline"),
			routing.WithVersion(version.CommitHash),
			routing.WithDefaultEnvelope(false),
			routing.WithErrorEncoder(encodeError),
		}, opts...)...)

	// The escape hatch every typed handler's error path depends on; must precede
	// route registration.
	router.Use(wireMiddleware())

	return router
}
