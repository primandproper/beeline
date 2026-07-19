// Package httpapi exposes the read path and the freshness contract over HTTP. The
// estimate endpoint is the latency-sensitive keyed lookup Beeline exists to serve;
// the /_ops_/freshness endpoint makes the §3 debt signal a first-class, scrapeable
// number, and /_ops_/live + /_ops_/ready are the health probes (the chi router
// already skips these paths from tracing/logging noise).
package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/query"

	"github.com/primandproper/platform-go/v4/healthcheck"
	"github.com/primandproper/platform-go/v4/observability/logging"
	"github.com/primandproper/platform-go/v4/routing"
)

// Deps are the dependencies the routes close over.
type Deps struct {
	Handler        *query.Handler
	Index          beeline.FreshnessIndex
	Coordinator    *control.Coordinator
	Health         healthcheck.Registry
	Logger         logging.Logger
	DefaultProfile beeline.Profile
}

// Register attaches all routes to the router.
func Register(router routing.Router, deps Deps) {
	logger := logging.EnsureLogger(deps.Logger)

	router.Get("/estimate", estimateHandler(deps, logger))
	router.Get("/_ops_/freshness", freshnessHandler(deps, logger))
	router.Get("/_ops_/cells", cellsHandler(deps, logger))
	router.Get("/_ops_/live", liveHandler(logger))
	router.Get("/_ops_/ready", readyHandler(deps.Health, logger))
	router.Get("/_config_/area", areaGetHandler(deps, logger))
	router.Post("/_config_/area", areaPostHandler(deps, logger))
}

// estimateResponse is the JSON body for a successful estimate.
type estimateResponse struct {
	ComputedAt     string  `json:"computedAt"`
	Source         string  `json:"source"`
	Profile        string  `json:"profile"`
	DurationSec    float64 `json:"durationSec"`
	DistanceMeters float64 `json:"distanceMeters"`
	Stale          bool    `json:"stale"`
}

func estimateHandler(deps Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin, err := parseLatLng(r.URL.Query().Get("origin"))
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid origin: "+err.Error())
			return
		}

		dest, err := parseLatLng(r.URL.Query().Get("dest"))
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid dest: "+err.Error())
			return
		}

		profile := deps.DefaultProfile
		if p := strings.TrimSpace(r.URL.Query().Get("profile")); p != "" {
			profile = beeline.Profile(p)
		}

		result, err := deps.Handler.Estimate(r.Context(), origin, dest, profile)
		if err != nil {
			logger.Error("computing estimate", err)
			writeError(w, logger, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, logger, http.StatusOK, estimateResponse{
			DurationSec:    result.Estimate.Duration,
			DistanceMeters: result.Estimate.Distance,
			ComputedAt:     result.ComputedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Source:         string(result.Source),
			Profile:        string(profile),
			Stale:          result.Stale,
		})
	}
}

func freshnessHandler(deps Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		stats, err := deps.Index.Debt(r.Context())
		if err != nil {
			logger.Error("reading freshness debt", err)
			writeError(w, logger, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, logger, http.StatusOK, stats)
	}
}

// cellStateResponse is one origin cell's freshness rollup for the progress map.
type cellStateResponse struct {
	Cell             string  `json:"cell"` // H3 index, hex string (h3-js compatible)
	Lat              float64 `json:"lat"`
	Lng              float64 `json:"lng"`
	Total            int     `json:"total"`
	Fresh            int     `json:"fresh"`
	OldestAgeSeconds float64 `json:"oldestAgeSeconds"`
}

// cellsResponse is the body of GET /_ops_/cells: the area in force plus per-cell
// freshness, enough for the frontend to paint each H3 cell by its fresh fraction.
type cellsResponse struct {
	Cells []cellStateResponse `json:"cells"`
	Area  control.Summary     `json:"area"`
}

func cellsHandler(deps Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Coordinator == nil {
			writeError(w, logger, http.StatusServiceUnavailable, "control plane not configured")
			return
		}

		states, err := deps.Coordinator.CellStates(r.Context())
		if err != nil {
			logger.Error("reading cell states", err)
			writeError(w, logger, http.StatusInternalServerError, err.Error())
			return
		}

		cells := make([]cellStateResponse, 0, len(states))
		for i := range states {
			center, cErr := beeline.Center(states[i].Origin)
			if cErr != nil {
				logger.Error("resolving cell center", cErr)
				continue
			}

			cells = append(cells, cellStateResponse{
				Cell:             states[i].Origin.String(),
				Lat:              center.Lat,
				Lng:              center.Lng,
				Total:            states[i].Total,
				Fresh:            states[i].Fresh,
				OldestAgeSeconds: states[i].OldestAgeSeconds,
			})
		}

		writeJSON(w, logger, http.StatusOK, cellsResponse{
			Area:  deps.Coordinator.Current(),
			Cells: cells,
		})
	}
}

func areaGetHandler(deps Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if deps.Coordinator == nil {
			writeError(w, logger, http.StatusServiceUnavailable, "control plane not configured")
			return
		}

		writeJSON(w, logger, http.StatusOK, deps.Coordinator.Current())
	}
}

func areaPostHandler(deps Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if deps.Coordinator == nil {
			writeError(w, logger, http.StatusServiceUnavailable, "control plane not configured")
			return
		}

		var area control.Area
		if err := json.NewDecoder(r.Body).Decode(&area); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid area body: "+err.Error())
			return
		}

		summary, err := deps.Coordinator.Apply(r.Context(), area)
		if err != nil {
			// Apply's errors are validation/tessellation failures on caller input.
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		logger.Info("service area re-tessellated via control plane")

		writeJSON(w, logger, http.StatusOK, summary)
	}
}

func liveHandler(logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, logger, http.StatusOK, map[string]string{"status": string(healthcheck.StatusUp)})
	}
}

func readyHandler(registry healthcheck.Registry, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if registry == nil {
			writeJSON(w, logger, http.StatusOK, map[string]string{"status": string(healthcheck.StatusUp)})
			return
		}

		result := registry.CheckAll(r.Context())
		status := http.StatusOK
		if result.Status == healthcheck.StatusDown {
			status = http.StatusServiceUnavailable
		}

		writeJSON(w, logger, status, result)
	}
}

// parseLatLng parses a "lat,lng" pair in decimal degrees.
func parseLatLng(raw string) (beeline.LatLng, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return beeline.LatLng{}, errors.New("empty coordinate")
	}

	parts := strings.Split(raw, ",")
	if len(parts) != 2 {
		return beeline.LatLng{}, errors.New(`expected "lat,lng"`)
	}

	lat, err := strconv.ParseFloat(strings.TrimSpace(parts[0]), 64)
	if err != nil {
		return beeline.LatLng{}, errors.New("latitude is not a number")
	}

	lng, err := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64)
	if err != nil {
		return beeline.LatLng{}, errors.New("longitude is not a number")
	}

	if lat < -90 || lat > 90 {
		return beeline.LatLng{}, errors.New("latitude out of range [-90,90]")
	}
	if lng < -180 || lng > 180 {
		return beeline.LatLng{}, errors.New("longitude out of range [-180,180]")
	}

	return beeline.LatLng{Lat: lat, Lng: lng}, nil
}

func writeJSON(w http.ResponseWriter, logger logging.Logger, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		logger.Error("encoding response body", err)
	}
}

func writeError(w http.ResponseWriter, logger logging.Logger, status int, message string) {
	writeJSON(w, logger, status, map[string]string{"error": message})
}
