package postgres

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
)

// bumpFlushTimeout bounds one merged upsert. It is generous on purpose: the
// batch's waiters have already given up their own deadlines to it, and a bump
// that lands late still schedules refresh correctly.
const bumpFlushTimeout = 10 * time.Second

// bumpFlushAttempts bounds how many times one merged batch is retried after a
// class 40 failure. Ordered locking (see bumpKeys) makes a deadlock against
// another bump impossible; this covers the residual cross-statement case — a
// MarkComputed transaction touching the same rows in claim order.
const bumpFlushAttempts = 3

// errBumpClosed rejects a bump that arrives after the index has been closed,
// rather than parking the caller on a batch nothing will ever flush.
var errBumpClosed = errors.New("freshness: index closed")

// bumpBatcher merges concurrent Bump calls into one upsert (group commit).
//
// The read path bumps on every stale cache hit, so under load a head issues one
// multi-row upsert per in-flight request against the same handful of popular
// rows. That is what took this service down: ~30 pool connections all sitting in
// INSERT … ON CONFLICT DO UPDATE on pair_freshness, deadlocking against each
// other (SQLSTATE 40P01) and starving every other query — including the
// control plane's area list — of a connection.
//
// Merging fixes the starvation at its root: however many requests are bumping,
// exactly one bump statement is ever in flight per head, and overlapping key
// sets collapse into one row apiece instead of contending for the same row.
//
// Callers still block until their own keys have landed, so Bump keeps the
// read-your-write contract the conformance suite pins on both index
// implementations (bump, then claim, and the bumped pair comes first). A caller
// waits for at most one in-flight flush plus its own.
type bumpBatcher struct {
	index *Index
	// open is the batch now accepting keys; the flusher swaps it out under mu,
	// so a caller that captured it is guaranteed its keys ride that flush.
	open   *bumpBatch
	wake   chan struct{}
	stop   chan struct{}
	done   chan struct{}
	mu     sync.Mutex
	closed bool
}

// bumpBatch is one merged group of keys plus the result its waiters read.
// Waiters read err only after done closes, which the flusher does last.
type bumpBatch struct {
	keys map[beeline.PairKey]struct{}
	done chan struct{}
	err  error
}

func newBumpBatcher(index *Index) *bumpBatcher {
	b := &bumpBatcher{
		index: index,
		wake:  make(chan struct{}, 1),
		stop:  make(chan struct{}),
		done:  make(chan struct{}),
	}
	go b.run()

	return b
}

// bump adds keys to the batch currently accepting them and blocks until that
// batch has been written. A caller whose context expires stops waiting but does
// not cancel the flush: the batch is shared, and its other waiters still need
// it.
func (b *bumpBatcher) bump(ctx context.Context, keys []beeline.PairKey) error {
	batch := b.join(keys)

	select {
	case <-batch.done:
		return batch.err
	case <-ctx.Done():
		return fmt.Errorf("freshness: bumping: %w", ctx.Err())
	}
}

// join merges keys into the open batch (creating it if this is the first
// caller) and nudges the flusher. The returned batch is the one those keys will
// ride, captured under the same lock that lets the flusher swap it out.
func (b *bumpBatcher) join(keys []beeline.PairKey) *bumpBatch {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.closed {
		return closedBatch()
	}

	if b.open == nil {
		b.open = &bumpBatch{
			keys: make(map[beeline.PairKey]struct{}, len(keys)),
			done: make(chan struct{}),
		}
	}
	for pos := range keys {
		b.open.keys[keys[pos]] = struct{}{}
	}
	batch := b.open

	select {
	case b.wake <- struct{}{}:
	default: // a flush is already pending; this batch is part of it
	}

	return batch
}

// take swaps the open batch out for flushing. Keys arriving after this point
// start the next batch.
func (b *bumpBatcher) take() *bumpBatch {
	b.mu.Lock()
	defer b.mu.Unlock()

	batch := b.open
	b.open = nil

	return batch
}

// dropArea discards buffered bumps for one area (Unseed), so a flush cannot
// resurrect a disabled area's pairs — the same guard the access buffer keeps.
func (b *bumpBatcher) dropArea(area beeline.AreaID) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.open == nil {
		return
	}
	for k := range b.open.keys {
		if k.Area == area {
			delete(b.open.keys, k)
		}
	}
}

