package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	memory "github.com/primandproper/beeline/internal/freshness/memory"
)

// BenchmarkClaimLargeColdIndex models the demo-cluster shape: a ~1M-pair cold
// working set with every pair due, claimed in refresh-batch-sized chunks. Claim
// holds the index mutex, so its latency here is the whole pool's serialization
// cost per batch.
func BenchmarkClaimLargeColdIndex(b *testing.B) {
	ctx := context.Background()
	idx := memory.New(time.Hour, nil)

	keys := make([]beeline.PairKey, 0, 1_000_000)
	for origin := range 2000 {
		for dest := range 500 {
			keys = append(keys, beeline.PairKey{
				Origin:  beeline.H3Cell(origin),
				Dest:    beeline.H3Cell(dest),
				Profile: "car",
				Res:     9,
			})
		}
	}
	if err := idx.Seed(ctx, keys); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		claimed, err := idx.Claim(ctx, 256, time.Nanosecond) // lease expires immediately: every iteration sees the full due set
		if err != nil || len(claimed) != 256 {
			b.Fatalf("claimed %d, err %v", len(claimed), err)
		}
	}
}
