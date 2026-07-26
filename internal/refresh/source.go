package refresh

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
)

// WorkSource is where a pool gets work and puts results. It is the seam between
// the compute loop (claim → group → table → unpack) and whoever owns the freshness
// index and hot store: a leader wires the pool to its own index/store via
// LocalSource, while a follower's implementation claims from and submits to a
// leader over HTTP.
//
// One claim may produce several Submit calls, each carrying a disjoint slice of
// the batch's results (see Config.SubmitChunk), so an implementation must be safe
// to invoke repeatedly. Each call is all-or-nothing from the pool's point of view:
// on error the pool drops that flush and abandons the rest of the claim, leaving
// those pairs' leases to expire so they are reclaimed and recomputed (wasted work,
// never corruption). Results from earlier flushes stay durable.
type WorkSource interface {
	Claim(ctx context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error)
	Submit(ctx context.Context, entries []beeline.Entry) error
}

// LocalSource adapts an in-process freshness index + store into a WorkSource: the
// leader (and any single-node deployment) computes against its own state.
type LocalSource struct {
	index beeline.FreshnessIndex
	store beeline.Store
}

// NewLocalSource wires a WorkSource over the given index and store.
func NewLocalSource(index beeline.FreshnessIndex, store beeline.Store) *LocalSource {
	return &LocalSource{index: index, store: store}
}

// Claim leases up to limit due pairs from the index.
func (s *LocalSource) Claim(ctx context.Context, limit int, lease time.Duration) ([]beeline.PairKey, error) {
	return s.index.Claim(ctx, limit, lease)
}

// Submit writes the computed entries and marks them fresh. A store failure returns
// before marking, leaving the leases to expire and the pairs to be reclaimed. The
// pool stamps one ComputedAt per claimed batch — not per flush — so any entry's
// timestamp is the batch's, and using entries[0]'s for the whole call is exact.
func (s *LocalSource) Submit(ctx context.Context, entries []beeline.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	if err := s.store.Put(ctx, entries); err != nil {
		return err
	}

	keys := make([]beeline.PairKey, len(entries))
	for pos := range entries {
		keys[pos] = entries[pos].Key
	}

	return s.index.MarkComputed(ctx, keys, entries[0].ComputedAt)
}
