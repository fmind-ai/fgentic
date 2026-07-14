#!/usr/bin/env bash
# Offline contract test for audit-attribution.sh. This file also serves as the mock kubectl
# executable through a temporary symlink, so the fixture cannot make a live cluster call.
set -euo pipefail

bridge_record() {
	local task_id="$1"
	jq -cn --arg task_id "${task_id}" '{
	    level: "INFO",
	    time: "2026-07-11T09:00:02Z",
	    msg: "delegation audit",
	    log_stream: "audit",
	    audit_schema: "fgentic.delegation.v1",
    sender_mxid: "@alice:fgentic.localhost",
    sender_homeserver: "fgentic.localhost",
    matrix_event_id: "$audit-event",
    matrix_origin_server_ts: 1783760400000,
    room_id: "!room:fgentic.localhost",
    reply_event_id: "$reply",
    ghost: "agent-assistant",
    ghost_mxid: "@agent-assistant:fgentic.localhost",
    agent_path: "/api/a2a/kagent/platform-assistant",
    a2a_attempted: true,
    a2a_user_id: "@alice:fgentic.localhost",
    a2a_context_id: "context-1",
	    a2a_task_id: $task_id,
	    outcome: "ok",
	    terminal_stage: "task_result",
	    terminal_reason: "completed",
	    duration_ms: 2100,
	    dedup_verdict: "accepted",
	    rate_limit_verdict: "allowed"
	  }'
}

duplicate_record() {
	jq -cn '{
	    level: "INFO",
	    time: "2026-07-11T09:00:03Z",
	    msg: "delegation audit",
	    log_stream: "audit",
	    audit_schema: "fgentic.delegation.v1",
	    sender_mxid: "@alice:fgentic.localhost",
	    sender_homeserver: "fgentic.localhost",
	    matrix_event_id: "$audit-event",
	    matrix_origin_server_ts: 1783760400000,
	    room_id: "!room:fgentic.localhost",
	    reply_event_id: "",
	    ghost: "agent-assistant",
	    ghost_mxid: "@agent-assistant:fgentic.localhost",
	    agent_path: "/api/a2a/kagent/platform-assistant",
	    a2a_attempted: false,
	    a2a_user_id: "",
	    a2a_context_id: "",
	    a2a_task_id: "",
	    outcome: "deduplicated",
	    terminal_stage: "dedup",
	    terminal_reason: "duplicate_delivery",
	    duration_ms: 0,
	    dedup_verdict: "duplicate",
	    rate_limit_verdict: "not_checked"
	  }'
}

mock_kubectl() {
	case "$*" in
	"-n bridge logs deploy/matrix-a2a-bridge --since=15m")
		case "${AUDIT_SCENARIO:-success}" in
		success) bridge_record "task-1" ;;
		absent) printf '%s\n' '{"level":"INFO","msg":"ordinary diagnostic"}' ;;
		ambiguous)
			bridge_record "task-1"
			bridge_record "task-2"
			;;
		redelivered)
			bridge_record "task-1"
			duplicate_record
			;;
		duplicate_only) duplicate_record ;;
		empty_task) bridge_record "" ;;
		*)
			printf 'unknown audit test scenario: %s\n' "${AUDIT_SCENARIO}" >&2
			return 2
			;;
		esac
		;;
	"get --raw "*"/tasks?user_id="*)
		printf '%s\n' '{"error":false,"data":[{"contextId":"context-1","id":"task-1","kind":"task","metadata":{"kagent_app_name":"kagent__NS__platform_assistant","kagent_invocation_id":"invocation-1","kagent_session_id":"context-1","kagent_user_id":"@alice:fgentic.localhost","kagent_usage_metadata":{"promptTokenCount":10,"candidatesTokenCount":2,"totalTokenCount":12}},"status":{"state":"completed","timestamp":"2026-07-11T09:00:02Z"},"history":[{}],"artifacts":[]}],"message":"ok"}'
		;;
	"get --raw "*"/api/sessions/context-1?user_id="*)
		printf '%s\n' '{"error":false,"data":{"session":{"id":"context-1","user_id":"@alice:fgentic.localhost","agent_id":"kagent__NS__platform_assistant","created_at":"2026-07-11T09:00:00Z","updated_at":"2026-07-11T09:00:02Z"},"events":[{},{}]},"message":"ok"}'
		;;
	"-n agentgateway-system logs deploy/agentgateway-proxy --since=15m")
		printf '%s\n' '{"level":"info","time":"2026-07-11T09:00:01Z","scope":"request","route":"agentgateway-system/llm","http.method":"POST","http.path":"/v1/chat/completions","http.status":200,"protocol":"llm","gen_ai.operation.name":"chat","gen_ai.provider.name":"test","gen_ai.request.model":"test-model","gen_ai.response.model":"test-model","gen_ai.usage.input_tokens":10,"gen_ai.usage.output_tokens":2,"duration":"1ms"}'
		;;
	"get --raw "*"fgentic_delegations_total"*)
		printf '%s\n' '{"status":"success","data":{"resultType":"vector","result":[{"metric":{"ghost":"agent-assistant","outcome":"ok"},"value":[1,"1"]}]}}'
		;;
	"get --raw "*"agentgateway_gen_ai_client_token_usage_sum"*)
		printf '%s\n' '{"status":"success","data":{"resultType":"vector","result":[{"metric":{"gen_ai_token_type":"input","gen_ai_request_model":"test-model"},"value":[1,"10"]}]}}'
		;;
	*)
		printf 'unexpected kubectl call: %s\n' "$*" >&2
		return 2
		;;
	esac
}

