package cli

import (
	"context"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/control"
	memindex "github.com/primandproper/beeline/internal/freshness/memory"
	pgfresh "github.com/primandproper/beeline/internal/freshness/postgres"
	memstore "github.com/primandproper/beeline/internal/store/memory"
	pgstore "github.com/primandproper/beeline/internal/store/postgres"
	redisstore "github.com/primandproper/beeline/internal/store/redis"
	areasqlite "github.com/primandproper/beeline/internal/store/sqlite"

	"github.com/jackc/pgx/v5/pgxpool"
)

// statsCacheTTL memoizes the aggregate Debt/CellStates reads per head; the
// console polls at 2Hz, so half a second of staleness is invisible while
// keeping the shared table's scan load constant per head.
const statsCacheTTL = 500 * time.Millisecond

// indexDrainTimeout bounds the access-buffer drain on shutdown.
const indexDrainTimeout = 5 * time.Second

// hotStore is what serve needs from an estimate store: the read path's
// beeline.Store plus the control plane's AreaStore deletes. All three
// implementations (memory, postgres, redis) satisfy it.
type hotStore interface {
	beeline.Store
	control.AreaStore
}

// freshnessIndex is what serve needs from a freshness index: the scheduling
// interface (read path, work endpoints, local pool) plus the control plane's
// per-area seam. Both the memory and postgres indexes satisfy it.
type freshnessIndex interface {
	beeline.FreshnessIndex
	control.AreaIndex
}

// backendHandles is what buildBackend wires up from matrix.backend: the chosen
// estimate store and freshness index, the control-plane repositories (SQLite
// single-node, shared Postgres distributed), the cross-head mutation locker and
// config-generation source (nil outside distributed mode), the shared Postgres
// pool when the mode needs one, and a close that releases everything in the
// right order (index first — its access-buffer drain still writes to the pool).
type backendHandles struct {
	pool         *pgxpool.Pool
	store        hotStore
	index        freshnessIndex
	areas        control.AreasRepository
	providers    control.ProvidersRepository
	locker       control.Locker
	configSource *pgstore.Repository
	// sweepGate elects the single head that runs a janitor tick (nil = no
	// election needed, always run — the single-node mode).
	sweepGate func(ctx context.Context, fn func(ctx context.Context) error) (bool, error)
	close     func()
}

// buildBackend opens the configured backend: the zero-dependency in-memory
// default, or the shared Postgres pool (opened first — migrations run there)
// plus the shared index and whichever hot store is selected. Callers defer
// handles.close().
func (a *application) buildBackend(ctx context.Context, mcfg *config.MatrixConfig) (*backendHandles, error) {
	bcfg := &mcfg.Backend
	handles := &backendHandles{close: func() {}}

	if bcfg.Distributed() || bcfg.EffectiveHotStore() == config.HotStorePostgres {
		pool, err := pgstore.Open(ctx, &bcfg.Postgres)
		if err != nil {
			return nil, err
		}
		handles.pool = pool
		handles.close = pool.Close
	}

	// The index and control plane: shared Postgres in distributed mode (every
	// head and follower drains one queue on the database's clock, and every
	// head converges on one area/provider registry), in-process memory + local
	// SQLite otherwise. In distributed mode the SQLite DatabasePath is ignored.
	if bcfg.Distributed() {
		index := pgfresh.New(handles.pool, pgfresh.Config{
			TargetTTL:     mcfg.TargetTTL,
			StatsCacheTTL: statsCacheTTL,
		}, a.logger)
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
		locker := pgstore.NewAdvisoryLocker(handles.pool)
		handles.locker = locker
		handles.sweepGate = locker.TryJanitorLock
	} else {
		handles.index = memindex.New(mcfg.TargetTTL, nil)

		db, err := areasqlite.Open(mcfg.DatabasePath)
		if err != nil {
			handles.close()
			return nil, err
		}
		repo := areasqlite.NewRepository(db, nil)
		handles.areas = repo
		handles.providers = repo
		closeRest := handles.close
		handles.close = func() {
			if closeErr := db.Close(); closeErr != nil {
				a.log().Error("closing area database", closeErr)
			}
			closeRest()
		}
	}

	switch bcfg.EffectiveHotStore() {
	case config.HotStorePostgres:
		handles.store = pgstore.NewEstimateStore(handles.pool)
	case config.HotStoreRedis:
		store, err := redisstore.New(ctx, &bcfg.Redis)
		if err != nil {
			handles.close()
			return nil, err
		}
		handles.store = store
		closePool := handles.close
		handles.close = func() {
			if closeErr := store.Close(); closeErr != nil {
				a.log().Error("closing redis store", closeErr)
			}
			closePool()
		}
	default:
		handles.store = memstore.New()
	}

	return handles, nil
}
