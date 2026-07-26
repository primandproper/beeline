# Platform-go gap analysis

Beeline exists partly to prove out `github.com/primandproper/platform-go` and find its gaps. This
document is that audit: how thoroughly beeline uses platform today, where it hand-rolls things
platform already provides, where platform's offering exists but doesn't fit, and which packages
platform doesn't have yet that beeline now articulates a concrete use case for.

**Version caveat up front:** beeline pins `platform-go/v4 v4.1.0` (`go.mod:8`); the platform repo
is at `/v7`. This audit evaluates against the current `/v7` checkout, so some "should adopt" items
may require the v4→v7 upgrade first. The upgrade itself is arguably finding #0 — a proving-ground
repo three major versions behind the platform it's proving isn't exercising much.

---

## 1. Current usage snapshot

Beeline imports **12 distinct platform-go packages** out of ~45. By depth:

| Seam | Depth | Notes |
|---|---|---|
| `observability/logging` | Deep | 17 import sites; the one truly pervasive platform seam. |
| `config` (`ApplyEnvironmentVariables`, `LoadFromJSONFile`) | Deep | `internal/config/config.go:106-156`; env-prefix layering used correctly, plus `Render` built on top. |
| `server/http` + `routing`/`routing/chi` | Medium | Real router/server stack in `serve.go:179-216` and `work.go:117-138`, including `chirouter.NewRouteParamManager`. But all handlers are raw `http.HandlerFunc`s — none of the typed-handler/OpenAPI machinery. |
| `observability` (Pillars) | Shallow | Bootstrap exists (`cli/root.go:90-117`) but `NewPillars` hard-codes noop tracing/metrics/profiling (`config/config.go:175-187`). Providers are threaded all the way to the router and server, then never emit anything. |
| `healthcheck` | Shallow | A `Registry` drives `/_ops_/ready` (`httpapi/routes.go:1061-1078`) — with **zero checkers registered** (`cli/serve.go:195`). |
| `httpclient` | Shallow | `FollowerConfig.HTTP` embeds `httpclient.Config` and calls `BuildClient()` (`cli/work.go:80-86`). |

Notably absent everywhere: `database`, `distributedlock`, `cache`, `retry`, `encoding`, `errors`,
`errors/http`, `circuitbreaking`, `ratelimiting`, `testutils`, `version`, `identifiers`,
`cryptography/hashing`, `analytics`, `messagequeue`.

---

## 2. Should adopt: platform has it, beeline hand-rolls it

Ordered by (value ÷ effort), highest first.

### 2.1 `errors` + `errors/http` — kill the string-matched error contract

The sharpest single symptom in the codebase: `httpapi.isNotFound` matches on
`strings.Contains(err.Error(), "not found")` (`routes.go:1101-1106`), and **both** store packages
encode that contract in their sentinel message text (`store/sqlite/areas.go:15`,
`store/postgres/areas.go:19`). No `errors.Is`, no shared taxonomy. Every error response is a flat
hand-built `{"error": "..."}` (`writeError`, `routes.go:1223-1225`), duplicated again in
`follower/health.go:41-47`.

Platform's `errors` (cockroachdb re-export + sentinels) and `errors/http` (`APIError`,
`ToAPIResponse`, `HTTPStatusForCode`, pluggable mapper) cover exactly this. Adopting them removes
the cross-package string coupling and gives the API a structured error shape for free.

### 2.2 `healthcheck` checkers — the registry is empty

`/_ops_/ready` on a distributed-mode head cannot fail today even with Postgres and Redis down,
because `cli/serve.go:195` passes a bare `NewRegistry()`. Platform ships
`NewDatabaseChecker`/`NewCacheChecker` adapters purpose-built for this. The follower side is worse:
`follower/health.go` reimplements live/ready endpoints bespoke, with its own JSON writer, using
none of `healthcheck`. One afternoon of work makes readiness real on both roles.

### 2.3 `encoding.ServerEncoderDecoder` — the HTTP response/request layer

`writeJSON`/`decodeJSON` (`routes.go:1177-1221`) reimplement `EncodeResponseWithStatus` /
`DecodeRequest`. Beeline's versions have real bugs platform's don't: `decodeJSON`'s comment claims
strict decoding but never calls `DisallowUnknownFields`, and the 8 MiB GeoJSON body cap is reused
as the limit for every endpoint including `/_work_/claim`. The router already accepts an encoder —
beeline just never constructs one.

### 2.4 Real observability pillars + `Observer`/`Operation`

