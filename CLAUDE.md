# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

`github.com/primandproper/template-go` — a batteries-included Go application template built on
[`github.com/primandproper/platform-go`](https://github.com/primandproper/platform-go). Go 1.26.

The application is a **Cobra CLI** that acts as the single entrypoint. Out of the box it bootstraps
the platform-go observability suite (logging/tracing/metrics/profiling) with graceful shutdown and
ships a `version` subcommand. Grow it by adding subcommands (e.g. a `serve` command that stands up
`platform-go`'s HTTP server).

## Layout

- `cmd/main/main.go` — thin entrypoint: signal-cancellable context → `cli.Execute`.
- `cmd/tools/codegen/configs/` — codegen tool behind `make configs`: builds each environment's
  `*config.Config` as a real, typed Go object (`environments.go`), validates it, and renders it to
  `config/<env>.json` via `config.Render`. The checked-in JSON is a projection of these builders — edit
  the Go, never the JSON, then re-run `make configs`.
- `config/` — generated per-environment config files (`localdev.json`, `production.json`); committed so
  they stay reviewable, and loadable at runtime via `--config`.
- `internal/cli/` — cobra root command, observability bootstrap + shutdown, subcommands.
- `internal/config/` — assembles `observability.Config` and builds the pillars (slog logging + noop
  tracing/metrics/profiling by default). See `Config.NewPillars` for the upgrade path to real telemetry.
  Two loaders use `platform-go/v4/config`: `Load` overlays `TEMPLATE_GO_`-prefixed environment
  variables on the flag/default-seeded config, and `LoadFromFile` decodes a complete JSON config file
  and then overlays the same environment variables. `Render` goes the other way: it validates typed
  `Config` objects and writes them to disk (see `make configs`).
- `version/` — build metadata (`CommitHash`/`BuildTime`/`CommitTime`), injected via `-ldflags` by
  `scripts/build.sh`.

## Common Commands

```bash
make setup          # Create artifacts dir + download the module cache
make configs        # Render config/<env>.json from the real Go objects in cmd/tools/codegen/configs
make build          # Compile all packages, then build artifacts/template-go with version metadata
make run ARGS="version"   # go run the CLI with arguments
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
2. `github.com/primandproper/template-go` (this module)
3. `github.com/primandproper` (org-level packages, including platform-go)
4. Everything else (third-party)

The Makefile `THIS` variable must be the full module path (`github.com/primandproper/template-go`)
because `format_imports.sh` runs `dirname` on it to derive the org-level prefix.

## Testing

- Tests use `stretchr/testify` (assert, require).
- Tests call `t.Parallel()` by default.
- `make test` excludes `cmd` packages, so keep testable logic in `internal/` and `version/`.
- Test command: `CGO_ENABLED=1 go test -shuffle=on -race -vet=all -failfast`.

## Conventions worth knowing

- Observability logs are structured slog written to **stdout**. `version` prints its data to stdout
  and emits nothing at the default `info` level, so `template-go version` stays machine-parseable.
- The `--log-level` / `--service-name` persistent flags default from the `TEMPLATE_GO_LOG_LEVEL` and
  `TEMPLATE_GO_SERVICE_NAME` environment variables. The `--config` flag (default from
  `TEMPLATE_GO_CONFIG_FILEPATH`) points at a JSON config file; when set, `bootstrap` loads it via
  `config.LoadFromFile` instead of the flag/env defaults.
- Configuration is layered: defaults (or a JSON file) < `TEMPLATE_GO_`-prefixed environment variables.
  Env vars follow platform-go's nested `envPrefix` tags, e.g.
  `TEMPLATE_GO_OBSERVABILITY_LOGGING_LEVEL`. Give new `Config` fields both `envPrefix`/`env` and `json`
  tags so they participate in `Load` and `LoadFromFile`.
- To enable real tracing/metrics/profiling, populate the sub-configs in `internal/config` and call
  `observability.Config.NewPillars`, or swap the noop constructors in `Config.NewPillars`.

## Linting

- ~46 linters enabled via `.golangci.yml` (golangci-lint v2 format).
- Formatters: `gci` and `gofmt` (configured in the `formatters:` section).
- Notable strictness: `errcheck` (with `check-blank` + `check-type-assertions`), `errorlint`,
  `gosec`, `forcetypeassert`, `unconvert`, `unparam`. Many are relaxed for `_test.go` files.
