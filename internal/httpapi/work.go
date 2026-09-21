package httpapi

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/primitives-go/v2/observability/logging"
	"github.com/primandproper/primitives-go/v2/routing"
)

// The /_work_/ endpoints are the leader side of the leader/follower split (design
// §8): the shared leased queue over the FreshnessIndex, exposed over HTTP so
// follower processes (`beeline work`) can claim pending pairs, compute them with
// their own engines, and submit the results back. The index's lease semantics make
// the protocol safe without any session state: a follower that dies or loses
// connectivity simply lets its leases expire, and the pairs are reclaimed. Like
// every endpoint in this prototype the pair is unauthenticated; a real deploy
// would gate them.

// maxClaimPairs bounds one claim, mirroring maxWarmPairs. maxClaimLease bounds the
// visibility timeout a follower may request, so a buggy client cannot park pairs
// out of the queue for hours (note the index's per-area lease override wins over
// the requested value whenever the area sets one).
const (
	maxClaimPairs = 10_000
	maxClaimLease = 10 * time.Minute
)

// workPair is one claimed pair on the wire: hex H3 cells plus the partition and
// profile the estimate must be written back under. Submit results echo these
// fields verbatim.
type workPair struct {
	Profile string `json:"profile"`
	Origin  string `json:"origin"`
	Dest    string `json:"dest"`
	Area    int64  `json:"area"`
	Res     int    `json:"res"`
}

// claimRequest asks for work. Zero values fall back to the leader's own
// refreshBatch and leaseDuration; explicit values are capped.
type claimRequest struct {
	BatchSize    int `json:"batchSize"`
	LeaseSeconds int `json:"leaseSeconds"`
}

// claimAreaMeta is the per-area metadata a follower needs to compute a claimed
// pair the way the leader would: the routing-provider name the area resolves
// through, matched against the follower's own provider registry.
type claimAreaMeta struct {
	RoutingProvider string `json:"routingProvider"`
}

// claimResponse hands out leased pairs. Areas covers each distinct area id in
// Pairs (keys are the ids in decimal). LeaseSeconds echoes the granted
// (defaulted/capped) lease so the follower knows its visibility budget.
// ProvidersHash is the provider catalog's current content hash: a follower holding
// a different hash refetches /_work_/providers and rebuilds its engines before
// computing, so provider config changes propagate within one claim cycle.
type claimResponse struct {
	Areas         map[string]claimAreaMeta `json:"areas,omitempty"`
	ProvidersHash string                   `json:"providersHash,omitempty"`
	Pairs         []workPair               `json:"pairs"`
	LeaseSeconds  int                      `json:"leaseSeconds"`
}

// claimHandler leases up to batchSize due pairs to the calling follower via the
// same Index.Claim the local refresh pool uses, so leader workers and followers
// drain one queue with identical priority order.
func claimHandler(deps *Deps, logger logging.Logger) routing.Handler[claimRequest, claimResponse] {
	return func(ctx context.Context, req claimRequest) (claimResponse, error) {
		var zero claimResponse

		if req.BatchSize < 0 {
			fail(ctx, logger, http.StatusBadRequest, "batchSize must be >= 0")
			return zero, nil
		}
		if req.LeaseSeconds < 0 {
			fail(ctx, logger, http.StatusBadRequest, "leaseSeconds must be >= 0")
			return zero, nil
		}

		batch := req.BatchSize
		if batch == 0 {
			batch = deps.RefreshBatch
		}
		if batch > maxClaimPairs {
			batch = maxClaimPairs
		}

		lease := time.Duration(req.LeaseSeconds) * time.Second
		if lease == 0 {
			lease = deps.LeaseDuration
		}
		if lease > maxClaimLease {
			lease = maxClaimLease
		}

		keys, err := deps.Index.Claim(ctx, batch, lease)
		if err != nil {
			return zero, internalError("claiming work for follower", err)
		}

		resp := claimResponse{
			Pairs:        make([]workPair, 0, len(keys)),
			LeaseSeconds: int(lease / time.Second),
		}
		if deps.Coordinator != nil {
			resp.ProvidersHash = deps.Coordinator.ProvidersHash()
		}
		for i := range keys {
			resp.Pairs = append(resp.Pairs, workPair{
				Profile: string(keys[i].Profile),
				Origin:  keys[i].Origin.String(),
				Dest:    keys[i].Dest.String(),
				Area:    int64(keys[i].Area),
				Res:     keys[i].Res,
			})
			if deps.Coordinator == nil {
				continue
			}
			id := strconv.FormatInt(int64(keys[i].Area), 10)
			if _, seen := resp.Areas[id]; !seen {
				if resp.Areas == nil {
					resp.Areas = make(map[string]claimAreaMeta)
				}
				resp.Areas[id] = claimAreaMeta{RoutingProvider: deps.Coordinator.ProviderNameFor(keys[i].Area)}
			}
		}

		return resp, nil
	}
}

