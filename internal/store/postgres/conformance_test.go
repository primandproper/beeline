package postgres_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/control/repositorytest"
	"github.com/primandproper/beeline/internal/store/postgres"
	"github.com/primandproper/beeline/internal/store/postgres/pgtest"
	"github.com/primandproper/beeline/internal/store/storetest"
)

// TestConformance runs the shared Store behavioral suite against the Postgres
// estimate store. Each subtest gets its own schema, which is stricter isolation
// than the suite requires (it already keys every subtest to its own area).
func TestConformance(t *testing.T) {
	t.Parallel()

	storetest.Run(t, func(tb testing.TB) storetest.Store {
		tb.Helper()

		return postgres.NewEstimateStore(pgtest.Open(tb))
	})
}

// TestRepositoryConformance runs the shared control-plane repository suite — the
// same one the SQLite repository runs — against the Postgres repository. The
// schema-per-subtest isolation is what supplies the empty repository the suite
// requires.
func TestRepositoryConformance(t *testing.T) {
	t.Parallel()

	repositorytest.Run(t, func(tb testing.TB) repositorytest.Repository {
		tb.Helper()

		return postgres.NewRepository(pgtest.Open(tb), nil)
	})
}
