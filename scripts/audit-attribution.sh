#!/usr/bin/env bash
# Build a content-free evidence bundle for one Matrix delegation event.
set -euo pipefail

usage() {
	printf 'Usage: %s <matrix-event-id> [log-window]\n' "${0##*/}" >&2
	printf "Example: %s '\$event-id' 15m > audit-evidence.json\n" "${0##*/}" >&2
}

if [ "$#" -lt 1 ] || [ "$#" -gt 2 ] || [ "$1" = "--help" ] || [ "$1" = "-h" ]; then
	usage
	if [ "$#" -eq 1 ] && { [ "$1" = "--help" ] || [ "$1" = "-h" ]; }; then
		exit 0
	fi
	exit 2
fi

for command in kubectl jq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		printf 'error: required command not found: %s\n' "${command}" >&2
		exit 2
	fi
done

event_id="$1"
log_window="${2:-15m}"
bridge_namespace="${BRIDGE_NAMESPACE:-bridge}"
kagent_namespace="${KAGENT_NAMESPACE:-kagent}"
gateway_namespace="${AGENTGATEWAY_NAMESPACE:-agentgateway-system}"
monitoring_namespace="${MONITORING_NAMESPACE:-monitoring}"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT

kubectl -n "${bridge_namespace}" logs deploy/matrix-a2a-bridge --since="${log_window}" \
	>"${workdir}/bridge.log"
jq -Rsc --arg event_id "${event_id}" '
  [
    split("\n")[]
    | fromjson?
	    | select(
	        .msg == "delegation audit"
	        and .log_stream == "audit"
	        and .audit_schema == "fgentic.delegation.v1"
	        and .matrix_event_id == $event_id
	      )
	  ] as $deliveries
	  | [
	      $deliveries[]
	      | select(.dedup_verdict == "accepted" or .dedup_verdict == "check_error")
	    ] as $delegations
	  | if ($delegations | length) == 0 then
	      error("no accepted delegation audit record for Matrix event in selected log window")
	    elif ($delegations | length) > 1 then
	      error("multiple delegation audit records for Matrix event; use a single-target audit event")
	    else
	      {delegation: $delegations[0], deliveries: $deliveries}
	    end
	' "${workdir}/bridge.log" >"${workdir}/bridge-audits.json"
jq '.delegation' "${workdir}/bridge-audits.json" >"${workdir}/bridge.json"
jq '.deliveries' "${workdir}/bridge-audits.json" >"${workdir}/bridge-deliveries.json"

sender_mxid="$(jq -er '.sender_mxid' "${workdir}/bridge.json")"
context_id="$(jq -er '.a2a_context_id' "${workdir}/bridge.json")"
task_id="$(jq -er '.a2a_task_id' "${workdir}/bridge.json")"

