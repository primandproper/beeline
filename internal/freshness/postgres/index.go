// Package postgres is the Postgres beeline.FreshnessIndex: the shared
// scheduling brain that lets any number of stateless heads and followers drain
// one leased queue (design §5.3/§8). Claim is the canonical
// SELECT … FOR UPDATE SKIP LOCKED job-queue shape; every timestamp that governs
// scheduling — lease expiry, staleness, access recency, ComputedAt — comes from
// the database's now(), so process clocks never have to agree. No fencing
// tokens are needed: writes stay idempotent (double compute is waste, not
// corruption) exactly as in the in-memory index, and MarkComputed stamps its
// own authoritative time, so a straggler's late submit can never regress
// freshness ordering.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/primitives-go/v2/clock"
	"github.com/primandproper/primitives-go/v2/observability/logging"
	"github.com/primandproper/primitives-go/v2/observability/metrics"
	"github.com/primandproper/primitives-go/v2/observability/tracing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The class 40 SQLSTATEs Postgres resolves by retrying the whole transaction.
const (
	pgerrSerializationFailure = "40001"
	pgerrDeadlockDetected     = "40P01"
)

// Config tunes the index. TargetTTL is the index-wide freshness default an
// area's override falls back to (the memory index's New parameter).
type Config struct {
	// Clock drives the access flusher's ticker only; every scheduling timestamp
	// still comes from the database's now(). Nil takes the wall clock.
	Clock clock.Clock
	// TracerProvider and MetricsProvider instrument the stats memo caches. Nil
	// values become noops.
	TracerProvider  tracing.Provider
	MetricsProvider metrics.Provider
	TargetTTL       time.Duration
	// StatsCacheTTL memoizes Debt/DebtForArea/CellStatesForArea per head — the
	// console polls twice a second and these are aggregate scans. 0 disables
	// (tests want uncached reads).
	StatsCacheTTL time.Duration
	// AccessFlushInterval and AccessFlushLimit bound the coalescing buffer that
	// keeps the read path's per-query Access stamps from becoming per-query
	// round trips. Zero values take the package defaults.
	AccessFlushInterval time.Duration
	AccessFlushLimit    int
}

const (
	defaultAccessFlushInterval = time.Second
	defaultAccessFlushLimit    = 10_000
)

// slowQueryThreshold flags index operations that took long enough to matter
// (§11: claim latency and debt scan time are first-class signals).
const slowQueryThreshold = time.Second

// Index implements beeline.FreshnessIndex and control.AreaIndex over a shared
// pgx pool. Construct with New; call Close on shutdown to drain the access
// buffer.
type Index struct {
	log    logging.Logger
	pool   *pgxpool.Pool
	access *accessBuffer
	bumps  *bumpBatcher
	stats  *statsCache
	cfg    Config
}

// New builds the index and starts its access-flush goroutine.
func New(pool *pgxpool.Pool, cfg *Config, log logging.Logger) (*Index, error) {
	if cfg.AccessFlushInterval <= 0 {
		cfg.AccessFlushInterval = defaultAccessFlushInterval
	}
	if cfg.AccessFlushLimit <= 0 {
		cfg.AccessFlushLimit = defaultAccessFlushLimit
	}
	if cfg.Clock == nil {
		cfg.Clock = clock.NewClock()
	}

	log = logging.EnsureLogger(log)

	i := &Index{
		pool: pool,
		log:  log,
		cfg:  *cfg,
	}

	// After the Index exists: the memo reads through to i's own scans, so the
	// loaders have to close over it.
	stats, err := newStatsCache(i, cfg.StatsCacheTTL, log, cfg.TracerProvider, cfg.MetricsProvider)
	if err != nil {
		return nil, err
	}
	i.stats = stats

	i.access = newAccessBuffer(i, cfg.Clock, cfg.AccessFlushInterval, cfg.AccessFlushLimit)
	i.bumps = newBumpBatcher(i)

	return i, nil
}

// Close stops the access flusher and the bump batcher, draining whatever each
// still holds. Both are drained even if the first fails: leaving a bump's
// waiters parked would outlive the shutdown that is trying to release them.
func (i *Index) Close(ctx context.Context) error {
	return errors.Join(i.access.close(ctx), i.bumps.close(ctx))
}

