// Package storetest is the conformance suite for Store implementations: one set
// of behavioral tests the in-memory, Postgres and Redis stores must all pass, so
// the three backends cannot drift apart. It is the Store counterpart to
// internal/freshness/freshnesstest.
//
// It matters most for the in-memory store. That one is the test double five
// packages (control, httpapi, query, refresh, follower) build their fixtures on,
// so anything it gets wrong is invisible until it reaches a real backend — and it
// was the least-tested of the three before this suite existed.
//
// Timestamps are handled per beeline.Store's contract: ComputedAt is advisory,
// the store may substitute its own clock, and precision/monotonic/location are
// all fair game to lose. The suite therefore asserts a window around the write
// rather than round-trip equality — asserting equality would fail Postgres for
// being correct.
package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/control"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Store is the full surface the suite exercises: the hot-path interface plus the
// control plane's per-area seam.
type Store interface {
	beeline.Store
	control.AreaStore
}

// Factory builds a fresh store. Implementations register their own cleanup on tb.
//
// Backends that cannot actually give out a *fresh* store (Redis and Postgres are
// shared servers) rely on the suite's per-subtest area discipline instead: every
// subtest below uses its own area IDs, so a shared backend cannot
// cross-contaminate. Factories for those backends may return the same handle
// every call.
type Factory func(tb testing.TB) Store

// clockSkew is how far outside the [write start, write end] window a stored
// ComputedAt may land before the suite calls it wrong. A server-stamped backend
// uses the database clock rather than the test process's, so the two can differ
// by more than scheduling jitter alone; this is generous on purpose, since the
// assertion is about "roughly now, not the value I passed in", not precision.
const clockSkew = 2 * time.Minute

// chunkSpanningKeys must exceed every backend's internal batch chunk size, or
// the alignment subtest silently proves nothing for the backends that chunk the
// hardest. Today: Postgres reads in 20k chunks (concurrently, so reassembly
// order is genuinely unspecified) and writes in 10k; Redis pipelines 5k. This is
// deliberately not a multiple of any of them, so an off-by-one at a boundary
// lands mid-chunk where it is visible. Raise it if a backend ever chunks larger.
const chunkSpanningKeys = 45_000

// key builds a suite pair key. Suite tests always use distinct areas per
// subtest, so shared backends don't cross-contaminate.
func key(area beeline.AreaID, origin, dest int) beeline.PairKey {
	return beeline.PairKey{
		Profile: "car",
		Area:    area,
		Origin:  beeline.H3Cell(origin),
		Dest:    beeline.H3Cell(dest),
		Res:     8,
	}
}

// entry pairs a key with a value whose scalars encode its identity, so a
// misaligned read is caught by value and not just by presence.
func entry(k beeline.PairKey, seq float64) beeline.Entry {
	return beeline.Entry{
		Key:        k,
		ComputedAt: time.Now(),
		Duration:   seq, Distance: seq * 10,
	}
}

