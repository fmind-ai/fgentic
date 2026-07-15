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
	export llm_provider="${profile}"
	export llm_model="${model}"
	export gcp_project=fixture-project
	export vertex_region=europe-west1
	{
		kubectl kustomize "${repo_root}/infra/agentgateway"
		kubectl kustomize "${repo_root}/infra/agentgateway/providers/profiles/${profile}"
	} | flux envsubst --strict >"${tmp_dir}/${profile}.yaml"
}

(
	cd "${repo_root}/apps/matrix-a2a-bridge"
	go run ./cmd/check-model-catalog --repo-root ../..
)

# The combined policy is owned beside the always-live A2A route. Provider failure or pruning must
# therefore leave strict workload authentication and fail-closed model admission installed.
common_render="${tmp_dir}/common.yaml"
kubectl kustomize "${repo_root}/infra/agentgateway" >"${common_render}"
policy_count="$(yq eval-all -N '[.] | map(select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")) | length' "${common_render}")"
[[ "${policy_count}" == 1 ]] || fail "common A2A route must own exactly one model-admission policy"
route_count="$(yq eval-all -N '[.] | map(select(.kind == "HTTPRoute" and .metadata.name == "kagent-a2a")) | length' "${common_render}")"
[[ "${route_count}" == 1 ]] || fail "common model-admission policy must be owned beside exactly one A2A route"
[[ "$(yq -er 'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") | .spec.traffic.apiKeyAuthentication.mode' "${common_render}")" == "Strict" ]] \
	|| fail "common A2A policy must retain strict workload authentication"
for profile_dir in "${repo_root}"/infra/agentgateway/providers/profiles/*; do
	profile_policy_count="$(kubectl kustomize "${profile_dir}" | yq eval-all -N '[.] | map(select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization")) | length')"
	[[ "${profile_policy_count}" == 0 ]] \
		|| fail "provider inventory must not compete with the common A2A policy"
done

render_profile demo fgentic-demo
render_profile vertex google/gemini-2.5-flash
render_profile vllm Qwen/Qwen2.5-0.5B-Instruct
render_profile anthropic ungoverned-model

vertex_expression="$(
	yq -er 'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") |
    .spec.traffic.authorization.policy.matchExpressions[1]' "${tmp_dir}/vertex.yaml"
)"
[[ "${vertex_expression}" == *'("vertex" == "vertex" && "google/gemini-2.5-flash" == "google/gemini-2.5-flash" && request.headers["x-fgentic-data-classification"] in ["public"])'* ]] \
	|| fail "Vertex active branch is not bound to its governed public-only exact model"

anthropic_expression="$(
	yq -er 'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") |
    .spec.traffic.authorization.policy.matchExpressions[1]' "${tmp_dir}/anthropic.yaml"
)"
[[ "${anthropic_expression}" != *'"anthropic" == "anthropic"'* ]] \
	|| fail "uncataloged Anthropic profile unexpectedly admits SendMessage"

vllm_expression="$(
	yq -er 'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") |
    .spec.traffic.authorization.policy.matchExpressions[1]' "${tmp_dir}/vllm.yaml"
)"
[[ "${vllm_expression}" == *'("vllm" == "vllm" && "Qwen/Qwen2.5-0.5B-Instruct" == "Qwen/Qwen2.5-0.5B-Instruct" && request.headers["x-fgentic-data-classification"] in ["public", "approved_non_public", "restricted", "regulated"])'* ]] \
	|| fail "vLLM active branch is not bound to its governed all-class exact model"

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
