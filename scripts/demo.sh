#!/usr/bin/env bash
#
# make demo — the whole deployment on a local k3s cluster (k3d), with both pools
# autoscaling. Shared Postgres (coordination state: freshness index, operator
# config, advisory-lock election), Redis (the hot estimate store), a pool of
# identical `serve` heads behind one Service, a pool of `work` followers, and a
# bundled k6 load generator aimed at the heads.
#
# The two autoscalers are the point:
#
#   * The head pool scales on CPU (a HorizontalPodAutoscaler over the
#     metrics-server k3s already ships), because the read path is CPU-bound. The
#     load generator is what makes it move.
#   * The follower pool scales on freshness DEBT — the §3 contract read straight
#     off GET /_ops_/freshness by KEDA's metrics-api scaler, with no metrics
#     pipeline in between — all the way down to zero when there is nothing stale.
#
# Seeded with the three canonical Austin service areas
# (scripts/seed_demo_areas.sh, against the simulated network-latency provider so
# the follower pool has real work to do). Their per-area targetTTL is 5m, so the
# whole ~1.1M-pair working set re-stales at once every five minutes: leave this
# running and the pools scale up and back down on their own, unattended. That
# sawtooth is the best thing to watch.
#
# Nothing here is a "leader" in the durable sense: every head is stateless and
# disposable, an area enabled through one head is served by all of them within a
# config-poll interval, and a follower's leader URL just names the head Service.
#
# This script never reads or changes your current kubectl context. It targets
# k3d-${CLUSTER} explicitly on every call, and every command it suggests carries
# the same --context so copy-paste stays where you meant it.
#
# Things worth trying mid-run are printed once the cluster is up.
#
# Ctrl-C deletes the cluster. Needs Docker, kubectl and k3d (`brew install k3d`);
# no local binary.
#
# Overridable via environment: ENVIRONMENT, CLUSTER, NAMESPACE, IMAGE, PORT,
# K3D_AGENTS, KEDA_VERSION, RPS, TABLE_GRID, FOLLOWER_WORKERS, WORKER_MAX,
# HEAD_MIN, HEAD_MAX, DEMO_PROVIDER, DEMO_LEASE, STATS_INTERVAL, LOG_LEVEL,
# KEEP_CLUSTER, RECREATE, SKIP_BUILD, CONTAINER_RUNNER.
set -euo pipefail

PROJECT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SCRIPT_DIR="${PROJECT_ROOT}/scripts"

ENVIRONMENT="${ENVIRONMENT:-local}"
ENV_DIR="${PROJECT_ROOT}/deploy/environments/${ENVIRONMENT}"
CLUSTER="${CLUSTER:-beeline-${ENVIRONMENT}}"
NAMESPACE="${NAMESPACE:-beeline-${ENVIRONMENT}}"
IMAGE="${IMAGE:-beeline:demo}"
CONTAINER_RUNNER="${CONTAINER_RUNNER:-docker}"

PORT="${PORT:-8080}"
BASE="http://localhost:${PORT}"
K3D_AGENTS="${K3D_AGENTS:-2}"
KEDA_VERSION="${KEDA_VERSION:-2.20.1}"

# Pool knobs. The follower replica count is KEDA's now, so WORKER_MAX is a
# ceiling rather than a fixed size; FOLLOWER_WORKERS is the per-pod thread count,
# which multiplies against it.
RPS="${RPS:-25}"
TABLE_GRID="${TABLE_GRID:-24}"
FOLLOWER_WORKERS="${FOLLOWER_WORKERS:-4}"
WORKER_MAX="${WORKER_MAX:-8}"
HEAD_MIN="${HEAD_MIN:-2}"
HEAD_MAX="${HEAD_MAX:-6}"

