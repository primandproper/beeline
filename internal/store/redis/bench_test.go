package redis_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/store/storebench"
)

// The Redis half of the hot-store benchmark gate (see the postgres sibling).
// Skips without BEELINE_TEST_REDIS_ADDR.

func BenchmarkBatchGet300k(b *testing.B) {
	store := open(b)
	keys := storebench.Keys(storebench.GateKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}

func BenchmarkBatchGet128(b *testing.B) {
	store := open(b)
	keys := storebench.Keys(storebench.HotPathKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}
