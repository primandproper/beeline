#!/usr/bin/env bash
#
# make test-short — the Docker-free fast loop.
#
# -short is the explicit opt-out from the container-backed tests: pgtest and
# redistest both check testing.Short() and skip rather than provision. Every
# other test runs. Use this when iterating without a Docker daemon; use
# `make test` before pushing, since this leaves the Postgres and Redis backends
# untested.
#
# Overridable via environment: FAILFAST (see test.sh). Extra arguments are
# forwarded to `go test`.
set -euo pipefail

FAILFAST="${FAILFAST:-true}"

flags=(-short -shuffle=on -race -vet=all)

case "${FAILFAST}" in
  false | FALSE | False | 0 | no | NO | No) ;;
  *) flags+=(-failfast) ;;
esac

packages=()
while IFS= read -r pkg; do
  packages+=("${pkg}")
done < <(go list github.com/primandproper/beeline/... | grep -Ev '(cmd)')

CGO_ENABLED=1 go test "${flags[@]}" "$@" "${packages[@]}"
