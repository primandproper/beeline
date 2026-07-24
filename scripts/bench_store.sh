#!/usr/bin/env bash
#
# make bench-store — the hot-store benchmark gate (design decision: which
# backend is the blessed distributed-mode default). Starts Postgres + Redis,
# seeds 300k estimates into each store, and measures full-batch BatchGet wall
# time (p50/p95 reported per benchmark) plus a 128-key hot-path read, alongside
# the in-memory baseline. Decision rule: Postgres p95 < 1s on the 300k read →
# postgres stays the single-dependency default; otherwise redis carries the hot
# store.
#
# Overridable via environment: CONTAINER_RUNNER, POSTGRES_IMAGE, REDIS_IMAGE,
# PG_PORT, REDIS_PORT, BENCH_TIME (go test -benchtime; iterations, default 10x).
set -euo pipefail

CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16-alpine}"
REDIS_IMAGE="${REDIS_IMAGE:-redis:7-alpine}"
PG_PORT="${PG_PORT:-55433}"
REDIS_PORT="${REDIS_PORT:-56380}"
BENCH_TIME="${BENCH_TIME:-10x}"

PG_CONTAINER="beeline-bench-postgres-$$"
REDIS_CONTAINER="beeline-bench-redis-$$"

cleanup() {
  "${CONTAINER_RUNNER}" rm -f "${PG_CONTAINER}" "${REDIS_CONTAINER}" >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "bench-store: starting ${POSTGRES_IMAGE} on :${PG_PORT} and ${REDIS_IMAGE} on :${REDIS_PORT}"
"${CONTAINER_RUNNER}" run --rm --detach --name "${PG_CONTAINER}" \
  --publish "${PG_PORT}:5432" \
  --env POSTGRES_USER=beeline --env POSTGRES_PASSWORD=beeline --env POSTGRES_DB=beeline \
  "${POSTGRES_IMAGE}" >/dev/null
"${CONTAINER_RUNNER}" run --rm --detach --name "${REDIS_CONTAINER}" \
  --publish "${REDIS_PORT}:6379" \
  "${REDIS_IMAGE}" >/dev/null

for _ in $(seq 1 60); do
  if "${CONTAINER_RUNNER}" exec "${PG_CONTAINER}" pg_isready -U beeline -d beeline >/dev/null 2>&1; then
    pg_ready=1
    break
  fi
  sleep 0.5
done
if [[ -z "${pg_ready:-}" ]]; then
  echo "bench-store: postgres never became ready" >&2
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
  echo "bench-store: redis never became ready" >&2
  exit 1
fi

export BEELINE_TEST_POSTGRES_DSN="postgres://beeline:beeline@localhost:${PG_PORT}/beeline?sslmode=disable"
export BEELINE_TEST_REDIS_ADDR="localhost:${REDIS_PORT}"

go test -run '^$' -bench 'BenchmarkBatchGet' -benchtime "${BENCH_TIME}" \
  github.com/primandproper/beeline/internal/store/memory \
  github.com/primandproper/beeline/internal/store/postgres \
  github.com/primandproper/beeline/internal/store/redis
