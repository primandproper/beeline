package redis_test

import (
	"testing"

	"github.com/primandproper/beeline/internal/store/redis/redistest"
	"github.com/primandproper/beeline/internal/store/storebench"
)

// The Redis half of the hot-store benchmark gate (see the postgres sibling).
// Self-provisions a Redis container unless BEELINE_TEST_REDIS_ADDR points at a
// running server; skips only under -short.

func BenchmarkBatchGet300k(b *testing.B) {
	store := redistest.Open(b)
	keys := storebench.Keys(storebench.GateKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}

func BenchmarkBatchGet128(b *testing.B) {
	store := redistest.Open(b)
	keys := storebench.Keys(storebench.HotPathKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}
