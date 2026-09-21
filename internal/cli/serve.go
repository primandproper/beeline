package cli

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/engine/registry"
	"github.com/primandproper/beeline/internal/httpapi"
	"github.com/primandproper/beeline/internal/query"
	"github.com/primandproper/beeline/internal/refresh"
	pgstore "github.com/primandproper/beeline/internal/store/postgres"
	"github.com/primandproper/beeline/internal/telemetry"
	"github.com/primandproper/beeline/internal/webui"

	"github.com/primandproper/primitives-go/v2/eventcapture/jsonl"
	chibackend "github.com/primandproper/primitives-go/v2/routing/backends/chi"
	serverhttp "github.com/primandproper/primitives-go/v2/server/http"

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

	// Built-in routing providers: the raw Haversine stand-in is always synthesized
	// under the default name; when EngineLatency is enabled, a separate
	// "latent-haversine" provider is added that pays a random [Min, Max] delay per
	// Table call, modeling a network-bound engine an area can opt into. Everything
	// else in the registry is operator-defined and lives in the database
	// (/_config_/providers), loaded by InitProviders below.
	builtins := registry.BuiltinSpecs(mcfg.EngineLatency.Enabled, mcfg.EngineLatency.Min, mcfg.EngineLatency.Max)
	if lat := mcfg.EngineLatency; lat.Enabled {
		a.log().WithValues(map[string]any{
			"provider": config.LatentHaversineProviderName,
			"min":      lat.Min.String(),
			"max":      lat.Max.String(),
		}).Info("registered simulated-network-latency routing provider")
	}

	// Backend selection: memory (the default) keeps everything in process; a
	// configured matrix.backend moves the hot store — and, in distributed mode,
	// the coordination state — into shared Postgres/Redis so multiple heads can
	// serve one working set.
	// Engine construction for this process: every network-backed provider gets
	// its own circuit breaker, so a dead OSRM server sheds load instead of
	// paying a client timeout on every refresh and read-path miss.
	engines := registry.Builder{Ctx: ctx, Logger: a.logger, Metrics: a.pillars.MetricsProvider, Breakers: true}

	backend, err := a.buildBackend(ctx, &mcfg)
	if err != nil {
		return err
	}
	defer backend.close()
	store, index := backend.store, backend.index

	// Control plane: owns the enabled-area set and drives per-area seed/unseed on the
	// shared index/store. It also routes read-path queries to the containing area and
	// owns the provider registry (built-ins + the database-backed operator entries,
	// published to followers as a content-hashed catalog). The global freshness knobs
	// seed a new area's per-area contract when the operator leaves them unset; each
	// area then stores and honors its own values. The area/provider repositories (and
	// the cross-head mutation locker) come from the backend: local SQLite single-node,
	// shared Postgres distributed.
	coordinator, err := control.New(&control.Config{
		Areas:     backend.areas,
		Providers: backend.providers,
		Locker:    backend.locker,
		Index:     index,
		Store:     store,
		BuildEngine: func(spec *beeline.ProviderSpec) (beeline.RoutingEngine, error) {
			return engines.BuildEngine(spec, speeds)
		},
		Speeds:          mcfg.Profiles,
		Builtins:        builtins,
		DefaultProvider: config.DefaultProviderName,
		Defaults: control.FreshnessDefaults{
			TargetTTL:     mcfg.TargetTTL,
			LeaseDuration: mcfg.LeaseDuration,
			SweepInterval: mcfg.SweepInterval,
		},
	})
	if err != nil {
		return err
	}

	// Load the operator-defined providers from the database. A legacy
	// matrix.providers block seeds an empty table once; after that the database is
	// authoritative and provider changes happen through /_config_/providers.
	seedSpecs := make([]beeline.ProviderSpec, 0, len(mcfg.Providers))
	for name := range mcfg.Providers {
		pc := mcfg.Providers[name]
		seedSpecs = append(seedSpecs, pc.Spec(name))
	}
	if err = coordinator.InitProviders(ctx, seedSpecs); err != nil {
		return err
	}

	// Query telemetry: when a capture channel is enabled, the read path tees every
	// in-area fetch to a recorder over a bounded, never-blocking buffer, and a flusher
	// goroutine writes raw events and/or aggregated demand counts through the JSONL
	// sink — the training data for a demand-prediction model (fed back via
	// POST /_ops_/warm). Disabled (the default), the handler gets a nil recorder and
	// the read path records nothing.
	var recorder *telemetry.Recorder
	var fetchRecorder query.FetchRecorder
	if tcfg := mcfg.Telemetry; tcfg.Enabled() {
		sink, sinkErr := jsonl.NewSink(&jsonl.Config{
			Path:     tcfg.Path,
			MaxBytes: tcfg.MaxFileBytes,
			MaxFiles: tcfg.MaxFiles,
		}, jsonl.WithLogger(a.logger))
		if sinkErr != nil {
			return sinkErr
		}
		recorder, err = telemetry.NewRecorder(sink, telemetry.Config{
			BufferSize:       tcfg.BufferSize,
			FlushInterval:    tcfg.FlushInterval,
			RawEnabled:       tcfg.RawEnabled,
			AggregateEnabled: tcfg.AggregateEnabled,
			AggregateBucket:  tcfg.AggregateBucket,
			AggregateMaxKeys: tcfg.AggregateMaxKeys,
		}, a.logger, a.pillars.MetricsProvider)
		if err != nil {
			return err
		}
		// Assigned only when non-nil so the interface itself stays nil when telemetry
		// is off (a typed-nil *Recorder would defeat the handler's nil check).
		fetchRecorder = recorder
		a.log().WithValues(map[string]any{
			"path":      tcfg.Path,
			"raw":       tcfg.RawEnabled,
			"aggregate": tcfg.AggregateEnabled,
		}).Info("query telemetry capture enabled")
	}

	// Read path: routes each query through the coordinator to the enabled area that
	// contains it (and its resolution); staleness is judged against that area's own TTL.
	handler := query.NewHandler(store, index, coordinator, coordinator, a.logger, fetchRecorder)

	// Seed the areas that were already enabled in a prior run.
	if err = coordinator.ResumeEnabled(ctx); err != nil {
		return err
	}

	enabled := coordinator.EnabledAreas()
	a.log().WithValues(map[string]any{
		"hot_store":     mcfg.Backend.EffectiveHotStore(),
		"enabled_areas": len(enabled),
		"profiles":      len(profiles),
		"target_ttl":    mcfg.TargetTTL.String(),
		"workers":       mcfg.RefreshWorkers,
		"listen_port":   mcfg.Server.Port,
	}).Info("area store opened; starting refresh and HTTP server")

	// HTTP router + server, built from the observability pillars.
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

	httpapi.Register(router, &httpapi.Deps{
		Handler:        handler,
		Index:          index,
		Store:          store,
		Coordinator:    coordinator,
		Health:         backend.health,
		Logger:         a.logger,
		DefaultProfile: beeline.Profile(mcfg.DefaultProfile),
		RefreshBatch:   mcfg.RefreshBatch,
		LeaseDuration:  mcfg.LeaseDuration,
	})

	// Serve the embedded operator console at / (talks to the endpoints above).
	if err = webui.Register(router, a.logger); err != nil {
		return err
	}

	// The generated spec for the typed routes above; /docs is a browser UI over it.
	router.MountOpenAPI("/openapi.json", "/docs")

	if err = router.Err(); err != nil {
		return err
	}

	srv, err := serverhttp.NewHTTPServer(
		ctx,
		&mcfg.Server,
		router,
		serverhttp.WithLogger(a.logger),
		serverhttp.WithTracerProvider(a.pillars.TracerProvider),
		serverhttp.WithServiceName(a.cfg.Observability.Logging.ServiceName),
	)
	if err != nil {
		return err
	}

	// Start the background refresh loop and the HTTP server. Serve(ctx) blocks and
	// returns a bind/serve failure, so run it in its own goroutine and fold its
	// error into the same select as the signal-cancellable context. With
	// RefreshWorkers 0 the local pool never starts and this instance is a pure
	// coordinator: it seeds and serves work over /_work_/ and relies on followers
	// for all compute.
	if mcfg.RefreshWorkers > 0 {
		pool := refresh.NewPool(coordinator, refresh.NewLocalSource(index, store), a.logger, refresh.Config{
			Workers: mcfg.RefreshWorkers,
			Batch:   mcfg.RefreshBatch,
			Lease:   mcfg.LeaseDuration,
		})
		go pool.Run(ctx)
	} else {
		a.log().Info("local refresh pool disabled; coordinator-only mode (followers do the computing)")
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// The telemetry flusher is deliberately not tied to ctx: it must keep consuming
	// while srv.Shutdown drains in-flight requests (which still record events), and
	// is stopped explicitly below once the server is fully drained.
	if recorder != nil {
		go recorder.Run()
	}

	// Demand-decay janitor: ticks on a fixed base cadence and asks the coordinator to
	// sweep each enabled area that is due on its own SweepInterval, evicting cold demand
	// pairs (unqueried past their DemandIdleTTL) from the index and store so cost tracks
	// real usage. Areas with decay disabled (TTL 0) are skipped inside the sweep.
	go a.runSweeper(ctx, coordinator, backend.sweepGate)

	// Cross-head config convergence: poll the shared config_version generations
	// and re-derive this head's projections when another head mutates areas or
	// providers, so an Enable on one head routes on all heads within one poll
	// interval.
	go a.runConfigWatcher(ctx, coordinator, backend.configSource, mcfg.Backend.ConfigPollInterval)

	// A Serve failure (a bind error, most likely) must stop the process just as a
	// signal does — swallowing it would leave a head that answers nothing.
	var srvFailure error
	select {
	case <-ctx.Done():
		a.log().Info("shutdown signal received; draining HTTP server")
	case srvFailure = <-serveErr:
		a.log().Error("HTTP server failed; shutting down", srvFailure)
	}

	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serveShutdownTimeout)
	defer cancel()

	err = srv.Shutdown(shutdownCtx)
	if srvFailure != nil {
		err = srvFailure
	}

	// Only after the server has drained (no more requests can record events): drain
	// the telemetry buffer, flush the aggregator, and close the sink, within what
	// remains of the shutdown budget. A drain failure is logged, not returned — it
	// must not mask a server shutdown error.
	if recorder != nil {
		if drainErr := recorder.Close(shutdownCtx); drainErr != nil {
			a.log().Error("draining telemetry recorder", drainErr)
		}
	}

	return err
}

