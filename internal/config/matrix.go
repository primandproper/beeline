package config

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v4/httpclient"
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
	Providers           map[string]ProviderConfig `env:"PROVIDERS"             json:"providers,omitempty"`
	Profiles            map[string]float64        `env:"PROFILES"              json:"profiles"`
	DefaultProfile      string                    `env:"DEFAULT_PROFILE"       json:"defaultProfile"`
	DatabasePath        string                    `env:"DATABASE_PATH"         json:"databasePath"`
	Server              serverhttp.Config         `envPrefix:"SERVER_"         json:"server"`
	Telemetry           TelemetryConfig           `envPrefix:"TELEMETRY_"      json:"telemetry,omitzero"`
	Follower            FollowerConfig            `envPrefix:"FOLLOWER_"       json:"follower,omitzero"`
	EngineLatency       EngineLatencyConfig       `envPrefix:"ENGINE_LATENCY_" json:"engineLatency,omitzero"`
	TargetTTL           time.Duration             `env:"TARGET_TTL"            json:"targetTTL"`
	LeaseDuration       time.Duration             `env:"LEASE_DURATION"        json:"leaseDuration"`
	SweepInterval       time.Duration             `env:"SWEEP_INTERVAL"        json:"sweepInterval"`
	RefreshWorkers      int                       `env:"REFRESH_WORKERS"       json:"refreshWorkers"`
	RefreshBatch        int                       `env:"REFRESH_BATCH"         json:"refreshBatch"`
	SilenceRouteLogging bool                      `env:"SILENCE_ROUTE_LOGGING" json:"silenceRouteLogging,omitempty"`
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

// FollowerConfig configures the `work` subcommand: the same binary run as a
// follower that claims pending pairs from a leader's /_work_/ endpoints, computes
// them with its local routing engine, and submits the results back. It is inert for
// `serve` — a leader ignores it entirely — and the subcommand refuses to start
// unless a leader URL arrives here or via the --leader flag. Batch and Lease of
// zero fall back to the shared RefreshBatch/LeaseDuration knobs so a follower
// paces itself like a local worker unless tuned otherwise (remote workers may want
// a longer lease to cover network latency).
type FollowerConfig struct {
	// LeaderURL is the base URL of the leader instance (e.g. http://host:8080).
	LeaderURL string `env:"LEADER_URL" json:"leaderURL,omitempty"`
	// HTTP tunes the outbound client used for claim/submit calls.
	HTTP httpclient.Config `envPrefix:"HTTP_" json:"http,omitzero"`
	// Lease is the visibility timeout requested per claim; 0 uses LeaseDuration.
	Lease time.Duration `env:"LEASE" json:"lease,omitempty"`
	// IdleBackoff is how long a worker sleeps when the leader has no due work or
	// is unreachable.
	IdleBackoff time.Duration `env:"IDLE_BACKOFF" json:"idleBackoff,omitempty"`
	// Port serves the follower's own health probes (/_ops_/live, /_ops_/ready).
	Port uint16 `env:"PORT" json:"port,omitempty"`
	// Workers is the number of concurrent claim→compute→submit loops.
	Workers int `env:"WORKERS" json:"workers,omitempty"`
	// Batch is the max pairs requested per claim; 0 uses RefreshBatch.
	Batch int `env:"BATCH" json:"batch,omitempty"`
}

// validate constrains the follower knobs only when a leader URL is set (the
// Telemetry/EngineLatency convention), so serve deployments that never touch the
// block pass untouched.
func (f *FollowerConfig) validate() error {
	if f.LeaderURL == "" {
		return nil
	}
	u, err := url.Parse(f.LeaderURL)
	if err != nil {
		return fmt.Errorf("follower leader URL %q: %w", f.LeaderURL, err)
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("follower leader URL %q must be http(s)://host[:port]", f.LeaderURL)
	}
	if f.Workers < 1 {
		return fmt.Errorf("follower workers %d must be >= 1", f.Workers)
	}
	if f.Batch < 0 {
		return fmt.Errorf("follower batch %d must be >= 0", f.Batch)
	}
	if f.Lease < 0 {
		return fmt.Errorf("follower lease %v must be >= 0", f.Lease)
	}
	if f.IdleBackoff < 0 {
		return fmt.Errorf("follower idle backoff %v must be >= 0", f.IdleBackoff)
	}

	return nil
}

