#!/usr/bin/env bash
# Parse the committed Grafana dashboard JSON and the exact ConfigMap produced by Flux/Kustomize.
# This is a static provisioning/query contract; live series and rendered panels remain a cluster
# acceptance step because Prometheus cannot manufacture runtime bridge or provider traffic.
# shellcheck disable=SC2016 # PromQL template variables and capture references are intentionally literal
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
dashboard_dir="${repo_root}/infra/observability/dashboards"
bridge_dashboard="${dashboard_dir}/fgentic-bridge.json"
llm_dashboard="${dashboard_dir}/fgentic-llm-token-cost.json"
dashboard_label_allowlist='["ghost","outcome","le","gen_ai_system","gen_ai_request_model","route","gen_ai_token_type","status"]'
label_query='
  def query_labels:
    ([scan("(?:by|without|on|ignoring|group_left|group_right)[[:space:]]*\\(([^)]*)\\)")
      | .[] | gsub("[[:space:]]"; "") | split(",")[]]
    + [scan("(?:\\{|,)[[:space:]]*([[:alpha:]_][[:alnum:]_]*)[[:space:]]*(?:=~|!~|!=|=)") | .[0]]);
  def labels_allowed:
    (test("(^|[^[:alnum:]_])(label_replace|label_join|count_values)([^[:alnum:]_]|$)") | not)
    and all(query_labels[]; . as $label | $allowed | index($label));
