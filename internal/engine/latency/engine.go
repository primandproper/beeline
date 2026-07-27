// Package latency wraps any beeline.RoutingEngine and adds a configurable,
// randomized delay to every Table call. It exists to model a network-bound engine
// — a real OSRM/Valhalla reached over HTTP, where each /table request pays a round
// trip — so the rest of the system (the refresh worker pool, the read path) can be
// exercised under realistic I/O latency instead of the nanosecond returns of the
// in-process Haversine stand-in.
//
// It is a decorator, not a second routing implementation: it carries no routing math
// of its own, so it composes over the Haversine engine today and a real engine later
// without change. The delay is a context-aware sleep, so a cancelled context aborts
// the simulated in-flight request exactly as a real HTTP client would.
package latency

import (
	"context"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/primandproper/beeline/internal/beeline"
)

// Engine adds a per-call latency in [min, max] to a wrapped RoutingEngine.
type Engine struct {
	inner beeline.RoutingEngine

	// pick returns the delay for one Table call, in [minLatency, maxLatency].
	// Injectable so tests can make the delay deterministic.
	pick func(minLatency, maxLatency time.Duration) time.Duration
	// sleep waits d, aborting early if ctx is cancelled. Injectable so tests do
	// not wait real wall-clock time.
	sleep func(ctx context.Context, d time.Duration) error

	minLatency time.Duration
	maxLatency time.Duration
}

// Option customizes an Engine at construction. The defaults (a math/rand/v2 delay
// and a real context-aware sleep) are what production uses; the options exist so
// tests can inject a deterministic delay and a no-op sleep.
type Option func(*Engine)

// WithPick overrides how the per-call delay is chosen. Used by tests to make the
// delay deterministic.
func WithPick(pick func(minLatency, maxLatency time.Duration) time.Duration) Option {
	return func(e *Engine) { e.pick = pick }
}

// WithSleep overrides how the chosen delay is waited out. Used by tests to avoid
// spending real time while still observing that a delay was requested.
func WithSleep(sleep func(ctx context.Context, d time.Duration) error) Option {
	return func(e *Engine) { e.sleep = sleep }
}

// New wraps inner so every Table call is delayed by a random duration in
// [minLatency, maxLatency] before delegating. minLatency and maxLatency must be
// non-negative with maxLatency >= minLatency; equal bounds give a fixed delay and
// both-zero is a pass-through. It panics on an invalid range so a misconfiguration
// fails at construction rather than silently disabling the delay.
func New(inner beeline.RoutingEngine, minLatency, maxLatency time.Duration, opts ...Option) *Engine {
	if minLatency < 0 || maxLatency < minLatency {
		panic(fmt.Sprintf("latency: invalid range [%v, %v]", minLatency, maxLatency))
	}

	e := &Engine{
		inner:      inner,
		pick:       randomLatency,
		sleep:      sleepContext,
		minLatency: minLatency,
		maxLatency: maxLatency,
	}
	for _, opt := range opts {
		opt(e)
	}

	return e
}

// Table waits the simulated network delay, then delegates to the wrapped engine.
// A context cancelled during the delay returns the context error without ever
// reaching the inner engine, matching an aborted HTTP request.
func (e *Engine) Table(ctx context.Context, req beeline.TableRequest) (beeline.TableResponse, error) {
	if err := e.sleep(ctx, e.pick(e.minLatency, e.maxLatency)); err != nil {
		return beeline.TableResponse{}, err
	}

	return e.inner.Table(ctx, req)
}

// Capabilities reports the wrapped engine's capabilities unchanged: adding latency
// does not alter which profiles route or whether distance is returned.
func (e *Engine) Capabilities() beeline.Capabilities {
	return e.inner.Capabilities()
}

// randomLatency returns a uniformly random duration in [minLatency, maxLatency].
// It uses math/rand/v2's top-level source, which is safe for the concurrent Table
// calls the refresh pool makes.
func randomLatency(minLatency, maxLatency time.Duration) time.Duration {
	if maxLatency <= minLatency {
		return minLatency
	}

	// Simulated network jitter needs no cryptographic randomness.
	return minLatency + time.Duration(rand.Int64N(int64(maxLatency-minLatency))) //nolint:gosec // G404: latency simulation, not security-sensitive.
}

// sleepContext waits for d or until ctx is done, whichever comes first. A
// non-positive d returns immediately (still honoring an already-cancelled context).
func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
