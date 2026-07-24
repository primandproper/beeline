package telemetry

import (
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func aggEvent(at time.Time, key beeline.PairKey, source string, stale bool) *FetchEvent {
	return &FetchEvent{At: at, Key: key, Source: source, Stale: stale}
}

func TestAggregatorFoldsOneBucketWithSourceBreakdown(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Minute)
	key := beeline.PairKey{Area: 1, Origin: 1, Dest: 2, Profile: "car", Res: 8}

	agg := newAggregator(time.Minute, 100)
	agg.observe(aggEvent(start.Add(1*time.Second), key, SourceCache, false))
	agg.observe(aggEvent(start.Add(2*time.Second), key, SourceCache, true))
	agg.observe(aggEvent(start.Add(3*time.Second), key, SourceDemand, false))
	agg.observe(aggEvent(start.Add(4*time.Second), key, SourceSameCell, false))

	got := agg.flush(start.Add(2*time.Minute), false)
	require.Len(t, got, 1)
	rec := got[0]
	assert.Equal(t, start, rec.BucketStart)
	assert.Equal(t, time.Minute, rec.BucketSize)
	assert.Equal(t, key, rec.Key)
	assert.Equal(t, int64(4), rec.Count)
	assert.Equal(t, int64(2), rec.Cache)
	assert.Equal(t, int64(1), rec.Demand)
	assert.Equal(t, int64(1), rec.SameCell)
	assert.Equal(t, int64(1), rec.Stale)
}

func TestAggregatorSplitsAcrossBucketBoundary(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Minute)
	key := beeline.PairKey{Area: 1, Origin: 1, Dest: 2, Profile: "car", Res: 8}

	agg := newAggregator(time.Minute, 100)
	agg.observe(aggEvent(start.Add(59*time.Second), key, SourceCache, false))
	agg.observe(aggEvent(start.Add(60*time.Second), key, SourceCache, false))

	got := agg.flush(start.Add(5*time.Minute), false)
	require.Len(t, got, 2)
	assert.Equal(t, start, got[0].BucketStart)
	assert.Equal(t, int64(1), got[0].Count)
	assert.Equal(t, start.Add(time.Minute), got[1].BucketStart)
	assert.Equal(t, int64(1), got[1].Count)
}

func TestAggregatorFlushEmitsOnlyCompletedBucketsUnlessAll(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Minute)
	key := beeline.PairKey{Area: 1, Origin: 1, Dest: 2, Profile: "car", Res: 8}

	agg := newAggregator(time.Minute, 100)
	agg.observe(aggEvent(start.Add(time.Second), key, SourceCache, false))

	// The bucket's window has not ended yet: a periodic flush must hold it back.
	assert.Empty(t, agg.flush(start.Add(30*time.Second), false))

	// Still counted, not lost: the drain flush emits it.
	got := agg.flush(start.Add(30*time.Second), true)
	require.Len(t, got, 1)
	assert.Equal(t, int64(1), got[0].Count)

	// And flushing removes it: nothing is emitted twice.
	assert.Empty(t, agg.flush(start.Add(time.Hour), true))
}

func TestAggregatorBoundsKeysAndCountsOverflow(t *testing.T) {
	t.Parallel()

	start := time.Unix(1_700_000_000, 0).UTC().Truncate(time.Minute)

	agg := newAggregator(time.Minute, 2)
	for i := range 4 {
		key := beeline.PairKey{Area: 1, Origin: beeline.H3Cell(i + 1), Dest: 9, Profile: "car", Res: 8}
		agg.observe(aggEvent(start.Add(time.Second), key, SourceCache, false))
	}
	// A known key still counts even when the map is full.
	agg.observe(aggEvent(start.Add(2*time.Second), beeline.PairKey{Area: 1, Origin: 1, Dest: 9, Profile: "car", Res: 8}, SourceCache, false))

	assert.Equal(t, uint64(2), agg.takeOverflow(), "two new keys past the cap were dropped")
	assert.Zero(t, agg.takeOverflow(), "taking the overflow resets it")

	got := agg.flush(start.Add(time.Hour), true)
	require.Len(t, got, 2)
	assert.Equal(t, int64(2), got[0].Count, "the known key kept counting at the cap")
}
