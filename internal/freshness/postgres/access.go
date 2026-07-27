package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v7/clock"

	"github.com/jackc/pgx/v5/pgconn"
)

// drainTimeout bounds a background flush (and the final drain when close gets
// an already-cancelled context).
const drainTimeout = 5 * time.Second

// accessBuffer coalesces Access stamps so the read path never pays a
// per-request index round trip: keys are deduped in memory and upserted in one
// batch when the flush interval elapses or the buffer fills. MarkComputed
// steals its own keys out of the buffer first (take), preserving the
// Access-before-MarkComputed ordering the demand-fill commit depends on; the
// remaining stamps are at most one interval stale, which is noise against
// minutes-scale sweep cutoffs.
type accessBuffer struct {
	index   *Index
	clock   clock.Clock
	pending map[beeline.PairKey]struct{}
	wake    chan struct{}
	stop    chan struct{}
	done    chan struct{}

	interval time.Duration
	limit    int

	mu sync.Mutex
}

func newAccessBuffer(index *Index, clk clock.Clock, interval time.Duration, limit int) *accessBuffer {
	b := &accessBuffer{
		index:    index,
		clock:    clk,
		pending:  make(map[beeline.PairKey]struct{}),
		wake:     make(chan struct{}, 1),
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
		interval: interval,
		limit:    limit,
	}
	go b.run()

	return b
}

// add records keys for the next flush, nudging the flusher when the buffer
// fills. Never blocks on I/O.
func (b *accessBuffer) add(keys []beeline.PairKey) {
	if len(keys) == 0 {
		return
	}

	b.mu.Lock()
	for pos := range keys {
		b.pending[keys[pos]] = struct{}{}
	}
	full := len(b.pending) >= b.limit
	b.mu.Unlock()

	if full {
		select {
		case b.wake <- struct{}{}:
		default:
		}
	}
}

// take removes and returns the subset of keys currently buffered, so a caller
// (MarkComputed) can flush exactly its own keys inside its own transaction.
func (b *accessBuffer) take(keys []beeline.PairKey) []beeline.PairKey {
	b.mu.Lock()
	defer b.mu.Unlock()

	var taken []beeline.PairKey
	for pos := range keys {
		if _, ok := b.pending[keys[pos]]; ok {
			delete(b.pending, keys[pos])
			taken = append(taken, keys[pos])
		}
	}

	return taken
}

// dropArea discards buffered keys for one area (Unseed), so a later flush
// cannot resurrect a disabled area's pairs.
func (b *accessBuffer) dropArea(area beeline.AreaID) {
	b.mu.Lock()
	defer b.mu.Unlock()

	for k := range b.pending {
		if k.Area == area {
			delete(b.pending, k)
		}
	}
}

// swap takes the whole buffer for a flush.
func (b *accessBuffer) swap() []beeline.PairKey {
	b.mu.Lock()
	defer b.mu.Unlock()

	if len(b.pending) == 0 {
		return nil
	}
	keys := make([]beeline.PairKey, 0, len(b.pending))
	for k := range b.pending {
		keys = append(keys, k)
	}
	b.pending = make(map[beeline.PairKey]struct{})

	return keys
}

// run flushes on the interval, on a fullness nudge, and stops on close.
func (b *accessBuffer) run() {
	defer close(b.done)

	ticker := b.clock.NewTicker(b.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stop:
			return
		case <-ticker.Chan():
		case <-b.wake:
		}

		if keys := b.swap(); len(keys) > 0 {
			ctx, cancel := context.WithTimeout(context.Background(), drainTimeout)
			if err := flushAccess(ctx, b.index.pool, keys); err != nil {
				// The swapped stamps are dropped, not retried: access recency is
				// advisory (sweep cutoffs are minutes-scale), and retrying against
				// a down database would just grow the buffer without bound.
				b.index.log.WithValues(map[string]any{"dropped": len(keys)}).
					Error("flushing access stamps", err)
			}
			cancel()
		}
	}
}

// close stops the flusher and drains whatever is left.
func (b *accessBuffer) close(ctx context.Context) error {
	close(b.stop)
	<-b.done

	if keys := b.swap(); len(keys) > 0 {
		return flushAccess(ctx, b.index.pool, keys)
	}

	return nil
}

// execer is the slice of pgx that both a pool and a transaction satisfy, so
// one flush implementation serves the background flusher and MarkComputed's
// in-transaction ordering flush.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// flushAccess upserts one batch of access stamps: unknown keys become unpinned
// never-computed demand entries (their last_access defaults to now()), known
// keys get their last_access refreshed.
//
// The ORDER BY puts this statement on the same total row-lock order as bumpKeys
// — the two upserts share this table, and one ordering them without the other
// would still leave a deadlock cycle between them.
func flushAccess(ctx context.Context, db execer, keys []beeline.PairKey) error {
	areas, profiles, resolutions, origins, dests := keyColumns(keys)
	if _, err := db.Exec(ctx, `
		INSERT INTO pair_freshness (area_id, profile, res, origin, dest)
		SELECT DISTINCT area_id, profile, res, origin, dest
		FROM unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[])
		     AS k(area_id, profile, res, origin, dest)
		ORDER BY area_id, profile, res, origin, dest
		ON CONFLICT (area_id, profile, res, origin, dest) DO UPDATE
		SET last_access = now()`,
		areas, profiles, resolutions, origins, dests); err != nil {
		return fmt.Errorf("freshness: flushing access: %w", err)
	}

	return nil
}
