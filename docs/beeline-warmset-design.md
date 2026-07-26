# Beeline Warm-Set Design: bounding, warm strategies, and decay

**Status:** **Implemented** (Phases 1–4). Phase 4 landed as **layers** (`Area.Layers []Layer`,
the sketch's "tiers" plus a per-layer `MinDistanceMeters` recorded for future distance-based
selection); read-path fallthrough across layers is still deferred — reads key at the finest
layer. See §7 for the decisions taken; the plan below is retained as the record of intent.
**Audience:** an implementing agent starting fresh. This doc is self-contained; read
`beeline-design.md` (esp. §3 freshness, §6 batching, §7 bounding, §9 read path) for the
underlying model, and `CLAUDE.md` for build/test commands. Section refs (§) point into
`beeline-design.md`.

## 1. Goal

Replace the current all-or-nothing coverage model with a small set of **operator-configurable,
per-area knobs** that let a single-resolution area cover a real metro without either (a) computing
the full N² mesh or (b) needing a demand-prediction ML model. We approximate DoorDash's
"precompute the pairs that matter" using **observed demand** (the read path already records it)
instead of **predicted demand** (their ML). Everything here is config + mechanical index logic —
no model.

Three levers, all bubbled up to the area config:

1. **Bound** — a travel radius expressed in **meters** (operator-meaningful), not H3 rings.
2. **Warm strategy** — how much of the bounded set we keep fresh eagerly vs. fill on demand
   (`eager` | `lazy` | `hybrid`).
3. **Decay** — demand-filled pairs that stop being queried get evicted, so cost tracks real usage.

Multi-resolution tiering (§7) is **explicitly deferred** to the last phase — it is "the same
bounded logic repeated per resolution" and should land only after the single-resolution knobs are
solid. It is sketched in Phase 4 so the earlier phases don't paint us into a corner.

## 2. Non-goals / out of scope

- **ML demand prediction.** We use observed demand instead. Not building a model.
  *(Update: the scaffolding for this now exists, though the model itself remains out of scope.
  `internal/telemetry` exports the read path's fetch events — raw and/or per-(pair, bucket)
  aggregates, H3 cells only — through a pluggable sink (JSONL file today) when
  `matrix.telemetry` is enabled, and `POST /_ops_/warm` feeds a trained model's predicted pairs
  back into the freshness index via `control.Coordinator.WarmPairs` (bump = decayable demand,
  seed = pinned). Observed demand is still the default behavior.)*
- **Swapping the engine (OSRM/Valhalla) or store (Redis).** These are independent tracks already
  enabled by the `beeline.RoutingEngine` / `beeline.Store` seams (`internal/beeline/types.go`). This
  doc changes neither interface. Do not entangle them with this work.
- **Time-based (minutes) bounds.** Deferred — a travel-*time* budget depends on per-profile speed
  (`MatrixConfig.Profiles`) while a bound is per-area, so a 15-min car radius ≠ 15-min walk radius.
  Distance sidesteps this. Revisit only after tiering, if wanted. Note it in the config as a future
  `maxTravelSeconds` sibling, but do not implement.

## 3. Current state (what exists today)

Read these before changing anything.

- **Bound is expressed in H3 rings.** `beeline.Area.RadiusRings` (`internal/beeline/area.go:24`),
  persisted in SQLite (`internal/store/sqlite/migrations/0001_areas.sql`, column `radius_rings`,
  round-tripped in `internal/store/sqlite/areas.go`). Validated `>= 1` in
  `internal/control/control.go` (`validateAreaFields`, ~line 448). Exposed over HTTP as JSON
  `radiusRings` (`internal/httpapi/routes.go`, create/update DTOs). The console form
  (`internal/webui/static/`) collects it. `scripts/demo.sh` / `make demo` seed a demo area with it.
- **Tessellation is purely geometric.** `tessellate.PairsFromCells`
  (`internal/tessellate/tessellate.go:74`): for each origin cell, `h3.GridDisk(origin, radiusRings)`,
  then **clip destinations to in-area cells**, emit one directed `PairKey` per
  origin × in-area-dest × profile. No ML, no demand. This is already the §7 bound; we are only
  changing how the operator *expresses* the radius and how much of the result we *eagerly warm*.
