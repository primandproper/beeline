package registry_test

import (
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/engine/registry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuild(t *testing.T) {
	t.Parallel()

	speeds := map[beeline.Profile]float64{"car": 13.9}

	t.Run("always registers the built-in haversine default", func(t *testing.T) {
		t.Parallel()

		engines, err := registry.Build(nil, speeds, config.EngineLatencyConfig{})
		require.NoError(t, err)
		require.Contains(t, engines, config.DefaultProviderName)
		assert.True(t, engines[config.DefaultProviderName].Capabilities().SupportsDistance)
	})

	t.Run("does not register latent-haversine when latency is disabled", func(t *testing.T) {
		t.Parallel()

		engines, err := registry.Build(nil, speeds, config.EngineLatencyConfig{Enabled: false})
		require.NoError(t, err)
		assert.Contains(t, engines, config.DefaultProviderName)
		assert.NotContains(t, engines, config.LatentHaversineProviderName)
	})

	t.Run("registers latent-haversine alongside the raw default when latency is enabled", func(t *testing.T) {
		t.Parallel()

		engines, err := registry.Build(nil, speeds, config.EngineLatencyConfig{
			Enabled: true,
			Min:     10 * time.Millisecond,
			Max:     20 * time.Millisecond,
		})
		require.NoError(t, err)
		// The default stays present and separate from the latent provider — enabling
		// latency adds an entry rather than replacing the default.
		require.Contains(t, engines, config.DefaultProviderName)
		require.Contains(t, engines, config.LatentHaversineProviderName)
		assert.NotSame(t, engines[config.DefaultProviderName], engines[config.LatentHaversineProviderName])
		// Both expose the same routing capabilities; the wrapper only adds delay.
		assert.True(t, engines[config.LatentHaversineProviderName].Capabilities().SupportsDistance)
	})

	t.Run("builds a named osrm provider alongside the default", func(t *testing.T) {
		t.Parallel()

		engines, err := registry.Build(map[string]config.ProviderConfig{
			"osrm-west": {
				Type:         config.ProviderTypeOSRM,
				BaseURL:      "http://osrm-west:5000",
				Profiles:     map[string]string{"car": "driving"},
				MaxTableSize: 10000,
				Timeout:      5 * time.Second,
			},
		}, speeds, config.EngineLatencyConfig{})
		require.NoError(t, err)

		require.Contains(t, engines, "osrm-west")
		caps := engines["osrm-west"].Capabilities()
		assert.True(t, caps.SupportsDistance)
		assert.Equal(t, 10000, caps.MaxTableSize)
		assert.Contains(t, caps.SupportedProfiles, beeline.Profile("car"))
		// The default is still present.
		assert.Contains(t, engines, config.DefaultProviderName)
	})

	t.Run("builds an extra named haversine provider", func(t *testing.T) {
		t.Parallel()

		engines, err := registry.Build(map[string]config.ProviderConfig{
			"fast": {Type: config.ProviderTypeHaversine},
		}, speeds, config.EngineLatencyConfig{})
		require.NoError(t, err)
		require.Contains(t, engines, "fast")
		assert.Contains(t, engines["fast"].Capabilities().SupportedProfiles, beeline.Profile("car"))
	})

	t.Run("rejects an unknown provider type", func(t *testing.T) {
		t.Parallel()

		_, err := registry.Build(map[string]config.ProviderConfig{
			"bogus": {Type: "valhalla"},
		}, speeds, config.EngineLatencyConfig{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown type")
	})
}
