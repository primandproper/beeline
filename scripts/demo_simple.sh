#!/usr/bin/env bash
#
# make simpledemo — the zero-dependency demo: one `serve` process against a fresh,
# gitignored SQLite database, seeded with the three canonical Austin service areas
# (scripts/seed_demo_areas.sh) so the operator console shows a realistic multi-area
# cache loading right away.
#
# Everything is in-process: the in-memory hot store and freshness index, the SQLite
# area store, and the DEMO_PROVIDER routing provider — the raw "haversine" in-process
# engine by default, so estimates compute in nanoseconds and the working set goes
# fresh almost immediately. (demo_cluster.sh reuses this script with
# DEMO_PROVIDER=latent-haversine to model a network-bound engine instead.) Ctrl-C
# stops the server.
#
# Overridable via environment: BINARY, CONFIG, DEMO_DB, PORT, DEMO_PROVIDER, DEMO_LEASE.
set -euo pipefail

BINARY="${BINARY:-artifacts/beeline}"
CONFIG="${CONFIG:-config/localdev.json}"
DEMO_DB="${DEMO_DB:-artifacts/demo.db}"
PORT="${PORT:-8080}"
BASE="http://localhost:${PORT}"
DEMO_PROVIDER="${DEMO_PROVIDER:-haversine}"
DEMO_LEASE="${DEMO_LEASE:-15s}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ ! -x "${BINARY}" ]]; then
  echo "simpledemo: ${BINARY} not found — run 'make build' first" >&2
  exit 1
fi

# Start from scratch: drop any previous demo database (and its WAL sidecars).
rm -f "${DEMO_DB}" "${DEMO_DB}-wal" "${DEMO_DB}-shm"
mkdir -p "$(dirname "${DEMO_DB}")"

echo "▶ starting beeline on ${BASE} (database: ${DEMO_DB})"
BEELINE_MATRIX_DATABASE_PATH="${DEMO_DB}" BEELINE_MATRIX_SERVER_PORT="${PORT}" \
  "${BINARY}" serve --config "${CONFIG}" &
SERVER_PID=$!

cleanup() {
  kill "${SERVER_PID}" 2>/dev/null || true
  wait "${SERVER_PID}" 2>/dev/null || true
}
trap cleanup INT TERM EXIT

echo "▶ waiting for the server…"
for _ in $(seq 1 50); do
  if curl -sf "${BASE}/_ops_/live" >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

read -r downtown_id austin_id metro_id \
  <<<"$("${SCRIPT_DIR}/seed_demo_areas.sh" "${BASE}" "${DEMO_PROVIDER}" "${DEMO_LEASE}")"

echo
echo "✔ demo ready — open ${BASE}"
echo "  three areas are enabled and loading:"
echo "    #${downtown_id} Downtown Austin (res 9)   curl '${BASE}/_ops_/freshness?area=${downtown_id}'"
echo "    #${austin_id} Austin proper   (res 8)   curl '${BASE}/_ops_/freshness?area=${austin_id}'"
echo "    #${metro_id} Austin metro    (res 7+6) curl '${BASE}/_ops_/freshness?area=${metro_id}'"
echo "  Ctrl-C to stop (the gitignored ${DEMO_DB} is left in place; re-run to reset)."
echo

wait "${SERVER_PID}"