// keyColumns splits keys into the five parallel arrays every unnest join uses.
func keyColumns(keys []beeline.PairKey) (areas []int64, profiles []string, resolutions []int16, origins, dests []int64) {
	areas = make([]int64, len(keys))
	profiles = make([]string, len(keys))
	resolutions = make([]int16, len(keys))
	origins = make([]int64, len(keys))
	dests = make([]int64, len(keys))
	for pos := range keys {
		areas[pos] = int64(keys[pos].Area)
		profiles[pos] = string(keys[pos].Profile)
		resolutions[pos] = int16(keys[pos].Res)
		origins[pos] = int64(keys[pos].Origin)
		dests[pos] = int64(keys[pos].Dest)
	}

	return areas, profiles, resolutions, origins, dests
}

// seedCopyChunk bounds one CopyFrom batch while seeding.
const seedCopyChunk = 100_000

// Seed adds keys to the working set as never-computed, pinned pairs; existing
// keys — including unpinned demand entries — are left untouched, so re-seeding
// is idempotent across racing heads (ON CONFLICT DO NOTHING). The
// achieved-throughput baseline of every area in the batch is reset so a freshly
// enabled area's progress reads from zero.
func (i *Index) Seed(ctx context.Context, keys []beeline.PairKey) error {
	if len(keys) == 0 {
		return nil
	}

	conn, err := i.pool.Acquire(ctx)
	if err != nil {
		return fmt.Errorf("freshness: acquiring seed conn: %w", err)
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("freshness: beginning seed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	if _, err = tx.Exec(ctx, `
		CREATE TEMP TABLE seed_keys (
		    area_id BIGINT, profile TEXT, res SMALLINT, origin BIGINT, dest BIGINT
		) ON COMMIT DROP`); err != nil {
		return fmt.Errorf("freshness: creating seed temp table: %w", err)
	}

	for start := 0; start < len(keys); start += seedCopyChunk {
		end := min(start+seedCopyChunk, len(keys))
		chunk := keys[start:end]
		if _, err = tx.CopyFrom(ctx, pgx.Identifier{"seed_keys"},
			[]string{"area_id", "profile", "res", "origin", "dest"},
			pgx.CopyFromSlice(len(chunk), func(row int) ([]any, error) {
				k := chunk[row]
				return []any{int64(k.Area), string(k.Profile), int16(k.Res), int64(k.Origin), int64(k.Dest)}, nil
			})); err != nil {
			return fmt.Errorf("freshness: copying seed keys: %w", err)
		}
	}

	if _, err = tx.Exec(ctx, `
		INSERT INTO pair_freshness (area_id, profile, res, origin, dest, pinned)
		SELECT area_id, profile, res, origin, dest, TRUE FROM seed_keys
		ON CONFLICT (area_id, profile, res, origin, dest) DO NOTHING`); err != nil {
		return fmt.Errorf("freshness: inserting seed keys: %w", err)
	}

	if _, err = tx.Exec(ctx, `
		INSERT INTO area_baselines (area_id)
		SELECT DISTINCT area_id FROM seed_keys
		ON CONFLICT (area_id) DO UPDATE SET started_at = now(), computed_total = 0`); err != nil {
		return fmt.Errorf("freshness: resetting baselines: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("freshness: committing seed: %w", err)
	}

	return nil
}

// Claim atomically leases up to limit of the stalest due pairs. Due = not
// currently leased AND (never computed OR demand-bumped OR past its area's
// TTL); order matches the memory index's claimBefore (bumped, then
// never-computed, then oldest, ties clustered by origin for dense 1×K table
// calls). SKIP LOCKED keeps concurrent claimers from blocking each other; the
// per-area lease override, when set, wins over the requested lease. A
// non-positive limit claims everything due (the memory index's contract).
func (i *Index) Claim(ctx context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	start := time.Now()
	defer func() {
		if elapsed := time.Since(start); elapsed > slowQueryThreshold {
			i.log.WithValues(map[string]any{"elapsed": elapsed.String(), "limit": limit}).
				Info("slow freshness claim (dead-tuple churn on the claim index? check autovacuum)")
		}
	}()

	rows, err := i.pool.Query(ctx, `
		WITH due AS (
		    SELECT ctid FROM pair_freshness
		    WHERE lease_until <= now()
		      AND (bumped OR computed_at IS NULL OR stale_at <= now())
		    ORDER BY bumped DESC, computed_at ASC NULLS FIRST, origin, dest
		    LIMIT NULLIF(GREATEST($1::int, 0), 0)
		    FOR UPDATE SKIP LOCKED
		)
		UPDATE pair_freshness pf
		SET lease_until = now() + make_interval(secs =>
		        COALESCE(NULLIF((SELECT lease_seconds FROM area_freshness af
		                         WHERE af.area_id = pf.area_id), 0), $2))
		FROM due WHERE pf.ctid = due.ctid
		RETURNING pf.area_id, pf.profile, pf.res, pf.origin, pf.dest`,
		limit, lease.Seconds())
	if err != nil {
		return nil, fmt.Errorf("freshness: claiming: %w", err)
	}
	defer rows.Close()

	return scanKeys(rows)
}

// MarkComputed records successful refreshes: stamps computed_at/stale_at from
// the database clock (the at parameter is advisory — this backend substitutes
// its authoritative clock), releases the lease, clears the demand bump, and
// advances the per-area and global throughput counters. Unknown keys are
// ignored, exactly like the memory index (a lease may have expired and the pair
// been swept meanwhile).
//
// Any of the keys still sitting in the access buffer are flushed first, inside
// the same transaction: the read path's demand-fill commit calls Access then
// MarkComputed, and the mark must land on a row the access created.
func (i *Index) MarkComputed(ctx context.Context, keys []beeline.PairKey, _ time.Time) error {
	if len(keys) == 0 {
		return nil
	}

	pending := i.access.take(keys)

	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("freshness: beginning mark tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	if len(pending) > 0 {
		if err = flushAccess(ctx, tx, pending); err != nil {
			return err
		}
	}

	areas, profiles, resolutions, origins, dests := keyColumns(keys)
	rows, err := tx.Query(ctx, `
		UPDATE pair_freshness pf
		SET computed_at = now(),
		    stale_at    = now() + make_interval(secs =>
		        COALESCE(NULLIF((SELECT target_ttl_seconds FROM area_freshness af
		                         WHERE af.area_id = pf.area_id), 0), $6)),
		    lease_until = 'epoch',
		    bumped      = FALSE
		FROM unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[])
		     AS k(area_id, profile, res, origin, dest)
		WHERE (pf.area_id, pf.profile, pf.res, pf.origin, pf.dest)
		    = (k.area_id, k.profile, k.res, k.origin, k.dest)
		RETURNING pf.area_id`,
		areas, profiles, resolutions, origins, dests, i.cfg.TargetTTL.Seconds())
	if err != nil {
		return fmt.Errorf("freshness: marking computed: %w", err)
	}

	perArea := make(map[int64]int64)
	var total int64
	for rows.Next() {
		var area int64
		if err = rows.Scan(&area); err != nil {
			rows.Close()
			return fmt.Errorf("freshness: scanning marked areas: %w", err)
		}
		perArea[area]++
		total++
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return fmt.Errorf("freshness: reading marked areas: %w", err)
	}

	if total > 0 {
		baselineAreas := make([]int64, 0, len(perArea))
		baselineCounts := make([]int64, 0, len(perArea))
		for area, n := range perArea {
			baselineAreas = append(baselineAreas, area)
			baselineCounts = append(baselineCounts, n)
		}
		// A lazy area is never seeded, so its first computed demand fill
		// establishes the baseline here (insert), matching the memory index.
		if _, err = tx.Exec(ctx, `
			INSERT INTO area_baselines (area_id, computed_total)
			SELECT * FROM unnest($1::bigint[], $2::bigint[])
			ON CONFLICT (area_id) DO UPDATE
			SET computed_total = area_baselines.computed_total + excluded.computed_total`,
			baselineAreas, baselineCounts); err != nil {
			return fmt.Errorf("freshness: advancing baselines: %w", err)
		}
		if _, err = tx.Exec(ctx,
			`UPDATE freshness_counters SET computed_total = computed_total + $1`, total); err != nil {
			return fmt.Errorf("freshness: advancing global counter: %w", err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("freshness: committing mark: %w", err)
	}

	return nil
}

// Bump raises refresh priority for demand-driven pairs (stale-while-
// revalidate), adding unknown keys as unpinned demand entries; a bump is also
// an access.
//
// The write is merged with every other bump in flight on this head (see
// bumpBatcher) and the caller blocks until its own keys land, so the
// read-your-write contract holds — bump, then claim, and the bumped pair comes
// first — while the read path can no longer put one upsert per request on the
// pool.
func (i *Index) Bump(ctx context.Context, keys []beeline.PairKey) error {
	if len(keys) == 0 {
		return nil
	}

	return i.bumps.bump(ctx, keys)
}

// Access records that pairs were queried: unknown keys join the working set as
// unpinned demand entries and every key's last-access is stamped, without
// raising refresh priority. Writes are coalesced in a bounded buffer and
// flushed in batches (see accessBuffer) — the read path never pays a
// per-request index round trip, and sweep cutoffs are minutes-scale so the
// bounded staleness is harmless.
func (i *Index) Access(_ context.Context, keys []beeline.PairKey) error {
	i.access.add(keys)

	return nil
}

// Invalidate re-enqueues every computed pair the selector matches by clearing its
// computed_at/stale_at (so it sorts to the front of the claim order, which reads
// NULLS FIRST) and releasing any lease on it (so a pair claimed moments earlier is
// immediately re-claimable rather than swallowing the invalidation). Cached
// estimates in the store are untouched: only the schedule changes.
//
// Each scope is expressed as a nullable parameter so one plan serves every
// combination; area+res is the operator's layer invalidation and rides the primary
// key's leading column. It returns how many rows were re-enqueued.
func (i *Index) Invalidate(ctx context.Context, sel beeline.Selector) (int, error) {
	var (
		olderThan *time.Time
		area      *int64
		res       *int16
		profile   *string
	)
	if !sel.OlderThan.IsZero() {
		olderThan = &sel.OlderThan
	}
	if sel.Area != 0 {
		id := int64(sel.Area)
		area = &id
	}
	if sel.Res != nil {
		r := int16(*sel.Res) // an H3 resolution is 0-15; the HTTP edge rejects anything else
		res = &r
	}
	if sel.Profile != "" {
		p := string(sel.Profile)
		profile = &p
	}

	tag, err := i.pool.Exec(ctx, `
		UPDATE pair_freshness
		SET computed_at = NULL, stale_at = NULL, lease_until = 'epoch'
		WHERE computed_at IS NOT NULL
		  AND ($1::timestamptz IS NULL OR computed_at < $1)
		  AND ($2::bigint IS NULL OR area_id = $2)
		  AND ($3::smallint IS NULL OR res = $3)
		  AND ($4::text IS NULL OR profile = $4)`,
		olderThan, area, res, profile)
	if err != nil {
		return 0, fmt.Errorf("freshness: invalidating: %w", err)
	}

	// The memoized debt/cells aggregates would otherwise report the pre-invalidation
	// working set for another StatsCacheTTL, which is exactly the moment an operator
	// is watching the console for the debt spike they just caused.
	i.stats.purge(ctx)

	return int(tag.RowsAffected()), nil
}

// Unseed removes every pair belonging to one area, plus its freshness override
// and throughput baseline. Buffered accesses and bumps for the area are
// discarded so a later flush cannot resurrect its pairs.
func (i *Index) Unseed(ctx context.Context, area beeline.AreaID) error {
	i.access.dropArea(area)
	i.bumps.dropArea(area)

	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("freshness: beginning unseed tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	for _, stmt := range []string{
		`DELETE FROM pair_freshness WHERE area_id = $1`,
		`DELETE FROM area_freshness WHERE area_id = $1`,
		`DELETE FROM area_baselines WHERE area_id = $1`,
	} {
		if _, err = tx.Exec(ctx, stmt, int64(area)); err != nil {
			return fmt.Errorf("freshness: unseeding area %d: %w", area, err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("freshness: committing unseed: %w", err)
	}

	return nil
}

// SweepArea evicts one area's cold demand pairs: unpinned, unleased, and last
// accessed before cutoff. The removed keys are returned so the caller can drop
// them from the hot store. The cutoff comes from the caller's clock — it is
// minutes-scale (DemandIdleTTL), so head-vs-database skew is noise.
func (i *Index) SweepArea(ctx context.Context, area beeline.AreaID, cutoff time.Time) ([]beeline.PairKey, error) {
	rows, err := i.pool.Query(ctx, `
		DELETE FROM pair_freshness
		WHERE area_id = $1 AND NOT pinned AND lease_until <= now() AND last_access < $2
		RETURNING area_id, profile, res, origin, dest`, int64(area), cutoff)
	if err != nil {
		return nil, fmt.Errorf("freshness: sweeping area %d: %w", area, err)
	}
	defer rows.Close()

	return scanKeys(rows)
}

// setAreaFreshnessAttempts bounds the deadlock retry below. Postgres kills one
// transaction to break a deadlock and expects the loser to try again; the bulk
// re-derive is the biggest writer in the system and so the usual victim.
const setAreaFreshnessAttempts = 4

// SetAreaFreshness records an area's freshness contract (target TTL + claim
// lease; non-positive values fall back to the index-wide defaults) and, when
// that contract actually changed, re-derives the denormalized stale_at of its
// already-computed pairs so the new TTL takes effect immediately.
//
// "When it actually changed" is the whole trick. Callers hit this constantly
// with values that are already stored: every head runs it per enabled area at
// boot and again on every config-poll convergence (control.projectAreaLocked,
// which documents itself as idempotent). Re-deriving unconditionally made each
// of those a rewrite of every computed row in the area — a six-figure UPDATE per
// head per area, competing for row locks with the follower pool's MarkComputed
// writes, which deadlocked and (since it runs during boot) crashlooped the head.
// Skipping the no-op case is what makes the projection path idempotent in fact
// and not just in the comment; the retry below covers the genuine change that
// still collides with live traffic.
//
// Skipping is safe because MarkComputed derives stale_at from this same row at
// mark time, so an unchanged contract cannot leave a stale stale_at behind.
func (i *Index) SetAreaFreshness(ctx context.Context, area beeline.AreaID, targetTTL, lease time.Duration) error {
	var err error
	for attempt := 1; ; attempt++ {
		if err = i.setAreaFreshness(ctx, area, targetTTL, lease); err == nil {
			return nil
		}
		if attempt >= setAreaFreshnessAttempts || !isSerializationFailure(err) {
			return err
		}
		i.log.WithValues(map[string]any{
			"area": int64(area), "attempt": attempt, "error": err.Error(),
		}).Info("retrying area freshness after a serialization failure")
	}
}

// setAreaFreshness is one attempt of SetAreaFreshness.
func (i *Index) setAreaFreshness(ctx context.Context, area beeline.AreaID, targetTTL, lease time.Duration) error {
	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("freshness: beginning freshness tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	// The DO UPDATE ... WHERE suppresses the row entirely when nothing differs,
	// so RowsAffected is 0 for a genuine no-op and 1 for an insert or a real
	// change. That is the signal the re-derive below keys off.
	tag, err := tx.Exec(ctx, `
		INSERT INTO area_freshness (area_id, target_ttl_seconds, lease_seconds)
		VALUES ($1, $2, $3)
		ON CONFLICT (area_id) DO UPDATE
		SET target_ttl_seconds = excluded.target_ttl_seconds,
		    lease_seconds      = excluded.lease_seconds
		WHERE area_freshness.target_ttl_seconds IS DISTINCT FROM excluded.target_ttl_seconds
		   OR area_freshness.lease_seconds      IS DISTINCT FROM excluded.lease_seconds`,
		int64(area), targetTTL.Seconds(), lease.Seconds())
	if err != nil {
		return fmt.Errorf("freshness: setting area freshness: %w", err)
	}

	if tag.RowsAffected() > 0 {
		// The ::float8 casts are load-bearing. Both operands here are parameters,
		// so without them Postgres infers $2's type from the bare `0` literal,
		// makes it an integer, and truncates the seconds — a sub-second TTL
		// becomes 0, which is this expression's "fall back to the index-wide
		// default" sentinel. The area's override would then be silently ignored
		// by exactly the statement whose job is to apply it. (The sibling
		// expressions in Claim and MarkComputed take their NULLIF operand from a
		// double precision column, so they were never exposed to this.)
		if _, err = tx.Exec(ctx, `
			UPDATE pair_freshness
			SET stale_at = computed_at + make_interval(secs => COALESCE(NULLIF($2::float8, 0), $3::float8))
			WHERE area_id = $1 AND computed_at IS NOT NULL`,
			int64(area), targetTTL.Seconds(), i.cfg.TargetTTL.Seconds()); err != nil {
			return fmt.Errorf("freshness: rederiving stale_at: %w", err)
		}
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("freshness: committing area freshness: %w", err)
	}

	return nil
}

// isSerializationFailure reports whether err is one of the two transient class
// 40 conditions Postgres resolves by asking the caller to retry the whole
// transaction: deadlock_detected and serialization_failure. Anything else —
// including a constraint violation or a dead connection — is the caller's
// problem and must not be retried.
func isSerializationFailure(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}

	return pgErr.Code == pgerrDeadlockDetected || pgErr.Code == pgerrSerializationFailure
}

// scanKeys collects (area, profile, res, origin, dest) rows into pair keys.
func scanKeys(rows pgx.Rows) ([]beeline.PairKey, error) {
	var keys []beeline.PairKey
	for rows.Next() {
		var (
			area, origin, dest int64
			profile            string
			res                int16
		)
		if err := rows.Scan(&area, &profile, &res, &origin, &dest); err != nil {
			return nil, fmt.Errorf("freshness: scanning key: %w", err)
		}
		keys = append(keys, beeline.PairKey{
			Profile: beeline.Profile(profile),
			Area:    beeline.AreaID(area),
			Origin:  beeline.H3Cell(origin),
			Dest:    beeline.H3Cell(dest),
			Res:     int(res),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("freshness: reading keys: %w", err)
	}

	return keys, nil
}
