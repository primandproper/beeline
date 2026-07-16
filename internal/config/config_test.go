package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/primandproper/platform-go/v4/observability/logging"
	loggingcfg "github.com/primandproper/platform-go/v4/observability/logging/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNew(t *testing.T) {
	t.Parallel()

	t.Run("defaults are valid and build pillars", func(t *testing.T) {
		t.Parallel()

		cfg := New(Options{})

		assert.Equal(t, DefaultServiceName, cfg.Observability.Logging.ServiceName)
		assert.Equal(t, loggingcfg.ProviderSlog, cfg.Observability.Logging.Provider)
		assert.True(t, logging.LevelsEqual(logging.InfoLevel, cfg.Observability.Logging.Level))

		require.NoError(t, cfg.Validate(context.Background()))

		pillars, err := cfg.Observability.NewPillars(context.Background())
		require.NoError(t, err)
		require.NotNil(t, pillars)
		require.NotNil(t, pillars.Logger)
	})

	t.Run("options override defaults", func(t *testing.T) {
		t.Parallel()

		cfg := New(Options{ServiceName: "custom", LogLevel: "debug"})

		assert.Equal(t, "custom", cfg.Observability.Logging.ServiceName)
		assert.True(t, logging.LevelsEqual(logging.DebugLevel, cfg.Observability.Logging.Level))
		require.NoError(t, cfg.Validate(context.Background()))
	})
}

// These loader tests mutate the process environment via t.Setenv, which is
// incompatible with t.Parallel, so they run serially by design.

func TestLoad(t *testing.T) {
	t.Run("defaults when no environment variables are set", func(t *testing.T) {
		cfg, err := Load(context.Background(), Options{})
		require.NoError(t, err)

		assert.Equal(t, DefaultServiceName, cfg.Observability.Logging.ServiceName)
		assert.Equal(t, loggingcfg.ProviderSlog, cfg.Observability.Logging.Provider)
		assert.True(t, logging.LevelsEqual(logging.InfoLevel, cfg.Observability.Logging.Level))
	})

	t.Run("environment variables overlay the option defaults", func(t *testing.T) {
		t.Setenv(EnvVarPrefix+"OBSERVABILITY_LOGGING_SERVICE_NAME", "from-env")
		t.Setenv(EnvVarPrefix+"OBSERVABILITY_LOGGING_LEVEL", "error")

		cfg, err := Load(context.Background(), Options{ServiceName: "from-opts", LogLevel: "info"})
		require.NoError(t, err)

		// The env var wins over the option-seeded default.
		assert.Equal(t, "from-env", cfg.Observability.Logging.ServiceName)
		assert.True(t, logging.LevelsEqual(logging.ErrorLevel, cfg.Observability.Logging.Level))
		// A field with no env var keeps the default set by New.
		assert.Equal(t, loggingcfg.ProviderSlog, cfg.Observability.Logging.Provider)
	})
}

func TestLoadFromFile(t *testing.T) {
	t.Run("decodes a complete config file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, []byte(
			`{"observability":{"logging":{"provider":"slog","serviceName":"from-file","level":"warn"}}}`,
		), 0o600))

		cfg, err := LoadFromFile(context.Background(), path)
		require.NoError(t, err)

		assert.Equal(t, "from-file", cfg.Observability.Logging.ServiceName)
		assert.Equal(t, loggingcfg.ProviderSlog, cfg.Observability.Logging.Provider)
		assert.True(t, logging.LevelsEqual(logging.WarnLevel, cfg.Observability.Logging.Level))
	})

	t.Run("environment variables overlay the file", func(t *testing.T) {
		t.Setenv(EnvVarPrefix+"OBSERVABILITY_LOGGING_SERVICE_NAME", "env-wins")

		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, []byte(
			`{"observability":{"logging":{"provider":"slog","serviceName":"from-file"}}}`,
		), 0o600))

		cfg, err := LoadFromFile(context.Background(), path)
		require.NoError(t, err)

		assert.Equal(t, "env-wins", cfg.Observability.Logging.ServiceName)
	})

	t.Run("an incomplete file fails validation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "config.json")
		require.NoError(t, os.WriteFile(path, []byte(
			`{"observability":{"logging":{"provider":"slog"}}}`,
		), 0o600))

		_, err := LoadFromFile(context.Background(), path)
		require.Error(t, err)
	})

	t.Run("a missing file is an error", func(t *testing.T) {
		_, err := LoadFromFile(context.Background(), filepath.Join(t.TempDir(), "does-not-exist.json"))
		require.Error(t, err)
	})
}

func TestLevelFromString(t *testing.T) {
	t.Parallel()

	cases := map[string]logging.Level{
		"debug":     logging.DebugLevel,
		"info":      logging.InfoLevel,
		"warn":      logging.WarnLevel,
		"warning":   logging.WarnLevel,
		"error":     logging.ErrorLevel,
		"ERROR":     logging.ErrorLevel,
		"":          logging.InfoLevel,
		"nonsense":  logging.InfoLevel,
		"  debug  ": logging.DebugLevel,
	}

	for input, want := range cases {
		assert.Truef(t, logging.LevelsEqual(want, levelFromString(input)), "input %q", input)
	}
}
