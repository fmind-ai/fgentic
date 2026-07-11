#!/usr/bin/env bash
# Prove that kagent's managed agents use agentgateway for MCP, that platform-helper's credential
# and five-tool allowlist fail closed, and that tool-call audit records contain identity/operation
# metadata without arguments or results. --runtime uses only disposable Docker containers.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
runtime=false

case "${1:-}" in
"") ;;
--runtime) runtime=true ;;
-h | --help)
	echo "usage: scripts/test-mcp-governance.sh [--runtime]" >&2
	exit 0
	;;
*)
	echo "usage: scripts/test-mcp-governance.sh [--runtime]" >&2
	exit 2
	;;
esac
if [ "$#" -gt 1 ]; then
	echo "usage: scripts/test-mcp-governance.sh [--runtime]" >&2
	exit 2
fi

fail() {
	echo "MCP governance check failed: $*" >&2
	exit 1
}

assert_equal() {
	[ "$1" = "$2" ] || fail "$3: got '$1', want '$2'"
}

assert_contains() {
	[[ "$1" == *"$2"* ]] || fail "$3: missing '$2'"
}

for command in flux helm jq kubeconform yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

tmp_dir="$(mktemp -d)"
cleanup_static() {
	rm -rf "${tmp_dir}"
}
trap cleanup_static EXIT

echo "==> Validating governed MCP manifests"
flux build kustomization cluster-overlay-validation \
	--path "${REPO_ROOT}/infra/agentgateway" \
	--kustomization-file "${REPO_ROOT}/scripts/testdata/flux-build-kustomization.yaml" \
	--dry-run \
	--in-memory-build \
	--strict-substitute >"${tmp_dir}/agentgateway.yaml"

for resource in mcp-backend.yaml mcp-route.yaml mcp-authorization.yaml mcp-audit.yaml; do
	yq -e ".resources | contains([\"${resource}\"])" \
		"${REPO_ROOT}/infra/agentgateway/kustomization.yaml" >/dev/null \
		|| fail "agentgateway kustomization omits ${resource}"
done

assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayBackend" and .metadata.name == "kagent-tools")
      | .spec.mcp.targets[0].static
      | [.host, (.port | tostring), .path, .protocol] | join("|")
    ' "${tmp_dir}/agentgateway.yaml"
})" \
	"kagent-tools.kagent.svc.cluster.local|8084|/mcp|StreamableHTTP" \
	"MCP backend target"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "HTTPRoute" and .metadata.name == "kagent-tools-mcp")
      | [.spec.rules[0].matches[0].path.type,
         .spec.rules[0].matches[0].path.value,
         .spec.rules[0].backendRefs[0].name] | join("|")
    ' "${tmp_dir}/agentgateway.yaml"
})" "Exact|/mcp|kagent-tools" "MCP route"

traffic_expression="$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayPolicy" and .metadata.name == "platform-helper-mcp-authorization")
      | .spec.traffic.authorization.policy.matchExpressions[0]
    ' "${tmp_dir}/agentgateway.yaml"
})"
mcp_expression="$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayPolicy" and .metadata.name == "platform-helper-mcp-authorization")
      | .spec.backend.mcp.authorization.policy.matchExpressions[0]
    ' "${tmp_dir}/agentgateway.yaml"
})"
traffic_runtime="${traffic_expression//$'\n'/ }"
mcp_runtime="${mcp_expression//$'\n'/ }"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayPolicy" and .metadata.name == "platform-helper-mcp-authorization")
      | [.spec.traffic.apiKeyAuthentication.mode,
         .spec.traffic.apiKeyAuthentication.secretRef.name,
         .spec.traffic.authorization.action,
         .spec.backend.mcp.authorization.action] | join("|")
    ' "${tmp_dir}/agentgateway.yaml"
})" "Strict|mcp-agent-callers|Require|Require" "MCP authentication modes"
assert_contains "${traffic_expression}" 'apiKey.agent == "platform-helper"' "per-agent authentication"
assert_contains "${traffic_expression}" 'request.path == "/mcp"' "MCP path authorization"
assert_contains "${traffic_expression}" 'request.method in ["POST", "GET", "DELETE"]' "MCP method authorization"
for tool in \
	k8s_get_resources \
	k8s_describe_resource \
	k8s_get_events \
	k8s_get_resource_yaml \
	k8s_get_pod_logs; do
	assert_contains "${mcp_expression}" "\"${tool}\"" "MCP tool allowlist"