DEMO_PROVIDER="${DEMO_PROVIDER:-latent-haversine}"
# Shorter than the compose demo's 60s: with KEDA scaling the pool down whenever
# debt drains, a departed worker's claims should return to the queue sooner.
DEMO_LEASE="${DEMO_LEASE:-30s}"
STATS_INTERVAL="${STATS_INTERVAL:-2}"

KEEP_CLUSTER="${KEEP_CLUSTER:-false}"
RECREATE="${RECREATE:-false}"
SKIP_BUILD="${SKIP_BUILD:-false}"

## PREFLIGHT

for tool in k3d kubectl curl "${CONTAINER_RUNNER}"; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    case "${tool}" in
      k3d) hint="'brew install k3d' (or see https://k3d.io/#installation)" ;;
      kubectl) hint="'brew install kubectl'" ;;
      *) hint="install it and try again" ;;
    esac
    echo "demo: ${tool} is not installed — ${hint}" >&2
    exit 1
  fi
done

if ! "${CONTAINER_RUNNER}" info >/dev/null 2>&1; then
  echo "demo: '${CONTAINER_RUNNER}' is installed but not reachable — is the daemon running?" >&2
  exit 1
fi

if [[ ! -d "${ENV_DIR}" ]]; then
  echo "demo: no such environment '${ENVIRONMENT}' (looked in ${ENV_DIR})" >&2
  exit 1
fi

if curl -sf "${BASE}/_ops_/live" >/dev/null 2>&1; then
  echo "demo: something is already serving on ${BASE} — stop it (or set PORT) first" >&2
  exit 1
fi

## CONTEXT
#
# One entry point each for namespaced and cluster-scoped calls. There is no bare
# kubectl anywhere below, and nothing calls `kubectl config use-context`.

KUBE_CONTEXT="k3d-${CLUSTER}"
ORIGINAL_CONTEXT="$(kubectl config current-context 2>/dev/null || echo "none")"

kube() { kubectl --context "${KUBE_CONTEXT}" --namespace "${NAMESPACE}" "$@"; }
kubec() { kubectl --context "${KUBE_CONTEXT}" "$@"; }

## TEARDOWN

cleanup() {
  echo
  if [[ "${KEEP_CLUSTER}" == "true" ]]; then
    echo "▶ leaving cluster '${CLUSTER}' up (KEEP_CLUSTER=true)"
    echo "  delete it with: k3d cluster delete ${CLUSTER}"
    return
  fi
  echo "▶ deleting the k3d cluster '${CLUSTER}'"
  k3d cluster delete "${CLUSTER}" >/dev/null 2>&1 || true
}
trap cleanup INT TERM EXIT

## CLUSTER

echo "▶ kube context ${KUBE_CONTEXT} (yours stays ${ORIGINAL_CONTEXT})"

REUSED=false
if k3d cluster list "${CLUSTER}" >/dev/null 2>&1 && [[ "${RECREATE}" != "true" ]]; then
  echo "▶ reusing the existing k3d cluster '${CLUSTER}' (RECREATE=true to rebuild it)"
  REUSED=true
else
  k3d cluster delete "${CLUSTER}" >/dev/null 2>&1 || true
  echo "▶ creating the k3d cluster — about a minute for one server and ${K3D_AGENTS} agents"
  k3d cluster create \
    --config "${ENV_DIR}/k3d.yaml" \
    --agents "${K3D_AGENTS}" \
    --port "${PORT}:80@loadbalancer" \
    --wait
fi

if ! kubec config get-contexts "${KUBE_CONTEXT}" >/dev/null 2>&1; then
  echo "demo: expected kube context ${KUBE_CONTEXT} to exist after cluster creation" >&2
  exit 1
fi

## IMAGE
#
# The image exists in no registry, so it is side-loaded onto every node. On a
# reused cluster the tag is unchanged, which means nothing restarts on its own —
# the rollout restart further down is what picks the new build up.

if [[ "${SKIP_BUILD}" == "true" ]]; then
  echo "▶ skipping the image build (SKIP_BUILD=true)"
