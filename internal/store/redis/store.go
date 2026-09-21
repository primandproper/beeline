// Package redis is the Redis beeline.Store: the hot estimate cache option for
// distributed deployments whose batch reads outgrow Postgres (the 300k-key
// sub-second contract). Only the estimate cache lives here — the freshness
// index, operator config, and election stay in Postgres regardless — so losing
// Redis loses cached scalars, never coordination state.
package redis

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"

	"github.com/redis/go-redis/v9"
)

// batchChunk bounds how many keys travel in one pipelined MGET/MSET/DEL.
const batchChunk = 5_000

// scanCount is the COUNT hint for DeleteArea's keyspace scan.
const scanCount = 10_000

// valueSize is the fixed binary value: duration f64 | distance f64 |
// computed-at unix-milli i64, all little-endian.
const valueSize = 24

// Store is the Redis-backed hot estimate store. It also satisfies the control
// plane's AreaStore seam (Delete/DeleteArea). Unlike the Postgres store,
// ComputedAt is client-stamped (Redis has no server clock in the value); the
// freshness index still schedules refresh from the database clock, so skew here
// can only shade the *displayed* staleness on reads, never the refresh order.
type Store struct {
	client *redis.Client
}

// New connects a client and verifies the server is reachable.
func New(ctx context.Context, cfg *config.RedisConfig) (*Store, error) {
	client := redis.NewClient(&redis.Options{
		Addr:     cfg.Addr,
		Password: cfg.Password,
		DB:       cfg.DB,
		PoolSize: cfg.PoolSize,
	})
	if err := client.Ping(ctx).Err(); err != nil {
		return nil, fmt.Errorf("redis: pinging %q: %w", cfg.Addr, err)
	}

	return &Store{client: client}, nil
}

// Close releases the client's connections.
func (s *Store) Close() error { return s.client.Close() }

// Ping reports whether the server is reachable, satisfying
// healthcheck.CacheReadyChecker so the hot store can back /_ops_/ready.
func (s *Store) Ping(ctx context.Context) error { return s.client.Ping(ctx).Err() }

// key renders one pair key. Hex cells keep keys short and the area prefix makes
// DeleteArea's scan pattern (`est:{area}:*`) cheap to express.
func key(k beeline.PairKey) string {
	return fmt.Sprintf("est:%d:%d:%s:%x:%x", k.Area, k.Res, k.Profile, uint64(k.Origin), uint64(k.Dest))
}

func encode(s beeline.Stored) []byte {
	buf := make([]byte, valueSize)
	binary.LittleEndian.PutUint64(buf[0:8], math.Float64bits(s.Duration))
	binary.LittleEndian.PutUint64(buf[8:16], math.Float64bits(s.Distance))
	binary.LittleEndian.PutUint64(buf[16:24], uint64(s.ComputedAt.UnixMilli()))
	return buf
}

func decode(raw string) (beeline.Stored, error) {
	if len(raw) != valueSize {
		return beeline.Stored{}, fmt.Errorf("redis: malformed value of %d bytes", len(raw))
	}
	b := []byte(raw)

	return beeline.Stored{
		ComputedAt: time.UnixMilli(int64(binary.LittleEndian.Uint64(b[16:24]))),
		Duration:   math.Float64frombits(binary.LittleEndian.Uint64(b[0:8])),
		Distance:   math.Float64frombits(binary.LittleEndian.Uint64(b[8:16])),
	}, nil
}

// BatchGet returns one result per key, positionally aligned; nil marks a miss.
// Chunked MGETs ride one pipeline, so a 300k-key read costs one round trip.
func (s *Store) BatchGet(ctx context.Context, keys []beeline.PairKey) ([]*beeline.Stored, error) {
	out := make([]*beeline.Stored, len(keys))
	if len(keys) == 0 {
		return out, nil
	}

	pipe := s.client.Pipeline()
	cmds := make([]*redis.SliceCmd, 0, (len(keys)+batchChunk-1)/batchChunk)
	for start := 0; start < len(keys); start += batchChunk {
		end := min(start+batchChunk, len(keys))
		names := make([]string, end-start)
		for i := start; i < end; i++ {
			names[i-start] = key(keys[i])
		}
		cmds = append(cmds, pipe.MGet(ctx, names...))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, fmt.Errorf("redis: batch get: %w", err)
	}

	for c, cmd := range cmds {
		for i, v := range cmd.Val() {
			if v == nil {
				continue
			}
			raw, ok := v.(string)
			if !ok {
				return nil, fmt.Errorf("redis: unexpected value type %T", v)
			}
			stored, err := decode(raw)
			if err != nil {
				return nil, err
			}
			out[c*batchChunk+i] = &stored
		}
	}

	return out, nil
}

// Put writes each entry, overwriting any prior value for the same key.
func (s *Store) Put(ctx context.Context, entries []beeline.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	pipe := s.client.Pipeline()
	for start := 0; start < len(entries); start += batchChunk {
		end := min(start+batchChunk, len(entries))
		pairs := make([]any, 0, 2*(end-start))
		for i := start; i < end; i++ {
			pairs = append(pairs, key(entries[i].Key), encode(entries[i].Stored))
		}
		pipe.MSet(ctx, pairs...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: put: %w", err)
	}

	return nil
}

// Delete drops the given keys, ignoring any that are absent.
func (s *Store) Delete(ctx context.Context, keys []beeline.PairKey) error {
	if len(keys) == 0 {
		return nil
	}

	pipe := s.client.Pipeline()
	for start := 0; start < len(keys); start += batchChunk {
		end := min(start+batchChunk, len(keys))
		names := make([]string, end-start)
		for i := start; i < end; i++ {
			names[i-start] = key(keys[i])
		}
		pipe.Del(ctx, names...)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis: delete: %w", err)
	}

	return nil
}

// DeleteArea drops one area's estimates by scanning its key prefix. Disable is
// a rare operator event (the same argument the in-memory store makes for its
// full-map scan), so a keyspace walk is acceptable here.
func (s *Store) DeleteArea(ctx context.Context, area beeline.AreaID) error {
	var cursor uint64
	pattern := fmt.Sprintf("est:%d:*", area)
	for {
		names, next, err := s.client.Scan(ctx, cursor, pattern, scanCount).Result()
		if err != nil {
			return fmt.Errorf("redis: scanning area %d: %w", area, err)
		}
		if len(names) > 0 {
			if err = s.client.Del(ctx, names...).Err(); err != nil {
				return fmt.Errorf("redis: deleting area %d: %w", area, err)
			}
		}
		if next == 0 {
			return nil
		}
		cursor = next
	}
}
