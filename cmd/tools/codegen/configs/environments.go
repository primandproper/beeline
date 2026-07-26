package main

import (
	"time"

	"github.com/primandproper/beeline/internal/config"

	"github.com/primandproper/platform-go/v7/httpclient"
	"github.com/primandproper/platform-go/v7/observability"
	"github.com/primandproper/platform-go/v7/observability/logging"
	loggingcfg "github.com/primandproper/platform-go/v7/observability/logging/config"
	serverhttp "github.com/primandproper/platform-go/v7/server/http"
)

// Each builder returns a fully-formed *config.Config for one environment. The
// configs are constructed here as real, typed Go objects — the compiler and the
// config's own Validate method are the guardrails — and then Render projects
// them to the JSON files under config/. Grow a builder as the application's
// Config grows (database, HTTP server, real telemetry, ...); the leftover
// observability pillars (tracing, metrics, profiling) stay at their zero values,
// which platform-go resolves to noop providers.

// defaultProfiles are the routing modes and their constant speeds (m/s) the
// Haversine engine uses.
func defaultProfiles() map[string]float64 {
	return map[string]float64{
		"car":  13.9, // ~50 km/h
		"bike": 4.2,  // ~15 km/h
		"walk": 1.4,  // ~5 km/h
	}
}

// buildLocalDevConfig is the config a developer runs against locally: structured
// slog logging at debug, and a short freshness TTL so the debt signal moves quickly
// while iterating.
func buildLocalDevConfig() *config.Config {
	return &config.Config{
		Observability: observability.Config{
			Logging: loggingcfg.Config{
				Provider:    loggingcfg.ProviderSlog,
				ServiceName: config.DefaultServiceName,
				Level:       logging.DebugLevel,
			},
		},
		Matrix: config.MatrixConfig{
			Server: serverhttp.Config{
				Port:            8080,
				StartupDeadline: 5 * time.Second,
			},
			DatabasePath:   "beeline.db",
			Profiles:       defaultProfiles(),
			DefaultProfile: "car",
			// An example named provider: a local OSRM server. This block is seed data
			// only — it is imported into the SQLite provider registry the first time a
			// leader boots against an empty providers table, after which the database is
			// authoritative and providers change through /_config_/providers (followers
			// sync the registry from the leader, so nothing here reaches them). Nothing
			// routes through it until an area selects it (routingProvider: "osrm-local");
			// the built-in haversine default still serves every area that names no
			// provider.
			Providers: map[string]config.ProviderConfig{
				"osrm-local": {
					Type:         config.ProviderTypeOSRM,
					BaseURL:      "http://localhost:5000",
					Profiles:     map[string]string{"car": "driving", "bike": "cycling", "walk": "foot"},
					MaxTableSize: 10000,
					Timeout:      5 * time.Second,
				},
			},
			// Model the routing engine as network-bound locally, so the refresh pool
			// and read path can be watched under realistic I/O latency. Off in
			// production, where the in-process engine returns in nanoseconds.
			EngineLatency: config.EngineLatencyConfig{
				Enabled: true,
				Min:     25 * time.Millisecond,
				Max:     120 * time.Millisecond,
			},
			// Query telemetry stays OFF even locally — the capture pipeline is young.
			// The sink/buffer knobs are pre-filled so trying it is a single env-var
			// flip (BEELINE_MATRIX_TELEMETRY_RAW_ENABLED=true and/or
			// …_AGGREGATE_ENABLED=true) writing into the gitignored artifacts dir,
			// with short flush/bucket windows so activity shows quickly. Production
			// leaves the whole block unset.
			Telemetry: config.TelemetryConfig{
				RawEnabled:       false,
				AggregateEnabled: false,
				Path:             "artifacts/telemetry.jsonl",
				Sink:             config.TelemetrySinkJSONL,
				MaxFileBytes:     8 << 20,
				MaxFiles:         3,
				BufferSize:       8192,
				FlushInterval:    2 * time.Second,
				AggregateBucket:  time.Minute,
				AggregateMaxKeys: 100_000,
			},
			// Follower knobs for `beeline work` against a local leader. Inert for
			// `serve`; the subcommand still requires --leader (or
			// BEELINE_MATRIX_FOLLOWER_LEADER_URL) to actually start, so having the
			// block filled in costs nothing.
			Follower: config.FollowerConfig{
				LeaderURL:   "http://localhost:8080",
				Port:        8081,
				Workers:     4,
				IdleBackoff: time.Second,
				HTTP:        httpclient.Config{Timeout: 10 * time.Second},
			},
			TargetTTL:           30 * time.Second,
			LeaseDuration:       15 * time.Second,
			SweepInterval:       15 * time.Second,
			RefreshWorkers:      4,
			RefreshBatch:        256,
			SilenceRouteLogging: true, // the console polls twice a second; keep the demo quiet
		},
	}
}