- **Enable seeds the entire bounded set.** `control.Coordinator.seedLocked`
  (`internal/control/control.go:396`) tessellates the whole area and calls `index.Seed(pairs)`.
  The refresh pool (`internal/refresh/`) then keeps **all** of it fresh forever. There is no notion
  of "core vs tail" and nothing is ever evicted while the area is enabled. This is effectively
  `warmStrategy = eager` today, and the only strategy.
- **The read path already observes demand — but caches unboundedly.**
  `query.Handler.Estimate` (`internal/query/query.go:73`):
  - Origin outside every enabled area → compute directly, cache nothing (`query.go:77`).
  - Same origin/dest cell → direct correction, not cached (`query.go:95`).
  - In-area miss → **demand-fill**: compute now, `store.Put`, `index.MarkComputed`
    (`query.go:126-139`). **Note:** this happens for *any* in-area destination regardless of the
    radius bound — demand-fill is currently unbounded within the area.
  - In-area stale hit → return stale + `index.Bump` to reprioritize (`query.go:112`).
- **Freshness index internals.** `internal/freshness/memory/index.go`. Per-pair `entry` holds
  `computedAt`, `leaseUntil`, `bumped` (`index.go:18`). `Seed` adds never-computed entries and is
  idempotent (`index.go:65`). `Bump` **adds unknown keys** (`index.go:292`) — this is what makes
  demand-fill grow the working set. `Unseed` drops a whole area (`index.go:83`). `Claim` prioritizes
  bumped → never-computed → oldest (`index.go:218`). There is **no per-pair eviction** and **no
  last-accessed tracking** today — that is the Phase 3 gap.

Key consequence: today the enabled working set is **grow-only** — `Seed` puts the full bound in,
demand-fill/`Bump` only add, nothing leaves until the area is disabled. Lazy/hybrid + decay are the
new behavior.

## 4. The model

For an enabled area, at steady state the warm (tracked-and-refreshed) set is:

```
warm_set = eager_core  ∪  observed_demand
```

- **`eager_core`** — pairs seeded at enable time and pinned fresh forever. Sized by
  `warmStrategy` + `coreRadiusMeters`. Never decays.
- **`observed_demand`** — pairs pulled in by real queries (demand-fill / `Bump`), bounded by
  `maxRadiusMeters`, that **decay** if not queried within `demandIdleTTL`.

`warmStrategy` picks where the core boundary sits:

