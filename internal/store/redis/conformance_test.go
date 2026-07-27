package redis_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/store/redis/redistest"
	"github.com/primandproper/beeline/internal/store/storetest"
)

// TestConformance runs the shared Store behavioral suite against the Redis
// store. Every subtest connects to the same server; isolation comes from the
// suite's per-subtest area IDs, since every Redis key embeds its area.
func TestConformance(t *testing.T) {
	t.Parallel()

	storetest.Run(t, func(tb testing.TB) storetest.Store {
		tb.Helper()

		return redistest.Open(tb)
	})
}
