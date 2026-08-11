// Package telemetry captures read-path fetch events for offline analysis — the
// training data an organization needs to model which estimates get queried in which
// circumstances, and later feed predictions back through the warm endpoint. The
// pipeline itself is platform-go's eventcapture package: the read path hands events
// to a Recorder over a bounded, never-blocking buffer; a flusher goroutine
// serializes them through a pluggable Sink (an eventcapture/jsonl file today,
// Kafka/S3/… behind the same interface later). This package owns only the
// composition: the event type, the demand aggregation, and the JSONL wire
// projections, which are a frozen contract with offline training consumers. Events
// identify pairs by H3 cell only — raw query coordinates are never recorded.
package telemetry

import (
	"cmp"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/primandproper/platform-go/v10/eventcapture"
	"github.com/primandproper/platform-go/v10/observability/logging"
	"github.com/primandproper/platform-go/v10/observability/metrics"
)

// Source values for FetchEvent.Source. They mirror the read path's query.Source
// constants (query imports telemetry, so the strings are duplicated here; the query
// tests assert the two sets agree).
const (
	SourceCache    = "cache"
	SourceSameCell = "same-cell"
	SourceDemand   = "demand"
)

// FetchEvent is one estimate actually fetched on the read path. It carries domain
// types, not strings: no formatting or allocation happens on the hot path —
// serialization is deferred to the sink in the flusher goroutine.
type FetchEvent struct {
	At     time.Time
	Source string // "cache" | "same-cell" | "demand" (query.Source values)
	Key    beeline.PairKey
	Stale  bool
}

// Config carries the Recorder's runtime knobs. It mirrors the operator-facing
// telemetry configuration without importing the config package, so telemetry stays
// a leaf package.
type Config struct {
	// BufferSize caps the in-flight event channel; a full buffer drops (and counts)
	// new events rather than ever blocking a query.
	BufferSize int
	// FlushInterval is the cadence of the flusher tick: completed aggregate buckets
	// are emitted and the sink is flushed.
	FlushInterval time.Duration
	// RawEnabled writes one record per fetch through the sink.
	RawEnabled bool
	// AggregateEnabled counts fetches per (pair, bucket) and writes a record per
	// completed bucket.
	AggregateEnabled bool
	// AggregateBucket is the aggregation window size.
	AggregateBucket time.Duration
	// AggregateMaxKeys bounds the in-memory bucket map; new keys past the cap are
	// dropped and counted.
	AggregateMaxKeys int
}

// Recorder is the capture pipeline instantiated for fetch events. The alias keeps
// callers on the domain name (`*telemetry.Recorder`) while the buffering, flushing,
// drop accounting, and lifecycle live in eventcapture.
type Recorder = eventcapture.Recorder[FetchEvent]

// NewRecorder composes the capture pipeline over sink from the configured
// channels: RawEnabled writes one wire line per fetch, AggregateEnabled folds
// fetches into per-(pair, time-bucket) demand counts emitted on each flush tick.
// Start it with `go r.Run()` and stop it with Close once the HTTP server has
// drained.
func NewRecorder(sink eventcapture.Sink, cfg Config, logger logging.Logger, metricsProvider metrics.Provider) (*Recorder, error) {
	opts := []eventcapture.Option{
		eventcapture.WithBufferSize(cfg.BufferSize),
		eventcapture.WithFlushInterval(cfg.FlushInterval),
		eventcapture.WithLogger(logger),
		eventcapture.WithMetricsProvider(metricsProvider),
	}

	if cfg.RawEnabled {
		opts = append(opts, eventcapture.WithTransform(newFetchLine))
	} else {
		opts = append(opts, eventcapture.WithoutRawRecords())
	}

	if cfg.AggregateEnabled {
		agg := eventcapture.NewAggregator[beeline.PairKey, demandCounts](
			cfg.AggregateBucket,
			cfg.AggregateMaxKeys,
			// Origin-then-dest ties the same-window bucket order to the old
			// aggregator's sort, keeping flushed output byte-identical for
			// downstream diffing.
			eventcapture.WithKeyOrder(func(a, b beeline.PairKey) int {
				if c := cmp.Compare(a.Origin, b.Origin); c != 0 {
					return c
				}

				return cmp.Compare(a.Dest, b.Dest)
			}),
		)
		opts = append(opts,
			eventcapture.WithObserver(func(ev *FetchEvent) {
				agg.Observe(ev.Key, ev.At, func(c *demandCounts) { c.fold(ev) })
			}),
			eventcapture.WithOnFlush(func(now time.Time, final bool, emit func(record any)) {
				for _, b := range agg.Flush(now, final) {
					emit(newAggregateLine(b))
				}
			}),
			eventcapture.WithOverflowSource(agg.TakeOverflow),
		)
	}

	return eventcapture.NewRecorder[FetchEvent](sink, opts...)
}

