package telemetry_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testKey() beeline.PairKey {
	return beeline.PairKey{
		Area:    3,
		Origin:  beeline.H3Cell(0x8944d55013bffff),
		Dest:    beeline.H3Cell(0x8944d55010fffff),
		Profile: "car",
		Res:     9,
	}
}

func readLines(t *testing.T, path string) []map[string]any {
	t.Helper()

	raw, err := os.ReadFile(path)
	require.NoError(t, err)

	var lines []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), "every line must be standalone JSON: %q", line)
		lines = append(lines, m)
	}

	return lines
}

func TestJSONLSinkWritesFetchAndAggregateLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := telemetry.NewJSONLSink(path, 1<<20, 3)
	require.NoError(t, err)

	at := time.Date(2026, 7, 23, 18, 4, 5, 123_000_000, time.UTC)
	require.NoError(t, sink.WriteFetch(&telemetry.FetchEvent{
		At:     at,
		Key:    testKey(),
		Source: telemetry.SourceCache,
		Stale:  true,
	}))
	require.NoError(t, sink.WriteAggregate(&telemetry.AggregateRecord{
		BucketStart: at.Truncate(5 * time.Minute),
		BucketSize:  5 * time.Minute,
		Key:         testKey(),
		Count:       17,
		Cache:       12,
		Demand:      5,
		Stale:       3,
	}))
	require.NoError(t, sink.Close())

	lines := readLines(t, path)
	require.Len(t, lines, 2)

	fetch := lines[0]
	assert.Equal(t, map[string]any{
		"type":    "fetch",
		"ts":      "2026-07-23T18:04:05.123Z",
		"area":    float64(3),
		"origin":  "8944d55013bffff",
		"dest":    "8944d55010fffff",
		"res":     float64(9),
		"profile": "car",
		"source":  "cache",
		"stale":   true,
	}, fetch)

	agg := lines[1]
	assert.Equal(t, map[string]any{
		"type":        "demand",
		"bucketStart": "2026-07-23T18:00:00.000Z",
		"bucketSec":   float64(300),
		"area":        float64(3),
		"origin":      "8944d55013bffff",
		"dest":        "8944d55010fffff",
		"res":         float64(9),
		"profile":     "car",
		"count":       float64(17),
		"cache":       float64(12),
		"sameCell":    float64(0),
		"demand":      float64(5),
		"stale":       float64(3),
	}, agg)
}

func TestJSONLSinkRotatesAtMaxBytesAndPrunes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	// Every line (~170 bytes) exceeds the threshold, so each write past the first
	// rotates the file aside.
	sink, err := telemetry.NewJSONLSink(path, 64, 2)
	require.NoError(t, err)

	at := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	for range 5 {
		require.NoError(t, sink.WriteFetch(&telemetry.FetchEvent{At: at, Key: testKey(), Source: telemetry.SourceDemand}))
	}
	require.NoError(t, sink.Close())

	rotated, err := filepath.Glob(path + ".*")
	require.NoError(t, err)
	assert.Len(t, rotated, 2, "pruning keeps at most maxFiles rotated siblings")

	// The live file holds exactly the last line.
	assert.Len(t, readLines(t, path), 1)
	for _, r := range rotated {
		assert.Len(t, readLines(t, r), 1)
	}
}

func TestJSONLSinkResumesAppendAfterReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	at := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)

	sink, err := telemetry.NewJSONLSink(path, 1<<20, 3)
	require.NoError(t, err)
	require.NoError(t, sink.WriteFetch(&telemetry.FetchEvent{At: at, Key: testKey(), Source: telemetry.SourceCache}))
	require.NoError(t, sink.Close())

	reopened, err := telemetry.NewJSONLSink(path, 1<<20, 3)
	require.NoError(t, err)
	require.NoError(t, reopened.WriteFetch(&telemetry.FetchEvent{At: at, Key: testKey(), Source: telemetry.SourceDemand}))
	require.NoError(t, reopened.Close())

	lines := readLines(t, path)
	require.Len(t, lines, 2, "reopening appends instead of truncating")
	assert.Equal(t, "cache", lines[0]["source"])
	assert.Equal(t, "demand", lines[1]["source"])
}

func TestJSONLSinkCloseIsIdempotentAndBlocksWrites(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := telemetry.NewJSONLSink(path, 1<<20, 3)
	require.NoError(t, err)

	require.NoError(t, sink.Close())
	require.NoError(t, sink.Close())
	require.NoError(t, sink.Flush(), "flushing a closed sink is a no-op")
	assert.Error(t, sink.WriteFetch(&telemetry.FetchEvent{Key: testKey(), Source: telemetry.SourceCache}))
}
