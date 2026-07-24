package postgres

import (
	"context"
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/errgroup"
)

// batchGetChunk bounds how many keys travel in one unnest query. 300k-key reads
// split into ~15 concurrent chunks, keeping each statement's parse/bind cost and
// result buffer modest while the pool parallelizes across connections.
const batchGetChunk = 20_000

// putChunk bounds one upsert statement; refresh batches are far smaller, so
// this only matters for bulk backfills.
const putChunk = 10_000

// batchConcurrency caps concurrent chunk queries per BatchGet/Put call so one
// giant read cannot monopolize the pool.
const batchConcurrency = 8

// EstimateStore is the Postgres beeline.Store: the hot estimate cache shared by
// every head in distributed mode. It also satisfies the control plane's
// AreaStore seam (Delete/DeleteArea). computed_at is stamped by the database
// clock at Put, keeping all freshness ordering on one clock authority.
type EstimateStore struct {
	pool *pgxpool.Pool
}

// NewEstimateStore returns a store over an opened pool.
func NewEstimateStore(pool *pgxpool.Pool) *EstimateStore {
	return &EstimateStore{pool: pool}
}

// keyColumns splits keys into the five parallel arrays unnest joins against.
func keyColumns(keys []beeline.PairKey) (areas []int64, profiles []string, resolutions []int16, origins, dests []int64) {
	areas = make([]int64, len(keys))
	profiles = make([]string, len(keys))
	resolutions = make([]int16, len(keys))
	origins = make([]int64, len(keys))
	dests = make([]int64, len(keys))
	for i := range keys {
		areas[i] = int64(keys[i].Area)
		profiles[i] = string(keys[i].Profile)
		resolutions[i] = int16(keys[i].Res)
		origins[i] = int64(keys[i].Origin)
		dests[i] = int64(keys[i].Dest)
	}

	return areas, profiles, resolutions, origins, dests
}

// BatchGet returns one result per key, positionally aligned; nil marks a miss.
// Large key sets are chunked and the chunks queried concurrently — each chunk
// writes a disjoint range of the shared result slice, so no locking is needed.
func (s *EstimateStore) BatchGet(ctx context.Context, keys []beeline.PairKey) ([]*beeline.Stored, error) {
	out := make([]*beeline.Stored, len(keys))

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(batchConcurrency)
	for start := 0; start < len(keys); start += batchGetChunk {
		end := min(start+batchGetChunk, len(keys))
		g.Go(func() error {
			return s.batchGetChunk(gctx, keys[start:end], out[start:end])
		})
	}
	if err := g.Wait(); err != nil {
		return nil, fmt.Errorf("postgres: batch get: %w", err)
	}

	return out, nil
}

// batchGetChunk resolves one chunk of keys into its slice of the result. The
// WITH ORDINALITY join hands back each hit tagged with its 1-based position in
// the chunk, which is what keeps the output positionally aligned.
func (s *EstimateStore) batchGetChunk(ctx context.Context, keys []beeline.PairKey, out []*beeline.Stored) error {
	areas, profiles, resolutions, origins, dests := keyColumns(keys)

	rows, err := s.pool.Query(ctx, `
		SELECT k.ord, e.duration_sec, e.distance_meters, e.computed_at
		FROM unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[])
		     WITH ORDINALITY AS k(area_id, profile, res, origin, dest, ord)
		JOIN estimates e USING (area_id, profile, res, origin, dest)`,
		areas, profiles, resolutions, origins, dests)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var ord int64
		var stored beeline.Stored
		if err = rows.Scan(&ord, &stored.Duration, &stored.Distance, &stored.ComputedAt); err != nil {
			return err
		}
		out[ord-1] = &stored
	}

	return rows.Err()
}

// Put upserts each entry. The entries' ComputedAt is deliberately ignored: the
// database stamps now() so every head and follower shares one clock (a
// follower's submit is stamped at receipt today for the same reason).
func (s *EstimateStore) Put(ctx context.Context, entries []beeline.Entry) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(batchConcurrency)
	for start := 0; start < len(entries); start += putChunk {
		end := min(start+putChunk, len(entries))
		g.Go(func() error {
			return s.putChunk(gctx, entries[start:end])
		})
	}
	if err := g.Wait(); err != nil {
		return fmt.Errorf("postgres: put: %w", err)
	}

	return nil
}

func (s *EstimateStore) putChunk(ctx context.Context, entries []beeline.Entry) error {
	keys := make([]beeline.PairKey, len(entries))
	durations := make([]float64, len(entries))
	distances := make([]float64, len(entries))
	for i := range entries {
		keys[i] = entries[i].Key
		durations[i] = entries[i].Duration
		distances[i] = entries[i].Distance
	}
	areas, profiles, resolutions, origins, dests := keyColumns(keys)

	_, err := s.pool.Exec(ctx, `
		INSERT INTO estimates (area_id, profile, res, origin, dest, duration_sec, distance_meters)
		SELECT * FROM unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[],
		                     $6::double precision[], $7::double precision[])
		ON CONFLICT (area_id, profile, res, origin, dest) DO UPDATE SET
		    duration_sec    = excluded.duration_sec,
		    distance_meters = excluded.distance_meters,
		    computed_at     = now()`,
		areas, profiles, resolutions, origins, dests, durations, distances)

	return err
}

// Delete drops the given keys, ignoring any that are absent (the demand-decay
// janitor's half of a sweep).
func (s *EstimateStore) Delete(ctx context.Context, keys []beeline.PairKey) error {
	areas, profiles, resolutions, origins, dests := keyColumns(keys)

	_, err := s.pool.Exec(ctx, `
		DELETE FROM estimates e
		USING unnest($1::bigint[], $2::text[], $3::smallint[], $4::bigint[], $5::bigint[])
		      AS k(area_id, profile, res, origin, dest)
		WHERE (e.area_id, e.profile, e.res, e.origin, e.dest)
		    = (k.area_id, k.profile, k.res, k.origin, k.dest)`,
		areas, profiles, resolutions, origins, dests)
	if err != nil {
		return fmt.Errorf("postgres: delete: %w", err)
	}

	return nil
}

// DeleteArea drops one area's estimates (disable / geometry change) — a single
// primary-key-prefix delete.
func (s *EstimateStore) DeleteArea(ctx context.Context, area beeline.AreaID) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM estimates WHERE area_id = $1`, int64(area)); err != nil {
		return fmt.Errorf("postgres: delete area %d: %w", area, err)
	}

	return nil
}
