# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`github.com/primandproper/beeline` — a self-hostable server that precomputes travel-time/distance
matrices between H3 cells and serves cached scalar estimates under a freshness contract. Built on
[`github.com/primandproper/platform-go`](https://github.com/primandproper/platform-go). Go 1.26.
See `beeline-design.md` for the full design; section references (§) below point into it.

The application is a **Cobra CLI**. Two subcommands:

- `version` — prints build metadata to stdout.
- `serve` — the prototype. It tessellates the configured service area into H3 cells, seeds a
  freshness index with the origin→destination pair set, runs a background refresh loop that keeps
  those pairs fresh, and serves the read path over HTTP. The routing engine is a **Haversine**
  stand-in (great-circle distance ÷ per-profile speed) behind the same `RoutingEngine` interface a
  real engine (OSRM/Valhalla) would implement — so the whole pipeline runs with no external routing
  dependency. Store and freshness index are in-memory for the single-node prototype. `serve` also
  serves an **embedded operator console** at `/` (see `internal/webui/`) for drawing the service area
  and watching the cache load.

HTTP endpoints (default `:8080`):

- Read path — `GET /estimate?origin=lat,lng&dest=lat,lng&profile=car` (cache hit, same-cell
  correction, or synchronous demand-fill).
- Freshness/progress — `GET /_ops_/freshness` (the §3 debt/throughput contract as JSON; camelCase
  keys, the wire shape of `beeline.DebtStats`) and `GET /_ops_/cells` (per-origin-cell freshness
  rollup — cell id, center, `total`/`fresh`/`oldestAgeSeconds` — that paints the console's progress
  map).
- Control plane — `GET /_config_/area` (current area + derived cell/pair counts) and
  `POST /_config_/area` (`{lat,lng,resolution,areaRings,radiusRings}` → re-tessellate and re-seed the
  index/store at runtime). Both are served by `internal/control`. Unauthenticated, like the other
  endpoints; a real deploy would gate the `POST`.
- Health — `/_ops_/live` + `/_ops_/ready`.
- UI — `GET /` (the embedded console) and `/assets/*` (its bundled JS/CSS + vendored Leaflet/h3-js).

## Layout

- `cmd/main/main.go` — thin entrypoint: signal-cancellable context → `cli.Execute`.
- `cmd/tools/codegen/configs/` — codegen tool behind `make configs`: builds each environment's
  `*config.Config` as a real, typed Go object (`environments.go`), validates it, and renders it to
  `config/<env>.json` via `config.Render`. The checked-in JSON is a projection of these builders — edit
  the Go, never the JSON, then re-run `make configs`.
- `config/` — generated per-environment config files (`localdev.json`, `production.json`); committed so
  they stay reviewable, and loadable at runtime via `--config`.
- `internal/cli/` — cobra root command, observability bootstrap + shutdown, subcommands
  (`version.go`, `serve.go`). `serve.go` wires the whole matrix pipeline from `application.cfg` +
  `application.pillars`.
- `internal/config/` — assembles `observability.Config` and builds the pillars (slog logging + noop
  tracing/metrics/profiling by default). See `Config.NewPillars` for the upgrade path to real telemetry.
  Two loaders use `platform-go/v4/config`: `Load` overlays `BEELINE_`-prefixed environment
  variables on the flag/default-seeded config, and `LoadFromFile` decodes a complete JSON config file
  and then overlays the same environment variables. `Render` goes the other way: it validates typed
  `Config` objects and writes them to disk (see `make configs`). The matrix service is configured by
  `MatrixConfig` (`matrix.go`), a `Config.Matrix` field (env prefix `BEELINE_MATRIX_`, JSON key
  `matrix`): HTTP server, service-area/tessellation, profiles+speeds, and freshness knobs.

### Matrix service packages (design §5 seams)

- `internal/beeline/` — domain model and the three pluggable interfaces: `RoutingEngine`, `Store`,
  `FreshnessIndex` (§5). Core types (`PairKey`, `Estimate`, `Stored`, `DebtStats`, …) and cell
  helpers (`Center`, `CellAt`). `H3Cell` aliases `h3.Cell`.
- `internal/geo/` — pure `Haversine(a, b)` great-circle distance.
- `internal/engine/haversine/` — `RoutingEngine` implemented as Haversine ÷ per-profile speed. Swap
  a real engine in behind the interface without touching callers.
- `internal/tessellate/` — seeds the pair set from a center cell + ring counts (`h3.GridDisk`), the
  travel-radius-bounded stand-in for the design's GeoJSON polyfill (§7).
- `internal/store/memory/` — in-memory `Store` (map + RWMutex).
- `internal/freshness/memory/` — in-memory `FreshnessIndex`: leased queue (`Claim`/`MarkComputed`,
  §8), demand `Bump` (stale-while-revalidate), and the `Debt` signals (§3). Clock is injectable for
  tests.
- `internal/refresh/` — the worker+engine pool (§4): claim stalest → dense origin-centric 1×K table
  request → write → mark computed. Idle-backs-off when caught up.
- `internal/query/` — the read path (§9): cache hit (stale-while-revalidate), same-cell correction,
  synchronous demand-fill on a miss. `Handler.resolution` is held atomically so the control plane can
  swap it on a runtime re-tessellation.
- `internal/control/` — the runtime control plane. A `Coordinator` owns the mutable service `Area`
  and, on `Apply`, re-tessellates and swaps it in over three narrow seams (`Cache.Reset`,
  `Reseeder.Reseed`+`CellStates`, `Resolver.SetResolution`) while the refresh pool keeps running.
  `serve.go` seeds the initial area through it (replacing the old boot-time tessellate→Seed path).
- `internal/httpapi/` — HTTP routes registered on the platform-go chi router (read path, freshness,
  cells, control plane, health).
- `internal/webui/` — the embedded single-page operator console (`go:embed static`): Leaflet + h3-js
  (vendored under `static/assets/vendor/`, no CDN or build step). Configure the boundary/tessellation
  with a live client-side H3 preview, `POST /_config_/area` to rebuild, and watch the load via
  `/_ops_/freshness` (aggregate bar) + `/_ops_/cells` (per-cell overlay). Map tiles come from OSM, so
  the basemap needs internet at runtime; cells/markers still render offline.
- `version/` — build metadata (`CommitHash`/`BuildTime`/`CommitTime`), injected via `-ldflags` by
  `scripts/build.sh`.

## Common Commands

```bash
make setup          # Create artifacts dir + download the module cache
make configs        # Render config/<env>.json from the real Go objects in cmd/tools/codegen/configs
make build          # Compile all packages, then build artifacts/beeline with version metadata
make run ARGS="version"   # go run the CLI with arguments
make run ARGS="serve --config config/localdev.json"   # tessellate + refresh + serve HTTP on :8080
make format         # Format all Go code (imports, field alignment, tag alignment, gofmt)
make lint           # Run golangci-lint (Docker) + shellcheck
make test           # Run tests (race detector, shuffle, failfast); excludes cmd packages
```

Run a single test:
```bash
go test -run TestName ./internal/config/...
```

Linting runs in Docker (`golangci/golangci-lint` image). Formatting runs locally via `go tool` with
`gci`, `goimports`, `fieldalignment`, `tagalign`, and `gofmt` (declared in the `tool` block of go.mod).

This template does **not** vendor dependencies (platform-go's dependency tree is large); builds and
tests run against the module cache. Vendoring targets (`make vendor` / `make revendor`) exist for
consumers who want them.

## Import Ordering

Import ordering uses `gci` with four sections, separated by blank lines:

1. Standard library
2. `github.com/primandproper/beeline` (this module)
3. `github.com/primandproper` (org-level packages, including platform-go)
4. Everything else (third-party)

The Makefile `THIS` variable must be the full module path (`github.com/primandproper/beeline`)
because `format_imports.sh` runs `dirname` on it to derive the org-level prefix.

## Testing

- Tests use `stretchr/testify` (assert, require).
- Tests call `t.Parallel()` by default.
- `make test` excludes `cmd` packages, so keep testable logic in `internal/` and `version/`.
- Test command: `CGO_ENABLED=1 go test -shuffle=on -race -vet=all -failfast`.

## Conventions worth knowing

- Observability logs are structured slog written to **stdout**. `version` prints its data to stdout
  and emits nothing at the default `info` level, so `beeline version` stays machine-parseable.
- The `--log-level` / `--service-name` persistent flags default from the `BEELINE_LOG_LEVEL` and
  `BEELINE_SERVICE_NAME` environment variables. The `--config` flag (default from
  `BEELINE_CONFIG_FILEPATH`) points at a JSON config file; when set, `bootstrap` loads it via
  `config.LoadFromFile` instead of the flag/env defaults.
- Configuration is layered: defaults (or a JSON file) < `BEELINE_`-prefixed environment variables.
  Env vars follow platform-go's nested `envPrefix` tags, e.g.
  `BEELINE_OBSERVABILITY_LOGGING_LEVEL`. Give new `Config` fields both `envPrefix`/`env` and `json`
  tags so they participate in `Load` and `LoadFromFile`.
- To enable real tracing/metrics/profiling, populate the sub-configs in `internal/config` and call
  `observability.Config.NewPillars`, or swap the noop constructors in `Config.NewPillars`.

## Linting

- ~46 linters enabled via `.golangci.yml` (golangci-lint v2 format).
- Formatters: `gci` and `gofmt` (configured in the `formatters:` section).
- Notable strictness: `errcheck` (with `check-blank` + `check-type-assertions`), `errorlint`,
  `gosec`, `forcetypeassert`, `unconvert`, `unparam`. Many are relaxed for `_test.go` files.
