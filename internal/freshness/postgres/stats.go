package postgres

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v10/cache"
	cachememory "github.com/primandproper/platform-go/v10/cache/memory"
	"github.com/primandproper/platform-go/v10/observability/logging"
	"github.com/primandproper/platform-go/v10/observability/metrics"
	"github.com/primandproper/platform-go/v10/observability/tracing"
)

// statsCache memoizes the aggregate observability reads per head: the console
// polls /_ops_/freshness and /_ops_/cells twice a second, and each poll is an
// aggregate scan of the shared table. A short TTL makes the cost per head
// constant; the staleness is invisible at poll cadence and head-independent
// (every head reads the same table). A nil statsCache disables caching entirely
// — the TTL <= 0 case tests rely on for uncached reads.
//
// Lazy eviction is fine here: the key space is bounded by the enabled-area
// count plus the one aggregate key.
type statsCache struct {
	debt  cache.Cache[beeline.DebtStats]
	cells cache.Cache[[]beeline.CellState]
}

// debtAllKey names the aggregate (all-areas) debt entry; per-area entries are
// keyed by id, which can never collide with it.
const debtAllKey = "all"

// areaKey names one area's entry in either cache.
func areaKey(area beeline.AreaID) string {
	return strconv.FormatInt(int64(area), 10)
}

// newStatsCache builds the memo caches, or returns nil when caching is off.
//
// Both caches read through to the index's own scans, so a miss runs exactly one
// scan no matter how many callers raced into it. That matters more than it
// looks: the memo exists because the console polls twice a second, and without
// read-through every expiry let each concurrent poller start its own aggregate
// scan of the shared table — one expiry, N scans, which is the load the memo
// was added to remove.
func newStatsCache(
	i *Index,
	ttl time.Duration,
	logger logging.Logger,
	tracerProvider tracing.Provider,
	metricsProvider metrics.Provider,
) (*statsCache, error) {
	if ttl <= 0 {
		return nil, nil //nolint:nilnil // a nil cache IS the "caching disabled" value
	}

	opts := []cachememory.Option{
		cachememory.WithLogger(logger),
		cachememory.WithTracerProvider(tracerProvider),
		cachememory.WithMetricsProvider(metricsProvider),
	}

	debt, err := cachememory.NewInMemoryCache[beeline.DebtStats](ttl, append(opts,
		cachememory.WithLoader(func(ctx context.Context, key string) (*beeline.DebtStats, error) {
			return i.loadDebt(ctx, key)
		}))...)
	if err != nil {
		return nil, fmt.Errorf("freshness: building debt stats cache: %w", err)
	}

	cells, err := cachememory.NewInMemoryCache[[]beeline.CellState](ttl, append(opts,
		cachememory.WithLoader(func(ctx context.Context, key string) (*[]beeline.CellState, error) {
			area, parseErr := parseAreaKey(key)
			if parseErr != nil {
				return nil, parseErr
			}

			states, scanErr := i.scanCellStatesForArea(ctx, area)
			if scanErr != nil {
				return nil, &scanError{err: scanErr}
			}

			return &states, nil
		}))...)
	if err != nil {
		return nil, fmt.Errorf("freshness: building cell states cache: %w", err)
	}

	return &statsCache{debt: debt, cells: cells}, nil
}

// scanError carries a scan's own error out through the cache's loader
// wrapper. cache/memory wraps whatever a loader returns with `loading "<key>"`,
// and the key is a memo implementation detail: without this, a database outage
// answered /_ops_/freshness with `loading "all": freshness: reading debt: …`,
// naming an internal cache entry in a body a client reads. unwrapScan recovers
// the error the scan actually produced, whatever the wrapper did.
type scanError struct{ err error }

func (e *scanError) Error() string { return e.err.Error() }
func (e *scanError) Unwrap() error { return e.err }

// unwrapScan strips the loader wrapper from err, leaving the scan's own error.
// Anything that is not a scan error (a context cancellation, a type
// mismatch) is returned untouched.
func unwrapScan(err error) error {
	if failure, ok := errors.AsType[*scanError](err); ok {
		return failure.err
	}

	return err
}

// loadDebt resolves either debt key: the aggregate entry, or one area's.
func (i *Index) loadDebt(ctx context.Context, key string) (*beeline.DebtStats, error) {
	if key == debtAllKey {
		stats, err := i.scanDebt(ctx)
		if err != nil {
			return nil, &scanError{err: err}
		}

		return &stats, nil
	}

	area, err := parseAreaKey(key)
	if err != nil {
		return nil, err
	}

	stats, err := i.scanDebtForArea(ctx, area)
	if err != nil {
		return nil, &scanError{err: err}
	}

	return &stats, nil
}

