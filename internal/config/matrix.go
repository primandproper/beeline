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
	Profiles       map[string]float64 `env:"PROFILES"        json:"profiles"`
	DefaultProfile string             `env:"DEFAULT_PROFILE" json:"defaultProfile"`
	DatabasePath   string             `env:"DATABASE_PATH"   json:"databasePath"`
	Server         serverhttp.Config  `envPrefix:"SERVER_"   json:"server"`
	TargetTTL      time.Duration      `env:"TARGET_TTL"      json:"targetTTL"`
	LeaseDuration  time.Duration      `env:"LEASE_DURATION"  json:"leaseDuration"`
	SweepInterval  time.Duration      `env:"SWEEP_INTERVAL"  json:"sweepInterval"`
	RefreshWorkers int                `env:"REFRESH_WORKERS" json:"refreshWorkers"`
	RefreshBatch   int                `env:"REFRESH_BATCH"   json:"refreshBatch"`

	// SilenceRouteLogging suppresses the router's per-response access log. The
	// operator console polls /_ops_/freshness and /_ops_/cells every second or two,
	// which otherwise floods stdout; health probes are always excluded regardless.
	// Errors, panics, and startup logs still print. On for localdev/demo, off in
	// production where access logs are wanted.
	SilenceRouteLogging bool `env:"SILENCE_ROUTE_LOGGING" json:"silenceRouteLogging,omitempty"`
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

	return m.Server.ValidateWithContext(ctx)
}
