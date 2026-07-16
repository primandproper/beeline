package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/primandproper/platform-go/v4/observability"
	"github.com/primandproper/platform-go/v4/observability/logging"
	loggingcfg "github.com/primandproper/platform-go/v4/observability/logging/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validConfig(serviceName string) *Config {
	return &Config{
		Observability: observability.Config{
			Logging: loggingcfg.Config{
				Provider:    loggingcfg.ProviderSlog,
				ServiceName: serviceName,
				Level:       logging.InfoLevel,
			},
		},
	}
}

func TestRender(t *testing.T) {
	t.Parallel()

	t.Run("writes each config and round-trips through LoadFromFile", func(t *testing.T) {
		t.Parallel()

		dir := t.TempDir()
		envs := []Environment{
			{Name: "localdev", Path: filepath.Join(dir, "localdev.json"), Config: validConfig("dev-service")},
			{Name: "production", Path: filepath.Join(dir, "nested", "production.json"), Config: validConfig("prod-service")},
		}

		require.NoError(t, Render(context.Background(), envs, true))

		for _, env := range envs {
			loaded, err := LoadFromFile(context.Background(), env.Path)
			require.NoErrorf(t, err, "loading %s", env.Name)
			assert.Equal(t, env.Config.Observability.Logging.ServiceName, loaded.Observability.Logging.ServiceName)
		}
	})

	t.Run("is idempotent", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.json")
		env := []Environment{{Name: "localdev", Path: path, Config: validConfig("svc")}}

		require.NoError(t, Render(context.Background(), env, true))
		first, err := os.ReadFile(path)
		require.NoError(t, err)

		require.NoError(t, Render(context.Background(), env, true))
		second, err := os.ReadFile(path)
		require.NoError(t, err)

		assert.Equal(t, first, second)
	})

	t.Run("validation rejects an invalid config before writing", func(t *testing.T) {
		t.Parallel()

		path := filepath.Join(t.TempDir(), "config.json")
		// Missing logging service name fails validation.
		env := []Environment{{Name: "broken", Path: path, Config: &Config{}}}

		require.Error(t, Render(context.Background(), env, true))

		_, err := os.Stat(path)
		assert.Truef(t, os.IsNotExist(err), "no file should be written for an invalid config")
	})
}