done

assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayPolicy" and .metadata.name == "mcp-tool-audit")
      | [.spec.frontend.accessLog.filter,
         (.spec.frontend.accessLog.attributes.add | map(.name) | join(","))] | join("|")
    ' "${tmp_dir}/agentgateway.yaml"
})" \
	'request.path == "/mcp" && mcp.methodName == "tools/call"|audit.kind,fgentic.agent,mcp.method,mcp.tool.name,mcp.tool.target' \
	"content-free MCP audit contract"

gateway_version="$({
	yq eval-all -N -r '
      select(.kind == "OCIRepository" and .metadata.name == "agentgateway") | .spec.ref.tag
    ' "${REPO_ROOT}/infra/flux/sources.yaml"
})"
helm template agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
	--version "${gateway_version}" >"${tmp_dir}/agentgateway-crds.yaml"
for kind in AgentgatewayBackend AgentgatewayPolicy; do
	case "${kind}" in
	AgentgatewayBackend) crd_name=agentgatewaybackends.agentgateway.dev ;;
	AgentgatewayPolicy) crd_name=agentgatewaypolicies.agentgateway.dev ;;
	esac
	yq -o=json \
		"select(.kind == \"CustomResourceDefinition\" and .metadata.name == \"${crd_name}\") | .spec.versions[] | select(.name == \"v1alpha1\") | .schema.openAPIV3Schema" \
		"${tmp_dir}/agentgateway-crds.yaml" >"${tmp_dir}/${kind}-schema.json"
	jq -e '.type == "object" and (.properties.spec | type == "object")' \
		"${tmp_dir}/${kind}-schema.json" >/dev/null \
		|| fail "pinned ${kind} schema was not rendered"
	yq eval-all "select(.kind == \"${kind}\")" "${tmp_dir}/agentgateway.yaml" \
		| kubeconform -strict -summary -schema-location "${tmp_dir}/${kind}-schema.json"
done

server_key="$({
	yq eval-all -N -r '
      select(.kind == "Secret" and .metadata.name == "mcp-agent-callers")
      | .stringData."platform-helper" | from_json | .key
    ' "${REPO_ROOT}/infra/secrets/mcp-authorization.sops.yaml.example"
})"
agent_authorization="$({
	yq eval-all -N -r '
      select(.kind == "Secret" and .metadata.name == "platform-helper-mcp-credential")
      | .stringData.authorization
    ' "${REPO_ROOT}/infra/secrets/mcp-authorization.sops.yaml.example"
})"
assert_equal "${agent_authorization}" "Bearer ${server_key}" "example MCP credential copies"

echo "==> Validating kagent proxy and direct-egress boundary"
flux build kustomization cluster-overlay-validation \
	--path "${REPO_ROOT}/infra/kagent" \
	--kustomization-file "${REPO_ROOT}/scripts/testdata/flux-build-kustomization.yaml" \
	--dry-run \
	--in-memory-build \
	--strict-substitute >"${tmp_dir}/kagent-infra.yaml"

