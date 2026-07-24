// Package pgtest opens isolated Postgres schemas for integration tests. Tests
// are gated on BEELINE_TEST_POSTGRES_DSN (set by scripts/test_integration.sh):
// unset, every caller skips, so `make test` stays Docker-free. Each Open gets
// its own random-named schema in the shared database — migrations, data and
// the goose version table land there — so parallel tests and -race never
// collide, and the schema is dropped on cleanup.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/store/postgres"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

// EnvDSN is the environment variable that carries the test database URL.
const EnvDSN = "BEELINE_TEST_POSTGRES_DSN"

// openTimeout bounds schema creation + migration for one test's setup.
const openTimeout = 30 * time.Second

// Open returns a migrated pool over a fresh, private schema, or skips the test
// when no test database is configured. The DSN must be URL-form
// (postgres://…), because the schema is injected via a search_path query
// parameter.
func Open(tb testing.TB) *pgxpool.Pool {
	tb.Helper()

	dsn := os.Getenv(EnvDSN)
	if dsn == "" {
		tb.Skipf("%s not set; skipping Postgres integration test", EnvDSN)
	}

	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()

	schema := randomSchema(tb)

	admin, err := pgxpool.New(ctx, dsn)
	require.NoError(tb, err, "connecting admin pool")
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema))
	require.NoError(tb, err, "creating test schema")

	pool, err := postgres.Open(ctx, &config.PostgresConfig{URL: withSearchPath(tb, dsn, schema)})
	require.NoError(tb, err, "opening migrated pool")

	tb.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), openTimeout)
		defer dropCancel()
		_, dropErr := admin.Exec(dropCtx, fmt.Sprintf("DROP SCHEMA %s CASCADE", schema))
		require.NoError(tb, dropErr, "dropping test schema")
		admin.Close()
	})

	return pool
}

// randomSchema returns a collision-proof, identifier-safe schema name.
func randomSchema(tb testing.TB) string {
	tb.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(tb, err)

	return "bt_" + hex.EncodeToString(buf)
}

// withSearchPath pins every connection of a pool to the test schema.
func withSearchPath(tb testing.TB, dsn, schema string) string {
	tb.Helper()

	u, err := url.Parse(dsn)
	require.NoError(tb, err, "test DSN must be URL-form (postgres://…)")
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()

	return u.String()
}
