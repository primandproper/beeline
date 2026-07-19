// Package haversine implements beeline.RoutingEngine with great-circle distance
// and a per-profile constant speed. It is the prototype's routing engine: it needs
// no graph, no external process, and returns in nanoseconds, which lets the rest of
// the system (tessellation, store, freshness loop, read path) be exercised end to
// end before a real engine like OSRM is wired in behind the same interface.
package haversine

import (
	"context"
	"fmt"
	"maps"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/geo"
)

// Engine derives duration from Haversine distance and a constant speed per profile.
type Engine struct {
	speeds       map[beeline.Profile]float64 // meters per second
	maxTableSize int
}

// New builds an Engine from a mode→speed (m/s) map. It copies the map so later
// mutation of the caller's map cannot change engine behavior.
func New(speeds map[beeline.Profile]float64, maxTableSize int) *Engine {
	return &Engine{speeds: maps.Clone(speeds), maxTableSize: maxTableSize}
}

// Table computes the dense Sources × Destinations matrix. Distance is filled when
// requested; duration is distance divided by the profile's speed.
func (e *Engine) Table(_ context.Context, req beeline.TableRequest) (beeline.TableResponse, error) {
	speed, ok := e.speeds[req.Profile]
	if !ok || speed <= 0 {
		return beeline.TableResponse{}, fmt.Errorf("haversine: no positive speed for profile %q", req.Profile)
	}

	if n := len(req.Sources) * len(req.Destinations); e.maxTableSize > 0 && n > e.maxTableSize {
		return beeline.TableResponse{}, fmt.Errorf("haversine: table size %d exceeds max %d", n, e.maxTableSize)
	}

	wantDist := req.Want.Has(beeline.AnnotateDistance)
	wantDur := req.Want == 0 || req.Want.Has(beeline.AnnotateDuration)

	resp := beeline.TableResponse{}
	if wantDur {
		resp.Duration = make([][]float64, len(req.Sources))
	}
	if wantDist {
		resp.Distance = make([][]float64, len(req.Sources))
	}

	for i, src := range req.Sources {
		if wantDur {
			resp.Duration[i] = make([]float64, len(req.Destinations))
		}
		if wantDist {
			resp.Distance[i] = make([]float64, len(req.Destinations))
		}

		for j, dst := range req.Destinations {
			meters := geo.Haversine(src, dst)
			if wantDist {
				resp.Distance[i][j] = meters
			}
			if wantDur {
				resp.Duration[i][j] = meters / speed
			}
		}
	}

	return resp, nil
}

// Capabilities reports the profiles this engine can route and that it can return
// distance. MaxTableSize mirrors the configured bound (0 means unbounded).
func (e *Engine) Capabilities() beeline.Capabilities {
	profiles := make([]beeline.Profile, 0, len(e.speeds))
	for p := range e.speeds {
		profiles = append(profiles, p)
	}

	return beeline.Capabilities{
		MaxTableSize:      e.maxTableSize,
		SupportedProfiles: profiles,
		SupportsDistance:  true,
	}
}
