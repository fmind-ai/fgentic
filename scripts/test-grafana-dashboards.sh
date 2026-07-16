#!/usr/bin/env bash
# Parse the committed Grafana dashboard JSON and the exact ConfigMap produced by Flux/Kustomize.
# This is a static provisioning/query contract; live series and rendered panels remain a cluster
# acceptance step because Prometheus cannot manufacture runtime bridge or provider traffic.
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dashboard_dir="${repo_root}/infra/observability/dashboards"
bridge_dashboard="${dashboard_dir}/fgentic-bridge.json"
llm_dashboard="${dashboard_dir}/fgentic-llm-token-cost.json"

fail() {
	echo "Error: $*" >&2
	exit 1
}

assert_dashboard() {
	local file="$1"
	local title="$2"
	local uid="$3"

	jq -e --arg title "${title}" --arg uid "${uid}" '
    .title == $title
    and .uid == $uid
    and .editable == false
    and (.panels | length > 0)
    and (([.panels[].id] | length) == ([.panels[].id] | unique | length))
    and (([.panels[].title] | length) == ([.panels[].title] | unique | length))
    and all(.panels[] | select(.type != "text");
      .datasource.uid == "prometheus"
      and all(.targets[]; .datasource.uid == "prometheus" and ((.expr // "") | length > 0)))
  ' "${file}" >/dev/null || fail "invalid dashboard contract: ${file}"
}

assert_query() {
	local file="$1"
	local panel="$2"
	local fragment="$3"

	jq -e --arg panel "${panel}" --arg fragment "${fragment}" '
    any(.panels[];
      .title == $panel
      and any(.targets[]?; ((.expr // "") | contains($fragment))))
  ' "${file}" >/dev/null || fail "${file}: panel '${panel}' lacks query fragment '${fragment}'"
}

assert_text() {
	local file="$1"
	local panel="$2"
	local fragment="$3"

	jq -e --arg panel "${panel}" --arg fragment "${fragment}" '
    any(.panels[];
      .title == $panel
      and ((.options.content // "") | contains($fragment)))
	' "${file}" >/dev/null || fail "${file}: panel '${panel}' lacks text fragment '${fragment}'"
}

assert_no_identity_labels() {
	local file="$1"

	jq -e '
    all(.panels[].targets[]?.expr // "";
      test("(^|[^[:alnum:]_])(matrix_room_id|room_id|room|matrix_sender_id|matrix_sender|sender_id|sender|matrix_user_id|user_id|user|mxid)([^[:alnum:]_]|$)") | not)
	' "${file}" >/dev/null || fail "${file}: dashboard query exposes a raw room, sender, or MXID label"
}

assert_dashboard "${bridge_dashboard}" "Fgentic — Bridge" "fgentic-bridge"
assert_query "${bridge_dashboard}" "Delegations by agent and outcome" "fgentic_delegations_total"
assert_query "${bridge_dashboard}" "Delegations by agent and outcome" "sum by (ghost, outcome)"
assert_query "${bridge_dashboard}" "A2A latency histogram quantiles" "fgentic_a2a_request_seconds_bucket"
assert_query "${bridge_dashboard}" "Queue depth" "fgentic_queue_depth"
assert_query "${bridge_dashboard}" "In-flight delegations" "fgentic_inflight_delegations"
assert_query "${bridge_dashboard}" "Rate-limit rejections" 'outcome="rate_limited"'
assert_query "${bridge_dashboard}" "Deduplicated events" "fgentic_dedup_skips_total"
assert_no_identity_labels "${bridge_dashboard}"

assert_dashboard "${llm_dashboard}" "Fgentic — LLM Token & Cost Guard" "fgentic-llm-token-cost"
assert_query "${llm_dashboard}" "Token rate by provider, model, and route" "agentgateway_gen_ai_client_token_usage_sum"
assert_query "${llm_dashboard}" "Token rate by provider, model, and route" "sum by (gen_ai_system, gen_ai_request_model, route, gen_ai_token_type)"
assert_query "${llm_dashboard}" "15-minute token burn vs guard" "system_model_token_type:agentgateway_gen_ai_client_token_usage_sum:increase15m"
assert_query "${llm_dashboard}" "15-minute token burn vs guard" 'vector(${llm_usage_budget_15m})'
assert_query "${llm_dashboard}" "Cost-catalog lookup coverage" "agentgateway_cost_catalog_lookups_total"
assert_query "${llm_dashboard}" "Token mix by provider and model" "agentgateway_gen_ai_client_token_usage_sum"
assert_text "${llm_dashboard}" "Cost and agent-attribution boundary" "no Prometheus currency-cost value"
assert_text "${llm_dashboard}" "Cost and agent-attribution boundary" "no stable Fgentic agent identity"
assert_no_identity_labels "${llm_dashboard}"

yq -e '
  .spec.values.grafana.sidecar.dashboards.enabled == true
  and .spec.values.grafana.sidecar.dashboards.label == "grafana_dashboard"
  and .spec.values.grafana.sidecar.dashboards.labelValue == "1"
  and .spec.values.grafana.sidecar.dashboards.searchNamespace == "ALL"
  and .spec.values.grafana.sidecar.dashboards.provider.allowUiUpdates == false
' "${repo_root}/infra/observability/helmrelease.yaml" >/dev/null \
	|| fail "Grafana dashboard sidecar contract is missing"

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
rendered="${workdir}/observability.yaml"
flux build kustomization cluster-overlay-validation \
	--path "${repo_root}/infra/observability" \
	--kustomization-file "${repo_root}/scripts/testdata/flux-build-kustomization.yaml" \
	--dry-run \
	--in-memory-build \
	--strict-substitute >"${rendered}"

yq -e '
  select(.kind == "ConfigMap" and .metadata.name == "fgentic-grafana-dashboards")
  | .metadata.namespace == "monitoring"
    and .metadata.labels.grafana_dashboard == "1"
    and (.data."fgentic-bridge.json" | length > 0)
    and (.data."fgentic-llm-token-cost.json" | length > 0)
' "${rendered}" >/dev/null || fail "rendered dashboard ConfigMap is missing payloads or sidecar label"

for dashboard_key in fgentic-bridge.json fgentic-llm-token-cost.json; do
	export dashboard_key
	rendered_dashboard="${workdir}/${dashboard_key}"
	yq -er '
    select(.kind == "ConfigMap" and .metadata.name == "fgentic-grafana-dashboards")
    | .data[strenv(dashboard_key)]
  ' "${rendered}" >"${rendered_dashboard}"
	jq -e '.uid != "" and .title != "" and (.panels | length > 0)' "${rendered_dashboard}" >/dev/null \
		|| fail "rendered dashboard is not valid JSON: ${dashboard_key}"
done

jq -e '
  any(.panels[];
    .title == "15-minute token burn vs guard"
    and any(.targets[]?; .expr == "vector(100000)"))
' "${workdir}/fgentic-llm-token-cost.json" >/dev/null \
	|| fail "Flux did not substitute the dashboard token-budget threshold"

echo "Grafana dashboard contracts OK"
