// Package registry turns routing-provider specs into engines: a spec fully
// describes one named provider (§ per-area providers), and BuildEngine/BuildAll
// construct the corresponding RoutingEngine locally. Both halves of the
// leader/follower split use the same builder — the leader over the specs in its
// SQLite provider registry (plus the synthesized built-ins), a follower over the
// provider catalog it syncs from the leader — so a claimed area's provider name
// resolves to an identically-configured engine everywhere.
package registry

import (
	"fmt"
	"net/http"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	haversineengine "github.com/primandproper/beeline/internal/engine/haversine"
	latencyengine "github.com/primandproper/beeline/internal/engine/latency"
	osrmengine "github.com/primandproper/beeline/internal/engine/osrm"
)

// BuiltinSpecs returns the specs of the always-synthesized providers: the raw
// in-process Haversine engine under the default name, plus — when the latency
// simulation is enabled — the same engine wrapped with a random delay in
// [latencyMin, latencyMax] under the latent name. They are specs (not engines) so
// they travel in the provider catalog and followers build them like any other
// provider.
func BuiltinSpecs(latencyEnabled bool, latencyMin, latencyMax time.Duration) []beeline.ProviderSpec {
	specs := []beeline.ProviderSpec{{
		Name: beeline.DefaultProviderName,
		Type: beeline.ProviderTypeHaversine,
	}}
	if latencyEnabled {
		specs = append(specs, beeline.ProviderSpec{
			Name:         beeline.LatentHaversineProviderName,
			Type:         beeline.ProviderTypeLatentHaversine,
			LatencyMinMs: latencyMin.Milliseconds(),
			LatencyMaxMs: latencyMax.Milliseconds(),
		})
	}

	return specs
}

// BuildAll constructs the name→engine map for a set of specs. speeds is the
// profile speed map haversine-type engines divide by. Specs are validated as they
// are built, so a catalog from an untrusted-but-authoritative source (the leader)
// fails loudly instead of registering a nil engine.
func BuildAll(specs []beeline.ProviderSpec, speeds map[beeline.Profile]float64) (map[string]beeline.RoutingEngine, error) {
	out := make(map[string]beeline.RoutingEngine, len(specs))
	for i := range specs {
		eng, err := BuildEngine(&specs[i], speeds)
		if err != nil {
			return nil, err
		}
		out[specs[i].Name] = eng
	}

	return out, nil
}

// BuildEngine constructs a single provider's engine from its spec.
func BuildEngine(spec *beeline.ProviderSpec, speeds map[beeline.Profile]float64) (beeline.RoutingEngine, error) {
	if err := spec.Validate(); err != nil {
		return nil, fmt.Errorf("registry: %w", err)
	}

	switch spec.Type {
	case beeline.ProviderTypeHaversine:
		return haversineengine.New(speeds, spec.MaxTableSize), nil
	case beeline.ProviderTypeLatentHaversine:
		return latencyengine.New(
			haversineengine.New(speeds, spec.MaxTableSize), spec.LatencyMin(), spec.LatencyMax(),
		), nil
	case beeline.ProviderTypeOSRM:
		opts := make([]osrmengine.Option, 0, 3)
		if spec.MaxTableSize > 0 {
			opts = append(opts, osrmengine.WithMaxTableSize(spec.MaxTableSize))
		}
		if len(spec.Profiles) > 0 {
			opts = append(opts, osrmengine.WithProfiles(profileMap(spec.Profiles)))
		}
		if timeout := spec.Timeout(); timeout > 0 {
			opts = append(opts, osrmengine.WithHTTPClient(&http.Client{Timeout: timeout}))
		}

		return osrmengine.New(spec.BaseURL, opts...), nil
	default:
		// Validate already rejects unknown types; kept defensive.
		return nil, fmt.Errorf("registry: provider %q has unknown type %q", spec.Name, spec.Type)
	}
}

// profileMap converts the string-keyed spec mapping into the typed one the OSRM
// engine expects (beeline profile → OSRM profile path segment).
func profileMap(m map[string]string) map[beeline.Profile]string {
	out := make(map[beeline.Profile]string, len(m))
	for k, v := range m {
		out[beeline.Profile(k)] = v
	}

	return out
}
