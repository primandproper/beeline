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
		assert.False(t, m.Telemetry.Enabled(), "telemetry capture is opt-in")
	})

	t.Run("zero refresh workers is coordinator-only mode, not an error", func(t *testing.T) {
		t.Parallel()

		m := defaultMatrixConfig()
		m.RefreshWorkers = 0
		require.NoError(t, m.validate(context.Background()))
	})

	t.Run("a zero-value follower config validates while no leader URL is set", func(t *testing.T) {
		t.Parallel()

		m := defaultMatrixConfig()
		m.Follower = FollowerConfig{}
		require.NoError(t, m.validate(context.Background()))
	})

	t.Run("the follower defaults validate once a leader URL is set", func(t *testing.T) {
		t.Parallel()

		m := defaultMatrixConfig()
		m.Follower.LeaderURL = "http://leader:8080"
		require.NoError(t, m.validate(context.Background()), "pointing at a leader must need no other knob")
	})

	t.Run("a zero-value telemetry config validates while disabled", func(t *testing.T) {
		t.Parallel()

		m := defaultMatrixConfig()
		m.Telemetry = TelemetryConfig{}
		require.NoError(t, m.validate(context.Background()))
	})

	t.Run("the defaults validate once a telemetry channel is switched on", func(t *testing.T) {
		t.Parallel()

		m := defaultMatrixConfig()
		m.Telemetry.RawEnabled = true
		require.NoError(t, m.validate(context.Background()), "flipping a channel on must need no other knob")
		m.Telemetry.AggregateEnabled = true
		require.NoError(t, m.validate(context.Background()))
	})

	t.Run("rejects invalid fields", func(t *testing.T) {
		t.Parallel()

		cases := map[string]func(*MatrixConfig){
			"no backend postgres url": func(m *MatrixConfig) { m.Backend.Postgres.URL = "" },
			"no profiles":             func(m *MatrixConfig) { m.Profiles = nil },
			"non-positive speed":      func(m *MatrixConfig) { m.Profiles["car"] = 0 },
			"unknown default":         func(m *MatrixConfig) { m.DefaultProfile = "hovercraft" },
			"zero ttl":                func(m *MatrixConfig) { m.TargetTTL = 0 },
			"negative workers":        func(m *MatrixConfig) { m.RefreshWorkers = -1 },
			"missing server port":     func(m *MatrixConfig) { m.Server.Port = 0 },
			"follower with a garbage leader URL": func(m *MatrixConfig) {
				m.Follower.LeaderURL = "not a url"
			},
			"follower with a non-http scheme": func(m *MatrixConfig) {
				m.Follower.LeaderURL = "ftp://leader:8080"
			},
			"follower with zero workers": func(m *MatrixConfig) {
				m.Follower.LeaderURL = "http://leader:8080"
				m.Follower.Workers = 0
			},
			"follower with a negative lease": func(m *MatrixConfig) {
				m.Follower.LeaderURL = "http://leader:8080"
				m.Follower.Lease = -1
			},
			"telemetry on without a path": func(m *MatrixConfig) {
				m.Telemetry.RawEnabled = true
				m.Telemetry.Path = ""
			},
			"telemetry with unknown sink": func(m *MatrixConfig) {
				m.Telemetry.RawEnabled = true
				m.Telemetry.Sink = "kafka"
			},
			"telemetry with zero buffer": func(m *MatrixConfig) {
				m.Telemetry.AggregateEnabled = true
				m.Telemetry.BufferSize = 0
			},
			"aggregation without a bucket": func(m *MatrixConfig) {
				m.Telemetry.AggregateEnabled = true
				m.Telemetry.AggregateBucket = 0
			},
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
