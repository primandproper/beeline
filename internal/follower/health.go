package follower

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/primandproper/primitives-go/v2/healthcheck"
	"github.com/primandproper/primitives-go/v2/observability/logging"
	"github.com/primandproper/primitives-go/v2/routing"
)

// readyProbeTimeout bounds the leader liveness probe behind /_ops_/ready, so a
// hung leader turns into a fast 503 instead of a stalled health check. It lives
// inside the checker rather than around the registry because the registry's own
// per-checker budget is a fixed 5s — longer than a follower should wait.
const readyProbeTimeout = 2 * time.Second

// leaderChecker reports whether the leader is reachable right now: one live
// probe per call, so an orchestrator's readiness gate tracks actual
// connectivity, not a cached flag.
type leaderChecker struct {
	follower *Follower
}

// Name identifies the component in health results.
func (c leaderChecker) Name() string { return "leader" }

// Check pings the leader under the follower's own probe budget.
func (c leaderChecker) Check(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, readyProbeTimeout)
	defer cancel()

	return c.follower.Ping(ctx)
}

// RegisterHealth mounts the follower's only HTTP surface: /_ops_/live (process
// up) and /_ops_/ready (leader reachable). Both are raw mounts: a follower
// serves no OpenAPI spec, and the ready probe's dynamic 200/503 doesn't fit the
// typed model. The registry's aggregate status is mapped back onto the flat
// {"status": …} body rather than serialized directly — that body is the frozen
// wire contract with orchestrators already probing followers.
func RegisterHealth(router *routing.Router, f *Follower, logger logging.Logger) error {
	logger = logging.EnsureLogger(logger)

	registry, err := healthcheck.NewRegistry(healthcheck.WithLogger(logger))
	if err != nil {
		return fmt.Errorf("building the follower health registry: %w", err)
	}

	registry.Register(leaderChecker{follower: f})

	router.Handle(http.MethodGet, "/_ops_/live", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, logger, http.StatusOK, "up")
	}))

	router.Handle(http.MethodGet, "/_ops_/ready", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if result := registry.CheckAll(r.Context()); result.Status == healthcheck.StatusDown {
			logger.WithValues(map[string]any{"components": result.Components}).
				Debug("leader unreachable; reporting not ready")
			writeStatus(w, logger, http.StatusServiceUnavailable, "down")

			return
		}

		writeStatus(w, logger, http.StatusOK, "up")
	}))

	return nil
}

func writeStatus(w http.ResponseWriter, logger logging.Logger, status int, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": state}); err != nil {
		logger.Error("encoding health response", err)
	}
}
