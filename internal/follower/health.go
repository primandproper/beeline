package follower

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/primandproper/platform-go/v7/observability/logging"
	"github.com/primandproper/platform-go/v7/routing"
)

// readyProbeTimeout bounds the leader liveness probe behind /_ops_/ready, so a
// hung leader turns into a fast 503 instead of a stalled health check.
const readyProbeTimeout = 2 * time.Second

// RegisterHealth mounts the follower's only HTTP surface: /_ops_/live (process
// up) and /_ops_/ready (leader reachable — one live probe per request, so an
// orchestrator's readiness gate tracks actual connectivity, not a cached flag).
// Both are raw mounts: a follower serves no OpenAPI spec, and the ready probe's
// dynamic 200/503 doesn't fit the typed model.
func RegisterHealth(router *routing.Router, f *Follower, logger logging.Logger) {
	logger = logging.EnsureLogger(logger)

	router.Handle(http.MethodGet, "/_ops_/live", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, logger, http.StatusOK, "up")
	}))

	router.Handle(http.MethodGet, "/_ops_/ready", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), readyProbeTimeout)
		defer cancel()

		if err := f.Ping(ctx); err != nil {
			logger.WithValues(map[string]any{"error": err.Error()}).Debug("leader unreachable; reporting not ready")
			writeStatus(w, logger, http.StatusServiceUnavailable, "down")
			return
		}

		writeStatus(w, logger, http.StatusOK, "up")
	}))
}

func writeStatus(w http.ResponseWriter, logger logging.Logger, status int, state string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(map[string]string{"status": state}); err != nil {
		logger.Error("encoding health response", err)
	}
}
