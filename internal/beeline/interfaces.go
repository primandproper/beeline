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

// Store is the hot read path: a minimal batch key/value surface so "bring your own
// datastore" is actually true (§5.2). No range queries, no ordering — any KV that
// can batch-get qualifies.
type Store interface {
	// BatchGet returns one result per key, positionally aligned; a nil element
	// marks a miss.
	BatchGet(ctx context.Context, keys []PairKey) ([]*Stored, error)
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
	MarkComputed(ctx context.Context, keys []PairKey, at time.Time) error
	Bump(ctx context.Context, keys []PairKey) error // demand-driven priority
	// Access records that a pair was queried: it adds unknown keys to the working set
	// (unpinned, so they are eligible to decay) and stamps their last-access time,
	// without raising refresh priority the way Bump does. It is the demand-fill and
	// fresh-hit signal that keeps actively-queried pairs alive against the sweep.
	Access(ctx context.Context, keys []PairKey) error
	Invalidate(ctx context.Context, sel Selector) error
	Debt(ctx context.Context) (DebtStats, error)
}
