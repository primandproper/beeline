// Package webui serves the embedded single-page operator console: a map-based UI
// for drawing the service-area boundary and tessellation scheme (POST
// /_config_/area) and watching the cache load for that area (GET /_ops_/freshness
// and /_ops_/cells). The assets — the app plus vendored Leaflet and h3-js — are
// compiled into the binary via go:embed so `beeline serve` stays a single
// self-hostable artifact with no build step and no CDN dependency.
package webui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/primandproper/platform-go/v4/observability/logging"
	"github.com/primandproper/platform-go/v4/routing"
)

//go:embed static
var content embed.FS

// Register mounts the UI: index.html at / and every bundled asset under /assets/.
// It reads index.html once at startup so the hot path is a byte write. The
// explicit /assets/ prefix keeps the file server clear of the API routes.
func Register(router routing.Router, logger logging.Logger) error {
	logger = logging.EnsureLogger(logger)

	index, err := content.ReadFile("static/index.html")
	if err != nil {
		return err
	}

	assets, err := fs.Sub(content, "static/assets")
	if err != nil {
		return err
	}

	fileServer := http.StripPrefix("/assets/", http.FileServer(http.FS(assets)))

	router.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, wErr := w.Write(index); wErr != nil {
			logger.Error("writing index.html", wErr)
		}
	})

	router.Handle("/assets/*", fileServer)

	return nil
}
