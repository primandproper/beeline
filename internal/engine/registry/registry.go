// Package registry builds the named routing-provider registry from configuration: a
// name→RoutingEngine map the control plane consults so each service area can route
// through its chosen provider (§ per-area providers).
//
// It always registers a built-in raw Haversine engine under the default name, so an area
// with no provider set keeps using the nanosecond in-process engine. When engine latency
// is enabled it additionally registers a "latent-haversine" provider — the same engine
// wrapped with a simulated network delay — that an area can select to model a network-bound
// engine (or too few workers) and deselect to go back to raw. Configured providers add
// further named entries — additional Haversine engines or real OSRM endpoints — by name.
package registry

import (
	"fmt"
	"net/http"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	latencyengine "github.com/primandproper/beeline/internal/engine/latency"
	osrmengine "github.com/primandproper/beeline/internal/engine/osrm"
)

// Build assembles the provider registry. The built-in Haversine engine (from speeds)
// is always registered raw under config.DefaultProviderName, so that name resolves to
// the nanosecond in-process engine regardless of latency config. When latency.Enabled,
// a second entry — the same engine wrapped to simulate a network-bound one — is added
// under config.LatentHaversineProviderName as a separate, selectable provider; an area
// opts into the delay by choosing it and reverts by choosing the raw default. Each
// configured provider is then built by type. The provider config is assumed already
// validated (config.MatrixConfig.validate), but Build still rejects an unknown type
// defensively rather than registering a nil engine.
func Build(
	providers map[string]config.ProviderConfig,
	speeds map[beeline.Profile]float64,
	latency config.EngineLatencyConfig,
) (map[string]beeline.RoutingEngine, error) {
	out := make(map[string]beeline.RoutingEngine, len(providers)+2)

	out[config.DefaultProviderName] = haversineengine.New(speeds, 0)
	if latency.Enabled {
		out[config.LatentHaversineProviderName] = latencyengine.New(
			haversineengine.New(speeds, 0), latency.Min, latency.Max,
		)
	}

	for name := range providers {
		eng, err := buildOne(name, providers[name], speeds)
		if err != nil {
			return nil, err
		}
		out[name] = eng
	}

	return out, nil
}

// buildOne constructs a single provider's engine from its typed config.
func buildOne(name string, pc config.ProviderConfig, speeds map[beeline.Profile]float64) (beeline.RoutingEngine, error) {
	switch pc.Type {
	case config.ProviderTypeHaversine:
		return haversineengine.New(speeds, pc.MaxTableSize), nil
	case config.ProviderTypeOSRM:
		opts := make([]osrmengine.Option, 0, 3)
		if pc.MaxTableSize > 0 {
			opts = append(opts, osrmengine.WithMaxTableSize(pc.MaxTableSize))
		}
		if len(pc.Profiles) > 0 {
			opts = append(opts, osrmengine.WithProfiles(profileMap(pc.Profiles)))
		}
		if pc.Timeout > 0 {
			opts = append(opts, osrmengine.WithHTTPClient(&http.Client{Timeout: pc.Timeout}))
		}

		return osrmengine.New(pc.BaseURL, opts...), nil
	default:
		return nil, fmt.Errorf("registry: provider %q has unknown type %q", name, pc.Type)
	}
}

// profileMap converts the string-keyed config mapping into the typed one the OSRM
// engine expects (beeline profile → OSRM profile path segment).
func profileMap(m map[string]string) map[beeline.Profile]string {
	out := make(map[beeline.Profile]string, len(m))
	for k, v := range m {
		out[beeline.Profile(k)] = v
	}

	return out
}
