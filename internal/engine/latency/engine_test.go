package latency_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/engine/latency"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stubEngine records how it was called and returns a fixed response, so the tests
// can assert the decorator delegates without pulling in the real Haversine engine.
type stubEngine struct {
	err    error
	resp   beeline.TableResponse
	lastns beeline.TableRequest
	caps   beeline.Capabilities
	calls  int
}

func (s *stubEngine) Table(_ context.Context, req beeline.TableRequest) (beeline.TableResponse, error) {
	s.calls++
	s.lastns = req

	return s.resp, s.err
}

func (s *stubEngine) Capabilities() beeline.Capabilities { return s.caps }

func TestEngine(t *testing.T) {
	t.Parallel()

	sf := beeline.LatLng{Lat: 37.7749, Lng: -122.4194}
	req := beeline.TableRequest{Sources: []beeline.LatLng{sf}, Destinations: []beeline.LatLng{sf}, Profile: "car"}

	t.Run("delays then delegates to the inner engine", func(t *testing.T) {
		t.Parallel()

		inner := &stubEngine{resp: beeline.TableResponse{Duration: [][]float64{{0}}}}

		var slept time.Duration
		eng := latency.New(inner, 20*time.Millisecond, 80*time.Millisecond,
			latency.WithPick(func(minLatency, maxLatency time.Duration) time.Duration {
				// A pick that respects the bounds; assert it stays within range.
				assert.GreaterOrEqual(t, maxLatency, minLatency)
				return minLatency
			}),
			latency.WithSleep(func(_ context.Context, d time.Duration) error {
				slept = d
				return nil
			}),
		)

		resp, err := eng.Table(context.Background(), req)
		require.NoError(t, err)
		assert.Equal(t, 20*time.Millisecond, slept, "sleep should use the picked delay")
		assert.Equal(t, 1, inner.calls, "inner engine should be called once")
		assert.Equal(t, req.Profile, inner.lastns.Profile, "request should pass through unchanged")
		assert.Equal(t, inner.resp, resp, "response should pass through unchanged")
	})

	t.Run("a cancelled context aborts before reaching the inner engine", func(t *testing.T) {
		t.Parallel()

		inner := &stubEngine{}
		eng := latency.New(inner, time.Millisecond, time.Millisecond,
			latency.WithSleep(func(_ context.Context, _ time.Duration) error {
				return context.Canceled
			}),
		)

		_, err := eng.Table(context.Background(), req)
		require.ErrorIs(t, err, context.Canceled)
		assert.Zero(t, inner.calls, "inner engine must not be called when the delay is aborted")
	})

	t.Run("an inner error propagates after the delay", func(t *testing.T) {
		t.Parallel()

		sentinel := errors.New("engine down")
		inner := &stubEngine{err: sentinel}
		eng := latency.New(inner, 0, 0,
			latency.WithSleep(func(_ context.Context, _ time.Duration) error { return nil }),
		)

		_, err := eng.Table(context.Background(), req)
		assert.ErrorIs(t, err, sentinel)
	})

	t.Run("capabilities pass through unchanged", func(t *testing.T) {
		t.Parallel()

		inner := &stubEngine{caps: beeline.Capabilities{SupportsDistance: true, SupportedProfiles: []beeline.Profile{"car"}}}
		eng := latency.New(inner, 0, time.Second)

		caps := eng.Capabilities()
		assert.True(t, caps.SupportsDistance)
		assert.Contains(t, caps.SupportedProfiles, beeline.Profile("car"))
	})

	t.Run("default pick stays within the configured bounds", func(t *testing.T) {
		t.Parallel()

		inner := &stubEngine{}
		// Real sleep, but a sub-millisecond window so the test stays fast while still
		// exercising the default randomized pick and context-aware sleep.
		eng := latency.New(inner, 100*time.Microsecond, 300*time.Microsecond)

		start := time.Now()
		_, err := eng.Table(context.Background(), req)
		elapsed := time.Since(start)

		require.NoError(t, err)
		assert.GreaterOrEqual(t, elapsed, 100*time.Microsecond, "should wait at least the minimum")
		assert.Equal(t, 1, inner.calls)
	})

	t.Run("an invalid range panics at construction", func(t *testing.T) {
		t.Parallel()

		assert.Panics(t, func() { latency.New(&stubEngine{}, 2*time.Second, time.Second) })
		assert.Panics(t, func() { latency.New(&stubEngine{}, -time.Second, time.Second) })
	})
}
