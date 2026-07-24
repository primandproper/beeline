package beeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Provider registry names and types. The built-in Haversine engine is always
// registered under DefaultProviderName, so an area with no provider set (the empty
// string) keeps routing through the in-process engine. Everything else is a named
// entry an area can select.
const (
	// DefaultProviderName is the name of the always-present built-in engine. It is
	// always the raw, in-process Haversine stand-in — enabling engine latency never
	// changes what this name resolves to.
	DefaultProviderName = "haversine"
	// LatentHaversineProviderName is the built-in Haversine engine wrapped with a
	// simulated network delay, registered as a separate, selectable provider only
	// when engine latency is enabled.
	LatentHaversineProviderName = "latent-haversine"
	// ProviderTypeHaversine builds an in-process great-circle engine from the
	// profile speed map.
	ProviderTypeHaversine = "haversine"
	// ProviderTypeOSRM builds an HTTP client against a live OSRM /table endpoint.
	ProviderTypeOSRM = "osrm"
	// ProviderTypeLatentHaversine is the Haversine engine wrapped with a simulated
	// network delay. It exists so the leader's latency simulation travels to
	// followers through the provider catalog; it is synthesized from the leader's
	// engine-latency config, never created through the provider control plane.
	ProviderTypeLatentHaversine = "latent-haversine"
)

// ProviderSpec is one named routing provider, fully described: everything a process
// needs to construct the engine locally. It is both the persisted shape (the leader's
// SQLite provider registry) and the wire shape (the provider catalog followers sync),
// so pointing the whole cluster at a different OSRM instance is one control-plane
// change — the config travels with the work, not through N follower config files.
//
// Type selects the engine; the remaining fields configure it. haversine providers use
// the catalog's profile speed map and need no other field. osrm providers require a
// BaseURL and may map beeline profiles to the OSRM profile path segments the server
// was built with (e.g. car→driving), bound the matrix size to the server's
// max-table-size, and set a per-request timeout. latent-haversine carries the
// simulated delay window and is synthesized by the leader, never operator-created.
type ProviderSpec struct {
	Profiles     map[string]string `json:"profiles,omitempty"`
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"baseURL,omitempty"`
	TimeoutMs    int64             `json:"timeoutMs,omitempty"`
	LatencyMinMs int64             `json:"latencyMinMs,omitempty"`
	LatencyMaxMs int64             `json:"latencyMaxMs,omitempty"`
	MaxTableSize int               `json:"maxTableSize,omitempty"`
}

// Timeout converts the wire-friendly millisecond field to a duration.
func (p *ProviderSpec) Timeout() time.Duration { return time.Duration(p.TimeoutMs) * time.Millisecond }

// LatencyMin converts the simulated-delay lower bound to a duration.
func (p *ProviderSpec) LatencyMin() time.Duration {
	return time.Duration(p.LatencyMinMs) * time.Millisecond
}

// LatencyMax converts the simulated-delay upper bound to a duration.
func (p *ProviderSpec) LatencyMax() time.Duration {
	return time.Duration(p.LatencyMaxMs) * time.Millisecond
}

// Validate confirms a spec is internally consistent for its type. It does not decide
// whether the name is allowed (the control plane reserves the built-in names); it
// only rejects specs no engine could be built from.
func (p *ProviderSpec) Validate() error {
	if p.Name == "" {
		return fmt.Errorf("provider name is required")
	}
	switch p.Type {
	case ProviderTypeHaversine:
		// Uses the catalog's profile speeds; nothing else to check.
	case ProviderTypeOSRM:
		if p.BaseURL == "" {
			return fmt.Errorf("provider %q of type osrm requires a baseURL", p.Name)
		}
	case ProviderTypeLatentHaversine:
		if p.LatencyMinMs < 0 || p.LatencyMaxMs < p.LatencyMinMs {
			return fmt.Errorf("provider %q latency window [%dms, %dms] is not a sane range", p.Name, p.LatencyMinMs, p.LatencyMaxMs)
		}
	default:
		return fmt.Errorf("provider %q has unknown type %q (want haversine or osrm)", p.Name, p.Type)
	}
	if p.MaxTableSize < 0 {
		return fmt.Errorf("provider %q max table size %d must be >= 0", p.Name, p.MaxTableSize)
	}
	if p.TimeoutMs < 0 {
		return fmt.Errorf("provider %q timeout %dms must be >= 0", p.Name, p.TimeoutMs)
	}

	return nil
}

// ProviderCatalog is the complete provider registry on the wire: every spec
// (built-ins included) plus the profile speed map haversine-type engines are built
// from, and a content hash over both. The leader serves it at /_work_/providers and
// stamps Hash into every /_work_/claim response; a follower that sees an unknown
// hash refetches the catalog and rebuilds its engines, so a control-plane change
// propagates to the fleet within one claim cycle with no follower config edits.
type ProviderCatalog struct {
	Speeds    map[string]float64 `json:"profiles"`
	Hash      string             `json:"hash,omitempty"`
	Providers []ProviderSpec     `json:"providers"`
}

// ComputeHash returns the canonical content hash of the catalog: sha256 over the
// JSON encoding with providers sorted by name and the Hash field itself excluded
// (encoding/json already emits map keys sorted). Identical catalogs hash
// identically regardless of construction order or which process computed it.
func (c *ProviderCatalog) ComputeHash() (string, error) {
	canon := ProviderCatalog{
		Speeds:    c.Speeds,
		Providers: slices.Clone(c.Providers),
	}
	slices.SortFunc(canon.Providers, func(a, b ProviderSpec) int {
		return strings.Compare(a.Name, b.Name)
	})

	raw, err := json.Marshal(canon)
	if err != nil {
		return "", fmt.Errorf("hashing provider catalog: %w", err)
	}
	sum := sha256.Sum256(raw)

	return hex.EncodeToString(sum[:]), nil
}
