package cli

import (
	"context"
	"errors"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/engine/registry"
	"github.com/primandproper/beeline/internal/follower"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/refresh"

	circuitbreakingcfg "github.com/primandproper/platform-go/v10/circuitbreaking/config"
	"github.com/primandproper/platform-go/v10/httpclient"
	retrycfg "github.com/primandproper/platform-go/v10/retry/config"
	chibackend "github.com/primandproper/platform-go/v10/routing/backends/chi"
	serverhttp "github.com/primandproper/platform-go/v10/server/http"

	"github.com/spf13/cobra"
)

// LeaderURLEnvVar names the environment variable that seeds the --leader flag,
// matching the matrix.follower.leaderURL config path.
const LeaderURLEnvVar = config.EnvVarPrefix + "MATRIX_FOLLOWER_LEADER_URL"

// newWorkCommand returns the `work` subcommand: the same binary run as a follower.
// Pointed at a leader instance, it claims pending pairs over /_work_/claim,
// computes them with its local routing engines, and submits the results back over
// /_work_/submit — so an operator scales a saturated leader by simply starting
// more `work` processes. It holds no state of its own: no database, no store, no
// index — only the claim→compute→submit loop plus health probes.
func (a *application) newWorkCommand() *cobra.Command {
	var leaderURL string

	cmd := &cobra.Command{
		Use:   "work",
		Short: "Run as a follower: claim estimate work from a leader, compute it, submit it back.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.work(cmd.Context(), leaderURL)
		},
	}

	cmd.Flags().StringVar(&leaderURL, "leader", envOr(LeaderURLEnvVar, ""),
		"base URL of the leader instance (overrides matrix.follower.leaderURL)")

	return cmd
}

// work builds the follower pipeline from config + pillars and runs it until the
// process receives SIGINT/SIGTERM.
func (a *application) work(ctx context.Context, leaderURL string) error {
	mcfg := a.cfg.Matrix
	fcfg := mcfg.Follower

	if leaderURL == "" {
		leaderURL = fcfg.LeaderURL
	}
	if leaderURL == "" {
		return errors.New("a leader URL is required: pass --leader, set " + LeaderURLEnvVar + ", or configure matrix.follower.leaderURL")
	}

	// The follower's provider registry is synced from the leader: every claim
	// carries the leader's provider-catalog hash, and an unfamiliar hash makes the
	// follower fetch /_work_/providers and rebuild its engines (speeds included)
	// before computing. Locally we only construct the fallback engine used until
	// the first sync; any matrix.providers block in this config is leader-side
	// seed data and is deliberately ignored here.
	if len(mcfg.Providers) > 0 {
		a.log().Info("matrix.providers is ignored by the work subcommand; provider config syncs from the leader")
	}
	speeds := make(map[beeline.Profile]float64, len(mcfg.Profiles))
	for name, speed := range mcfg.Profiles {
		speeds[beeline.Profile(name)] = speed
	}
	engines := registry.Builder{Ctx: ctx, Logger: a.logger, Metrics: a.pillars.MetricsProvider, Breakers: true}

	fallbackSpec := beeline.ProviderSpec{Name: config.DefaultProviderName, Type: config.ProviderTypeHaversine}
	fallback, err := engines.BuildEngine(&fallbackSpec, speeds)
	if err != nil {
		return err
	}

	httpCfg := fcfg.HTTP
	httpCfg.EnsureDefaults()

	// A breaker on the leader client: once the leader stops answering, claims
	// fail immediately and every worker falls back to its idle backoff rather
	// than spending a full retry budget each cycle against a sick leader.
	breaker, err := circuitbreakingcfg.NewCircuitBreaker(ctx,
		&circuitbreakingcfg.Config{Name: "follower_leader"},
		circuitbreakingcfg.WithLogger(a.logger),
		circuitbreakingcfg.WithMetricsProvider(a.pillars.MetricsProvider))
	if err != nil {
		return err
	}

	outbound, err := httpclient.NewHTTPClient(httpCfg.Options()...)
	if err != nil {
		return err
	}

	retryPolicy, err := retrycfg.NewExponentialBackoffPolicy(fcfg.Retry)
	if err != nil {
		return err
	}

	f, err := follower.New(&follower.Config{
		LeaderURL:    leaderURL,
		Client:       outbound,
		Retry:        retryPolicy,
		Breaker:      breaker,
		Fallback:     fallback,
		BuildEngines: engines.BuildAll,
	}, a.logger)
	if err != nil {
		return err
	}

	// Worker knobs: follower-specific values win, zero falls back to the shared
	// refresh knobs so an untuned follower paces itself like a local worker. The
	// health port gets the built-in default when the config file omits the whole
	// follower block (the leader URL may have arrived via the flag alone); worker
	// count and idle backoff fall to NewPool's own floors.
	batch := fcfg.Batch
	if batch == 0 {
		batch = mcfg.RefreshBatch
	}
	lease := fcfg.Lease
	if lease == 0 {
		lease = mcfg.LeaseDuration
	}
	if fcfg.Port == 0 {
		fcfg.Port = 8081
	}

	pool := refresh.NewPool(f, f, a.logger, refresh.Config{
		Workers:     fcfg.Workers,
		Batch:       batch,
		Lease:       lease,
		IdleBackoff: fcfg.IdleBackoff,
	})

	// The follower's only HTTP surface: liveness + leader-reachability readiness.
	router := httpapi.NewRouter(
		a.logger,
		a.pillars.TracerProvider,
		a.pillars.MetricsProvider,
		&chibackend.Config{
			ServiceName:            a.cfg.Observability.Logging.ServiceName,
			EnableCORSForLocalhost: true,
			SilenceRouteLogging:    mcfg.SilenceRouteLogging,
		},
	)
	if err = follower.RegisterHealth(router, f, a.logger); err != nil {
		return err
	}

	if err = router.Err(); err != nil {
		return err
	}

	srv, err := serverhttp.NewHTTPServer(
		ctx,
		&serverhttp.Config{Port: fcfg.Port, StartupDeadline: 5 * time.Second},
		router,
		serverhttp.WithLogger(a.logger),
		serverhttp.WithTracerProvider(a.pillars.TracerProvider),
		serverhttp.WithServiceName(a.cfg.Observability.Logging.ServiceName),
	)
	if err != nil {
		return err
	}

	a.log().WithValues(map[string]any{
		"leader":       leaderURL,
		"workers":      fcfg.Workers,
		"batch":        batch,
		"lease":        lease.String(),
		"idle_backoff": fcfg.IdleBackoff.String(),
		"health_port":  fcfg.Port,
	}).Info("starting follower work loop")

	go pool.Run(ctx)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// A Serve failure (a bind error, most likely) must stop the process just as a
	// signal does — swallowing it would leave a follower with no health probes.
	var srvFailure error
	select {
	case <-ctx.Done():
		a.log().Info("shutdown signal received; stopping follower")
	case srvFailure = <-serveErr:
		a.log().Error("health server failed; stopping follower", srvFailure)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	// Drain regardless, but report the Serve failure that got us here in
	// preference to a shutdown error, since it is the actual cause.
	err = srv.Shutdown(shutdownCtx)
	if srvFailure != nil {
		return srvFailure
	}

	return err
}
