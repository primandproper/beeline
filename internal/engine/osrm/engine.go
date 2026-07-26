// Package osrm implements beeline.RoutingEngine against a live OSRM server's HTTP
// table service. It is the first real (network-bound) engine: where the Haversine
// stand-in returns great-circle math in-process, this issues one GET /table request
// per Table call and parses the dense duration/distance matrices OSRM returns.
//
// OSRM's /table is natively matrix-shaped (§6) — it returns the full cartesian
// product of the selected sources × destinations — which is exactly the shape the
// refresh pool batches around, so a 1×K origin-centric request maps to a single call.
package osrm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v7/circuitbreaking"
	circuitbreakingcfg "github.com/primandproper/platform-go/v7/circuitbreaking/config"
)

// defaultTimeout bounds a single /table request so a hung OSRM server cannot stall a
// refresh worker or a read indefinitely. Overridable with WithHTTPClient.
const defaultTimeout = 10 * time.Second

// Engine talks to one OSRM server. It carries no routing math of its own: OSRM owns
// the graph, so this is purely a request builder and response parser behind the
// RoutingEngine interface.
type Engine struct {
	client  *http.Client
	breaker circuitbreaking.CircuitBreaker
	// profiles maps a beeline profile to the OSRM profile path segment (the mode the
	// server was built with, e.g. "car"→"driving"). A profile absent from the map is
	// passed through unchanged, so an identity mapping needs no configuration.
	profiles map[beeline.Profile]string
	baseURL  string
	// supported is the profile list Capabilities reports; empty means "unconstrained"
	// (the caller may request any profile and let OSRM accept or reject it).
	supported    []beeline.Profile
	maxTableSize int
}

// Option customizes an Engine at construction, mirroring the latency package's style
// so tests can inject an httptest client and callers can declare capabilities.
type Option func(*Engine)

// WithHTTPClient overrides the HTTP client (its Timeout, transport, or an httptest
// client in tests). The default client has a defaultTimeout.
func WithHTTPClient(c *http.Client) Option {
	return func(e *Engine) { e.client = c }
}

// WithProfiles sets the beeline→OSRM profile-path mapping and, as a side effect, the
// SupportedProfiles that Capabilities reports (the map's keys). Profiles not in the
// map still pass through at request time; this only bounds what Capabilities advertises.
func WithProfiles(m map[beeline.Profile]string) Option {
	return func(e *Engine) {
		e.profiles = m
		e.supported = e.supported[:0]
		for p := range m {
			e.supported = append(e.supported, p)
		}
	}
}

// WithCircuitBreaker sheds load when the OSRM server stops answering: once tripped,
// Table fails immediately instead of paying the client timeout per call. A refresh
// worker's lease then expires and the pairs return to the queue; a read-path miss
// surfaces as the usual engine error. The default is a no-op breaker.
func WithCircuitBreaker(cb circuitbreaking.CircuitBreaker) Option {
	return func(e *Engine) { e.breaker = cb }
}

// WithMaxTableSize sets the matrix-size bound Capabilities reports and Table enforces,
// matching the server's own max-table-size limit (0 = unbounded, the default).
func WithMaxTableSize(n int) Option {
	return func(e *Engine) { e.maxTableSize = n }
}

// New builds an Engine for the OSRM server at baseURL (e.g. "http://localhost:5000").
// A trailing slash is trimmed so URL assembly stays predictable.
func New(baseURL string, opts ...Option) *Engine {
	e := &Engine{
		baseURL: strings.TrimRight(baseURL, "/"),
		client:  &http.Client{Timeout: defaultTimeout},
	}
	for _, opt := range opts {
		opt(e)
	}
	e.breaker = circuitbreakingcfg.EnsureCircuitBreaker(e.breaker)

	return e
}

// tableResponse is the subset of OSRM's /table JSON we consume. durations/distances
// are pointer matrices so an unreachable cell (JSON null) is distinguishable from a
// real zero during decode.
type tableResponse struct {
	Code      string       `json:"code"`
	Message   string       `json:"message"`
	Durations [][]*float64 `json:"durations"`
	Distances [][]*float64 `json:"distances"`
}

