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
readonly DRIVER_WAIT_TIMEOUT="${DRIVER_WAIT_TIMEOUT:-240s}"
readonly PRE_KILL_AUDIT_EXPECTED='{"managed_room_reasons":["ghost_membership_required","room_binding_rejected"],"unauthorized_invites":1,"agent_card_denials":1,"content_bearing_records":0}'
# Kubernetes 1.34 is the newest line that supports both cgroup v1 developer hosts and the
# cgroup v2 GitHub runner. Kubernetes 1.35 intentionally refuses to start on cgroup v1.
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
APP_DIR="$(cd -- "${SCRIPT_DIR}/../.." && pwd)"
readonly APP_DIR
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
crash_log_file=""
crash_runtime_node=""
crash_bridge_container=""
dead_man_log_file=""

print_driver_logs() {
  echo "==> Driver logs"
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs \
    "job/${DRIVER_JOB_NAME}" --all-containers --tail=200 || true
}

print_crash_fault_state() {
  echo "==> Crash fault state"
  kubectl --request-timeout=5s get --raw \
    "/api/v1/namespaces/${NAMESPACE}/services/fault-proxy:control/proxy/state" || true
}

print_crash_delegation_state() {
  echo "==> Content-free crash delegation state"
  echo "matrix_event_id|state|lease_generation|lease_expires_at|attempt_count|poll_count|next_attempt_at|error_code|admission_checked|admission_allowed|admission_reason|reply_recorded|placeholder_recorded|edit_recorded|created_at|updated_at|terminal_at"
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" exec deployment/postgres -- \
    psql -U postgres -d bridge -At -F '|' -c \
    "SELECT matrix_event_id, state, lease_generation, COALESCE(lease_expires_at::text, ''), attempt_count, poll_count, next_attempt_at, error_code, admission_checked, admission_allowed, admission_reason, matrix_reply_event_id <> '', matrix_placeholder_event_id <> '', matrix_edit_event_id <> '', created_at, updated_at, COALESCE(terminal_at::text, '') FROM bridge_delegations WHERE appservice_transaction_id LIKE 'crash-%' ORDER BY intake_sequence;" || true
  echo "==> Content-free crash control state"
  echo "matrix_event_id|kind|state|slot|lease_generation|recovery_count|error_code|payload_bytes|terminal_at"
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" exec deployment/postgres -- \
    psql -U postgres -d bridge -At -F '|' -c \
    "SELECT jobs.matrix_event_id, controls.kind, controls.state, controls.slot, controls.lease_generation, controls.recovery_count, controls.error_code, octet_length(controls.payload), COALESCE(controls.terminal_at::text, '') FROM bridge_delegation_controls AS controls JOIN bridge_delegations AS jobs ON jobs.job_id = controls.job_id WHERE jobs.appservice_transaction_id LIKE 'crash-%' ORDER BY jobs.intake_sequence, controls.created_at, controls.control_id;" || true
}

crash_driver_terminal_status() {
  local job_json=""
  if job_json="$(
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" get \
      "job/${DRIVER_JOB_NAME}" --output=json 2>/dev/null
  )" && jq -e '
      ((.status.failed // 0) > 0)
      or any(.status.conditions[]?; .type == "Failed" and .status == "True")
    ' <<<"${job_json}" >/dev/null; then
    jq -r '
      [
        .status.conditions[]?
        | select(.type == "Failed" and .status == "True")
        | .reason // "unknown"
      ] as $reasons
      | "job failed failed_count=\(.status.failed // 0) reason=\($reasons[0] // "unknown")"
    ' <<<"${job_json}"
    return 0
  fi

  local pods_json=""
  if ! pods_json="$(
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pods \
      --selector "job-name=${DRIVER_JOB_NAME}" --output=json 2>/dev/null
  )"; then
    return 1
  fi
  jq -r '
    [
      .items[]? as $pod
      | $pod.status.containerStatuses[]?
      | select(.state.terminated != null)
      | "container terminated pod=\($pod.metadata.name) container=\(.name) exit_code=\(.state.terminated.exitCode) reason=\(.state.terminated.reason // "unknown")"
    ][0] // empty
  ' <<<"${pods_json}"
}

