#!/usr/bin/env bash
# Run the real Synapse -> appservice -> A2A -> Matrix reply path in an isolated kind cluster.
set -euo pipefail

readonly CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-integration}"
readonly NAMESPACE=bridge-integration
readonly FIXTURE_IMAGE=matrix-a2a-bridge-integration:local
readonly SCENARIO="${INTEGRATION_SCENARIO:-integration}"
readonly FIXTURE_SETTINGS="${FIXTURE_SETTINGS:-}"
readonly DRIVER_MANIFEST="${DRIVER_MANIFEST:-driver-job.yaml}"
readonly DRIVER_JOB_NAME="${DRIVER_JOB_NAME:-integration-driver}"
readonly DRIVER_WAIT_TIMEOUT="${DRIVER_WAIT_TIMEOUT:-90s}"
# Kubernetes 1.34 is the newest line that supports both cgroup v1 developer hosts and the
# cgroup v2 GitHub runner. Kubernetes 1.35 intentionally refuses to start on cgroup v1.
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
readonly SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly APP_DIR="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly STARTED_AT="${SECONDS}"
KUBECONFIG="$(mktemp -t fgentic-bridge-kind.XXXXXX)"
export KUBECONFIG

diagnose() {
  echo "==> Integration fixture diagnostics"
  kubectl --namespace "${NAMESPACE}" get all --output=wide || true
  kubectl --namespace "${NAMESPACE}" logs deployment/postgres --all-containers --tail=200 || true
  kubectl --namespace "${NAMESPACE}" logs deployment/synapse --all-containers --tail=200 || true
  kubectl --namespace "${NAMESPACE}" logs deployment/a2a-stub --all-containers --tail=200 || true
  kubectl --namespace "${NAMESPACE}" logs deployment/bridge --all-containers --tail=200 || true
  kubectl --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}" --all-containers --tail=200 || true
  kubectl --namespace "${NAMESPACE}" describe pods || true
}

cleanup() {
  readonly result=$?
  trap - EXIT
  if ((result != 0)); then
    diagnose
  fi
  if [[ "${KEEP_KIND_CLUSTER:-0}" == "1" ]]; then
    echo "==> Keeping kind cluster ${CLUSTER_NAME}; use KUBECONFIG=${KUBECONFIG}"
  else
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    rm -f "${KUBECONFIG}"
  fi
  exit "${result}"
}
trap cleanup EXIT

docker info >/dev/null 2>&1 || {
  echo "Error: Docker daemon is not available" >&2
  exit 1
}

echo "==> Building bridge integration image"
docker build --provenance=false --file "${SCRIPT_DIR}/Dockerfile" --tag "${FIXTURE_IMAGE}" "${APP_DIR}"

# A stale fixture cluster has no durable state and is safe to replace. The dedicated kubeconfig
# keeps kind from changing the developer's current kubectl context (normally the shared k3d cluster).
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
echo "==> Creating isolated kind cluster ${CLUSTER_NAME}"
kind create cluster \
  --name "${CLUSTER_NAME}" \
  --image "${KIND_NODE_IMAGE}" \
  --config "${SCRIPT_DIR}/kind.yaml" \
  --wait 240s
kubectl wait --for=condition=Ready nodes --all --timeout=60s
kind load docker-image --name "${CLUSTER_NAME}" "${FIXTURE_IMAGE}"

echo "==> Starting Postgres and Synapse"
kubectl apply --filename "${SCRIPT_DIR}/platform.yaml"
if [[ -n "${FIXTURE_SETTINGS}" ]]; then
  kubectl apply --filename "${SCRIPT_DIR}/${FIXTURE_SETTINGS}"
fi
kubectl --namespace "${NAMESPACE}" rollout status deployment/postgres --timeout=180s
kubectl --namespace "${NAMESPACE}" rollout status deployment/synapse --timeout=240s

echo "==> Starting real bridge and SDK-backed A2A stub"
kubectl apply --filename "${SCRIPT_DIR}/workloads.yaml"
kubectl --namespace "${NAMESPACE}" rollout status deployment/a2a-stub --timeout=60s
kubectl --namespace "${NAMESPACE}" rollout status deployment/bridge --timeout=90s

echo "==> Running ${SCENARIO} scenario"
kubectl apply --filename "${SCRIPT_DIR}/${DRIVER_MANIFEST}"
kubectl --namespace "${NAMESPACE}" wait --for=condition=complete "job/${DRIVER_JOB_NAME}" --timeout="${DRIVER_WAIT_TIMEOUT}"
kubectl --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}"

if [[ "${SCENARIO}" == "integration" ]]; then
  echo "==> Verifying fail-closed remote AgentCard audit"
  readonly untrusted_audits="$(
    kubectl --namespace "${NAMESPACE}" logs deployment/bridge --all-containers | jq -Rsc '
    [
      split("\n")[]
      | fromjson?
      | select(
          .log_stream == "audit"
          and .msg == "delegation audit"
          and .ghost == "agent-remote"
          and .outcome == "denied"
          and .terminal_stage == "agent_card"
          and .terminal_reason == "agent_card_untrusted"
          and .rate_limit_verdict == "not_checked"
          and .a2a_attempted == false
        )
    ]
    | length
    '
  )"
  if [[ "${untrusted_audits}" != "1" ]]; then
    echo "Error: expected one content-free agent_card_untrusted audit, got ${untrusted_audits}" >&2
    exit 1
  fi
fi

echo "==> Bridge ${SCENARIO} scenario passed in $((SECONDS - STARTED_AT))s"
