package telemetry

import (
	"sort"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
)

// Source values for FetchEvent.Source. They mirror the read path's query.Source
// constants (query imports telemetry, so the strings are duplicated here; the query
// tests assert the two sets agree).
const (
	SourceCache    = "cache"
	SourceSameCell = "same-cell"
	SourceDemand   = "demand"
)

// aggKey identifies one aggregation cell: a pair in a time bucket. The bucket start
// is unix seconds (floored to the bucket size) so the key stays comparable.
type aggKey struct {
	key         beeline.PairKey
	bucketStart int64
}

// aggCounts is the running tally for one aggregation cell.
type aggCounts struct {
	count    int64
	cache    int64
	sameCell int64
	demand   int64
	stale    int64
}

// aggregator folds fetch events into per-(pair, bucket) counts. It is owned by the
// Recorder's flusher goroutine exclusively — single-threaded by construction, so it
// carries no lock. The map is bounded by maxKeys: once full, events for keys not
// already present are dropped and counted in overflow.
type aggregator struct {
	counts   map[aggKey]*aggCounts
	bucket   time.Duration
	maxKeys  int
	overflow uint64
}

func newAggregator(bucket time.Duration, maxKeys int) *aggregator {
	return &aggregator{
		bucket:  bucket,
		maxKeys: maxKeys,
		counts:  make(map[aggKey]*aggCounts),
	}
}

// observe folds one event into its bucket.
func (a *aggregator) observe(ev *FetchEvent) {
	k := aggKey{key: ev.Key, bucketStart: ev.At.Truncate(a.bucket).Unix()}

	c, ok := a.counts[k]
	if !ok {
		if len(a.counts) >= a.maxKeys {
			a.overflow++

			return
		}
		c = &aggCounts{}
		a.counts[k] = c
	}

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

// flush emits and removes completed buckets — those whose window ended at or before
// now — or every bucket when all is set (the drain path). Records are ordered by
// bucket start, then origin/dest, so output is deterministic.
func (a *aggregator) flush(now time.Time, all bool) []AggregateRecord {
	var out []AggregateRecord
	for k, c := range a.counts {
		start := time.Unix(k.bucketStart, 0).UTC()
		if !all && start.Add(a.bucket).After(now) {
			continue
		}

		out = append(out, AggregateRecord{
			BucketStart: start,
			BucketSize:  a.bucket,
			Key:         k.key,
			Count:       c.count,
			Cache:       c.cache,
			SameCell:    c.sameCell,
			Demand:      c.demand,
			Stale:       c.stale,
		})
		delete(a.counts, k)
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].BucketStart.Equal(out[j].BucketStart) {
			return out[i].BucketStart.Before(out[j].BucketStart)
		}
		if out[i].Key.Origin != out[j].Key.Origin {
			return out[i].Key.Origin < out[j].Key.Origin
		}

		return out[i].Key.Dest < out[j].Key.Dest
	})

	return out
}

// takeOverflow returns and resets the count of events dropped because the bucket
// map was full, for periodic logging.
func (a *aggregator) takeOverflow() uint64 {
	ov := a.overflow
	a.overflow = 0

	return ov
}