// TelemetrySinkJSONL is the only telemetry sink type implemented today: an
// append-only newline-delimited JSON file, rotated by size.
const TelemetrySinkJSONL = "jsonl"

// TelemetryConfig configures query-event capture for offline demand-model training
// (which estimates get fetched, keyed by H3 cell — never raw coordinates). Two
// independently switchable channels share one recorder and sink: RawEnabled writes
// one record per fetch; AggregateEnabled writes per-(pair, time bucket) counts on
// each flush. Both are off by default — nothing is recorded or written unless an
// operator opts in. The read path hands events to a bounded buffer and never
// blocks: a full buffer drops (and counts) events instead of slowing queries.
type TelemetryConfig struct {
	// Path is the sink file the JSONL writer appends to.
	Path string `env:"PATH" json:"path,omitempty"`
	// Sink selects the writer implementation; only "jsonl" exists today.
	Sink string `env:"SINK" json:"sink,omitempty"`
	// MaxFileBytes rotates the sink file when it would grow past this size.
	MaxFileBytes int64 `env:"MAX_FILE_BYTES" json:"maxFileBytes,omitempty"`
	// MaxFiles is how many rotated files are retained alongside the live one.
	MaxFiles int `env:"MAX_FILES" json:"maxFiles,omitempty"`
	// BufferSize caps the in-flight event buffer between the read path and the
	// flusher; overflow drops events rather than ever blocking a query.
	BufferSize int `env:"BUFFER_SIZE" json:"bufferSize,omitempty"`
	// FlushInterval is the flusher cadence: completed aggregate buckets are emitted
	// and buffered sink writes are pushed to disk.
	FlushInterval time.Duration `env:"FLUSH_INTERVAL" json:"flushInterval,omitempty"`
	// AggregateBucket is the aggregation window (counts per pair per bucket).
	AggregateBucket time.Duration `env:"AGGREGATE_BUCKET" json:"aggregateBucket,omitempty"`
	// AggregateMaxKeys bounds the in-memory aggregation map.
	AggregateMaxKeys int `env:"AGGREGATE_MAX_KEYS" json:"aggregateMaxKeys,omitempty"`
	// RawEnabled records one event per fetch.
	RawEnabled bool `env:"RAW_ENABLED" json:"rawEnabled,omitempty"`
	// AggregateEnabled records per-(pair, bucket) demand counts.
	AggregateEnabled bool `env:"AGGREGATE_ENABLED" json:"aggregateEnabled,omitempty"`
}

// Enabled reports whether any capture channel is on — the switch for constructing
// the recorder and sink at all.
func (t *TelemetryConfig) Enabled() bool {
	return t.RawEnabled || t.AggregateEnabled
}

// validate constrains the knobs only when a channel is enabled, so a disabled
// zero-value config passes untouched (the EngineLatency convention).
func (t *TelemetryConfig) validate() error {
	if !t.Enabled() {
		return nil
	}
	if t.Path == "" {
		return fmt.Errorf("telemetry path is required when telemetry is enabled")
	}
	if t.Sink != TelemetrySinkJSONL {
		return fmt.Errorf("telemetry sink %q is unknown (want %q)", t.Sink, TelemetrySinkJSONL)
	}
	if t.MaxFileBytes <= 0 {
		return fmt.Errorf("telemetry max file bytes %d must be positive", t.MaxFileBytes)
	}
	if t.MaxFiles < 1 {
		return fmt.Errorf("telemetry max files %d must be >= 1", t.MaxFiles)
	}
	if t.BufferSize < 1 {
		return fmt.Errorf("telemetry buffer size %d must be >= 1", t.BufferSize)
	}
	if t.FlushInterval <= 0 {
		return fmt.Errorf("telemetry flush interval %v must be positive", t.FlushInterval)
	}
	if t.AggregateEnabled {
		if t.AggregateBucket <= 0 {
			return fmt.Errorf("telemetry aggregate bucket %v must be positive", t.AggregateBucket)
		}
		if t.AggregateMaxKeys < 1 {
			return fmt.Errorf("telemetry aggregate max keys %d must be >= 1", t.AggregateMaxKeys)
		}
	}

	return nil
}