// workProvidersHandler serves the complete provider catalog — every spec plus the
// profile speed map, stamped with its content hash. Followers fetch it at startup
// and whenever a claim response carries an unfamiliar hash, then build their
// engines from it: provider configuration lives only on the leader, and followers
// need to know nothing but a leader URL.
func workProvidersHandler(deps *Deps) routing.Handler[routing.Empty, beeline.ProviderCatalog] {
	return func(_ context.Context, _ routing.Empty) (beeline.ProviderCatalog, error) {
		return deps.Coordinator.Catalog(), nil
	}
}

// submitResult is one computed estimate coming back: the claimed pair echoed
// verbatim plus the scalars. No timestamp travels — the leader stamps ComputedAt
// with its own clock at receipt, so follower clock skew can never distort
// freshness ordering (the cost, one network RTT of apparent extra freshness, is
// noise against TTLs measured in tens of seconds).
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

type submitResponse struct {
	Accepted int `json:"accepted"`
}

// submitHandler writes a follower's computed estimates and marks them fresh —
// the remote half of what LocalSource.Submit does in-process. Results for areas
// disabled since the claim are dropped (MarkComputed would ignore their vanished
// keys anyway, but Store.Put would happily resurrect estimates the disable just
// purged). An invalid cell fails the whole request: results echo leader-issued
// keys, so garbage here means a broken follower, not bad user input.
func submitHandler(deps *Deps, logger logging.Logger) routing.Handler[submitRequest, submitResponse] {
	return func(ctx context.Context, req submitRequest) (submitResponse, error) {
		var zero submitResponse

		if len(req.Results) == 0 {
			fail(ctx, logger, http.StatusBadRequest, "results must be non-empty")
			return zero, nil
		}
		if len(req.Results) > maxClaimPairs {
			fail(ctx, logger, http.StatusBadRequest,
				"too many results: limit "+strconv.Itoa(maxClaimPairs))
			return zero, nil
		}

		now := time.Now()
		entries := make([]beeline.Entry, 0, len(req.Results))
		keys := make([]beeline.PairKey, 0, len(req.Results))
		for i := range req.Results {
			res := &req.Results[i]
			origin, err := parseCell(res.Origin)
			if err != nil {
				fail(ctx, logger, http.StatusBadRequest, "invalid origin: "+err.Error())
				return zero, nil
			}
			dest, err := parseCell(res.Dest)
			if err != nil {
				fail(ctx, logger, http.StatusBadRequest, "invalid dest: "+err.Error())
				return zero, nil
			}

			area := beeline.AreaID(res.Area)
			if deps.Coordinator != nil && !deps.Coordinator.AreaEnabled(area) {
				continue
			}

			key := beeline.PairKey{
				Area:    area,
				Origin:  origin,
				Dest:    dest,
				Profile: beeline.Profile(res.Profile),
				Res:     res.Res,
			}
			entries = append(entries, beeline.Entry{
				Key:      key,
				Duration: res.DurationSec, Distance: res.DistanceMeters,
				ComputedAt: now,
			})
			keys = append(keys, key)
		}

		if len(entries) > 0 {
			if err := deps.Store.Put(ctx, entries); err != nil {
				return zero, internalError("writing follower estimates", err)
			}
			if err := deps.Index.MarkComputed(ctx, keys, now); err != nil {
				return zero, internalError("marking follower estimates computed", err)
			}
		}

		return submitResponse{Accepted: len(entries)}, nil
	}
}