// run flushes whatever has accumulated, one batch at a time, until close.
func (b *bumpBatcher) run() {
	defer close(b.done)

	for {
		select {
		case <-b.stop:
			return
		case <-b.wake:
		}

		b.flush(b.take())
	}
}

// flush writes one batch and releases its waiters. Every exit path closes done
// exactly once — a waiter parked on a batch that never completes would hold a
// request open until its own context expired.
func (b *bumpBatcher) flush(batch *bumpBatch) {
	if batch == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), bumpFlushTimeout)
	defer cancel()

	batch.err = b.write(ctx, keysOf(batch.keys))
	close(batch.done)
}

// write applies one merged batch, retrying the transient class 40 conditions
// Postgres resolves by re-running the statement.
func (b *bumpBatcher) write(ctx context.Context, keys []beeline.PairKey) error {
	if len(keys) == 0 {
		return nil
	}

	var err error
	for attempt := 1; attempt <= bumpFlushAttempts; attempt++ {
		if err = bumpKeys(ctx, b.index.pool, keys); err == nil {
			return nil
		}
		if !isSerializationFailure(err) {
			return err
		}
		b.index.log.WithValues(map[string]any{
			"keys": len(keys), "attempt": attempt, "error": err.Error(),
		}).Info("retrying demand bump after a serialization failure")
	}

	return err
}

// close stops the flusher and writes whatever was still accumulating, so a
// caller blocked in bump during shutdown gets a real answer instead of hanging
// until its context expires.
func (b *bumpBatcher) close(ctx context.Context) error {
	close(b.stop)
	<-b.done // an in-flight flush finishes before run returns

	b.mu.Lock()
	b.closed = true
	batch := b.open
	b.open = nil
	b.mu.Unlock()

	if batch == nil {
		return nil
	}
	batch.err = bumpKeys(ctx, b.index.pool, keysOf(batch.keys))
	close(batch.done)

	return batch.err
}

// closedBatch is an already-completed batch carrying the shutdown error.
func closedBatch() *bumpBatch {
	batch := &bumpBatch{done: make(chan struct{}), err: errBumpClosed}
	close(batch.done)

	return batch
}

// keysOf flattens a batch's key set.
func keysOf(set map[beeline.PairKey]struct{}) []beeline.PairKey {
	keys := make([]beeline.PairKey, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}

	return keys
}

// bumpKeys is the upsert itself: unknown keys join the working set as unpinned
// demand entries, known keys are flagged for priority refresh, and either way
// the access stamp advances (a bump is also an access).
//
// The ORDER BY is load-bearing, not tidiness. ON CONFLICT DO UPDATE locks each
// conflicting row as the SELECT reaches it, so two overlapping batches arriving
// in different row orders deadlock (SQLSTATE 40P01) — which is exactly how this
// table wedged in production. A total order over the primary key makes every
// writer of this table (here and in flushAccess) acquire its row locks in the
// same sequence, so contention degrades into a queue instead of a cycle.
//
// DISTINCT is load-bearing too: ON CONFLICT DO UPDATE refuses to touch the same
// row twice in one statement (SQLSTATE 21000), and callers legitimately pass
// duplicates — a /table grid whose coordinates collapse onto one H3 cell pair,
// or a warm-set feed naming a pair twice. Without it Postgres rejects the WHOLE
// batch, so one repeat silently discards every demand signal alongside it. The
// batcher's key set already dedupes; this keeps the statement correct on its
// own terms. The memory index, looping over a map, has always tolerated
// duplicates; the conformance suite pins both.
func bumpKeys(ctx context.Context, db execer, keys []beeline.PairKey) error {
	areas, profiles, resolutions, origins, dests := keyColumns(keys)
	if _, err := db.Exec(ctx, `
		INSERT INTO pair_freshness AS pf (area_id, profile, res, origin, dest, bumped)
		SELECT DISTINCT area_id, profile, res, origin, dest, TRUE
		FROM unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[])
		     AS k(area_id, profile, res, origin, dest)
		ORDER BY area_id, profile, res, origin, dest
		ON CONFLICT (area_id, profile, res, origin, dest) DO UPDATE
		SET bumped = TRUE, last_access = now()`,
		areas, profiles, resolutions, origins, dests); err != nil {
		return fmt.Errorf("freshness: bumping: %w", err)
	}

	return nil
}
