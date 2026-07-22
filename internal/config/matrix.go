package config

import (
	"context"
	"fmt"
	"time"

	serverhttp "github.com/primandproper/platform-go/v4/server/http"
)

// MatrixConfig is the configuration for the travel-time/distance matrix service
// (design §10): the HTTP server, the area database path, the routing profiles and
// their speeds, and the freshness/refresh knobs. It sits alongside Observability
// under the top-level Config.
//
// Service areas are no longer configured here — they live in the SQLite database at
// DatabasePath, are created disabled via the control plane, and only refresh once
// enabled. A fresh database boots with no areas.
//
// Every field carries both an envPrefix/env tag and a json tag so it participates
// in Load (BEELINE_MATRIX_… environment overlay) and LoadFromFile (JSON), per the
// project's configuration convention.
type MatrixConfig struct {
	Profiles       map[string]float64        `env:"PROFILES"              json:"profiles"`
	Providers      map[string]ProviderConfig `env:"PROVIDERS"             json:"providers,omitempty"`
	DefaultProfile string                    `env:"DEFAULT_PROFILE"       json:"defaultProfile"`
	DatabasePath   string                    `env:"DATABASE_PATH"         json:"databasePath"`
	Server         serverhttp.Config         `envPrefix:"SERVER_"         json:"server"`
	EngineLatency  EngineLatencyConfig       `envPrefix:"ENGINE_LATENCY_" json:"engineLatency,omitzero"`
	TargetTTL      time.Duration             `env:"TARGET_TTL"            json:"targetTTL"`
	LeaseDuration  time.Duration             `env:"LEASE_DURATION"        json:"leaseDuration"`
	SweepInterval  time.Duration             `env:"SWEEP_INTERVAL"        json:"sweepInterval"`
	RefreshWorkers int                       `env:"REFRESH_WORKERS"       json:"refreshWorkers"`
	RefreshBatch   int                       `env:"REFRESH_BATCH"         json:"refreshBatch"`

	// SilenceRouteLogging suppresses the router's per-response access log. The
	// operator console polls /_ops_/freshness and /_ops_/cells every second or two,
	// which otherwise floods stdout; health probes are always excluded regardless.
	// Errors, panics, and startup logs still print. On for localdev/demo, off in
	// production where access logs are wanted.
	SilenceRouteLogging bool `env:"SILENCE_ROUTE_LOGGING" json:"silenceRouteLogging,omitempty"`
}

// EngineLatencyConfig models the routing engine as network-bound. When Enabled, a
// second provider named LatentHaversineProviderName is registered alongside the raw
// default: the same in-process Haversine engine wrapped so every Table call waits a
// random delay in [Min, Max] before returning, simulating an OSRM/Valhalla reached
// over HTTP. An area opts into this by selecting that provider (e.g. to see how the
// refresh pool and read path behave under real I/O latency, or to simulate too few
// workers) and reverts by selecting the raw "haversine" default — enabling latency
// never changes what the default provider resolves to. Off by default, so production
// registers only the nanosecond in-process engine unless it opts in.
type EngineLatencyConfig struct {
	Min     time.Duration `env:"MIN"     json:"min,omitempty"`
	Max     time.Duration `env:"MAX"     json:"max,omitempty"`
	Enabled bool          `env:"ENABLED" json:"enabled,omitempty"`
}

// Provider registry constants. The built-in Haversine engine is always registered
// under DefaultProviderName, so an area with no provider set (the empty string) keeps
// routing through the in-process engine. Configured providers add named entries an
// area can select instead.
const (
	// DefaultProviderName is the name of the always-present built-in engine. It is
	// always the raw, in-process Haversine stand-in — enabling EngineLatency never
	// changes what this name resolves to.
	DefaultProviderName = "haversine"
	// LatentHaversineProviderName is the built-in Haversine engine wrapped with a
	// simulated network delay. It is registered as a separate, selectable provider
	// only when EngineLatency is enabled, so an area opts into the network-bound
	// behavior by selecting it and reverts by pointing back at DefaultProviderName.
	LatentHaversineProviderName = "latent-haversine"
	// ProviderTypeHaversine builds an in-process great-circle engine from Profiles.
	ProviderTypeHaversine = "haversine"
	// ProviderTypeOSRM builds an HTTP client against a live OSRM /table endpoint.
	ProviderTypeOSRM = "osrm"
)

