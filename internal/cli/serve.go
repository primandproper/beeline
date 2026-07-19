package cli

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/query"
	"github.com/primandproper/beeline/internal/refresh"
	memstore "github.com/primandproper/beeline/internal/store/memory"
	"github.com/primandproper/beeline/internal/tessellate"
	"github.com/primandproper/beeline/internal/webui"

	"github.com/primandproper/platform-go/v4/healthcheck"
	chirouter "github.com/primandproper/platform-go/v4/routing/chi"
	serverhttp "github.com/primandproper/platform-go/v4/server/http"

	"github.com/spf13/cobra"
)

// serveShutdownTimeout bounds how long we wait for in-flight requests to drain.
const serveShutdownTimeout = 10 * time.Second

// newServeCommand returns the `serve` subcommand: it tessellates the configured
// service area, seeds the freshness index, starts the background refresh loop, and
// serves the read path (`/estimate`) plus the freshness contract (`/_ops_/freshness`)
// over HTTP. It runs until the process receives SIGINT/SIGTERM.
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

	engine := haversineengine.New(speeds, 0)
	store := memstore.New()
	index := memindex.New(mcfg.TargetTTL, nil)

	// Read path.
	handler := query.NewHandler(store, index, engine, a.logger, mcfg.Area.Resolution, mcfg.TargetTTL)

	// Road mask (§7): if configured, cells with no road coverage are pruned from
	// every tessellation. An unset path disables it (full geometric disk).
	var mask *tessellate.Mask
	if mcfg.RoadMaskPath != "" {
		loaded, loadErr := tessellate.LoadMask(mcfg.RoadMaskPath)
		if loadErr != nil {
			return loadErr
		}
		mask = loaded
		a.log().WithValues(map[string]any{
			"path":       mcfg.RoadMaskPath,
			"cells":      mask.Len(),
			"resolution": mask.Resolution,
		}).Info("road mask loaded; pruning roadless cells")
	}

	// Control plane: owns the mutable service area and re-tessellates on demand
	// (from the web UI). The initial Apply seeds the index from the config's area,
	// replacing the old boot-time tessellate → Seed path (§7).
	coordinator := control.New(index, store, handler, profiles, mask)
	summary, err := coordinator.Apply(ctx, control.Area{
		Lat:         mcfg.Area.Lat,
		Lng:         mcfg.Area.Lng,
		Resolution:  mcfg.Area.Resolution,
		AreaRings:   mcfg.Area.AreaRings,
		RadiusRings: mcfg.Area.RadiusRings,
	})
	if err != nil {
		return err
	}

	a.log().WithValues(map[string]any{
		"cells":       summary.Cells,
		"pairs":       summary.Pairs,
		"profiles":    len(profiles),
		"resolution":  mcfg.Area.Resolution,
		"target_ttl":  mcfg.TargetTTL.String(),
		"workers":     mcfg.RefreshWorkers,
		"listen_port": mcfg.Server.Port,
	}).Info("service area tessellated; starting refresh and HTTP server")

	// HTTP router + server, built from the observability pillars.
	router := chirouter.NewRouter(
		a.logger,
		a.pillars.TracerProvider,
		a.pillars.MetricsProvider,
		&chirouter.Config{
			ServiceName:            a.cfg.Observability.Logging.ServiceName,
			EnableCORSForLocalhost: true,
		},
	)

	httpapi.Register(router, httpapi.Deps{
		Handler:        handler,
		Index:          index,
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
	pool := refresh.NewPool(engine, store, index, a.logger, refresh.Config{
		Workers: mcfg.RefreshWorkers,
		Batch:   mcfg.RefreshBatch,
		Lease:   mcfg.LeaseDuration,
	})

	go pool.Run(ctx)
	go srv.Serve()

	<-ctx.Done()
	a.log().Info("shutdown signal received; draining HTTP server")

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	return srv.Shutdown(shutdownCtx)
}
