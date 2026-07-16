# ENVIRONMENT
PWD      := $(shell pwd)
MYSELF   := $(shell id -u)
MY_GROUP := $(shell id -g)

# PATHS
THIS          := github.com/primandproper/template-go
BINARY_NAME   := template-go
CMD_PACKAGE   := $(THIS)/cmd/main
ARTIFACTS_DIR := artifacts
SCRIPTS_DIR   := scripts
COVERAGE_OUT  := $(ARTIFACTS_DIR)/coverage.out

# COMPUTED
TOTAL_PACKAGE_LIST := `go list $(THIS)/...`

# CONTAINER VERSIONS
LINTER_IMAGE     := golangci/golangci-lint:v2.10.1
SHELLCHECK_IMAGE := koalaman/shellcheck:stable

# COMMANDS
CONTAINER_RUNNER      := docker
RUN_CONTAINER         := $(CONTAINER_RUNNER) run --rm --volume $(PWD):$(PWD) --workdir=$(PWD) --network=host
RUN_CONTAINER_AS_USER := $(RUN_CONTAINER) --user $(MYSELF):$(MY_GROUP)
LINTER                := $(RUN_CONTAINER) $(LINTER_IMAGE) golangci-lint

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

.PHONY: test
test: $(ARTIFACTS_DIR)
	$(SCRIPTS_DIR)/test.sh
