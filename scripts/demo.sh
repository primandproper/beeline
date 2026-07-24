#!/usr/bin/env bash
#
# make demo — run beeline against a fresh, gitignored SQLite database and seed three
# enabled Austin service areas so the operator console shows a realistic multi-area
# cache loading right away:
#
#   1. Downtown Austin  — one res-9 layer, full mesh (maxRadiusMeters 0): small, dense.
#   2. Austin proper    — one res-8 layer, 8 km radius bound: city-scale trips.
#   3. Austin metro     — TWO layers (the multi-layer showcase): res 7 bounded to
#                         40 km for commuter trips, plus a coarse res-6 layer bounded
#                         to 80 km recorded to serve trips beyond 20 km once
#                         distance-based layer selection lands.
#
# All three route through the raw "haversine" provider with a 5-minute target TTL.
# Creation order matters: the read path resolves an overlapping point to the enabled
# area with the LOWEST id, so the most specific area (downtown) is created first and
# the coarsest (metro) last. The server polyfills every layer of each polygon into
# its cell set (design §7). Ctrl-C stops the server.
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

# create_area POSTs the area body on stdin, enables the new area, and prints its id.
create_area() {
  local name="$1"
  local response area_id
  response="$(curl -sf -X POST "${BASE}/_config_/areas" \
    -H 'content-type: application/json' \
    --data-binary @-)"
  area_id="$(printf '%s' "${response}" | grep -o '"id":[0-9]\{1,\}' | head -1 | grep -o '[0-9]\{1,\}')"
  if [[ -z "${area_id}" ]]; then
    echo "demo: failed to create area '${name}': ${response}" >&2
    exit 1
  fi
  curl -sf -X POST "${BASE}/_config_/areas/${area_id}/enable" >/dev/null
  echo "${area_id}"
}

echo "▶ creating + enabling Downtown Austin (res 9, full mesh)"
downtown_id="$(create_area "Downtown Austin" <<'JSON'
{
  "name": "Downtown Austin",
  "layers": [
    { "resolution": 9, "minDistanceMeters": 0, "maxRadiusMeters": 0 }
  ],
  "routingProvider": "haversine",
  "targetTTL": "5m",
  "geojson": {
    "type": "Polygon",
    "coordinates": [[
      [-97.7562560714113, 30.2666553730407],
      [-97.749885173004, 30.283821900995793],
      [-97.73046473182343, 30.27869488249841],
      [-97.73766667762762, 30.257482414343396],
      [-97.7562560714113, 30.2666553730407]
    ]]
  }
}
JSON
)"

echo "▶ creating + enabling Austin proper (res 8, 8 km bound)"
austin_id="$(create_area "Austin" <<'JSON'
{
  "name": "Austin",
  "layers": [
    { "resolution": 8, "minDistanceMeters": 0, "maxRadiusMeters": 8000 }
  ],
  "routingProvider": "haversine",
  "targetTTL": "5m",
  "geojson": {
    "type": "Polygon",
    "coordinates": [[
      [-97.7940, 30.4620],
      [-97.6880, 30.4640],
      [-97.6250, 30.3300],
      [-97.6300, 30.2100],
      [-97.7200, 30.1300],
      [-97.8900, 30.1750],
      [-97.9100, 30.2450],
      [-97.8300, 30.3600],
      [-97.7940, 30.4620]
    ]]
  }
}
JSON
)"

echo "▶ creating + enabling Austin metro (layers: res 7 / 40 km + res 6 / 80 km)"
metro_id="$(create_area "Austin Metro" <<'JSON'
{
  "name": "Austin Metro",
  "layers": [
    { "resolution": 7, "minDistanceMeters": 0, "maxRadiusMeters": 40000 },
    { "resolution": 6, "minDistanceMeters": 20000, "maxRadiusMeters": 80000 }
  ],
  "routingProvider": "haversine",
  "targetTTL": "5m",
  "geojson": {
    "type": "Polygon",
    "coordinates": [[
      [-97.9600, 30.5900],
      [-97.6200, 30.5800],
      [-97.5400, 30.4500],
      [-97.5600, 30.2000],
      [-97.7400, 29.9500],
      [-97.9800, 29.9800],
      [-98.0500, 30.2500],
      [-98.0300, 30.4500],
      [-97.9600, 30.5900]
    ]]
  }
}
JSON
)"

echo
echo "✔ demo ready — open ${BASE}"
echo "  three areas are enabled and loading:"
echo "    #${downtown_id} Downtown Austin (res 9)   curl '${BASE}/_ops_/freshness?area=${downtown_id}'"
echo "    #${austin_id} Austin proper   (res 8)   curl '${BASE}/_ops_/freshness?area=${austin_id}'"
echo "    #${metro_id} Austin metro    (res 7+6) curl '${BASE}/_ops_/freshness?area=${metro_id}'"
echo "  Ctrl-C to stop (the gitignored ${DEMO_DB} is left in place; re-run to reset)."
echo

wait "${SERVER_PID}"
