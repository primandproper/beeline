package follower_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
	"github.com/primandproper/beeline/internal/engine/registry"
	"github.com/primandproper/beeline/internal/follower"

	"github.com/primandproper/platform-go/v10/circuitbreaking"
	"github.com/primandproper/platform-go/v10/retry"
	retrycfg "github.com/primandproper/platform-go/v10/retry/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fastPolicy builds a millisecond-scale backoff policy, so a retry budget is
// observable without the test waiting on real delays.
func fastPolicy(t *testing.T, maxAttempts uint) retry.Policy {
	t.Helper()

	policy, err := retrycfg.NewExponentialBackoffPolicy(retrycfg.Config{
		MaxAttempts:  maxAttempts,
		InitialDelay: time.Millisecond,
		MaxDelay:     2 * time.Millisecond,
		Multiplier:   2,
	})
	require.NoError(t, err)

	return policy
}

// retryingFollower points at leaderURL with a fast three-attempt policy, so the
// classification is observable without the test waiting on real backoff.
func retryingFollower(t *testing.T, leaderURL string) *follower.Follower {
	t.Helper()

	f, err := follower.New(&follower.Config{
		LeaderURL:    leaderURL,
		Fallback:     testFallback(),
		BuildEngines: registry.BuildAll,
		Retry:        fastPolicy(t, 3),
	}, nil)
	require.NoError(t, err)

	return f
}

// TestClaimRetriesServerErrors covers a leader that is briefly unhealthy — a
// restart, a failover — where the next attempt is expected to succeed.
func TestClaimRetriesServerErrors(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusInternalServerError)

			return
		}

		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
			"leaseSeconds": 30,
			"pairs":        []map[string]any{},
		}))
	}))
	defer srv.Close()

	keys, err := retryingFollower(t, srv.URL).Claim(context.Background(), 8, 30*time.Second)
	require.NoError(t, err, "the third attempt succeeds")
	assert.Empty(t, keys)
	assert.Equal(t, int64(3), attempts.Load(), "the two 5xx responses were retried")
}

// TestClaimDoesNotRetryClientErrors is the other half of the contract: a leader
// that rejected the request will reject it again, so retrying only wastes the
// worker's time.
func TestClaimDoesNotRetryClientErrors(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	_, err := retryingFollower(t, srv.URL).Claim(context.Background(), 8, 30*time.Second)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned status 400", "the original error still surfaces verbatim")
	assert.Equal(t, int64(1), attempts.Load(), "a 4xx is terminal")
}

// TestSubmitRetriesServerErrors pins that submits retry too. Duplicate submits
// are idempotent by construction, so a retry after an ambiguous failure is at
// worst wasted work.
func TestSubmitRetriesServerErrors(t *testing.T) {
	t.Parallel()

	origin, err := beeline.CellAt(beeline.LatLng{Lat: 37.7749, Lng: -122.4194}, 9)
	require.NoError(t, err)
	dest, err := beeline.CellAt(beeline.LatLng{Lat: 37.70, Lng: -122.4194}, 9)
	require.NoError(t, err)

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 2 {
			w.WriteHeader(http.StatusBadGateway)

			return
		}
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(map[string]any{}))
	}))
	defer srv.Close()

	err = retryingFollower(t, srv.URL).Submit(context.Background(), []beeline.Entry{{
		Key:    beeline.PairKey{Area: 1, Origin: origin, Dest: dest, Profile: "car", Res: 9},
		Stored: beeline.Stored{Estimate: beeline.Estimate{Duration: 12, Distance: 340}},
	}})
	require.NoError(t, err)
	assert.Equal(t, int64(2), attempts.Load(), "the 502 was retried")
}

// TestClaimRetryExhaustionSurfacesError keeps a permanently-down leader from
// looking like an empty queue: the pool must see an error and idle, not treat
// the failure as "nothing due".
func TestClaimRetryExhaustionSurfacesError(t *testing.T) {
	t.Parallel()

	var attempts atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := retryingFollower(t, srv.URL).Claim(context.Background(), 8, 30*time.Second)
	require.Error(t, err)
	assert.Equal(t, int64(3), attempts.Load(), "every attempt in the budget was spent")
}

// stubBreaker is an open-or-closed breaker with no failure accounting, so a test
// can assert what a follower does on each side of the switch.
type stubBreaker struct {
	open atomic.Bool
}

func (b *stubBreaker) Failed()             { b.open.Store(true) }
func (b *stubBreaker) Succeeded()          {}
func (b *stubBreaker) CanProceed() bool    { return !b.open.Load() }
func (b *stubBreaker) CannotProceed() bool { return b.open.Load() }

// TestClaimShedsLoadWhenBreakerIsOpen pins the shed behavior: once the breaker
// trips, a claim costs no request at all — the worker returns to its idle
// backoff instead of queueing more load onto a leader that is already failing.
func TestClaimShedsLoadWhenBreakerIsOpen(t *testing.T) {
	t.Parallel()

	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	breaker := &stubBreaker{}
	f, err := follower.New(&follower.Config{
		LeaderURL:    srv.URL,
		Fallback:     testFallback(),
		BuildEngines: registry.BuildAll,
		Breaker:      breaker,
		Retry:        fastPolicy(t, 5),
	}, nil)
	require.NoError(t, err)

	// The first 5xx trips the stub, and the retry loop stops immediately rather
	// than spending the remaining four attempts.
	_, err = f.Claim(context.Background(), 8, 30*time.Second)
	require.Error(t, err)
	assert.Equal(t, int64(1), requests.Load(), "a tripped breaker ends the retry budget")

	// Subsequent claims never reach the network at all.
	_, err = f.Claim(context.Background(), 8, 30*time.Second)
	require.ErrorIs(t, err, circuitbreaking.ErrCircuitBroken)
	assert.Equal(t, int64(1), requests.Load(), "no further requests are issued while open")
}