else
  IMAGE="${IMAGE}" CONTAINER_RUNNER="${CONTAINER_RUNNER}" "${SCRIPT_DIR}/docker_build.sh"
fi

echo "▶ importing ${IMAGE} into the cluster — ~60MB per node, so 15-40s"
k3d image import "${IMAGE}" --cluster "${CLUSTER}"

## KEDA
#
# Cluster-scoped, and its manifest is a few megabytes of CRDs and webhooks, so it
# is installed here rather than folded into the app kustomization.

echo "▶ installing KEDA v${KEDA_VERSION}"
kubec apply --server-side -f \
  "https://github.com/kedacore/keda/releases/download/v${KEDA_VERSION}/keda-${KEDA_VERSION}.yaml"
# All three: the operator, the metrics apiserver, and the admission webhook —
# whose service must answer before the API server will accept a ScaledObject.
kubec --namespace keda wait --for=condition=Available deployment --all --timeout=180s
# The real gate: a ScaledObject is inert until the external metrics API is up.
kubec wait --for=condition=Available apiservice/v1beta1.external.metrics.k8s.io --timeout=180s

## APPLY
#
# Knobs are patched before waiting, so they don't trigger a second rollout in the
# middle of the demo.

echo "▶ applying deploy/environments/${ENVIRONMENT}"
kubec apply -k "${ENV_DIR}"

kube set env deployment/beeline-loadgen RPS="${RPS}" TABLE_GRID="${TABLE_GRID}" >/dev/null
kube set env deployment/beeline-worker \
  BEELINE_MATRIX_FOLLOWER_WORKERS="${FOLLOWER_WORKERS}" >/dev/null
kube patch scaledobject beeline-worker --type merge \
  --patch "{\"spec\":{\"maxReplicaCount\":${WORKER_MAX}}}" >/dev/null
kube patch hpa beeline-head --type merge \
  --patch "{\"spec\":{\"minReplicas\":${HEAD_MIN},\"maxReplicas\":${HEAD_MAX}}}" >/dev/null

if [[ -n "${LOG_LEVEL:-}" ]]; then
  kube set env deployment/beeline-head deployment/beeline-worker \
    BEELINE_OBSERVABILITY_LOGGING_LEVEL="${LOG_LEVEL}" >/dev/null
fi

if [[ "${REUSED}" == "true" ]]; then
  echo "▶ restarting the deployments to pick up the re-imported image"
  kube rollout restart deployment/beeline-head deployment/beeline-worker \
    deployment/beeline-loadgen >/dev/null
fi

## WAIT
#
# Heads only. Never wait on beeline-worker: KEDA holds it at zero replicas, and a
# follower's readiness is its leader's reachability, so the wait would hang.

echo "▶ waiting for postgres and the head pool…"
kube rollout status deployment/beeline-postgres --timeout=180s
kube rollout status deployment/beeline-head --timeout=300s

up=""
for _ in $(seq 1 240); do
  if curl -sf "${BASE}/_ops_/ready" >/dev/null 2>&1; then
    up=1
    break
  fi
  sleep 0.5
done
if [[ -z "${up}" ]]; then
  echo "demo: the head pool never answered on ${BASE} — try:" >&2
  echo "  kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} logs -l role=head --tail=50" >&2
  exit 1
fi

# The head HPA is only as good as metrics-server. Say so now rather than letting
# it sit on <unknown> for the rest of the run.
if ! kubec top nodes >/dev/null 2>&1; then
  echo "demo: metrics-server is not answering, so the head HPA will read <unknown>." >&2
  echo "      Usually fixed by patching it for k3d's kubelet certificates:" >&2
  echo "      kubectl --context ${KUBE_CONTEXT} -n kube-system patch deployment metrics-server --type=json \\" >&2
  echo "        -p '[{\"op\":\"add\",\"path\":\"/spec/template/spec/containers/0/args/-\",\"value\":\"--kubelet-insecure-tls\"}]'" >&2
