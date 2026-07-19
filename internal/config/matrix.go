package config

import (
	"context"
	"fmt"
	"time"

	serverhttp "github.com/primandproper/platform-go/v4/server/http"
)

// MatrixConfig is the configuration for the travel-time/distance matrix service
// (design §10): the HTTP server, the service-area tessellation, the routing
// profiles and their speeds, and the freshness/refresh knobs. It sits alongside
// Observability under the top-level Config.
//
// Every field carries both an envPrefix/env tag and a json tag so it participates
// in Load (BEELINE_MATRIX_… environment overlay) and LoadFromFile (JSON), per the
// project's configuration convention.
type MatrixConfig struct {
	Profiles       map[string]float64 `env:"PROFILES"        json:"profiles"`
	DefaultProfile string             `env:"DEFAULT_PROFILE" json:"defaultProfile"`
	RoadMaskPath   string             `env:"ROAD_MASK_PATH"  json:"roadMaskPath,omitempty"`
	Server         serverhttp.Config  `envPrefix:"SERVER_"   json:"server"`
	Area           AreaConfig         `envPrefix:"AREA_"     json:"area"`
	TargetTTL      time.Duration      `env:"TARGET_TTL"      json:"targetTTL"`
	LeaseDuration  time.Duration      `env:"LEASE_DURATION"  json:"leaseDuration"`
	RefreshWorkers int                `env:"REFRESH_WORKERS" json:"refreshWorkers"`
	RefreshBatch   int                `env:"REFRESH_BATCH"   json:"refreshBatch"`
}

// AreaConfig describes the service area to tessellate: a center point, the H3
// resolution, how many rings around the center define the area, and the
// travel-radius bound (rings of reachable neighbors materialized per origin, §7).
type AreaConfig struct {
	Lat         float64 `env:"LAT"          json:"lat"`
	Lng         float64 `env:"LNG"          json:"lng"`
	Resolution  int     `env:"RESOLUTION"   json:"resolution"`
	AreaRings   int     `env:"AREA_RINGS"   json:"areaRings"`
	RadiusRings int     `env:"RADIUS_RINGS" json:"radiusRings"`
}

// defaultMatrixConfig returns the built-in matrix defaults so the binary boots
// without a config file: an HTTP server on :8080 and a service area over Lake
// Travis in the Austin, TX metro with car/bike/walk profiles. The default carries
// no road mask (RoadMaskPath is empty) since the file lives with a specific config;
// localdev wires it in. Boot with the full geometric disk when no config is given.
func defaultMatrixConfig() MatrixConfig {
	return MatrixConfig{
		Server: serverhttp.Config{
			Port:            8080,
			StartupDeadline: 5 * time.Second,
		},
		Area: AreaConfig{
			Lat:         30.34284460447388,
			Lng:         -98.02736352689148,
			Resolution:  9,
			AreaRings:   14,
			RadiusRings: 3,
		},
		Profiles: map[string]float64{
			"car":  13.9, // ~50 km/h
			"bike": 4.2,  // ~15 km/h
			"walk": 1.4,  // ~5 km/h
		},
		DefaultProfile: "car",
		TargetTTL:      60 * time.Second,
		LeaseDuration:  30 * time.Second,
		RefreshWorkers: 4,
		RefreshBatch:   256,
	}
}

// validate confirms the matrix config is internally consistent. It is called from
// Config.Validate so both loaders reject a broken config before anything uses it.
func (m *MatrixConfig) validate(ctx context.Context) error {
	if m.Area.Resolution < 0 || m.Area.Resolution > 15 {
		return fmt.Errorf("area resolution %d out of range [0,15]", m.Area.Resolution)
	}
	if m.Area.AreaRings < 0 {
		return fmt.Errorf("area rings %d must be >= 0", m.Area.AreaRings)
	}
	if m.Area.RadiusRings < 1 {
		return fmt.Errorf("radius rings %d must be >= 1", m.Area.RadiusRings)
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
	if m.RefreshWorkers < 1 {
		return fmt.Errorf("refresh workers %d must be >= 1", m.RefreshWorkers)
	}
	if m.RefreshBatch < 1 {
		return fmt.Errorf("refresh batch %d must be >= 1", m.RefreshBatch)
	}

	return m.Server.ValidateWithContext(ctx)
}
