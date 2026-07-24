package postgres

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
)

// statsCache memoizes the aggregate observability reads per head: the console
// polls /_ops_/freshness and /_ops_/cells twice a second, and each poll is an
// aggregate scan of the shared table. A short TTL makes the cost per head
// constant; the staleness is invisible at poll cadence and head-independent
// (every head reads the same table). TTL <= 0 disables caching entirely.
type statsCache struct {
	debt    map[beeline.AreaID]debtMemo
	cells   map[beeline.AreaID]cellsMemo
	debtAll debtMemo
	ttl     time.Duration
	mu      sync.Mutex
	hasAll  bool
}

type debtMemo struct {
	at    time.Time
	stats beeline.DebtStats
}

type cellsMemo struct {
	at     time.Time
	states []beeline.CellState
}

func newStatsCache(ttl time.Duration) *statsCache {
	return &statsCache{
		ttl:   ttl,
		debt:  make(map[beeline.AreaID]debtMemo),
		cells: make(map[beeline.AreaID]cellsMemo),
	}
}

// Debt reports the aggregate freshness contract: working set, stale count
// (never-computed pairs count as debt), p100 age, and required vs achieved
// throughput. Required sums each area's workingSet/TTL; achieved comes from
// the shared global counter, so every head reports the same number.
func (i *Index) Debt(ctx context.Context) (beeline.DebtStats, error) {
	i.stats.mu.Lock()
	if i.stats.hasAll && i.stats.ttl > 0 && time.Since(i.stats.debtAll.at) < i.stats.ttl {
		stats := i.stats.debtAll.stats
		i.stats.mu.Unlock()
		return stats, nil
	}
	i.stats.mu.Unlock()

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

	i.stats.mu.Lock()
	i.stats.debtAll = debtMemo{at: time.Now(), stats: stats}
	i.stats.hasAll = true
	i.stats.mu.Unlock()

	return stats, nil
}

// DebtForArea is Debt scoped to one area, with achieved throughput measured
// from the area's own baseline (reset when it was last seeded), so a freshly
// enabled area's progress reads from zero.
func (i *Index) DebtForArea(ctx context.Context, area beeline.AreaID) (beeline.DebtStats, error) {
	i.stats.mu.Lock()
	if memo, ok := i.stats.debt[area]; ok && i.stats.ttl > 0 && time.Since(memo.at) < i.stats.ttl {
		i.stats.mu.Unlock()
		return memo.stats, nil
	}
	i.stats.mu.Unlock()

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

	i.stats.mu.Lock()
	i.stats.debt[area] = debtMemo{at: time.Now(), stats: stats}
	i.stats.mu.Unlock()

	return stats, nil
}

// CellStatesForArea rolls one area's working set up per origin cell for the
// console's progress map: total outgoing pairs, currently-fresh pairs, and the
// oldest computed age. The result is unordered.
func (i *Index) CellStatesForArea(ctx context.Context, area beeline.AreaID) ([]beeline.CellState, error) {
	i.stats.mu.Lock()
	if memo, ok := i.stats.cells[area]; ok && i.stats.ttl > 0 && time.Since(memo.at) < i.stats.ttl {
		i.stats.mu.Unlock()
		return memo.states, nil
	}
	i.stats.mu.Unlock()

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

	i.stats.mu.Lock()
	i.stats.cells[area] = cellsMemo{at: time.Now(), states: states}
	i.stats.mu.Unlock()

	return states, nil
}
