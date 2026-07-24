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
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"
	"github.com/primandproper/beeline/internal/query"
	"github.com/primandproper/beeline/internal/tessellate"

	"github.com/primandproper/platform-go/v4/healthcheck"
	"github.com/primandproper/platform-go/v4/observability/logging"
	"github.com/primandproper/platform-go/v4/routing"
	chirouter "github.com/primandproper/platform-go/v4/routing/chi"

	"github.com/uber/h3-go/v4"
)

// maxGeoJSONBytes bounds an uploaded polygon so a malicious body can't exhaust memory.
const maxGeoJSONBytes = 8 << 20 // 8 MiB

// maxTableCells bounds the grid a single /table request may ask for (sources ×
// destinations), so one call can't pin an unbounded amount of memory or engine work.
// It is a prototype guard; a real deploy would make it configurable and tie the fill
// path to the engine's Capabilities.MaxTableSize.
const maxTableCells = 10_000

// Deps are the dependencies the routes close over. Store is read directly only by
// the /_ops_/pairs cache probe; the read path proper goes through Handler.
type Deps struct {
	Handler        *query.Handler
	Index          beeline.FreshnessIndex
	Store          beeline.Store
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
	router.Post("/table", tableHandler(deps, logger))

	router.Get("/_ops_/freshness", freshnessHandler(deps, logger))
	router.Get("/_ops_/cells", cellsHandler(deps, logger))
	router.Post("/_ops_/pairs", pairsHandler(deps, logger))
	router.Get("/_ops_/live", liveHandler(logger))
	router.Get("/_ops_/ready", readyHandler(deps.Health, logger))

	router.Get("/_config_/providers", providersListHandler(deps, logger))
	router.Get("/_config_/areas", areasListHandler(deps, logger))
	router.Post("/_config_/areas", areaCreateHandler(deps, logger))
	router.Get("/_config_/areas/{areaID}", areaGetHandler(deps, logger, areaID))
	router.Patch("/_config_/areas/{areaID}", areaUpdateHandler(deps, logger, areaID))
	router.Delete("/_config_/areas/{areaID}", areaDeleteHandler(deps, logger, areaID))
	router.Post("/_config_/areas/{areaID}/enable", areaEnableHandler(deps, logger, areaID))
	router.Post("/_config_/areas/{areaID}/disable", areaDisableHandler(deps, logger, areaID))
	router.Put("/_config_/areas/{areaID}/geojson", areaGeoJSONHandler(deps, logger, areaID))
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

// tableRequest is the JSON body for a sparse batch estimate. Sources and Destinations
// are "lat,lng" strings (the same form as /estimate's query params); Skip names grid
// cells [sourceIdx, destIdx] to omit; Fill (default true when omitted) decides whether
// misses are demand-filled through the engine or left absent.
type tableRequest struct {
	Fill         *bool    `json:"fill"`
	Profile      string   `json:"profile"`
	Sources      []string `json:"sources"`
	Destinations []string `json:"destinations"`
	Skip         [][2]int `json:"skip"`
}

// tableMeta is the per-request rollup describing how the grid resolved.
type tableMeta struct {
	Cells     int `json:"cells"`
	Skipped   int `json:"skipped"`
	Hits      int `json:"hits"`
	Misses    int `json:"misses"`
	Filled    int `json:"filled"`
	SameCell  int `json:"sameCell"`
	OutOfArea int `json:"outOfArea"`
}

// tableResponse is the dense result. Durations/Distances are sources × destinations;
// a nil entry (JSON null) marks a skipped cell, or an uncomputed miss when fill=false.
type tableResponse struct {
	Profile   string       `json:"profile"`
	Durations [][]*float64 `json:"durations"`
	Distances [][]*float64 `json:"distances"`
	Meta      tableMeta    `json:"meta"`
}

func tableHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req tableRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid table body: "+err.Error())
			return
		}

		if len(req.Sources) == 0 || len(req.Destinations) == 0 {
			writeError(w, logger, http.StatusBadRequest, "sources and destinations must both be non-empty")
			return
		}
		if len(req.Sources)*len(req.Destinations) > maxTableCells {
			writeError(w, logger, http.StatusBadRequest,
				"table too large: sources × destinations exceeds "+strconv.Itoa(maxTableCells))
			return
		}

		sources, err := parseLatLngs(req.Sources)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid source: "+err.Error())
			return
		}

		destinations, err := parseLatLngs(req.Destinations)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid destination: "+err.Error())
			return
		}

		skip, err := parseSkip(req.Skip, len(sources), len(destinations))
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid skip: "+err.Error())
			return
		}

		profile := deps.DefaultProfile
		if p := strings.TrimSpace(req.Profile); p != "" {
			profile = beeline.Profile(p)
		}

		fill := true
		if req.Fill != nil {
			fill = *req.Fill
		}

		result, err := deps.Handler.Table(r.Context(), &query.TableQuery{
			Sources:      sources,
			Destinations: destinations,
			Profile:      profile,
			Skip:         skip,
			Fill:         fill,
		})
		if err != nil {
			logger.Error("computing table", err)
			writeError(w, logger, http.StatusInternalServerError, err.Error())
			return
		}

		writeJSON(w, logger, http.StatusOK, toTableResponse(result, string(profile)))
	}
}

