package telemetry_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/telemetry"

	"github.com/primandproper/platform-go/v10/eventcapture/jsonl"

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

func TestRecorderWritesFetchAndAggregateLines(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := jsonl.NewSink(&jsonl.Config{Path: path, MaxBytes: 1 << 20, MaxFiles: 3})
	require.NoError(t, err)

	rec, err := telemetry.NewRecorder(sink, telemetry.Config{
		BufferSize:       16,
		FlushInterval:    time.Minute,
		RawEnabled:       true,
		AggregateEnabled: true,
		AggregateBucket:  5 * time.Minute,
		AggregateMaxKeys: 64,
	}, nil, nil)
	require.NoError(t, err)

	at := time.Date(2026, 7, 23, 18, 4, 5, 123_000_000, time.UTC)
	rec.Record(&telemetry.FetchEvent{At: at, Key: testKey(), Source: telemetry.SourceCache, Stale: true})
	rec.Record(&telemetry.FetchEvent{At: at.Add(time.Second), Key: testKey(), Source: telemetry.SourceDemand})
	rec.Record(&telemetry.FetchEvent{At: at.Add(2 * time.Second), Key: testKey(), Source: telemetry.SourceDemand})

	go rec.Run()
	require.NoError(t, rec.Close(context.Background()))

	lines := readLines(t, path)
	require.Len(t, lines, 4, "three raw fetch lines plus one aggregate bucket")

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
	}, lines[0])

	assert.Equal(t, map[string]any{
		"type":        "demand",
		"bucketStart": "2026-07-23T18:00:00.000Z",
		"bucketSec":   float64(300),
		"area":        float64(3),
		"origin":      "8944d55013bffff",
		"dest":        "8944d55010fffff",
		"res":         float64(9),
		"profile":     "car",
		"count":       float64(3),
		"cache":       float64(1),
		"sameCell":    float64(0),
		"demand":      float64(2),
		"stale":       float64(1),
	}, lines[3])
}

func TestRecorderAggregateOnlyOrdersBucketsByPair(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	sink, err := jsonl.NewSink(&jsonl.Config{Path: path, MaxBytes: 1 << 20, MaxFiles: 3})
	require.NoError(t, err)

	rec, err := telemetry.NewRecorder(sink, telemetry.Config{
		BufferSize:       16,
		FlushInterval:    time.Minute,
		AggregateEnabled: true,
		AggregateBucket:  5 * time.Minute,
		AggregateMaxKeys: 64,
	}, nil, nil)
	require.NoError(t, err)

	at := time.Date(2026, 7, 23, 18, 4, 0, 0, time.UTC)
	later := testKey() // sorts after: same origin, larger dest
	later.Dest = beeline.H3Cell(0x8944d55013bffff)
	// Recorded larger-dest first: output order must come from the sort, not
	// insertion.
	rec.Record(&telemetry.FetchEvent{At: at, Key: later, Source: telemetry.SourceDemand})
	rec.Record(&telemetry.FetchEvent{At: at, Key: testKey(), Source: telemetry.SourceCache})

	go rec.Run()
	require.NoError(t, rec.Close(context.Background()))

	lines := readLines(t, path)
	require.Len(t, lines, 2, "aggregate-only: no raw fetch lines")
	assert.Equal(t, "demand", lines[0]["type"])
	assert.Equal(t, "8944d55010fffff", lines[0]["dest"], "same-bucket rows sort by origin, then dest")
	assert.Equal(t, "8944d55013bffff", lines[1]["dest"])
}

func TestRecorderSinkRotatesAtMaxBytesAndPrunes(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	// Every line (~170 bytes) exceeds the threshold, so each write past the first
	// rotates the file aside.
	sink, err := jsonl.NewSink(&jsonl.Config{Path: path, MaxBytes: 64, MaxFiles: 2})
	require.NoError(t, err)

	rec, err := telemetry.NewRecorder(sink, telemetry.Config{
		BufferSize:    16,
		FlushInterval: time.Minute,
		RawEnabled:    true,
	}, nil, nil)
	require.NoError(t, err)

	at := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	for range 5 {
		rec.Record(&telemetry.FetchEvent{At: at, Key: testKey(), Source: telemetry.SourceDemand})
	}

	go rec.Run()
	require.NoError(t, rec.Close(context.Background()))

	rotated, err := filepath.Glob(path + ".*")
	require.NoError(t, err)
	assert.Len(t, rotated, 2, "pruning keeps at most maxFiles rotated siblings")

	// The live file holds exactly the last line.
	assert.Len(t, readLines(t, path), 1)
	for _, r := range rotated {
		assert.Len(t, readLines(t, r), 1)
	}
}

func TestRecorderResumesAppendAfterReopen(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "telemetry.jsonl")
	at := time.Date(2026, 7, 23, 18, 0, 0, 0, time.UTC)
	cfg := telemetry.Config{BufferSize: 16, FlushInterval: time.Minute, RawEnabled: true}

	for _, source := range []string{telemetry.SourceCache, telemetry.SourceDemand} {
		sink, err := jsonl.NewSink(&jsonl.Config{Path: path, MaxBytes: 1 << 20, MaxFiles: 3})
		require.NoError(t, err)

		rec, err := telemetry.NewRecorder(sink, cfg, nil, nil)
		require.NoError(t, err)
		rec.Record(&telemetry.FetchEvent{At: at, Key: testKey(), Source: source})

		go rec.Run()
		require.NoError(t, rec.Close(context.Background()))
	}

	lines := readLines(t, path)
	require.Len(t, lines, 2, "reopening appends instead of truncating")
	assert.Equal(t, "cache", lines[0]["source"])
	assert.Equal(t, "demand", lines[1]["source"])
}
