package memory_test

import (
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/freshness/freshnesstest"
	memory "github.com/primandproper/beeline/internal/freshness/memory"
)

// TestConformance runs the shared FreshnessIndex behavioral suite against the
// in-memory index on the real clock (the same suite the Postgres index runs,
// so the two backends cannot drift).
func TestConformance(t *testing.T) {
	t.Parallel()

	freshnesstest.Run(t, func(_ testing.TB, targetTTL time.Duration) freshnesstest.Index {
		return memory.New(targetTTL, nil)
	})
}
