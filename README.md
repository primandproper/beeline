# beeline

A batteries-included Go application template built on
[`primandproper/platform-go`](https://github.com/primandproper/platform-go).

Unlike a bare scaffold, this template ships a **real, runnable application**: a
[Cobra](https://github.com/spf13/cobra) CLI that bootstraps the platform-go
observability suite (logging, tracing, metrics, profiling) with graceful
shutdown, plus the full build/format/lint/test toolchain and CI to go with it.

The CLI is meant to be your single entrypoint. Building a one-off tool? Add a
subcommand. A long-running worker? Add a subcommand. An HTTP service? Add a
`serve` subcommand that stands up `platform-go`'s HTTP server. You start here.

## Quickstart

Requires **Go 1.26+**. [Docker](https://www.docker.com/) is used for linting and
shellcheck. The demo additionally needs `kubectl` and [k3d](https://k3d.io)
(`brew install k3d`) — it runs on a real Kubernetes cluster.

```bash
make setup                  # create artifacts/ and download the module cache
make build                  # compile everything, produce artifacts/beeline
./artifacts/beeline version
./artifacts/beeline --help
```

Or run without building a binary:

```bash
make run ARGS="version"
```

## Demo: the matrix service

The `serve` subcommand runs a **stateless head**: coordination state — the freshness index,
operator config, and singleton-job election — lives in shared Postgres, so any number of
identical heads run at once and none of them is special. A head keeps origin→destination
travel-time estimates fresh in the background for every **enabled** service area, and serves
the read path plus an embedded operator console over HTTP. The routing engine is a Haversine
stand-in (great-circle distance ÷ per-profile speed) behind the same interface a real engine
(OSRM/Valhalla) would implement, so the whole pipeline runs with **no external routing
dependency**.

There is one demo, because there is one deployment shape. **`make demo`** brings up a local
**k3s cluster** (via [k3d](https://k3d.io)) running shared Postgres (the freshness index,
operator config, and singleton election), Redis (the hot estimate store), an **autoscaling pool
of `serve` heads** behind one Service, an **autoscaling pool of `work` followers**, and a k6 load
generator aimed at the heads. It seeds the three Austin service areas (downtown at res 9, the
city at res 8, the metro at res 7+6 — ~1.1M pairs) and then tails the freshness contract next to
live replica counts:

```
  debt=1087054  achieved=213    required=3635    heads=2/2    workers=1/1
  debt= 481907  achieved=4820   required=3635    heads=2/3    workers=2/2
  debt=  56681  achieved=11940  required=3635    heads=3/3    workers=8/8
  debt=      0  achieved=9310   required=3635    heads=3/3    workers=0/0
```

**Both pools scale on their own, and that is the point.**

- The **head pool** scales on CPU — the read path is CPU-bound, so a `HorizontalPodAutoscaler`
  over the metrics-server k3s already ships is the honest signal. The bundled load generator is
  what moves it. Heads also serve `/_work_/claim` and `/_work_/submit` for the follower pool, so
  the two autoscalers visibly couple.
- The **follower pool** scales on **freshness debt** — the §3 contract read straight off
  `GET /_ops_/freshness` by KEDA's `metrics-api` scaler, with no metrics pipeline in between. The
  cache's own health is the scaling metric. It scales **to zero**: no enabled areas means no debt
  means no reason to run a compute pool at all.

Because the seeded areas carry a 5-minute `targetTTL`, the whole working set re-stales at once
every five minutes — so the pool wakes, burns ~1.1M pairs of debt down to zero in about ninety
seconds, and goes back to sleep. Just leave it running and watch the sawtooth.

```bash
make demo                                    # or RPS=250 WORKER_MAX=16 make demo
kubectl --context k3d-beeline-local -n beeline-local get hpa,scaledobject,deploy -w

# turn the read load up; the head pool follows. Cost per request matters more than
# rate — a bigger /table grid is what pushes head CPU past the 60% target.
kubectl --context k3d-beeline-local -n beeline-local \
  set env deploy/beeline-loadgen RPS=250 TABLE_GRID=48 TABLE_RATIO=0.6

# kill ONE head; the survivors keep the contract. (Deleting them all with
# -l role=head is an outage, not a demo: a PDB gates evictions, never a delete.)
kubectl --context k3d-beeline-local -n beeline-local delete pod --wait=false \
  "$(kubectl --context k3d-beeline-local -n beeline-local get pod -l role=head -o name | head -1)"
curl 'localhost:8080/estimate?origin=30.27,-97.745&dest=30.275,-97.74'
```

Then open **http://localhost:8080** for the operator console.

It needs Docker, `kubectl` and k3d, and no local binary. It never reads or changes your current
kubectl context — every command above names the demo's context explicitly, and the cluster is
created with `switchCurrentContext: false`. Postgres is not optional: there is no single-node
mode, which is what makes every head disposable and every endpoint answer the same regardless of
which head you ask.

The manifests are split the way you would split them for a real deployment. `deploy/base/` is
what beeline *is* — two roles, how they are exposed, and the shape of both autoscalers.
`deploy/environments/local/` is what makes it a laptop demo — in-cluster Postgres and Redis,
throwaway credentials, the load generator, laptop-sized bounds. Base names its dependencies and
the environment supplies them, so pointing a new environment at managed Postgres is an overlay
change and nothing else.

The compute side is the `work` subcommand: the same binary pointed at a running head becomes a
stateless **follower** that claims pending pairs, computes them locally, and submits the results
back. A head started with `refreshWorkers: 0` does no computing of its own, which is the shape
`config/cluster.json` and `config/production.json` both use: heads coordinate and serve reads,
the follower pool computes. Followers hold no state at all — a dead one just lets its leases
expire — which is what makes handing the replica count to an autoscaler safe.

```bash
./artifacts/beeline work --config config/cluster.json --leader http://localhost:8080
```

To run a head against your own Postgres instead of the demo cluster's:

```bash
make build
BEELINE_MATRIX_BACKEND_POSTGRES_URL=postgres://…  ./artifacts/beeline serve
```

Started with plain `serve`, a fresh database has **no areas** — nothing refreshes
until you configure one. In the
console: click **+ New**, give it a name, upload a **GeoJSON polygon** (required —
it is the area's canonical geometry), and create it. The area starts **disabled**.
Flip it **On** and the refresh loop begins filling it with H3 cells. An area carries
one or more **precision layers** (H3 resolutions, DoorDash-style); each layer's cell
set is derived from the polygon and precomputed independently. The console creates
one-layer areas; add more layers via the HTTP API. Multiple areas can be enabled at
once; the progress overlay paints the selected one.

Everything the console does is a plain HTTP call — drive it with curl too:

```bash
# create an area from a GeoJSON polygon (starts disabled), then enable it. layers is
# the ordered list of precision layers: per layer, maxRadiusMeters is the per-origin
# travel-radius bound (0 = full mesh) and minDistanceMeters is recorded for future
# distance-based layer selection; warmStrategy is eager | lazy | hybrid;
# demandIdleTTL evicts cold demand-filled pairs (blank = never).
curl -sX POST localhost:8080/_config_/areas -H content-type:application/json -d '{
  "name":"downtown","warmStrategy":"hybrid","demandIdleTTL":"1h",
  "layers":[{"resolution":8,"minDistanceMeters":0,"maxRadiusMeters":3000,"coreRadiusMeters":1500}],
  "geojson":{"type":"Polygon","coordinates":[[[-98.05,30.32],[-97.99,30.32],[-97.99,30.37],[-98.05,30.37],[-98.05,30.32]]]}}'
curl -sX POST localhost:8080/_config_/areas/1/enable

curl 'localhost:8080/_ops_/freshness?area=1'   # §3 debt/throughput contract (watch it drain)
curl 'localhost:8080/_ops_/cells?area=1'       # per-origin-cell freshness rollup (the console's overlay)
curl 'localhost:8080/estimate?origin=30.34,-98.02&dest=30.35,-98.00&profile=car'
curl -sX POST localhost:8080/_config_/areas/1/disable   # stop refreshing it; its pairs leave the working set
```

Area definitions persist in the shared Postgres (point at it with
`BEELINE_MATRIX_BACKEND_POSTGRES_URL`), so an enabled area re-seeds and re-warms on
restart — on *every* head, not just the one it was created through — and a disabled
one stays idle. Cells are derived data: every layer's cell set is
re-polyfilled from the area's GeoJSON at seed time, so replacing the polygon
(PUT …/geojson) reshapes the whole area.

## What's included

- **A working CLI** — `cmd/main` → `internal/cli` (cobra root + `version`
  subcommand), wired to the observability suite in `internal/config`.
- **Observability out of the box** — structured slog logging; tracing, metrics,
  and profiling default to noop so the binary is quiet and dependency-free until
  you turn them on.
- **`Makefile` + `scripts/`** — thin Makefile delegating to shellcheck-clean
  scripts for build, format, lint, and test.
- **`.golangci.yml`** — ~46 linters (golangci-lint v2), with `gci` + `gofmt`
  formatters and a strict-but-practical policy.
- **GitHub Actions** — `build`, `formatting`, `lint`, `shellcheck`, and
  `unit tests`, each mirroring a `make` target and path-filtered.
- **`CLAUDE.md`**, issue/PR templates, and a Go `.gitignore`.

## Common commands

```bash
make format     # imports (gci), field/tag alignment, gofmt -s
make lint       # golangci-lint (Docker) + shellcheck (Docker)
make test       # go test -shuffle -race -vet=all -failfast (excludes cmd)
make build      # compile all packages + build the binary with version metadata
```

## Configuration

The CLI reads two settings, via flags or environment variables:

| Flag             | Environment variable       | Default       | Values                           |
| ---------------- | -------------------------- | ------------- | -------------------------------- |
| `--log-level`    | `BEELINE_LOG_LEVEL`    | `info`        | `debug`, `info`, `warn`, `error` |
| `--service-name` | `BEELINE_SERVICE_NAME` | `beeline` | any string                       |

```bash
BEELINE_LOG_LEVEL=debug ./artifacts/beeline version
```

Observability logs are structured slog written to **stdout**. The `version`
subcommand prints its data to stdout and emits nothing at the default `info`
level, so `beeline version` stays machine-parseable.

To enable real tracing/metrics/profiling, populate the corresponding sub-configs
in `internal/config/config.go` and call `observability.Config.NewPillars`
(the platform's aggregate bootstrap), or replace the noop constructors in
`Config.NewPillars`.

## Layout

```
cmd/main/             # entrypoint: signal-cancellable context -> cli.Execute
internal/cli/         # cobra root command, observability bootstrap, subcommands
internal/config/      # assembles observability.Config and builds the pillars
version/              # build metadata, injected via -ldflags by scripts/build.sh
scripts/              # build/format/lint/test/shellcheck helpers
.github/workflows/    # CI mirroring the make targets
```

## Make it yours

After creating a repository from this template, run the rename script with your
new module path. It rewrites every reference to this template's module path and
app name, reformats the code, and then deletes itself — leaving no trace that the
project started from a template:

```bash
./rename.sh github.com/acme/coolapp
```

Then confirm everything is wired up:

```bash
make setup && make build && make test
```

<details>
<summary>What the script changes (in case you prefer to do it by hand)</summary>

- **`go.mod`** — the `module` path.
- **`Makefile`** — `THIS` (full module path) and `BINARY_NAME`.
- **`.golangci.yml`** — the `prefix(...)` entries under `formatters.gci.sections`
  (this module and its org).
- **`scripts/`** — the module path in `scripts/test.sh` and
  `scripts/format_imports.sh`, and `VERSION_PKG` in `scripts/build.sh`.
- **`internal/`** — `DefaultServiceName` and the `BEELINE_*` env-var prefixes.
- **`CLAUDE.md`** and this **`README.md`** — project details.

The `Makefile` `THIS` variable must be the full module path, because
`scripts/format_imports.sh` runs `dirname` on it to derive the org-level import
prefix (section 3 of the `gci` ordering). `platform-go` (also under
`github.com/primandproper`) intentionally moves to the third-party import group
once your module lives under a different org.
</details>

## License

[AGPL-3.0](./LICENSE).
