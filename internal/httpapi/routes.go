// Package httpapi exposes the read path, the freshness contract, and the area
// control plane over HTTP. The estimate endpoint is the latency-sensitive keyed
// lookup Beeline exists to serve; /_ops_/freshness makes the §3 debt signal a
// first-class, scrapeable number (aggregate, or per-area with ?area=); the
// /_config_/areas endpoints are the CRUD + enable/disable + geometry surface over the
// SQLite-backed area registry; and /_ops_/live + /_ops_/ready are the health probes.
package httpapi

import (
	"context"
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

	"github.com/primandproper/platform-go/v10/healthcheck"
	"github.com/primandproper/platform-go/v10/observability/logging"
	"github.com/primandproper/platform-go/v10/routing"

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
	// RefreshBatch and LeaseDuration are the defaults a /_work_/claim request
	// falls back to when it doesn't name its own batch size or lease.
	RefreshBatch  int
	LeaseDuration time.Duration
}

// Register attaches all routes to the router. Every route is typed — the input
// structs below double as the generated OpenAPI request schemas.
//
// Handlers report failures two ways, and which one is correct depends on whose
// fault the failure is:
//
//   - The service's fault (a store read failed, the engine errored): return an
//     internalError. The router renders it through encodeError, logs it, and
//     attaches it to the request span — all of which a 500 deserves.
//   - The caller's fault (an unparseable coordinate, an unknown area): write it
//     with fail and return a nil error. Returning it would be tidier, but the
//     router acknowledges every returned error at ERROR level, and an
//     unauthenticated caller must not be able to fill the log with 400s.
//
// Both paths produce the same flat {"error": …} body — fail writes it directly,
// encodeError renders it — so the shape does not depend on the choice.
//
// POST routes that answer 200 say so explicitly: the router's POST default is 201.
func Register(router *routing.Router, deps *Deps) {
	logger := logging.EnsureLogger(deps.Logger)
	ok := routing.WithResponseStatus(http.StatusOK)

	routing.Get(router, "/estimate", estimateHandler(deps, logger), routing.WithTags("read"))
	routing.Post(router, "/table", tableHandler(deps, logger), routing.WithTags("read"), ok)

	routing.Get(router, "/_ops_/freshness", freshnessHandler(deps, logger), routing.WithTags("ops"))
	routing.Get(router, "/_ops_/cells", cellsHandler(deps, logger), routing.WithTags("ops"))
	routing.Post(router, "/_ops_/pairs", pairsHandler(deps, logger), routing.WithTags("ops"), ok)
	routing.Post(router, "/_ops_/warm", warmHandler(deps, logger), routing.WithTags("ops"), ok)
	routing.Get(router, "/_ops_/live", liveHandler(), routing.WithTags("ops"))
	routing.Get(router, "/_ops_/ready", readyHandler(deps.Health, logger), routing.WithTags("ops"))

	routing.Post(router, "/_work_/claim", claimHandler(deps, logger), routing.WithTags("work"), ok)
	routing.Post(router, "/_work_/submit", submitHandler(deps, logger), routing.WithTags("work"), ok)
	routing.Get(router, "/_work_/providers", workProvidersHandler(deps), routing.WithTags("work"))

	routing.Get(router, "/_config_/providers", providersListHandler(deps), routing.WithTags("providers"))
	routing.Put(router, "/_config_/providers/{providerName}", providerPutHandler(deps, logger), routing.WithTags("providers"))
	routing.Delete(router, "/_config_/providers/{providerName}", providerDeleteHandler(deps, logger),
		routing.WithTags("providers"), routing.WithResponseStatus(http.StatusNoContent))
	routing.Get(router, "/_config_/areas", areasListHandler(deps, logger), routing.WithTags("areas"))
	routing.Post(router, "/_config_/areas", areaCreateHandler(deps, logger), routing.WithTags("areas"))
	routing.Get(router, "/_config_/areas/{areaID}", areaGetHandler(deps, logger), routing.WithTags("areas"))
	routing.Patch(router, "/_config_/areas/{areaID}", areaUpdateHandler(deps, logger), routing.WithTags("areas"))
	routing.Delete(router, "/_config_/areas/{areaID}", areaDeleteHandler(deps, logger),
		routing.WithTags("areas"), routing.WithResponseStatus(http.StatusNoContent))
	routing.Post(router, "/_config_/areas/{areaID}/enable", areaEnableHandler(deps, logger), routing.WithTags("areas"), ok)
	routing.Post(router, "/_config_/areas/{areaID}/disable", areaDisableHandler(deps, logger), routing.WithTags("areas"), ok)
	routing.Post(router, "/_config_/areas/{areaID}/invalidate", areaInvalidateHandler(deps, logger), routing.WithTags("areas"), ok,
		routing.WithDescription("Re-enqueues the area's cached pairs for refresh, optionally one precision "+
			"layer (?resolution=) and/or profile (?profile=). Cached estimates are not dropped: reads keep "+
			"being answered from them until the refresh pool recomputes them."))
	routing.Put(router, "/_config_/areas/{areaID}/geojson", areaGeoJSONHandler(deps, logger), routing.WithTags("areas"),
		routing.WithDescription("Replaces the area's geometry. The request body is a raw GeoJSON polygon document."))
}

