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

The `serve` subcommand is the prototype. It tessellates a service area into H3
cells, keeps origin→destination travel-time estimates fresh in the background, and
serves the read path plus an embedded operator console over HTTP. The routing
engine is a Haversine stand-in (great-circle distance ÷ per-profile speed) behind
the same interface a real engine (OSRM/Valhalla) would implement, so the whole
pipeline runs with **no external routing dependency**.

```bash
make build
make run ARGS="serve --config config/localdev.json"   # or ./artifacts/beeline serve --config config/localdev.json
```

Then open **http://localhost:8080**.

The demo area is **Lake Travis** in the Austin, TX metro. As the background refresh
loop computes estimates, the console fills the service area with H3 cells — and the
reservoir stays **carved out**: cells over open water are pruned because they have
no roads (design §7, "road-aware tessellation"). The startup log shows it:

```
road mask loaded; pruning roadless cells   cells=1421 resolution=9
service area tessellated ...                cells=488  pairs=43848
```

488 cells are seeded out of the full 631-cell disk — the 143 roadless water cells
(~23%) are dropped. (The basemap tiles need internet; the cells and markers render
regardless.)

Poke at it while it runs:

```bash
curl 'localhost:8080/_ops_/freshness'    # §3 debt/throughput contract (watch it drain)
curl 'localhost:8080/_ops_/cells'        # per-origin-cell freshness rollup (the console's overlay)
curl 'localhost:8080/estimate?origin=30.3428,-98.0274&dest=30.35,-98.00&profile=car'
```

You can also redraw the boundary, move the center, or change the H3 resolution at
runtime from the console (or `POST /_config_/area`) and watch the cache reload. The
road mask follows resolution changes through the H3 hierarchy — pruning stays exact
at or below the mask's resolution and approximate (edge only as sharp as the mask)
when you zoom finer. The console warns under the resolution slider when you've zoomed
past the mask's resolution; rebuild the mask at that resolution for a crisp edge.

### The road mask

The road mask (`config/masks/austin-res9.cells`) is **committed**, so the demo
needs neither the network nor DuckDB — it loads the cell set from disk. Point
`serve` at your own mask with the `roadMaskPath` config key (env
`BEELINE_MATRIX_ROAD_MASK_PATH`); an empty path disables pruning (full geometric
disk).

To rebuild the mask, or build one for a different area, use `make mask`. It reads
[Overture Maps](https://overturemaps.org/) road data via the
[DuckDB](https://duckdb.org/) CLI — pinned in `mise.toml`, so `mise install`
provisions it:

```bash
mise install                 # provides the duckdb CLI
make mask                    # rebuild the demo mask (pulls from Overture's public S3, then caches)

# build a mask for a different area — matches the config's area spec:
make mask MASK_LAT=30.2672 MASK_LNG=-97.7431 MASK_RESOLUTION=8 \
          MASK_AREA_RINGS=6 MASK_OUT=config/masks/myarea.cells
```

`make mask` prints how much it prunes for the configured area (e.g. `488/631 disk
cells have roads, 143 pruned`) and caches the S3 pull under `artifacts/`, so
re-runs are offline unless the area changes or you pass `--refetch`.

> **Note:** the mask is resolution-specific, and how much it prunes depends
> entirely on the area. A wide water body at a fine resolution (like the Lake
> Travis demo) drops a big fraction of the disk; a dense urban area (e.g. downtown
> Austin, ~`30.27, -97.74`) is fully road-covered and prunes almost nothing — the
> mask still loads, it just has little to remove.

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