// sweepBaseTick is how often the janitor wakes to check which areas are due for a demand-
// decay sweep. Each area is only swept once its own SweepInterval has elapsed (enforced in
// SweepDue), so this base tick just needs to be at least as fine as the smallest per-area
// interval; a second is well below any realistic decay cadence and the check is cheap.
const sweepBaseTick = time.Second

// defaultConfigPollInterval paces the config watcher when the knob is unset.
const defaultConfigPollInterval = 2 * time.Second

// runConfigWatcher polls the shared config_version generations and converges
// this head's projections when they move: areas (enable/disable/update
// elsewhere) re-project the routing snapshot, providers rebuild engines and the
// catalog hash. The first tick resyncs unconditionally, closing the window
// between boot-time loading and the first observed generation. Poll-based by
// design — a 2s convergence lag matches the follower catalog-sync grain;
// LISTEN/NOTIFY can accelerate it later without changing the shape.
func (a *application) runConfigWatcher(ctx context.Context, coordinator *control.Coordinator, source *pgstore.Repository, interval time.Duration) {
	if interval <= 0 {
		interval = defaultConfigPollInterval
	}
	ticker := a.clock.NewTicker(interval)
	defer ticker.Stop()

	var last map[string]int64
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
		}

		generations, err := source.ConfigGenerations(ctx)
		if err != nil {
			if ctx.Err() == nil {
				a.log().Error("reading config generations", err)
			}
			continue
		}

		if last == nil || generations[pgstore.ConfigKindProviders] != last[pgstore.ConfigKindProviders] {
			if err = coordinator.ResyncProviders(ctx); err != nil {
				a.log().Error("resyncing providers", err)
				continue // retry next tick with last unchanged
			}
		}
		if last == nil || generations[pgstore.ConfigKindAreas] != last[pgstore.ConfigKindAreas] {
			if err = coordinator.ResyncAreas(ctx); err != nil {
				a.log().Error("resyncing areas", err)
				continue
			}
		}
		last = generations
	}
}