Because tracing/metrics are noop'd, beeline grew scar tissue: a hand-rolled
`slowQueryThreshold` with manual `defer`-elapsed logging (`freshness/postgres/stats.go:57-63`),
and no visibility at all into the hot paths (`BatchGet`, `Claim`, engine calls) that the design
doc's freshness contract depends on. Flipping `config/config.go:175-187` to call
`Observability.NewPillars` (per its own comment) and threading `Observer` through the matrix
packages is the single highest-leverage proving-ground exercise available — it would test the
platform's central abstraction against a genuinely latency-sensitive workload.

### 2.5 `retry` — the claim-error path deserves better than a flat sleep

`refresh/worker.go:76-87` uses the same flat `IdleBackoff` sleep for both "nothing due" (fine)
and "claim errored" (not fine — a follower pool hammering a struggling leader at a fixed 250ms
cadence with zero jitter is a thundering herd). There is **no exponential backoff, jitter, or
attempt cap anywhere in the repo**. `retry.NewExponentialBackoffPolicy` + `Unretryable` fits the
claim-error path directly; the deliberate "no client-side retry, leases expire" design
(`follower/follower.go:9-13`) is unaffected — this is about pacing re-polls, not re-driving work.

### 2.6 `version` — a straight duplicate

`version/version.go` + `scripts/build.sh` ldflags injection reimplements `platform-go/version`
(which also carries `Version` and a `Get()` struct + `WriteJSON`). Delete ours, adopt theirs.

### 2.7 `testutils/containers` — replace the shell readiness loops and env gates

`scripts/test_integration.sh:38-60` hand-rolls `seq`/`sleep` readiness polling for Postgres and
Redis; `pgtest` and `store/redis/store_test.go` each invent their own env-var gate
(`BEELINE_TEST_POSTGRES_DSN`, `BEELINE_TEST_REDIS_ADDR`). Platform's `testutils/containers`
(`RunningTests`, `SkipIfNotRunning`, `StartWithRetry`, `redistest.Start`) covers the Redis half
outright and the gating convention for both. Keep `pgtest`'s random-schema-per-test trick — that
part is genuinely good and platform has no equivalent (see §4.2).

### 2.8 `routing` typed handlers + OpenAPI — bigger swing, real payoff

All ~25 endpoints are raw handlers with ~8 hand-rolled parsers (`parseLatLng`, `parseCell`,
`parseDuration`, `parseSkip`, …) and string-concatenated 400 bodies. The v7 router's
`Handler[In, Out]` generics, typed path params (`{areaID:uint64}`), and struct-tag binding would
replace most of that parsing — and beeline would get a generated OpenAPI spec for an API that
currently documents its wire shapes only in CLAUDE.md prose. This is a v7-only feature and a
larger refactor; do it after the upgrade, endpoint by endpoint. The lat/lng and H3-cell parsing
stays hand-rolled either way (domain types), which is fine.

### 2.9 Small ones

- `cli/root.go:143-149` `envOr()` — three flags read env vars bespoke instead of via
  `platformconfig`.
- `compression` — the embedded console serves vendored Leaflet/h3-js with no compression, no
  cache headers, no ETag (`webui/embed.go:25-50`).
- `ratelimiting` — every endpoint is unauthenticated and unthrottled by prototype-design; when
  that changes, the package exists.

---

## 3. Platform has it, but it doesn't fit — adoption blockers that are platform gaps

These are the most valuable proving-ground findings: beeline *tried the shape* platform offers
(or would have) and the offering misses. Each is both an adoption blocker here and a concrete
improvement request there.

### 3.1 `database` is database/sql-shaped; beeline's hot path is pgx-native

`store/postgres/` bypasses `database/postgres.NewDatabaseClient` for raw `pgxpool`
(`db.go:41-67`) — and it has to. The hot paths depend on pgx-native features that
`database.Client`'s `database/sql` surface cannot express:

- `CopyFrom` for 100k-row chunked seeds (`freshness/postgres/index.go`, `seedCopyChunk`)
- `unnest` array-join `BatchGet` with pgx-native array binding (`estimates.go:78-105`)
- transaction-scoped advisory locks on the same conn as the work (`locks.go`)

The cost of bypassing: no otelsql spans/metrics on any query, no `IsReady` ping-retry, no
`CurrentTime`. **Platform gap:** a pgx-native seam in `database` — either a
`RawPgxAccess`-style assertion alongside `RawAccess`, or a `database/pgx` provider whose client
exposes the pool — so high-throughput consumers keep the instrumentation without giving up
CopyFrom/arrays. Also: platform ships no migration runner at all (`Migrator` is BYO); see §4.2.

### 3.2 `distributedlock/postgres` pins connections and uses session locks; beeline's xact-scoped design is better

