package telemetry_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/telemetry"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// captureSink records everything written to it, guarded because the recorder's
// flusher goroutine writes while tests read.
type captureSink struct {
	fetches    []telemetry.FetchEvent
	aggregates []telemetry.AggregateRecord
	mu         sync.Mutex
	closed     bool
}

func (s *captureSink) WriteFetch(ev *telemetry.FetchEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches = append(s.fetches, *ev)

	return nil
}

func (s *captureSink) WriteAggregate(rec *telemetry.AggregateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.aggregates = append(s.aggregates, *rec)

	return nil
}

func (s *captureSink) Flush() error { return nil }

func (s *captureSink) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true

	return nil
}

func (s *captureSink) fetchCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.fetches)
}

func (s *captureSink) snapshot() ([]telemetry.FetchEvent, []telemetry.AggregateRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return append([]telemetry.FetchEvent(nil), s.fetches...), append([]telemetry.AggregateRecord(nil), s.aggregates...), s.closed
}

func recorderKey() beeline.PairKey {
	return beeline.PairKey{Area: 1, Origin: 1, Dest: 2, Profile: "car", Res: 8}
}

func TestRecorderWritesRawEventsThroughRun(t *testing.T) {
	t.Parallel()

	sink := &captureSink{}
	rec := telemetry.NewRecorder(sink, telemetry.Config{BufferSize: 16, FlushInterval: 10 * time.Millisecond, RawEnabled: true}, nil)
	go rec.Run()

	rec.RecordFetch(&telemetry.FetchEvent{At: time.Now(), Key: recorderKey(), Source: telemetry.SourceCache, Stale: true})

	require.Eventually(t, func() bool { return sink.fetchCount() == 1 }, time.Second, time.Millisecond)
	fetches, _, _ := sink.snapshot()
	assert.Equal(t, telemetry.SourceCache, fetches[0].Source)
	assert.True(t, fetches[0].Stale)
	assert.Equal(t, recorderKey(), fetches[0].Key)

	require.NoError(t, rec.Close(context.Background()))
	_, _, closed := sink.snapshot()
	assert.True(t, closed, "Close closes the sink")
}

func TestRecorderDropsInsteadOfBlockingWhenFull(t *testing.T) {
	t.Parallel()

	// Never started: the buffer fills and stays full, so overflow must drop.
	rec := telemetry.NewRecorder(&captureSink{}, telemetry.Config{BufferSize: 2, RawEnabled: true}, nil)

	for range 5 {
		rec.RecordFetch(&telemetry.FetchEvent{Key: recorderKey(), Source: telemetry.SourceCache})
	}

	assert.Equal(t, uint64(3), rec.Dropped(), "events past the buffer are counted, not blocked on")
}

func TestRecorderCloseDrainsBufferedEvents(t *testing.T) {
	t.Parallel()

	sink := &captureSink{}
	// Long flush interval: nothing is consumed by the ticker; only the drain path
	// can deliver these.
	rec := telemetry.NewRecorder(sink, telemetry.Config{BufferSize: 16, FlushInterval: time.Hour, RawEnabled: true}, nil)

	for range 5 {
		rec.RecordFetch(&telemetry.FetchEvent{At: time.Now(), Key: recorderKey(), Source: telemetry.SourceDemand})
	}

	go rec.Run()
	require.NoError(t, rec.Close(context.Background()))

	fetches, _, closed := sink.snapshot()
	assert.Len(t, fetches, 5, "Close drains every buffered event before closing")
	assert.True(t, closed)
}

func TestRecorderAggregateOnlyWritesNoRawEvents(t *testing.T) {
	t.Parallel()

	sink := &captureSink{}
	rec := telemetry.NewRecorder(sink, telemetry.Config{
		BufferSize:       16,
		FlushInterval:    time.Hour,
		AggregateEnabled: true,
		AggregateBucket:  time.Minute,
		AggregateMaxKeys: 100,
	}, nil)

	at := time.Date(2026, 7, 23, 18, 0, 30, 0, time.UTC)
	for range 3 {
		rec.RecordFetch(&telemetry.FetchEvent{At: at, Key: recorderKey(), Source: telemetry.SourceCache})
	}

	go rec.Run()
	require.NoError(t, rec.Close(context.Background()))

	fetches, aggregates, _ := sink.snapshot()
	assert.Empty(t, fetches, "raw channel disabled")
	require.Len(t, aggregates, 1, "drain flushes the incomplete bucket")
	assert.Equal(t, int64(3), aggregates[0].Count)
	assert.Equal(t, int64(3), aggregates[0].Cache)
	assert.Equal(t, at.Truncate(time.Minute), aggregates[0].BucketStart)
}

func TestRecorderCloseHonorsContextWhenRunNeverStarted(t *testing.T) {
	t.Parallel()

	rec := telemetry.NewRecorder(&captureSink{}, telemetry.Config{BufferSize: 1, RawEnabled: true}, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	err := rec.Close(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded, "no Run goroutine ever drains, so Close must give up with the context")
}
