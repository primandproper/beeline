package postgres

import (
	"context"
	"fmt"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/primitives-go/v2/database"
	"github.com/primandproper/primitives-go/v2/distributedlock"
	dlpostgres "github.com/primandproper/primitives-go/v2/distributedlock/postgres"
	"github.com/primandproper/primitives-go/v2/observability/logging"
	"github.com/primandproper/primitives-go/v2/observability/metrics"
	"github.com/primandproper/primitives-go/v2/observability/tracing"
)

// Advisory-lock names. The platform locker hashes them into the advisory-lock
// keyspace, so the namespace can grow without coordinating integer ranges; a
// hash collision merely over-serializes two unrelated operations, never
// corrupts.
const (
	// lockNameJanitor elects the single head that runs a demand-decay sweep tick.
	lockNameJanitor = "beeline:janitor"
	// lockNameBootSeed elects the single head that seeds enabled areas at boot.
	lockNameBootSeed = "beeline:boot-seed"
	// lockNameAreaPrefix scopes one area's lifecycle mutations.
	lockNameAreaPrefix = "beeline:area:"
)

// AdvisoryLocker names beeline's cross-head mutual-exclusion points on top of a
// platform scoped locker. Locks are transaction-scoped
// (pg_advisory_xact_lock): each call opens a throwaway transaction that exists
// only to hold the lock while fn runs, so there is no session bookkeeping and a
// crashed holder's lock dies with its connection. fn's own database work runs
// on its own connections, outside the lock-holding transaction. It implements
// control.Locker.
type AdvisoryLocker struct {
	scoped distributedlock.ScopedLocker
}

// NewAdvisoryLocker returns a locker over an opened database client.
func NewAdvisoryLocker(
	db database.Client,
	logger logging.Logger,
	tracerProvider tracing.Provider,
	metricsProvider metrics.Provider,
) (*AdvisoryLocker, error) {
	scoped, err := dlpostgres.NewPostgresScopedLocker(
		&dlpostgres.Config{},
		db,
		nil, // no breaker: losing the database is already fatal to every caller here
		dlpostgres.WithLogger(logger),
		dlpostgres.WithTracerProvider(tracerProvider),
		dlpostgres.WithMetricsProvider(metricsProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: building advisory locker: %w", err)
	}

	return &AdvisoryLocker{scoped: scoped}, nil
}

// WithAreaLock serializes one area's lifecycle mutations across heads: the
// callback runs while this process holds the area's advisory lock, and any
// other head's mutation of the same area waits.
func (l *AdvisoryLocker) WithAreaLock(ctx context.Context, id beeline.AreaID, fn func(ctx context.Context) error) error {
	return l.scoped.WithLock(ctx, fmt.Sprintf("%s%d", lockNameAreaPrefix, id), fn)
}

// WithBootSeedLock runs fn while holding the boot-seed election lock, blocking
// until it is acquired. Concurrent booting heads serialize here so exactly one
// runs the index seeding (the others' fn runs after, when seeding is done and
// idempotently skips).
func (l *AdvisoryLocker) WithBootSeedLock(ctx context.Context, fn func(ctx context.Context) error) error {
	return l.scoped.WithLock(ctx, lockNameBootSeed, fn)
}

// TryJanitorLock runs fn only if this head wins the janitor election for the
// moment of the call, reporting whether it ran. Losing is normal — another
// head is sweeping — and costs one round trip.
func (l *AdvisoryLocker) TryJanitorLock(ctx context.Context, fn func(ctx context.Context) error) (bool, error) {
	return l.scoped.TryWithLock(ctx, lockNameJanitor, fn)
}
