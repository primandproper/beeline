#!/usr/bin/env bash
#
# make clusterdemo — the leader/follower split, live: one coordinator-only leader
# plus FOLLOWERS follower processes, against a workload that actually needs them.
#
# The leader runs the simple demo (scripts/demo_simple.sh: fresh SQLite db, three
# Austin areas, ~1M pairs) but with BEELINE_MATRIX_REFRESH_WORKERS=0 — it seeds and serves
# the work queue over /_work_/ and computes nothing itself. The areas route through
# the "latent-haversine" provider (each 1×K table call pays a simulated 25–120 ms of
# network latency, per config/localdev.json), so meeting the freshness contract
# genuinely requires parallel remote compute: one follower falls short of the
# required throughput, three comfortably clear it. Area leases are set to 60s so a
# remote worker's claim survives a slow batch (per-area leaseDuration overrides the
# follower's requested lease).
#
# Followers listen for health probes on PORT+1 … PORT+FOLLOWERS. The script tails
# the leader's /_ops_/freshness contract every couple of seconds so you can watch
# debt burn and achieved throughput climb past required; Ctrl-C stops everything.
# Kill an individual follower to watch throughput sag and debt regrow — then
# restart it (or add another on the next port) to catch back up:
#
#   BEELINE_MATRIX_FOLLOWER_PORT=8084 artifacts/beeline work \
#     --config config/localdev.json --leader http://localhost:8080
#
# Overridable via environment: BINARY, CONFIG, DEMO_DB, PORT, FOLLOWERS,
# FOLLOWER_WORKERS, DEMO_PROVIDER, DEMO_LEASE, STATS_INTERVAL.
set -euo pipefail

BINARY="${BINARY:-artifacts/beeline}"
CONFIG="${CONFIG:-config/localdev.json}"
PORT="${PORT:-8080}"
BASE="http://localhost:${PORT}"
# Sizing: against the latency-simulated engine (~72 ms per dense 1×K call) a
# follower worker sustains very roughly 400–800 pairs/s, so at 4 workers one
# follower lands short of the demo's ~3.6k pairs/s freshness requirement, while
# three clear it with headroom — the "add workers until smooth" story.
FOLLOWERS="${FOLLOWERS:-3}"
FOLLOWER_WORKERS="${FOLLOWER_WORKERS:-4}"
STATS_INTERVAL="${STATS_INTERVAL:-2}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

if [[ ! -x "${BINARY}" ]]; then
  echo "clusterdemo: ${BINARY} not found — run 'make build' first" >&2
  exit 1
fi

# A half-dead previous instance on the port makes the seeding requests land on the
# wrong (draining) server with baffling errors — refuse to start over one.
if curl -sf "${BASE}/_ops_/live" >/dev/null 2>&1; then
  echo "clusterdemo: something is already serving on ${BASE} — stop it (or set PORT) first" >&2
  exit 1
fi

PIDS=()
cleanup() {
  # Followers first, then the leader (demo_simple.sh's own trap tears down its server).
  for ((i = ${#PIDS[@]} - 1; i >= 0; i--)); do
    kill "${PIDS[i]}" 2>/dev/null || true
  done
  for pid in "${PIDS[@]}"; do
    wait "${pid}" 2>/dev/null || true
  done
}
trap cleanup INT TERM EXIT

# Leader: the standard demo, minus local compute, with the network-bound provider.
echo "▶ starting coordinator-only leader on ${BASE} (refreshWorkers=0, provider: ${DEMO_PROVIDER:-latent-haversine})"
BEELINE_MATRIX_REFRESH_WORKERS=0 \
  DEMO_PROVIDER="${DEMO_PROVIDER:-latent-haversine}" \
  DEMO_LEASE="${DEMO_LEASE:-60s}" \
  "${SCRIPT_DIR}/demo_simple.sh" &
PIDS+=($!)

# Wait until the leader is live and all three demo areas are enabled. Enable is
# synchronous (it returns after polyfill + seed), so three enabled areas in the
# registry means the full working set is queued.
echo "▶ waiting for the leader to seed its areas…"
# Every stage of the poll pipeline can fail while the leader is still coming up
# (curl refused, grep no-match); the trailing `|| true` keeps set -e/pipefail from
# killing the whole cluster over a not-yet-ready poll.
enabled=0
for _ in $(seq 1 300); do
  enabled="$(curl -sf "${BASE}/_config_/areas" 2>/dev/null | grep -o '"enabled":true' | wc -l | tr -d ' ' || true)"
  if [[ "${enabled:-0}" -ge 3 ]]; then
    break
  fi
  sleep 0.2
done
if [[ "${enabled:-0}" -lt 3 ]]; then
  echo "clusterdemo: leader never seeded its three demo areas" >&2
  exit 1
fi
working_set="$(curl -sf "${BASE}/_ops_/freshness" | sed -n 's/.*"workingSet":\([0-9]*\).*/\1/p' || true)"

echo "▶ starting ${FOLLOWERS} followers (${FOLLOWER_WORKERS} workers each)"
for i in $(seq 1 "${FOLLOWERS}"); do
  health_port=$((PORT + i))
  BEELINE_MATRIX_FOLLOWER_PORT="${health_port}" \
    BEELINE_MATRIX_FOLLOWER_WORKERS="${FOLLOWER_WORKERS}" \
    "${BINARY}" work --config "${CONFIG}" --leader "${BASE}" &
  PIDS+=($!)
  echo "    follower ${i}: health on http://localhost:${health_port}/_ops_/ready"
done

echo
echo "✔ cluster ready — console: ${BASE}   freshness: ${BASE}/_ops_/freshness"
echo "  ${working_set} pairs in the working set; all compute is remote. Ctrl-C stops everything."
echo

# Tail the freshness contract: debt should burn down and achieved throughput climb
# past required — the 'add a node, watch % climb' property from design §8.
while sleep "${STATS_INTERVAL}"; do
  stats="$(curl -sf "${BASE}/_ops_/freshness" 2>/dev/null || true)"
  if [[ -z "${stats}" ]]; then
    echo "  leader unreachable"
    continue
  fi
  debt="$(printf '%s' "${stats}" | sed -n 's/.*"debt":\([0-9]*\).*/\1/p')"
  required="$(printf '%s' "${stats}" | sed -n 's/.*"requiredThroughput":\([0-9.]*\).*/\1/p')"
  achieved="$(printf '%s' "${stats}" | sed -n 's/.*"achievedThroughput":\([0-9.]*\).*/\1/p')"
  printf '  debt %9s / %s pairs   throughput %8.0f achieved / %.0f required (pairs/s)\n' \
    "${debt}" "${working_set}" "${achieved}" "${required}"
done
