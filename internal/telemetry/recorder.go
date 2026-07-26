package telemetry

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/primandproper/platform-go/v7/observability/logging"
)

// defaultFlushInterval backstops a zero FlushInterval so a misconfigured Recorder
// still flushes; config validation normally rejects that before it gets here.
const defaultFlushInterval = 5 * time.Second

// Recorder is the bridge between the read path and a Sink: RecordFetch is a
// non-blocking bounded-channel send (a full buffer drops the event and counts it —
// telemetry never slows a query), and a single flusher goroutine (Run) consumes the
// channel, writing raw events and/or folding them into the aggregator per Config.
//
// Run deliberately takes no context: were it tied to the serve context it would
// stop consuming before the HTTP server finishes draining in-flight requests,
// silently dropping their events. Instead the owner calls Close after the server
// has shut down; Close drains whatever is buffered, flushes the aggregator fully,
// and closes the sink.
type Recorder struct {
	events        chan FetchEvent
	sink          Sink
	agg           *aggregator
	logger        logging.Logger
	stop          chan struct{}
	done          chan struct{}
	flushInterval time.Duration
	dropped       atomic.Uint64
	loggedDropped uint64 // flusher-goroutine only: high-water mark already logged
	raw           bool
	stopOnce      sync.Once
}

// NewRecorder builds a Recorder over sink. Start it with `go r.Run()` and stop it
// with Close. A nil logger is replaced with a noop.
func NewRecorder(sink Sink, cfg Config, logger logging.Logger) *Recorder {
	r := &Recorder{
		events:        make(chan FetchEvent, max(cfg.BufferSize, 1)),
		sink:          sink,
		raw:           cfg.RawEnabled,
		flushInterval: cfg.FlushInterval,
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		logger:        logging.EnsureLogger(logger),
	}
	if r.flushInterval <= 0 {
		r.flushInterval = defaultFlushInterval
	}
	if cfg.AggregateEnabled {
		r.agg = newAggregator(cfg.AggregateBucket, cfg.AggregateMaxKeys)
	}

	return r
}

// RecordFetch hands one event to the flusher. It never blocks: when the buffer is
// full the event is dropped and counted instead. The event is copied into the
// buffer; the pointer is not retained.
func (r *Recorder) RecordFetch(ev *FetchEvent) {
	select {
	case r.events <- *ev:
	default:
		r.dropped.Add(1)
	}
}

// Dropped reports how many events have been dropped because the buffer was full.
func (r *Recorder) Dropped() uint64 {
	return r.dropped.Load()
}

// Run is the flusher loop: it consumes events, ticks the periodic flush, and on
// Close drains the buffer, flushes everything, and closes the sink. Run returns
// only after Close is called.
func (r *Recorder) Run() {
	defer close(r.done)

	ticker := time.NewTicker(r.flushInterval)
	defer ticker.Stop()

	for {
		select {
		case ev := <-r.events:
			r.consume(&ev)
		case <-ticker.C:
			r.flush(time.Now(), false)
		case <-r.stop:
			r.drain()

			return
		}
	}
}

// Close stops the flusher and waits for it to drain buffered events and close the
// sink, up to ctx's deadline. Safe to call more than once.
func (r *Recorder) Close(ctx context.Context) error {
	r.stopOnce.Do(func() { close(r.stop) })

	select {
	case <-r.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// consume applies one event to the enabled channels. Sink errors are logged, never
// surfaced: the query that generated the event has long been answered.
func (r *Recorder) consume(ev *FetchEvent) {
	if r.raw {
		if err := r.sink.WriteFetch(ev); err != nil {
			r.logger.Error("writing telemetry fetch event", err)
		}
	}
	if r.agg != nil {
		r.agg.observe(ev)
	}
}

// flush emits completed aggregate buckets (all of them when all is set), reports
// drop/overflow counters, and flushes the sink.
func (r *Recorder) flush(now time.Time, all bool) {
	if r.agg != nil {
		recs := r.agg.flush(now, all)
		for i := range recs {
			if err := r.sink.WriteAggregate(&recs[i]); err != nil {
				r.logger.Error("writing telemetry aggregate record", err)
			}
		}
		if ov := r.agg.takeOverflow(); ov > 0 {
			r.logger.WithValues(map[string]any{"overflowed": ov}).Info("telemetry aggregation dropped events: bucket map full")
		}
	}

	if d := r.dropped.Load(); d > r.loggedDropped {
		r.logger.WithValues(map[string]any{"dropped": d - r.loggedDropped, "total": d}).Info("telemetry events dropped: buffer full")
		r.loggedDropped = d
	}

	if err := r.sink.Flush(); err != nil {
		r.logger.Error("flushing telemetry sink", err)
	}
}

// drain empties the channel after stop, then does a final full flush and closes
// the sink. New RecordFetch calls racing the drain may still land in the buffer and
// are consumed too; anything sent after the final sweep is dropped by the closed
// sink's error path, not lost silently mid-file.
func (r *Recorder) drain() {
	for {
		select {
		case ev := <-r.events:
			r.consume(&ev)
		default:
			r.flush(time.Now(), true)
			if err := r.sink.Close(); err != nil {
				r.logger.Error("closing telemetry sink", err)
			}

			return
		}
	}
}
