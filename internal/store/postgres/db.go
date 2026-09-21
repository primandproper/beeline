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
	"fmt"
	"io/fs"
	"time"

	"github.com/primandproper/beeline/internal/config"

	"github.com/primandproper/primitives-go/v2/database"
	"github.com/primandproper/primitives-go/v2/database/migrate"
	pgclient "github.com/primandproper/primitives-go/v2/database/postgres"
	"github.com/primandproper/primitives-go/v2/observability/logging"
	"github.com/primandproper/primitives-go/v2/observability/metrics"
	"github.com/primandproper/primitives-go/v2/observability/tracing"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Migration lock cadence: the probe is tightened from goose's 5s default so a
// waiting head notices the winner promptly.
const (
	migrationLockProbe     = time.Second
	migrationLockTimeout   = time.Minute
	migrationUnlockTimeout = 30 * time.Second
)

// Open connects the shared database client, fails fast if it is unreachable,
// applies pending migrations, and returns the ready client. Migration runs on
// every open and is idempotent; concurrent heads booting at once serialize on
// goose's session advisory lock, so racing opens are safe.
//
// The returned client exposes the underlying pgx pool through Pool: the store,
// freshness index, and sqlc-generated queries all speak pgx natively, while the
// derived database/sql handle carries migrations and advisory-lock work.
func Open(
	ctx context.Context,
	cfg *config.PostgresConfig,
	logger logging.Logger,
	tracerProvider tracing.Provider,
	metricsProvider metrics.Provider,
) (database.Client, error) {
	client, err := pgclient.NewDatabaseClient(ctx, cfg,
		pgclient.WithLogger(logger),
		pgclient.WithTracerProvider(tracerProvider),
		pgclient.WithMetricsProvider(metricsProvider),
	)
	if err != nil {
		return nil, fmt.Errorf("postgres: building database client: %w", err)
	}

	// NewDatabaseClient does not ping, so boot would otherwise defer the failure
	// to the first query. Fail here instead, as the hand-rolled pool used to.
	// v10's constructor returns the concrete *postgres.Client, so IsReady is a
	// direct call rather than an optional-capability assertion.
	if !client.IsReady(ctx) {
		return nil, fmt.Errorf("postgres: database is not ready: %w", database.ErrDatabaseNotReady)
	}

	if err = applyMigrations(ctx, client, logger, tracerProvider, metricsProvider); err != nil {
		return nil, err
	}

	return client, nil
}

// Pool returns the pgx pool backing a client opened by Open. Every pgx-native
// caller in the package funnels through here rather than type-asserting inline.
func Pool(client database.Client) (*pgxpool.Pool, error) {
	access, ok := client.(pgclient.PgxAccess)
	if !ok {
		return nil, fmt.Errorf("postgres: database client does not expose a pgx pool")
	}

	return access.WritePool(), nil
}

// applyMigrations runs the embedded goose migrations over the client's
// database/sql handle (goose speaks database/sql, the rest of the package
// speaks pgx). The lock key is derived from the connection's search_path:
// deployments (default schema) share one lock, while schema-isolated parallel
// tests migrate concurrently instead of queueing on a global key.
func applyMigrations(
	ctx context.Context,
	client database.Client,
	logger logging.Logger,
	tracerProvider tracing.Provider,
	metricsProvider metrics.Provider,
) error {
	sub, err := fs.Sub(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("postgres: locating migrations: %w", err)
	}

	pool, err := Pool(client)
	if err != nil {
		return err
	}

	raw, ok := client.(database.RawAccess)
	if !ok {
		return fmt.Errorf("postgres: database client does not expose a database/sql handle for migrations")
	}

	migrator, err := migrate.New(client.Dialect(), sub,
		migrate.WithLockKey("beeline-migrations:"+pool.Config().ConnConfig.RuntimeParams["search_path"]),
		migrate.WithLockTimeout(migrationLockProbe, migrationLockTimeout),
		migrate.WithUnlockTimeout(migrationLockProbe, migrationUnlockTimeout),
		migrate.WithLogger(logger),
		migrate.WithTracerProvider(tracerProvider),
		migrate.WithMetricsProvider(metricsProvider),
	)
	if err != nil {
		return fmt.Errorf("postgres: building migrator: %w", err)
	}

	if err = migrator.Migrate(ctx, raw.WriteDB()); err != nil {
		return fmt.Errorf("postgres: applying migrations: %w", err)
	}

	return nil
}
