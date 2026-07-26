// Package follower is the client side of the leader/follower split (design §8):
// the same binary run as `beeline work`, pointed at a leader instance. It
// implements the refresh pool's WorkSource over the leader's /_work_/ endpoints —
// claim pending pairs, compute them with the local routing engines, submit the
// scalars back — and the EngineResolver seam by matching each claimed area's
// routing-provider name (shipped in the claim response) against a provider
// registry synced from the leader itself: every claim response carries the
// leader's provider-catalog hash, and a follower holding a different hash fetches
// /_work_/providers and rebuilds its engines before computing. Provider
// configuration therefore lives only on the leader — a follower needs nothing but
// a leader URL. All coordination is the leader's leased queue: a follower that
// dies or loses connectivity lets its leases expire and the pairs are simply
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

	"github.com/primandproper/platform-go/v7/circuitbreaking"
	circuitbreakingcfg "github.com/primandproper/platform-go/v7/circuitbreaking/config"
	"github.com/primandproper/platform-go/v7/observability/logging"
	"github.com/primandproper/platform-go/v7/retry"

	"github.com/uber/h3-go/v4"
)

// defaultTimeout bounds one claim/submit call when the caller supplies no client,
// mirroring the osrm engine's default.
const defaultTimeout = 10 * time.Second

// EnginesBuilder constructs the name→engine map from a synced provider catalog.
// The CLI wires it to the engine registry; the seam keeps this package free of
// concrete engine dependencies and lets tests inject observable engines.
type EnginesBuilder func(specs []beeline.ProviderSpec, speeds map[beeline.Profile]float64) (map[string]beeline.RoutingEngine, error)

// Config wires a Follower to its leader.
type Config struct {
	Fallback     beeline.RoutingEngine
	Client       *http.Client
	BuildEngines EnginesBuilder
	// Retry governs how a failed claim/submit round trip is re-attempted. Nil
	// means a single attempt — the behavior before retries existed, and what
	// tests asserting one call per operation want.
	Retry retry.Policy
	// Breaker sheds load when the leader stops answering: once tripped, claims
	// fail immediately and the pool idles on its backoff instead of hammering a
	// sick leader with the full retry budget per worker. Nil means no breaker.
	Breaker   circuitbreaking.CircuitBreaker
	LeaderURL string
}

// Follower claims work from and submits results to one leader. It satisfies
// refresh.WorkSource (Claim/Submit) and beeline.EngineResolver (EngineFor), so
// the unchanged refresh pool drives it exactly as a leader drives its local
// index/store.
type Follower struct {
	client          *http.Client
	retry           retry.Policy
	breaker         circuitbreaking.CircuitBreaker
	buildEngines    EnginesBuilder
	fallback        beeline.RoutingEngine
	providers       map[string]beeline.RoutingEngine
	areaProviders   map[beeline.AreaID]string
	warnedProviders map[string]struct{}
	logger          logging.Logger
	baseURL         string
	providersHash   string
	mu              sync.RWMutex
}

