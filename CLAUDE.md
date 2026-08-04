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
- `serve` — a **stateless head** (design §8.2). There is exactly one backend: the freshness index,
  hot estimate store, and operator config all live in a shared Postgres, so any number of identical
  heads run at once — each is stateless and disposable, and "leader" just means "any head a follower
  points at." `matrix.backend.postgres.url` is therefore required; there is no single-node or
  in-memory mode, and no `matrix.backend.mode` (the zero-dependency build was deleted deliberately —
  one backend, one set of operational semantics, one deployment story). Heads converge on config
  changes by polling a `config_version` generation (bumped transactionally by every mutation,
  `configPollInterval`, default 2s), singleton chores are elected with Postgres advisory locks
  (janitor sweeps per-tick, boot seeding once — later heads find the working set populated and only
  re-project), and the database's `now()` is the only clock that matters for leases/staleness. The
  hot store is the one remaining choice (`hotStore`): `postgres` (default — benchmarked ~275ms p95
  for a 300k-key BatchGet, well inside the sub-second contract) or `redis` (~146ms, for batch-read
  headroom); coordination state stays in Postgres either way. At boot a head re-seeds the freshness
  index from any already-**enabled** service areas, runs a background refresh loop that keeps those
  areas' pairs fresh (plus a demand-decay janitor that evicts cold demand-filled pairs on the
  `sweepInterval` cadence), and serves the read path over HTTP. Service areas live in the database (not the config file),
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
  Postgres store: `GET` (list) / `POST` (create disabled, from a required GeoJSON polygon plus a
  `layers` list of precision levels);
  `GET`/`PATCH`/`DELETE /_config_/areas/{areaID}`; `POST …/{areaID}/enable` + `…/disable`;
  `PUT …/{areaID}/geojson` (replace geometry); `POST …/{areaID}/invalidate` (cache invalidation,
  scoped by query param — `?resolution=7` for **one precision layer**, `?profile=car`, both, or
  neither for the whole enabled area; no request body). It re-enqueues the selected pairs at the
  front of the refresh queue (via `Coordinator.Invalidate` → `FreshnessIndex.Invalidate`) and answers
  `{"area","resolution","profile","invalidated"}`. Cached estimates are deliberately **not** dropped, so
  invalidating a million-pair layer costs refresh throughput, not read latency — watch the debt spike
  and burn-down on `/_ops_/freshness`. The flip side is that it is invisible to readers: `/estimate`
  keeps serving the pre-invalidation value, and still reports `stale:false`, because the read path
  measures staleness from the *stored* estimate's age against the area TTL, not from the refresh
  schedule. A layer whose cached values must not be served again wants disable/enable, which drops them. A resolution the area has no layer for is a 400, not a silent
  no-op, and a disabled area is a 400 (it has no pairs in the index). Plus the **provider registry** under
  `/_config_/providers`: `GET` (list every provider — built-ins flagged) and
  `PUT`/`DELETE /_config_/providers/{providerName}` (upsert/remove an operator-defined provider —
  haversine or osrm spec, Postgres-backed). Updating a provider re-points enabled areas' engines live
  and changes the catalog hash so followers converge on their next claim; deleting is refused (409)
  while any area references the name, and built-ins are immutable. Unauthenticated, like the other
  endpoints; a real deploy would gate these.
