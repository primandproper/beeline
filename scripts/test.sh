#!/usr/bin/env bash
#
# make test — the test suite over every package except cmd, against real
# Postgres and Redis.
#
# The suite provisions its own dependencies: internal/store/postgres/pgtest and
# internal/store/redis/redistest each start a container when no server is
# configured, so nothing silently skips. This script exists only to make that
# cheap. It starts ONE Postgres and ONE Redis up front and exports the two
# address variables, so all four container-backed package binaries share a single
# server each instead of starting one apiece. Bare `go test ./...` still works —
# it just pays for a container per package binary.
#
# Set SELF_PROVISION=true to skip the preamble and let the packages do it, which
# is the path CI-less environments and `go test` users get by default.
#
# Overridable via environment:
#
#   FAILFAST=false     Drop -failfast, so one run reports every failing package
#                      instead of stopping at the first. Wanted when a shared
#                      conformance suite is expected to surface several
#                      divergences at once and they should be triaged together
#                      rather than one red test at a time.
#
#   SELF_PROVISION     Skip the shared containers (see above).
#   CONTAINER_RUNNER   docker (default) or a compatible CLI.
#   POSTGRES_IMAGE / REDIS_IMAGE / PG_PORT / REDIS_PORT
#
# Extra arguments are forwarded to `go test` ahead of the package list, so a
# targeted run without failfast is:
#
#   FAILFAST=false scripts/test.sh -run TestConformance
set -euo pipefail

FAILFAST="${FAILFAST:-true}"
SELF_PROVISION="${SELF_PROVISION:-false}"
CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16-alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
PG_PORT="${PG_PORT:-55432}"
REDIS_PORT="${REDIS_PORT:-56379}"

PG_CONTAINER="beeline-test-postgres-$$"
REDIS_CONTAINER="beeline-test-redis-$$"

started_containers=false

cleanup() {
  if [[ "${started_containers}" == "true" ]]; then
    "${CONTAINER_RUNNER}" rm -f "${PG_CONTAINER}" "${REDIS_CONTAINER}" >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT

# wait_for polls a readiness command inside a container for up to 30s.
wait_for() {
  local container="$1"
  shift
  for _ in $(seq 1 60); do
    if "${CONTAINER_RUNNER}" exec "${container}" "$@" >/dev/null 2>&1; then
      return 0
    fi
    sleep 0.5
  done

  echo "test: ${container} never became ready" >&2

  return 1
}

# start_shared_containers brings up one server of each kind for the whole run.
# max_connections is lifted for the same reason pgtest lifts it on its own
# container: every package binary's schema-isolated tests share this one server,
# each holding a client pool plus an admin pool.
start_shared_containers() {
  echo "test: starting ${POSTGRES_IMAGE} on :${PG_PORT} and ${REDIS_IMAGE} on :${REDIS_PORT}"
  started_containers=true

  "${CONTAINER_RUNNER}" run --rm --detach --name "${PG_CONTAINER}" \
    --publish "${PG_PORT}:5432" \
    --env POSTGRES_USER=beeline --env POSTGRES_PASSWORD=beeline --env POSTGRES_DB=beeline \
    "${POSTGRES_IMAGE}" -c fsync=off -c max_connections=200 >/dev/null
  "${CONTAINER_RUNNER}" run --rm --detach --name "${REDIS_CONTAINER}" \
    --publish "${REDIS_PORT}:6379" \
    "${REDIS_IMAGE}" >/dev/null

  # pg_isready inside the container avoids needing psql host-side.
  wait_for "${PG_CONTAINER}" pg_isready -U beeline -d beeline
  wait_for "${REDIS_CONTAINER}" redis-cli ping

  export BEELINE_TEST_POSTGRES_DSN="postgres://beeline:beeline@localhost:${PG_PORT}/beeline?sslmode=disable"
  export BEELINE_TEST_REDIS_ADDR="localhost:${REDIS_PORT}"
}

if [[ "${SELF_PROVISION}" != "true" ]]; then
  start_shared_containers
else
  echo "test: SELF_PROVISION=true — each package binary starts its own containers"
fi

flags=(-shuffle=on -race -vet=all)

# Anything but an explicitly falsey value keeps -failfast, so the default stays
# today's behavior: stop at the first broken package.
case "${FAILFAST}" in
  false | FALSE | False | 0 | no | NO | No) ;;
  *) flags+=(-failfast) ;;
esac

packages=()
while IFS= read -r pkg; do
  packages+=("${pkg}")
done < <(go list github.com/primandproper/beeline/... | grep -Ev '(cmd)')

CGO_ENABLED=1 go test "${flags[@]}" "$@" "${packages[@]}"