'

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
		and ((.templating.list // []) | length == 0)
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

assert_only_reviewed_labels() {
	local file="$1"

	jq -e --argjson allowed "${dashboard_label_allowlist}" "${label_query}
    all(.panels[].targets[]?.expr // \"\";
			labels_allowed)
	" "${file}" >/dev/null || fail "${file}: dashboard query uses a label outside the reviewed allowlist"
}

assert_dashboard_label_allowlist() {
	local unsafe_query
	local safe_query

	for unsafe_query in \
		'sum by (sender_mxid) (metric)' \
		'sum by (source_mxid) (metric)' \
		'sum by (bridge_room_id_hash) (metric)' \
		'sum by (principal_hash) (metric)' \
		'metric{matrix_user_id="example"}' \
		'label_replace(metric, "principal_hash", "$1", "route", "(.*)")'; do
		jq -en --argjson allowed "${dashboard_label_allowlist}" --arg query "${unsafe_query}" "${label_query}
			\$query | labels_allowed | not
		" >/dev/null || fail "dashboard label allowlist accepted: ${unsafe_query}"
	done

	for safe_query in \
		'sum by (route, ghost) (metric{outcome="rate_limited"})' \
		'rate(agentgateway_gen_ai_client_token_usage_sum[5m])'; do
		jq -en --argjson allowed "${dashboard_label_allowlist}" --arg query "${safe_query}" "${label_query}
			\$query | labels_allowed
		" >/dev/null || fail "dashboard label allowlist rejected: ${safe_query}"
	done
}

assert_dashboard_label_allowlist

assert_dashboard "${bridge_dashboard}" "Fgentic — Bridge" "fgentic-bridge"
assert_query "${bridge_dashboard}" "Delegations by agent and outcome" "fgentic_delegations_total"
assert_query "${bridge_dashboard}" "Delegations by agent and outcome" "sum by (ghost, outcome)"
assert_query "${bridge_dashboard}" "A2A latency histogram quantiles" "fgentic_a2a_request_seconds_bucket"
assert_query "${bridge_dashboard}" "Queue depth" "fgentic_queue_depth"
assert_query "${bridge_dashboard}" "In-flight delegations" "fgentic_inflight_delegations"
assert_query "${bridge_dashboard}" "Rate-limit rejections" 'outcome="rate_limited"'
assert_query "${bridge_dashboard}" "Deduplicated events" "fgentic_dedup_skips_total"
assert_query "${bridge_dashboard}" "Room token-budget exhaustions" "fgentic_room_token_budget_exhaustions_total"
assert_only_reviewed_labels "${bridge_dashboard}"

assert_dashboard "${llm_dashboard}" "Fgentic — LLM Token & Cost Guard" "fgentic-llm-token-cost"
assert_query "${llm_dashboard}" "Token rate by provider, model, and route" "agentgateway_gen_ai_client_token_usage_sum"
assert_query "${llm_dashboard}" "Token rate by provider, model, and route" "sum by (gen_ai_system, gen_ai_request_model, route, gen_ai_token_type)"
assert_query "${llm_dashboard}" "15-minute token burn vs guard" "system_model_token_type:agentgateway_gen_ai_client_token_usage_sum:increase15m"
assert_query "${llm_dashboard}" "15-minute token burn vs guard" 'vector(${llm_usage_budget_15m})'
assert_query "${llm_dashboard}" "Cost-catalog lookup coverage" "agentgateway_cost_catalog_lookups_total"
assert_query "${llm_dashboard}" "Token mix by provider and model" "agentgateway_gen_ai_client_token_usage_sum"
assert_text "${llm_dashboard}" "Cost and agent-attribution boundary" "no Prometheus currency-cost value"
assert_text "${llm_dashboard}" "Cost and agent-attribution boundary" "no stable Fgentic agent identity"
assert_only_reviewed_labels "${llm_dashboard}"

yq -e '
  .spec.values.grafana.sidecar.dashboards.enabled == true
  and .spec.values.grafana.sidecar.dashboards.label == "grafana_dashboard"
  and .spec.values.grafana.sidecar.dashboards.labelValue == "1"
  and .spec.values.grafana.sidecar.dashboards.searchNamespace == "monitoring"
  and .spec.values.grafana.sidecar.dashboards.provider.allowUiUpdates == false
' "${repo_root}/infra/observability/helmrelease.yaml" >/dev/null \
	|| fail "Grafana dashboard sidecar contract is missing"

# Issue #647: the Grafana sidecars must have no cluster-wide, cross-namespace, or Secret-read path.
# Assert the values that deterministically drive the subchart RBAC render: broad generated RBAC off,
# both sidecars scoped to `monitoring` ConfigMaps only, and the ServiceAccount name pinned so the
# namespaced RoleBinding subject is stable.
yq -e '
  .spec.values.grafana.rbac.create == false
  and .spec.values.grafana.serviceAccount.name == "kube-prometheus-stack-grafana"
  and .spec.values.grafana.sidecar.dashboards.resource == "configmap"
  and .spec.values.grafana.sidecar.datasources.enabled == true
  and .spec.values.grafana.sidecar.datasources.searchNamespace == "monitoring"
  and .spec.values.grafana.sidecar.datasources.resource == "configmap"
' "${repo_root}/infra/observability/helmrelease.yaml" >/dev/null \
	|| fail "Grafana sidecar RBAC scoping contract is missing"

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

# Issue #647: the rendered layer grants the Grafana ServiceAccount exactly one namespaced Role —
# ConfigMap read in `monitoring` — and nothing cluster-scoped or Secret-touching.
yq -e '
  select(.kind == "Role" and .metadata.name == "kube-prometheus-stack-grafana-sidecar")
  | .metadata.namespace == "monitoring"
    and (.rules | length == 1)
    and ((.rules[0].apiGroups | join("|")) == "")
    and ((.rules[0].resources | join(",")) == "configmaps")
    and ((.rules[0].verbs | sort | join(",")) == "get,list,watch")
' "${rendered}" >/dev/null || fail "scoped Grafana sidecar Role is missing or over-privileged"

yq -e '
  select(.kind == "RoleBinding" and .metadata.name == "kube-prometheus-stack-grafana-sidecar")
  | .metadata.namespace == "monitoring"
    and .roleRef.kind == "Role"
    and .roleRef.name == "kube-prometheus-stack-grafana-sidecar"
    and (.subjects | length == 1)
    and .subjects[0].kind == "ServiceAccount"
    and .subjects[0].name == "kube-prometheus-stack-grafana"
    and .subjects[0].namespace == "monitoring"
' "${rendered}" >/dev/null || fail "scoped Grafana sidecar RoleBinding is missing or misbound"

if yq -e 'select(.kind == "ClusterRole" or .kind == "ClusterRoleBinding") | .metadata.name' \
	"${rendered}" 2>/dev/null | grep -qi grafana; then
	fail "observability layer must not render a cluster-scoped Grafana binding"
fi
if yq -e 'select(.kind == "Role" or .kind == "ClusterRole") | .rules[]?.resources[]?' \
	"${rendered}" 2>/dev/null | grep -qx secrets; then
	fail "observability RBAC must not grant Secret access"
fi

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