- Health — `/_ops_/live` (process up) + `/_ops_/ready`, which runs a platform-go `healthcheck`
  registry populated per backend: a Postgres checker (the client's `IsReady`), a Redis checker when
  that hot store is selected. A failing dependency answers 503 with
  the component named.
- UI — `GET /` (the embedded console) and `/assets/*` (its bundled JS/CSS + vendored Leaflet/h3-js).

## Layout

- `cmd/main/main.go` — thin entrypoint: signal-cancellable context → `cli.Execute`.
- `cmd/tools/codegen/configs/` — codegen tool behind `make configs`: builds each environment's
  `*config.Config` as a real, typed Go object (`environments.go`), validates it, and renders it to
  `config/<env>.json` via `config.Render`. The checked-in JSON is a projection of these builders — edit
  the Go, never the JSON, then re-run `make configs`.
- `config/` — generated per-environment config files (`localdev.json`, `cluster.json` — the
  distributed-mode reference both roles run from in `make demo` — and `production.json`); committed so
  they stay reviewable, and loadable at runtime via `--config`. Note `cluster.json`'s `targetTTL` is
  only a **fallback**: a service area created with its own `targetTTL` (as the demo's are, at 5m)
  overrides it, and the per-area value is what the debt/throughput SQL divides by.
- `Dockerfile` + `deploy/` — the Kubernetes deployment behind `make demo`. One cgo-enabled image
  (uber/h3-go wraps the C library) serves every role, `serve` and `work` alike. `deploy/base/` is what
  beeline *is*: the head Deployment/Service/PDB, the worker Deployment, an ingress, and the *shape* of
  both autoscalers — a CPU `HorizontalPodAutoscaler` for the heads and a KEDA `ScaledObject` for the
  followers. Neither Deployment carries a `replicas` field, because an autoscaler owns it.
  `deploy/environments/local/` is what makes it a laptop demo: a k3d cluster spec, in-cluster Postgres
  and Redis, throwaway credentials, a k6 load generator, and laptop-sized bounds. Base names its
  dependencies (`beeline-postgres` Secret, `beeline-head-env`/`beeline-worker-env` ConfigMaps) and the
  environment supplies them, so pointing at managed Postgres is an overlay change and nothing more.
  Backend wiring is env-only (`BEELINE_MATRIX_BACKEND_*` over `config/cluster.json`) — but use
  `BEELINE_OBSERVABILITY_LOGGING_LEVEL`, since `BEELINE_LOG_LEVEL` is inert whenever `--config` is set.
- `scripts/seed_demo_areas.sh` — the three canonical Austin demo areas (downtown res 9 full mesh,
  city res 8 / 8 km, metro res 7+6), used by `make demo` so the polygons live in one place. It creates
  each area with `targetTTL: 5m`, which is what sets the demo's required throughput (~3.6k pairs/sec
  over a ~1.1M-pair working set) and therefore both autoscalers' arithmetic.
- `internal/cli/` — cobra root command, observability bootstrap + shutdown, subcommands
  (`version.go`, `serve.go`). `serve.go` wires the whole matrix pipeline from `application.cfg` +
  `application.pillars`.
- `internal/config/` — assembles `observability.Config` and builds the pillars (slog logging + noop
  tracing/metrics/profiling by default). See `Config.NewPillars` for the upgrade path to real telemetry.
  Two loaders use `platform-go/v9/config`: `Load` overlays `BEELINE_`-prefixed environment
  variables on the flag/default-seeded config, and `LoadFromFile` decodes a complete JSON config file
  and then overlays the same environment variables. `Render` goes the other way: it validates typed
  `Config` objects and writes them to disk (see `make configs`). The matrix service is configured by
  `MatrixConfig` (`matrix.go`), a `Config.Matrix` field (env prefix `BEELINE_MATRIX_`, JSON key
  `matrix`): HTTP server, profiles+speeds, the `backend` sub-config
  (`BackendConfig`, env prefix `BEELINE_MATRIX_BACKEND_`: `hotStore` postgres|redis,
  Postgres/Redis connection blocks — `PostgresConfig` doubles as platform-go's
  `database.ClientConfig` — and `configPollInterval`. The Postgres URL is **required**: validation
  rejects an empty one, and a `hotStore` of `memory` is rejected by name so an upgraded config fails
  loudly instead of silently pointing a head at the default DSN), and freshness knobs (`targetTTL`,
  `leaseDuration`, `sweepInterval` for the demand-decay janitor, refresh workers/batch —
  `refreshWorkers: 0` runs a coordinator-only leader), the `follower` sub-config (`FollowerConfig`,
  env prefix `BEELINE_MATRIX_FOLLOWER_`: leader URL, worker/batch/lease/backoff knobs, health port,
  outbound HTTP client, and a `retry` block (`RETRY_`/`matrix.follower.retry`) for claim/submit
  re-attempts — an omitted or partial retry block is valid, since the policy clamps every field —
  inert unless the `work` subcommand runs), plus the
  `telemetry` sub-config (`TelemetryConfig`, env prefix `BEELINE_MATRIX_TELEMETRY_`): query-event
  capture for offline demand-model training, off by default, with independently switchable raw
  (`rawEnabled`) and aggregated (`aggregateEnabled`) channels, a JSONL sink path + rotation bounds,
  and buffer/flush/bucket knobs. Service areas
  — including their per-area warm strategy, precision layers (with per-layer bounds), and demand-idle
  TTL — are not configured here; they live in the database (`internal/store/postgres`). So do routing
  **providers**: the `matrix.providers` block is seed data only, imported into the database the
  first time a leader boots against an empty `providers` table, after which the database is
  authoritative (`/_config_/providers`) and the block is inert — followers ignore it entirely and
  sync provider config from the leader.

### Matrix service packages (design §5 seams)

- `internal/beeline/` — domain model and the three pluggable interfaces: `RoutingEngine`, `Store`,
  `FreshnessIndex` (§5). Core types (`Area`, `Layer`, `AreaID`, `PairKey` — keyed by `Area` —
  `Estimate`, `Stored`, `DebtStats`, `Selector` — the scope of one `Invalidate` (age bound, area,
  precision layer, profile) — `RoutedArea`/`RoutedLayer`, `ProviderSpec`/`ProviderCatalog` —
  the persisted + wire shape of one routing provider and the content-hashed registry followers
  sync, …) and cell helpers (`Center`, `CellAt`). `H3Cell` aliases `h3.Cell`.
- `internal/geo/` — pure `Haversine(a, b)` great-circle distance.
- `internal/telemetry/` — query-event capture for offline demand-model training, composed from
  platform-go's `eventcapture` (+ `eventcapture/jsonl` sink) rather than hand-rolled. The read path
  tees every in-area fetch (cache hit/stale, same-cell, demand — H3 cells only, never coordinates) to
  a `Recorder` over a bounded never-blocking buffer (overflow drops and counts); a flusher goroutine
  writes raw `fetch` events (`WithTransform`) and/or per-(pair, time-bucket) `demand` aggregates
  (`eventcapture.Aggregator` via `WithObserver`/`WithOnFlush`) to an append-only, size-rotated JSONL
  file. This package owns only the composition: the event type, the demand counters, and the
  **frozen JSONL line shapes** offline consumers read — same keys, same order, same timestamp layout,
  and same-bucket rows ordered by origin then dest (`WithKeyOrder`). Off unless a `matrix.telemetry`
  channel is enabled; drained explicitly in `serve.go` after HTTP shutdown. The predictions flow back
  in via `POST /_ops_/warm`.
- `internal/engine/haversine/` — `RoutingEngine` implemented as Haversine ÷ per-profile speed. Swap
  a real engine in behind the interface without touching callers. Siblings: `osrm/` (an HTTP client
  against a live OSRM `/table` endpoint), `latency/` (a delay-injecting wrapper simulating a
  network-bound engine, and carrying an optional circuit breaker so a dead OSRM server sheds load
  instead of paying a client timeout per call), and `registry/` (`BuildEngine`/`BuildAll` — construct
  engines from `beeline.ProviderSpec`s — plus `BuiltinSpecs`, the synthesized haversine/latent-
  haversine specs, and `Builder`, the same construction with observability wiring: it gives each
  OSRM provider its own breaker. The free functions are a zero `Builder`, i.e. no breakers).
- `internal/tessellate/` — turns an area's geometry into its pair set: `CellsFromGeoJSON` polyfills the
  uploaded polygon via `h3.PolygonToCells` (§7), called once per layer resolution; `SamplePoint` returns
  a representative in-polygon coordinate (cheap create-time validation + radius-floor sample cells);
  `PairsFromCells` builds the directed, `AreaID`-tagged pair set from a cell set + a **travel-radius
  bound in meters** (`RingsForRadius` converts it to an H3 ring count empirically via `geo.Haversine`;
  `radiusMeters == 0` is the full-mesh sentinel).
- `internal/store/memory/` — in-memory `Store` (map + RWMutex); `DeleteArea` drops one area's estimates.
- `internal/store/postgres/` — the distributed-mode backend home (pgx/v5): `Open` (builds platform-go's
  `database.Client` over the DSN — `config.PostgresConfig` implements `database.ClientConfig`, with an
  empty read connection string so one pool serves both sides — pings once for fail-fast, then applies
  embedded migrations through `database/migrate`, serialized across booting heads by a schema-scoped
  lock key; `Pool` exposes the pgx pool the store/index/sqlc code uses natively),
  `EstimateStore` (the hot `Store`: unnest-join `BatchGet` chunked/concurrent, server-stamped
  `computed_at`), the control-plane `Repository` (areas/providers via a second sqlc target in
  `generated/`, every mutation bumping its `config_version` generation transactionally),
  `AdvisoryLocker` (beeline's names for per-area mutation locks, janitor try-lock and boot-seed lock
  over platform-go's `distributedlock` transaction-scoped Postgres `ScopedLocker`), and `pgtest/`
  (test helper: one random schema per test — `Open` for the pool, `OpenClient` for the
  `database.Client`). `pgtest` **self-provisions**: it uses `BEELINE_TEST_POSTGRES_DSN` when set,
  skips under `go test -short`, and otherwise starts one `testcontainers` Postgres per test binary
  (via platform-go's `testutils/containers`, reaped by Ryuk at process exit). A missing Docker daemon
  is a hard failure, never a silent skip — that asymmetry is deliberate and is why the distributed
  backend can no longer reach zero CI coverage unnoticed. Note this departs from platform-go's
  `RUN_CONTAINER_TESTS` convention, which defaults to skipping.
- `internal/store/redis/` — the optional Redis hot `Store` (go-redis/v9): pipelined MGET/MSET over
  fixed 24-byte binary values; only cached scalars live here, never coordination state. Its
  `redistest/` helper mirrors `pgtest`'s resolution order (`BEELINE_TEST_REDIS_ADDR` → `-short` skip
  → container) and wraps platform-go's `testutils/containers/redistest` rather than driving
  testcontainers directly. Isolation is by random area ID, not database, so one server serves every
  parallel test.
- `internal/store/storebench/` — the shared 300k-key BatchGet benchmark harness behind `make
  bench-store` (the hot-store decision gate; p50/p95 reported per backend).
- `internal/freshness/memory/` — in-memory `FreshnessIndex`: leased queue (`Claim`/`MarkComputed`,
  §8), demand `Bump`, the query-access signal `Access` (tracks a pair + stamps last-access without
  raising refresh priority), per-area demand decay `SweepArea` (evicts unpinned, unqueried pairs; `Seed`
  pins the eager core), the `Debt` signals (§3), per-area `Seed`/`Unseed` + `DebtForArea`/
  `CellStatesForArea` (each area keeps its own throughput baseline), and scoped `Invalidate`. Time comes from an injected
  `clock.Clock` (nil = wall clock); the clock-dependent tests run inside `testing/synctest` bubbles,
  where the wall clock reads bubble time.
- `internal/freshness/postgres/` — the same `FreshnessIndex` contract over shared Postgres (design
  §8.2): `Claim` is `FOR UPDATE SKIP LOCKED` over a denormalized `stale_at`, every scheduling
  timestamp comes from the database's `now()` (the `at` param of `MarkComputed` is advisory),
  `Seed` is CopyFrom + `ON CONFLICT DO NOTHING` (idempotent across racing heads), `Access` stamps
  coalesce in a bounded buffer (flushed on interval/fullness; `MarkComputed` flushes its own keys
  first inside its tx — the demand-fill ordering rule), `Bump` merges every in-flight demand write on
  the head into one upsert (`bump.go`, group commit: callers still block until their own keys land,
  so the read-your-write contract the conformance suite pins is intact, but the read path can no
  longer put one statement per request on the pool — it once wedged a demo by holding ~30 connections
  in deadlocking upserts and starving `/_config_/areas`), and every writer of `pair_freshness` orders
  its rows by the primary key so their lock acquisition is on one total order and contention degrades
  into a queue rather than a 40P01 cycle, `Invalidate` expresses every `Selector`
  scope as a nullable parameter in one statement (area+res rides the primary key's leading column)
  and purges this head's stats memo so the console sees the debt spike it just caused, and
  `Debt`/`CellStates` aggregates memoize
  ~500ms per head for console polling in platform-go `cache/memory` caches (a nil cache — `StatsCacheTTL`
  <= 0 — disables memoization, which the tests rely on). No fencing tokens by design — see §8.2.
- `internal/freshness/freshnesstest/` — the conformance suite both index implementations run
  (real-clock, window-tolerant), so the backends cannot drift apart.
- `internal/refresh/` — the worker+engine pool (§4): claim stalest → dense origin-centric 1×K table
  request → write → mark computed. Claims span all enabled areas from the shared index. The pool
  talks to a `WorkSource` seam (`Claim`/`Submit`): `LocalSource` adapts the in-process index+store
  (the leader), while `internal/follower` implements the same seam over a leader's HTTP endpoints —
  one pool implementation serves both roles. Results are **flushed incrementally**, every
  `SubmitChunk` entries (default `Batch/4`) instead of once at the end of a claim, so a long batch's
  early groups are durable and visibly fresh while its later groups are still in the engine, and a
  worker that dies mid-batch forfeits one chunk rather than everything it had computed. A failed
  flush abandons the rest of the claim — the sink is unhealthy, so computing on would only pile up
  results with nowhere to go — and the unsubmitted pairs' leases simply expire.
- `internal/follower/` — the `work` subcommand's client: implements `refresh.WorkSource` and
  `beeline.EngineResolver` against a leader's `/_work_/` endpoints, resolving each claimed area's
  `routingProvider` name against a registry **synced from the leader's provider catalog** (claim
  responses carry the catalog hash; a mismatch fetches `/_work_/providers` and rebuilds the engines
  before computing, and a failed sync fails the claim so leases expire back into the queue; unknown
  names fall back to the default engine with a once-per-name log only against pre-catalog leaders).
  Claim/submit round trips run under a `retry.Policy` (5xx and transport failures are retried, 4xx is
  terminal via `retry.Unretryable`; one claim including its catalog sync shares one budget) behind a
  circuit breaker that sheds load onto the pool's idle backoff once the leader stops answering. Also
  the follower's health endpoints (`RegisterHealth`), whose `/_ops_/ready` runs a `healthcheck`
  registry holding a leader-ping checker but maps the result back onto the frozen
  `{"status":"up"|"down"}` body.
- `internal/query/` — the read path (§9): resolves the origin to its enabled area via the `AreaRouter`
  seam, then keys the lookup against that area's partition at the **finest layer**
  (`RoutedArea.ReadLayer()`; distance-based fallthrough to coarser layers is deliberately not
  implemented yet). A demand-fill is cached and tracked only when the trip falls within that layer's
  `MaxRadiusMeters` bound (measured with `geo.Haversine`); beyond the bound — like an out-of-area
  coordinate — it is computed but not cached.
- `internal/control/` — the multi-area control plane. A `Coordinator` (backed by the Postgres
  `AreasRepository`) owns the enabled-area set and, on `Enable`/`Disable`/`Update`/`SetGeoJSON`/…,
  drives per-area seed/unseed over the `AreaIndex`/`AreaStore` seams while the refresh pool keeps
  running. It also owns the **provider registry**: built-in specs at construction plus the
  `ProvidersRepository`-persisted operator entries (`InitProviders` loads them at boot, seeding an
  empty table once from legacy `matrix.providers` file config), mutated via
  `PutProvider`/`DeleteProvider` (engines rebuilt through the injected `EngineBuilder`, enabled
  areas re-pointed live, deletes refused while referenced) and published as a content-hashed
  `beeline.ProviderCatalog` (`Catalog`/`ProvidersHash`) that followers sync from. `Enable` polyfills every layer from the area's GeoJSON and seeds by warm strategy (eager
  pins each layer's whole bound, lazy nothing, hybrid each layer's core);
  `SweepExpired` (driven by a janitor goroutine in `serve.go`) evicts cold demand pairs per area;
  `Invalidate` (behind `POST /_config_/areas/{id}/invalidate`) re-enqueues an enabled area's cached
  pairs for refresh, optionally scoped to one precision layer and/or profile — it validates the
  resolution against the area's live layer list, then hands a `beeline.Selector` to the index, and
  never touches the hot store (reschedule, not evict). It
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
  shaped via the API and rendered display-only), invalidate any one precision layer's cache from its
  row in the layers table (enabled areas only — it POSTs `…/invalidate?resolution=`, then polls so the
  debt spike shows immediately), and watch a selected area's load via
  `/_ops_/freshness?area` + `/_ops_/cells?area`. Map tiles come
  from OSM, so the basemap needs internet at runtime; cells/markers still render offline. The area
  list bounds its own fetch and retries on a backoff, because a head that accepts the request and
  never answers is otherwise indistinguishable from an empty registry — no rows, no "no areas yet"
  hint, no error — and nothing else in the poll loop ever re-lists.
