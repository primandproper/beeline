// Package storebench is the shared harness for the hot-store benchmark gate:
// the same seeded working set and BatchGet loop run against every beeline.Store
// implementation, so the numbers that pick the blessed distributed-mode default
// (design decision: 300k+ estimates readable in under a second) are directly
// comparable. Run via `make bench-store`.
package storebench

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/stretchr/testify/require"
)

// GateKeys is the batch size the sub-second contract is measured at.
const GateKeys = 300_000

// HotPathKeys sizes the small mixed read modeling a busy /table request.
const HotPathKeys = 128

// benchArea keeps benchmark rows apart from any test data sharing the backend.
const benchArea beeline.AreaID = 424242

// Keys returns n distinct pair keys in the benchmark area.
func Keys(n int) []beeline.PairKey {
	keys := make([]beeline.PairKey, n)
	for i := range n {
		keys[i] = beeline.PairKey{
			Profile: "car",
			Area:    benchArea,
			Origin:  beeline.H3Cell(i + 1),
			Dest:    beeline.H3Cell(i + 2),
			Res:     8,
		}
	}

	return keys
}

// Seed writes one entry per key so every benchmark read is a hit.
func Seed(tb testing.TB, store beeline.Store, keys []beeline.PairKey) {
	tb.Helper()

	entries := make([]beeline.Entry, len(keys))
	for i := range keys {
		entries[i] = beeline.Entry{
			Key:        keys[i],
			ComputedAt: time.Now(),
			Duration:   float64(i), Distance: float64(i),
		}
	}
	require.NoError(tb, store.Put(context.Background(), entries))
}

// Run measures BatchGet over the full key set b.N times and reports p50/p95
// wall time in milliseconds alongside the standard ns/op. Use -benchtime=10x
// (the bench-store target does) so the percentiles rest on enough samples.
func Run(b *testing.B, store beeline.Store, keys []beeline.PairKey) {
	b.Helper()

	ctx := context.Background()
	samples := make([]time.Duration, 0, b.N)

	b.ResetTimer()
	for range b.N {
		start := time.Now()
		got, err := store.BatchGet(ctx, keys)
		elapsed := time.Since(start)
		if err != nil {
			b.Fatalf("BatchGet: %v", err)
		}
		if len(got) != len(keys) || got[0] == nil {
			b.Fatalf("BatchGet returned %d results (first nil: %t), want %d hits", len(got), got[0] == nil, len(keys))
		}
		samples = append(samples, elapsed)
	}
	b.StopTimer()

	slices.Sort(samples)
	b.ReportMetric(ms(percentile(samples, 0.50)), "p50-ms")
	b.ReportMetric(ms(percentile(samples, 0.95)), "p95-ms")
	b.ReportMetric(float64(len(keys)), "keys")
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(p * float64(len(sorted)-1))

	return sorted[idx]
}

func ms(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1e6 }