// parseAreaKey reverses areaKey. A key this cannot parse is a bug in this file,
// not bad input, since nothing outside it ever names an entry.
func parseAreaKey(key string) (beeline.AreaID, error) {
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("freshness: unroutable stats cache key %q: %w", key, err)
	}

	return beeline.AreaID(id), nil
}

// debtEntry serves one debt key through the memo, or runs scan directly when
// the memo is off (TTL <= 0). The loader behind the cache resolves the same key
// to the same scan; scan is taken as an argument so the caller that already
// knows which one it wants does not round-trip through key parsing.
func (c *statsCache) debtEntry(
	ctx context.Context,
	key string,
	scan func(context.Context) (beeline.DebtStats, error),
) (beeline.DebtStats, error) {
	if c == nil {
		return scan(ctx)
	}

	stats, err := c.debt.Get(ctx, key)
	if err != nil {
		return beeline.DebtStats{}, unwrapScan(err)
	}
	if stats == nil {
		return beeline.DebtStats{}, nil
	}

	return *stats, nil
}

// purge drops every memoized aggregate on this head. It is for the operator
// mutations that move the numbers by design — invalidating a layer moves the
// whole working set into debt — where waiting out the TTL would show the
// operator a stale readout of the change they just made. The polling path never
// calls it; the caches are a handful of entries, so flushing both is cheaper
// than reasoning about which keys one selector touched.
//
// Only this head's memo is dropped: another head serves its own cached copy for
// up to its own TTL, which is the same head-independent staleness the memo
// already has.
func (c *statsCache) purge(ctx context.Context) {
	if c == nil {
		return
	}

	_ = c.debt.Flush(ctx)  //nolint:errcheck // memoization is best-effort
	_ = c.cells.Flush(ctx) //nolint:errcheck // memoization is best-effort
}

// Debt reports the aggregate freshness contract: working set, stale count
// (never-computed pairs count as debt), p100 age, and required vs achieved
// throughput. Required sums each area's workingSet/TTL; achieved comes from
// the shared global counter, so every head reports the same number.
func (i *Index) Debt(ctx context.Context) (beeline.DebtStats, error) {
	return i.stats.debtEntry(ctx, debtAllKey, i.scanDebt)
}

// scanDebt is Debt's uncached scan; the memo reads through to it.
func (i *Index) scanDebt(ctx context.Context) (beeline.DebtStats, error) {
	start := time.Now()
	defer func() {
		if elapsed := time.Since(start); elapsed > slowQueryThreshold {
			i.log.WithValues(map[string]any{"elapsed": elapsed.String()}).
				Info("slow debt scan (consider a summary rollup at this working-set size)")
		}
	}()

	var stats beeline.DebtStats
	err := i.pool.QueryRow(ctx, `
		WITH totals AS (
		    SELECT count(*)::int AS working_set,
		           count(*) FILTER (WHERE computed_at IS NULL OR stale_at <= now())::int AS debt,
		           COALESCE(EXTRACT(EPOCH FROM (now() - min(computed_at))), 0)::double precision AS oldest
		    FROM pair_freshness
		), required AS (
		    SELECT COALESCE(SUM(cnt / ttl), 0)::double precision AS required
		    FROM (
		        SELECT count(*)::double precision AS cnt,
		               COALESCE(NULLIF(af.target_ttl_seconds, 0), $1)::double precision AS ttl
		        FROM pair_freshness pf
		        LEFT JOIN area_freshness af USING (area_id)
		        GROUP BY pf.area_id, af.target_ttl_seconds
		    ) per_area
		), achieved AS (
		    SELECT CASE WHEN EXTRACT(EPOCH FROM (now() - started_at)) > 0
		                THEN computed_total / EXTRACT(EPOCH FROM (now() - started_at))
		                ELSE 0 END::double precision AS achieved
		    FROM freshness_counters
		)
		SELECT totals.working_set, totals.debt, totals.oldest, required.required, achieved.achieved
		FROM totals, required, achieved`,
		i.cfg.TargetTTL.Seconds()).
		Scan(&stats.WorkingSet, &stats.Debt, &stats.OldestAgeSeconds, &stats.RequiredThroughput, &stats.AchievedThroughput)
	if err != nil {
		return beeline.DebtStats{}, fmt.Errorf("freshness: reading debt: %w", err)
	}

	return stats, nil
}