| Strategy | `eager_core` | Cost profile |
|---|---|---|
| `eager`  | entire `maxRadiusMeters` bound (today's behavior) | O(N·k), usage-independent |
| `lazy`   | empty — warm nothing up front | grows purely with real queries |
| `hybrid` | pairs within `coreRadiusMeters` (≤ `maxRadiusMeters`) | dense near-field pinned, sparse tail on demand |

`maxRadiusMeters` is the hard outer bound for **both** eager seeding and demand-fill: beyond it, a
query is still answered (compute directly) but **never cached** — same treatment as an out-of-area
coordinate today. `maxRadiusMeters == 0` is the **full-mesh sentinel** (every in-area pair; skips the
GridDisk entirely). A degenerate 0-meter radius is useless, so overloading 0 to mean "no bound" is
safe and matches the operator's stated preference for "0 = everything".

## 5. Phased plan

Each phase is independently shippable and testable. Land them in order; do not start a later phase
before the earlier one's tests pass. Tests: `stretchr/testify`, `t.Parallel()`, run with
`make test` (race/shuffle/failfast). Keep testable logic in `internal/` (cmd is excluded from tests).

### Phase 1 — Bound in meters + full-mesh sentinel

**Goal:** operators think in meters/miles; H3 ring count becomes an internal detail; `0` means
full mesh.

**Decision (recommended):** *replace* `RadiusRings` with `RadiusMeters float64` end to end. Do not
keep both — a derived-and-cached ring count invites drift. The ring count is computed at
tessellation time and never stored.

**Meters → rings conversion (recommended: empirical).** Add a helper in `internal/tessellate/`
that grows `k` outward from an origin cell measuring actual great-circle distance until it exceeds
the bound, reusing `internal/geo.Haversine`. This stays correct across resolutions and if we later
mix them, and avoids brittle per-resolution edge-length constants. (Formula fallback
`k ≈ R / (√3 · edgeLen(res))` is fine for a sanity check but not the source of truth.)
Cache the derived `k` per (resolution, radiusMeters) within a tessellation call — it is the same for
every origin at a given resolution.

**Full-mesh:** when `radiusMeters == 0`, `PairsFromCells` skips `GridDisk` and pairs every in-area
cell with every other in-area cell (still clipped to the area, which is trivially satisfied). Guard
the obvious O(N²) blow-up with a `log()`/warning if the cell count is large.

**Touch points:**
- `internal/beeline/area.go` — `RadiusRings int` → `RadiusMeters float64`; update doc comment.
- `internal/tessellate/tessellate.go` — `PairsFromCells` signature takes `radiusMeters float64`;
  add meters→rings helper; add full-mesh branch. Update `Seed`/`Area` struct in this file too
  (`AreaRings`/`RadiusRings` fields at `tessellate.go:22`).
- `internal/store/sqlite/` — goose migration to replace `radius_rings` with `radius_meters`
  (REAL NOT NULL); update `sqlc_queries`; `make sqlc` to regenerate `generated/`; update `areas.go`
  read/write conversions. **Migration note:** a data migration for existing rows can convert
  `radius_rings` → meters using the empirical distance of that ring count at the row's resolution,
  or (simpler for a prototype with no real data) drop and recreate. Decide based on whether any real
  DBs exist — for the demo, recreate is fine.
- `internal/control/control.go` — `CreateAreaInput`/`UpdateAreaInput` fields; `validateAreaFields`
  (allow `>= 0`; document `0` = full mesh; reject negative); `seedLocked` passes `RadiusMeters`.
- `internal/httpapi/routes.go` — rename DTO fields to `radiusMeters` (create/update/response).
- `internal/webui/static/` — console form: rings input → meters (or a miles input that ×1609.34).
- `scripts/demo.sh`, `Makefile` (`demo` target) — update the seeded area's field.

**Done when:** create/enable an area with `radiusMeters`, pairs match the expected ring disk;
`radiusMeters: 0` yields full mesh; all existing tests updated and green.

### Phase 2 — Warm strategies + core seed + read-path bound enforcement

**Goal:** stop eagerly warming the whole bound; enforce `maxRadiusMeters` on the read path.

**New area fields:** `WarmStrategy string` (`eager`|`lazy`|`hybrid`, default `eager` for backward
compat) and `CoreRadiusMeters float64` (used only by `hybrid`; validate `0 <= core <= max`, and
`max == 0` full-mesh implies `eager`). Rename `RadiusMeters` → `MaxRadiusMeters` for clarity, or
keep `RadiusMeters` as the max and add `CoreRadiusMeters` — pick one and be consistent.

**Seeding:** `control.seedLocked` chooses the eager seed set by strategy:
- `eager` → full `maxRadiusMeters` bound (call `PairsFromCells` as today).
- `lazy` → seed nothing (`index.Seed(nil)` or skip); the routing snapshot in `enabledArea` still
  records the cell set so `Locate` works and demand-fill can happen.
- `hybrid` → `PairsFromCells(..., coreRadiusMeters, ...)`.

**Read-path bound enforcement (new):** the demand-fill branch in `query.Estimate` must respect
`maxRadiusMeters`: within bound → demand-fill + cache (as today); beyond bound → compute directly,
**do not** `Put`/`MarkComputed`. This requires the handler to know the area's `maxRadiusMeters`.
`AreaRouter.Locate` currently returns only `RoutedArea{ID, Resolution}`
(`internal/query/query.go:19`, `internal/beeline/area.go:32`). Extend `RoutedArea` with the bound
(and enough to measure it — origin/dest are already in hand; measure with `geo.Haversine` on the
true coordinates, consistent with the tessellation helper). Update `control.Locate` to populate it.

**Touch points:** `internal/beeline/area.go` (fields + `RoutedArea`), `internal/control/control.go`
(`seedLocked`, `Locate`, validation, DTOs), `internal/query/query.go` (bound check before
demand-fill), `internal/httpapi/routes.go` (DTOs), `internal/webui/static/` (strategy dropdown +
core radius input), SQLite migration + sqlc for the new columns.

**Done when:** a `lazy` area seeds 0 pairs, answers queries via demand-fill, and its working set
grows only with distinct queried pairs; a `hybrid` area seeds the core and demand-fills the tail; a
query beyond `maxRadiusMeters` is answered but not cached. Freshness endpoints
(`/_ops_/freshness`, `/_ops_/cells`) reflect the smaller working set.

### Phase 3 — Decay / eviction of cold demand pairs

**Goal:** `observed_demand` pairs that stop being queried leave the working set (and store), so cost
tracks usage instead of ratcheting up forever.

**Index changes (`internal/freshness/memory/index.go`):**
- Add to `entry`: `lastAccess time.Time` and `pinned bool`.
- `Seed` sets `pinned = true` (eager core — never evicted).
- `Bump` (and the demand-fill path via a new signal) sets `lastAccess = now`, `pinned = false` for
  newly created demand entries. Update `lastAccess` on every query hit/bump so active pairs survive.
- New method `Sweep(ctx, idleOlderThan time.Time) ([]beeline.PairKey, error)`: remove every
  `!pinned` entry whose `lastAccess` predates the cutoff and is not currently leased; return the
  removed keys so the caller can drop them from the store too. Add it to the `FreshnessIndex`
  interface in `internal/beeline/types.go` and the `control.AreaIndex` seam if control drives it.
- Consider updating `lastAccess` on `MarkComputed` too? No — refresh is not demand. Only queries
  (`Bump` / demand-fill) count as access, else nothing ever decays.

**Read-path change:** demand-fill and stale-hit paths must record access. Cleanest is to make
`Bump` the single "this pair was queried" signal and call it on **every** in-area hit (not just
stale ones), or add an explicit `Touch`/`Access` call. Decide: reusing `Bump` conflates "queried"
with "prioritize refresh" — probably fine, but a dedicated `Access` keeps intent clear. Recommend a
dedicated method to avoid over-refreshing fresh-but-queried pairs.

**Janitor:** a periodic sweep. Reuse the refresh loop's ticker (`internal/refresh/worker.go`) or a
small dedicated goroutine in `serve.go`. On each tick, for each enabled area call `Sweep(now -
demandIdleTTL)` and `store` delete the returned keys. Serialize via the `control.Coordinator` if it
must stay consistent with enable/disable (it owns the mutex over index+store).

**New area field:** `DemandIdleTTL time.Duration` (how long an unqueried demand pair survives;
default e.g. 1h). Persist + DTO + console as with the others.

**Touch points:** `internal/freshness/memory/index.go` (+ its tests, `cellstates_test.go` etc.),
`internal/beeline/types.go` (interface), `internal/query/query.go` (access signal),
`internal/refresh/worker.go` or `internal/cli/serve.go` (janitor), `internal/control/control.go`
(wire the sweep + `DemandIdleTTL`), SQLite/DTO/webui.

**Done when:** a demand-filled pair disappears from the working set and store after `DemandIdleTTL`
with no queries; a repeatedly-queried pair stays; eager-core pairs never leave. Unit-test the
sweep with the injectable clock (`index.New(ttl, clock)`).

### Phase 4 — Multi-resolution tiering (**implemented as "layers"**)

Landed after Phases 1–3, with the sketch's "tier" renamed **layer** and one addition:
each layer records `MinDistanceMeters`, the trip distance from which that layer is meant
to serve (the DoorDash-style distance-based selection input), unused on reads for now.

- Area holds `Layers []Layer{Resolution int; MinDistanceMeters, MaxRadiusMeters,
  CoreRadiusMeters float64}` (finest→coarsest) instead of a single resolution+radius.
  Single-res is the one-layer case.
- GeoJSON became the canonical, **required** geometry: every layer's cell set is derived by
  polyfilling it at that layer's resolution at seed time (enable / boot resume / geometry
  change). The persisted cell set and the manual hex-refinement surface (create-from-cells,
  `POST …/cells`, console hex editing) were removed with it.
