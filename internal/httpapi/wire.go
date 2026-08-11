package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/primandproper/platform-go/v10/observability/logging"
	"github.com/primandproper/platform-go/v10/routing"
)

// The typed router serializes any error a handler returns as the platform
// APIError envelope, whose body shape and status map (no 409) don't match this
// API's flat {"error": "..."} contract. So handlers never return errors:
// wireMiddleware smuggles a committable ResponseWriter into the request
// context, fail/commitJSON write the legacy bytes through it, and the wire
// drops the framework's follow-up success write.

// wireKey carries the *wire through the request context.
type wireKey struct{}

// wire wraps the ResponseWriter handed to a typed route. Once a handler has
// committed a response through it, every later write — the framework encoding
// the handler's zero return value — is silently dropped.
type wire struct {
	http.ResponseWriter
	req       *http.Request
	committed bool
}

func (w *wire) WriteHeader(status int) {
	if w.committed {
		return
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *wire) Write(b []byte) (int, error) {
	if w.committed {
		return len(b), nil
	}

	return w.ResponseWriter.Write(b)
}

// wireMiddleware installs the escape hatch on every request. It must be
// registered before any route (the chi backend forbids later Use calls).
func wireMiddleware() routing.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ww := &wire{ResponseWriter: w}
			r = r.WithContext(context.WithValue(r.Context(), wireKey{}, ww))
			ww.req = r
			next.ServeHTTP(ww, r)
		})
	}
}

// wireFrom fetches the escape hatch; it is present on every request served
// through NewRouter.
func wireFrom(ctx context.Context) (*wire, bool) {
	w, ok := ctx.Value(wireKey{}).(*wire)
	return w, ok
}

// requestFrom exposes the raw *http.Request to the one handler that consumes
// its body directly (PUT …/geojson, whose payload is a document, not a struct).
func requestFrom(ctx context.Context) (*http.Request, bool) {
	w, ok := wireFrom(ctx)
	if !ok {
		return nil, false
	}

	return w.req, true
}

// commitJSON writes body with status through the escape hatch and marks the
// response committed, so the framework's own encoding pass becomes a no-op.
func commitJSON(ctx context.Context, logger logging.Logger, status int, body any) {
	w, ok := wireFrom(ctx)
	if !ok {
		logger.Error("committing response", errors.New("no wire response writer in context"))
		return
	}

	writeJSON(w, logger, status, body)
	w.committed = true
}

// fail writes this API's error shape — {"error": message} — with status.
func fail(ctx context.Context, logger logging.Logger, status int, message string) {
	commitJSON(ctx, logger, status, map[string]string{"error": message})
}