// parseLatLngs parses a slice of "lat,lng" strings, reporting the offending index.
func parseLatLngs(raw []string) ([]beeline.LatLng, error) {
	out := make([]beeline.LatLng, len(raw))
	for i := range raw {
		p, err := parseLatLng(raw[i])
		if err != nil {
			return nil, errors.New("index " + strconv.Itoa(i) + ": " + err.Error())
		}
		out[i] = p
	}

	return out, nil
}

// parseSkip validates the [sourceIdx, destIdx] skip pairs against the grid bounds and
// collapses them into a set. An empty list yields an empty set (nothing skipped).
func parseSkip(raw [][2]int, rows, cols int) (map[[2]int]struct{}, error) {
	set := make(map[[2]int]struct{}, len(raw))
	for _, cell := range raw {
		if cell[0] < 0 || cell[0] >= rows || cell[1] < 0 || cell[1] >= cols {
			return nil, errors.New("cell out of range: [" +
				strconv.Itoa(cell[0]) + "," + strconv.Itoa(cell[1]) + "]")
		}
		set[cell] = struct{}{}
	}

	return set, nil
}

// toTableResponse projects a query.TableResult onto the dense wire matrices, leaving a
// nil (JSON null) wherever a cell is absent (skipped, or an unfilled miss).
func toTableResponse(result query.TableResult, profile string) tableResponse {
	rows := len(result.Cells)

	durations := make([][]*float64, rows)
	distances := make([][]*float64, rows)
	cells := 0

	for i := range result.Cells {
		cols := len(result.Cells[i])
		cells += cols
		durations[i] = make([]*float64, cols)
		distances[i] = make([]*float64, cols)

		for j := range result.Cells[i] {
			cell := result.Cells[i][j]
			if !cell.Present {
				continue
			}
			dur, dist := cell.Estimate.Duration, cell.Estimate.Distance
			durations[i][j] = &dur
			distances[i][j] = &dist
		}
	}

	return tableResponse{
		Profile:   profile,
		Durations: durations,
		Distances: distances,
		Meta: tableMeta{
			Cells:     cells,
			Skipped:   result.Skipped,
			Hits:      result.Hits,
			Misses:    result.Misses,
			Filled:    result.Filled,
			SameCell:  result.SameCell,
			OutOfArea: result.OutOfArea,
		},
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

// cellStateResponse is one origin cell's freshness rollup for the progress map. A
// multi-layer area's cells arrive at several resolutions side by side; Resolution
// (decoded from the cell id) lets the console tell the layers apart.
type cellStateResponse struct {
	Cell             string  `json:"cell"` // H3 index, hex string (h3-js compatible)
	Area             int64   `json:"area"`
	Resolution       int     `json:"resolution"`
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
					Resolution:       states[i].Origin.Resolution(),
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

// maxPairsDests bounds a single /_ops_/pairs probe so one hover can't ask for an
// unbounded BatchGet.
const maxPairsDests = 512

// pairsRequest asks for the cached estimates from one origin cell to a set of
// destination cells (all hex H3 strings at the same resolution). Unlike /estimate
// and /table — which route through the area's finest layer — the lookup is keyed at
// the cells' own resolution, so any layer's cached pairs are inspectable.
type pairsRequest struct {
	Profile string   `json:"profile"`
	Origin  string   `json:"origin"`
	Dests   []string `json:"dests"`
	Area    int64    `json:"area"`
}

// pairEstimateResponse is one cached origin→dest estimate. Destinations with no
// cached estimate are simply absent from the response.
type pairEstimateResponse struct {
	Dest           string  `json:"dest"`
	ComputedAt     string  `json:"computedAt"`
	DurationSec    float64 `json:"durationSec"`
	DistanceMeters float64 `json:"distanceMeters"`
}

type pairsResponse struct {
	Pairs []pairEstimateResponse `json:"pairs"`
}

// pairsHandler is the console's hover probe: a pure cache read (never the engine)
// over the store seam, returning whichever of the asked origin→dest pairs are
// cached right now.
func pairsHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req pairsRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid pairs body: "+err.Error())
			return
		}
		if req.Area <= 0 {
			writeError(w, logger, http.StatusBadRequest, "area must be a positive id")
			return
		}
		if len(req.Dests) == 0 {
			writeError(w, logger, http.StatusBadRequest, "dests must be non-empty")
			return
		}
		if len(req.Dests) > maxPairsDests {
			writeError(w, logger, http.StatusBadRequest,
				"too many dests: limit "+strconv.Itoa(maxPairsDests))
			return
		}

		origin, err := parseCell(req.Origin)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid origin: "+err.Error())
			return
		}

		profile := deps.DefaultProfile
		if p := strings.TrimSpace(req.Profile); p != "" {
			profile = beeline.Profile(p)
		}

		keys := make([]beeline.PairKey, 0, len(req.Dests))
		dests := make([]beeline.H3Cell, 0, len(req.Dests))
		for _, raw := range req.Dests {
			dest, destErr := parseCell(raw)
			if destErr != nil {
				writeError(w, logger, http.StatusBadRequest, "invalid dest: "+destErr.Error())
				return
			}
			if dest.Resolution() != origin.Resolution() {
				writeError(w, logger, http.StatusBadRequest,
					"dest "+raw+" is resolution "+strconv.Itoa(dest.Resolution())+
						", want the origin's "+strconv.Itoa(origin.Resolution()))
				return
			}
			dests = append(dests, dest)
			keys = append(keys, beeline.PairKey{
				Area:    beeline.AreaID(req.Area),
				Origin:  origin,
				Dest:    dest,
				Profile: profile,
				Res:     origin.Resolution(),
			})
		}

		stored, err := deps.Store.BatchGet(r.Context(), keys)
		if err != nil {
			logger.Error("reading cached pairs", err)
			writeError(w, logger, http.StatusInternalServerError, err.Error())
			return
		}

		pairs := make([]pairEstimateResponse, 0, len(stored))
		for i, s := range stored {
			if s == nil {
				continue
			}
			pairs = append(pairs, pairEstimateResponse{
				Dest:           dests[i].String(),
				DurationSec:    s.Duration,
				DistanceMeters: s.Distance,
				ComputedAt:     s.ComputedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			})
		}

		writeJSON(w, logger, http.StatusOK, pairsResponse{Pairs: pairs})
	}
}

