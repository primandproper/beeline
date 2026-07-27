# ENVIRONMENT
PWD      := $(shell pwd)
MYSELF   := $(shell id -u)
MY_GROUP := $(shell id -g)

# PATHS
THIS          := github.com/primandproper/beeline
BINARY_NAME   := beeline
CMD_PACKAGE   := $(THIS)/cmd/main
ARTIFACTS_DIR := artifacts
SCRIPTS_DIR   := scripts
COVERAGE_OUT  := $(ARTIFACTS_DIR)/coverage.out

# COMPUTED
TOTAL_PACKAGE_LIST := `go list $(THIS)/...`

# CONTAINER VERSIONS
LINTER_IMAGE        := golangci/golangci-lint:v2.10.1
SHELLCHECK_IMAGE    := koalaman/shellcheck:stable
SQL_GENERATOR_IMAGE := sqlc/sqlc:1.26.0

# COMMANDS
CONTAINER_RUNNER      := docker
RUN_CONTAINER         := $(CONTAINER_RUNNER) run --rm --volume $(PWD):$(PWD) --workdir=$(PWD) --network=host
RUN_CONTAINER_AS_USER := $(RUN_CONTAINER) --user $(MYSELF):$(MY_GROUP)
LINTER                := $(RUN_CONTAINER) $(LINTER_IMAGE) golangci-lint
SQL_GENERATOR         := $(RUN_CONTAINER_AS_USER) $(SQL_GENERATOR_IMAGE)

## non-PHONY folders/files

$(ARTIFACTS_DIR):
	@mkdir -p $(ARTIFACTS_DIR)

## PREREQUISITES

# setup prepares a fresh clone: creates the artifacts dir and downloads the
# module cache. This template does not vendor (platform-go's dependency tree is
# large); builds and tests run against the module cache.
.PHONY: setup
setup: $(ARTIFACTS_DIR)
	go mod download

# Vendoring targets are provided for consumers who prefer a committed vendor
# tree, but nothing depends on them by default.
.PHONY: clean_vendor
clean_vendor:
	$(SCRIPTS_DIR)/clean_vendor.sh

vendor:
	$(SCRIPTS_DIR)/vendor.sh

.PHONY: revendor
revendor: clean_vendor vendor

## FORMATTING

.PHONY: format_imports
format_imports:
	$(SCRIPTS_DIR)/format_imports.sh $(THIS) $(PWD)

.PHONY: format_go_fieldalignment
format_go_fieldalignment:
	@$(SCRIPTS_DIR)/format_go_fieldalignment.sh

.PHONY: format_go_tag_alignment
format_go_tag_alignment:
	@$(SCRIPTS_DIR)/format_go_tag_alignment.sh

.PHONY: go_fix
go_fix:
	go fix ./...

.PHONY: goimports
goimports:
	$(SCRIPTS_DIR)/goimports.sh

.PHONY: format_golang
format_golang: go_fix goimports format_imports format_go_fieldalignment format_go_tag_alignment
	@$(SCRIPTS_DIR)/format_golang.sh $(PWD)

.PHONY: format
format: format_golang

.PHONY: fmt
fmt: format

## LINTING

.PHONY: golang_lint
golang_lint:
	@$(SCRIPTS_DIR)/golang_lint.sh $(CONTAINER_RUNNER) $(LINTER_IMAGE) "$(LINTER)"

.PHONY: shellcheck
shellcheck:
	@$(SCRIPTS_DIR)/shellcheck.sh $(CONTAINER_RUNNER) $(SHELLCHECK_IMAGE) $(SCRIPTS_DIR)

.PHONY: lint
lint: golang_lint shellcheck

## GENERATED FILES

# configs renders the per-environment config files under config/ from their real
# Go objects (cmd/tools/codegen/configs). Commit the output so the checked-in
# JSON stays in lockstep with the code.
.PHONY: configs
configs:
	$(SCRIPTS_DIR)/configs.sh $(THIS)

# sqlc regenerates the typed queries under internal/store/postgres/generated from
# sqlc_queries + migrations. Edit the .sql, then re-run this; commit the
# generated Go so it stays reviewable and in lockstep.
.PHONY: sqlc
sqlc:
	$(SQL_GENERATOR) generate

## EXECUTION

# build compiles every package (fast failure on breakage) and then produces the
# binary with version metadata injected via ldflags.
.PHONY: build
build: $(ARTIFACTS_DIR)
	go build $(THIS)/...
	$(SCRIPTS_DIR)/build.sh -o $(ARTIFACTS_DIR)/$(BINARY_NAME) $(CMD_PACKAGE)

# run builds and runs the binary; pass args with `make run ARGS="version"`.
.PHONY: run
run:
	go run $(CMD_PACKAGE) $(ARGS)

## DEMO
#
# demo runs the deployment as a docker-compose cluster: shared Postgres
# (coordination state) and Redis (hot estimate store) wired into a pool of three
# identical serve heads on :8080/:8090/:8100 and a pool of eight work followers,
# seeded with the three Austin service areas (scripts/seed_demo_areas.sh). Areas
# enabled through one head are served by all three; stop a head and the survivors
# keep the freshness contract. Everything runs in containers — no local binary
# needed — so it needs only Docker (Compose v2).
#
# This is the only demo, because it is the only deployment shape: there is no
# single-node mode to show off any more. PORT moves the first head; WORKERS=n
# scales the follower pool.
PORT ?= 8080
WORKERS ?= 8
.PHONY: demo
demo:
	PORT=$(PORT) WORKERS=$(WORKERS) CONTAINER_RUNNER=$(CONTAINER_RUNNER) $(SCRIPTS_DIR)/demo.sh

# test runs the whole suite against real Postgres and Redis. The suite
# provisions its own containers, so nothing silently skips; this target starts
# one server of each kind up front so the four container-backed package binaries
# share them instead of starting one apiece. Needs Docker, like lint and sqlc.
# FAILFAST=false reports every failing package instead of stopping at the first.
.PHONY: test
test: $(ARTIFACTS_DIR)
	CONTAINER_RUNNER=$(CONTAINER_RUNNER) $(SCRIPTS_DIR)/test.sh

# test-short is the Docker-free fast loop: -short makes the container-backed
# tests skip explicitly. It leaves the Postgres and Redis backends untested, so
# run `make test` before pushing.
.PHONY: test-short
test-short: $(ARTIFACTS_DIR)
	$(SCRIPTS_DIR)/test_short.sh

# bench-store runs the hot-store benchmark gate: 300k-key BatchGet p50/p95 for
# the in-memory baseline, Postgres, and Redis. The numbers decide the blessed
# distributed-mode hot store (postgres p95 < 1s keeps the single-dependency
# default). The benchmarks provision their own containers, so this needs Docker
# unless BEELINE_TEST_POSTGRES_DSN / BEELINE_TEST_REDIS_ADDR are exported.
.PHONY: bench-store
bench-store:
	$(SCRIPTS_DIR)/bench_store.sh
