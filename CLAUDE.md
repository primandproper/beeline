# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`github.com/primandproper/beeline` — a self-hostable server that precomputes travel-time/distance
matrices between H3 cells and serves cached scalar estimates under a freshness contract. Built on
[`github.com/primandproper/platform-go`](https://github.com/primandproper/platform-go). Go 1.26.
See `beeline-design.md` for the full design; section references (§) below point into it.

The application is a **Cobra CLI**. Three subcommands:

- `version` — prints build metadata to stdout.
- `work` — the follower half of the leader/follower split (design §8): the same binary pointed at a
  running `serve` instance (`--leader <url>`, `BEELINE_MATRIX_FOLLOWER_LEADER_URL`, or
  `matrix.follower.leaderURL`). It claims pending pairs from the leader over `POST /_work_/claim`,
  computes them with routing engines built from the **leader's provider catalog** (every claim
  response carries the catalog's content hash; an unfamiliar hash makes the follower fetch
  `GET /_work_/providers` and rebuild its engines — speeds included — before computing, so a
  follower needs no provider config of its own, and a failed sync fails the claim rather than
  compute with stale engines), and
  submits the scalars back over `POST /_work_/submit`; the leader's leased freshness index is the
  only coordination, so followers are stateless — one that dies just lets its leases expire. A
  follower serves only health probes (`/_ops_/live`, and `/_ops_/ready` = leader reachable) on
  `matrix.follower.port` (default 8081). Scale a saturated leader by starting more `work` processes;
  a leader with `refreshWorkers: 0` computes nothing itself (pure coordinator).
- `serve` — the prototype. Runs in one of two backend modes (`matrix.backend`, design §8.2): the
  zero-dependency **single-node default** described below, or **distributed mode** (`mode:
  "postgres"`), where the freshness index, hot estimate store, and operator config all live in a
  shared Postgres so any number of identical `serve` **heads** run at once — each head is stateless
  and disposable, "leader" just means "any head a follower points at." In distributed mode SQLite is
  ignored; heads converge on config changes by polling a `config_version` generation (bumped
  transactionally by every mutation, `configPollInterval`, default 2s), singleton chores are elected
  with Postgres advisory locks (janitor sweeps per-tick, boot seeding once — later heads find the
  working set populated and only re-project), and the database's `now()` is the only clock that
  matters for leases/staleness. The hot store is selectable (`hotStore`): `postgres` (default —
  benchmarked ~275ms p95 for a 300k-key BatchGet, well inside the sub-second contract) or `redis`
  (~146ms, for batch-read headroom). Single-node it opens the SQLite **area store**, re-seeds the freshness index from any
  already-**enabled** service areas, runs a background refresh loop that keeps those areas' pairs
  fresh (plus a demand-decay janitor that evicts cold demand-filled pairs on the `sweepInterval`
  cadence), and serves the read path over HTTP. Service areas live in the database (not the config file),
  are **disabled by default**, and only enter the working set once enabled — so a fresh database boots
  with **no areas** and nothing to refresh until the operator creates and enables one. An area is a
  **GeoJSON polygon plus an ordered list of precision layers** (DoorDash-style multi-resolution):
  each layer `{resolution, minDistanceMeters, maxRadiusMeters, coreRadiusMeters}` polyfills the
  polygon at its resolution and is precomputed/cached independently (`PairKey.Res` keeps layers
  apart); reads key at the finest layer (`minDistanceMeters` is recorded for future distance-based
  layer selection, not yet used). Multiple areas can be enabled at once; each partitions the shared
  store/index by its `AreaID`. The routing engine is
  a **Haversine** stand-in (great-circle distance ÷ per-profile speed) behind the same `RoutingEngine`
  interface a real engine (OSRM/Valhalla) would implement. The hot store and freshness index are
  in-memory (only area *definitions* are persisted). `serve` also serves an **embedded operator
  console** at `/` (see `internal/webui/`) for creating areas from GeoJSON, enabling them, and
  watching the cache load.