- Tessellation produces the pair set per layer (each layer its own resolution + bound).
  `PairKey.Res` distinguishes layers, so they coexist in one index/store partition, and the
  bound/warm/decay machinery from Phases 1–3 applies per layer unchanged.
- `AreaRouter.Locate` returns the layer list (finest→coarsest); containment is membership in
  the finest layer's cell set.
- **Still deferred:** the read path iterating layers (pick the finest layer whose bound covers
  the trip and has a hit; fall through to coarser, §7/§9). Reads use `RoutedArea.ReadLayer()`
  (the finest layer) only — the funnel to extend when fallthrough lands. Per-area freshness
  accounting (`DebtForArea`, `/_ops_/cells`) blends layers; `/_ops_/cells` tags each cell with
  its resolution so they can be told apart.

## 6. Final area config surface (target end state)

```jsonc
{
  "name": "austin",
  "warmStrategy": "hybrid",        // eager | lazy | hybrid   (Phase 2)
  "layers": [                      // Phase 4: finest→coarsest precision layers
    {
      "resolution": 9,
      "minDistanceMeters": 0,      // finest layer serves all reads today
      "maxRadiusMeters": 15000,    // outer bound; 0 = full mesh (Phase 1); beyond = uncached
      "coreRadiusMeters": 3000     // eagerly pinned fresh      (Phase 2)
    },
    { "resolution": 7, "minDistanceMeters": 10000, "maxRadiusMeters": 60000 }
  ],
  "geojson": { /* required — every layer polyfills from it */ },
  "demandIdleTTL": "1h"            // cold demand pairs evicted after this (Phase 3)
  // future: "maxTravelSeconds" per-profile time bound — NOT in scope
}
```

