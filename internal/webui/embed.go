// Package webui serves the embedded single-page operator console: a map-based UI
// for managing service areas (the /_config_/areas registry — create from GeoJSON,
// enable/disable, refine hexes) and watching a selected area's cache load (GET
// /_ops_/freshness and /_ops_/cells, scoped with ?area). The assets — the app plus
// vendored Leaflet and h3-js — are compiled into the binary via go:embed so
// `beeline serve` stays a single self-hostable artifact with no build step and no
// CDN dependency.
package webui

import (
	"embed"
	"io/fs"
	"net/http"

	"github.com/primandproper/platform-go/v7/observability/logging"
	"github.com/primandproper/platform-go/v7/routing"
)

//go:embed static
var content embed.FS

// Register mounts the UI: index.html at / and every bundled asset under /assets/.
// It reads index.html once at startup so the hot path is a byte write. The
// explicit /assets/ prefix keeps the file server clear of the API routes. Both
// mounts are raw handlers — static files don't fit the typed model and stay out
// of the OpenAPI spec.
func Register(router *routing.Router, logger logging.Logger) error {
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

	router.Handle(http.MethodGet, "/", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if _, wErr := w.Write(index); wErr != nil {
			logger.Error("writing index.html", wErr)
		}
	}))

	router.Handle(http.MethodGet, "/assets/*", fileServer)

	return nil
}