// Table issues one GET /table request for the dense Sources × Destinations matrix and
// returns the requested scalars. Sources and Destinations are packed into a single
// coordinate list (sources first), and the sources=/destinations= index subsets select
// the rectangle, so OSRM computes only the pairs we asked for (§6).
func (e *Engine) Table(ctx context.Context, req beeline.TableRequest) (beeline.TableResponse, error) {
	if len(req.Sources) == 0 || len(req.Destinations) == 0 {
		return beeline.TableResponse{}, nil
	}
	if n := len(req.Sources) * len(req.Destinations); e.maxTableSize > 0 && n > e.maxTableSize {
		return beeline.TableResponse{}, fmt.Errorf("osrm: table size %d exceeds max %d", n, e.maxTableSize)
	}

	if e.breaker.CannotProceed() {
		return beeline.TableResponse{}, fmt.Errorf("osrm: table: %w", circuitbreaking.ErrCircuitBroken)
	}

	wantDist := req.Want.Has(beeline.AnnotateDistance)
	wantDur := req.Want == 0 || req.Want.Has(beeline.AnnotateDuration)

	reqURL := e.tableURL(req, wantDur, wantDist)

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, http.NoBody)
	if err != nil {
		return beeline.TableResponse{}, fmt.Errorf("osrm: building request: %w", err)
	}

	// The request URL is assembled from the operator-configured baseURL plus fixed
	// path/query structure, not from end-user input, so it is not an SSRF vector.
	httpResp, err := e.client.Do(httpReq) //nolint:gosec // G704: baseURL is trusted operator configuration.
	if err != nil {
		e.breaker.Failed()

		return beeline.TableResponse{}, fmt.Errorf("osrm: table request: %w", err)
	}

	body, err := io.ReadAll(httpResp.Body)
	closeErr := httpResp.Body.Close()
	if err != nil {
		e.breaker.Failed()

		return beeline.TableResponse{}, fmt.Errorf("osrm: reading table response: %w", err)
	}
	if closeErr != nil {
		e.breaker.Failed()

		return beeline.TableResponse{}, fmt.Errorf("osrm: closing table response: %w", closeErr)
	}
	if httpResp.StatusCode >= http.StatusInternalServerError {
		e.breaker.Failed()

		return beeline.TableResponse{}, fmt.Errorf("osrm: table returned status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(body)))
	}

	// The server answered. A 4xx or a non-Ok code is this request's problem, not
	// the server's health, so those count as successes for the breaker.
	e.breaker.Succeeded()

	if httpResp.StatusCode != http.StatusOK {
		return beeline.TableResponse{}, fmt.Errorf("osrm: table returned status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(body)))
	}

	var decoded tableResponse
	if err = json.Unmarshal(body, &decoded); err != nil {
		return beeline.TableResponse{}, fmt.Errorf("osrm: decoding table response: %w", err)
	}
	if decoded.Code != "Ok" {
		return beeline.TableResponse{}, fmt.Errorf("osrm: table code %q: %s", decoded.Code, decoded.Message)
	}

	resp := beeline.TableResponse{}
	if wantDur {
		resp.Duration = denseMatrix(decoded.Durations, len(req.Sources), len(req.Destinations))
	}
	if wantDist {
		resp.Distance = denseMatrix(decoded.Distances, len(req.Sources), len(req.Destinations))
	}

	return resp, nil
}

// Capabilities reports the configured profiles, that OSRM returns distance, and the
// matrix-size bound. An empty SupportedProfiles means the mapping was left as identity
// and the engine does not constrain the profile.
func (e *Engine) Capabilities() beeline.Capabilities {
	return beeline.Capabilities{
		SupportedProfiles: e.supported,
		MaxTableSize:      e.maxTableSize,
		SupportsDistance:  true,
	}
}

// tableURL assembles the /table request URL: the profile path segment, the packed
// coordinate list, and the sources/destinations index subsets plus annotations.
func (e *Engine) tableURL(req beeline.TableRequest, wantDur, wantDist bool) string {
	coords := make([]string, 0, len(req.Sources)+len(req.Destinations))
	for i := range req.Sources {
		coords = append(coords, formatCoord(req.Sources[i]))
	}
	for i := range req.Destinations {
		coords = append(coords, formatCoord(req.Destinations[i]))
	}

	q := url.Values{}
	q.Set("sources", indexRange(0, len(req.Sources)))
	q.Set("destinations", indexRange(len(req.Sources), len(req.Destinations)))
	q.Set("annotations", annotationsParam(wantDur, wantDist))

	return fmt.Sprintf("%s/table/v1/%s/%s?%s", e.baseURL, e.osrmProfile(req.Profile), strings.Join(coords, ";"), q.Encode())
}

// osrmProfile resolves a beeline profile to the OSRM profile path segment, defaulting
// to the profile name itself when no mapping is configured.
func (e *Engine) osrmProfile(p beeline.Profile) string {
	if mapped, ok := e.profiles[p]; ok {
		return mapped
	}

	return string(p)
}

// denseMatrix converts OSRM's pointer matrix into the plain matrix beeline stores,
// mapping an unreachable cell (null) to 0. A 0 here is indistinguishable from a
// genuinely zero-length trip, which is acceptable because an unroutable pair within a
// service area is rare and a real deployment would more likely drop such a pair than
// serve it; storing 0 keeps the value JSON-encodable on the read path (unlike +Inf).
// It also tolerates a short/ragged matrix by treating missing entries as 0.
func denseMatrix(src [][]*float64, rows, cols int) [][]float64 {
	out := make([][]float64, rows)
	for i := range rows {
		out[i] = make([]float64, cols)
		if i >= len(src) {
			continue
		}
		for j := 0; j < cols && j < len(src[i]); j++ {
			if v := src[i][j]; v != nil {
				out[i][j] = *v
			}
		}
	}

	return out
}

// formatCoord renders a coordinate as OSRM's "lng,lat" with fixed precision (~0.1m).
func formatCoord(c beeline.LatLng) string {
	return strconv.FormatFloat(c.Lng, 'f', 6, 64) + "," + strconv.FormatFloat(c.Lat, 'f', 6, 64)
}

// indexRange renders count consecutive indices starting at start as OSRM's
// semicolon-separated sources=/destinations= subset.
func indexRange(start, count int) string {
	idx := make([]string, count)
	for i := range count {
		idx[i] = strconv.Itoa(start + i)
	}

	return strings.Join(idx, ";")
}

// annotationsParam builds OSRM's annotations= value from the requested scalars. At
// least duration is always requested, matching the interface's zero-Want default.
func annotationsParam(wantDur, wantDist bool) string {
	switch {
	case wantDur && wantDist:
		return "duration,distance"
	case wantDist:
		return "distance"
	default:
		return "duration"
	}
}