if [ "${0##*/}" = "kubectl" ]; then
	mock_kubectl "$@"
	exit
fi

for command in jq rg; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		printf 'error: required test command not found: %s\n' "${command}" >&2
		exit 2
	fi
done

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
collector="${repo_root}/scripts/audit-attribution.sh"
workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
ln -s "${repo_root}/scripts/test-audit-attribution.sh" "${workdir}/kubectl"
test_path="${workdir}:${PATH}"
event_id="\$audit-event"

AUDIT_SCENARIO=success PATH="${test_path}" "${collector}" "${event_id}" 15m \
	>"${workdir}/success.json"
jq -e '
  .bridge.matrix_event_id == "$audit-event"
  and .bridge.ghost_mxid == "@agent-assistant:fgentic.localhost"
  and .bridge.sender_mxid == .bridge.a2a_user_id
	  and .bridge.a2a_context_id == .kagent.session.session.id
	  and .bridge.a2a_task_id == .kagent.task.id
	  and .bridge.terminal_reason == "completed"
	  and .bridge.duration_ms == 2100
	  and .bridge.dedup_verdict == "accepted"
	  and .bridge.rate_limit_verdict == "allowed"
	  and (.bridge_delivery_audits | length) == 1
  and .kagent.task.metadata.kagent_usage_metadata.totalTokenCount == 12
  and (.agentgateway.requests | length) == 1
  and (.prometheus.delegations | length) == 1
  and (.prometheus.token_usage | length) == 1
' "${workdir}/success.json" >/dev/null

assert_rejected() {
	local scenario="$1"
	local message="$2"
	if AUDIT_SCENARIO="${scenario}" PATH="${test_path}" "${collector}" "${event_id}" 15m \
		>"${workdir}/${scenario}.json" 2>"${workdir}/${scenario}.err"; then
		printf 'error: collector accepted %s fixture\n' "${scenario}" >&2
		return 1
	fi
	if ! rg --fixed-strings --quiet "${message}" "${workdir}/${scenario}.err"; then
		printf 'error: %s fixture did not fail with the expected diagnostic\n' "${scenario}" >&2
		return 1
	fi
}

assert_rejected absent "no accepted delegation audit record for Matrix event"
assert_rejected duplicate_only "no accepted delegation audit record for Matrix event"
assert_rejected ambiguous "multiple delegation audit records for Matrix event"

AUDIT_SCENARIO=redelivered PATH="${test_path}" "${collector}" "${event_id}" 15m \
	>"${workdir}/redelivered.json"
jq -e '
	  .bridge.dedup_verdict == "accepted"
	  and (.bridge_delivery_audits | length) == 2
	  and .bridge_delivery_audits[1].outcome == "deduplicated"
	  and .bridge_delivery_audits[1].dedup_verdict == "duplicate"
	  and .bridge_delivery_audits[1].rate_limit_verdict == "not_checked"
	  and .bridge_delivery_audits[1].a2a_attempted == false
	' "${workdir}/redelivered.json" >/dev/null

AUDIT_SCENARIO=empty_task PATH="${test_path}" "${collector}" "${event_id}" 15m \
	>"${workdir}/empty-task.json"
jq -e '
  .bridge.a2a_task_id == ""
  and .kagent.session.session.id == "context-1"
  and .kagent.task == null
' "${workdir}/empty-task.json" >/dev/null

echo "Attribution collector fixture tests passed"
