#!/usr/bin/env bash
#
# make test-integration — run the test suite with real Postgres + Redis behind
# the env-gated integration tests (pgtest / the redis store tests). Both
# containers are started fresh on random host ports, the gate env vars are
# exported, the full internal test suite runs (the gated tests now execute
# instead of skipping), and the containers are removed on exit.
#
# Overridable via environment: CONTAINER_RUNNER, POSTGRES_IMAGE, REDIS_IMAGE,
# PG_PORT, REDIS_PORT (defaults pick free-ish high ports so parallel checkouts
# don't collide).
set -euo pipefail

CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16-alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
PG_PORT="${PG_PORT:-55432}"
REDIS_PORT="${REDIS_PORT:-56379}"

PG_CONTAINER="beeline-test-postgres-$$"
REDIS_CONTAINER="beeline-test-redis-$$"

cleanup() {
  "${CONTAINER_RUNNER}" rm -f "${PG_CONTAINER}" "${REDIS_CONTAINER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "test-integration: starting ${POSTGRES_IMAGE} on :${PG_PORT} and ${REDIS_IMAGE} on :${REDIS_PORT}"
"${CONTAINER_RUNNER}" run --rm --detach --name "${PG_CONTAINER}" \
  --publish "${PG_PORT}:5432" \
  --env POSTGRES_USER=beeline --env POSTGRES_PASSWORD=beeline --env POSTGRES_DB=beeline \
  "${POSTGRES_IMAGE}" >/dev/null
"${CONTAINER_RUNNER}" run --rm --detach --name "${REDIS_CONTAINER}" \
  --publish "${REDIS_PORT}:6379" \
  "${REDIS_IMAGE}" >/dev/null

# Postgres readiness: pg_isready inside the container avoids needing psql host-side.
for _ in $(seq 1 60); do
  if "${CONTAINER_RUNNER}" exec "${PG_CONTAINER}" pg_isready -U beeline -d beeline >/dev/null 2>&1; then
    pg_ready=1
    break
  fi
  sleep 0.5
done
if [[ -z "${pg_ready:-}" ]]; then
  echo "test-integration: postgres never became ready" >&2
  exit 1
fi

for _ in $(seq 1 60); do
  if "${CONTAINER_RUNNER}" exec "${REDIS_CONTAINER}" redis-cli ping >/dev/null 2>&1; then
    redis_ready=1
    break
  fi
  sleep 0.5
done
if [[ -z "${redis_ready:-}" ]]; then
  echo "test-integration: redis never became ready" >&2
  exit 1
fi

export BEELINE_TEST_POSTGRES_DSN="postgres://beeline:beeline@localhost:${PG_PORT}/beeline?sslmode=disable"
export BEELINE_TEST_REDIS_ADDR="localhost:${REDIS_PORT}"

# Same shape as scripts/test.sh, over the packages that host integration tests.
# shellcheck disable=SC2046
CGO_ENABLED=1 go test -shuffle=on -race -vet=all -failfast \
  $(go list github.com/primandproper/beeline/... | grep -Ev '(cmd)')
