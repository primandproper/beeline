// Package pgtest opens isolated Postgres schemas for integration tests.
//
// The database is resolved in this precedence:
//
//  1. BEELINE_TEST_POSTGRES_DSN is set — use it. One shared server for the whole
//     run, which is what scripts/test.sh arranges, and what keeps a full suite
//     to a single container instead of one per package binary.
//  2. `go test -short` — skip, explicitly. This is the Docker-free fast loop
//     (`make test-short`).
//  3. Otherwise start a container, once per test binary. A missing Docker daemon
//     is a hard failure here, not a skip.
//
// Never skip silently. A green run that quietly skipped the only backend
// distributed mode has is worse than a run that fails for want of Docker: that
// is precisely how the Postgres backend once reached zero CI coverage. This is a
// deliberate departure from primitives-go's RUN_CONTAINER_TESTS convention
// (testutils/containers), which defaults to skipping because its consumers may
// have no Docker daemon; beeline is an application whose distributed mode *is*
// Postgres, so the default here is to actually exercise it.
//
// Each Open gets its own random-named schema in the shared database —
// migrations, data and the goose version table land there — so parallel tests
// and -race never collide, and the schema is dropped on cleanup. postgres.Open
// serializes migrations on a *schema-scoped* lock key, so distinct schemas never
// contend with each other.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/config"
	"github.com/primandproper/beeline/internal/store/postgres"

	"github.com/primandproper/primitives-go/v2/database"
	"github.com/primandproper/primitives-go/v2/testutils/containers"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// EnvDSN is the environment variable that carries the test database URL. Set it
// to reuse an already-running server instead of starting a container.
const EnvDSN = "BEELINE_TEST_POSTGRES_DSN"

// openTimeout bounds schema creation + migration for one test's setup.
const openTimeout = 30 * time.Second

// Container knobs. The image matches the one the demo compose cluster runs, so
// tests and `make demo` exercise the same server version.
const (
	containerImage        = "postgres:16-alpine"
	containerStartTimeout = 3 * time.Minute
	// containerMaxConnections lifts Postgres' default 100. A full run opens
	// dozens of schemas concurrently against this one server, each holding a
	// client pool plus an admin pool, and "too many clients already" is the
	// failure mode that shows up as CI flake rather than as a clear error.
	containerMaxConnections = 200
)

// Pool bounds per test. Both are deliberately small for the same reason
// containerMaxConnections is large: the ceiling is shared by every test in the
// run, not owned by one.
const (
	clientMaxConns = 4
	adminMaxConns  = 2
)

// shared is the one Postgres this test binary uses. It is started at most once
// and never explicitly terminated: testcontainers' Ryuk reaper removes it when
// the process exits, which — unlike a shell trap — survives a panic or a killed
// test run. The tradeoff is that teardown is asynchronous and invisible in test
// output. Set TESTCONTAINERS_RYUK_DISABLED=true to opt out, at the cost of
// leaked containers.
//
// The outcome is held here rather than returned so that every caller reports the
// same failure, not just whichever one lost the race to start it.
var shared struct {
	err  error
	dsn  string
	once sync.Once
}

// Open returns a migrated pool over a fresh, private schema. The DSN must be
// URL-form (postgres://…), because the schema is injected via a search_path
// query parameter.
func Open(tb testing.TB) *pgxpool.Pool {
	tb.Helper()

	pool, err := postgres.Pool(OpenClient(tb))
	require.NoError(tb, err, "reaching the pgx pool behind the test client")

	return pool
}

// OpenClient is Open for callers that need the platform database client itself
// (the advisory locker, database health checks) rather than the pgx pool.
func OpenClient(tb testing.TB) database.Client {
	tb.Helper()

	dsn := baseDSN(tb)

	ctx, cancel := context.WithTimeout(context.Background(), openTimeout)
	defer cancel()

	schema := randomSchema(tb)

	admin, err := pgxpool.New(ctx, withParams(tb, dsn, "pool_max_conns", strconv.Itoa(adminMaxConns)))
	require.NoError(tb, err, "connecting admin pool")
	_, err = admin.Exec(ctx, fmt.Sprintf("CREATE SCHEMA %s", schema))
	require.NoError(tb, err, "creating test schema")

	client, err := postgres.Open(ctx, &config.PostgresConfig{
		URL:      withParams(tb, dsn, "search_path", schema),
		MaxConns: clientMaxConns,
	}, nil, nil, nil)
	require.NoError(tb, err, "opening migrated client")

	tb.Cleanup(func() {
		require.NoError(tb, client.Close(), "closing test client")
		dropCtx, dropCancel := context.WithTimeout(context.Background(), openTimeout)
		defer dropCancel()
		_, dropErr := admin.Exec(dropCtx, fmt.Sprintf("DROP SCHEMA %s CASCADE", schema))
		require.NoError(tb, dropErr, "dropping test schema")
		admin.Close()
	})

	return client
}

// baseDSN resolves the server every schema in this binary is carved out of, per
// the precedence documented on the package.
func baseDSN(tb testing.TB) string {
	tb.Helper()

	if dsn := os.Getenv(EnvDSN); dsn != "" {
		return dsn
	}

	if testing.Short() {
		tb.Skipf("-short set and %s unset; skipping Postgres integration test", EnvDSN)
	}

	shared.once.Do(startContainer)
	require.NoErrorf(tb, shared.err,
		"starting the test Postgres container; set %s to point at a running server, or use -short to skip", EnvDSN)

	return shared.dsn
}

// startContainer brings up the shared Postgres, recording its outcome in shared
// rather than failing a test directly.
func startContainer() {
	ctx, cancel := context.WithTimeout(context.Background(), containerStartTimeout)
	defer cancel()

	container, err := containers.StartWithRetry(ctx, func(ctx context.Context) (*postgrescontainer.PostgresContainer, error) {
		return postgrescontainer.Run(ctx, containerImage,
			// Appends to the module's own `postgres -c fsync=off`.
			testcontainers.WithCmdArgs("-c", "max_connections="+strconv.Itoa(containerMaxConnections)),
			testcontainers.WithWaitStrategyAndDeadline(containerStartTimeout,
				// Postgres restarts itself once after first boot, so readiness
				// is logged twice and the first occurrence is a false start.
				wait.ForLog("database system is ready to accept connections").WithOccurrence(2),
				// Required on macOS and Windows, where Docker serves the
				// published port through a separate proxy.
				wait.ForListeningPort("5432/tcp"),
			),
		)
	})
	if err != nil {
		shared.err = fmt.Errorf("running postgres container: %w", err)

		return
	}

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		shared.err = fmt.Errorf("resolving container connection string: %w", err)

		return
	}

	shared.dsn = dsn
}

// randomSchema returns a collision-proof, identifier-safe schema name.
func randomSchema(tb testing.TB) string {
	tb.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(tb, err)

	return "bt_" + hex.EncodeToString(buf)
}

// withParams sets one query parameter on a DSN — the search_path that pins a
// pool to its test schema, or a pool bound.
func withParams(tb testing.TB, dsn, key, value string) string {
	tb.Helper()

	u, err := url.Parse(dsn)
	require.NoError(tb, err, "test DSN must be URL-form (postgres://…)")
	q := u.Query()
	q.Set(key, value)
	u.RawQuery = q.Encode()

	return u.String()
}
