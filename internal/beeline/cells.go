package beeline

import (
	"fmt"

	"github.com/uber/h3-go/v4"
)

// Center returns the geographic center of a cell. Every estimate is
// center-to-center, so this is the coordinate the engine routes from/to (§9).
func Center(c H3Cell) (LatLng, error) {
	ll, err := h3.CellToLatLng(c)
	if err != nil {
		return LatLng{}, fmt.Errorf("cell %s center: %w", c, err)
	}

	return LatLng{Lat: ll.Lat, Lng: ll.Lng}, nil
}

// CellAt returns the cell containing the given point at the given resolution.
func CellAt(p LatLng, resolution int) (H3Cell, error) {
	c, err := h3.LatLngToCell(h3.NewLatLng(p.Lat, p.Lng), resolution)
	if err != nil {
		return 0, fmt.Errorf("locating cell for (%f,%f) at res %d: %w", p.Lat, p.Lng, resolution, err)
	}

	return c, nil
}
