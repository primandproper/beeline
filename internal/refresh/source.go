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
// leader over HTTP. Submit is all-or-nothing from the pool's point of view — on
// error the batch is dropped and the claims' leases expire, so the pairs are
// reclaimed and recomputed (wasted work, never corruption).
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
// pool stamps one ComputedAt per batch, so any entry's timestamp is the batch's.
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
