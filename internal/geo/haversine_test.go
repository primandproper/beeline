package geo_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/geo"

	"github.com/stretchr/testify/assert"
)

func TestHaversine(t *testing.T) {
	t.Parallel()

	sf := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	la := beeline.LatLng{Lat: 34.0522, Lng: -118.2437}

	t.Run("known distance SF to LA is ~559km", func(t *testing.T) {
		t.Parallel()

		got := geo.Haversine(sf, la)
		assert.InDelta(t, 559_000, got, 5_000)
	})

	t.Run("identical points are zero distance", func(t *testing.T) {
		t.Parallel()

		assert.Zero(t, geo.Haversine(sf, sf))
	})

	t.Run("distance is symmetric", func(t *testing.T) {
		t.Parallel()

		assert.InDelta(t, geo.Haversine(sf, la), geo.Haversine(la, sf), 1e-6)
	})
}