// ProviderConfig declares one named routing provider in the registry (§ per-area
// providers). Type selects the engine; the remaining fields configure it. haversine
// providers reuse the global Profiles/speeds and need no other field. osrm providers
// require a BaseURL and may map beeline profiles to the OSRM profile path segments the
// server was built with (e.g. car→driving), bound the matrix size to the server's
// max-table-size, and set a per-request Timeout.
type ProviderConfig struct {
	Profiles     map[string]string `json:"profiles,omitempty"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"baseURL,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`
	MaxTableSize int               `json:"maxTableSize,omitempty"`
}

// validate confirms a provider entry is internally consistent for its type: a known
// type, and a base URL for osrm (whose engine cannot be reached without one).
func (p *ProviderConfig) validate(name string) error {
	switch p.Type {
	case ProviderTypeHaversine:
		// Reuses the global profiles/speeds; nothing else to check.
	case ProviderTypeOSRM:
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q of type osrm requires a baseURL", name)
		}
	default:
		return fmt.Errorf("provider %q has unknown type %q (want haversine or osrm)", name, p.Type)
	}
	if p.MaxTableSize < 0 {
		return fmt.Errorf("provider %q max table size %d must be >= 0", name, p.MaxTableSize)
	}
	if p.Timeout < 0 {
		return fmt.Errorf("provider %q timeout %v must be >= 0", name, p.Timeout)
	}

	return nil
}

// validate confirms the latency window is a sane range. It only constrains the
// bounds when the wrapper is Enabled, so a disabled config with zero bounds passes.
func (e *EngineLatencyConfig) validate() error {
	if !e.Enabled {
		return nil
	}
	if e.Min < 0 {
		return fmt.Errorf("engine latency min %v must be non-negative", e.Min)
	}
	if e.Max < e.Min {
		return fmt.Errorf("engine latency max %v must be >= min %v", e.Max, e.Min)
	}

	return nil
}

// defaultMatrixConfig returns the built-in matrix defaults so the binary boots
// without a config file: an HTTP server on :8080, an area database at beeline.db, and
// car/bike/walk profiles. No areas exist until one is created via the control plane.
func defaultMatrixConfig() MatrixConfig {
	return MatrixConfig{
		Server: serverhttp.Config{
			Port:            8080,
			StartupDeadline: 5 * time.Second,
		},
		DatabasePath: "beeline.db",
		Profiles: map[string]float64{
			"car":  13.9, // ~50 km/h
			"bike": 4.2,  // ~15 km/h
			"walk": 1.4,  // ~5 km/h
		},
		DefaultProfile: "car",
		TargetTTL:      60 * time.Second,
		LeaseDuration:  30 * time.Second,
		SweepInterval:  30 * time.Second,
		RefreshWorkers: 4,
		RefreshBatch:   256,
	}
}

// validate confirms the matrix config is internally consistent. It is called from
// Config.Validate so both loaders reject a broken config before anything uses it.
func (m *MatrixConfig) validate(ctx context.Context) error {
	if m.DatabasePath == "" {
		return fmt.Errorf("database path is required")
	}
	if len(m.Profiles) == 0 {
		return fmt.Errorf("at least one profile is required")
	}
	for name, speed := range m.Profiles {
		if speed <= 0 {
			return fmt.Errorf("profile %q must have a positive speed, got %v", name, speed)
		}
	}
	if _, ok := m.Profiles[m.DefaultProfile]; !ok {
		return fmt.Errorf("default profile %q is not one of the configured profiles", m.DefaultProfile)
	}
	if m.TargetTTL <= 0 {
		return fmt.Errorf("target TTL %v must be positive", m.TargetTTL)
	}
	if m.LeaseDuration <= 0 {
		return fmt.Errorf("lease duration %v must be positive", m.LeaseDuration)
	}
	if m.SweepInterval <= 0 {
		return fmt.Errorf("sweep interval %v must be positive", m.SweepInterval)
	}
	if m.RefreshWorkers < 1 {
		return fmt.Errorf("refresh workers %d must be >= 1", m.RefreshWorkers)
	}
	if m.RefreshBatch < 1 {
		return fmt.Errorf("refresh batch %d must be >= 1", m.RefreshBatch)
	}
	if err := m.EngineLatency.validate(); err != nil {
		return err
	}
	for name := range m.Providers {
		if name == DefaultProviderName {
			return fmt.Errorf("provider name %q is reserved for the built-in engine", name)
		}
		provider := m.Providers[name]
		if err := provider.validate(name); err != nil {
			return fmt.Errorf("provider config: %w", err)
		}
	}

	return m.Server.ValidateWithContext(ctx)
}
