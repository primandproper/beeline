// Package postgres is beeline's shared coordination backend: the hot estimate
// store in this file's pool, plus (in later phases) the freshness index,
// operator config, and singleton-job election that let any number of identical
// `serve` heads run over one database. It is the production counterpart of the
// in-memory prototypes — distributed mode is opt-in via matrix.backend, and the
// zero-dependency single-node path never imports a running Postgres.
package postgres

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"

	"github.com/primandproper/beeline/internal/config"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/pressly/goose/v3/lock"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockID hashes the connection's schema into the advisory lock ID
// migrations serialize on (empty search_path = the default schema).
func migrationLockID(searchPath string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("beeline-migrations:" + searchPath))

	return int64(h.Sum64())
}

// Open connects a pgx pool to the configured database, applies pending
// migrations, and returns the ready pool. Migration runs on every open and is
// idempotent; concurrent heads booting at once serialize on goose's session
// advisory lock, so racing opens are safe.
func Open(ctx context.Context, cfg *config.PostgresConfig) (*pgxpool.Pool, error) {
	pc, err := pgxpool.ParseConfig(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("postgres: parsing URL: %w", err)
	}
	if cfg.MaxConns > 0 {
		pc.MaxConns = cfg.MaxConns
	}
	if cfg.MinConns > 0 {
		pc.MinConns = cfg.MinConns
	}

	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, fmt.Errorf("postgres: building pool: %w", err)
	}
	if err = pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: pinging %q: %w", pc.ConnConfig.Host, err)
	}
	if err = migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, err
	}

	return pool, nil
}

// migrate applies the embedded goose migrations through a temporary
// database/sql handle over the pool's config (goose speaks database/sql, the
// rest of the package speaks pgx). Instance-based Provider for the same reason
// as the sqlite package: no shared goose state across parallel tests.
func migrate(ctx context.Context, pool *pgxpool.Pool) (err error) {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("postgres: locating migrations: %w", err)
	}

	// The handle only borrows the pool's connections; closing it releases them
	// without touching the pool itself.
	db := stdlib.OpenDBFromPool(pool)
	defer func() {
		err = errors.Join(err, db.Close())
	}()

	// A session-level advisory lock serializes concurrent heads booting against
	// one database, so racing migrations wait instead of erroring. The lock ID
	// is derived from the connection's search_path: deployments (default
	// schema) share one lock, while schema-isolated parallel tests migrate
	// concurrently instead of queueing on a global ID. The probe is tightened
	// from goose's 5s default so a waiting head notices the winner promptly.
	locker, err := lock.NewPostgresSessionLocker(
		lock.WithLockID(migrationLockID(pool.Config().ConnConfig.RuntimeParams["search_path"])),
		lock.WithLockTimeout(1, 60),   // probe every second, give up after a minute
		lock.WithUnlockTimeout(1, 30), //nolint:mnd // same cadence for release
	)
	if err != nil {
		return fmt.Errorf("postgres: building migration locker: %w", err)
	}

	provider, err := goose.NewProvider(goose.DialectPostgres, db, sub, goose.WithSessionLocker(locker))
	if err != nil {
		return fmt.Errorf("postgres: building migration provider: %w", err)
	}
	if _, err = provider.Up(ctx); err != nil {
		return fmt.Errorf("postgres: applying migrations: %w", err)
	}

	return nil
}
