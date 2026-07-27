package memory_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/store/memory"
	"github.com/primandproper/beeline/internal/store/storetest"
)

// TestConformance runs the shared Store behavioral suite against the in-memory
// store — the double that control, httpapi, query, refresh and follower build
// their fixtures on, so this is what keeps those fixtures honest.
func TestConformance(t *testing.T) {
	t.Parallel()

	storetest.Run(t, func(testing.TB) storetest.Store {
		return memory.New()
	})
}
