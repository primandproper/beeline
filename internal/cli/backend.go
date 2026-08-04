package cli

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/control"
	pgfresh "github.com/primandproper/beeline/internal/freshness/postgres"
	pgstore "github.com/primandproper/beeline/internal/store/postgres"
	redisstore "github.com/primandproper/beeline/internal/store/redis"

	"github.com/primandproper/platform-go/v9/database"
	"github.com/primandproper/platform-go/v9/healthcheck"

	"github.com/jackc/pgx/v5/pgxpool"
)

// statsCacheTTL memoizes the aggregate Debt/CellStates reads per head; the
// console polls at 2Hz, so half a second of staleness is invisible while
// keeping the shared table's scan load constant per head.
const statsCacheTTL = 500 * time.Millisecond

// indexDrainTimeout bounds the access-buffer drain on shutdown.
const indexDrainTimeout = 5 * time.Second

// hotStore is what serve needs from an estimate store: the read path's
// beeline.Store plus the control plane's AreaStore deletes. This stays
// polymorphic because the hot store genuinely is — Postgres by default, Redis
// for batch-read headroom.
type hotStore interface {
	beeline.Store
	control.AreaStore
}

// backendHandles is what buildBackend wires up: the chosen estimate store, the
// shared freshness index, the control-plane repositories, the cross-head
// mutation locker and config-generation source, the shared Postgres pool, and a
// close that releases everything in the right order (index first — its
// access-buffer drain still writes to the pool).
//
// Every field is always populated. There is one backend, so there are no
// mode-dependent nils to guard against.
type backendHandles struct {
	// db is the platform database client; pool is the pgx pool behind it, which
	// every pgx-native package (store, index, sqlc) uses directly.
	db           database.Client
	pool         *pgxpool.Pool
	store        hotStore
	index        *pgfresh.Index
	areas        control.AreasRepository
	providers    control.ProvidersRepository
	locker       control.Locker
	configSource *pgstore.Repository
	// sweepGate elects the single head that runs a janitor tick.
	sweepGate func(ctx context.Context, fn func(ctx context.Context) error) (bool, error)
	// health carries the readiness checkers for whatever this backend actually
	// depends on, so /_ops_/ready reports the dependencies rather than a constant.
	health healthcheck.Registry
	close  func()
}

// buildBackend opens the shared Postgres pool (first — migrations run there),
// then the shared freshness index, control-plane repositories and advisory
// locker, and whichever hot store is selected. Callers defer handles.close().
func (a *application) buildBackend(ctx context.Context, mcfg *config.MatrixConfig) (*backendHandles, error) {
	bcfg := &mcfg.Backend
	handles := &backendHandles{close: func() {}, health: healthcheck.NewRegistry()}

	db, err := pgstore.Open(ctx, &bcfg.Postgres, a.logger, a.pillars.TracerProvider, a.pillars.MetricsProvider)
	if err != nil {
		return nil, err
	}

	pool, err := pgstore.Pool(db)
	if err != nil {
		_ = db.Close() //nolint:errcheck // already failing; the close error adds nothing
		return nil, err
	}

	handles.db = db
	handles.pool = pool
	if ready, ok := db.(healthcheck.DatabaseReadyChecker); ok {
		handles.health.Register(healthcheck.NewDatabaseChecker("postgres", ready))
	}
	handles.close = func() {
		if closeErr := db.Close(); closeErr != nil {
			a.log().Error("closing postgres client", closeErr)
		}
	}

	// The index and control plane both live in the shared database: every head
	// and follower drains one queue on the database's clock, and every head
	// converges on one area/provider registry.
	index, err := pgfresh.New(handles.pool, &pgfresh.Config{
		TargetTTL:       mcfg.TargetTTL,
		StatsCacheTTL:   statsCacheTTL,
		Clock:           a.clock,
		TracerProvider:  a.pillars.TracerProvider,
		MetricsProvider: a.pillars.MetricsProvider,
	}, a.logger)
	if err != nil {
		handles.close()
		return nil, err
	}
	closePool := handles.close
	handles.close = func() {
		drainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), indexDrainTimeout)
		defer cancel()
		if drainErr := index.Close(drainCtx); drainErr != nil {
			a.log().Error("draining freshness access buffer", drainErr)
		}
		closePool()
	}
	handles.index = index

	repo := pgstore.NewRepository(handles.pool, nil)
	handles.areas = repo
	handles.providers = repo
	handles.configSource = repo

	locker, err := pgstore.NewAdvisoryLocker(handles.db, a.logger, a.pillars.TracerProvider, a.pillars.MetricsProvider)
	if err != nil {
		handles.close()
		return nil, err
	}
	handles.locker = locker
	handles.sweepGate = locker.TryJanitorLock

	if err = a.attachHotStore(ctx, bcfg, handles); err != nil {
		return nil, err
	}

	return handles, nil
}

// attachHotStore selects the estimate cache: the shared Postgres tables by
// default (benchmarked well inside the sub-second read contract), or Redis when
// batch-read headroom is worth the extra dependency.
func (a *application) attachHotStore(ctx context.Context, bcfg *config.BackendConfig, handles *backendHandles) error {
	if bcfg.EffectiveHotStore() == config.HotStoreRedis {
		store, err := redisstore.New(ctx, &bcfg.Redis)
		if err != nil {
			handles.close()
			return err
		}
		handles.store = store
		handles.health.Register(healthcheck.NewCacheChecker("redis", store))
		closeRest := handles.close
		handles.close = func() {
			if closeErr := store.Close(); closeErr != nil {
				a.log().Error("closing redis store", closeErr)
			}
			closeRest()
		}

		return nil
	}

	handles.store = pgstore.NewEstimateStore(handles.pool)

	return nil
}