fi

## SEED
#
# Postgres lives in an emptyDir that survives an apply and a kept cluster, so a
# blind re-seed would create three MORE areas, silently doubling the working set
# and invalidating every autoscaler number below.

if curl -sf "${BASE}/_config_/areas" 2>/dev/null | grep -q '"id"'; then
  echo "▶ areas already exist in the shared Postgres — skipping the seed"
else
  "${SCRIPT_DIR}/seed_demo_areas.sh" "${BASE}" "${DEMO_PROVIDER}" "${DEMO_LEASE}" >/dev/null
fi

working_set="$(curl -sf "${BASE}/_ops_/freshness" | sed -n 's/.*"workingSet":\([0-9]*\).*/\1/p' || true)"

cat <<BANNER

✔ cluster ready — operator console at ${BASE}
  ${working_set:-?} pairs in the working set; all compute is in the follower pool.
  The head pool scales on CPU; the follower pool scales on freshness debt, to zero.

  watch both autoscalers:
    kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} get hpa,scaledobject,deploy -w
  turn the read load up — this is what moves the head HPA:
    kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} \\
      set env deploy/beeline-loadgen RPS=250 TABLE_GRID=48 TABLE_RATIO=0.6
    (cost per request matters more than rate here: a bigger /table grid is what
     pushes head CPU past the 60% target. Rate alone mostly just queues.)
  drive the follower pool to zero, then watch it come back:
    for a in 1 2 3; do curl -sX POST ${BASE}/_config_/areas/\$a/disable; done
    for a in 1 2 3; do curl -sX POST ${BASE}/_config_/areas/\$a/enable;  done
  kill ONE head; the survivors keep the contract:
    kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} delete pod --wait=false \\
      "\$(kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} get pod -l role=head -o name | head -1)"
    (deleting them all with -l role=head is an outage, not a demo: a
     PodDisruptionBudget gates evictions, never a direct delete.)
    curl '${BASE}/estimate?origin=30.27,-97.745&dest=30.275,-97.74'
  logs, and psql into the shared state:
    kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} logs -l role=head -f --max-log-requests=10
    kubectl --context ${KUBE_CONTEXT} -n ${NAMESPACE} port-forward svc/beeline-postgres 55432:5432
  or just leave it running: the whole working set re-stales every 5 minutes, so
  both pools cycle on their own.

  Ctrl-C deletes the cluster.

BANNER

## STATS
#
# The freshness contract joined to live replica counts. go-template rather than
# jsonpath because status.readyReplicas is ABSENT (not zero) on a deployment
# scaled to zero, which is the follower pool's normal resting state — jsonpath
# would silently drop the column.

replicas() {
  kube get deployment "$1" -o go-template='{{if .status.readyReplicas}}{{.status.readyReplicas}}{{else}}0{{end}}/{{if .spec.replicas}}{{.spec.replicas}}{{else}}0{{end}}' 2>/dev/null || echo "?/?"
}

while sleep "${STATS_INTERVAL}"; do
  stats="$(curl -sf "${BASE}/_ops_/freshness" 2>/dev/null || true)"
  if [[ -z "${stats}" ]]; then
    echo "  head pool unreachable"
    continue
  fi
  debt="$(printf '%s' "${stats}" | sed -n 's/.*"debt":\([0-9]*\).*/\1/p')"
  achieved="$(printf '%s' "${stats}" | sed -n 's/.*"achievedThroughput":\([0-9.]*\).*/\1/p')"
  required="$(printf '%s' "${stats}" | sed -n 's/.*"requiredThroughput":\([0-9.]*\).*/\1/p')"
  printf '  debt=%-9s achieved=%-8.0f required=%-8.0f heads=%-7s workers=%-7s\n' \
    "${debt:-?}" "${achieved:-0}" "${required:-0}" \
    "$(replicas beeline-head)" "$(replicas beeline-worker)"
done
