package haversine_test

import (
	"context"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/engine/haversine"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEngineTable(t *testing.T) {
	t.Parallel()

	const carSpeed = 10.0 // m/s, chosen so duration = distance/10 is easy to check

	engine := haversine.New(map[beeline.Profile]float64{"car": carSpeed}, 0)

	sf := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	la := beeline.LatLng{Lat: 34.0522, Lng: -118.2437}

	t.Run("dense 1x2 table with duration and distance", func(t *testing.T) {
		t.Parallel()

		resp, err := engine.Table(context.Background(), beeline.TableRequest{
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{sf, la},
			Profile:      "car",
			Want:         beeline.AnnotateDuration | beeline.AnnotateDistance,
		})
		require.NoError(t, err)
		require.Len(t, resp.Duration, 1)
		require.Len(t, resp.Duration[0], 2)
		require.Len(t, resp.Distance, 1)

		assert.Zero(t, resp.Distance[0][0])
		assert.InDelta(t, 559_000, resp.Distance[0][1], 5_000)
		assert.InDelta(t, resp.Distance[0][1]/carSpeed, resp.Duration[0][1], 1e-6)
	})

	t.Run("distance omitted when not requested", func(t *testing.T) {
		t.Parallel()

		resp, err := engine.Table(context.Background(), beeline.TableRequest{
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{la},
			Profile:      "car",
			Want:         beeline.AnnotateDuration,
		})
		require.NoError(t, err)
		assert.Nil(t, resp.Distance)
		assert.NotNil(t, resp.Duration)
	})

	t.Run("unknown profile is an error", func(t *testing.T) {
		t.Parallel()

		_, err := engine.Table(context.Background(), beeline.TableRequest{
			Sources:      []beeline.LatLng{sf},
			Destinations: []beeline.LatLng{la},
			Profile:      "spaceship",
			Want:         beeline.AnnotateDuration,
		})
		assert.Error(t, err)
	})

	t.Run("capabilities report the configured profile and distance support", func(t *testing.T) {
		t.Parallel()

		caps := engine.Capabilities()
		assert.True(t, caps.SupportsDistance)
		assert.Contains(t, caps.SupportedProfiles, beeline.Profile("car"))
	})
}