HTTP endpoints (default `:8080`):

- Read path — `GET /estimate?origin=lat,lng&dest=lat,lng&profile=car`. Routes the origin to the
  enabled area that contains it (cache hit, same-cell correction, or demand-fill against that area's
  partition, keyed at the area's finest layer); a demand-fill is only cached when the trip is within
  that layer's `maxRadiusMeters` bound. A coordinate outside every enabled area — or a trip beyond the bound — is
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
  `GET /_ops_/cells` (per-origin-cell freshness rollup — `cell`/`area`/`resolution`/center/
  `total`/`fresh`/`oldestAgeSeconds` — for one area with `?area=<id>` or all enabled areas
  otherwise; a multi-layer area's cells arrive at several resolutions side by side), and
  `POST /_ops_/pairs` (the console's hover probe: a pure cache read of one origin cell's
  estimates to a list of same-resolution destination cells, keyed at the cells' own
  resolution — unlike `/estimate`/`/table`, which key at the finest layer — so any layer's
  cached pairs are inspectable), and `POST /_ops_/warm` (the model-feed ingestion point: a JSON
  body of `area` + `pairs` (hex H3 origin/dest cells at the area's finest resolution, optional
  `profile`) pushes predicted demand into the freshness index via `Coordinator.WarmPairs` —
  `mode:"bump"` (default) as decayable demand at top refresh priority, `mode:"seed"` pinned
  against the demand sweep).
- Work distribution — `POST /_work_/claim` (lease up to `batchSize` due pairs — hex H3 cells +
  area/profile/res — with per-area `routingProvider` metadata and the provider catalog's
  `providersHash`; zero `batchSize`/`leaseSeconds` fall
  back to `refreshBatch`/`leaseDuration`, both capped), `GET /_work_/providers` (the full provider
  **catalog**: every spec — built-ins included — plus the profile speed map, stamped with its content
  hash; followers fetch it at startup and on any hash change), and `POST /_work_/submit` (write a
  follower's
  computed estimates: `store.Put` + `MarkComputed`, stamped with the **leader's** clock; results for
  since-disabled areas are dropped). Idempotent by construction — duplicate submits and expired-lease
  submits are waste, never corruption.
- Control plane — the area registry under `/_config_/areas`, served by `internal/control` over the
  SQLite store: `GET` (list) / `POST` (create disabled, from a required GeoJSON polygon plus a
  `layers` list of precision levels);
  `GET`/`PATCH`/`DELETE /_config_/areas/{areaID}`; `POST …/{areaID}/enable` + `…/disable`;
  `PUT …/{areaID}/geojson` (replace geometry). Plus the **provider registry** under
  `/_config_/providers`: `GET` (list every provider — built-ins flagged) and
  `PUT`/`DELETE /_config_/providers/{providerName}` (upsert/remove an operator-defined provider —
  haversine or osrm spec, SQLite-backed). Updating a provider re-points enabled areas' engines live
  and changes the catalog hash so followers converge on their next claim; deleting is refused (409)
  while any area references the name, and built-ins are immutable. Unauthenticated, like the other
  endpoints; a real deploy would gate these.
- Health — `/_ops_/live` + `/_ops_/ready`.
- UI — `GET /` (the embedded console) and `/assets/*` (its bundled JS/CSS + vendored Leaflet/h3-js).

## Layout

- `cmd/main/main.go` — thin entrypoint: signal-cancellable context → `cli.Execute`.
- `cmd/tools/codegen/configs/` — codegen tool behind `make configs`: builds each environment's
  `*config.Config` as a real, typed Go object (`environments.go`), validates it, and renders it to
  `config/<env>.json` via `config.Render`. The checked-in JSON is a projection of these builders — edit
  the Go, never the JSON, then re-run `make configs`.
- `config/` — generated per-environment config files (`localdev.json`, `cluster.json` — the
  distributed-mode reference, used by `make fulldemo` — and `production.json`); committed so
  they stay reviewable, and loadable at runtime via `--config`.
- `Dockerfile` + `docker-compose.yml` — the containerized deployment behind `make fulldemo`: one
  cgo-enabled image (uber/h3-go wraps the C library) serving every role, and a compose cluster of
  Postgres + Redis + 3 `serve` heads + 8 `work` followers. Only `head-a` carries the build block (four
  services declaring one tag makes buildx race); the heads share a `heads` network alias so followers
  spread across the pool. Backend wiring is env-only (`BEELINE_MATRIX_BACKEND_*` over
  `config/cluster.json`).
- `scripts/seed_demo_areas.sh` — the three canonical Austin demo areas (downtown res 9 full mesh,
  city res 8 / 8 km, metro res 7+6), shared by all three demo scripts so the polygons live in one place.
- `internal/cli/` — cobra root command, observability bootstrap + shutdown, subcommands
  (`version.go`, `serve.go`). `serve.go` wires the whole matrix pipeline from `application.cfg` +
  `application.pillars`.
- `internal/config/` — assembles `observability.Config` and builds the pillars (slog logging + noop
  tracing/metrics/profiling by default). See `Config.NewPillars` for the upgrade path to real telemetry.
  Two loaders use `platform-go/v7/config`: `Load` overlays `BEELINE_`-prefixed environment
  variables on the flag/default-seeded config, and `LoadFromFile` decodes a complete JSON config file
  and then overlays the same environment variables. `Render` goes the other way: it validates typed
  `Config` objects and writes them to disk (see `make configs`). The matrix service is configured by
  `MatrixConfig` (`matrix.go`), a `Config.Matrix` field (env prefix `BEELINE_MATRIX_`, JSON key
  `matrix`): HTTP server, the SQLite `databasePath`, profiles+speeds, the `backend` sub-config
  (`BackendConfig`, env prefix `BEELINE_MATRIX_BACKEND_`: `mode` memory|postgres, `hotStore`
  postgres|redis, Postgres/Redis connection blocks, `configPollInterval` — the distributed-mode
  switch; zero value = today's single-node behavior), and freshness knobs (`targetTTL`,
  `leaseDuration`, `sweepInterval` for the demand-decay janitor, refresh workers/batch —
  `refreshWorkers: 0` runs a coordinator-only leader), the `follower` sub-config (`FollowerConfig`,
  env prefix `BEELINE_MATRIX_FOLLOWER_`: leader URL, worker/batch/lease/backoff knobs, health port,
  outbound HTTP client — inert unless the `work` subcommand runs), plus the
  `telemetry` sub-config (`TelemetryConfig`, env prefix `BEELINE_MATRIX_TELEMETRY_`): query-event
  capture for offline demand-model training, off by default, with independently switchable raw
  (`rawEnabled`) and aggregated (`aggregateEnabled`) channels, a JSONL sink path + rotation bounds,
  and buffer/flush/bucket knobs. Service areas
  — including their per-area warm strategy, precision layers (with per-layer bounds), and demand-idle
  TTL — are not configured here; they live in the database (`internal/store/sqlite`). So do routing
  **providers**: the `matrix.providers` block is seed data only, imported into the database the
  first time a leader boots against an empty `providers` table, after which the database is
  authoritative (`/_config_/providers`) and the block is inert — followers ignore it entirely and
  sync provider config from the leader.

### Matrix service packages (design §5 seams)

- `internal/beeline/` — domain model and the three pluggable interfaces: `RoutingEngine`, `Store`,
  `FreshnessIndex` (§5). Core types (`Area`, `Layer`, `AreaID`, `PairKey` — keyed by `Area` —
  `Estimate`, `Stored`, `DebtStats`, `RoutedArea`/`RoutedLayer`, `ProviderSpec`/`ProviderCatalog` —
  the persisted + wire shape of one routing provider and the content-hashed registry followers
  sync, …) and cell helpers (`Center`, `CellAt`). `H3Cell` aliases `h3.Cell`.
- `internal/geo/` — pure `Haversine(a, b)` great-circle distance.
- `internal/telemetry/` — query-event capture for offline demand-model training. The read path tees
  every in-area fetch (cache hit/stale, same-cell, demand — H3 cells only, never coordinates) to a
  `Recorder` over a bounded never-blocking buffer (overflow drops and counts); a flusher goroutine
  writes raw `fetch` events and/or per-(pair, time-bucket) `demand` aggregates through the pluggable
  `Sink` seam (`JSONLSink` today: append-only, size-rotated). Off unless a `matrix.telemetry` channel
  is enabled; drained explicitly in `serve.go` after HTTP shutdown. The predictions flow back in via
  `POST /_ops_/warm`.
- `internal/engine/haversine/` — `RoutingEngine` implemented as Haversine ÷ per-profile speed. Swap
  a real engine in behind the interface without touching callers. Siblings: `osrm/` (an HTTP client
  against a live OSRM `/table` endpoint), `latency/` (a delay-injecting wrapper simulating a
  network-bound engine), and `registry/` (`BuildEngine`/`BuildAll` — construct engines from
  `beeline.ProviderSpec`s — plus `BuiltinSpecs`, the synthesized haversine/latent-haversine specs).
- `internal/tessellate/` — turns an area's geometry into its pair set: `CellsFromGeoJSON` polyfills the
  uploaded polygon via `h3.PolygonToCells` (§7), called once per layer resolution; `SamplePoint` returns
  a representative in-polygon coordinate (cheap create-time validation + radius-floor sample cells);
  `PairsFromCells` builds the directed, `AreaID`-tagged pair set from a cell set + a **travel-radius
  bound in meters** (`RingsForRadius` converts it to an H3 ring count empirically via `geo.Haversine`;
  `radiusMeters == 0` is the full-mesh sentinel).
- `internal/store/memory/` — in-memory `Store` (map + RWMutex); `DeleteArea` drops one area's estimates.
- `internal/store/postgres/` — the distributed-mode backend home (pgx/v5): `Open` (pool + embedded
  goose migrations, serialized across booting heads by a schema-scoped advisory lock),
  `EstimateStore` (the hot `Store`: unnest-join `BatchGet` chunked/concurrent, server-stamped
  `computed_at`), the control-plane `Repository` (areas/providers via a second sqlc target in
  `generated/`, every mutation bumping its `config_version` generation transactionally),
  `AdvisoryLocker` (per-area mutation locks, janitor try-lock, boot-seed lock), and `pgtest/` (test
  helper: one random schema per test, gated on `BEELINE_TEST_POSTGRES_DSN`).
- `internal/store/redis/` — the optional Redis hot `Store` (go-redis/v9): pipelined MGET/MSET over
  fixed 24-byte binary values; only cached scalars live here, never coordination state. Gated tests
  on `BEELINE_TEST_REDIS_ADDR`.
- `internal/store/storebench/` — the shared 300k-key BatchGet benchmark harness behind `make
  bench-store` (the hot-store decision gate; p50/p95 reported per backend).
- `internal/store/sqlite/` — the persistent **area store** (`modernc.org/sqlite`, pure-Go): a
  `Repository` over sqlc-generated queries (`generated/`, regenerate with `make sqlc`) and embedded
  goose migrations (`migrations/`). Stores area definitions (name, `warm_strategy`,
  `demand_idle_ttl_seconds`, GeoJSON, enabled flag, plus the `area_layers` child table — one row per
  precision layer) and the operator-defined **provider registry** (`providers` table — one
  `beeline.ProviderSpec` per row; built-ins are synthesized, never stored) — not the computed matrix
  and not cells (cells are derived by polyfill at seed time).
- `internal/freshness/memory/` — in-memory `FreshnessIndex`: leased queue (`Claim`/`MarkComputed`,
  §8), demand `Bump`, the query-access signal `Access` (tracks a pair + stamps last-access without
  raising refresh priority), per-area demand decay `SweepArea` (evicts unpinned, unqueried pairs; `Seed`
  pins the eager core), the `Debt` signals (§3), and per-area `Seed`/`Unseed` + `DebtForArea`/
  `CellStatesForArea` (each area keeps its own throughput baseline). Clock is injectable for tests.
- `internal/freshness/postgres/` — the same `FreshnessIndex` contract over shared Postgres (design
  §8.2): `Claim` is `FOR UPDATE SKIP LOCKED` over a denormalized `stale_at`, every scheduling
  timestamp comes from the database's `now()` (the `at` param of `MarkComputed` is advisory),
  `Seed` is CopyFrom + `ON CONFLICT DO NOTHING` (idempotent across racing heads), `Access` stamps
  coalesce in a bounded buffer (flushed on interval/fullness; `MarkComputed` flushes its own keys
  first inside its tx — the demand-fill ordering rule), and `Debt`/`CellStates` aggregates memoize
  ~500ms per head for console polling. No fencing tokens by design — see §8.2.
- `internal/freshness/freshnesstest/` — the conformance suite both index implementations run
  (real-clock, window-tolerant), so the backends cannot drift apart.
- `internal/refresh/` — the worker+engine pool (§4): claim stalest → dense origin-centric 1×K table
  request → write → mark computed. Claims span all enabled areas from the shared index. The pool
  talks to a `WorkSource` seam (`Claim`/`Submit`): `LocalSource` adapts the in-process index+store
  (the leader), while `internal/follower` implements the same seam over a leader's HTTP endpoints —
  one pool implementation serves both roles.
- `internal/follower/` — the `work` subcommand's client: implements `refresh.WorkSource` and
  `beeline.EngineResolver` against a leader's `/_work_/` endpoints, resolving each claimed area's
  `routingProvider` name against a registry **synced from the leader's provider catalog** (claim
  responses carry the catalog hash; a mismatch fetches `/_work_/providers` and rebuilds the engines
  before computing, and a failed sync fails the claim so leases expire back into the queue; unknown
  names fall back to the default engine with a once-per-name log only against pre-catalog leaders).
  Also the follower's health endpoints (`RegisterHealth`).
- `internal/query/` — the read path (§9): resolves the origin to its enabled area via the `AreaRouter`
  seam, then keys the lookup against that area's partition at the **finest layer**
  (`RoutedArea.ReadLayer()`; distance-based fallthrough to coarser layers is deliberately not
  implemented yet). A demand-fill is cached and tracked only when the trip falls within that layer's
  `MaxRadiusMeters` bound (measured with `geo.Haversine`); beyond the bound — like an out-of-area
  coordinate — it is computed but not cached.
- `internal/control/` — the multi-area control plane. A `Coordinator` (backed by the SQLite
  `AreasRepository`) owns the enabled-area set and, on `Enable`/`Disable`/`Update`/`SetGeoJSON`/…,
  drives per-area seed/unseed over the `AreaIndex`/`AreaStore` seams while the refresh pool keeps
  running. It also owns the **provider registry**: built-in specs at construction plus the
  `ProvidersRepository`-persisted operator entries (`InitProviders` loads them at boot, seeding an
  empty table once from legacy `matrix.providers` file config), mutated via
  `PutProvider`/`DeleteProvider` (engines rebuilt through the injected `EngineBuilder`, enabled
  areas re-pointed live, deletes refused while referenced) and published as a content-hashed
  `beeline.ProviderCatalog` (`Catalog`/`ProvidersHash`) that followers sync from. `Enable` polyfills every layer from the area's GeoJSON and seeds by warm strategy (eager
  pins each layer's whole bound, lazy nothing, hybrid each layer's core);
  `SweepExpired` (driven by a janitor goroutine in `serve.go`) evicts cold demand pairs per area. It
  also implements `query.AreaRouter` (`Locate` — containment is the finest layer's cell set; the
  returned `RoutedArea` carries the full layer list finest→coarsest). `serve.go` calls
  `ResumeEnabled` at boot.