// New builds a Follower. Its provider registry starts as just the fallback engine
// under the default name and is replaced wholesale by the first catalog sync — a
// follower is configured by its leader, not by local provider config.
func New(cfg *Config, logger logging.Logger) (*Follower, error) {
	if cfg.LeaderURL == "" {
		return nil, errors.New("follower: leader URL is required")
	}
	if cfg.Fallback == nil {
		return nil, errors.New("follower: a fallback engine is required")
	}
	if cfg.BuildEngines == nil {
		return nil, errors.New("follower: an engines builder is required")
	}

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}

	policy := cfg.Retry
	if policy == nil {
		policy = retry.NewExponentialBackoffPolicy(retry.Config{MaxAttempts: 1})
	}

	return &Follower{
		baseURL:         strings.TrimRight(cfg.LeaderURL, "/"),
		client:          client,
		retry:           policy,
		breaker:         circuitbreakingcfg.EnsureCircuitBreaker(cfg.Breaker),
		buildEngines:    cfg.BuildEngines,
		fallback:        cfg.Fallback,
		providers:       map[string]beeline.RoutingEngine{beeline.DefaultProviderName: cfg.Fallback},
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
	Areas         map[string]claimAreaMeta `json:"areas"`
	ProvidersHash string                   `json:"providersHash"`
	Pairs         []workPair               `json:"pairs"`
	LeaseSeconds  int                      `json:"leaseSeconds"`
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
// routing-provider name for EngineFor. When the response carries a provider-catalog
// hash the follower doesn't hold, the catalog is synced before the claim returns —
// and a failed sync fails the whole claim rather than compute pairs with engines
// the leader no longer intends (the leased pairs simply expire back into the
// queue). A cell the leader hands out is parsed strictly — a bad one fails the
// whole claim, since it can only mean protocol drift, not recoverable input.
//
// The whole claim, catalog sync included, shares one retry budget. Retrying is
// safe even when the leader actually granted the lost attempt: those pairs stay
// leased until they expire back into the queue.
func (f *Follower) Claim(ctx context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	var keys []beeline.PairKey
	err := f.retry.Execute(ctx, func(ctx context.Context) error {
		claimed, err := f.claimOnce(ctx, limit, lease)
		if err != nil {
			return classify(err)
		}
		keys = claimed

		return nil
	})
	if err != nil {
		return nil, err
	}

	return keys, nil
}

// claimOnce is one claim attempt.
func (f *Follower) claimOnce(ctx context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	var resp claimResponse
	err := f.post(ctx, "/_work_/claim", claimRequest{
		BatchSize:    limit,
		LeaseSeconds: int(lease / time.Second),
	}, &resp)
	if err != nil {
		return nil, err
	}

	if resp.ProvidersHash != "" && resp.ProvidersHash != f.currentHash() {
		if err = f.syncProviders(ctx); err != nil {
			return nil, fmt.Errorf("follower: syncing provider catalog: %w", err)
		}
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

	// Duplicate submits are idempotent by construction (Put + MarkComputed), so a
	// retry after an ambiguous failure is waste at worst, never corruption.
	return f.retry.Execute(ctx, func(ctx context.Context) error {
		return classify(f.post(ctx, "/_work_/submit", req, nil))
	})
}

// EngineFor resolves the routing engine for an area from the provider name the
// leader shipped with the claim, against the registry synced from the leader's
// catalog. An area the follower hasn't seen a claim for, or a provider name absent
// from the registry, falls back to the default engine — the latter with a
// once-per-name warning, since it means this follower computes that area with
// different routing than the leader intended (it should not occur against a
// catalog-serving leader, whose claims sync the registry first).
func (f *Follower) EngineFor(area beeline.AreaID) beeline.RoutingEngine {
	f.mu.RLock()
	name, known := f.areaProviders[area]
	engine, ok := f.providers[name]
	fallback := f.providers[beeline.DefaultProviderName]
	f.mu.RUnlock()

	if !known {
		return fallback
	}
	if !ok {
		f.warnUnknownProvider(name, area)
		return fallback
	}

	return engine
}

// currentHash returns the catalog hash of the registry currently in use ("" before
// the first sync).
func (f *Follower) currentHash() string {
	f.mu.RLock()
	defer f.mu.RUnlock()

	return f.providersHash
}

// syncProviders fetches the leader's provider catalog and swaps in a freshly built
// engine registry. The stored hash is the fetched catalog's own (not the claim's
// that triggered the sync), so a catalog that changes between claim and fetch is
// simply newer — the next claim's hash will match. The default name always
// resolves: if a (malformed) catalog omits it, the local fallback engine fills in.
func (f *Follower) syncProviders(ctx context.Context) error {
	var catalog beeline.ProviderCatalog
	if err := f.get(ctx, "/_work_/providers", &catalog); err != nil {
		return err
	}

	speeds := make(map[beeline.Profile]float64, len(catalog.Speeds))
	for name, speed := range catalog.Speeds {
		speeds[beeline.Profile(name)] = speed
	}

	engines, err := f.buildEngines(catalog.Providers, speeds)
	if err != nil {
		return err
	}
	if _, ok := engines[beeline.DefaultProviderName]; !ok {
		engines[beeline.DefaultProviderName] = f.fallback
	}

	f.mu.Lock()
	f.providers = engines
	f.providersHash = catalog.Hash
	// A fresh registry deserves fresh warnings: a name that was missing may exist
	// now, and vice versa.
	f.warnedProviders = make(map[string]struct{})
	f.mu.Unlock()

	f.logger.WithValues(map[string]any{
		"hash":      catalog.Hash,
		"providers": len(engines),
	}).Info("synced provider catalog from leader")

	return nil
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

// post issues one JSON POST round-trip to the leader. A non-200 status is an error
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

	return f.roundTrip(httpReq, path, out)
}

// get issues one JSON GET round-trip to the leader, with post's status/body rules.
func (f *Follower) get(ctx context.Context, path string, out any) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, f.baseURL+path, http.NoBody)
	if err != nil {
		return fmt.Errorf("follower: building %s request: %w", path, err)
	}

	return f.roundTrip(httpReq, path, out)
}

