# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`github.com/primandproper/beeline` — a self-hostable server that precomputes travel-time/distance
matrices between H3 cells and serves cached scalar estimates under a freshness contract. Built on
[`github.com/primandproper/platform-go`](https://github.com/primandproper/platform-go). Go 1.26.
See `beeline-design.md` for the full design; section references (§) below point into it.

The application is a **Cobra CLI**. Two subcommands:

- `version` — prints build metadata to stdout.
- `serve` — the prototype. It opens the SQLite **area store**, re-seeds the freshness index from any
  already-**enabled** service areas, runs a background refresh loop that keeps those areas' pairs
  fresh (plus a demand-decay janitor that evicts cold demand-filled pairs on the `sweepInterval`
  cadence), and serves the read path over HTTP. Service areas live in the database (not the config file),
  are **disabled by default**, and only enter the working set once enabled — so a fresh database boots
  with **no areas** and nothing to refresh until the operator creates and enables one. Multiple areas
  can be enabled at once; each partitions the shared store/index by its `AreaID`. The routing engine is
  a **Haversine** stand-in (great-circle distance ÷ per-profile speed) behind the same `RoutingEngine`
  interface a real engine (OSRM/Valhalla) would implement. The hot store and freshness index are
  in-memory (only area *definitions* are persisted). `serve` also serves an **embedded operator
  console** at `/` (see `internal/webui/`) for creating areas from GeoJSON, enabling them, refining
  hexes, and watching the cache load.

HTTP endpoints (default `:8080`):

- Read path — `GET /estimate?origin=lat,lng&dest=lat,lng&profile=car`. Routes the origin to the
  enabled area that contains it (cache hit, same-cell correction, or demand-fill against that area's
  partition/resolution); a demand-fill is only cached when the trip is within the area's
  `maxRadiusMeters` bound. A coordinate outside every enabled area — or a trip beyond the bound — is
  still answered directly but not cached.
- Batch read path — `POST /table`. A sparse, OSRM-`/table`-shaped batch: a JSON body of `sources` and
  `destinations` (`"lat,lng"` strings) plus an optional `skip` denylist of `[sourceIdx, destIdx]` grid
  cells and a `fill` flag (default `true`). It applies the same per-pair rules as `/estimate` but
  batched — the whole grid's cache lookups are one `Store.BatchGet`, and (when `fill`) misses are
  computed with at most one dense 1×K engine call per source coordinate (grouping like the refresh
  pool). Response is dense `durations`/`distances` matrices (`null` for a skipped cell, or an
  uncomputed miss when `fill=false`) plus a `meta` rollup (`hits`/`misses`/`filled`/`skipped`/…).
  `fill=false` is a pure cache read that never touches the engine. `maxTableCells` bounds the grid.
- Freshness/progress — `GET /_ops_/freshness` (the §3 debt/throughput contract as JSON, wire shape of
  `beeline.DebtStats`; aggregate across enabled areas, or one area with `?area=<id>`) and
  `GET /_ops_/cells` (per-origin-cell freshness rollup — `cell`/`area`/center/`total`/`fresh`/
  `oldestAgeSeconds` — for one area with `?area=<id>` or all enabled areas otherwise).
- Control plane — the area registry under `/_config_/areas`, served by `internal/control` over the
  SQLite store: `GET` (list) / `POST` (create disabled, from a GeoJSON polygon or explicit cells);
  `GET`/`PATCH`/`DELETE /_config_/areas/{areaID}`; `POST …/{areaID}/enable` + `…/disable`;
  `PUT …/{areaID}/geojson` (replace geometry); `POST …/{areaID}/cells` (`{add,remove}` hex
  refinement). Unauthenticated, like the other endpoints; a real deploy would gate these.
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
  `matrix`): HTTP server, the SQLite `databasePath`, profiles+speeds, and freshness knobs (`targetTTL`,
  `leaseDuration`, `sweepInterval` for the demand-decay janitor, refresh workers/batch). Service areas
  — including their per-area warm strategy, radius bound, and demand-idle TTL — are not configured here;
  they live in the database (`internal/store/sqlite`).

### Matrix service packages (design §5 seams)

- `internal/beeline/` — domain model and the three pluggable interfaces: `RoutingEngine`, `Store`,
  `FreshnessIndex` (§5). Core types (`Area`, `AreaID`, `PairKey` — keyed by `Area` — `Estimate`,
  `Stored`, `DebtStats`, `RoutedArea`, …) and cell helpers (`Center`, `CellAt`). `H3Cell` aliases
  `h3.Cell`.
