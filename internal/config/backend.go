package config

import (
	"fmt"
	"time"
)

// Backend mode and hot-store selectors. Distributed mode is opt-in: the zero
// value (or "memory") keeps today's zero-dependency single-node behavior, where
// the freshness index and hot store live in process memory and service areas in
// the local SQLite file.
const (
	// BackendModeMemory is the single-node default: in-memory index + store.
	BackendModeMemory = "memory"
	// BackendModePostgres runs the instance as a stateless head over shared
	// Postgres coordination state (freshness index, operator config, election).
	BackendModePostgres = "postgres"
	// HotStorePostgres serves cached estimates from the same Postgres.
	HotStorePostgres = "postgres"
	// HotStoreRedis serves cached estimates from Redis.
	HotStoreRedis = "redis"
)

// BackendConfig selects where beeline's mutable coordination state lives
// (design §8 addendum). Mode "memory" (or empty) is the self-hostable default:
// everything in process, exactly one `serve` instance. Mode "postgres" makes
// this instance a stateless head over a shared Postgres: the freshness index,
// operator config (areas/providers), and singleton-job election all move to the
// database, so any number of identical heads can run at once and `work`
// followers may point at any of them. HotStore picks the estimate cache
// independently ("" follows Mode): Postgres keeps the deployment to a single
// dependency, Redis buys hot-read headroom for very large batch reads.
//
// In postgres mode the SQLite DatabasePath is ignored — operator config lives
// in Postgres so every head sees the same areas and providers.
type BackendConfig struct {
	Mode               string         `env:"MODE"                 json:"mode,omitempty"`
	HotStore           string         `env:"HOT_STORE"            json:"hotStore,omitempty"`
	Postgres           PostgresConfig `envPrefix:"POSTGRES_"      json:"postgres,omitzero"`
	Redis              RedisConfig    `envPrefix:"REDIS_"         json:"redis,omitzero"`
	ConfigPollInterval time.Duration  `env:"CONFIG_POLL_INTERVAL" json:"configPollInterval,omitempty"`
}

// PostgresConfig is the shared-state database. URL is a pgx-compatible DSN or
// URL (postgres://user:pass@host:5432/beeline). Conn bounds map onto pgxpool;
// zero values use the pool's defaults.
//
// It satisfies platform-go's database.ClientConfig, so the shared pool is built
// by database/postgres rather than by hand. Beeline runs one pool against one
// URL, so the read side is deliberately empty: an empty read connection string
// makes the platform client alias its read handles to the write ones instead of
// opening a second pool.
type PostgresConfig struct {
	URL      string `env:"URL"       json:"url,omitempty"`
	MaxConns int32  `env:"MAX_CONNS" json:"maxConns,omitempty"`
}

// pingAttempts and pingWaitPeriod bound the client's readiness ping. The profile
// is deliberately fast: IsReady backs the /_ops_/ready database check, whose
// per-checker budget is 5s, so a down database must fail well inside it.
const (
	pingAttempts   = 3
	pingWaitPeriod = 500 * time.Millisecond
)

// databaseSQLIdleConns bounds the database/sql handle derived from the pgx pool.
// That handle only serves migrations and advisory-lock transactions — everything
// else in beeline speaks pgx natively — and each idle connection there still
// pins one pool connection, so it stays small.
const databaseSQLIdleConns = 2

// GetReadConnectionString is empty by design: see PostgresConfig.
func (p *PostgresConfig) GetReadConnectionString() string { return "" }

// GetWriteConnectionString returns the configured DSN.
func (p *PostgresConfig) GetWriteConnectionString() string { return p.URL }

// GetMaxOpenConns bounds the pool; zero keeps pgxpool's default.
func (p *PostgresConfig) GetMaxOpenConns() int { return int(p.MaxConns) }

// GetMaxIdleConns bounds the derived database/sql handle.
func (p *PostgresConfig) GetMaxIdleConns() int { return databaseSQLIdleConns }

// GetConnMaxLifetime keeps pgxpool's parsed default (1h).
func (p *PostgresConfig) GetConnMaxLifetime() time.Duration { return 0 }

// GetMaxPingAttempts bounds the readiness ping retry loop.
func (p *PostgresConfig) GetMaxPingAttempts() uint64 { return pingAttempts }

// GetPingWaitPeriod paces the readiness ping retry loop.
func (p *PostgresConfig) GetPingWaitPeriod() time.Duration { return pingWaitPeriod }

// RedisConfig is the optional hot-store cache. Zero PoolSize uses go-redis's
// default (10 per CPU).
type RedisConfig struct {
	Addr     string `env:"ADDR"      json:"addr,omitempty"`
	Password string `env:"PASSWORD"  json:"password,omitempty"`
	DB       int    `env:"DB"        json:"db,omitempty"`
	PoolSize int    `env:"POOL_SIZE" json:"poolSize,omitempty"`
}

// Distributed reports whether this instance coordinates through shared Postgres
// state rather than process memory.
func (b *BackendConfig) Distributed() bool {
	return b.Mode == BackendModePostgres
}

// EffectiveHotStore resolves the estimate-cache backend: an explicit HotStore
// wins, otherwise it follows Mode (memory mode → in-memory store, postgres mode
// → postgres store).
func (b *BackendConfig) EffectiveHotStore() string {
	if b.HotStore != "" {
		return b.HotStore
	}
	if b.Distributed() {
		return HotStorePostgres
	}

	return BackendModeMemory
}

// validate constrains the knobs only when a non-default backend is selected, so
// the zero-value block passes untouched (the EngineLatency convention).
func (b *BackendConfig) validate() error {
	switch b.Mode {
	case "", BackendModeMemory, BackendModePostgres:
	default:
		return fmt.Errorf("backend mode %q is unknown (want %q or %q)", b.Mode, BackendModeMemory, BackendModePostgres)
	}
	switch b.HotStore {
	case "", BackendModeMemory, HotStorePostgres, HotStoreRedis:
	default:
		return fmt.Errorf("backend hot store %q is unknown (want %q, %q or %q)",
			b.HotStore, BackendModeMemory, HotStorePostgres, HotStoreRedis)
	}
	if b.Distributed() && b.EffectiveHotStore() == BackendModeMemory {
		return fmt.Errorf("backend mode %q cannot use the in-memory hot store: heads would serve divergent caches", b.Mode)
	}
	if (b.Distributed() || b.EffectiveHotStore() == HotStorePostgres) && b.Postgres.URL == "" {
		return fmt.Errorf("backend postgres URL is required when mode or hot store is %q", BackendModePostgres)
	}
	if b.EffectiveHotStore() == HotStoreRedis && b.Redis.Addr == "" {
		return fmt.Errorf("backend redis addr is required when hot store is %q", HotStoreRedis)
	}
	if b.Postgres.MaxConns < 0 {
		return fmt.Errorf("backend postgres max conns %d must be >= 0", b.Postgres.MaxConns)
	}
	if b.Redis.DB < 0 || b.Redis.PoolSize < 0 {
		return fmt.Errorf("backend redis db and pool size must be >= 0")
	}
	if b.Mode != "" && b.ConfigPollInterval < 0 {
		return fmt.Errorf("backend config poll interval %v must be >= 0", b.ConfigPollInterval)
	}

	return nil
}