- `internal/httpapi/` — HTTP routes on platform-go's typed OpenAPI router over the chi backend
  (read path, freshness, cells, the `/_config_/areas` registry, health). Handlers are typed
  (`routing.Handler[In, Out]`; path/query params bind from struct tags), which generates
  `/openapi.json` + a `/docs` browser UI for free — note `/docs` loads Stoplight Elements from a
  CDN, unlike the fully-embedded operator console. The wire format predates the typed router and
  is preserved exactly: `router.go` builds the router with enveloping off over `encoder.go`'s
  lenient JSON codec, and error paths bypass the framework's APIError envelope through `wire.go`'s
  committable-writer escape hatch (`fail`/`commitJSON`), keeping flat `{"error": …}` bodies and
  statuses the platform can't produce (409). Handlers therefore return nil errors on purpose
  (nilerr is excluded for this package).
- `internal/webui/` — the embedded single-page operator console (`go:embed static`): Leaflet + h3-js
  (vendored under `static/assets/vendor/`, no CDN or build step). List/enable/disable areas, create a
  one-layer area from an uploaded GeoJSON polygon (client-side polyfill preview; multi-layer areas are
  shaped via the API and rendered display-only), and watch a selected area's load via
  `/_ops_/freshness?area` + `/_ops_/cells?area`. Map tiles come
  from OSM, so the basemap needs internet at runtime; cells/markers still render offline.