diagnose() {
  echo "==> Integration fixture diagnostics"
  print_driver_logs
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/bridge --all-containers --tail=200 || true
  if [[ "${SCENARIO}" == "crash-recovery" ]]; then
    print_crash_fault_state
    print_crash_delegation_state
  fi
  if [[ "${SCENARIO}" == "crash-recovery" || "${SCENARIO}" == "model-outage" ]]; then
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/fault-proxy --all-containers --tail=200 || true
  fi
  kubectl --request-timeout=5s --namespace "${PLAIN_AGENT_NAMESPACE}" logs deployment/plain-a2a-agent --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/postgres --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/synapse --all-containers --tail=200 || true
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" get all --output=wide || true
  kubectl --request-timeout=5s --namespace "${PLAIN_AGENT_NAMESPACE}" get all --output=wide || true
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

wait_for_dead_man_armed() {
  local deadline=$((SECONDS + 60))
  local armed_jobs=""
  while ((SECONDS < deadline)); do
    if kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}" 2>/dev/null |
      jq -R -e 'fromjson? | select(.dead_man_phase == "armed")' >/dev/null; then
      armed_jobs="$(
        kubectl --request-timeout=5s --namespace "${NAMESPACE}" exec deployment/postgres -- \
          psql -U postgres -d bridge -Atc \
          "SELECT count(*) FROM bridge_delegations WHERE state = 'awaiting_task' AND matrix_dead_man_delay_id <> '';" \
          2>/dev/null || true
      )"
      if [[ "${armed_jobs}" == "1" ]]; then
        return 0
      fi
    fi
    sleep 0.2
  done
  echo "Error: integration driver did not durably arm exactly one delayed-event dead-man switch within 60s (jobs=${armed_jobs:-unknown})" >&2
  print_driver_logs
  return 1
}

pre_kill_audit_summary() {
  jq -Rsc '
    [split("\n")[] | fromjson?] as $records
    | ([$records[] | select(
        .log_stream == "audit"
        and .msg == "delegation audit"
        and .ghost == "agent-integration"
        and .terminal_stage == "room_authorization"
        and (.terminal_reason == "room_binding_rejected" or
          .terminal_reason == "ghost_membership_required")
        and .rate_limit_verdict == "not_checked"
        and .a2a_attempted == false
      )]) as $managed
    | ([$records[] | select(
        .log_stream == "audit"
        and .audit_schema == "fgentic.managed_room_invite.v1"
        and .outcome == "denied"
        and .reason == "invite_sender_rejected"
      )]) as $invites
    | ([$records[] | select(
        .log_stream == "audit"
        and .msg == "delegation audit"
        and .ghost == "agent-plain"
        and .outcome == "denied"
        and .terminal_stage == "agent_card"
        and .terminal_reason == "agent_card_untrusted"
        and .rate_limit_verdict == "not_checked"
        and .a2a_attempted == false
      )]) as $cards
    | {
        managed_room_reasons: ([$managed[] | .terminal_reason] | sort),
        unauthorized_invites: ($invites | length),
        agent_card_denials: ($cards | length),
        content_bearing_records: ([$managed + $invites + $cards | .[] | select(
          has("content") or has("body") or has("prompt")
        )] | length)
      }
  '
}

wait_for_pre_kill_audits() {
  local deadline=$((SECONDS + 60))
  local bridge_logs=""
  local summary=""
  while ((SECONDS < deadline)); do
    if bridge_logs="$(
      kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs deployment/bridge \
        --container bridge 2>/dev/null
    )" && summary="$(pre_kill_audit_summary <<<"${bridge_logs}")" && [[ "${summary}" == "${PRE_KILL_AUDIT_EXPECTED}" ]]; then
      return 0
    fi
    sleep 0.5
  done
  echo "Error: bridge did not emit the complete pre-kill audit contract within 60s: ${summary:-unavailable}" >&2
  return 1
}