// estimateInput carries /estimate's query params; origin and dest are "lat,lng"
// pairs parsed in-handler so a bad coordinate reports which side is wrong.
type estimateInput struct {
	Origin  string `query:"origin"`
	Dest    string `query:"dest"`
	Profile string `query:"profile"`
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

func estimateHandler(deps *Deps, logger logging.Logger) routing.Handler[estimateInput, estimateResponse] {
	return func(ctx context.Context, in estimateInput) (estimateResponse, error) {
		var zero estimateResponse

		origin, err := parseLatLng(in.Origin)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid origin: "+err.Error())
			return zero, nil
		}

		dest, err := parseLatLng(in.Dest)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid dest: "+err.Error())
			return zero, nil
		}

		profile := deps.DefaultProfile
		if p := strings.TrimSpace(in.Profile); p != "" {
			profile = beeline.Profile(p)
		}

		result, err := deps.Handler.Estimate(ctx, origin, dest, profile)
		if err != nil {
			return zero, internalError("computing estimate", err)
		}

		return estimateResponse{
			DurationSec:    result.Estimate.Duration,
			DistanceMeters: result.Estimate.Distance,
			ComputedAt:     result.ComputedAt.UTC().Format("2006-01-02T15:04:05.000Z"),
			Source:         string(result.Source),
			Profile:        string(profile),
			Stale:          result.Stale,
		}, nil
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

func tableHandler(deps *Deps, logger logging.Logger) routing.Handler[tableRequest, tableResponse] {
	return func(ctx context.Context, req tableRequest) (tableResponse, error) {
		var zero tableResponse

		if len(req.Sources) == 0 || len(req.Destinations) == 0 {
			fail(ctx, logger, http.StatusBadRequest, "sources and destinations must both be non-empty")
			return zero, nil
		}
		if len(req.Sources)*len(req.Destinations) > maxTableCells {
			fail(ctx, logger, http.StatusBadRequest,
				"table too large: sources × destinations exceeds "+strconv.Itoa(maxTableCells))
			return zero, nil
		}

		sources, err := parseLatLngs(req.Sources)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid source: "+err.Error())
			return zero, nil
		}

		destinations, err := parseLatLngs(req.Destinations)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid destination: "+err.Error())
			return zero, nil
		}

		skip, err := parseSkip(req.Skip, len(sources), len(destinations))
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid skip: "+err.Error())
			return zero, nil
		}

		profile := deps.DefaultProfile
		if p := strings.TrimSpace(req.Profile); p != "" {
			profile = beeline.Profile(p)
		}

		fill := true
		if req.Fill != nil {
			fill = *req.Fill
		}

		result, err := deps.Handler.Table(ctx, &query.TableQuery{
			Sources:      sources,
			Destinations: destinations,
			Profile:      profile,
			Skip:         skip,
			Fill:         fill,
		})
		if err != nil {
			return zero, internalError("computing table", err)
		}

		return toTableResponse(result, string(profile)), nil
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

// areaQueryInput is the optional ?area=<id> scope shared by the freshness and
// cells rollups.
type areaQueryInput struct {
	Area string `query:"area"`
}

