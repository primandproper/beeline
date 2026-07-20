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
shellcheck.

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

The `serve` subcommand is the prototype. It opens a SQLite **area store**, keeps
origin→destination travel-time estimates fresh in the background for every
**enabled** service area, and serves the read path plus an embedded operator console
over HTTP. The routing engine is a Haversine stand-in (great-circle distance ÷
per-profile speed) behind the same interface a real engine (OSRM/Valhalla) would
implement, so the whole pipeline runs with **no external routing dependency**.

The fastest path is **`make demo`**: it runs the server against a fresh, gitignored
SQLite database (`artifacts/demo.db`) and auto-seeds one **enabled** demo area (Lake
Travis, Austin), so the console shows the cache loading immediately. `Ctrl-C` stops
it; re-running resets from scratch. Override the port with `make demo PORT=9090`.

```bash
make demo                                             # fresh db + seeded demo area + serve
# …or start empty and configure areas yourself:
make build
make run ARGS="serve --config config/localdev.json"   # or ./artifacts/beeline serve --config config/localdev.json
```

Then open **http://localhost:8080**.

Started with plain `serve`, a fresh database has **no areas** — nothing refreshes
until you configure one. In the
console: click **+ New**, give it a name, upload a **GeoJSON polygon** (or leave it
empty), and create it. The area starts **disabled**. Flip it **On** and the refresh
loop begins filling it with H3 cells. Areas over open water stay **carved out**: cells
with no roads are pruned when the polygon is polyfilled (design §7, "road-aware
tessellation"). Refine the shape by clicking **Edit hexes** and toggling cells on the
map. Multiple areas can be enabled at once; the progress overlay paints the selected
one.

Everything the console does is a plain HTTP call — drive it with curl too:

```bash
# create an area from a GeoJSON polygon (starts disabled), then enable it
curl -sX POST localhost:8080/_config_/areas -H content-type:application/json -d '{
  "name":"downtown","resolution":8,"radiusRings":2,
  "geojson":{"type":"Polygon","coordinates":[[[-98.05,30.32],[-97.99,30.32],[-97.99,30.37],[-98.05,30.37],[-98.05,30.32]]]}}'
curl -sX POST localhost:8080/_config_/areas/1/enable

curl 'localhost:8080/_ops_/freshness?area=1'   # §3 debt/throughput contract (watch it drain)
curl 'localhost:8080/_ops_/cells?area=1'       # per-origin-cell freshness rollup (the console's overlay)
curl 'localhost:8080/estimate?origin=30.34,-98.02&dest=30.35,-98.00&profile=car'
curl -sX POST localhost:8080/_config_/areas/1/disable   # stop refreshing it; its pairs leave the working set
```

Area definitions persist in the database (default `beeline.db`, override with
`BEELINE_MATRIX_DATABASE_PATH`), so an enabled area re-seeds and re-warms on restart;
a disabled one stays idle. An area's cell set is the polyfill of its uploaded polygon;
refine it by hand from the console (click cells to add or remove) to carve out water,
private land, or anywhere else you don't want covered.

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