hard_kill_dead_man_bridge() {
  local runtime_node="${kind_nodes%%$'\n'*}"
  local container=""
  local pid=""
  container="$(
    docker exec "${runtime_node}" crictl ps \
      --quiet \
      --label io.kubernetes.container.name=bridge \
      --label "io.kubernetes.pod.namespace=${NAMESPACE}"
  )"
  if [[ -z "${container}" || "${container}" == *$'\n'* ]]; then
    echo "Error: expected exactly one running bridge container for the dead-man proof" >&2
    return 1
  fi
  pid="$(
    docker exec "${runtime_node}" crictl inspect "${container}" |
      jq -er '.info.pid | tonumber | select(. > 0)'
  )"
  dead_man_log_file="$(mktemp -t fgentic-bridge-dead-man-logs.XXXXXX)"
  docker exec "${runtime_node}" crictl logs "${container}" >"${dead_man_log_file}" 2>&1 || true
  echo "==> SIGKILL bridge process ${pid} with its delayed stale-task notice armed"
  docker exec "${runtime_node}" kill -KILL "${pid}"
  # Stop the Deployment after the hard process boundary so no replacement can refresh or cancel
  # the server-owned timer. Synapse must materialize the notice without any bridge process.
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" scale deployment/bridge --replicas=0
  kubectl --request-timeout=65s --namespace "${NAMESPACE}" wait \
    --for=delete pod --selector app.kubernetes.io/name=bridge --timeout=60s
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

wait_for_crash_phase() {
  local phase="$1"
  local deadline=$((SECONDS + 60))
  local next_status_check="${SECONDS}"
  local terminal_status=""
  while ((SECONDS < deadline)); do
    if ((SECONDS >= next_status_check)); then
      terminal_status="$(crash_driver_terminal_status || true)"
      if [[ -n "${terminal_status}" ]]; then
        echo "Error: crash driver ${terminal_status} before reaching ${phase}" >&2
        print_driver_logs
        return 1
      fi
      next_status_check=$((SECONDS + 1))
    fi
    if kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}" 2>/dev/null |
      jq -R -e --arg phase "${phase}" '
        fromjson?
        | select(.crash_action == "sigkill_ready" and .crash_phase == $phase)
      ' >/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "Error: crash driver did not reach ${phase} within 60s" >&2
  print_crash_fault_state
  print_crash_delegation_state
  print_driver_logs
  return 1
}

bridge_runtime_container() {
  docker exec "${crash_runtime_node}" crictl ps \
    --quiet \
    --label io.kubernetes.container.name=bridge \
    --label "io.kubernetes.pod.namespace=${NAMESPACE}"
}

hard_kill_bridge_process() {
  local phase="$1"
  local old_container="${crash_bridge_container}"
  local old_pod=""
  local pid=""
  old_pod="$(
    kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pods \
      --selector app.kubernetes.io/name=bridge \
      --output=json |
      jq -er '
        [.items[] | select(.metadata.deletionTimestamp == null)]
        | if length == 1 then .[0].metadata.name else error("expected one bridge pod") end
      '
  )"
  pid="$(
    docker exec "${crash_runtime_node}" crictl inspect "${old_container}" |
      jq -er '.info.pid | tonumber | select(. > 0)'
  )"
  docker exec "${crash_runtime_node}" crictl logs "${old_container}" >>"${crash_log_file}" 2>&1 || true
  echo "==> SIGKILL bridge process ${pid} at ${phase}"
  docker exec "${crash_runtime_node}" kill -KILL "${pid}"
  # The process boundary is complete once SIGKILL returns. Replace its dead pod so twelve
  # intentional crashes do not accumulate CrashLoopBackOff delays on constrained hosts.
  kubectl --request-timeout=5s --namespace "${NAMESPACE}" delete pod "${old_pod}" --wait=false

  local deadline=$((SECONDS + 60))
  local replacement=""
  while ((SECONDS < deadline)); do
    replacement="$(bridge_runtime_container || true)"
    if [[ -n "${replacement}" && "${replacement}" != "${old_container}" && "${replacement}" != *$'\n'* ]] &&
      kubectl --request-timeout=5s --namespace "${NAMESPACE}" get pod \
        --selector app.kubernetes.io/name=bridge \
        --output=json 2>/dev/null |
      jq -e 'any(.items[]?.status.conditions[]?; .type == "Ready" and .status == "True")' >/dev/null; then
      crash_bridge_container="${replacement}"
      return 0
    fi
    sleep 0.2
  done
  echo "Error: bridge container did not restart after SIGKILL at ${phase}" >&2
  return 1
}

