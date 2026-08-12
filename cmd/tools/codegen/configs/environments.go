package main

import (
	"time"

	"github.com/primandproper/beeline/internal/config"

	"github.com/primandproper/platform-go/v10/httpclient"
	"github.com/primandproper/platform-go/v10/observability"
	"github.com/primandproper/platform-go/v10/observability/logging"
	loggingcfg "github.com/primandproper/platform-go/v10/observability/logging/config"
	retrycfg "github.com/primandproper/platform-go/v10/retry/config"
	serverhttp "github.com/primandproper/platform-go/v10/server/http"
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
			Profiles:       defaultProfiles(),
			DefaultProfile: "car",
			Backend: config.BackendConfig{
				// Points at a local Postgres you supply — `make demo` runs its own
				// in-cluster one. Every serve process is a head over shared
				// coordination state, so a local run needs the database up.
				Postgres: config.PostgresConfig{ //nolint:gosec // local demo creds
					URL: "postgres://beeline:beeline@localhost:5432/beeline?sslmode=disable",
				},
				ConfigPollInterval: 2 * time.Second,
			},
			// An example named provider: a local OSRM server. This block is seed data
			// only — it is imported into the provider registry the first time a
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
				// A leader restart costs a short backoff instead of a wasted
				// claim cycle; a 4xx is never retried.
				Retry: retrycfg.Config{
					MaxAttempts:  3,
					InitialDelay: 100 * time.Millisecond,
					MaxDelay:     2 * time.Second,
					Multiplier:   2,
					UseJitter:    true,
				},
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

// buildClusterConfig is the distributed-mode config `make demo` (and any real
// multi-head deployment) starts from: identical heads over a shared
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
			Profiles:       defaultProfiles(),
			DefaultProfile: "car",
			Backend: config.BackendConfig{
				// Demo credentials for the throwaway in-cluster postgres; every
				// deployment overrides this via BEELINE_MATRIX_BACKEND_POSTGRES_URL.
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
				// A leader restart costs a short backoff instead of a wasted
				// claim cycle; a 4xx is never retried.
				Retry: retrycfg.Config{
					MaxAttempts:  3,
					InitialDelay: 100 * time.Millisecond,
					MaxDelay:     2 * time.Second,
					Multiplier:   2,
					UseJitter:    true,
				},
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
			Profiles:       defaultProfiles(),
			DefaultProfile: "car",
			Backend: config.BackendConfig{
				// A placeholder DSN: a real deploy overrides it with
				// BEELINE_MATRIX_BACKEND_POSTGRES_URL from a secret rather than
				// shipping credentials in a committed file. It is present because
				// validation requires a URL, and a config that cannot boot is worse
				// documentation than one that names the knob.
				Postgres: config.PostgresConfig{
					URL:      "postgres://beeline@postgres:5432/beeline?sslmode=require",
					MaxConns: 16,
				},
				ConfigPollInterval: 2 * time.Second,
			},
			TargetTTL:     300 * time.Second,
			LeaseDuration: 60 * time.Second,
			SweepInterval: 60 * time.Second,
			// Zero refresh workers: heads coordinate and serve reads, the `work`
			// follower pool computes. That is the deployment shape the follower
			// autoscaler assumes, and it keeps a head's latency budget free of
			// engine calls.
			RefreshWorkers: 0,
			RefreshBatch:   512,
		},
	}
}
