#!/usr/bin/env bash
#
# make fulldemo — the whole distributed deployment, containerized: a
# docker-compose cluster of shared Postgres (coordination state), Redis (the hot
# estimate store), a pool of THREE identical `serve` heads, and a pool of EIGHT
# `work` followers. See docker-compose.yml for the wiring.
#
# The script builds the image, brings the cluster up, seeds the three canonical
# demo areas through head A (scripts/seed_demo_areas.sh, against the simulated
# network-latency provider so the worker pool has real work to do), then tails
# all three heads' /_ops_/freshness side by side. The acceptance signal is
# identical debt/throughput lines from every head while the eight followers drain
# the one shared queue.
#
# Things worth trying mid-run:
#
#   docker compose -p beeline-fulldemo stop head-b        # A and C keep the contract
#   docker compose -p beeline-fulldemo up -d --scale worker=16
#   curl 'localhost:8100/estimate?origin=30.27,-97.745&dest=30.275,-97.74'
#                                                         # head C answers for head A's areas
#
# Ctrl-C stops and removes everything, containers and volumes included.
#
# Overridable via environment: CONTAINER_RUNNER, COMPOSE_FILE, PORT (head A;
# heads B/C default to PORT+10/PORT+20), WORKERS, FOLLOWER_WORKERS, PG_PORT,
# REDIS_PORT, POSTGRES_IMAGE, REDIS_IMAGE, DEMO_PROVIDER, DEMO_LEASE,
# STATS_INTERVAL, LOG_LEVEL.
set -euo pipefail

CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"
PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_DIR="${PROJECT_ROOT}/scripts"
COMPOSE_FILE="${COMPOSE_FILE:-${PROJECT_ROOT}/docker-compose.yml}"
PROJECT_NAME="beeline-fulldemo"

PORT="${PORT:-8080}"
PORT_B="${PORT_B:-$((PORT + 10))}"
PORT_C="${PORT_C:-$((PORT + 20))}"
BASE_A="http://localhost:${PORT}"
BASE_B="http://localhost:${PORT_B}"
BASE_C="http://localhost:${PORT_C}"

# The pools. Three heads are fixed in the compose file (they publish distinct
# host ports); the worker pool is a replica count, so it scales from here.
WORKERS="${WORKERS:-8}"
FOLLOWER_WORKERS="${FOLLOWER_WORKERS:-4}"
DEMO_PROVIDER="${DEMO_PROVIDER:-latent-haversine}"
DEMO_LEASE="${DEMO_LEASE:-60s}"
STATS_INTERVAL="${STATS_INTERVAL:-2}"

export PORT PORT_B PORT_C WORKERS FOLLOWER_WORKERS

if ! "${CONTAINER_RUNNER}" compose version >/dev/null 2>&1; then
  echo "fulldemo: '${CONTAINER_RUNNER} compose' is unavailable — install Docker Compose v2" >&2
  exit 1
fi

for base in "${BASE_A}" "${BASE_B}" "${BASE_C}"; do
  if curl -sf "${base}/_ops_/live" >/dev/null 2>&1; then
    echo "fulldemo: something is already serving on ${base} — stop it (or set PORT) first" >&2
    exit 1
  fi
done

COMPOSE=("${CONTAINER_RUNNER}" compose --file "${COMPOSE_FILE}" --project-name "${PROJECT_NAME}")

cleanup() {
  echo
  echo "▶ tearing the cluster down"
  "${COMPOSE[@]}" down --volumes --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup INT TERM EXIT

# Version metadata for the image: .dockerignore keeps .git out of the build
# context, so the ldflags values ride in as build args instead (see Dockerfile).
COMMIT_HASH="$(git -C "${PROJECT_ROOT}" rev-parse HEAD 2>/dev/null || echo unknown)"
COMMIT_TIME="$(git -C "${PROJECT_ROOT}" log -1 --format=%cI HEAD 2>/dev/null || echo unknown)"
BUILD_TIME="$(date -u -Iseconds 2>/dev/null || echo unknown)"
export COMMIT_HASH COMMIT_TIME BUILD_TIME

echo "▶ building the beeline image and starting the cluster"
echo "  postgres + redis, 3 heads (:${PORT} :${PORT_B} :${PORT_C}), ${WORKERS} workers × ${FOLLOWER_WORKERS} threads"
"${COMPOSE[@]}" up --build --detach

echo "▶ waiting for the heads…"
for base in "${BASE_A}" "${BASE_B}" "${BASE_C}"; do
  up=""
  for _ in $(seq 1 240); do
    if curl -sf "${base}/_ops_/ready" >/dev/null 2>&1; then
      up=1
      break
    fi
    sleep 0.5
  done
  if [[ -z "${up}" ]]; then
    echo "fulldemo: head on ${base} never became ready — '${CONTAINER_RUNNER} compose -p ${PROJECT_NAME} logs' has the detail" >&2
    exit 1
  fi
done

# Seeded through head A only. Every other head picks the areas up from the shared
# Postgres within one configPollInterval (2s per config/cluster.json) — that
# convergence is the point of the demo.
read -r downtown_id austin_id metro_id \
  <<<"$("${SCRIPT_DIR}/seed_demo_areas.sh" "${BASE_A}" "${DEMO_PROVIDER}" "${DEMO_LEASE}")"

working_set="$(curl -sf "${BASE_A}/_ops_/freshness" | sed -n 's/.*"workingSet":\([0-9]*\).*/\1/p' || true)"

echo
echo "✔ cluster ready — three consoles over one shared state:"
echo "    head A ${BASE_A}   head B ${BASE_B}   head C ${BASE_C}"
echo "  areas #${downtown_id} downtown, #${austin_id} Austin, #${metro_id} metro seeded via head A"
echo "  ${working_set} pairs in the working set; all compute is in the ${WORKERS}-follower pool."
echo "  logs:        ${CONTAINER_RUNNER} compose -p ${PROJECT_NAME} logs -f head-a"
echo "  scale:       ${CONTAINER_RUNNER} compose -p ${PROJECT_NAME} up -d --scale worker=16"
echo "  kill a head: ${CONTAINER_RUNNER} compose -p ${PROJECT_NAME} stop head-b"
echo "  Ctrl-C stops and removes everything."
echo

# Tail every head's view of the same contract. All three lines should agree (they
# read one Postgres index), and debt should burn down as the pool computes.
while sleep "${STATS_INTERVAL}"; do
  line=""
  for base in "${BASE_A}" "${BASE_B}" "${BASE_C}"; do
    stats="$(curl -sf "${base}/_ops_/freshness" 2>/dev/null || true)"
    if [[ -z "${stats}" ]]; then
      line+="  [:${base##*:}] down"
      continue
    fi
    debt="$(printf '%s' "${stats}" | sed -n 's/.*"debt":\([0-9]*\).*/\1/p')"
    achieved="$(printf '%s' "${stats}" | sed -n 's/.*"achievedThroughput":\([0-9.]*\).*/\1/p')"
    line+="$(printf '  [:%s] debt=%-9s achieved=%-8.0f' "${base##*:}" "${debt:-?}" "${achieved:-0}")"
  done
  echo "${line}"
done