disrupt_crash_recovery() {
  crash_runtime_node="${kind_nodes%%$'\n'*}"
  crash_bridge_container="$(bridge_runtime_container)"
  if [[ -z "${crash_bridge_container}" || "${crash_bridge_container}" == *$'\n'* ]]; then
    echo "Error: expected exactly one running bridge container for crash recovery" >&2
    return 1
  fi
  crash_log_file="$(mktemp -t fgentic-bridge-crash-logs.XXXXXX)"

  local phases=(
    ledger_committed_pre_ack
    acknowledged_pre_claim
    a2a_accepted_pre_record
    control_intent_committed_pre_claim
    cancel_accepted_pre_record
    continuation_accepted_pre_record
    question_accepted_pre_record
    progress_accepted_pre_record
    pin_accepted_pre_record
    result_persisted_pre_matrix
    matrix_accepted_pre_record
    long_task_polling
  )
  local phase
  for phase in "${phases[@]}"; do
    wait_for_crash_phase "${phase}"
    hard_kill_bridge_process "${phase}"
  done
}

wait_for_driver_phase() {
  local field="$1"
  local phase="$2"
  local timeout="${3:-120}"
  local deadline=$((SECONDS + timeout))
  while ((SECONDS < deadline)); do
    if kubectl --request-timeout=5s --namespace "${NAMESPACE}" logs "job/${DRIVER_JOB_NAME}" 2>/dev/null |
      jq -R -e --arg field "${field}" --arg phase "${phase}" \
        'fromjson? | select(.[$field] == $phase)' >/dev/null; then
      return 0
    fi
    sleep 0.2
  done
  echo "Error: driver did not reach ${field}=${phase} within ${timeout}s" >&2
  print_driver_logs
  return 1
}

disrupt_synapse_restart() {
  echo "==> Restarting Synapse while a task is mid-poll"
  wait_for_driver_phase synapse_restart_phase task_polling 120
  kubectl --request-timeout=10s --namespace "${NAMESPACE}" rollout restart deployment/synapse
  kubectl --request-timeout=305s --namespace "${NAMESPACE}" \
    rollout status deployment/synapse --timeout=300s
}

verify_model_outage_evidence() {
  local want=$((${MODEL_OUTAGE_MAX_ATTEMPTS:-3} - 1))
  local retries
  retries="$(
    kubectl --request-timeout=10s --namespace "${NAMESPACE}" logs deployment/bridge 2>/dev/null |
      jq -Rr 'fromjson?
        | select(.msg == "durable delegation scheduled for retry" and .error_code == "a2a_preflight_retry")
        | .error_code' |
      wc -l | tr -d ' '
  )"
  if [[ "${retries}" != "${want}" ]]; then
    echo "Error: model-outage preflight retries = ${retries}, want ${want} (DELEGATION_MAX_ATTEMPTS-1)" >&2
    return 1
  fi
  echo "==> Model-outage bounded to ${MODEL_OUTAGE_MAX_ATTEMPTS:-3} attempts (${retries} content-free retry log lines)"
}

