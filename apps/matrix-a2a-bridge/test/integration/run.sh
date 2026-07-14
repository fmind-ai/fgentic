#!/usr/bin/env bash
# Run the real Synapse -> appservice -> A2A -> Matrix reply path in an isolated kind cluster.
set -euo pipefail

readonly CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-integration}"
readonly NAMESPACE=bridge-integration
readonly PLAIN_AGENT_NAMESPACE=plain-agent
readonly FIXTURE_IMAGE=matrix-a2a-bridge-integration:local
readonly PLAIN_AGENT_IMAGE=plain-a2a-agent-integration:local
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
POSTGRES_IMAGE="$(
  yq -er 'select(.kind == "Deployment" and .metadata.name == "postgres") |
    .spec.template.spec.containers[] | select(.name == "postgres") | .image' \
    "${SCRIPT_DIR}/platform.yaml"
)"
readonly POSTGRES_IMAGE
SYNAPSE_IMAGE="$(
  yq -er 'select(.kind == "Deployment" and .metadata.name == "synapse") |
    .spec.template.spec.containers[] | select(.name == "synapse") | .image' \
    "${SCRIPT_DIR}/platform.yaml"
)"
readonly SYNAPSE_IMAGE
KUBECONFIG="$(mktemp -t fgentic-bridge-kind.XXXXXX)"
export KUBECONFIG
availability_delete_requested_seconds=""
availability_replacement_ready_seconds=""
availability_old_pod=""
availability_old_uid=""
availability_replacement_pod=""
availability_runtime_node=""
availability_bridge_container=""
availability_synapse_container=""
availability_shutdown_watcher_pid=""

diagnose() {
  echo "==> Integration fixture diagnostics"
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" get all --output=wide || true
  kubectl --request-timeout=5s --namespace "${PLAIN_AGENT_NAMESPACE}" get all --output=wide || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/postgres --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/synapse --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${PLAIN_AGENT_NAMESPACE}" logs deployment/plain-a2a-agent --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/bridge --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}" --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" describe pods || true
}

wait_for_stable_control_plane() {
  local deadline=$((SECONDS + 180))
  local stable_samples=0
  while ((SECONDS < deadline)); do
    if [[ "$(kubectl --request-timeout=5s get --raw=/readyz 2>/dev/null || true)" == "ok" ]] &&
      kubectl --request-timeout=5s api-resources >/dev/null 2>&1 &&
      kubectl --request-timeout=5s get nodes --output=json 2>/dev/null |
        jq -e '
          (.items | length) > 0 and
          all(.items[]; any(.status.conditions[]?; .type == "Ready" and .status == "True"))
        ' >/dev/null; then
      stable_samples=$((stable_samples + 1))
      if ((stable_samples == 3)); then
        return 0
      fi
    else
      stable_samples=0
    fi
    sleep 1
  done
  echo "Error: kind control plane was not stably reachable within 180s" >&2
  return 1
}

wait_for_synapse_background_updates() {
  local runtime_node="${kind_nodes%%$'\n'*}"
  local postgres_container=""
  postgres_container="$(
    docker exec "${runtime_node}" crictl ps \
      --quiet \
      --label io.kubernetes.container.name=postgres \
      --label "io.kubernetes.pod.namespace=${NAMESPACE}"
  )"
  if [[ -z "${postgres_container}" || "${postgres_container}" == *$'\n'* ]]; then
    echo "Error: expected exactly one running Postgres container for the Synapse readiness gate" >&2
    return 1
  fi

  local deadline=$((SECONDS + 300))
  local stable_samples=0
  local pending=""
  while ((SECONDS < deadline)); do
    if pending="$(
      docker exec "${runtime_node}" crictl exec "${postgres_container}" \
        psql -U postgres -d synapse -Atc 'SELECT count(*) FROM background_updates;' \
        2>/dev/null
    )" && [[ "${pending}" == "0" ]]; then
      stable_samples=$((stable_samples + 1))
      if ((stable_samples == 3)); then
        echo "==> Synapse background updates quiesced"
        return 0
      fi
    else
      stable_samples=0
    fi
    sleep 2
  done
  echo "Error: Synapse background updates did not quiesce within 300s (pending=${pending:-unknown})" >&2
  return 1
}

