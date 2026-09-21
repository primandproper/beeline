package config

import (
	"fmt"
	"time"
)

// Hot-store selectors. There is no backend mode: coordination state always
// lives in shared Postgres, so every `serve` process is a stateless head. Only
// the estimate cache is selectable.
const (
	// HotStorePostgres serves cached estimates from the same Postgres.
	HotStorePostgres = "postgres"
	// HotStoreRedis serves cached estimates from Redis.
	HotStoreRedis = "redis"
	// hotStoreMemory is no longer a valid selection. It is retained only so
	// validate can reject an upgraded config by name instead of silently
	// promoting it to Postgres.
	hotStoreMemory = "memory"
)

// BackendConfig configures the shared coordination state every `serve` process
// runs against (design §8.2). The freshness index, operator config
// (areas/providers), and singleton-job election all live in Postgres, so heads
// are stateless and disposable and `work` followers may point at any of them.
//
// HotStore picks the estimate cache independently: Postgres ("" or "postgres")
// keeps the deployment to a single dependency, Redis buys hot-read headroom for
// very large batch reads.
type BackendConfig struct {
	HotStore           string         `env:"HOT_STORE"            json:"hotStore,omitempty"`
	Postgres           PostgresConfig `envPrefix:"POSTGRES_"      json:"postgres,omitzero"`
	Redis              RedisConfig    `envPrefix:"REDIS_"         json:"redis,omitzero"`
	ConfigPollInterval time.Duration  `env:"CONFIG_POLL_INTERVAL" json:"configPollInterval,omitempty"`
}

// PostgresConfig is the shared-state database. URL is a pgx-compatible DSN or
// URL (postgres://user:pass@host:5432/beeline). Conn bounds map onto pgxpool;
// zero values use the pool's defaults.
//
// It satisfies primitives-go's database.ClientConfig, so the shared pool is built
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

// EffectiveHotStore resolves the estimate-cache backend. Empty means Postgres,
// which keeps a deployment to one dependency.
func (b *BackendConfig) EffectiveHotStore() string {
	if b.HotStore != "" {
		return b.HotStore
	}

	return HotStorePostgres
}

// validate rejects configurations this build cannot serve. Unlike the knobs that
// tolerate a zero value, the Postgres URL is unconditionally required: there is
// no backend that runs without it.
func (b *BackendConfig) validate() error {
	switch b.HotStore {
	case "", HotStorePostgres, HotStoreRedis:
	case hotStoreMemory:
		// Named explicitly because it used to be the default. Silently promoting
		// it to Postgres would start a head against a database the operator may
		// not have meant to point at.
		return fmt.Errorf("backend hot store %q is no longer supported: heads would serve divergent caches (want %q or %q)",
			hotStoreMemory, HotStorePostgres, HotStoreRedis)
	default:
		return fmt.Errorf("backend hot store %q is unknown (want %q or %q)",
			b.HotStore, HotStorePostgres, HotStoreRedis)
	}
	if b.Postgres.URL == "" {
		return fmt.Errorf("backend postgres URL is required: coordination state always lives in Postgres")
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
	if b.ConfigPollInterval < 0 {
		return fmt.Errorf("backend config poll interval %v must be >= 0", b.ConfigPollInterval)
	}

	return nil
}
