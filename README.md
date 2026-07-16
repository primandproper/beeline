# template-go

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
make build                  # compile everything, produce artifacts/template-go
./artifacts/template-go version
./artifacts/template-go --help
```

Or run without building a binary:

```bash
make run ARGS="version"
```

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
| `--log-level`    | `TEMPLATE_GO_LOG_LEVEL`    | `info`        | `debug`, `info`, `warn`, `error` |
| `--service-name` | `TEMPLATE_GO_SERVICE_NAME` | `template-go` | any string                       |

```bash
TEMPLATE_GO_LOG_LEVEL=debug ./artifacts/template-go version
```

Observability logs are structured slog written to **stdout**. The `version`
subcommand prints its data to stdout and emits nothing at the default `info`
level, so `template-go version` stays machine-parseable.

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
- **`internal/`** — `DefaultServiceName` and the `TEMPLATE_GO_*` env-var prefixes.
- **`CLAUDE.md`** and this **`README.md`** — project details.

The `Makefile` `THIS` variable must be the full module path, because
`scripts/format_imports.sh` runs `dirname` on it to derive the org-level import
prefix (section 3 of the `gci` ordering). `platform-go` (also under
`github.com/primandproper`) intentionally moves to the third-party import group
once your module lives under a different org.
</details>

## License

[AGPL-3.0](./LICENSE).
