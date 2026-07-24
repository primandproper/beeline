package postgres_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/store/postgres"
	"github.com/primandproper/beeline/internal/store/postgres/pgtest"
	"github.com/primandproper/beeline/internal/store/storebench"
)

// The benchmark gate: Postgres p95 under 1s for a 300k-key BatchGet keeps the
// distributed deployment single-dependency; otherwise Redis carries the hot
// store. Skips (like every pgtest caller) without BEELINE_TEST_POSTGRES_DSN.

func BenchmarkBatchGet300k(b *testing.B) {
	store := postgres.NewEstimateStore(pgtest.Open(b))
	keys := storebench.Keys(storebench.GateKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}

func BenchmarkBatchGet128(b *testing.B) {
	store := postgres.NewEstimateStore(pgtest.Open(b))
	keys := storebench.Keys(storebench.HotPathKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}
