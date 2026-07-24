// Package follower is the client side of the leader/follower split (design §8):
// the same binary run as `beeline work`, pointed at a leader instance. It
// implements the refresh pool's WorkSource over the leader's /_work_/ endpoints —
// claim pending pairs, compute them with the local routing engines, submit the
// scalars back — and the EngineResolver seam by matching each claimed area's
// routing-provider name (shipped in the claim response) against the follower's own
// provider registry. All coordination is the leader's leased queue: a follower
// that dies or loses connectivity lets its leases expire and the pairs are simply
// reclaimed, so followers hold no durable state at all.
package follower

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v4/observability/logging"

	"github.com/uber/h3-go/v4"
)

// defaultTimeout bounds one claim/submit call when the caller supplies no client,
// mirroring the osrm engine's default.
const defaultTimeout = 10 * time.Second

// Config wires a Follower to its leader.
type Config struct {
	Client    *http.Client
	LeaderURL string
}

// Follower claims work from and submits results to one leader. It satisfies
// refresh.WorkSource (Claim/Submit) and beeline.EngineResolver (EngineFor), so
// the unchanged refresh pool drives it exactly as a leader drives its local
// index/store.
type Follower struct {
	client          *http.Client
	providers       map[string]beeline.RoutingEngine
	areaProviders   map[beeline.AreaID]string
	warnedProviders map[string]struct{}
	logger          logging.Logger
	baseURL         string
	defaultProvider string
	mu              sync.RWMutex
}

// New builds a Follower over the given provider registry, which must contain
// defaultProvider — the engine used for any area whose provider name the follower
// doesn't recognize.
func New(cfg Config, providers map[string]beeline.RoutingEngine, defaultProvider string, logger logging.Logger) (*Follower, error) {
	if cfg.LeaderURL == "" {
		return nil, errors.New("follower: leader URL is required")
	}
	if _, ok := providers[defaultProvider]; !ok {
		return nil, fmt.Errorf("follower: provider registry is missing the default provider %q", defaultProvider)
	}

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	return &Follower{
		baseURL:         strings.TrimRight(cfg.LeaderURL, "/"),
		client:          client,
		providers:       providers,
		defaultProvider: defaultProvider,
		areaProviders:   make(map[beeline.AreaID]string),
		warnedProviders: make(map[string]struct{}),
		logger:          logging.EnsureLogger(logger),
	}, nil
}

// The wire shapes mirror internal/httpapi/work.go's claim/submit contract; the
// in-process end-to-end test drives both through a real router to keep them from
// drifting.
type workPair struct {
	Profile string `json:"profile"`
	Origin  string `json:"origin"`
	Dest    string `json:"dest"`
	Area    int64  `json:"area"`
	Res     int    `json:"res"`
}

type claimRequest struct {
	BatchSize    int `json:"batchSize"`
	LeaseSeconds int `json:"leaseSeconds"`
}

type claimAreaMeta struct {
	RoutingProvider string `json:"routingProvider"`
}

type claimResponse struct {
	Areas        map[string]claimAreaMeta `json:"areas"`
	Pairs        []workPair               `json:"pairs"`
	LeaseSeconds int                      `json:"leaseSeconds"`
}

type submitResult struct {
	Profile        string  `json:"profile"`
	Origin         string  `json:"origin"`
	Dest           string  `json:"dest"`
	Area           int64   `json:"area"`
	Res            int     `json:"res"`
	DurationSec    float64 `json:"durationSec"`
	DistanceMeters float64 `json:"distanceMeters"`
}

type submitRequest struct {
	Results []submitResult `json:"results"`
}

// Claim leases up to limit pairs from the leader and records each claimed area's
// routing-provider name for EngineFor. A cell the leader hands out is parsed
// strictly — a bad one fails the whole claim, since it can only mean protocol
// drift, not recoverable input.
func (f *Follower) Claim(ctx context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	var resp claimResponse
	err := f.post(ctx, "/_work_/claim", claimRequest{
		BatchSize:    limit,
		LeaseSeconds: int(lease / time.Second),
	}, &resp)
	if err != nil {
		return nil, err
	}

	f.recordAreaProviders(resp.Areas)

	keys := make([]beeline.PairKey, 0, len(resp.Pairs))
	for i := range resp.Pairs {
		p := &resp.Pairs[i]
		origin := h3.CellFromString(p.Origin)
		dest := h3.CellFromString(p.Dest)
		if !origin.IsValid() || !dest.IsValid() {
			return nil, fmt.Errorf("follower: claim returned invalid cell pair %q→%q", p.Origin, p.Dest)
		}
		keys = append(keys, beeline.PairKey{
			Area:    beeline.AreaID(p.Area),
			Origin:  origin,
			Dest:    dest,
			Profile: beeline.Profile(p.Profile),
			Res:     p.Res,
		})
	}

	return keys, nil
}

