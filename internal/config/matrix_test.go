package config

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMatrixConfigValidate(t *testing.T) {
	t.Parallel()

	t.Run("the built-in defaults are valid", func(t *testing.T) {
		t.Parallel()

		m := defaultMatrixConfig()
		require.NoError(t, m.validate(context.Background()))
	})

	t.Run("rejects invalid fields", func(t *testing.T) {
		t.Parallel()

		cases := map[string]func(*MatrixConfig){
			"empty database path": func(m *MatrixConfig) { m.DatabasePath = "" },
			"no profiles":         func(m *MatrixConfig) { m.Profiles = nil },
			"non-positive speed":  func(m *MatrixConfig) { m.Profiles["car"] = 0 },
			"unknown default":     func(m *MatrixConfig) { m.DefaultProfile = "hovercraft" },
			"zero ttl":            func(m *MatrixConfig) { m.TargetTTL = 0 },
			"zero workers":        func(m *MatrixConfig) { m.RefreshWorkers = 0 },
			"missing server port": func(m *MatrixConfig) { m.Server.Port = 0 },
		}

		for name, mutate := range cases {
			t.Run(name, func(t *testing.T) {
				t.Parallel()

				m := defaultMatrixConfig()
				mutate(&m)
				assert.Error(t, m.validate(context.Background()))
			})
		}
	})
}