// runSweeper ticks on the fixed base cadence and asks the coordinator to sweep every
// enabled area that is due on its own SweepInterval. It runs until ctx is cancelled.
// With a gate (distributed mode), every head keeps ticking but only the tick's
// advisory-lock winner sweeps — per-tick election means zero session bookkeeping
// and automatic failover on the next tick after a holder dies. Sweeps are
// idempotent deletes, so the per-head lastSwept cadence state at worst costs one
// extra sweep after a failover.
func (a *application) runSweeper(
	ctx context.Context,
	coordinator *control.Coordinator,
	gate func(ctx context.Context, fn func(ctx context.Context) error) (bool, error),
) {
	ticker := a.clock.NewTicker(sweepBaseTick)
	defer ticker.Stop()

	sweep := func(ctx context.Context) error {
		swept, err := coordinator.SweepDue(ctx, a.clock.Now())
		if err != nil {
			return err
		}
		if swept > 0 {
			a.log().WithValues(map[string]any{"swept": swept}).Debug("demand-decay sweep evicted cold pairs")
		}

		return nil
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.Chan():
			// Not winning the election is normal: another head swept this tick.
			_, err := gate(ctx, sweep)
			if err != nil && ctx.Err() == nil {
				a.log().Error("sweeping cold demand pairs", err)
			}
		}
	}
}