wait_for_availability_active() {
  local deadline=$((SECONDS + 60))
  while ((SECONDS < deadline)); do
    if kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}" 2>/dev/null |
      jq -R -e 'fromjson? | select(.availability_phase == "active")' >/dev/null; then
      return 0
    fi
    sleep 0.5
  done
  echo "Error: availability driver did not reach an active A2A call within 60s" >&2
  return 1
}

capture_original_bridge() {
  availability_old_pod="$(
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pods \
      --selector app.kubernetes.io/name=bridge \
      --output=json |
      jq -er '.items[] | select(.metadata.deletionTimestamp == null) | .metadata.name'
  )"
  availability_old_uid="$(
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pod "${availability_old_pod}" \
      --output=jsonpath='{.metadata.uid}'
  )"
  availability_runtime_node="${kind_nodes%%$'\n'*}"
  availability_bridge_container="$(
    docker exec "${availability_runtime_node}" crictl ps \
      --quiet \
      --label io.kubernetes.container.name=bridge \
      --label "io.kubernetes.pod.namespace=${NAMESPACE}"
  )"
  availability_synapse_container="$(
    docker exec "${availability_runtime_node}" crictl ps \
      --quiet \
      --label io.kubernetes.container.name=synapse \
      --label "io.kubernetes.pod.namespace=${NAMESPACE}"
  )"
  if [[ -z "${availability_bridge_container}" || "${availability_bridge_container}" == *$'\n'* ]]; then
    echo "Error: expected exactly one running bridge container for the shutdown proof" >&2
    return 1
  fi
  if [[ -z "${availability_synapse_container}" || "${availability_synapse_container}" == *$'\n'* ]]; then
    echo "Error: expected exactly one running Synapse container for the release control" >&2
    return 1
  fi
}

start_availability_shutdown_watcher() {
  # Establish one CRI log stream before deletion. Reopening logs while the node creates the
  # replacement can be delayed long enough to outlive the held A2A request on constrained hosts.
  (
    set +o pipefail
    docker exec "${availability_runtime_node}" \
      crictl logs --follow "${availability_bridge_container}" 2>/dev/null |
      {
        while IFS= read -r line; do
          if jq -e 'select(.msg == "stopping matrix-a2a-bridge")' <<<"${line}" >/dev/null 2>&1; then
            echo "==> Original bridge observed SIGTERM; releasing held A2A call"
            release_availability_call
            exit $?
          fi
        done
        exit 1
      }
  ) &
  availability_shutdown_watcher_pid=$!
}

wait_for_bridge_shutdown_signal() {
  local deadline=$((SECONDS + 20))
  while kill -0 "${availability_shutdown_watcher_pid}" 2>/dev/null; do
    if ((SECONDS >= deadline)); then
      kill "${availability_shutdown_watcher_pid}" 2>/dev/null || true
      wait "${availability_shutdown_watcher_pid}" 2>/dev/null || true
      availability_shutdown_watcher_pid=""
      echo "Error: original bridge did not log SIGTERM handling within 20s" >&2
      return 1
    fi
    sleep 0.2
  done
  if ! wait "${availability_shutdown_watcher_pid}"; then
    availability_shutdown_watcher_pid=""
    echo "Error: original bridge shutdown watcher ended before releasing the held call" >&2
    return 1
  fi
  availability_shutdown_watcher_pid=""
}

release_availability_call() {
  # The Kubernetes API is deliberately under pressure while it replaces the bridge pod. Use the
  # fixture node's CRI to reach the already-captured Synapse container, so releasing the held A2A
  # call does not depend on a second control-plane round trip after SIGTERM.
  docker exec "${availability_runtime_node}" crictl exec "${availability_synapse_container}" python -c '
import urllib.request

request = urllib.request.Request(
    "http://plain-a2a-agent.plain-agent.svc.cluster.local:8080/control/release",
    method="POST",
)
with urllib.request.urlopen(request, timeout=5) as response:
    if response.status != 204:
        raise SystemExit(f"release status {response.status}, want 204")
'
}

