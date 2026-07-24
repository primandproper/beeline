package cli

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/engine/registry"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/query"
	"github.com/primandproper/beeline/internal/refresh"
	memstore "github.com/primandproper/beeline/internal/store/memory"
	areasqlite "github.com/primandproper/beeline/internal/store/sqlite"
	"github.com/primandproper/beeline/internal/webui"

	"github.com/primandproper/platform-go/v4/healthcheck"
	chirouter "github.com/primandproper/platform-go/v4/routing/chi"
	serverhttp "github.com/primandproper/platform-go/v4/server/http"

	"github.com/spf13/cobra"
)

// serveShutdownTimeout bounds how long we wait for in-flight requests to drain.
const serveShutdownTimeout = 10 * time.Second

// newServeCommand returns the `serve` subcommand: it opens the SQLite area store,
// seeds the freshness index from any already-enabled areas, starts the background
// refresh loop, and serves the read path (`/estimate`), the freshness contract
// (`/_ops_/freshness`), and the area control plane (`/_config_/areas`) over HTTP. A
// fresh database has no areas, so nothing refreshes until one is created and enabled.
// It runs until the process receives SIGINT/SIGTERM.
func (a *application) newServeCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "serve",
		Short: "Precompute and serve the travel-time/distance matrix.",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return a.serve(cmd.Context())
		},
	}
}

// serve builds the whole pipeline from config + pillars and runs it.
func (a *application) serve(ctx context.Context) error {
	mcfg := a.cfg.Matrix

	// Profiles: convert the config's string→speed map to the domain's key type and
	// collect the profile list for tessellation.
	speeds := make(map[beeline.Profile]float64, len(mcfg.Profiles))
	profiles := make([]beeline.Profile, 0, len(mcfg.Profiles))
	for name, speed := range mcfg.Profiles {
		p := beeline.Profile(name)
		speeds[p] = speed
		profiles = append(profiles, p)
	}

	// Routing providers: the named engine registry the control plane resolves per area.
	// The built-in Haversine stand-in is always registered raw under the default name;
	// when EngineLatency is enabled, a separate "latent-haversine" provider is added that
	// pays a random [Min, Max] delay per Table call, modeling a network-bound engine an
	// area can opt into. Any configured providers — additional Haversine engines or real
	// OSRM endpoints — add further named entries an area can select.
	providers, err := registry.Build(mcfg.Providers, speeds, mcfg.EngineLatency)
	if err != nil {
		return err
	}
	if lat := mcfg.EngineLatency; lat.Enabled {
		a.log().WithValues(map[string]any{
			"provider": config.LatentHaversineProviderName,
			"min":      lat.Min.String(),
			"max":      lat.Max.String(),
		}).Info("registered simulated-network-latency routing provider")
	}

	store := memstore.New()
	index := memindex.New(mcfg.TargetTTL, nil)

	// Area store: SQLite-backed area definitions. Opening it runs migrations; a fresh
	// database has no areas, so nothing is seeded and the refresh pool idles until an
	// area is created and enabled via the control plane.
	db, err := areasqlite.Open(mcfg.DatabasePath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			a.log().Error("closing area database", closeErr)
		}
	}()
	repo := areasqlite.NewRepository(db, nil)

	// Control plane: owns the enabled-area set and drives per-area seed/unseed on the
	// shared index/store. It also routes read-path queries to the containing area. The
	// global freshness knobs seed a new area's per-area contract when the operator leaves
	// them unset; each area then stores and honors its own values.
	coordinator := control.New(repo, index, store, profiles, providers, config.DefaultProviderName, control.FreshnessDefaults{
		TargetTTL:     mcfg.TargetTTL,
		LeaseDuration: mcfg.LeaseDuration,
		SweepInterval: mcfg.SweepInterval,
	})

	// Read path: routes each query through the coordinator to the enabled area that
	// contains it (and its resolution); staleness is judged against that area's own TTL.
	handler := query.NewHandler(store, index, coordinator, coordinator, a.logger)

	// Seed the areas that were already enabled in a prior run.
	if err = coordinator.ResumeEnabled(ctx); err != nil {
		return err
	}

	enabled := coordinator.EnabledAreas()
	a.log().WithValues(map[string]any{
		"database":      mcfg.DatabasePath,
		"enabled_areas": len(enabled),
		"profiles":      len(profiles),
		"target_ttl":    mcfg.TargetTTL.String(),
		"workers":       mcfg.RefreshWorkers,
		"listen_port":   mcfg.Server.Port,
	}).Info("area store opened; starting refresh and HTTP server")

	// HTTP router + server, built from the observability pillars.
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

	httpapi.Register(router, &httpapi.Deps{
		Handler:        handler,
		Index:          index,
		Store:          store,
		Coordinator:    coordinator,
		Health:         healthcheck.NewRegistry(),
		Logger:         a.logger,
		DefaultProfile: beeline.Profile(mcfg.DefaultProfile),
	})

	// Serve the embedded operator console at / (talks to the endpoints above).
	if err = webui.Register(router, a.logger); err != nil {
		return err
	}

	srv, err := serverhttp.NewHTTPServer(
		mcfg.Server,
		a.logger,
		router,
		a.pillars.TracerProvider,
		a.cfg.Observability.Logging.ServiceName,
	)
	if err != nil {
		return err
	}

	// Start the background refresh loop and the HTTP server. Serve() blocks and
	// panics on a bind/serve error, so run it in its own goroutine and coordinate
	// shutdown through the signal-cancellable context.
	pool := refresh.NewPool(coordinator, store, index, a.logger, refresh.Config{
		Workers: mcfg.RefreshWorkers,
		Batch:   mcfg.RefreshBatch,
		Lease:   mcfg.LeaseDuration,
	})

	go pool.Run(ctx)
	go srv.Serve()

	// Demand-decay janitor: ticks on a fixed base cadence and asks the coordinator to
	// sweep each enabled area that is due on its own SweepInterval, evicting cold demand
	// pairs (unqueried past their DemandIdleTTL) from the index and store so cost tracks
	// real usage. Areas with decay disabled (TTL 0) are skipped inside the sweep.
	go a.runSweeper(ctx, coordinator)

	<-ctx.Done()
	a.log().Info("shutdown signal received; draining HTTP server")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}

// sweepBaseTick is how often the janitor wakes to check which areas are due for a demand-
// decay sweep. Each area is only swept once its own SweepInterval has elapsed (enforced in
// SweepDue), so this base tick just needs to be at least as fine as the smallest per-area
// interval; a second is well below any realistic decay cadence and the check is cheap.
const sweepBaseTick = time.Second

// runSweeper ticks on the fixed base cadence and asks the coordinator to sweep every
// enabled area that is due on its own SweepInterval. It runs until ctx is cancelled.
func (a *application) runSweeper(ctx context.Context, coordinator *control.Coordinator) {
	ticker := time.NewTicker(sweepBaseTick)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			swept, err := coordinator.SweepDue(ctx, time.Now())
			if err != nil {
				a.log().Error("sweeping cold demand pairs", err)
				continue
			}
			if swept > 0 {
				a.log().WithValues(map[string]any{"swept": swept}).Debug("demand-decay sweep evicted cold pairs")
			}
		}
	}
}