// freshnessHandler serves the §3 debt contract. Without ?area it reports the aggregate
// across every enabled area (the whole working set); with ?area=<id> it reports that
// one area.
func freshnessHandler(deps *Deps, logger logging.Logger) routing.Handler[areaQueryInput, beeline.DebtStats] {
	return func(ctx context.Context, in areaQueryInput) (beeline.DebtStats, error) {
		var zero beeline.DebtStats

		if raw := strings.TrimSpace(in.Area); raw != "" {
			id, err := parseAreaID(raw)
			if err != nil {
				fail(ctx, logger, http.StatusBadRequest, "invalid area: "+err.Error())
				return zero, nil
			}

			stats, statErr := deps.Coordinator.DebtForArea(ctx, id)
			if statErr != nil {
				return zero, internalError("reading area freshness debt", statErr)
			}

			return stats, nil
		}

		stats, err := deps.Index.Debt(ctx)
		if err != nil {
			return zero, internalError("reading freshness debt", err)
		}

		return stats, nil
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
func cellsHandler(deps *Deps, logger logging.Logger) routing.Handler[areaQueryInput, cellsResponse] {
	return func(ctx context.Context, in areaQueryInput) (cellsResponse, error) {
		var zero cellsResponse

		var ids []beeline.AreaID
		if raw := strings.TrimSpace(in.Area); raw != "" {
			id, err := parseAreaID(raw)
			if err != nil {
				fail(ctx, logger, http.StatusBadRequest, "invalid area: "+err.Error())
				return zero, nil
			}
			ids = []beeline.AreaID{id}
		} else {
			ids = deps.Coordinator.EnabledAreas()
		}

		cells := make([]cellStateResponse, 0)
		for _, id := range ids {
			states, err := deps.Coordinator.CellStatesForArea(ctx, id)
			if err != nil {
				return zero, internalError("reading cell states", err)
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

		return cellsResponse{Cells: cells}, nil
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
func pairsHandler(deps *Deps, logger logging.Logger) routing.Handler[pairsRequest, pairsResponse] {
	return func(ctx context.Context, req pairsRequest) (pairsResponse, error) {
		var zero pairsResponse

		if req.Area <= 0 {
			fail(ctx, logger, http.StatusBadRequest, "area must be a positive id")
			return zero, nil
		}
		if len(req.Dests) == 0 {
			fail(ctx, logger, http.StatusBadRequest, "dests must be non-empty")
			return zero, nil
		}
		if len(req.Dests) > maxPairsDests {
			fail(ctx, logger, http.StatusBadRequest,
				"too many dests: limit "+strconv.Itoa(maxPairsDests))
			return zero, nil
		}

		origin, err := parseCell(req.Origin)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid origin: "+err.Error())
			return zero, nil
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
				fail(ctx, logger, http.StatusBadRequest, "invalid dest: "+destErr.Error())
				return zero, nil
			}
			if dest.Resolution() != origin.Resolution() {
				fail(ctx, logger, http.StatusBadRequest,
					"dest "+raw+" is resolution "+strconv.Itoa(dest.Resolution())+
						", want the origin's "+strconv.Itoa(origin.Resolution()))
				return zero, nil
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

		stored, err := deps.Store.BatchGet(ctx, keys)
		if err != nil {
			return zero, internalError("reading cached pairs", err)
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

		return pairsResponse{Pairs: pairs}, nil
	}
}

// maxWarmPairs bounds one warm request, mirroring maxTableCells. It caps a single
// request, not the cumulative working set: repeated seed-mode calls keep pinning.
const maxWarmPairs = 10_000

// Warm modes: bump raises decayable refresh priority; seed additionally pins the
// pairs against the demand sweep.
const (
	warmModeBump = "bump"
	warmModeSeed = "seed"
)

// warmRequest feeds predicted demand — typically a model trained on this service's
// telemetry export — into an enabled area's refresh queue. Pairs are hex H3 cells
// at the area's finest (read) resolution; Profile defaults to the server's default
// profile; Mode defaults to bump (decayable — recommended), while seed pins the
// pairs until the area is disabled. Origin+radius expansion is deliberately not
// offered: the telemetry is pair-granular, so a model's natural output is pairs.
type warmRequest struct {
	Profile string         `json:"profile"`
	Mode    string         `json:"mode"`
	Pairs   []warmPairBody `json:"pairs"`
	Area    int64          `json:"area"`
}

type warmPairBody struct {
	Origin string `json:"origin"`
	Dest   string `json:"dest"`
}

type warmResponse struct {
	Mode     string `json:"mode"`
	Accepted int    `json:"accepted"`
}

// warmHandler is the model-feed ingestion point: it validates the request shape
// here and delegates area membership to the coordinator, whose ErrInvalidWarm maps
// to 400. Like every endpoint in this prototype it is unauthenticated; a real
// deploy would gate it.
func warmHandler(deps *Deps, logger logging.Logger) routing.Handler[warmRequest, warmResponse] {
	return func(ctx context.Context, req warmRequest) (warmResponse, error) {
		var zero warmResponse

		if req.Area <= 0 {
			fail(ctx, logger, http.StatusBadRequest, "area must be a positive id")
			return zero, nil
		}
		if len(req.Pairs) == 0 {
			fail(ctx, logger, http.StatusBadRequest, "pairs must be non-empty")
			return zero, nil
		}
		if len(req.Pairs) > maxWarmPairs {
			fail(ctx, logger, http.StatusBadRequest,
				"too many pairs: limit "+strconv.Itoa(maxWarmPairs))
			return zero, nil
		}

		mode := warmModeBump
		if m := strings.TrimSpace(req.Mode); m != "" {
			if m != warmModeBump && m != warmModeSeed {
				fail(ctx, logger, http.StatusBadRequest, "unknown mode "+m+" (want bump or seed)")
				return zero, nil
			}
			mode = m
		}

		profile := deps.DefaultProfile
		if p := strings.TrimSpace(req.Profile); p != "" {
			profile = beeline.Profile(p)
		}

		keys := make([]beeline.PairKey, 0, len(req.Pairs))
		for i := range req.Pairs {
			p := &req.Pairs[i]
			origin, err := parseCell(p.Origin)
			if err != nil {
				fail(ctx, logger, http.StatusBadRequest, "invalid origin: "+err.Error())
				return zero, nil
			}
			dest, err := parseCell(p.Dest)
			if err != nil {
				fail(ctx, logger, http.StatusBadRequest, "invalid dest: "+err.Error())
				return zero, nil
			}
			if dest.Resolution() != origin.Resolution() {
				fail(ctx, logger, http.StatusBadRequest,
					"dest "+p.Dest+" is resolution "+strconv.Itoa(dest.Resolution())+
						", want the origin's "+strconv.Itoa(origin.Resolution()))
				return zero, nil
			}
			keys = append(keys, beeline.PairKey{
				Area:    beeline.AreaID(req.Area),
				Origin:  origin,
				Dest:    dest,
				Profile: profile,
				Res:     origin.Resolution(),
			})
		}

		accepted, err := deps.Coordinator.WarmPairs(ctx, beeline.AreaID(req.Area), keys, mode == warmModeSeed)
		if err != nil {
			if errors.Is(err, control.ErrInvalidWarm) {
				fail(ctx, logger, http.StatusBadRequest, err.Error())
				return zero, nil
			}
			return zero, internalError("warming pairs", err)
		}

		return warmResponse{Accepted: accepted, Mode: mode}, nil
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

// providerResponse is one registry entry on the wire: the full spec plus whether it
// is a synthesized built-in (immutable through this API).
type providerResponse struct {
	beeline.ProviderSpec
	Builtin bool `json:"builtin"`
}

// providersListHandler returns every registered routing provider (default first) so
// the operator console can render the registry and populate its picker from live
// state. Entries marked builtin cannot be modified or deleted.
func providersListHandler(deps *Deps) routing.Handler[routing.Empty, []providerResponse] {
	return func(_ context.Context, _ routing.Empty) ([]providerResponse, error) {
		infos := deps.Coordinator.ListProviderInfos()
		out := make([]providerResponse, 0, len(infos))
		for i := range infos {
			out = append(out, providerResponse{ProviderSpec: infos[i].Spec, Builtin: infos[i].Builtin})
		}

		return out, nil
	}
}

// providerPutInput is the PUT body — a full provider spec — plus the path name.
// The path names the provider; a body name is overridden, so the URL is
// authoritative.
type providerPutInput struct {
	ProviderName string `json:"-" path:"providerName"`
	beeline.ProviderSpec
}

// providerPutHandler creates or replaces one operator-defined provider by name.
// This is the single place a cluster's routing config changes: followers pick the
// new registry up on their next claim via the catalog hash.
func providerPutHandler(deps *Deps, logger logging.Logger) routing.Handler[providerPutInput, providerResponse] {
	return func(ctx context.Context, in providerPutInput) (providerResponse, error) {
		var zero providerResponse

		name := strings.TrimSpace(in.ProviderName)
		if name == "" {
			fail(ctx, logger, http.StatusBadRequest, "invalid or missing provider name")
			return zero, nil
		}

		spec := in.ProviderSpec
		spec.Name = name

		info, err := deps.Coordinator.PutProvider(ctx, &spec)
		if err != nil {
			writeProviderError(ctx, logger, err)
			return zero, nil
		}

		logger.Info("routing provider upserted")
		return providerResponse{ProviderSpec: info.Spec, Builtin: info.Builtin}, nil
	}
}

// providerNameInput names a provider through the path alone; it has no body
// fields, so the request body is never read.
type providerNameInput struct {
	ProviderName string `path:"providerName"`
}

// providerDeleteHandler removes one operator-defined provider. Deletion is refused
// while any area still routes through the name (409) and for built-ins (400).
func providerDeleteHandler(deps *Deps, logger logging.Logger) routing.Handler[providerNameInput, routing.Empty] {
	return func(ctx context.Context, in providerNameInput) (routing.Empty, error) {
		name := strings.TrimSpace(in.ProviderName)
		if name == "" {
			fail(ctx, logger, http.StatusBadRequest, "invalid or missing provider name")
			return routing.Empty{}, nil
		}

		if err := deps.Coordinator.DeleteProvider(ctx, name); err != nil {
			writeProviderError(ctx, logger, err)
			return routing.Empty{}, nil
		}

		logger.Info("routing provider deleted")
		return routing.Empty{}, nil
	}
}

// writeProviderError maps a provider mutation error to an HTTP status: unknown name
// → 404, still referenced by an area → 409, everything else (reserved built-in,
// validation) → 400.
func writeProviderError(ctx context.Context, logger logging.Logger, err error) {
	switch {
	case errors.Is(err, control.ErrProviderNotFound):
		fail(ctx, logger, http.StatusNotFound, err.Error())
	case errors.Is(err, control.ErrProviderInUse):
		fail(ctx, logger, http.StatusConflict, err.Error())
	default:
		fail(ctx, logger, http.StatusBadRequest, err.Error())
	}
}

func areasListHandler(deps *Deps, logger logging.Logger) routing.Handler[routing.Empty, []areaResponse] {
	return func(ctx context.Context, _ routing.Empty) ([]areaResponse, error) {
		areas, err := deps.Coordinator.List(ctx)
		if err != nil {
			return nil, internalError("listing areas", err)
		}

		out := make([]areaResponse, 0, len(areas))
		for i := range areas {
			out = append(out, toAreaResponse(logger, &areas[i], false))
		}

		return out, nil
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

func areaCreateHandler(deps *Deps, logger logging.Logger) routing.Handler[createAreaRequest, areaResponse] {
	return func(ctx context.Context, req createAreaRequest) (areaResponse, error) {
		var zero areaResponse

		ttl, err := parseDuration(req.DemandIdleTTL)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid demandIdleTTL: "+err.Error())
			return zero, nil
		}

		fresh, err := parseFreshness(req.TargetTTL, req.LeaseDuration, req.SweepInterval)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, err.Error())
			return zero, nil
		}

		area, err := deps.Coordinator.Create(ctx, &control.CreateAreaInput{
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
			fail(ctx, logger, http.StatusBadRequest, err.Error())
			return zero, nil
		}

		logger.Info("service area created")
		return toAreaResponse(logger, &area, true), nil
	}
}

// areaIDInput names an area through the path alone. The param is a string bound
// as-is and parsed by requireAreaID, so a non-numeric id keeps this API's
// established 400 body instead of a framework binding error.
type areaIDInput struct {
	AreaID string `path:"areaID"`
}

func areaGetHandler(deps *Deps, logger logging.Logger) routing.Handler[areaIDInput, areaResponse] {
	return func(ctx context.Context, in areaIDInput) (areaResponse, error) {
		var zero areaResponse

		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return zero, nil
		}

		area, err := deps.Coordinator.Get(ctx, id)
		if err != nil {
			writeAreaError(ctx, logger, err)
			return zero, nil
		}

		return toAreaResponse(logger, &area, true), nil
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

// areaUpdateInput is the PATCH body plus the path id.
type areaUpdateInput struct {
	AreaID string `json:"-" path:"areaID"`
	updateAreaRequest
}

func areaUpdateHandler(deps *Deps, logger logging.Logger) routing.Handler[areaUpdateInput, areaResponse] {
	return func(ctx context.Context, in areaUpdateInput) (areaResponse, error) {
		var zero areaResponse

		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return zero, nil
		}

		ttl, err := parseDuration(in.DemandIdleTTL)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "invalid demandIdleTTL: "+err.Error())
			return zero, nil
		}

		fresh, err := parseFreshness(in.TargetTTL, in.LeaseDuration, in.SweepInterval)
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, err.Error())
			return zero, nil
		}

		area, err := deps.Coordinator.Update(ctx, id, &control.UpdateAreaInput{
			Name:            in.Name,
			WarmStrategy:    beeline.WarmStrategy(in.WarmStrategy),
			RoutingProvider: in.RoutingProvider,
			DemandIdleTTL:   ttl,
			TargetTTL:       fresh.targetTTL,
			LeaseDuration:   fresh.lease,
			SweepInterval:   fresh.sweep,
			Layers:          toLayers(in.Layers),
		})
		if err != nil {
			writeAreaError(ctx, logger, err)
			return zero, nil
		}

		return toAreaResponse(logger, &area, true), nil
	}
}

func areaDeleteHandler(deps *Deps, logger logging.Logger) routing.Handler[areaIDInput, routing.Empty] {
	return func(ctx context.Context, in areaIDInput) (routing.Empty, error) {
		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return routing.Empty{}, nil
		}

		if err := deps.Coordinator.Delete(ctx, id); err != nil {
			writeAreaError(ctx, logger, err)
			return routing.Empty{}, nil
		}

		return routing.Empty{}, nil
	}
}

func areaEnableHandler(deps *Deps, logger logging.Logger) routing.Handler[areaIDInput, areaResponse] {
	return func(ctx context.Context, in areaIDInput) (areaResponse, error) {
		var zero areaResponse

		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return zero, nil
		}

		area, err := deps.Coordinator.Enable(ctx, id)
		if err != nil {
			writeAreaError(ctx, logger, err)
			return zero, nil
		}

		logger.Info("service area enabled")
		return toAreaResponse(logger, &area, true), nil
	}
}

func areaDisableHandler(deps *Deps, logger logging.Logger) routing.Handler[areaIDInput, areaResponse] {
	return func(ctx context.Context, in areaIDInput) (areaResponse, error) {
		var zero areaResponse

		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return zero, nil
		}

		area, err := deps.Coordinator.Disable(ctx, id)
		if err != nil {
			writeAreaError(ctx, logger, err)
			return zero, nil
		}

		logger.Info("service area disabled")
		return toAreaResponse(logger, &area, true), nil
	}
}

// areaGeoJSONHandler replaces an area's geometry from the uploaded GeoJSON body.
// The body is a raw document, not a struct — areaIDInput has no body fields, so
// the framework leaves it unread and the handler consumes it directly.
func areaGeoJSONHandler(deps *Deps, logger logging.Logger) routing.Handler[areaIDInput, areaResponse] {
	return func(ctx context.Context, in areaIDInput) (areaResponse, error) {
		var zero areaResponse

		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return zero, nil
		}

		r, ok := requestFrom(ctx)
		if !ok {
			return zero, internalMessage("request unavailable")
		}

		raw, err := io.ReadAll(io.LimitReader(r.Body, maxGeoJSONBytes))
		if err != nil {
			fail(ctx, logger, http.StatusBadRequest, "reading geojson body: "+err.Error())
			return zero, nil
		}

		area, err := deps.Coordinator.SetGeoJSON(ctx, id, raw)
		if err != nil {
			writeAreaError(ctx, logger, err)
			return zero, nil
		}

		return toAreaResponse(logger, &area, true), nil
	}
}

// areaInvalidateInput scopes an invalidation through the path and query string
// alone — no body, so `POST …/invalidate` with no payload is the valid
// "every layer, every profile" form. Both filters are strings parsed in-handler so
// an omitted one is distinguishable from resolution 0, a real H3 resolution.
type areaInvalidateInput struct {
	AreaID     string `path:"areaID"`
	Resolution string `query:"resolution"`
	Profile    string `query:"profile"`
}

// areaInvalidateResponse echoes the scope that was applied — so a caller can see
// which layer it actually hit — plus the number of pairs re-enqueued.
type areaInvalidateResponse struct {
	Resolution  *int   `json:"resolution"`
	Profile     string `json:"profile,omitempty"`
	Area        int64  `json:"area"`
	Invalidated int    `json:"invalidated"`
}

// areaInvalidateHandler re-enqueues an enabled area's cached pairs for refresh,
// optionally just one precision layer (?resolution=7) and/or one profile
// (?profile=car). Cached estimates survive and keep answering reads unchanged while
// the pool recomputes them, so this is a throughput cost, not a read outage.
func areaInvalidateHandler(deps *Deps, logger logging.Logger) routing.Handler[areaInvalidateInput, areaInvalidateResponse] {
	return func(ctx context.Context, in areaInvalidateInput) (areaInvalidateResponse, error) {
		var zero areaInvalidateResponse

		id, ok := requireAreaID(ctx, logger, in.AreaID)
		if !ok {
			return zero, nil
		}

		var res *int
		if raw := strings.TrimSpace(in.Resolution); raw != "" {
			parsed, err := strconv.Atoi(raw)
			if err != nil || parsed < 0 || parsed > 15 {
				fail(ctx, logger, http.StatusBadRequest, "invalid resolution "+raw+" (want an H3 resolution 0-15)")
				return zero, nil
			}
			res = &parsed
		}

		n, err := deps.Coordinator.Invalidate(ctx, id, &control.InvalidateInput{
			Resolution: res,
			Profile:    beeline.Profile(strings.TrimSpace(in.Profile)),
		})
		if err != nil {
			if errors.Is(err, control.ErrInvalidInvalidation) {
				fail(ctx, logger, http.StatusBadRequest, err.Error())
				return zero, nil
			}
			return zero, internalError("invalidating area pairs", err)
		}

		logger.WithValues(map[string]any{"invalidated": n}).Info("service area pairs invalidated")

		return areaInvalidateResponse{
			Area:        int64(id),
			Resolution:  res,
			Profile:     strings.TrimSpace(in.Profile),
			Invalidated: n,
		}, nil
	}
}

// statusResponse is the health probes' body: {"status":"up"}.
type statusResponse struct {
	Status string `json:"status"`
}

func liveHandler() routing.Handler[routing.Empty, statusResponse] {
	return func(_ context.Context, _ routing.Empty) (statusResponse, error) {
		return statusResponse{Status: string(healthcheck.StatusUp)}, nil
	}
}

func readyHandler(registry healthcheck.Registry, logger logging.Logger) routing.Handler[routing.Empty, *healthcheck.Result] {
	return func(ctx context.Context, _ routing.Empty) (*healthcheck.Result, error) {
		if registry == nil {
			commitJSON(ctx, logger, http.StatusOK, statusResponse{Status: string(healthcheck.StatusUp)})
			return nil, nil
		}

		result := registry.CheckAll(ctx)
		if result.Status == healthcheck.StatusDown {
			commitJSON(ctx, logger, http.StatusServiceUnavailable, result)
			return nil, nil
		}

		return result, nil
	}
}

// requireAreaID parses the {areaID} path param, failing the request with a 400 and
// returning ok=false when it is not a positive integer.
func requireAreaID(ctx context.Context, logger logging.Logger, raw string) (beeline.AreaID, bool) {
	id, err := parseAreaID(raw)
	if err != nil {
		fail(ctx, logger, http.StatusBadRequest, "invalid or missing area id")
		return 0, false
	}

	return id, true
}

// writeAreaError maps a coordinator error to an HTTP status: not-found → 404,
// everything else (validation, geometry, store) → 400.
func writeAreaError(ctx context.Context, logger logging.Logger, err error) {
	if isNotFound(err) {
		fail(ctx, logger, http.StatusNotFound, err.Error())
		return
	}

	fail(ctx, logger, http.StatusBadRequest, err.Error())
}

// isNotFound reports whether err signals a missing area. Every store's
// ErrNotFound wraps beeline.ErrNotFound, so this needs no dependency on a
// concrete store package — and unlike the substring match it replaces, the
// status code no longer depends on two independent stores wording their errors
// the same way.
func isNotFound(err error) bool {
	return errors.Is(err, beeline.ErrNotFound)
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

// parseAreaID parses a positive area id from a path or query value.
func parseAreaID(raw string) (beeline.AreaID, error) {
	id, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || id <= 0 {
		return 0, errors.New("area id must be a positive integer")
	}

	return beeline.AreaID(id), nil
}

// decodeJSON decodes a JSON request body into v: lenient (unknown fields pass)
// and bounded by maxGeoJSONBytes, the API's established decode behavior.
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