Beeline's `AdvisoryLocker` (`store/postgres/locks.go`) uses `pg_advisory_xact_lock` inside a
throwaway transaction: a crashed holder's lock dies with its connection, zero bookkeeping,
per-tick janitor election with automatic failover (`cli/serve.go:339-375`). Platform's
`distributedlock/postgres` holds a **dedicated `*sql.Conn` per lock** with an advisory-only TTL —
strictly worse for this pattern, and it would pin write-pool connections on every janitor tick.

**Platform gap:** add a scoped-execution mode to `distributedlock` —
`WithLock(ctx, key, fn)` / `TryWithLock(ctx, key, fn)` — backed by xact-scoped advisory locks on
Postgres. That shape (run `fn` while held, release implicitly) is also what most callers of the
Redis locker actually want. Beeline would adopt immediately and delete `locks.go` plus its
bespoke `Locker`/`noopLocker` seams (`control/control.go:83-96`).

### 3.3 `cache` can't express beeline's hot store

Three misses, in increasing severity:

1. **No per-call TTL** — expiry is fixed at construction; beeline's freshness contract is
   per-pair.
2. **Gob-only values** — beeline's Redis store uses a fixed 24-byte binary codec
   (`store/redis/store.go:64-85`) because at 300k-key batch scale, codec overhead is the p95.
   The codec should be a seam (`Codec[T]` with gob default).
3. **`BatchCache` by type assertion** — batch is beeline's *primary* access pattern
   (`BatchGet` of a whole `/table` grid); it shouldn't be an optional capability you discover at
   runtime.

With those three fixed, `internal/store/redis/` (194 lines) collapses into a key function and a
codec. Until then, hand-rolled is correct. (The `cache/redis/slots` key-affinity work is a good
foundation the beeline use case would actually exercise.)

### 3.4 `cache/memory` has no TTL, eviction, or singleflight

`freshness/postgres/stats.go` hand-rolls a TTL memo (three maps + mutex) for console-poll
aggregates, and it stampedes on cold cache under concurrent polls because there's no
singleflight. Platform's memory cache is an unbounded map that ignores the configured expiry.
**Platform gap:** TTL + size bound + optional singleflight on `cache/memory` would have covered
this out of the box.

---

## 4. Packages platform doesn't have yet — proven by this application

### 4.1 Streaming event capture with a JSONL sink (the articulated use case)

`internal/telemetry/` (~700 lines) is the package platform was missing a use case for, now with
the use case attached: **high-volume operational event capture for offline model training** —
distinct from `analytics` (low-volume product events to PostHog/Segment) and heavier than
plain logging. The generic, extractable parts:

- `Sink` seam (`telemetry.go:46-53`) — `WriteX`/`Flush`/`Close`; the doc comment already names
  Kafka/S3 as future implementations.
- `JSONLSink` (`jsonl.go`) — append-only, size-rotated, retention-pruned, byte-count resumed
  across restarts, lexically-sortable rotation stamps.
- Bounded never-blocking `Recorder` (`recorder.go`) — drop-and-count overflow, single flusher
  goroutine, deliberate drain-after-server-shutdown lifecycle.
- Time-bucketed `aggregator` (`aggregator.go`) — per-key counters, bounded with overflow
  counting, deterministic flush.

Proposed platform shape: an `eventcapture` (or `telemetry`) package with the `Sink` interface,
`jsonl` + `noop` + `mock` providers, and the recorder/aggregator as the core. Only the event
*types* stay in beeline. The requirements it proves: never block the hot path, never lose the
write-ahead file on crash mid-rotation, drop-with-count over backpressure.

### 4.2 Migration runner (goose + lock coordination)

Platform's `database.Migrator` is a one-method BYO interface; beeline had to hand-wire
instance-based goose providers **twice** (`store/postgres/db.go:73-110`,
`store/sqlite/migrate.go`), including the non-obvious parts: instance-based (not package-global)
providers so parallel tests don't race, a schema-derived FNV lock ID so schema-isolated tests
migrate concurrently while real deployments serialize (`db.go:30-35`), and tightened lock
timeouts. A `database/migrate` package wrapping embedded-FS goose with that lock discipline —
satisfying `database.Migrator` — turns ~80 lines of subtle wiring per consumer into a
constructor call. Pairs with §3.1.

### 4.3 Leased work queue over Postgres