verify_crash_recovery_evidence() {
  docker exec "${crash_runtime_node}" crictl logs "${crash_bridge_container}" >>"${crash_log_file}" 2>&1 || true
  if rg -n 'crash boundary|long room=(96|98)|input room=97|kube-system' "${crash_log_file}"; then
    echo "Error: bridge logs retained crash-scenario prompt content" >&2
    return 1
  fi

  local job_summary=""
  job_summary="$(
    kubectl --request-timeout=10s --namespace "${NAMESPACE}" exec deployment/postgres -- \
      psql -U postgres -d bridge -At -F '|' -c \
      "SELECT count(*), count(*) FILTER (WHERE state = 'delivered'), count(*) FILTER (WHERE state = 'ambiguous'), count(*) FILTER (WHERE prompt <> '' OR octet_length(payload) > 0 OR result_text <> '') FROM bridge_delegations WHERE matrix_event_id LIKE '\$crash-%';"
  )"
  if [[ "${job_summary}" != "12|9|3|0" ]]; then
    echo "Error: crash delegation summary ${job_summary}, want 12|9|3|0" >&2
    return 1
  fi
  local transaction_count=""
  transaction_count="$(
    kubectl --request-timeout=10s --namespace "${NAMESPACE}" exec deployment/postgres -- \
      psql -U postgres -d bridge -Atc \
      "SELECT count(*) FROM bridge_appservice_transactions WHERE transaction_id LIKE 'crash-%';"
  )"
  if [[ "${transaction_count}" != "18" ]]; then
    echo "Error: crash appservice transactions = ${transaction_count}, want 18" >&2
    return 1
  fi

  local control_summary=""
  control_summary="$(
    kubectl --request-timeout=10s --namespace "${NAMESPACE}" exec deployment/postgres -- \
      psql -U postgres -d bridge -At -F '|' -c \
      "SELECT count(*), count(*) FILTER (WHERE terminal_at IS NULL), count(*) FILTER (WHERE octet_length(payload) > 0) FROM bridge_delegation_controls WHERE job_id IN (SELECT job_id FROM bridge_delegations WHERE matrix_event_id LIKE '\$crash-%');"
  )"
  if [[ "${control_summary}" != *"|0|0" ]]; then
    echo "Error: crash control summary ${control_summary}, want every control terminal and content-free" >&2
    return 1
  fi
}

cleanup() {
  readonly result=$?
  trap - EXIT
  if [[ -n "${availability_shutdown_watcher_pid}" ]] &&
    kill -0 "${availability_shutdown_watcher_pid}" 2>/dev/null; then
    kill "${availability_shutdown_watcher_pid}" 2>/dev/null || true
    wait "${availability_shutdown_watcher_pid}" 2>/dev/null || true
  fi
  if [[ -n "${crash_log_file}" ]]; then
    rm -f "${crash_log_file}"
  fi
  if [[ -n "${dead_man_log_file}" ]]; then
    rm -f "${dead_man_log_file}"
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
plain_agent_runtime="$(docker image inspect --format '{{json .Config.Entrypoint}}|{{.Config.User}}' "${PLAIN_AGENT_IMAGE}")"
readonly plain_agent_runtime
if [[ "${plain_agent_runtime}" != '["/usr/local/bin/plain-a2a-agent"]|65532:65532' ]]; then
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
if [[ "${SCENARIO}" == "crash-recovery" || "${SCENARIO}" == "model-outage" ]]; then
  kubectl apply --filename "${SCRIPT_DIR}/crash-recovery-fault-proxy.yaml"
  kubectl --namespace "${NAMESPACE}" rollout status deployment/fault-proxy --timeout=60s
fi
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
if [[ "${SCENARIO}" == "integration" ]]; then
  echo "==> Waiting for the complete pre-kill audit contract"
  wait_for_pre_kill_audits
  echo "==> Waiting for the server-owned delayed-event timer"
  wait_for_dead_man_armed
  hard_kill_dead_man_bridge
fi
if [[ "${SCENARIO}" == "availability" ]]; then
  echo "==> Interrupting the active delegation with a graceful bridge pod deletion"
  disrupt_active_availability_call
fi
if [[ "${SCENARIO}" == "crash-recovery" ]]; then
  echo "==> Injecting twelve hard bridge process failures"
  disrupt_crash_recovery
fi
if [[ "${SCENARIO}" == "synapse-restart" ]]; then
  disrupt_synapse_restart
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

if [[ "${SCENARIO}" == "crash-recovery" ]]; then
  verify_crash_recovery_evidence
fi

if [[ "${SCENARIO}" == "model-outage" ]]; then
  verify_model_outage_evidence
fi

if [[ "${SCENARIO}" == "integration" ]]; then
  echo "==> Verifying complete content-free pre-kill audit contract"
  pre_kill_summary="$(pre_kill_audit_summary <"${dead_man_log_file}")"
  readonly pre_kill_summary
  if [[ "${pre_kill_summary}" != "${PRE_KILL_AUDIT_EXPECTED}" ]]; then
    echo "Error: incomplete content-free pre-kill audit contract: ${pre_kill_summary}" >&2
    exit 1
  fi
fi

echo "==> Bridge ${SCENARIO} scenario passed in $((SECONDS - STARTED_AT))s"