proxy_url="http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8080"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "HelmRelease" and .metadata.name == "kagent")
      | .spec.values.proxy.url
    ' "${tmp_dir}/kagent-infra.yaml"
})" "${proxy_url}" "kagent global proxy"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "Agent" and .metadata.name == "platform-helper")
      | .spec.declarative.tools[0].headersFrom[0]
      | [.name, .valueFrom.type, .valueFrom.name, .valueFrom.key] | join("|")
    ' "${tmp_dir}/kagent-infra.yaml"
})" \
	"Authorization|Secret|platform-helper-mcp-credential|authorization" \
	"platform-helper MCP credential reference"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "NetworkPolicy" and .metadata.name == "agent-zoo-egress")
      | [.spec.egress[].ports[]?.port] | map(select(. == 8084)) | length
    ' "${tmp_dir}/kagent-infra.yaml"
})" "0" "direct kagent-tools egress"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "NetworkPolicy" and .metadata.name == "agent-zoo-egress")
      | [.spec.egress[].ports[]?.port] | map(select(. == 8083)) | length
    ' "${tmp_dir}/kagent-infra.yaml"
})" "1" "controller discovery/session egress"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-vllm-egress")
      | [.spec.egress[]
          | select(
              ([.to[]? | select(
                .namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kagent" and
                .podSelector.matchLabels."app.kubernetes.io/name" == "kagent-tools" and
                .podSelector.matchLabels."app.kubernetes.io/instance" == "kagent"
              )] | length) == 1 and
              ([.ports[]? | select(.protocol == "TCP" and .port == 8084)] | length) == 1
            )]
      | length
    ' "${REPO_ROOT}/infra/agentgateway/providers/profiles/vllm/networkpolicy.yaml"
})" "1" "vLLM-profile gateway-to-tool egress"

kagent_repository="$({
	yq eval-all -N -r 'select(.kind == "HelmRepository" and .metadata.name == "kagent") | .spec.url' \
		"${REPO_ROOT}/infra/flux/sources.yaml"
})"
kagent_version="$(yq -er '.spec.chart.spec.version' "${REPO_ROOT}/infra/kagent/helmrelease.yaml")"
export llm_model=google/gemini-2.5-flash
yq -o=yaml '.spec.values' "${REPO_ROOT}/infra/kagent/helmrelease.yaml" \
	| flux envsubst --strict \
	| helm template kagent "${kagent_repository}/kagent" \
		--version "${kagent_version}" \
		--namespace kagent \
		--values - >"${tmp_dir}/kagent-chart.yaml"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "ConfigMap" and .metadata.name == "kagent-controller")
      | .data.PROXY_URL
    ' "${tmp_dir}/kagent-chart.yaml"
})" "${proxy_url}" "rendered kagent proxy"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "RemoteMCPServer" and .metadata.name == "kagent-tool-server")
      | .spec.url
    ' "${tmp_dir}/kagent-chart.yaml"
})" "http://kagent-tools.kagent:8084/mcp" "controller discovery endpoint"

if ! ${runtime}; then
	echo "MCP governance manifest contract passed"
	exit 0
fi

for command in curl docker sed; do
	command -v "${command}" >/dev/null 2>&1 || fail "runtime command not found: ${command}"
done
docker info >/dev/null 2>&1 || fail "Docker daemon is not running"

echo "==> Exercising authenticated MCP filtering and audit on pinned images"
gateway_image="${AGENTGATEWAY_IMAGE:-cr.agentgateway.dev/agentgateway:${gateway_version}}"
tools_image="$({
	yq eval-all -N -r '
      select(.kind == "Deployment" and .metadata.name == "kagent-tools")
      | .spec.template.spec.containers[0].image
    ' "${tmp_dir}/kagent-chart.yaml"
})"
network="fgentic-mcp-governance-$RANDOM-$$"
gateway_container="${network}-gateway"
tools_container="${network}-tools"
runtime_dir="$(mktemp -d)"

cleanup_runtime() {
	docker rm -f "${gateway_container}" "${tools_container}" >/dev/null 2>&1 || true
	docker network rm "${network}" >/dev/null 2>&1 || true
	rm -rf "${runtime_dir}"
	cleanup_static
}
trap cleanup_runtime EXIT INT TERM

