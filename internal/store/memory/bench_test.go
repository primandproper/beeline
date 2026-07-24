package memory_test

import (
	"testing"

	memory "github.com/primandproper/beeline/internal/store/memory"
	"github.com/primandproper/beeline/internal/store/storebench"
)

// Baseline for the hot-store benchmark gate: what the external candidates
// (postgres, redis) are compared against.

func BenchmarkBatchGet300k(b *testing.B) {
	store := memory.New()
	keys := storebench.Keys(storebench.GateKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}

func BenchmarkBatchGet128(b *testing.B) {
	store := memory.New()
	keys := storebench.Keys(storebench.HotPathKeys)
	storebench.Seed(b, store, keys)
	storebench.Run(b, store, keys)
}