// parseCell converts one hex H3 string to a cell, rejecting garbage.
func parseCell(raw string) (beeline.H3Cell, error) {
	cell := h3.CellFromString(strings.TrimSpace(raw))
	if !cell.IsValid() {
		return 0, errors.New("invalid h3 cell: " + raw)
	}

	return cell, nil
}

// layerResponse is one precision layer of an area on the wire. CellCount is derived
// (the layer's polyfill size) and only computed on single-area detail views; list
// responses leave it 0.
type layerResponse struct {
	Resolution        int     `json:"resolution"`
	MinDistanceMeters float64 `json:"minDistanceMeters"`
	MaxRadiusMeters   float64 `json:"maxRadiusMeters"`
	CoreRadiusMeters  float64 `json:"coreRadiusMeters"`
	CellCount         int     `json:"cellCount"`
}

// areaResponse is the JSON shape of a configured area. Layers is ordered
// finest→coarsest. Cells (the finest layer's polyfill, for the console map) and
// GeoJSON are populated only on the single-area detail view, not in list responses.
type areaResponse struct {
	CreatedAt       string          `json:"createdAt"`
	UpdatedAt       string          `json:"updatedAt"`
	Name            string          `json:"name"`
	WarmStrategy    string          `json:"warmStrategy"`
	RoutingProvider string          `json:"routingProvider"`
	DemandIdleTTL   string          `json:"demandIdleTTL"`
	TargetTTL       string          `json:"targetTTL"`
	LeaseDuration   string          `json:"leaseDuration"`
	SweepInterval   string          `json:"sweepInterval"`
	GeoJSON         json.RawMessage `json:"geojson,omitempty"`
	Cells           []string        `json:"cells,omitempty"`
	Layers          []layerResponse `json:"layers"`
	ID              int64           `json:"id"`
	Enabled         bool            `json:"enabled"`
}