if [ -n "${context_id}" ]; then
	encoded_sender="$(jq -rn --arg value "${sender_mxid}" '$value | @uri')"
	encoded_context="$(jq -rn --arg value "${context_id}" '$value | @uri')"
	session_path="/api/v1/namespaces/${kagent_namespace}/services/http:kagent-controller:8083/proxy/api/sessions/${encoded_context}?user_id=${encoded_sender}&limit=-1&order=asc"
	tasks_path="/api/v1/namespaces/${kagent_namespace}/services/http:kagent-controller:8083/proxy/api/sessions/${encoded_context}/tasks?user_id=${encoded_sender}"

	kubectl get --raw "${session_path}" | jq -e '
    if .error == false and .data.session then
      .data
      | {
          session: {
            id: .session.id,
            user_id: .session.user_id,
            agent_id: .session.agent_id,
            created_at: .session.created_at,
            updated_at: .session.updated_at
          },
          event_count: (.events | length)
        }
    else
      error(.message // "kagent session query failed")
    end
  ' >"${workdir}/session.json"

	kubectl get --raw "${tasks_path}" | jq --arg task_id "${task_id}" '
    if .error != false then
      error(.message // "kagent task query failed")
    elif $task_id == "" then
      null
    else
      (.data | map(select(.id == $task_id))) as $matches
      | if ($matches | length) != 1 then
          error("expected exactly one kagent task matching the bridge task ID")
        else
          $matches[0]
          | {
              id,
              contextId,
              kind,
              status: {
                state: .status.state,
                timestamp: .status.timestamp
              },
              metadata: {
                kagent_app_name: .metadata.kagent_app_name,
                kagent_invocation_id: .metadata.kagent_invocation_id,
                kagent_session_id: .metadata.kagent_session_id,
                kagent_user_id: .metadata.kagent_user_id,
                kagent_usage_metadata: .metadata.kagent_usage_metadata,
                kagent_error_code: .metadata.kagent_error_code
              },
              history_count: ((.history // []) | length),
              artifact_count: ((.artifacts // []) | length)
            }
        end
    end
  ' >"${workdir}/task.json"
else
	printf 'null\n' >"${workdir}/session.json"
	printf 'null\n' >"${workdir}/task.json"
fi

kubectl -n "${gateway_namespace}" logs deploy/agentgateway-proxy --since="${log_window}" |
	jq -Rsc '
      [
        split("\n")[]
        | fromjson?
        | select(
            .scope == "request"
            and (
              .protocol == "llm"
              or ((.["http.path"] // "") | startswith("/api/a2a/"))
            )
          )
        | {
            time,
            route,
            protocol,
            http_method: .["http.method"],
            http_path: .["http.path"],
            http_status: .["http.status"],
            operation: .["gen_ai.operation.name"],
            provider: .["gen_ai.provider.name"],
            request_model: .["gen_ai.request.model"],
            response_model: .["gen_ai.response.model"],
            input_tokens: .["gen_ai.usage.input_tokens"],
            output_tokens: .["gen_ai.usage.output_tokens"],
            duration,
            error,
            reason
          }
      ]
    ' >"${workdir}/gateway.json"

prometheus_query() {
	local query="$1"
	local encoded_query
	encoded_query="$(jq -rn --arg value "${query}" '$value | @uri')"
	kubectl get --raw \
		"/api/v1/namespaces/${monitoring_namespace}/services/http:kube-prometheus-stack-prometheus:9090/proxy/api/v1/query?query=${encoded_query}" |
		jq -e 'if .status == "success" then .data.result else error(.error // "Prometheus query failed") end'
}

prometheus_query 'sum by (ghost, outcome) (fgentic_delegations_total)' \
	>"${workdir}/delegations.json"
prometheus_query 'sum by (gen_ai_token_type, gen_ai_request_model, gen_ai_response_model, gen_ai_system, route) (agentgateway_gen_ai_client_token_usage_sum)' \
	>"${workdir}/tokens.json"

generated_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
jq -n \
	--arg generated_at "${generated_at}" \
	--arg log_window "${log_window}" \
	--slurpfile bridge "${workdir}/bridge.json" \
	--slurpfile bridge_deliveries "${workdir}/bridge-deliveries.json" \
	--slurpfile session "${workdir}/session.json" \
	--slurpfile task "${workdir}/task.json" \
	--slurpfile gateway "${workdir}/gateway.json" \
	--slurpfile delegations "${workdir}/delegations.json" \
	--slurpfile tokens "${workdir}/tokens.json" '
    {
	      generated_at: $generated_at,
	      log_window: $log_window,
	      bridge: $bridge[0],
	      bridge_delivery_audits: $bridge_deliveries[0],
      kagent: {
        session: $session[0],
        task: $task[0]
      },
      agentgateway: {
        correlation: "candidate requests in the time window; no Matrix or A2A task identity is propagated",
        requests: $gateway[0]
      },
      prometheus: {
        correlation: "current aggregate series; deliberately no user, room, context, or task labels",
        delegations: $delegations[0],
        token_usage: $tokens[0]
      },
      limitations: [
        "X-User-Id is a bridge assertion accepted by kagent unsecure mode, not downstream authentication",
        "gateway requests cannot be joined deterministically to one task when calls overlap",
        "currency cost requires a separately versioned price catalog or provider billing export"
      ]
    }
  '