// Submit posts a computed batch back to the leader, which stamps ComputedAt with
// its own clock (entries' local timestamps deliberately don't travel).
func (f *Follower) Submit(ctx context.Context, entries []beeline.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	req := submitRequest{Results: make([]submitResult, 0, len(entries))}
	for i := range entries {
		e := &entries[i]
		req.Results = append(req.Results, submitResult{
			Profile:        string(e.Key.Profile),
			Origin:         e.Key.Origin.String(),
			Dest:           e.Key.Dest.String(),
			Area:           int64(e.Key.Area),
			Res:            e.Key.Res,
			DurationSec:    e.Duration,
			DistanceMeters: e.Distance,
		})
	}

	return f.post(ctx, "/_work_/submit", req, nil)
}

// EngineFor resolves the routing engine for an area from the provider name the
// leader shipped with the claim. An area the follower hasn't seen a claim for, or
// a provider name absent from the local registry, falls back to the default engine
// — the latter with a once-per-name warning, since it means this follower computes
// that area with different routing than the leader intended.
func (f *Follower) EngineFor(area beeline.AreaID) beeline.RoutingEngine {
	f.mu.RLock()
	name, known := f.areaProviders[area]
	f.mu.RUnlock()
	if !known {
		return f.providers[f.defaultProvider]
	}

	engine, ok := f.providers[name]
	if !ok {
		f.warnUnknownProvider(name, area)
		return f.providers[f.defaultProvider]
	}

	return engine
}

// Ping probes the leader's liveness endpoint; nil means reachable and serving.
func (f *Follower) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+"/_ops_/live", http.NoBody)
	if err != nil {
		return fmt.Errorf("follower: building ping request: %w", err)
	}

	httpResp, err := f.client.Do(httpReq) //nolint:gosec // G704: leader URL is trusted operator configuration.
	if err != nil {
		return fmt.Errorf("follower: pinging leader: %w", err)
	}
	_, err = io.Copy(io.Discard, httpResp.Body)
	closeErr := httpResp.Body.Close()
	if err != nil {
		return fmt.Errorf("follower: reading ping response: %w", err)
	}
	if closeErr != nil {
		return fmt.Errorf("follower: closing ping response: %w", closeErr)
	}
	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("follower: leader liveness returned status %d", httpResp.StatusCode)
	}

	return nil
}

// post issues one JSON round-trip to the leader. A non-200 status is an error
// carrying the (truncated) body, and out is left untouched when nil.
func (f *Follower) post(ctx context.Context, path string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("follower: encoding %s request: %w", path, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, f.baseURL+path, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("follower: building %s request: %w", path, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	// The URL is the operator-configured leader base plus a fixed path, never
	// end-user input, so it is not an SSRF vector.
	httpResp, err := f.client.Do(httpReq) //nolint:gosec // G704: leader URL is trusted operator configuration.
	if err != nil {
		return fmt.Errorf("follower: %s request: %w", path, err)
	}

	raw, err := io.ReadAll(httpResp.Body)
	closeErr := httpResp.Body.Close()
	if err != nil {
		return fmt.Errorf("follower: reading %s response: %w", path, err)
	}
	if closeErr != nil {
		return fmt.Errorf("follower: closing %s response: %w", path, closeErr)
	}
	if httpResp.StatusCode != http.StatusOK {
		return fmt.Errorf("follower: %s returned status %d: %s", path, httpResp.StatusCode, truncate(raw))
	}

	if out == nil {
		return nil
	}
	if err = json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("follower: decoding %s response: %w", path, err)
	}

	return nil
}

// recordAreaProviders merges a claim response's area metadata into the shared
// area→provider map all workers consult through EngineFor.
func (f *Follower) recordAreaProviders(areas map[string]claimAreaMeta) {
	if len(areas) == 0 {
		return
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for raw, meta := range areas {
		var id int64
		if _, err := fmt.Sscan(raw, &id); err != nil || id <= 0 {
			continue
		}
		f.areaProviders[beeline.AreaID(id)] = meta.RoutingProvider
	}
}

// warnUnknownProvider logs once per unrecognized provider name; every claim after
// that falls back silently.
func (f *Follower) warnUnknownProvider(name string, area beeline.AreaID) {
	f.mu.Lock()
	_, seen := f.warnedProviders[name]
	if !seen {
		f.warnedProviders[name] = struct{}{}
	}
	f.mu.Unlock()

	if !seen {
		f.logger.WithValues(map[string]any{
			"provider": name,
			"area":     int64(area),
			"fallback": f.defaultProvider,
		}).Info("leader area uses a routing provider this follower does not have; falling back to the default engine")
	}
}

// truncate bounds an error body so a huge response can't flood the logs.
func truncate(raw []byte) string {
	const limit = 512
	s := strings.TrimSpace(string(raw))
	if len(s) > limit {
		return s[:limit] + "…"
	}

	return s
}
