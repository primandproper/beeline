package config

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("a URL is all a backend needs", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{Postgres: PostgresConfig{URL: "postgres://localhost/beeline"}}
		require.NoError(t, b.validate())
		assert.Equal(t, HotStorePostgres, b.EffectiveHotStore(), "an unset hot store means Postgres")
	})

	t.Run("the zero value is rejected: there is no backend without Postgres", func(t *testing.T) {
		t.Parallel()

		// The deliberate reversal of the old contract, under which the zero value
		// was a working single-node deployment.
		require.ErrorContains(t, (&BackendConfig{}).validate(), "postgres URL is required")
	})

	t.Run("the retired in-memory hot store is rejected by name", func(t *testing.T) {
		t.Parallel()

		// It used to be the default, so an upgraded config may still carry it.
		// Silently promoting it to Postgres would point a head at a database the
		// operator never chose.
		b := BackendConfig{
			HotStore: hotStoreMemory,
			Postgres: PostgresConfig{URL: "postgres://localhost/beeline"},
		}
		err := b.validate()
		require.ErrorContains(t, err, "no longer supported")
		require.ErrorContains(t, err, "divergent caches")
	})

	t.Run("a redis hot store needs an addr", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{
			HotStore: HotStoreRedis,
			Postgres: PostgresConfig{URL: "postgres://localhost/beeline"},
		}
		require.ErrorContains(t, b.validate(), "redis addr is required")

		b.Redis.Addr = "localhost:6379"
		require.NoError(t, b.validate())
		assert.Equal(t, HotStoreRedis, b.EffectiveHotStore())
	})

	t.Run("redis still requires Postgres for coordination", func(t *testing.T) {
		t.Parallel()

		// Selecting Redis moves the estimate cache, not the coordination state.
		b := BackendConfig{HotStore: HotStoreRedis, Redis: RedisConfig{Addr: "localhost:6379"}}
		require.ErrorContains(t, b.validate(), "postgres URL is required")
	})

	t.Run("unknown selectors are rejected", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{HotStore: "dynamo", Postgres: PostgresConfig{URL: "postgres://localhost/beeline"}}
		require.ErrorContains(t, b.validate(), "hot store")
	})

	t.Run("conn bounds must be non-negative", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{Postgres: PostgresConfig{URL: "postgres://localhost/beeline", MaxConns: -1}}
		require.ErrorContains(t, b.validate(), "max conns")

		b = BackendConfig{Postgres: PostgresConfig{URL: "postgres://localhost/beeline"}, Redis: RedisConfig{DB: -1}}
		require.ErrorContains(t, b.validate(), "db and pool size")
	})

	t.Run("a negative config poll interval is rejected", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{
			Postgres:           PostgresConfig{URL: "postgres://localhost/beeline"},
			ConfigPollInterval: -time.Second,
		}
		require.ErrorContains(t, b.validate(), "config poll interval")
	})
}