- `version/` — build metadata (`CommitHash`/`BuildTime`/`CommitTime`), injected via `-ldflags` by
  `scripts/build.sh`.

## Common Commands

```bash
make setup          # Create artifacts dir + download the module cache
make configs        # Render config/<env>.json from the real Go objects in cmd/tools/codegen/configs
make sqlc           # Regenerate internal/store/postgres/generated from sqlc_queries + migrations (Docker)
make build          # Compile all packages, then build artifacts/beeline with version metadata
make run ARGS="version"   # go run the CLI with arguments
make run ARGS="serve --config config/localdev.json"   # connect to Postgres + refresh + serve on :8080
                    # (needs a reachable Postgres — `make demo` brings one up)
make docker-build   # Build beeline:demo — the one image both roles run from — with version
                    # metadata as build args. `make demo` calls this for you.
make demo           # the deployment on a local k3s cluster (k3d): Postgres (coordination) + Redis
                    # (hot store), an autoscaling pool of `serve` heads behind one Service, an
                    # autoscaling pool of `work` followers, and a k6 load generator; seeds the three
                    # Austin areas and tails debt/throughput next to live replica counts. Heads
                    # scale on CPU (metrics-server); followers scale on freshness debt read off
                    # /_ops_/freshness by KEDA's metrics-api scaler, down to zero. The seeded areas
                    # re-stale every 5m, so both pools cycle unattended — leave it running.
                    # PORT publishes the ingress; WORKER_MAX bounds the follower pool; RPS drives
                    # the load generator; KEEP_CLUSTER=true skips teardown. Needs Docker, kubectl
                    # and k3d (`brew install k3d`), no local binary. The only demo, because it is
                    # the only deployment shape.
make demo-down      # Delete a cluster left behind by KEEP_CLUSTER=true.
make format         # Format all Go code (imports, field alignment, tag alignment, gofmt)
make lint           # Run golangci-lint (Docker) + shellcheck
make test           # Run tests (race detector, shuffle, failfast) against real Postgres + Redis;
                    # excludes cmd packages. Nothing silently skips: the suite provisions its own
                    # containers, and this target starts one server of each kind up front so the
                    # four container-backed package binaries share them. Needs Docker.
                    # FAILFAST=false reports every failing package instead of stopping at the first.
make test-short     # The Docker-free fast loop: -short makes the container-backed tests skip
                    # explicitly. Leaves Postgres/Redis untested — run `make test` before pushing.
make bench-store    # The hot-store benchmark gate: 300k-key BatchGet p50/p95 for memory,
                    # Postgres, and Redis (decides the blessed distributed default). The benchmarks
                    # provision their own containers, so it needs Docker unless
                    # BEELINE_TEST_POSTGRES_DSN / BEELINE_TEST_REDIS_ADDR are exported.
```