// buildClusterConfig is the distributed-mode config the multi-head demo (and
// any real multi-head deployment) starts from: identical heads over a shared
// Postgres — the freshness index, operator config, and estimate cache all live
// there, so every head is stateless and disposable. Heads run coordinator-only
// (refreshWorkers 0) with the simulated-latency provider available, so compute
// comes from `work` followers pointed at any head. The SQLite databasePath is
// ignored in this mode (config lives in Postgres); it is set only to satisfy
// validation. Deployments override the Postgres URL via
// BEELINE_MATRIX_BACKEND_POSTGRES_URL.
func buildClusterConfig() *config.Config {
	return &config.Config{
		Observability: observability.Config{
			Logging: loggingcfg.Config{
				Provider:    loggingcfg.ProviderSlog,
				ServiceName: config.DefaultServiceName,
				Level:       logging.InfoLevel,
			},
		},
		Matrix: config.MatrixConfig{
			Server: serverhttp.Config{
				Port:            8080,
				StartupDeadline: 5 * time.Second,
			},
			DatabasePath:   "beeline.db", // unused in postgres mode; validation requires it
			Profiles:       defaultProfiles(),
			DefaultProfile: "car",
			Backend: config.BackendConfig{
				Mode: config.BackendModePostgres,
				// Demo credentials for the throwaway local docker postgres; real
				// deploys override via BEELINE_MATRIX_BACKEND_POSTGRES_URL.
				Postgres: config.PostgresConfig{ //nolint:gosec // local demo creds
					URL: "postgres://beeline:beeline@localhost:5432/beeline?sslmode=disable",
				},
				ConfigPollInterval: 2 * time.Second,
			},
			EngineLatency: config.EngineLatencyConfig{
				Enabled: true,
				Min:     25 * time.Millisecond,
				Max:     120 * time.Millisecond,
			},
			Follower: config.FollowerConfig{
				Port:        8081,
				Workers:     4,
				IdleBackoff: time.Second,
				HTTP:        httpclient.Config{Timeout: 10 * time.Second},
			},
			TargetTTL:           30 * time.Second,
			LeaseDuration:       15 * time.Second,
			SweepInterval:       15 * time.Second,
			RefreshWorkers:      0, // heads coordinate; followers compute
			RefreshBatch:        256,
			SilenceRouteLogging: true,
		},
	}
}

// buildProductionConfig is the config for a production deployment: the same
// structured slog logging dialed back to info, a longer freshness TTL, and more
// refresh workers to keep the working set fresh at scale.
func buildProductionConfig() *config.Config {
	return &config.Config{
		Observability: observability.Config{
			Logging: loggingcfg.Config{
				Provider:    loggingcfg.ProviderSlog,
				ServiceName: config.DefaultServiceName,
				Level:       logging.InfoLevel,
			},
		},
		Matrix: config.MatrixConfig{
			Server: serverhttp.Config{
				Port:            8080,
				StartupDeadline: 5 * time.Second,
			},
			DatabasePath:   "beeline.db",
			Profiles:       defaultProfiles(),
			DefaultProfile: "car",
			TargetTTL:      300 * time.Second,
			LeaseDuration:  60 * time.Second,
			SweepInterval:  60 * time.Second,
			RefreshWorkers: 8,
			RefreshBatch:   512,
		},
	}
}
