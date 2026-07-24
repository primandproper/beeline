// Package telemetry captures read-path fetch events for offline analysis — the
// training data an organization needs to model which estimates get queried in which
// circumstances, and later feed predictions back through the warm endpoint. The
// read path hands events to a Recorder over a bounded, never-blocking buffer; a
// flusher goroutine serializes them through a pluggable Sink (JSONL file today,
// Kafka/S3/… behind the same interface later). Events identify pairs by H3 cell
// only — raw query coordinates are never recorded.
package telemetry

import (
	"time"

	"github.com/primandproper/beeline/internal/beeline"
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

// AggregateRecord is a flushed demand count for one (pair, time bucket): how many
// times the pair was fetched in the bucket, broken down by source, plus how many of
// those fetches served a stale value. The bucket start is floored to BucketSize.
type AggregateRecord struct {
	BucketStart time.Time
	Key         beeline.PairKey
	BucketSize  time.Duration
	Count       int64
	Cache       int64
	SameCell    int64
	Demand      int64
	Stale       int64
}

// Sink receives telemetry records from the Recorder's flusher goroutine — calls are
// single-threaded, so implementations need no locking for Write*, though Close may
// race a final flush and should guard itself. Records arrive by pointer purely to
// avoid copying; a sink must not retain or mutate them past the call. JSONLSink is
// the built-in implementation; a Kafka or object-store exporter would implement the
// same interface.
type Sink interface {
	WriteFetch(ev *FetchEvent) error
	WriteAggregate(rec *AggregateRecord) error
	// Flush pushes any buffered records toward durable storage; the Recorder calls
	// it on every flush tick so a tail -f of a file sink stays current.
	Flush() error
	Close() error
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
