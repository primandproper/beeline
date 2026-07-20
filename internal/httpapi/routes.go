// Package httpapi exposes the read path, the freshness contract, and the area
// control plane over HTTP. The estimate endpoint is the latency-sensitive keyed
// lookup Beeline exists to serve; /_ops_/freshness makes the §3 debt signal a
// first-class, scrapeable number (aggregate, or per-area with ?area=); the
// /_config_/areas endpoints are the CRUD + enable/disable + geometry surface over the
// SQLite-backed area registry; and /_ops_/live + /_ops_/ready are the health probes.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/query"

	"github.com/primandproper/platform-go/v4/healthcheck"
	"github.com/primandproper/platform-go/v4/observability/logging"
	"github.com/primandproper/platform-go/v4/routing"
	chirouter "github.com/primandproper/platform-go/v4/routing/chi"

	"github.com/uber/h3-go/v4"
)

// maxGeoJSONBytes bounds an uploaded polygon so a malicious body can't exhaust memory.
const maxGeoJSONBytes = 8 << 20 // 8 MiB

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
func Register(router routing.Router, deps *Deps) {
	logger := logging.EnsureLogger(deps.Logger)
	areaID := chirouter.NewRouteParamManager().BuildRouteParamIDFetcher(logger, "areaID", "area")

	router.Get("/estimate", estimateHandler(deps, logger))

	router.Get("/_ops_/freshness", freshnessHandler(deps, logger))
	router.Get("/_ops_/cells", cellsHandler(deps, logger))
	router.Get("/_ops_/live", liveHandler(logger))
	router.Get("/_ops_/ready", readyHandler(deps.Health, logger))

	router.Get("/_config_/areas", areasListHandler(deps, logger))
	router.Post("/_config_/areas", areaCreateHandler(deps, logger))
	router.Get("/_config_/areas/{areaID}", areaGetHandler(deps, logger, areaID))
	router.Patch("/_config_/areas/{areaID}", areaUpdateHandler(deps, logger, areaID))
	router.Delete("/_config_/areas/{areaID}", areaDeleteHandler(deps, logger, areaID))
	router.Post("/_config_/areas/{areaID}/enable", areaEnableHandler(deps, logger, areaID))
	router.Post("/_config_/areas/{areaID}/disable", areaDisableHandler(deps, logger, areaID))
	router.Put("/_config_/areas/{areaID}/geojson", areaGeoJSONHandler(deps, logger, areaID))
	router.Post("/_config_/areas/{areaID}/cells", areaCellsHandler(deps, logger, areaID))
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

func estimateHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
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

// freshnessHandler serves the §3 debt contract. Without ?area it reports the aggregate
// across every enabled area (the whole working set); with ?area=<id> it reports that
// one area.
func freshnessHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if raw := strings.TrimSpace(r.URL.Query().Get("area")); raw != "" {
			id, err := parseAreaID(raw)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, "invalid area: "+err.Error())
				return
			}

			stats, statErr := deps.Coordinator.DebtForArea(r.Context(), id)
			if statErr != nil {
				logger.Error("reading area freshness debt", statErr)
				writeError(w, logger, http.StatusInternalServerError, statErr.Error())
				return
			}

			writeJSON(w, logger, http.StatusOK, stats)
			return
		}

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
	Area             int64   `json:"area"`
	Lat              float64 `json:"lat"`
	Lng              float64 `json:"lng"`
	Total            int     `json:"total"`
	Fresh            int     `json:"fresh"`
	OldestAgeSeconds float64 `json:"oldestAgeSeconds"`
}

// cellsResponse is the body of GET /_ops_/cells: per-origin-cell freshness, each cell
// tagged with its area, enough for the console to paint each H3 cell by fresh fraction.
type cellsResponse struct {
	Cells []cellStateResponse `json:"cells"`
}

