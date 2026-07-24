package cli

import (
	"context"
	"errors"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/engine/registry"
	"github.com/primandproper/beeline/internal/follower"
	"github.com/primandproper/beeline/internal/refresh"

	chirouter "github.com/primandproper/platform-go/v4/routing/chi"
	serverhttp "github.com/primandproper/platform-go/v4/server/http"

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

	// The follower computes with the same provider registry a leader would build
	// from this config, so a claimed area's provider name resolves to the same
	// engine here as it does on a leader running the same config file.
	speeds := make(map[beeline.Profile]float64, len(mcfg.Profiles))
	for name, speed := range mcfg.Profiles {
		speeds[beeline.Profile(name)] = speed
	}
	providers, err := registry.Build(mcfg.Providers, speeds, mcfg.EngineLatency)
	if err != nil {
		return err
	}

	httpCfg := fcfg.HTTP
	httpCfg.EnsureDefaults()
	f, err := follower.New(follower.Config{
		LeaderURL: leaderURL,
		Client:    httpCfg.BuildClient(),
	}, providers, config.DefaultProviderName, a.logger)
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
	router := chirouter.NewRouter(
		a.logger,
		a.pillars.TracerProvider,
		a.pillars.MetricsProvider,
		&chirouter.Config{
			ServiceName:            a.cfg.Observability.Logging.ServiceName,
			EnableCORSForLocalhost: true,
			SilenceRouteLogging:    mcfg.SilenceRouteLogging,
		},
	)
	follower.RegisterHealth(router, f, a.logger)

	srv, err := serverhttp.NewHTTPServer(
		serverhttp.Config{Port: fcfg.Port, StartupDeadline: 5 * time.Second},
		a.logger,
		router,
		a.pillars.TracerProvider,
		a.cfg.Observability.Logging.ServiceName,
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
	go srv.Serve()

	<-ctx.Done()
	a.log().Info("shutdown signal received; stopping follower")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}
