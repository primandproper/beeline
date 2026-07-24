package registry_test

import (
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/engine/registry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuiltinSpecs(t *testing.T) {
	t.Parallel()

	t.Run("always includes the raw haversine default", func(t *testing.T) {
		t.Parallel()

		specs := registry.BuiltinSpecs(false, 0, 0)
		require.Len(t, specs, 1)
		assert.Equal(t, beeline.DefaultProviderName, specs[0].Name)
		assert.Equal(t, beeline.ProviderTypeHaversine, specs[0].Type)
	})

	t.Run("adds latent-haversine alongside the raw default when latency is enabled", func(t *testing.T) {
		t.Parallel()

		specs := registry.BuiltinSpecs(true, 10*time.Millisecond, 20*time.Millisecond)
		require.Len(t, specs, 2)
		assert.Equal(t, beeline.LatentHaversineProviderName, specs[1].Name)
		assert.Equal(t, beeline.ProviderTypeLatentHaversine, specs[1].Type)
		assert.Equal(t, int64(10), specs[1].LatencyMinMs)
		assert.Equal(t, int64(20), specs[1].LatencyMaxMs)
	})
}

func TestBuildAll(t *testing.T) {
	t.Parallel()

	speeds := map[beeline.Profile]float64{"car": 13.9}

	t.Run("builds the built-ins plus named providers", func(t *testing.T) {
		t.Parallel()

		specs := append(registry.BuiltinSpecs(true, time.Millisecond, 2*time.Millisecond),
			beeline.ProviderSpec{
				Name:         "osrm-west",
				Type:         beeline.ProviderTypeOSRM,
				BaseURL:      "http://osrm-west:5000",
				Profiles:     map[string]string{"car": "driving"},
				MaxTableSize: 10000,
				TimeoutMs:    5000,
			},
			beeline.ProviderSpec{Name: "flat-earth", Type: beeline.ProviderTypeHaversine},
		)

		engines, err := registry.BuildAll(specs, speeds)
		require.NoError(t, err)
		require.Len(t, engines, 4)

		require.Contains(t, engines, beeline.DefaultProviderName)
		require.Contains(t, engines, beeline.LatentHaversineProviderName)
		assert.NotSame(t, engines[beeline.DefaultProviderName], engines[beeline.LatentHaversineProviderName])

		caps := engines["osrm-west"].Capabilities()
		assert.True(t, caps.SupportsDistance)
		assert.Equal(t, 10000, caps.MaxTableSize)
		assert.Contains(t, caps.SupportedProfiles, beeline.Profile("car"))

		assert.Contains(t, engines["flat-earth"].Capabilities().SupportedProfiles, beeline.Profile("car"))
	})

	t.Run("rejects an invalid spec instead of registering a nil engine", func(t *testing.T) {
		t.Parallel()

		_, err := registry.BuildAll([]beeline.ProviderSpec{
			{Name: "broken", Type: beeline.ProviderTypeOSRM}, // no baseURL
		}, speeds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "baseURL")
	})

	t.Run("rejects an unknown provider type", func(t *testing.T) {
		t.Parallel()

		_, err := registry.BuildAll([]beeline.ProviderSpec{
			{Name: "bogus", Type: "valhalla"},
		}, speeds)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unknown type")
	})
}