// roundTrip executes a prepared request and decodes the JSON response into out
// (left untouched when nil). A non-200 status is an error carrying the (truncated)
// body.
func (f *Follower) roundTrip(httpReq *http.Request, path string, out any) error {
	// A tripped breaker is terminal for this attempt: spending the retry budget
	// sleeping between calls we already know will be rejected only delays the
	// worker's return to its idle backoff.
	if f.breaker.CannotProceed() {
		return retry.Unretryable(fmt.Errorf("follower: %s: %w", path, circuitbreaking.ErrCircuitBroken))
	}

	// The URL is the operator-configured leader base plus a fixed path, never
	// end-user input, so it is not an SSRF vector.
	httpResp, err := f.client.Do(httpReq) //nolint:gosec // G704: leader URL is trusted operator configuration.
	if err != nil {
		f.breaker.Failed()

		return fmt.Errorf("follower: %s request: %w", path, err)
	}

	raw, err := io.ReadAll(httpResp.Body)
	closeErr := httpResp.Body.Close()
	if err != nil {
		f.breaker.Failed()

		return fmt.Errorf("follower: reading %s response: %w", path, err)
	}
	if closeErr != nil {
		f.breaker.Failed()

		return fmt.Errorf("follower: closing %s response: %w", path, closeErr)
	}
	if httpResp.StatusCode >= http.StatusInternalServerError {
		f.breaker.Failed()

		return &statusError{code: httpResp.StatusCode, path: path, body: truncate(raw)}
	}

	// Anything the leader actually answered — 2xx and 4xx alike — proves it is
	// healthy. A rejected request is this follower's problem, not the leader's.
	f.breaker.Succeeded()

	if httpResp.StatusCode != http.StatusOK {
		return &statusError{code: httpResp.StatusCode, path: path, body: truncate(raw)}
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
			"fallback": beeline.DefaultProviderName,
		}).Info("leader area uses a routing provider this follower does not have; falling back to the default engine")
	}
}

// statusError is a non-200 response from the leader. It renders exactly as the
// flat error this package has always produced, but carries the status code so
// retries can tell a leader that is struggling (5xx, worth another attempt)
// from one that has rejected the request outright (4xx, terminal).
type statusError struct {
	path string
	body string
	code int
}

func (e *statusError) Error() string {
	return fmt.Sprintf("follower: %s returned status %d: %s", e.path, e.code, e.body)
}

// terminal reports whether retrying could only repeat the same rejection.
func (e *statusError) terminal() bool {
	return e.code >= http.StatusBadRequest && e.code < http.StatusInternalServerError
}

// classify marks an error unretryable when another attempt cannot change the
// outcome. Everything else — 5xx, transport failures, malformed responses — is
// worth retrying: the leader may be restarting or briefly unreachable.
func classify(err error) error {
	var status *statusError
	if errors.As(err, &status) && status.terminal() {
		return retry.Unretryable(err)
	}

	return err
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