cat >"${runtime_dir}/config.yaml" <<EOF
config:
  logging:
    format: json
frontendPolicies:
  accessLog:
    filter: 'request.path == "/mcp" && mcp.methodName == "tools/call"'
    add:
      audit.kind: '"fgentic.mcp_tool_call.v1"'
      fgentic.agent: apiKey.agent
      mcp.method: mcp.methodName
      mcp.tool.name: mcp.tool.name
      mcp.tool.target: mcp.tool.target
binds:
  - port: 3000
    listeners:
      - routes:
          - matches:
              - path:
                  exact: /mcp
            policies:
              apiKey:
                mode: strict
                keys:
                  - key: fixture-platform-helper-key
                    metadata: { agent: platform-helper }
                  - key: fixture-docs-key
                    metadata: { agent: docs-qa }
              authorization:
                rules:
                  - require: '${traffic_runtime}'
              mcpAuthorization:
                rules:
                  - require: '${mcp_runtime}'
            backends:
              - mcp:
                  targets:
                    - name: kagent-tools
                      mcp:
                        host: http://${tools_container}:8084/mcp
EOF

docker network create "${network}" >/dev/null
# Deliberately start the fixture without --read-only. The gateway test must hide/reject the
# write-capable tools even if an upstream configuration regression exposes them.
docker run --rm --name "${tools_container}" --network "${network}" -d \
	"${tools_image}" --port 8084 --metrics-port 8085 --tools=k8s >/dev/null
docker run --rm --name "${gateway_container}" --network "${network}" \
	-p 127.0.0.1::3000 \
	-v "${runtime_dir}/config.yaml:/config.yaml:ro" \
	-d "${gateway_image}" -f /config.yaml >/dev/null

host_port=""
for _ in {1..50}; do
	host_port="$(docker port "${gateway_container}" 3000/tcp 2>/dev/null | sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -1)"
	if [ -n "${host_port}" ] && curl --silent --output /dev/null \
		"http://127.0.0.1:${host_port}/mcp"; then
		break
	fi
	sleep 0.2
done
[ -n "${host_port}" ] || fail "agentgateway did not publish its MCP test port"

initialize_payload='{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"fgentic-test","version":"1"}}}'
fixture_bearer="Bearer fixture-platform-helper-key"
request_status() {
	local authorization="${1:-}"
	local args=(--silent --show-error --output /dev/null --write-out '%{http_code}'
		--header 'content-type: application/json'
		--header 'accept: application/json, text/event-stream'
		--data "${initialize_payload}")
	if [ -n "${authorization}" ]; then
		args+=(--header "Authorization: ${authorization}")
	fi
	curl "${args[@]}" "http://127.0.0.1:${host_port}/mcp"
}

assert_equal "$(request_status)" "401" "missing MCP credential"
assert_equal "$(request_status 'Bearer invalid')" "401" "invalid MCP credential"
assert_equal "$(request_status 'Bearer fixture-docs-key')" "403" "wrong agent credential"

headers="${runtime_dir}/headers"
curl --silent --show-error --dump-header "${headers}" --output /dev/null \
	--header "Authorization: ${fixture_bearer}" \
	--header 'content-type: application/json' \
	--header 'accept: application/json, text/event-stream' \
	--data "${initialize_payload}" \
	"http://127.0.0.1:${host_port}/mcp"
session_id="$(sed -n 's/^[Mm][Cc][Pp]-[Ss]ession-[Ii]d:[[:space:]]*\([^[:space:]]*\).*/\1/p' "${headers}" | tr -d '\r')"
[ -n "${session_id}" ] || fail "authorized MCP initialization returned no session ID"

mcp_request() {
	local payload="$1"
	curl --silent --show-error \
		--header "Authorization: ${fixture_bearer}" \
		--header 'content-type: application/json' \
		--header 'accept: application/json, text/event-stream' \
		--header "Mcp-Session-Id: ${session_id}" \
		--data "${payload}" \
		"http://127.0.0.1:${host_port}/mcp"
}

