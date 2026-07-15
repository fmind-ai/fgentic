#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-model-residency.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT

fail() {
	echo "error: $*" >&2
	exit 1
}

render_profile() {
	local profile="$1"
	local model="$2"
	export llm_model="${model}"
	export gcp_project=fixture-project
	export vertex_region=europe-west1
	kubectl kustomize "${repo_root}/infra/agentgateway/providers/profiles/${profile}" \
		| flux envsubst --strict >"${tmp_dir}/${profile}.yaml"
}

(
	cd "${repo_root}/apps/matrix-a2a-bridge"
	go run ./cmd/check-model-catalog --repo-root ../..
)

# Every selectable provider owns exactly one combined workload-authentication and model-admission
# policy. Ungoverned optional profiles admit AgentCard discovery only and deny SendMessage.
for profile_dir in "${repo_root}"/infra/agentgateway/providers/profiles/*; do
	profile="$(basename "${profile_dir}")"
	policy_count="$(kubectl kustomize "${profile_dir}" | yq eval-all -N '[.] | map(select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")) | length')"
	[[ "${policy_count}" == 1 ]] || fail "${profile} must render exactly one A2A model-admission policy"
done

render_profile demo fgentic-demo
render_profile vertex google/gemini-2.5-flash
render_profile vllm Qwen/Qwen2.5-0.5B-Instruct

vertex_expression="$(
	yq -er 'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") |
    .spec.traffic.authorization.policy.matchExpressions[1]' "${tmp_dir}/vertex.yaml"
)"
[[ "${vertex_expression}" == *'"google/gemini-2.5-flash"'* ]] \
	|| fail "Vertex admission is not bound to its governed exact model"
[[ "${vertex_expression}" == *'in ["public"]'* ]] \
	|| fail "Vertex must admit public data only"
[[ "${vertex_expression}" != *'regulated'* ]] \
	|| fail "Vertex unexpectedly admits regulated data"

vllm_expression="$(
	yq -er 'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") |
    .spec.traffic.authorization.policy.matchExpressions[1]' "${tmp_dir}/vllm.yaml"
)"
for classification in public approved_non_public restricted regulated; do
	[[ "${vllm_expression}" == *"\"${classification}\""* ]] \
		|| fail "vLLM does not admit governed ${classification} data"
done
[[ "${vllm_expression}" == *'"Qwen/Qwen2.5-0.5B-Instruct"'* ]] \
	|| fail "vLLM admission is not bound to its governed exact model"

# The admitted regulated path resolves only to the self-hosted backend, and the selected proxy's
# egress policy has no IP/CIDR escape hatch or external namespace destination.
yq eval-all -o=json '[.]' "${tmp_dir}/vllm.yaml" >"${tmp_dir}/vllm.json"
jq -e '
  (any(.[]; .kind == "AgentgatewayBackend" and .metadata.name == "llm-vllm" and
    .spec.ai.provider.host == "vllm-qwen2-5-0-5b-engine-service.models.svc.cluster.local")) and
  (any(.[]; .kind == "HTTPRoute" and .metadata.name == "llm" and
    .spec.rules[0].backendRefs[0].name == "llm-vllm")) and
  (any(.[]; .kind == "NetworkPolicy" and .metadata.name == "agentgateway-vllm-egress" and
    (.spec.policyTypes == ["Egress"]) and
    (all(.spec.egress[]; all(.to[]; has("ipBlock") | not))) and
    ([.spec.egress[].to[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] |
      unique | sort == ["agentgateway-system", "kagent", "kube-system", "models"])))
' "${tmp_dir}/vllm.json" >/dev/null || fail "vLLM render does not prove self-hosted-only model egress"

# Missing/unknown classifications fail closed: CEL requires the reviewed header, agents.schema.json
# is a closed enum, and the transport defaults absent context to the most restrictive class.
[[ "${vllm_expression}" == *'"x-fgentic-data-classification" in request.headers'* ]] \
	|| fail "model admission does not require the reviewed classification header"
jq -e '
  ."$defs".agent.properties.dataClassification.enum ==
    ["public", "approved_non_public", "restricted", "regulated"]
' "${repo_root}/apps/matrix-a2a-bridge/agents.schema.json" >/dev/null \
	|| fail "agents schema classification enum drifted"
rg -q 'classification = modelcatalog.ClassificationRegulated' \
	"${repo_root}/apps/matrix-a2a-bridge/internal/a2aclient/client.go" \
	|| fail "local transport no longer defaults missing classification to regulated"

echo "Model residency contract passed: public hyperscaler admission, regulated self-hosted admission, fail-closed unknowns"