// toAreaResponse projects an area onto the wire. The detail view (includeGeometry)
// derives each layer's cell count — and the finest layer's cell list, which the
// console map draws — by polyfilling the area's GeoJSON on the spot; cells are not
// persisted. The polyfill is best-effort: a geometry that fails to fill logs and
// leaves the counts 0 rather than failing the whole response.
func toAreaResponse(logger logging.Logger, a *beeline.Area, includeGeometry bool) areaResponse {
	resp := areaResponse{
		ID:              int64(a.ID),
		Name:            a.Name,
		WarmStrategy:    string(a.WarmStrategy),
		RoutingProvider: a.RoutingProvider,
		DemandIdleTTL:   a.DemandIdleTTL.String(),
		TargetTTL:       a.TargetTTL.String(),
		LeaseDuration:   a.LeaseDuration.String(),
		SweepInterval:   a.SweepInterval.String(),
		Enabled:         a.Enabled,
		CreatedAt:       a.CreatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
		UpdatedAt:       a.UpdatedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
	}

	resp.Layers = make([]layerResponse, 0, len(a.Layers))
	for i := range a.Layers {
		resp.Layers = append(resp.Layers, layerResponse{
			Resolution:        a.Layers[i].Resolution,
			MinDistanceMeters: a.Layers[i].MinDistanceMeters,
			MaxRadiusMeters:   a.Layers[i].MaxRadiusMeters,
			CoreRadiusMeters:  a.Layers[i].CoreRadiusMeters,
		})
	}

	if includeGeometry && len(a.GeoJSON) > 0 {
		resp.GeoJSON = json.RawMessage(a.GeoJSON)
		for i := range a.Layers {
			cells, err := tessellate.CellsFromGeoJSON(a.GeoJSON, a.Layers[i].Resolution)
			if err != nil {
				logger.Error("polyfilling layer for area detail", err)
				continue
			}
			resp.Layers[i].CellCount = len(cells)
			if i == 0 {
				resp.Cells = cellStrings(cells)
			}
		}
	}

	return resp
}

// providersListHandler returns the configured routing-provider names (default first)
// so the operator console can populate its provider picker from live config.
func providersListHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, logger, http.StatusOK, deps.Coordinator.ProviderNames())
	}
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
			out = append(out, toAreaResponse(logger, &areas[i], false))
		}

		writeJSON(w, logger, http.StatusOK, out)
	}
}

// layerPayload is one precision layer in a create/update request body.
type layerPayload struct {
	Resolution        int     `json:"resolution"`
	MinDistanceMeters float64 `json:"minDistanceMeters"`
	MaxRadiusMeters   float64 `json:"maxRadiusMeters"`
	CoreRadiusMeters  float64 `json:"coreRadiusMeters"`
}

// toLayers converts the wire layers to domain layers; ordering and validation are
// the coordinator's job.
func toLayers(in []layerPayload) []beeline.Layer {
	out := make([]beeline.Layer, 0, len(in))
	for i := range in {
		out = append(out, beeline.Layer{
			Resolution:        in[i].Resolution,
			MinDistanceMeters: in[i].MinDistanceMeters,
			MaxRadiusMeters:   in[i].MaxRadiusMeters,
			CoreRadiusMeters:  in[i].CoreRadiusMeters,
		})
	}

	return out
}

// createAreaRequest is the POST /_config_/areas body. GeoJSON is the required
// canonical geometry; every layer's cell set is derived from it server-side.
type createAreaRequest struct {
	Name            string          `json:"name"`
	WarmStrategy    string          `json:"warmStrategy"`
	RoutingProvider string          `json:"routingProvider"`
	DemandIdleTTL   string          `json:"demandIdleTTL"`
	TargetTTL       string          `json:"targetTTL"`
	LeaseDuration   string          `json:"leaseDuration"`
	SweepInterval   string          `json:"sweepInterval"`
	GeoJSON         json.RawMessage `json:"geojson"`
	Layers          []layerPayload  `json:"layers"`
}

