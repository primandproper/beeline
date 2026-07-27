package memory_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/control/repositorytest"
	"github.com/primandproper/beeline/internal/store/memory"
)

// TestRepositoryConformance runs the shared control-plane repository suite — the
// same one the SQLite and Postgres repositories run — against the in-memory
// fake. This is the only thing that makes a hand-written fake trustworthy: it is
// the double that control and httpapi build their fixtures on, so a deviation
// here silently weakens every test above it.
func TestRepositoryConformance(t *testing.T) {
	t.Parallel()

	repositorytest.Run(t, func(testing.TB) repositorytest.Repository {
		return memory.NewRepository(nil)
	})
}