// cellsHandler paints one area's progress with ?area=<id>, or every enabled area's when
// omitted.
func cellsHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ids []beeline.AreaID
		if raw := strings.TrimSpace(r.URL.Query().Get("area")); raw != "" {
			id, err := parseAreaID(raw)
			if err != nil {
				writeError(w, logger, http.StatusBadRequest, "invalid area: "+err.Error())
				return
			}
			ids = []beeline.AreaID{id}
		} else {
			ids = deps.Coordinator.EnabledAreas()
		}

		cells := make([]cellStateResponse, 0)
		for _, id := range ids {
			states, err := deps.Coordinator.CellStatesForArea(r.Context(), id)
			if err != nil {
				logger.Error("reading cell states", err)
				writeError(w, logger, http.StatusInternalServerError, err.Error())
				return
			}

			for i := range states {
				center, cErr := beeline.Center(states[i].Origin)
				if cErr != nil {
					logger.Error("resolving cell center", cErr)
					continue
				}

				cells = append(cells, cellStateResponse{
					Cell:             states[i].Origin.String(),
					Area:             int64(id),
					Lat:              center.Lat,
					Lng:              center.Lng,
					Total:            states[i].Total,
					Fresh:            states[i].Fresh,
					OldestAgeSeconds: states[i].OldestAgeSeconds,
				})
			}
		}

		writeJSON(w, logger, http.StatusOK, cellsResponse{Cells: cells})
	}
}

// areaResponse is the JSON shape of a configured area. Cells and GeoJSON are populated
// only on the single-area detail view, not in list responses.
type areaResponse struct {
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
	Name            string          `json:"name"`
	GeoJSON         json.RawMessage `json:"geojson,omitempty"`
	Cells           []string        `json:"cells,omitempty"`
	ID              int64           `json:"id"`
	Resolution      int             `json:"resolution"`
	MaxRadiusMeters float64         `json:"maxRadiusMeters"`
	CellCount       int             `json:"cellCount"`
	Enabled         bool            `json:"enabled"`
}

func toAreaResponse(a *beeline.Area, includeGeometry bool) areaResponse {
	resp := areaResponse{
		ID:              int64(a.ID),
		Name:            a.Name,
		Resolution:      a.Resolution,
		MaxRadiusMeters: a.MaxRadiusMeters,
		CellCount:       len(a.Cells),
		Enabled:         a.Enabled,
		CreatedAt:       a.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		UpdatedAt:       a.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	if includeGeometry {
		resp.Cells = cellStrings(a.Cells)
		if len(a.GeoJSON) > 0 {
			resp.GeoJSON = json.RawMessage(a.GeoJSON)
		}
	}

	return resp
}

func areasListHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		areas, err := deps.Coordinator.List(r.Context())
		if err != nil {
			logger.Error("listing areas", err)
			writeError(w, logger, http.StatusInternalServerError, err.Error())
			return
		}

		out := make([]areaResponse, 0, len(areas))
		for i := range areas {
			out = append(out, toAreaResponse(&areas[i], false))
		}

		writeJSON(w, logger, http.StatusOK, out)
	}
}

// createAreaRequest is the POST /_config_/areas body. Geometry comes from either an
// inline GeoJSON polygon (polyfilled server-side) or an explicit cell set.
type createAreaRequest struct {
	Name            string          `json:"name"`
	GeoJSON         json.RawMessage `json:"geojson,omitempty"`
	Cells           []string        `json:"cells,omitempty"`
	Resolution      int             `json:"resolution"`
	MaxRadiusMeters float64         `json:"maxRadiusMeters"`
}

func areaCreateHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createAreaRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid area body: "+err.Error())
			return
		}

		cells, err := parseCells(req.Cells)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		area, err := deps.Coordinator.Create(r.Context(), &control.CreateAreaInput{
			Name:            req.Name,
			Resolution:      req.Resolution,
			MaxRadiusMeters: req.MaxRadiusMeters,
			GeoJSON:         req.GeoJSON,
			Cells:           cells,
		})
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		logger.Info("service area created")
		writeJSON(w, logger, http.StatusCreated, toAreaResponse(&area, true))
	}
}

func areaGetHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		area, err := deps.Coordinator.Get(r.Context(), id)
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		writeJSON(w, logger, http.StatusOK, toAreaResponse(&area, true))
	}
}

// updateAreaRequest is the PATCH body: an area's mutable metadata.
type updateAreaRequest struct {
	Name            string  `json:"name"`
	Resolution      int     `json:"resolution"`
	MaxRadiusMeters float64 `json:"maxRadiusMeters"`
}

func areaUpdateHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		var req updateAreaRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid area body: "+err.Error())
			return
		}

		area, err := deps.Coordinator.Update(r.Context(), id, control.UpdateAreaInput{
			Name:            req.Name,
			Resolution:      req.Resolution,
			MaxRadiusMeters: req.MaxRadiusMeters,
		})
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		writeJSON(w, logger, http.StatusOK, toAreaResponse(&area, true))
	}
}

func areaDeleteHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		if err := deps.Coordinator.Delete(r.Context(), id); err != nil {
			writeAreaError(w, logger, err)
			return
		}

		w.WriteHeader(http.StatusNoContent)
	}
}

func areaEnableHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		area, err := deps.Coordinator.Enable(r.Context(), id)
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		logger.Info("service area enabled")
		writeJSON(w, logger, http.StatusOK, toAreaResponse(&area, true))
	}
}

func areaDisableHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		area, err := deps.Coordinator.Disable(r.Context(), id)
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		logger.Info("service area disabled")
		writeJSON(w, logger, http.StatusOK, toAreaResponse(&area, true))
	}
}

// areaGeoJSONHandler replaces an area's geometry from the uploaded GeoJSON body.
func areaGeoJSONHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, maxGeoJSONBytes))
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "reading geojson body: "+err.Error())
			return
		}

		area, err := deps.Coordinator.SetGeoJSON(r.Context(), id, raw)
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		writeJSON(w, logger, http.StatusOK, toAreaResponse(&area, true))
	}
}

// cellsRequest is the POST /_config_/areas/{id}/cells body for manual hex refinement.
type cellsRequest struct {
	Add    []string `json:"add,omitempty"`
	Remove []string `json:"remove,omitempty"`
}

func areaCellsHandler(deps *Deps, logger logging.Logger, areaID func(*http.Request) uint64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id, ok := requireAreaID(w, logger, r, areaID)
		if !ok {
			return
		}

		var req cellsRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid cells body: "+err.Error())
			return
		}

		add, err := parseCells(req.Add)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}
		remove, err := parseCells(req.Remove)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		if len(remove) > 0 {
			if _, err = deps.Coordinator.RemoveCells(r.Context(), id, remove); err != nil {
				writeAreaError(w, logger, err)
				return
			}
		}

		area, err := deps.Coordinator.AddCells(r.Context(), id, add)
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		writeJSON(w, logger, http.StatusOK, toAreaResponse(&area, true))
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

// requireAreaID fetches and validates the {areaID} path param, writing a 400 and
// returning ok=false when it is missing or not a positive integer.
func requireAreaID(w http.ResponseWriter, logger logging.Logger, r *http.Request, fetch func(*http.Request) uint64) (beeline.AreaID, bool) {
	raw := fetch(r)
	if raw == 0 {
		writeError(w, logger, http.StatusBadRequest, "invalid or missing area id")
		return 0, false
	}

	return beeline.AreaID(raw), true
}

// writeAreaError maps a coordinator error to an HTTP status: not-found → 404,
// everything else (validation, geometry, store) → 400.
func writeAreaError(w http.ResponseWriter, logger logging.Logger, err error) {
	if isNotFound(err) {
		writeError(w, logger, http.StatusNotFound, err.Error())
		return
	}

	writeError(w, logger, http.StatusBadRequest, err.Error())
}

// isNotFound reports whether err signals a missing area. The SQLite store's ErrNotFound
// message ends in "area not found"; matching on that keeps httpapi free of a direct
// dependency on the store package.
func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// parseCells converts hex H3 strings to cells, rejecting any that don't parse.
func parseCells(raw []string) ([]beeline.H3Cell, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	cells := make([]beeline.H3Cell, 0, len(raw))
	for _, s := range raw {
		cell := h3.CellFromString(strings.TrimSpace(s))
		if !cell.IsValid() {
			return nil, errors.New("invalid h3 cell: " + s)
		}
		cells = append(cells, cell)
	}

	return cells, nil
}

// cellStrings renders a cell set as hex strings.
func cellStrings(cells []beeline.H3Cell) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.String())
	}

	return out
}

// parseAreaID parses a positive area id from a query-string value.
func parseAreaID(raw string) (beeline.AreaID, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("area id must be a positive integer")
	}

	return beeline.AreaID(id), nil
}

// decodeJSON strictly decodes a JSON request body into v.
func decodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxGeoJSONBytes))
	return dec.Decode(v)
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
