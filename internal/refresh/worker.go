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

	"github.com/primandproper/platform-go/v4/observability/logging"
)

// Config tunes the refresh pool.
type Config struct {
	Workers     int           // number of concurrent worker goroutines
	Batch       int           // max keys claimed per iteration
	Lease       time.Duration // visibility timeout on a claim
	IdleBackoff time.Duration // sleep when the queue has nothing due
}

// Pool owns the worker goroutines and the dependencies they share.
type Pool struct {
	resolver beeline.EngineResolver
	store    beeline.Store
	index    beeline.FreshnessIndex
	logger   logging.Logger
	cfg      Config
}

// NewPool wires a refresh pool. The resolver selects the routing engine per area, so a
// claimed batch spanning several areas routes each through its own provider. It does
// not start any goroutines; call Run.
func NewPool(resolver beeline.EngineResolver, store beeline.Store, index beeline.FreshnessIndex, logger logging.Logger, cfg Config) *Pool {
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.Batch < 1 {
		cfg.Batch = 1
	}
	if cfg.IdleBackoff <= 0 {
		cfg.IdleBackoff = 250 * time.Millisecond
	}

	return &Pool{resolver: resolver, store: store, index: index, logger: logging.EnsureLogger(logger), cfg: cfg}
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

		keys, err := p.index.Claim(ctx, p.cfg.Batch, p.cfg.Lease)
		if err != nil {
			p.logger.Error("claiming refresh work", err)
			if !sleep(ctx, p.cfg.IdleBackoff) {
				return
			}

			continue
		}

		if len(keys) == 0 {
			// Caught up: nothing is due. Idle until something ages out or is bumped.
			if !sleep(ctx, p.cfg.IdleBackoff) {
				return
			}

			continue
		}

		p.refresh(ctx, keys)
	}
}

// refresh computes and stores a claimed batch. Keys are grouped by (origin,
// profile) so each group is a single dense 1×K table request (§6).
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

	var (
		entries []beeline.Entry
		done    []beeline.PairKey
	)

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
			done = append(done, groupKeys[j])
		}
	}

	if len(entries) == 0 {
		return
	}

	if err := p.store.Put(ctx, entries); err != nil {
		p.logger.Error("writing estimates", err)
		return // leave the lease to expire and be retried
	}

	if err := p.index.MarkComputed(ctx, done, now); err != nil {
		p.logger.Error("marking computed", err)
	}
}

// sleep waits for d or until ctx is cancelled. It returns false if ctx was
// cancelled (the caller should stop).
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
