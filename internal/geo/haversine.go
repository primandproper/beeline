// Package geo holds small, pure geographic primitives. It has no dependencies
// beyond the standard library and the domain's LatLng type, so it is trivially
// testable and reusable across the engine and the read path.
package geo

import (
	"math"

	"github.com/primandproper/beeline/internal/beeline"
)

// earthRadiusMeters is the mean Earth radius used for great-circle distance. It
// matches the value H3 uses, so estimates stay consistent with cell geometry.
const earthRadiusMeters = 6371007.180918475

// Haversine returns the great-circle distance in meters between two points. This
// is the prototype's stand-in for a routing engine's road distance: exact on a
// sphere, an underestimate of real driving distance, but enough to exercise the
// whole precompute→store→serve→refresh pipeline.
func Haversine(a, b beeline.LatLng) float64 {
	lat1 := radians(a.Lat)
	lat2 := radians(b.Lat)
	dLat := radians(b.Lat - a.Lat)
	dLng := radians(b.Lng - a.Lng)

	sinLat := math.Sin(dLat / 2)
	sinLng := math.Sin(dLng / 2)

	h := sinLat*sinLat + math.Cos(lat1)*math.Cos(lat2)*sinLng*sinLng

	return 2 * earthRadiusMeters * math.Asin(math.Min(1, math.Sqrt(h)))
}

func radians(deg float64) float64 { return deg * math.Pi / 180 }