// Provider registry constants, aliased from the domain package (the single source
// of truth) so existing config-level references keep compiling.
const (
	// DefaultProviderName is the name of the always-present built-in engine.
	DefaultProviderName = beeline.DefaultProviderName
	// LatentHaversineProviderName is the built-in Haversine engine wrapped with a
	// simulated network delay, registered only when EngineLatency is enabled.
	LatentHaversineProviderName = beeline.LatentHaversineProviderName
	// ProviderTypeHaversine builds an in-process great-circle engine from Profiles.
	ProviderTypeHaversine = beeline.ProviderTypeHaversine
	// ProviderTypeOSRM builds an HTTP client against a live OSRM /table endpoint.
	ProviderTypeOSRM = beeline.ProviderTypeOSRM
)

// ProviderConfig declares one named routing provider in the file config. Since the
// provider registry moved into the SQLite control plane (/_config_/providers), these
// entries are seed data only: imported into the database the first time a leader
// boots against an empty providers table, after which the database is authoritative
// and this block is ignored. Type selects the engine; haversine providers reuse the
// global Profiles/speeds and need no other field; osrm providers require a BaseURL
// and may map beeline profiles to OSRM profile path segments (e.g. car→driving),
// bound the matrix size, and set a per-request Timeout.
type ProviderConfig struct {
	Profiles     map[string]string `json:"profiles,omitempty"`
	Type         string            `json:"type"`
	BaseURL      string            `json:"baseURL,omitempty"`
	Timeout      time.Duration     `json:"timeout,omitempty"`
	MaxTableSize int               `json:"maxTableSize,omitempty"`
}

// Spec converts a named file-config provider entry to the domain spec the control
// plane persists and serves — the shape used to seed an empty providers table.
func (p *ProviderConfig) Spec(name string) beeline.ProviderSpec {
	return beeline.ProviderSpec{
		Name:         name,
		Type:         p.Type,
		BaseURL:      p.BaseURL,
		Profiles:     p.Profiles,
		TimeoutMs:    p.Timeout.Milliseconds(),
		MaxTableSize: p.MaxTableSize,
	}
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
		// Telemetry defaults are ready-to-enable: both channels start off, and
		// flipping RawEnabled/AggregateEnabled needs no other knob.
		Telemetry: TelemetryConfig{
			Path:             "beeline-telemetry.jsonl",
			Sink:             TelemetrySinkJSONL,
			MaxFileBytes:     64 << 20,
			MaxFiles:         5,
			BufferSize:       8192,
			FlushInterval:    5 * time.Second,
			AggregateBucket:  5 * time.Minute,
			AggregateMaxKeys: 100_000,
		},
		// Follower defaults are ready-to-enable, like Telemetry: pointing LeaderURL
		// (or --leader) at an instance needs no other knob.
		Follower: FollowerConfig{
			Port:        8081,
			Workers:     4,
			IdleBackoff: time.Second,
			HTTP:        httpclient.Config{Timeout: 10 * time.Second},
		},
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
	// 0 is a deliberate operating mode: a coordinator-only leader that seeds and
	// serves work over /_work_/ while followers do all the computing.
	if m.RefreshWorkers < 0 {
		return fmt.Errorf("refresh workers %d must be >= 0", m.RefreshWorkers)
	}
	if m.RefreshBatch < 1 {
		return fmt.Errorf("refresh batch %d must be >= 1", m.RefreshBatch)
	}
	if err := m.EngineLatency.validate(); err != nil {
		return err
	}
	if err := m.Telemetry.validate(); err != nil {
		return err
	}
	if err := m.Follower.validate(); err != nil {
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