wait_for_replacement_bridge() {
  local old_uid="$1"
  local deadline=$((SECONDS + 60))
  local replacement=""
  while ((SECONDS < deadline)); do
    if replacement="$(
      kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pods \
        --selector app.kubernetes.io/name=bridge \
        --output=json |
        jq -er --arg old_uid "${old_uid}" '
          [
            .items[]
            | select(.metadata.uid != $old_uid and .metadata.deletionTimestamp == null)
            | select(any(.status.conditions[]?; .type == "Ready" and .status == "True"))
          ]
          | first
          | select(. != null)
          | [.metadata.name, .metadata.uid]
          | @tsv
        '
    )"; then
      read -r availability_replacement_pod _ <<<"${replacement}"
      availability_replacement_ready_seconds="${SECONDS}"
      return 0
    fi
    sleep 0.5
  done
  echo "Error: replacement bridge pod was not Ready within 60s" >&2
  return 1
}

disrupt_active_availability_call() {
  wait_for_availability_active
  local current_uid
  current_uid="$(
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pod "${availability_old_pod}" \
      --output=jsonpath='{.metadata.uid}'
  )"
  if [[ "${current_uid}" != "${availability_old_uid}" ]]; then
    echo "Error: original bridge changed before the controlled disruption" >&2
    return 1
  fi
  start_availability_shutdown_watcher
  availability_delete_requested_seconds="${SECONDS}"
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" delete pod "${availability_old_pod}" --wait=false
  wait_for_bridge_shutdown_signal
  wait_for_replacement_bridge "${availability_old_uid}"
  kubectl --request-timeout=65s --namespace "${NAMESPACE}" wait \
    --for=delete "pod/${availability_old_pod}" \
    --timeout=60s
}

cleanup() {
  readonly result=$?
  trap - EXIT
  if [[ -n "${availability_shutdown_watcher_pid}" ]] &&
    kill -0 "${availability_shutdown_watcher_pid}" 2>/dev/null; then
    kill "${availability_shutdown_watcher_pid}" 2>/dev/null || true
    wait "${availability_shutdown_watcher_pid}" 2>/dev/null || true
  fi
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
echo "==> Building standalone plain A2A agent image"
docker build --provenance=false --file "${SCRIPT_DIR}/plain-agent.Dockerfile" --tag "${PLAIN_AGENT_IMAGE}" "${APP_DIR}"
if [[ "$(docker image inspect --format '{{json .Config.Entrypoint}}|{{.Config.User}}' "${PLAIN_AGENT_IMAGE}")" != \
  '["/usr/local/bin/plain-a2a-agent"]|65532:65532' ]]; then
  echo "Error: plain A2A image lost its single non-root runtime entrypoint" >&2
  exit 1
fi

# A stale fixture cluster has no durable state and is safe to replace. The dedicated kubeconfig
# keeps kind from changing the developer's current kubectl context (normally the shared k3d cluster).
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
echo "==> Creating isolated kind cluster ${CLUSTER_NAME}"
kind create cluster \
  --name "${CLUSTER_NAME}" \
  --image "${KIND_NODE_IMAGE}" \
  --config "${SCRIPT_DIR}/kind.yaml" \
  --wait 240s
kind load docker-image --name "${CLUSTER_NAME}" "${FIXTURE_IMAGE}" "${PLAIN_AGENT_IMAGE}"
# Pull immutable dependencies sequentially before workloads exist. Concurrent pod pulls can starve
# the colocated API server on constrained hosts; Kind's Docker archive path is also unsafe for
# partially cached multi-platform indexes, so let each node's CRI resolve its own platform.
kind_nodes="$(kind get nodes --name "${CLUSTER_NAME}")"
readonly kind_nodes
if [[ -z "${kind_nodes}" ]]; then
  echo "Error: kind returned no nodes for ${CLUSTER_NAME}" >&2
  exit 1
fi
while IFS= read -r node; do
  for image in "${POSTGRES_IMAGE}" "${SYNAPSE_IMAGE}"; do
    echo "==> Caching ${image} on ${node}"
    docker exec "${node}" crictl pull "${image}"
  done
done <<<"${kind_nodes}"
wait_for_stable_control_plane

echo "==> Starting Postgres and Synapse"
kubectl apply --filename "${SCRIPT_DIR}/platform.yaml"
if [[ -n "${FIXTURE_SETTINGS}" ]]; then
  kubectl apply --filename "${SCRIPT_DIR}/${FIXTURE_SETTINGS}"
fi
kubectl --namespace "${NAMESPACE}" rollout status deployment/postgres --timeout=300s
kubectl --namespace "${NAMESPACE}" rollout status deployment/synapse --timeout=420s
wait_for_synapse_background_updates

if kubectl get namespace kagent >/dev/null 2>&1; then
  echo "Error: runtime-independence fixture unexpectedly installed the kagent namespace" >&2
  exit 1
fi

echo "==> Starting real bridge and standalone SDK-backed A2A agent"
kubectl apply --filename "${SCRIPT_DIR}/workloads.yaml"
if ! kubectl --namespace "${PLAIN_AGENT_NAMESPACE}" get networkpolicy plain-a2a-agent --output=json | jq -e \
  --arg allowed_namespace "${NAMESPACE}" '
    .spec.policyTypes == ["Ingress", "Egress"]
    and (.spec.ingress | length == 1)
    and (.spec.ingress[0].from | length == 1)
    and .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == $allowed_namespace
    and (.spec.egress | length == 0)
  ' >/dev/null; then
  echo "Error: plain A2A agent NetworkPolicy is not exact and default-deny" >&2
  exit 1
fi
kubectl --namespace "${PLAIN_AGENT_NAMESPACE}" rollout status deployment/plain-a2a-agent --timeout=60s
kubectl --namespace "${NAMESPACE}" rollout status deployment/bridge --timeout=90s
if [[ "${SCENARIO}" == "availability" ]]; then
  capture_original_bridge
fi

echo "==> Running ${SCENARIO} scenario"
kubectl apply --filename "${SCRIPT_DIR}/${DRIVER_MANIFEST}"
if [[ "${SCENARIO}" == "availability" ]]; then
  echo "==> Interrupting the active delegation with a graceful bridge pod deletion"
  disrupt_active_availability_call
fi
kubectl --request-timeout=155s --namespace "${NAMESPACE}" wait \
  --for=condition=complete "job/${DRIVER_JOB_NAME}" --timeout="${DRIVER_WAIT_TIMEOUT}"
driver_logs="$(
  kubectl --request-timeout=10s --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}"
)"
readonly driver_logs
printf '%s\n' "${driver_logs}"