Areas remain **database-owned**, created disabled via the control plane — none of this goes in
`config/*.json` or `MatrixConfig` (`internal/config/matrix.go`). The global `TargetTTL` /
`LeaseDuration` / refresh knobs stay where they are.

## 7. Decisions & open questions

**Decided (recommended defaults — implementing agent may revisit with reason):**
1. Replace `RadiusRings` with meters outright; ring count is internal, never stored.
2. Meters→rings via empirical `Haversine` grow, not a per-resolution constant.
3. `maxRadiusMeters == 0` = full-mesh sentinel (skip GridDisk).
4. `warmStrategy` default `eager` (preserves today's behavior for existing/demo areas).
5. Access signal for decay is queries only (demand-fill + hits), never refresh.

**Open (get a human call, or pick and note it):** — all resolved during implementation:
1. Existing-row DB migration: **recreated.** The `internal/store/sqlite` package was still untracked, so
   migration `0001` was edited in place (no real DBs exist); the column is `radius_meters REAL`.
2. `Bump`-reuse vs dedicated `Access`/`Touch`: **dedicated `Access`.** It tracks a pair and stamps
   last-access without raising refresh priority; `Bump` (stale path) also stamps last-access.
3. Where the janitor lives: **dedicated goroutine in `serve.go`** (`runSweeper`) driving
   `control.Coordinator.SweepExpired` under the coordinator's lock, on the new `sweepInterval` config knob.
4. Field naming: **`MaxRadiusMeters`** (+ `CoreRadiusMeters`), used from the start to avoid a rename.
5. Miles vs meters: **meters end to end** — API, storage, and the console all use meters (the console
   radius sliders are meters; demand-idle TTL is a Go duration string like `"1h"`).

## 8. How to validate end to end

Beyond unit tests, drive the real flow (`make demo` boots a gitignored SQLite DB and serves on
`:8080`):
1. Create + enable a `lazy` area; confirm `/_ops_/freshness` shows workingSet 0.
2. Issue `GET /estimate?...` inside it; confirm the pair appears and gets refreshed.
3. Advance past `demandIdleTTL` with no further queries; confirm the pair is swept (workingSet drops).
4. Repeat with `hybrid`; confirm the core is warm at enable and the tail behaves like lazy.
5. `maxRadiusMeters: 0`; confirm full mesh (workingSet ≈ N² for the cell count).

The operator console (`/`) is the fastest way to watch the working set load and decay live via the
per-cell freshness map.