func areaCreateHandler(deps *Deps, logger logging.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createAreaRequest
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid area body: "+err.Error())
			return
		}

		ttl, err := parseDuration(req.DemandIdleTTL)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid demandIdleTTL: "+err.Error())
			return
		}

		fresh, err := parseFreshness(req.TargetTTL, req.LeaseDuration, req.SweepInterval)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		area, err := deps.Coordinator.Create(r.Context(), &control.CreateAreaInput{
			Name:            req.Name,
			WarmStrategy:    beeline.WarmStrategy(req.WarmStrategy),
			RoutingProvider: req.RoutingProvider,
			DemandIdleTTL:   ttl,
			TargetTTL:       fresh.targetTTL,
			LeaseDuration:   fresh.lease,
			SweepInterval:   fresh.sweep,
			GeoJSON:         req.GeoJSON,
			Layers:          toLayers(req.Layers),
		})
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		logger.Info("service area created")
		writeJSON(w, logger, http.StatusCreated, toAreaResponse(logger, &area, true))
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

		writeJSON(w, logger, http.StatusOK, toAreaResponse(logger, &area, true))
	}
}

// updateAreaRequest is the PATCH body: an area's mutable metadata. Layers replaces
// the whole layer list; geometry changes through PUT …/geojson.
type updateAreaRequest struct {
	Name            string         `json:"name"`
	WarmStrategy    string         `json:"warmStrategy"`
	RoutingProvider string         `json:"routingProvider"`
	DemandIdleTTL   string         `json:"demandIdleTTL"`
	TargetTTL       string         `json:"targetTTL"`
	LeaseDuration   string         `json:"leaseDuration"`
	SweepInterval   string         `json:"sweepInterval"`
	Layers          []layerPayload `json:"layers"`
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

		ttl, err := parseDuration(req.DemandIdleTTL)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, "invalid demandIdleTTL: "+err.Error())
			return
		}

		fresh, err := parseFreshness(req.TargetTTL, req.LeaseDuration, req.SweepInterval)
		if err != nil {
			writeError(w, logger, http.StatusBadRequest, err.Error())
			return
		}

		area, err := deps.Coordinator.Update(r.Context(), id, &control.UpdateAreaInput{
			Name:            req.Name,
			WarmStrategy:    beeline.WarmStrategy(req.WarmStrategy),
			RoutingProvider: req.RoutingProvider,
			DemandIdleTTL:   ttl,
			TargetTTL:       fresh.targetTTL,
			LeaseDuration:   fresh.lease,
			SweepInterval:   fresh.sweep,
			Layers:          toLayers(req.Layers),
		})
		if err != nil {
			writeAreaError(w, logger, err)
			return
		}

		writeJSON(w, logger, http.StatusOK, toAreaResponse(logger, &area, true))
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
		writeJSON(w, logger, http.StatusOK, toAreaResponse(logger, &area, true))
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
		writeJSON(w, logger, http.StatusOK, toAreaResponse(logger, &area, true))
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

		writeJSON(w, logger, http.StatusOK, toAreaResponse(logger, &area, true))
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

// cellStrings renders a cell set as hex strings.
func cellStrings(cells []beeline.H3Cell) []string {
	out := make([]string, 0, len(cells))
	for _, c := range cells {
		out = append(out, c.String())
	}

	return out
}

// parseDuration parses a Go duration string (e.g. "1h", "30m") for the demand-idle
// TTL. An empty string means 0 — decay disabled — so the field is optional. A negative
// duration is rejected here so a bad value surfaces as a 400 rather than deep in the
// control plane.
func parseDuration(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, nil
	}

	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, err
	}
	if d < 0 {
		return 0, errors.New("must not be negative")
	}

	return d, nil
}

// freshnessKnobs holds the three per-area freshness durations parsed from a request.
type freshnessKnobs struct {
	targetTTL time.Duration
	lease     time.Duration
	sweep     time.Duration
}

// parseFreshness parses the optional per-area freshness durations (target TTL, claim
// lease, sweep interval). Each is a Go duration string; an empty value means 0, which the
// control plane reads as "use the house default". A parse error is tagged with the field
// so a 400 names the offending knob. Positivity is enforced downstream by the coordinator.
func parseFreshness(targetTTL, lease, sweep string) (freshnessKnobs, error) {
	tt, err := parseDuration(targetTTL)
	if err != nil {
		return freshnessKnobs{}, errors.New("invalid targetTTL: " + err.Error())
	}
	ls, err := parseDuration(lease)
	if err != nil {
		return freshnessKnobs{}, errors.New("invalid leaseDuration: " + err.Error())
	}
	sw, err := parseDuration(sweep)
	if err != nil {
		return freshnessKnobs{}, errors.New("invalid sweepInterval: " + err.Error())
	}

	return freshnessKnobs{targetTTL: tt, lease: ls, sweep: sw}, nil
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