if [[ "${SCENARIO}" == "availability" ]]; then
  delivery_gap_ms="$(
    jq -Rr 'fromjson? | select(.availability_phase == "passed") | .delivery_gap_ms' <<<"${driver_logs}" |
      tail -n 1
  )"
  readonly delivery_gap_ms
  replacement_observed_ms="$(
    jq -Rr 'fromjson? | select(.availability_phase == "passed") | .replacement_observed_ms' <<<"${driver_logs}" |
      tail -n 1
  )"
  readonly replacement_observed_ms
  if [[ ! "${delivery_gap_ms}" =~ ^[0-9]+$ || ! "${replacement_observed_ms}" =~ ^[0-9]+$ ]]; then
    echo "Error: availability driver did not emit numeric recovery measurements" >&2
    exit 1
  fi
  readonly pod_ready_rto_seconds="$((availability_replacement_ready_seconds - availability_delete_requested_seconds))"
  if ((pod_ready_rto_seconds < 0)); then
    echo "Error: availability timing produced a negative interval" >&2
    exit 1
  fi
  jq --compact-output --null-input \
    --arg scenario "${SCENARIO}" \
    --arg old_pod "${availability_old_pod}" \
    --arg replacement_pod "${availability_replacement_pod}" \
    --argjson delivery_gap_ms "${delivery_gap_ms}" \
    --argjson replacement_observed_ms "${replacement_observed_ms}" \
    --argjson pod_ready_rto_seconds "${pod_ready_rto_seconds}" \
    '{
      scenario: $scenario,
      graceful_sigterm: true,
      old_pod: $old_pod,
      replacement_pod: $replacement_pod,
      delivery_gap_ms: $delivery_gap_ms,
      replacement_observed_ms: $replacement_observed_ms,
      pod_ready_rto_seconds: $pod_ready_rto_seconds
    }'
fi

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
          and .ghost == "agent-plain"
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