- `version/` — build metadata (`CommitHash`/`BuildTime`/`CommitTime`), injected via `-ldflags` by
  `scripts/build.sh`.

## Common Commands

```bash
make setup          # Create artifacts dir + download the module cache
make configs        # Render config/<env>.json from the real Go objects in cmd/tools/codegen/configs
make sqlc           # Regenerate both sqlc targets (sqlite + postgres generated/) from sqlc_queries + migrations (Docker)
make build          # Compile all packages, then build artifacts/beeline with version metadata
make run ARGS="version"   # go run the CLI with arguments
make run ARGS="serve --config config/localdev.json"   # open area store + refresh + serve HTTP on :8080
make simpledemo     # one process, no dependencies: fresh gitignored SQLite db (artifacts/demo.db),
                    # the three Austin demo areas auto-seeded & enabled against the in-process
                    # haversine engine, then serve. PORT=n to move it.
make clusterdemo    # leader/follower live: coordinator-only leader (refreshWorkers=0) + 3 `work`
                    # followers against the latency-simulated engine; tails /_ops_/freshness.
                    # FOLLOWERS=n to scale.
make fulldemo       # the whole distributed deployment as a docker-compose cluster: Postgres
                    # (coordination) + Redis (hot store) wired into a pool of 3 serve heads
                    # (:8080/:8090/:8100) and a pool of 8 `work` followers; seeds via head A and tails
                    # all three heads. Stop a head and the survivors keep the contract. WORKERS=n to
                    # scale the follower pool; needs Docker (Compose v2), no local binary.
make format         # Format all Go code (imports, field alignment, tag alignment, gofmt)
make lint           # Run golangci-lint (Docker) + shellcheck
make test           # Run tests (race detector, shuffle, failfast); excludes cmd packages
make test-integration  # Same suite with real Postgres+Redis containers, so the env-gated
                       # integration tests execute instead of skipping. Needs Docker.
make bench-store    # The hot-store benchmark gate: 300k-key BatchGet p50/p95 for memory,
                    # Postgres, and Redis (decides the blessed distributed default). Needs Docker.
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
