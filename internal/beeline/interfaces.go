package beeline

import (
	"context"
	"time"
)

// RoutingEngine computes travel scalars for a matrix of coordinate pairs. The
// matrix (Table) is the primitive, not point-to-point: the batching win in §6
// depends on it. A real engine (OSRM /table, Valhalla sources_to_targets) slots in
// behind this interface unchanged.
type RoutingEngine interface {
	// Table computes the full cartesian product of Sources × Destinations.
	Table(ctx context.Context, req TableRequest) (TableResponse, error)
	Capabilities() Capabilities
}

// EngineResolver selects the RoutingEngine an area is served by, so different areas
// can route through different providers (an in-process Haversine stand-in for one, a
// real OSRM instance for another). Both the refresh pool and the read path consult it
// per area instead of holding a single shared engine. AreaID 0 — an out-of-area query
// that belongs to no partition — resolves to the default engine.
type EngineResolver interface {
	EngineFor(area AreaID) RoutingEngine
}

// Store is the hot read path: a minimal batch key/value surface so "bring your own
// datastore" is actually true (§5.2). No range queries, no ordering — any KV that
// can batch-get qualifies.
//
// Implementations are pinned to one behavioral contract by
// internal/store/storetest, which every backend runs as a conformance suite.
type Store interface {
	// BatchGet returns one result per key, positionally aligned; a nil element
	// marks a miss. A key repeated in keys is answered at each of its positions.
	BatchGet(ctx context.Context, keys []PairKey) ([]*Stored, error)
	// Put writes each entry, overwriting any prior value for the same key.
	//
	// Entry.ComputedAt is advisory: a backend with its own clock authority
	// (Postgres stamps computed_at = now()) substitutes its own timestamp, so
	// distributed heads never compare process clocks — the same rule
	// FreshnessIndex.MarkComputed follows. Whatever BatchGet returns is
	// authoritative for staleness; callers must not assume they read back the
	// instant they wrote. A store may also lose sub-millisecond precision, the
	// monotonic reading, and the time.Location (Redis packs computed_at into a
	// fixed-width millisecond field), so compare with Time.Equal and tolerances,
	// never for exact identity.
	//
	// The practical consequence, and it is intended: staleness is measured from
	// the write, not from the computation, so a follower's compute-to-Put latency
	// is invisible to the freshness contract.
	Put(ctx context.Context, entries []Entry) error
}

// FreshnessIndex is the scheduling brain (§5.3): it tracks per-pair staleness and
// hands workers the stalest keys under an atomic lease. The prototype backs it with
// an in-memory structure; Postgres (SELECT … FOR UPDATE SKIP LOCKED) or a Redis
// sorted set implement the same contract.
type FreshnessIndex interface {
	// Seed adds keys to the working set (from ingestion/tessellation). Existing
	// keys are left untouched.
	Seed(ctx context.Context, keys []PairKey) error
	// Claim atomically leases up to limit of the stalest due keys with a
	// visibility timeout. At-least-once; writes are idempotent so a double-compute
	// is waste, not corruption.
	Claim(ctx context.Context, limit int, lease time.Duration) ([]PairKey, error)
	// MarkComputed records successful refreshes. The at parameter is advisory:
	// a backend with its own clock authority (Postgres) substitutes its own
	// timestamp, so distributed heads never compare process clocks.
	MarkComputed(ctx context.Context, keys []PairKey, at time.Time) error
	Bump(ctx context.Context, keys []PairKey) error // demand-driven priority
	// Access records that a pair was queried: it adds unknown keys to the working set
	// (unpinned, so they are eligible to decay) and stamps their last-access time,
	// without raising refresh priority the way Bump does. It is the demand-fill and
	// fresh-hit signal that keeps actively-queried pairs alive against the sweep.
	Access(ctx context.Context, keys []PairKey) error
	// Invalidate re-enqueues every computed pair matching sel by forgetting when
	// it was computed, and returns how many pairs it selected. The pairs sort to
	// the front of the queue (ahead of every computed pair, behind live
	// demand bumps) and become immediately claimable — an outstanding lease on a
	// selected pair is released, so the invalidation cannot be swallowed by a
	// worker that claimed the pair moments earlier. That worker's late submit is
	// still accepted, exactly as any other duplicate compute is: at most one
	// batch's worth of pairs can land a pre-invalidation value and wait a further
	// TTL, which is the same at-least-once bargain Claim/MarkComputed already make.
	//
	// Cached estimates are untouched, so invalidating a large layer costs refresh
	// throughput, never read latency. It is invisible to readers: they keep
	// getting the last computed value, and it still reports itself fresh, because
	// the read path judges staleness from the Store's own ComputedAt against the
	// area TTL — not from this schedule. Invalidate moves *when a pair is
	// recomputed*, nothing about what is served in the meantime.
	Invalidate(ctx context.Context, sel Selector) (int, error)
	Debt(ctx context.Context) (DebtStats, error)
}