// Run exercises the shared Store contract against the factory's implementation.
func Run(t *testing.T, factory Factory) {
	t.Helper()

	ctx := context.Background()

	t.Run("put then batch get round trips values and reports misses", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		hit := key(201, 1, 2)
		miss := key(201, 3, 4)

		before := time.Now()
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(hit, 12)}))
		after := time.Now()

		got, err := store.BatchGet(ctx, []beeline.PairKey{hit, miss})
		require.NoError(t, err)
		require.Len(t, got, 2, "one result per key")

		require.NotNil(t, got[0], "a written key is a hit")
		assert.InDelta(t, 12, got[0].Duration, 1e-9)
		assert.InDelta(t, 120, got[0].Distance, 1e-9)
		assertStampedAround(t, got[0].ComputedAt, before, after)

		assert.Nil(t, got[1], "a never-written key is a nil element, not a zero value")
	})

	t.Run("batch get is positionally aligned across chunk boundaries", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		// Every third key is a miss, so a chunk-offset bug in reassembly surfaces
		// as a value mismatch rather than a length mismatch — the length can be
		// right while every element is shifted.
		const n = chunkSpanningKeys
		keys := make([]beeline.PairKey, 0, n)
		entries := make([]beeline.Entry, 0, n)
		for i := range n {
			k := key(202, i, i+1)
			keys = append(keys, k)
			if i%3 != 0 {
				entries = append(entries, entry(k, float64(i)))
			}
		}
		require.NoError(t, store.Put(ctx, entries))

		got, err := store.BatchGet(ctx, keys)
		require.NoError(t, err)
		require.Len(t, got, n)
		for i := range n {
			if i%3 == 0 {
				require.Nilf(t, got[i], "key %d was never written", i)

				continue
			}
			require.NotNilf(t, got[i], "key %d was written", i)
			require.InDeltaf(t, float64(i), got[i].Duration, 1e-9, "key %d must get its own value back", i)
		}
	})

	t.Run("a repeated key is answered at every position", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		dup := key(203, 1, 2)
		absent := key(203, 9, 9)
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(dup, 7)}))

		// Positional alignment means one result per key, so a backend that
		// deduplicates keys before its lookup must still fan the answer back out.
		got, err := store.BatchGet(ctx, []beeline.PairKey{dup, absent, dup})
		require.NoError(t, err)
		require.Len(t, got, 3)
		require.NotNil(t, got[0])
		require.NotNil(t, got[2], "the repeated key is a hit at its second position too")
		assert.InDelta(t, 7, got[0].Duration, 1e-9)
		assert.InDelta(t, 7, got[2].Duration, 1e-9)
		assert.Nil(t, got[1])
	})

	t.Run("empty batches are no-ops, not errors", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		require.NoError(t, store.Put(ctx, nil), "Put with no entries")
		require.NoError(t, store.Put(ctx, []beeline.Entry{}), "Put with an empty slice")

		got, err := store.BatchGet(ctx, nil)
		require.NoError(t, err, "BatchGet with no keys")
		assert.Empty(t, got)

		got, err = store.BatchGet(ctx, []beeline.PairKey{})
		require.NoError(t, err, "BatchGet with an empty slice")
		assert.Empty(t, got)

		require.NoError(t, store.Delete(ctx, nil), "Delete with no keys")
		require.NoError(t, store.Delete(ctx, []beeline.PairKey{}), "Delete with an empty slice")
	})

	t.Run("put is idempotent and overwrites rather than duplicating", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		k := key(204, 1, 2)
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(k, 1)}))

		before := time.Now()
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(k, 2)}))
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(k, 2)}))
		after := time.Now()

		got, err := store.BatchGet(ctx, []beeline.PairKey{k})
		require.NoError(t, err)
		require.Len(t, got, 1, "a re-Put key is still one key")
		require.NotNil(t, got[0])
		assert.InDelta(t, 2, got[0].Duration, 1e-9, "the last write wins")
		assertStampedAround(t, got[0].ComputedAt, before, after)
	})

	t.Run("keys differing only in area, profile or resolution are distinct", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		base := key(205, 1, 2)

		otherArea := base
		otherArea.Area = 206

		otherProfile := base
		otherProfile.Profile = "bike"

		otherRes := base
		otherRes.Res = 9

		reversed := beeline.PairKey{Profile: base.Profile, Area: base.Area, Origin: base.Dest, Dest: base.Origin, Res: base.Res}

		variants := []beeline.PairKey{base, otherArea, otherProfile, otherRes, reversed}
		entries := make([]beeline.Entry, 0, len(variants))
		for i := range variants {
			entries = append(entries, entry(variants[i], float64(i+1)))
		}
		require.NoError(t, store.Put(ctx, entries))

		got, err := store.BatchGet(ctx, variants)
		require.NoError(t, err)
		require.Len(t, got, len(variants))
		for i := range variants {
			require.NotNilf(t, got[i], "variant %d must be stored", i)
			assert.InDeltaf(t, float64(i+1), got[i].Duration, 1e-9,
				"variant %d must not collide with another key", i)
		}
	})

	t.Run("delete removes the named keys and ignores absent ones", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		keep := key(207, 1, 2)
		drop := key(207, 3, 4)
		never := key(207, 5, 6)
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(keep, 1), entry(drop, 2)}))

		require.NoError(t, store.Delete(ctx, []beeline.PairKey{drop, never}),
			"deleting an absent key is a no-op, not an error")

		got, err := store.BatchGet(ctx, []beeline.PairKey{keep, drop, never})
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.NotNil(t, got[0], "an untouched key survives")
		assert.Nil(t, got[1], "the deleted key is gone")
		assert.Nil(t, got[2])
	})

	t.Run("delete area removes exactly one area and leaves others intact", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		doomed, survivor := beeline.AreaID(208), beeline.AreaID(209)
		inDoomed := []beeline.PairKey{key(doomed, 1, 2), key(doomed, 3, 4)}
		inSurvivor := []beeline.PairKey{key(survivor, 1, 2)}

		require.NoError(t, store.Put(ctx, []beeline.Entry{
			entry(inDoomed[0], 1), entry(inDoomed[1], 2), entry(inSurvivor[0], 3),
		}))

		require.NoError(t, store.DeleteArea(ctx, doomed))

		got, err := store.BatchGet(ctx, append(append([]beeline.PairKey{}, inDoomed...), inSurvivor...))
		require.NoError(t, err)
		require.Len(t, got, 3)
		assert.Nil(t, got[0], "the disabled area's estimates are evicted")
		assert.Nil(t, got[1])
		require.NotNil(t, got[2], "another area's estimates must survive")
		assert.InDelta(t, 3, got[2].Duration, 1e-9)
	})

	t.Run("delete area on an area with nothing stored is a no-op", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		require.NoError(t, store.DeleteArea(ctx, 210),
			"an area that was never written has nothing to evict, and that is not an error")
	})

	t.Run("computed at is store-authoritative and never zero", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		k := key(211, 1, 2)

		// A deliberately absurd advisory timestamp. Per the contract a store may
		// keep it or substitute its own clock — but it may not hand back a zero
		// time, because the read path measures staleness off this field
		// (query.go: time.Since(stored.ComputedAt) >= targetTTL), and a zero
		// value would read as infinitely stale.
		ancient := time.Now().Add(-100 * 24 * time.Hour)
		require.NoError(t, store.Put(ctx, []beeline.Entry{{
			Key:        k,
			ComputedAt: ancient, Duration: 1, Distance: 1,
		}}))

		got, err := store.BatchGet(ctx, []beeline.PairKey{k})
		require.NoError(t, err)
		require.Len(t, got, 1)
		require.NotNil(t, got[0])
		assert.False(t, got[0].ComputedAt.IsZero(), "a stored estimate always carries a usable timestamp")

		// Either behavior is conformant: the advisory stamp preserved, or the
		// store's own clock substituted. What is not conformant is a third
		// answer, and in particular a future timestamp.
		keptAdvisory := got[0].ComputedAt.Sub(ancient).Abs() < time.Second
		substituted := got[0].ComputedAt.After(ancient.Add(24 * time.Hour))
		assert.True(t, keptAdvisory || substituted,
			"ComputedAt must be either the advisory value or the store's own clock, got %s", got[0].ComputedAt)
		assert.False(t, got[0].ComputedAt.After(time.Now().Add(clockSkew)),
			"ComputedAt must never be in the future")
	})

	t.Run("successive puts do not move computed at backwards", func(t *testing.T) {
		t.Parallel()
		store := factory(t)

		k := key(212, 1, 2)
		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(k, 1)}))
		first, err := store.BatchGet(ctx, []beeline.PairKey{k})
		require.NoError(t, err)
		require.NotNil(t, first[0])

		// Enough of a gap to exceed millisecond truncation, so the comparison is
		// meaningful on a backend that stores whole milliseconds.
		time.Sleep(10 * time.Millisecond)

		require.NoError(t, store.Put(ctx, []beeline.Entry{entry(k, 2)}))
		second, err := store.BatchGet(ctx, []beeline.PairKey{k})
		require.NoError(t, err)
		require.NotNil(t, second[0])

		assert.False(t, second[0].ComputedAt.Before(first[0].ComputedAt),
			"re-computing a pair must not make it look older than it did before")
	})
}

// assertStampedAround asserts a stored ComputedAt is consistent with a write
// that happened between before and after, under either clock authority.
func assertStampedAround(t *testing.T, got, before, after time.Time) {
	t.Helper()

	assert.False(t, got.IsZero(), "ComputedAt must be set")
	assert.False(t, got.Before(before.Add(-clockSkew)),
		"ComputedAt %s is implausibly far before the write window opening at %s", got, before)
	assert.False(t, got.After(after.Add(clockSkew)),
		"ComputedAt %s is implausibly far after the write window closing at %s", got, after)
}
