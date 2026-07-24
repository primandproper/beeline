#!/usr/bin/env bash
#
# make demo-multihead — the distributed deployment, live: two identical `serve`
# heads over one shared Postgres (freshness index, operator config, estimate
# cache), plus FOLLOWERS follower processes split across both heads. There is
# no "leader" anymore — an area enabled through head A is served and
# coordinated by head B within one config-poll interval, and either head can
# die without stopping the other (or the followers pointed at the survivor).
#
# The script starts a throwaway Postgres container, both heads (coordinator
# only, refreshWorkers=0, per config/cluster.json), creates and enables one
# demo area through head A, then tails BOTH heads' /_ops_/freshness side by
# side: the acceptance signal is identical debt/throughput lines from both
# heads while the followers drain the one shared queue. Ctrl-C stops
# everything, container included.
#
# Try killing head A mid-run: head B and its followers keep burning debt, and
# `/estimate` against head B keeps answering for the area A enabled.
#
# Overridable via environment: BINARY, CONFIG, PORT (head A; head B is
# PORT+10), FOLLOWERS, FOLLOWER_WORKERS, PG_PORT, POSTGRES_IMAGE,
# CONTAINER_RUNNER, STATS_INTERVAL.
set -euo pipefail

BINARY="${BINARY:-artifacts/beeline}"
CONFIG="${CONFIG:-config/cluster.json}"
PORT="${PORT:-8080}"
PORT_B=$((PORT + 10))
BASE_A="http://localhost:${PORT}"
BASE_B="http://localhost:${PORT_B}"
FOLLOWERS="${FOLLOWERS:-3}"
FOLLOWER_WORKERS="${FOLLOWER_WORKERS:-4}"
PG_PORT="${PG_PORT:-55432}"
POSTGRES_IMAGE="${POSTGRES_IMAGE:-postgres:16-alpine}"
CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"
STATS_INTERVAL="${STATS_INTERVAL:-2}"

PG_CONTAINER="beeline-multihead-pg-$$"
PG_URL="postgres://beeline:beeline@localhost:${PG_PORT}/beeline?sslmode=disable"

if [[ ! -x "${BINARY}" ]]; then
  echo "demo-multihead: ${BINARY} not found — run 'make build' first" >&2
  exit 1
fi
for base in "${BASE_A}" "${BASE_B}"; do
  if curl -sf "${base}/_ops_/live" >/dev/null 2>&1; then
    echo "demo-multihead: something is already serving on ${base} — stop it (or set PORT) first" >&2
    exit 1
  fi
done

PIDS=()
cleanup() {
  for ((i = ${#PIDS[@]} - 1; i >= 0; i--)); do
    kill "${PIDS[i]}" 2>/dev/null || true
  done
  for pid in "${PIDS[@]}"; do
    wait "${pid}" 2>/dev/null || true
  done
  "${CONTAINER_RUNNER}" rm -f "${PG_CONTAINER}" >/dev/null 2>&1 || true
}
trap cleanup INT TERM EXIT

echo "▶ starting ${POSTGRES_IMAGE} on :${PG_PORT}"
"${CONTAINER_RUNNER}" run --rm --detach --name "${PG_CONTAINER}" \
  --publish "${PG_PORT}:5432" \
  --env POSTGRES_USER=beeline --env POSTGRES_PASSWORD=beeline --env POSTGRES_DB=beeline \
  "${POSTGRES_IMAGE}" >/dev/null
for _ in $(seq 1 60); do
  if "${CONTAINER_RUNNER}" exec "${PG_CONTAINER}" pg_isready -U beeline -d beeline >/dev/null 2>&1; then
    pg_ready=1
    break
  fi
  sleep 0.5
done
if [[ -z "${pg_ready:-}" ]]; then
  echo "demo-multihead: postgres never became ready" >&2
  exit 1
fi

start_head() {
  local port="$1"
  BEELINE_MATRIX_SERVER_PORT="${port}" \
    BEELINE_MATRIX_BACKEND_POSTGRES_URL="${PG_URL}" \
    "${BINARY}" serve --config "${CONFIG}" &
  PIDS+=($!)
}

echo "▶ starting head A on ${BASE_A} and head B on ${BASE_B} (both coordinator-only)"
start_head "${PORT}"
start_head "${PORT_B}"
for base in "${BASE_A}" "${BASE_B}"; do
  up=""
  for _ in $(seq 1 120); do
    if curl -sf "${base}/_ops_/live" >/dev/null 2>&1; then
      up=1
      break
    fi
    sleep 0.5
  done
  if [[ -z "${up}" ]]; then
    echo "demo-multihead: head on ${base} never came up" >&2
    exit 1
  fi
done

echo "▶ creating + enabling Downtown Austin through head A (latent-haversine, res 9)"
area_id="$(curl -sf -X POST "${BASE_A}/_config_/areas" \
  -H 'Content-Type: application/json' \
  --data @- <<'JSON' | sed -n 's/.*"id":\([0-9]*\).*/\1/p'
{
  "name": "Downtown Austin",
  "layers": [ { "resolution": 9, "minDistanceMeters": 0, "maxRadiusMeters": 0 } ],
  "routingProvider": "latent-haversine",
  "targetTTL": "5m",
  "leaseDuration": "60s",
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
curl -sf -X POST "${BASE_A}/_config_/areas/${area_id}/enable" >/dev/null
echo "  area #${area_id} enabled via head A"

echo "▶ starting ${FOLLOWERS} followers, alternating between the two heads"
for i in $(seq 1 "${FOLLOWERS}"); do
  if (( i % 2 == 1 )); then
    target="${BASE_A}"
  else
    target="${BASE_B}"
  fi
  BEELINE_MATRIX_FOLLOWER_PORT=$((PORT_B + i)) \
    BEELINE_MATRIX_FOLLOWER_WORKERS="${FOLLOWER_WORKERS}" \
    "${BINARY}" work --config "${CONFIG}" --leader "${target}" &
  PIDS+=($!)
  echo "    follower ${i} → ${target}"
done

echo
echo "✔ cluster ready — consoles: ${BASE_A} and ${BASE_B} (same shared state)"
echo "  estimate via head B for the area enabled via head A:"
echo "    curl '${BASE_B}/estimate?origin=30.27,-97.745&dest=30.275,-97.74'"
echo "  kill head A ($(echo "${PIDS[0]}")) and watch head B keep the contract."
echo

while sleep "${STATS_INTERVAL}"; do
  line=""
  for base in "${BASE_A}" "${BASE_B}"; do
    stats="$(curl -sf "${base}/_ops_/freshness?area=${area_id}" 2>/dev/null || true)"
    if [[ -z "${stats}" ]]; then
      line+="  [${base##*:}] head down"
      continue
    fi
    debt="$(printf '%s' "${stats}" | sed -n 's/.*"debt":\([0-9]*\).*/\1/p')"
    achieved="$(printf '%s' "${stats}" | sed -n 's/.*"achievedThroughput":\([0-9.]*\).*/\1/p')"
    line+="  [${base##*:}] debt=${debt} achieved=${achieved}/s"
  done
  echo "${line}"
done
