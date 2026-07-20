#!/usr/bin/env bash
#
# make demo — run beeline against a fresh, gitignored SQLite database and seed one
# enabled service area (Lake Travis, Austin) so the operator console shows the cache
# loading right away. The server polyfills the demo polygon into its cell set (design
# §7); refine it by hand from the console to carve out the reservoir. Ctrl-C stops the
# server.
#
# Overridable via environment: BINARY, CONFIG, DEMO_DB, PORT.
set -euo pipefail

BINARY="${BINARY:-artifacts/beeline}"
CONFIG="${CONFIG:-config/localdev.json}"
DEMO_DB="${DEMO_DB:-artifacts/demo.db}"
PORT="${PORT:-8080}"
BASE="http://localhost:${PORT}"

if [[ ! -x "${BINARY}" ]]; then
  echo "demo: ${BINARY} not found — run 'make build' first" >&2
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

echo "▶ creating + enabling the demo area (Lake Travis, Austin)"
response="$(curl -sf -X POST "${BASE}/_config_/areas" \
  -H 'content-type: application/json' \
  --data-binary @- <<'JSON'
{
  "name": "Lake Travis (demo)",
  "resolution": 9,
  "maxRadiusMeters": 1200,
  "geojson": {
    "type": "Polygon",
    "coordinates": [[[-98.06, 30.31], [-97.99, 30.31], [-97.99, 30.38], [-98.06, 30.38], [-98.06, 30.31]]]
  }
}
JSON
)"

area_id="$(printf '%s' "${response}" | grep -o '"id":[0-9]\{1,\}' | head -1 | grep -o '[0-9]\{1,\}')"
if [[ -z "${area_id}" ]]; then
  echo "demo: failed to create area: ${response}" >&2
  exit 1
fi

curl -sf -X POST "${BASE}/_config_/areas/${area_id}/enable" >/dev/null

echo
echo "✔ demo ready — open ${BASE}"
echo "  area #${area_id} is enabled and loading. Watch it in the console, or:"
echo "    curl '${BASE}/_ops_/freshness?area=${area_id}'"
echo "  Ctrl-C to stop (the gitignored ${DEMO_DB} is left in place; re-run to reset)."
echo

wait "${SERVER_PID}"