mcp_request '{"jsonrpc":"2.0","method":"notifications/initialized"}' >/dev/null
list_response="$(mcp_request '{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}' | sed -n 's/^data: //p')"
expected_tools=$'k8s_describe_resource\nk8s_get_events\nk8s_get_pod_logs\nk8s_get_resource_yaml\nk8s_get_resources'
actual_tools="$(jq -r '.result.tools[].name' <<<"${list_response}" | sort)"
assert_equal "${actual_tools}" "${expected_tools}" "gateway-filtered MCP tools"

denied_response="$(mcp_request '{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"k8s_apply_manifest","arguments":{"manifest":"MCP_ARGUMENT_SENTINEL"}}}')"
assert_contains "${denied_response}" "Unknown tool: k8s_apply_manifest" "disallowed MCP tool"
allowed_response="$(mcp_request '{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"k8s_get_resources","arguments":{"resource_type":"pods","namespace":"default"}}}' | sed -n 's/^data: //p')"
assert_equal "$(jq -r '.id' <<<"${allowed_response}")" "4" "allowed MCP tool response"

audit_record=""
for _ in {1..20}; do
	audit_record="$(docker logs "${gateway_container}" 2>&1 \
		| jq -Rrc 'fromjson? | select(.["audit.kind"] == "fgentic.mcp_tool_call.v1" and .["mcp.tool.name"] == "k8s_get_resources")' \
		| tail -1)"
	[ -n "${audit_record}" ] && break
	sleep 0.1
done
[ -n "${audit_record}" ] || fail "no content-free MCP audit record was emitted"
assert_equal "$(jq -r '.["fgentic.agent"]' <<<"${audit_record}")" "platform-helper" "MCP audit agent"
assert_equal "$(jq -r '.["mcp.method"]' <<<"${audit_record}")" "tools/call" "MCP audit method"
assert_equal "$(jq -r '.["mcp.tool.target"]' <<<"${audit_record}")" "kagent-tools" "MCP audit target"
[[ "${audit_record}" != *"fixture-platform-helper-key"* ]] || fail "MCP audit leaked the bearer credential"
[[ "${audit_record}" != *'arguments'* ]] || fail "MCP audit unexpectedly contains arguments"

denied_audit="$(docker logs "${gateway_container}" 2>&1 \
	| jq -Rrc 'fromjson? | select(.["audit.kind"] == "fgentic.mcp_tool_call.v1" and .["mcp.tool.name"] == "k8s_apply_manifest")' \
	| tail -1)"
[ -n "${denied_audit}" ] || fail "no rejected MCP audit record was emitted"
assert_equal "$(jq -r '.["fgentic.agent"]' <<<"${denied_audit}")" "platform-helper" "rejected MCP audit agent"
assert_equal "$(jq -r '.["http.status"]' <<<"${denied_audit}")" "400" "rejected MCP audit status"
[[ "${denied_audit}" != *"MCP_ARGUMENT_SENTINEL"* ]] || fail "MCP audit leaked tool arguments"
[[ "${denied_audit}" != *"fixture-platform-helper-key"* ]] || fail "MCP audit leaked the bearer credential"
[[ "${denied_audit}" != *'arguments'* ]] || fail "rejected MCP audit unexpectedly contains arguments"

terminate_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
	--request DELETE \
	--header "Authorization: ${fixture_bearer}" \
	--header "Mcp-Session-Id: ${session_id}" \
	"http://127.0.0.1:${host_port}/mcp")"
case "${terminate_status}" in
200 | 202 | 204 | 405) ;;
*) fail "authenticated MCP session termination returned ${terminate_status}" ;;
esac

echo "MCP governance runtime contract passed (${gateway_image}; ${tools_image})"