`distributedlock`'s doc explicitly scopes out job queues; beeline demonstrates the need next
door: `freshness/postgres/index.go` implements claim-with-lease (`FOR UPDATE SKIP LOCKED`),
DB-clock-only scheduling, idempotent re-submission, and expiry-based recovery — the entire
follower fleet coordinates through it with zero fencing. That's a general "leased pgqueue"
pattern (a lightweight River/neoq alternative shaped like platform): `Claim(n, lease)` /
`Complete(keys)` / expiry reaping, generic over a payload key. Beeline's freshness index would
stay domain-specific, but the SKIP-LOCKED lease engine underneath is extractable and is the part
every distributed-systems consumer will rewrite badly.

### 4.4 `clock` — injectable time

Beeline hand-rolls `func() time.Time` injection three separate times
(`freshness/memory/index.go:47`, `store/sqlite/areas.go:26`, `store/postgres/areas.go:39`) plus
two context-aware sleep helpers (`refresh/worker.go:180-190` duplicated at
`engine/latency/engine.go:110-124`). And the *absence* of a fake-able clock in the Postgres
freshness index is why the `freshnesstest` conformance suite runs in real wall time with
hundreds of milliseconds of `time.Sleep` per assertion. A tiny `clock` package —
`Clock` interface (`Now`, `Sleep(ctx, d)`, `NewTicker`), `real` + `fake` providers — is the
most-requested missing utility in any platform; beeline demonstrates all three call sites
(domain stamping, pacing loops, test control).

### 4.5 Canonical struct hashing

`ProviderCatalog.ComputeHash` (`beeline/providers.go:117-137`) hand-builds
canonicalize-then-sha256 (clone, sort slices, rely on JSON's sorted map keys) so identical
catalogs hash identically regardless of construction order — the change-detection signal the
whole follower-sync protocol rides on. `cryptography/hashing` has `Hasher` over bytes but
nothing for *canonical* hashing of a value. A `hashing.Canonical(v any)` helper (or
`cryptography/hashing/canonical`) is small, easy to get subtly wrong (float formatting, nil vs
empty slice), and exactly the kind of thing a platform should own. The FNV name→int64 lock-key
derivation (`store/postgres/locks.go:114`, `db.go:30`) is a second, related consumer.

### 4.6 Weaker candidates, noted for completeness

- **Coalescing write buffer** — `freshness/postgres/access.go` (dedup map, wake channel,
  steal-own-keys-into-tx, drop-on-failure policy) is excellent but the tx-stealing semantics are
  app-specific; a generic `batcher[K]` would cover maybe 60% of it. Revisit if a second consumer
  appears.
- **Config-generation watcher** — the `config_version` bump-in-tx + poll loop
  (`store/postgres/areas.go:21-30`, `cli/serve.go:286-330`) is a clean pattern for
  multi-head config convergence, but it's ~40 lines and tightly coupled to "what to resync."
  A platform `configwatch` feels premature at one consumer.
- **Fixed-size binary codecs** — the Redis 24-byte value encoding is app-specific; the platform
  fix is the `Codec` seam in `cache` (§3.3), not a codec library.

---

## 5. Deliberate non-uses (audited, not gaps)

For completeness — packages evaluated and correctly *not* used:

- `identifiers` — all identity is domain-derived (H3 cell tuples, serial area IDs). Correct.
- `messagequeue` — work distribution is deliberately HTTP claim/submit with leases as the only
  coordination (design §8); a queue would add a broker without removing the lease logic. Correct,
  though §4.3's leased-queue package is the platform-shaped version of what beeline built instead.
- `filtering` — no cursor-paginated list endpoints yet; the areas list is small. Correct for now.
- `eventstream` — the console polls at 2 Hz; SSE via `eventstream/sse` is a plausible future
  upgrade for `/_ops_/freshness` but polling is defensible at this scale.
- `featureflags`, `secrets`, `authentication`, `cookies` — nothing in the prototype needs them;
  `secrets` + `authentication` become relevant the day the control plane stops being
  unauthenticated.

## 6. Recommended sequencing

1. **Upgrade v4 → v7** (unblocks everything below; a finding in itself).
2. Quick wins, no design work: `version`, healthcheck checkers, `errors`/`errors/http`,
   `encoding`, `retry` on the claim-error path. (§2.1–2.6)
3. Turn on real pillars and thread `Observer` through the matrix packages — the flagship
   proving-ground exercise. (§2.4)
4. File the platform improvement issues from §3 (pgx seam, xact-scoped `WithLock`, cache
   TTL/codec/batch) — each unblocks deleting a beeline subsystem.
5. Extract §4.1 (event capture/JSONL) and §4.4 (`clock`) into platform — both have clean seams
   in beeline today and a second consumer is easy to imagine.
6. Typed-handler/OpenAPI migration of `httpapi`, endpoint by endpoint. (§2.8)