// demandCounts is the running tally for one (pair, time bucket) aggregation cell.
type demandCounts struct {
	count    int64
	cache    int64
	sameCell int64
	demand   int64
	stale    int64
}

// fold adds one fetch to the tally.
func (c *demandCounts) fold(ev *FetchEvent) {
	c.count++
	switch ev.Source {
	case SourceCache:
		c.cache++
	case SourceSameCell:
		c.sameCell++
	case SourceDemand:
		c.demand++
	}
	if ev.Stale {
		c.stale++
	}
}

// timeLayout matches the HTTP API's wire timestamp format so exported telemetry and
// live responses agree on how instants are rendered.
const timeLayout = "2006-01-02T15:04:05.000Z"

// fetchLine and aggregateLine are the JSONL projections of the record types: both
// kinds share one stream, discriminated by "type". Cells are hex-encoded H3 indexes
// (the same rendering as /_ops_/cells and /_ops_/pairs), never coordinates. These
// shapes are the frozen wire contract with offline training consumers — field set,
// order, and rendering must not change.
type fetchLine struct {
	Type    string `json:"type"` // "fetch"
	TS      string `json:"ts"`
	Origin  string `json:"origin"`
	Dest    string `json:"dest"`
	Profile string `json:"profile"`
	Source  string `json:"source"`
	Area    int64  `json:"area"`
	Res     int    `json:"res"`
	Stale   bool   `json:"stale"`
}

type aggregateLine struct {
	Type        string `json:"type"` // "demand"
	BucketStart string `json:"bucketStart"`
	Origin      string `json:"origin"`
	Dest        string `json:"dest"`
	Profile     string `json:"profile"`
	BucketSec   int64  `json:"bucketSec"`
	Area        int64  `json:"area"`
	Res         int    `json:"res"`
	Count       int64  `json:"count"`
	Cache       int64  `json:"cache"`
	SameCell    int64  `json:"sameCell"`
	Demand      int64  `json:"demand"`
	Stale       int64  `json:"stale"`
}

// newFetchLine projects one raw fetch event onto the wire.
func newFetchLine(ev *FetchEvent) any {
	return fetchLine{
		Type:    "fetch",
		TS:      ev.At.UTC().Format(timeLayout),
		Area:    int64(ev.Key.Area),
		Origin:  ev.Key.Origin.String(),
		Dest:    ev.Key.Dest.String(),
		Res:     ev.Key.Res,
		Profile: string(ev.Key.Profile),
		Source:  ev.Source,
		Stale:   ev.Stale,
	}
}

// newAggregateLine projects one flushed demand bucket onto the wire.
func newAggregateLine(b eventcapture.Bucket[beeline.PairKey, demandCounts]) aggregateLine {
	return aggregateLine{
		Type:        "demand",
		BucketStart: b.Start.UTC().Format(timeLayout),
		BucketSec:   int64(b.Size / time.Second),
		Area:        int64(b.Key.Area),
		Origin:      b.Key.Origin.String(),
		Dest:        b.Key.Dest.String(),
		Res:         b.Key.Res,
		Profile:     string(b.Key.Profile),
		Count:       b.Counts.count,
		Cache:       b.Counts.cache,
		SameCell:    b.Counts.sameCell,
		Demand:      b.Counts.demand,
		Stale:       b.Counts.stale,
	}
}
