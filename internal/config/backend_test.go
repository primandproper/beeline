package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBackendConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("the zero value is the memory default and validates", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{}
		require.NoError(t, b.validate())
		assert.False(t, b.Distributed())
		assert.Equal(t, BackendModeMemory, b.EffectiveHotStore())
	})

	t.Run("postgres mode needs only a URL", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{Mode: BackendModePostgres, Postgres: PostgresConfig{URL: "postgres://localhost/beeline"}}
		require.NoError(t, b.validate())
		assert.True(t, b.Distributed())
		assert.Equal(t, HotStorePostgres, b.EffectiveHotStore(), "hot store follows mode when unset")
	})

	t.Run("postgres mode without a URL is rejected", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{Mode: BackendModePostgres}
		require.ErrorContains(t, b.validate(), "postgres URL is required")
	})

	t.Run("postgres mode cannot fall back to the in-memory hot store", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{
			Mode:     BackendModePostgres,
			HotStore: BackendModeMemory,
			Postgres: PostgresConfig{URL: "postgres://localhost/beeline"},
		}
		require.ErrorContains(t, b.validate(), "divergent caches")
	})

	t.Run("a redis hot store needs an addr", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{
			Mode:     BackendModePostgres,
			HotStore: HotStoreRedis,
			Postgres: PostgresConfig{URL: "postgres://localhost/beeline"},
		}
		require.ErrorContains(t, b.validate(), "redis addr is required")

		b.Redis.Addr = "localhost:6379"
		require.NoError(t, b.validate())
		assert.Equal(t, HotStoreRedis, b.EffectiveHotStore())
	})

	t.Run("a single node may externalize just the hot store", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{HotStore: HotStorePostgres, Postgres: PostgresConfig{URL: "postgres://localhost/beeline"}}
		require.NoError(t, b.validate())
		assert.False(t, b.Distributed())
		assert.Equal(t, HotStorePostgres, b.EffectiveHotStore())
	})

	t.Run("unknown selectors are rejected", func(t *testing.T) {
		t.Parallel()

		require.ErrorContains(t, (&BackendConfig{Mode: "etcd"}).validate(), "backend mode")
		require.ErrorContains(t, (&BackendConfig{HotStore: "dynamo"}).validate(), "hot store")
	})

	t.Run("conn bounds must be coherent", func(t *testing.T) {
		t.Parallel()

		b := BackendConfig{
			Mode:     BackendModePostgres,
			Postgres: PostgresConfig{URL: "postgres://localhost/beeline", MaxConns: 4, MinConns: 8},
		}
		require.ErrorContains(t, b.validate(), "min conns")
	})
}