- `internal/geo/` — pure `Haversine(a, b)` great-circle distance.
- `internal/engine/haversine/` — `RoutingEngine` implemented as Haversine ÷ per-profile speed. Swap
  a real engine in behind the interface without touching callers.
- `internal/tessellate/` — turns an area's geometry into its pair set: `CellsFromGeoJSON` polyfills an
  uploaded polygon via `h3.PolygonToCells` (§7); `PairsFromCells` builds the directed, `AreaID`-tagged
  pair set from an explicit cell set + a **travel-radius bound in meters** (`RingsForRadius` converts it
  to an H3 ring count empirically via `geo.Haversine`; `radiusMeters == 0` is the full-mesh sentinel).
  Roadless cells (water, private land) are carved out by hand from the console, not by an automated mask.
- `internal/store/memory/` — in-memory `Store` (map + RWMutex); `DeleteArea` drops one area's estimates.
- `internal/store/sqlite/` — the persistent **area store** (`modernc.org/sqlite`, pure-Go): a
  `Repository` over sqlc-generated queries (`generated/`, regenerate with `make sqlc`) and embedded
  goose migrations (`migrations/`). Stores area definitions (name, resolution, `radius_meters`,
  `warm_strategy`, `core_radius_meters`, `demand_idle_ttl_seconds`, cell set, GeoJSON, enabled flag) —
  not the computed matrix.
- `internal/freshness/memory/` — in-memory `FreshnessIndex`: leased queue (`Claim`/`MarkComputed`,
  §8), demand `Bump`, the query-access signal `Access` (tracks a pair + stamps last-access without
  raising refresh priority), per-area demand decay `SweepArea` (evicts unpinned, unqueried pairs; `Seed`
  pins the eager core), the `Debt` signals (§3), and per-area `Seed`/`Unseed` + `DebtForArea`/
  `CellStatesForArea` (each area keeps its own throughput baseline). Clock is injectable for tests.
- `internal/refresh/` — the worker+engine pool (§4): claim stalest → dense origin-centric 1×K table
  request → write → mark computed. Claims span all enabled areas from the shared index.
- `internal/query/` — the read path (§9): resolves the origin to its enabled area via the `AreaRouter`
  seam, then keys the lookup against that area's partition/resolution (cache hit, same-cell correction,
  demand-fill). A demand-fill is cached and tracked only when the trip falls within the area's
  `MaxRadiusMeters` bound (measured with `geo.Haversine`); beyond the bound — like an out-of-area
  coordinate — it is computed but not cached.
- `internal/control/` — the multi-area control plane. A `Coordinator` (backed by the SQLite
  `AreasRepository`) owns the enabled-area set and, on `Enable`/`Disable`/`AddCells`/`SetGeoJSON`/…,
  drives per-area seed/unseed over the `AreaIndex`/`AreaStore` seams while the refresh pool keeps
  running. `Enable` seeds by warm strategy (eager pins the whole bound, lazy nothing, hybrid the core);
  `SweepExpired` (driven by a janitor goroutine in `serve.go`) evicts cold demand pairs per area. It
  also implements `query.AreaRouter` (`Locate`). `serve.go` calls `ResumeEnabled` at boot.
- `internal/httpapi/` — HTTP routes registered on the platform-go chi router (read path, freshness,
  cells, the `/_config_/areas` registry, health). `{areaID}` params via the router's param manager.
- `internal/webui/` — the embedded single-page operator console (`go:embed static`): Leaflet + h3-js
  (vendored under `static/assets/vendor/`, no CDN or build step). List/enable/disable areas, create one
  from an uploaded GeoJSON polygon (client-side polyfill preview), refine hexes by clicking the map,
  and watch a selected area's load via `/_ops_/freshness?area` + `/_ops_/cells?area`. Map tiles come
  from OSM, so the basemap needs internet at runtime; cells/markers still render offline.
- `version/` — build metadata (`CommitHash`/`BuildTime`/`CommitTime`), injected via `-ldflags` by
  `scripts/build.sh`.

## Common Commands

```bash
make setup          # Create artifacts dir + download the module cache
make configs        # Render config/<env>.json from the real Go objects in cmd/tools/codegen/configs
make sqlc           # Regenerate internal/store/sqlite/generated from sqlc_queries + migrations (Docker)
make build          # Compile all packages, then build artifacts/beeline with version metadata
make run ARGS="version"   # go run the CLI with arguments
make run ARGS="serve --config config/localdev.json"   # open area store + refresh + serve HTTP on :8080
make demo           # fresh gitignored SQLite db (artifacts/demo.db) + auto-seed & enable a demo area, then serve
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
