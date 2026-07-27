package sqlite_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/control/repositorytest"
)

// TestConformance runs the shared control-plane repository suite against the
// SQLite repository. A fresh :memory: database per subtest gives the empty
// repository the suite requires.
func TestConformance(t *testing.T) {
	t.Parallel()

	repositorytest.Run(t, func(tb testing.TB) repositorytest.Repository {
		tb.Helper()

		return newRepo(tb)
	})
}