// DebtForArea is Debt scoped to one area, with achieved throughput measured
// from the area's own baseline (reset when it was last seeded), so a freshly
// enabled area's progress reads from zero.
func (i *Index) DebtForArea(ctx context.Context, area beeline.AreaID) (beeline.DebtStats, error) {
	return i.stats.debtEntry(ctx, areaKey(area), func(ctx context.Context) (beeline.DebtStats, error) {
		return i.scanDebtForArea(ctx, area)
	})
}

// scanDebtForArea is DebtForArea's uncached scan; the memo reads through to it.
func (i *Index) scanDebtForArea(ctx context.Context, area beeline.AreaID) (beeline.DebtStats, error) {
	var stats beeline.DebtStats
	var ttlSeconds float64
	err := i.pool.QueryRow(ctx, `
		WITH totals AS (
		    SELECT count(*)::int AS working_set,
		           count(*) FILTER (WHERE computed_at IS NULL OR stale_at <= now())::int AS debt,
		           COALESCE(EXTRACT(EPOCH FROM (now() - min(computed_at))), 0)::double precision AS oldest
		    FROM pair_freshness WHERE area_id = $1
		), contract AS (
		    SELECT COALESCE((SELECT NULLIF(target_ttl_seconds, 0) FROM area_freshness
		                     WHERE area_id = $1), $2)::double precision AS ttl
		), achieved AS (
		    SELECT COALESCE((
		        SELECT CASE WHEN EXTRACT(EPOCH FROM (now() - started_at)) > 0
		                    THEN computed_total / EXTRACT(EPOCH FROM (now() - started_at))
		                    ELSE 0 END
		        FROM area_baselines WHERE area_id = $1), 0)::double precision AS achieved
		)
		SELECT totals.working_set, totals.debt, totals.oldest, contract.ttl, achieved.achieved
		FROM totals, contract, achieved`,
		int64(area), i.cfg.TargetTTL.Seconds()).
		Scan(&stats.WorkingSet, &stats.Debt, &stats.OldestAgeSeconds, &ttlSeconds, &stats.AchievedThroughput)
	if err != nil {
		return beeline.DebtStats{}, fmt.Errorf("freshness: reading debt for area %d: %w", area, err)
	}

	if ttlSeconds > 0 {
		stats.RequiredThroughput = float64(stats.WorkingSet) / ttlSeconds
	}

	return stats, nil
}

// CellStatesForArea rolls one area's working set up per origin cell for the
// console's progress map: total outgoing pairs, currently-fresh pairs, and the
// oldest computed age. The result is unordered.
func (i *Index) CellStatesForArea(ctx context.Context, area beeline.AreaID) ([]beeline.CellState, error) {
	if i.stats == nil {
		return i.scanCellStatesForArea(ctx, area)
	}

	states, err := i.stats.cells.Get(ctx, areaKey(area))
	if err != nil {
		return nil, unwrapScan(err)
	}
	if states == nil {
		return nil, nil
	}

	return *states, nil
}

// scanCellStatesForArea is CellStatesForArea's uncached scan; the memo reads
// through to it.
func (i *Index) scanCellStatesForArea(ctx context.Context, area beeline.AreaID) ([]beeline.CellState, error) {
	rows, err := i.pool.Query(ctx, `
		SELECT origin,
		       count(*)::int,
		       count(*) FILTER (WHERE computed_at IS NOT NULL AND stale_at > now())::int,
		       COALESCE(EXTRACT(EPOCH FROM (now() - min(computed_at))), 0)::double precision
		FROM pair_freshness
		WHERE area_id = $1
		GROUP BY origin`, int64(area))
	if err != nil {
		return nil, fmt.Errorf("freshness: reading cell states for area %d: %w", area, err)
	}
	defer rows.Close()

	var states []beeline.CellState
	for rows.Next() {
		var origin int64
		var state beeline.CellState
		if err = rows.Scan(&origin, &state.Total, &state.Fresh, &state.OldestAgeSeconds); err != nil {
			return nil, fmt.Errorf("freshness: scanning cell state: %w", err)
		}
		state.Origin = beeline.H3Cell(origin)
		states = append(states, state)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("freshness: reading cell states: %w", err)
	}

	return states, nil
}