Run a single test:
```bash
go test -run TestName ./internal/config/...
```

Bare `go test ./internal/...` also works and starts a container per package binary — slower than
`make test`, but it means an editor-driven or habitual `go test` run exercises the real backends
instead of skipping them. Add `-short` for the Docker-free path. To triage a conformance suite that
is expected to surface several failures at once:
```bash
FAILFAST=false scripts/test.sh -run TestConformance
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
- Test command: `CGO_ENABLED=1 go test -shuffle=on -race -vet=all -failfast` (drop `-failfast` with
  `FAILFAST=false`).
- **Tests never skip silently.** Container-backed tests resolve their dependency in one explicit
  order — env var, then `-short` skip, then start a container — so a green run cannot mean "the real
  backend was never exercised." Adding a new dependency-backed test means adding it to a
  `pgtest`/`redistest`-style helper, not a bare `t.Skip` on a missing env var.
- Behavioral contracts with more than one implementation get a **shared conformance suite** rather
  than per-implementation test files: `internal/freshness/freshnesstest/` is the template (one
  `Run(t, factory)` both the memory and Postgres indexes call). In-memory implementations used as
  test doubles are expected to be pinned to the real backend this way.

## Conventions worth knowing

- Observability logs are structured slog written to **stdout**. `version` prints its data to stdout
  and emits nothing at the default `info` level, so `beeline version` stays machine-parseable.
- The `--log-level` / `--service-name` persistent flags default from the `BEELINE_LOG_LEVEL` and
  `BEELINE_SERVICE_NAME` environment variables. The `--config` flag (default from
  `BEELINE_CONFIG_FILEPATH`) points at a JSON config file; when set, `bootstrap` loads it via
  `config.LoadFromFile` instead of the flag/env defaults. **`LoadFromFile` ignores `Options`
  entirely**, so with `--config` set the `--log-level` / `--service-name` flags and their
  `BEELINE_LOG_LEVEL` / `BEELINE_SERVICE_NAME` defaults are silently dropped. Deployments that pass
  `--config` (which is all of them) must use the nested `BEELINE_OBSERVABILITY_LOGGING_LEVEL`.
- Configuration is layered: defaults (or a JSON file) < `BEELINE_`-prefixed environment variables.
  Env vars follow platform-go's nested `envPrefix` tags, e.g.
  `BEELINE_OBSERVABILITY_LOGGING_LEVEL`. Give new `Config` fields both `envPrefix`/`env` and `json`
  tags so they participate in `Load` and `LoadFromFile`.
- To enable real tracing/metrics/profiling, populate the sub-configs in `internal/config` and call
  `observability.Config.NewPillars`, or swap the noop constructors in `Config.NewPillars`.
- Every `kubectl` call in `scripts/demo.sh` — and every command it prints for you to copy — names
  `--context k3d-beeline-local` explicitly. The demo never reads or changes your current context
  (`switchCurrentContext: false` in the k3d config), so whatever cluster you were pointed at stays
  the one you are pointed at. Keep it that way when adding commands.

## Linting

- ~46 linters enabled via `.golangci.yml` (golangci-lint v2 format).
- Formatters: `gci` and `gofmt` (configured in the `formatters:` section).
- Notable strictness: `errcheck` (with `check-blank` + `check-type-assertions`), `errorlint`,
  `gosec`, `forcetypeassert`, `unconvert`, `unparam`. Many are relaxed for `_test.go` files.
