package postgres

import (
	"context"
	"fmt"
	"hash/fnv"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Advisory-lock names. Keys are derived by hashing so the namespace can grow
// without coordinating integer ranges; a hash collision merely over-serializes
// two unrelated operations, never corrupts.
const (
	// lockNameJanitor elects the single head that runs a demand-decay sweep tick.
	lockNameJanitor = "beeline:janitor"
	// lockNameBootSeed elects the single head that seeds enabled areas at boot.
	lockNameBootSeed = "beeline:boot-seed"
	// lockNameAreaPrefix scopes one area's lifecycle mutations.
	lockNameAreaPrefix = "beeline:area:"
)

// AdvisoryLocker provides cross-head mutual exclusion over Postgres advisory
// locks. Locks are transaction-scoped (pg_advisory_xact_lock): each call opens
// a throwaway transaction that exists only to hold the lock while fn runs, so
// there is no session bookkeeping and a crashed holder's lock dies with its
// connection. It implements control.Locker.
type AdvisoryLocker struct {
	pool *pgxpool.Pool
}

// NewAdvisoryLocker returns a locker over an opened pool.
func NewAdvisoryLocker(pool *pgxpool.Pool) *AdvisoryLocker {
	return &AdvisoryLocker{pool: pool}
}

// WithAreaLock serializes one area's lifecycle mutations across heads: the
// callback runs while this process holds the area's advisory lock, and any
// other head's mutation of the same area waits.
func (l *AdvisoryLocker) WithAreaLock(ctx context.Context, id beeline.AreaID, fn func(ctx context.Context) error) error {
	return l.withLock(ctx, lockKey(fmt.Sprintf("%s%d", lockNameAreaPrefix, id)), fn)
}

// WithBootSeedLock runs fn while holding the boot-seed election lock, blocking
// until it is acquired. Concurrent booting heads serialize here so exactly one
// runs the index seeding (the others' fn runs after, when seeding is done and
// idempotently skips).
func (l *AdvisoryLocker) WithBootSeedLock(ctx context.Context, fn func(ctx context.Context) error) error {
	return l.withLock(ctx, lockKey(lockNameBootSeed), fn)
}

// TryJanitorLock runs fn only if this head wins the janitor election for the
// moment of the call, reporting whether it ran. Losing is normal — another
// head is sweeping — and costs one round trip.
func (l *AdvisoryLocker) TryJanitorLock(ctx context.Context, fn func(ctx context.Context) error) (bool, error) {
	return l.tryLock(ctx, lockKey(lockNameJanitor), fn)
}

// withLock opens a transaction, takes the lock (blocking), runs fn, and
// releases by closing the transaction. fn's own database work runs in its own
// transactions; the outer one exists purely to scope the lock.
func (l *AdvisoryLocker) withLock(ctx context.Context, key int64, fn func(ctx context.Context) error) error {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("postgres: beginning lock tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
		return fmt.Errorf("postgres: acquiring advisory lock: %w", err)
	}

	if err = fn(ctx); err != nil {
		return err
	}

	if err = tx.Commit(ctx); err != nil {
		return fmt.Errorf("postgres: releasing advisory lock: %w", err)
	}

	return nil
}

// tryLock is withLock's non-blocking variant.
func (l *AdvisoryLocker) tryLock(ctx context.Context, key int64, fn func(ctx context.Context) error) (bool, error) {
	tx, err := l.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("postgres: beginning lock tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() //nolint:errcheck // rollback after commit is a no-op

	var won bool
	if err = tx.QueryRow(ctx, `SELECT pg_try_advisory_xact_lock($1)`, key).Scan(&won); err != nil {
		return false, fmt.Errorf("postgres: trying advisory lock: %w", err)
	}
	if !won {
		return false, nil
	}

	if err = fn(ctx); err != nil {
		return true, err
	}

	if err = tx.Commit(ctx); err != nil {
		return true, fmt.Errorf("postgres: releasing advisory lock: %w", err)
	}

	return true, nil
}

// lockKey hashes a lock name into the advisory-lock keyspace.
func lockKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))

	return int64(h.Sum64())
}
