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
	"fmt"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v4/observability/logging"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config tunes the index. TargetTTL is the index-wide freshness default an
// area's override falls back to (the memory index's New parameter).
type Config struct {
	TargetTTL time.Duration
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
	stats  *statsCache
	cfg    Config
}

// New builds the index and starts its access-flush goroutine.
func New(pool *pgxpool.Pool, cfg Config, log logging.Logger) *Index {
	if cfg.AccessFlushInterval <= 0 {
		cfg.AccessFlushInterval = defaultAccessFlushInterval
	}
	if cfg.AccessFlushLimit <= 0 {
		cfg.AccessFlushLimit = defaultAccessFlushLimit
	}

	i := &Index{
		pool:  pool,
		log:   logging.EnsureLogger(log),
		cfg:   cfg,
		stats: newStatsCache(cfg.StatsCacheTTL),
	}
	i.access = newAccessBuffer(i, cfg.AccessFlushInterval, cfg.AccessFlushLimit)

	return i
}

// Close stops the access flusher and drains anything still buffered.
func (i *Index) Close(ctx context.Context) error {
	return i.access.close(ctx)
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
func (i *Index) Bump(ctx context.Context, keys []beeline.PairKey) error {
	if len(keys) == 0 {
		return nil
	}

	areas, profiles, resolutions, origins, dests := keyColumns(keys)
	if _, err := i.pool.Exec(ctx, `
		INSERT INTO pair_freshness AS pf (area_id, profile, res, origin, dest, bumped)
		SELECT area_id, profile, res, origin, dest, TRUE
		FROM unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[])
		     AS k(area_id, profile, res, origin, dest)
		ON CONFLICT (area_id, profile, res, origin, dest) DO UPDATE
		SET bumped = TRUE, last_access = now()`,
		areas, profiles, resolutions, origins, dests); err != nil {
		return fmt.Errorf("freshness: bumping: %w", err)
	}

	return nil
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

// Invalidate re-enqueues every computed pair older than sel.OlderThan by
// clearing its computed_at, so it sorts to the front of the queue.
func (i *Index) Invalidate(ctx context.Context, sel beeline.Selector) error {
	if _, err := i.pool.Exec(ctx, `
		UPDATE pair_freshness SET computed_at = NULL, stale_at = NULL
		WHERE computed_at IS NOT NULL AND computed_at < $1`, sel.OlderThan); err != nil {
		return fmt.Errorf("freshness: invalidating: %w", err)
	}

	return nil
}

// Unseed removes every pair belonging to one area, plus its freshness override
// and throughput baseline. Buffered accesses for the area are discarded so a
// later flush cannot resurrect its pairs.
func (i *Index) Unseed(ctx context.Context, area beeline.AreaID) error {
	i.access.dropArea(area)

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

// SetAreaFreshness records an area's freshness contract (target TTL + claim
// lease; non-positive values fall back to the index-wide defaults) and
// re-derives the denormalized stale_at of its already-computed pairs so the
// new TTL takes effect immediately. Enable-time cadence, so the extra UPDATE
// is rare.
func (i *Index) SetAreaFreshness(ctx context.Context, area beeline.AreaID, targetTTL, lease time.Duration) error {
	tx, err := i.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("freshness: beginning freshness tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	if _, err = tx.Exec(ctx, `
		INSERT INTO area_freshness (area_id, target_ttl_seconds, lease_seconds)
		VALUES ($1, $2, $3)
		ON CONFLICT (area_id) DO UPDATE
		SET target_ttl_seconds = excluded.target_ttl_seconds,
		    lease_seconds      = excluded.lease_seconds`,
		int64(area), targetTTL.Seconds(), lease.Seconds()); err != nil {
		return fmt.Errorf("freshness: setting area freshness: %w", err)
	}

	if _, err = tx.Exec(ctx, `
		UPDATE pair_freshness
		SET stale_at = computed_at + make_interval(secs => COALESCE(NULLIF($2, 0), $3))
		WHERE area_id = $1 AND computed_at IS NOT NULL`,
		int64(area), targetTTL.Seconds(), i.cfg.TargetTTL.Seconds()); err != nil {
		return fmt.Errorf("freshness: rederiving stale_at: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("freshness: committing area freshness: %w", err)
	}

	return nil
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
