// Package refresh runs the continuous incremental refresh loop from §3/§4: the
// worker + engine unit. Each worker claims the stalest due pairs, packs them into
// dense origin-centric table requests (§6), computes them through the engine,
// writes the estimates, and marks them fresh. Adding workers adds throughput and
// burns freshness debt down proportionally (§8), because the only shared state is
// the store (writes) and the index (claims).
package refresh

import (
	"context"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v10/clock"
	"github.com/primandproper/platform-go/v10/observability/logging"
)

// defaultSubmitDivisor derives the default flush size as a fraction of the claim
// batch, so a worker flushes about this many times per claim whatever the operator
// tuned Batch to. A fixed default would be wrong at both ends: set to the common
// batch size it never flushes early at all, and set well below it a large batch
// pays dozens of round trips. Four bounds the work a dying worker forfeits — and
// the lag before a computed pair is visibly fresh — to roughly a quarter of a
// batch, while keeping a follower to a handful of submits per claim.
const defaultSubmitDivisor = 4

// Config tunes the refresh pool.
type Config struct {
	Workers     int           // number of concurrent worker goroutines
	Batch       int           // max keys claimed per iteration
	SubmitChunk int           // max computed entries buffered before a flush (0 = Batch/4)
	Lease       time.Duration // visibility timeout on a claim
	IdleBackoff time.Duration // sleep when the queue has nothing due
}

// Pool owns the worker goroutines and the dependencies they share.
type Pool struct {
	resolver beeline.EngineResolver
	source   WorkSource
	logger   logging.Logger
	clock    clock.Clock
	cfg      Config
}

// NewPool wires a refresh pool. The resolver selects the routing engine per area, so a
// claimed batch spanning several areas routes each through its own provider. The source
// is where claims come from and results go — local index+store on a leader, an HTTP
// client on a follower. It does not start any goroutines; call Run.
func NewPool(resolver beeline.EngineResolver, source WorkSource, logger logging.Logger, cfg Config) *Pool {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.Batch < 1 {
		cfg.Batch = 1
	}
	// After the Batch floor above, so the derived chunk is never zero.
	if cfg.SubmitChunk < 1 {
		cfg.SubmitChunk = max(1, cfg.Batch/defaultSubmitDivisor)
	}
	if cfg.IdleBackoff <= 0 {
		cfg.IdleBackoff = 250 * time.Millisecond
	}

	return &Pool{resolver: resolver, source: source, logger: logging.EnsureLogger(logger), clock: clock.NewClock(), cfg: cfg}
}

// Run starts the workers and blocks until ctx is cancelled, then waits for them to
// drain. It is meant to run in its own goroutine.
func (p *Pool) Run(ctx context.Context) {
	var wg sync.WaitGroup

	for w := 0; w < p.cfg.Workers; w++ {
		wg.Go(func() {
			p.work(ctx)
		})
	}

	wg.Wait()
	p.logger.Debug("refresh pool stopped")
}

// work is one worker's claim→compute→write→mark loop.
func (p *Pool) work(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}

		keys, err := p.source.Claim(ctx, p.cfg.Batch, p.cfg.Lease)
		if err != nil {
			p.logger.Error("claiming refresh work", err)
			if p.clock.Sleep(ctx, p.cfg.IdleBackoff) != nil {
				return
			}

			continue
		}

		if len(keys) == 0 {
			// Caught up: nothing is due. Idle until something ages out or is bumped.
			if p.clock.Sleep(ctx, p.cfg.IdleBackoff) != nil {
				return
			}

			continue
		}

		p.refresh(ctx, keys)
	}
}

// refresh computes and stores a claimed batch. Keys are grouped by (origin,
// profile) so each group is a single dense 1×K table request (§6), and results are
// flushed to the source every SubmitChunk entries rather than once at the end: a
// long batch's early groups become durable — and visibly fresh — while its later
// groups are still in the engine, and a worker that dies mid-batch forfeits only
// the current chunk instead of every group it had computed.
func (p *Pool) refresh(ctx context.Context, keys []beeline.PairKey) {
	// An H3 cell id encodes its resolution, so (area, origin, profile) fully identifies
	// a dense 1×K request; resolution need not be part of the group key. Area IS part of
	// it: different areas may route through different engines (per-area providers), so a
	// group must be single-area for its one Table call to hit the right engine — and each
	// key still carries its Area, so estimates are written under the correct partition
	// even when overlapping areas share an origin cell in one batch.
	type groupKey struct {
		profile beeline.Profile
		area    beeline.AreaID
		origin  beeline.H3Cell
	}

	groups := make(map[groupKey][]beeline.PairKey)
	for pos := range keys {
		g := groupKey{origin: keys[pos].Origin, profile: keys[pos].Profile, area: keys[pos].Area}
		groups[g] = append(groups[g], keys[pos])
	}

	now := time.Now()

	var entries []beeline.Entry

	for g, groupKeys := range groups {
		origin, err := beeline.Center(g.origin)
		if err != nil {
			p.logger.Error("resolving origin center", err)
			continue
		}

		dests := make([]beeline.LatLng, 0, len(groupKeys))
		for pos := range groupKeys {
			center, centerErr := beeline.Center(groupKeys[pos].Dest)
			if centerErr != nil {
				p.logger.Error("resolving destination center", centerErr)
				dests = append(dests, beeline.LatLng{}) // keep positional alignment
				continue
			}
			dests = append(dests, center)
		}

		resp, err := p.resolver.EngineFor(g.area).Table(ctx, beeline.TableRequest{
			Sources:      []beeline.LatLng{origin},
			Destinations: dests,
			Profile:      g.profile,
			Want:         beeline.AnnotateDuration | beeline.AnnotateDistance,
		})
		if err != nil {
			p.logger.Error("computing table", err)
			continue
		}

		for j := range groupKeys {
			est := beeline.Estimate{}
			if len(resp.Duration) > 0 {
				est.Duration = resp.Duration[0][j]
			}
			if len(resp.Distance) > 0 {
				est.Distance = resp.Distance[0][j]
			}

			entries = append(entries, beeline.Entry{
				Key:    groupKeys[j],
				Stored: beeline.Stored{Estimate: est, ComputedAt: now},
			})
		}

		if len(entries) < p.cfg.SubmitChunk {
			continue
		}
		if !p.flush(ctx, entries) {
			return
		}
		entries = nil
	}

	p.flush(ctx, entries)
}

// flush hands the buffered entries to the work source. It reports whether the
// batch is worth continuing: a Submit failure means the sink is unhealthy — an
// unreachable leader for a follower, a failing store or index for a leader — so
// computing the claim's remaining groups would only pile up results with nowhere
// to put them. Either way the unsubmitted pairs keep their leases until those
// expire and the pairs are reclaimed, exactly as before.
func (p *Pool) flush(ctx context.Context, entries []beeline.Entry) bool {
	if len(entries) == 0 {
		return true
	}

	if err := p.source.Submit(ctx, entries); err != nil {
		p.logger.Error("submitting computed estimates", err)

		return false
	}

	return true
}
