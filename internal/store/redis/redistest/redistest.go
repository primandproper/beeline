// Package redistest opens Redis stores for integration tests, mirroring
// pgtest's resolution order exactly:
//
//  1. BEELINE_TEST_REDIS_ADDR is set — use it.
//  2. `go test -short` — skip, explicitly.
//  3. Otherwise start a container, once per test binary. A missing Docker daemon
//     is a hard failure here, not a skip.
//
// See pgtest's package doc for why the default is to run rather than to skip.
//
// The container itself comes from primitives-go's
// testutils/containers/redistest, which already owns the image, the wait
// strategy and the startup retry policy; this package adds only beeline's
// resolution order and the config-shaped constructor. Note that it calls Try
// rather than Start: Start registers Terminate as a tb.Cleanup, which would
// bind the container's lifetime to a single test, and we want one per binary.
//
// Isolation between parallel tests is by random area ID, not by database — every
// key embeds its area, so tests only ever touch their own areas' keys and a
// shared server needs no partitioning.
package redistest

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/config"
	redisstore "github.com/primandproper/beeline/internal/store/redis"

	platformredistest "github.com/primandproper/primitives-go/v2/testutils/containers/redistest"

	"github.com/stretchr/testify/require"
)

// EnvAddr is the environment variable that carries the test Redis address. Set
// it to reuse an already-running server instead of starting a container.
const EnvAddr = "BEELINE_TEST_REDIS_ADDR"

// shared is the one Redis this test binary uses. Started at most once, never
// explicitly terminated — Ryuk reaps it at process exit. See pgtest for the
// tradeoff, and for why the outcome is held here rather than returned.
var shared struct {
	err  error
	addr string
	once sync.Once
}

// Open connects a Store to the test Redis, resolving the server per the
// precedence documented on the package.
func Open(tb testing.TB) *redisstore.Store {
	tb.Helper()

	store, err := redisstore.New(context.Background(), &config.RedisConfig{Addr: Addr(tb)})
	require.NoError(tb, err, "connecting the test Redis store")
	tb.Cleanup(func() { require.NoError(tb, store.Close(), "closing the test Redis store") })

	return store
}

// Addr resolves the Redis this test binary uses, for callers that want the
// address rather than a Store.
func Addr(tb testing.TB) string {
	tb.Helper()

	if addr := os.Getenv(EnvAddr); addr != "" {
		return addr
	}

	if testing.Short() {
		tb.Skipf("-short set and %s unset; skipping Redis integration test", EnvAddr)
	}

	shared.once.Do(func() { startContainer(tb) })
	require.NoErrorf(tb, shared.err,
		"starting the test Redis container; set %s to point at a running server, or use -short to skip", EnvAddr)

	return shared.addr
}

// startContainer brings up the shared Redis, recording its outcome in shared
// rather than failing a test directly.
func startContainer(tb testing.TB) {
	tb.Helper()

	ctr, shutdown, err := platformredistest.Try(context.Background())
	if err != nil {
		shared.err = fmt.Errorf("running redis container: %w", err)

		return
	}
	// Try's shutdown is deliberately dropped: like pgtest, the container
	// outlives any single test and Ryuk reaps it at process exit.
	_ = shutdown

	shared.addr = platformredistest.Address(tb, ctr)
}

// RandomArea returns a random positive area ID so parallel tests sharing one
// Redis never collide.
func RandomArea(tb testing.TB) beeline.AreaID {
	tb.Helper()

	buf := make([]byte, 8)
	_, err := rand.Read(buf)
	require.NoError(tb, err)

	return beeline.AreaID(binary.LittleEndian.Uint64(buf) >> 1)
}
