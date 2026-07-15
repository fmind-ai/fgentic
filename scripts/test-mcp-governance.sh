#!/usr/bin/env bash
# Prove that kagent's managed agents use agentgateway for MCP, that every backend matches its
# reviewed execution/routing/surface pin, that platform-helper's five-tool allowlist fails closed,
# and that audit records omit arguments and results. --runtime uses disposable Docker containers.
set -euo pipefail
# Deterministic collation so `sort` matches the C-ordered expected literals on any workstation
# locale (e.g. en_US.UTF-8 orders '_' after 's', reversing k8s_get_resource_yaml/k8s_get_resources).
export LC_ALL=C

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
pin_path="${REPO_ROOT}/infra/agentgateway/mcp-surface.pin.json"
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

decode_initialize_response() {
	local media_type="${1%%;*}"
	local body_path="$2"
	local response_json
	media_type="${media_type,,}"
	case "${media_type}" in
	application/json)
		response_json="$(<"${body_path}")"
		;;
	text/event-stream)
		if ! response_json="$(awk '
          function dispatch() {
            if (!has_data) {
              return
            }
            events++
            if (events > 1) {
              invalid = 1
              return
            }
            print data
            data = ""
            has_data = 0
          }
          { sub(/\r$/, "") }
          $0 == "" { dispatch(); next }
          /^data(:|$)/ {
            value = $0
            sub(/^data:?[ ]?/, "", value)
            data = has_data ? data "\n" value : value
            has_data = 1
          }
          END {
            if (!invalid) dispatch()
            if (invalid || events != 1) exit 1
          }
        ' "${body_path}")"; then
			return 1
		fi
		;;
	*) return 1 ;;
	esac

	jq -ers '
    if length != 1 then
      error("expected exactly one JSON-RPC response")
    elif (
      .[0] | type == "object" and
      .jsonrpc == "2.0" and
      .id == 1 and
      (has("error") | not) and
      (.result | type == "object") and
      (.result.protocolVersion | type == "string" and length > 0)
    ) then
      .[0].result.protocolVersion
    else
      error("invalid initialize response")
    end
  ' <<<"${response_json}" 2>/dev/null
}

assert_initialize_decode_rejected() {
	if decode_initialize_response "$1" "$2" >/dev/null 2>&1; then
		fail "$3: malformed initialize response was accepted"
	fi
}

for command in flux go helm jq kubeconform rg yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

tmp_dir="$(mktemp -d)"
cleanup_static() {
	rm -rf "${tmp_dir}"
}
trap cleanup_static EXIT

initialize_json_fixture="${tmp_dir}/initialize.json"
initialize_sse_fixture="${tmp_dir}/initialize.sse"
initialize_split_sse_fixture="${tmp_dir}/initialize-split.sse"
initialize_multiple_sse_fixture="${tmp_dir}/initialize-multiple.sse"
cat >"${initialize_json_fixture}" <<'EOF'
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"fixture-version"}}
EOF
cat >"${initialize_sse_fixture}" <<'EOF'
data: {"jsonrpc":"2.0","id":1,
data: "result":{"protocolVersion":"fixture-version"}}

EOF
cat >"${initialize_split_sse_fixture}" <<'EOF'
data: {"jsonrpc":"2.0","id":1,

data: "result":{"protocolVersion":"fixture-version"}}

EOF
cat >"${initialize_multiple_sse_fixture}" <<'EOF'
data: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"fixture-version"}}

data: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"fixture-version"}}

EOF
decoded_json_fixture="$(decode_initialize_response \
	'application/json; charset=utf-8' "${initialize_json_fixture}")"
assert_equal "${decoded_json_fixture}" "fixture-version" "JSON initialize decoding"
decoded_sse_fixture="$(decode_initialize_response 'text/event-stream' "${initialize_sse_fixture}")"
assert_equal "${decoded_sse_fixture}" "fixture-version" "SSE initialize decoding"
assert_initialize_decode_rejected 'text/event-stream' "${initialize_split_sse_fixture}" \
	"split SSE initialize event"
assert_initialize_decode_rejected 'text/event-stream' "${initialize_multiple_sse_fixture}" \
	"multiple SSE initialize events"
assert_initialize_decode_rejected 'text/plain' "${initialize_json_fixture}" \
	"unexpected initialize media type"

echo "==> Validating governed MCP manifests"
"${REPO_ROOT}/scripts/pin-mcp.sh" check --pin "${pin_path}"
(
	cd "${REPO_ROOT}/apps/matrix-a2a-bridge"
	go test ./internal/mcppin ./cmd/pin-mcp \
		-run '^(TestCompareReportsInitializeInstructionDrift|TestCompareReportsRecursiveDescriptionAndSchemaDrift|TestUpdateCheckAndVerify)$' \
		-count=1
)
flux build kustomization cluster-overlay-validation \
	--path "${REPO_ROOT}/infra/agentgateway" \
	--kustomization-file "${REPO_ROOT}/scripts/testdata/flux-build-kustomization.yaml" \
	--dry-run \
	--in-memory-build \
	--strict-substitute >"${tmp_dir}/agentgateway.yaml"

rendered_backends="$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayBackend" and .spec.mcp != null)
      | .metadata.name
    ' "${tmp_dir}/agentgateway.yaml" | sort
})"
pinned_backends="$(jq -r '.servers[].name' "${pin_path}" | sort)"
assert_equal "${rendered_backends}" "${pinned_backends}" "complete MCP backend pin coverage"
rendered_targets="$({
	yq eval-all -N -r '
      select(.kind == "AgentgatewayBackend" and .spec.mcp != null)
      | .spec.mcp.targets[].name
    ' "${tmp_dir}/agentgateway.yaml" | sort
})"
assert_equal "${rendered_targets}" "${pinned_backends}" "complete MCP target pin coverage"
assert_equal "$(jq -r '.servers | length' "${pin_path}")" "1" "MCP pin server count"

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
	yq eval-all -N -o=json -I=0 '
      select(.kind == "AgentgatewayBackend" and .metadata.name == "kagent-tools")
      | .spec.mcp.targets[]
      | select(.name == "kagent-tools")
      | .static
    ' "${tmp_dir}/agentgateway.yaml" | jq -Sc .
})" \
	"$(jq -Sc '.servers[0].provenance.backend' "${pin_path}")" \
	"pinned MCP backend routing"
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
expected_tools=$'k8s_describe_resource\nk8s_get_events\nk8s_get_pod_logs\nk8s_get_resource_yaml\nk8s_get_resources'
for tool in ${expected_tools}; do
	assert_contains "${mcp_expression}" "\"${tool}\"" "MCP tool allowlist"
	jq -e --arg tool "${tool}" '
      .servers[0].tools.supported and
      (.servers[0].tools.entries | any(.identity == $tool))
    ' "${pin_path}" >/dev/null || fail "governed tool ${tool} is absent from the raw surface pin"
done
policy_tools="$(printf '%s' "${mcp_expression}" | rg -o '"k8s_[a-z_]+"' | tr -d '"' | sort -u)"
assert_equal "${policy_tools}" "${expected_tools}" "exact MCP policy tool set"

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
      select(.kind == "Agent" and .metadata.name == "platform-helper")
      | .spec.declarative.tools[]
      | select(.type == "McpServer")
      | .mcpServer.toolNames[]
    ' "${tmp_dir}/kagent-infra.yaml" | sort
})" "${expected_tools}" "platform-helper MCP tool set"
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
flux build kustomization cluster-overlay-validation \
	--path "${REPO_ROOT}/clusters/federation" \
	--kustomization-file "${REPO_ROOT}/scripts/testdata/flux-build-kustomization.yaml" \
	--dry-run \
	--in-memory-build \
	--strict-substitute \
	--recursive \
	--local-sources GitRepository/flux-system/flux-system="${REPO_ROOT}" \
	>"${tmp_dir}/federation.yaml"
assert_equal "$({
	yq eval-all -N -r '
      [select(.kind == "AgentgatewayBackend" and .spec.mcp != null)] | length
    ' "${tmp_dir}/federation.yaml"
})" "0" "federation profile MCP backend count"
yq eval-all -N -o=yaml '
    select(.kind == "HelmRelease" and .metadata.name == "kagent") | .spec.values
  ' "${tmp_dir}/federation.yaml" \
	| helm template kagent "${kagent_repository}/kagent" \
		--version "${kagent_version}" \
		--namespace kagent \
		--values - >"${tmp_dir}/federation-kagent-chart.yaml"
assert_equal "$({
	yq eval-all -N -r '
      [select(
        (.kind == "Deployment" and .metadata.name == "kagent-tools") or
        (.kind == "RemoteMCPServer" and .metadata.name == "kagent-tool-server")
      )] | length
    ' "${tmp_dir}/federation-kagent-chart.yaml"
})" "0" "federation profile MCP server surface count"
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
assert_equal "$({
	yq eval-all -N -r '
      [select(.kind == "RemoteMCPServer")] | length
    ' "${tmp_dir}/kagent-chart.yaml"
})" "1" "complete MCP discovery-server coverage"
assert_equal "$({
	yq eval-all -N -o=json -I=0 '
      select(.kind == "RemoteMCPServer" and .metadata.name == "kagent-tool-server")
      | {"url": .spec.url, "protocol": (.spec.protocol // "STREAMABLE_HTTP")}
    ' "${tmp_dir}/kagent-chart.yaml" | jq -Sc .
})" \
	"$(jq -Sc '.servers[0].provenance.discovery' "${pin_path}")" \
	"pinned MCP discovery routing"

tool_deployment_count="$({
	yq eval-all -N -r '
      [select(.kind == "Deployment" and .metadata.name == "kagent-tools")] | length
    ' "${tmp_dir}/kagent-chart.yaml"
})"
assert_equal "${tool_deployment_count}" "1" "kagent-tools Deployment count"
tool_image="$({
	yq eval-all -N -r '
      select(.kind == "Deployment" and .metadata.name == "kagent-tools")
      | .spec.template.spec.containers[] | select(.name == "tools") | .image
    ' "${tmp_dir}/kagent-chart.yaml"
})"
tool_command="$({
	yq eval-all -N -o=json -I=0 '
      select(.kind == "Deployment" and .metadata.name == "kagent-tools")
      | .spec.template.spec.containers[] | select(.name == "tools") | .command
    ' "${tmp_dir}/kagent-chart.yaml"
})"
tool_arguments="$({
	yq eval-all -N -o=json -I=0 '
      select(.kind == "Deployment" and .metadata.name == "kagent-tools")
      | .spec.template.spec.containers[] | select(.name == "tools") | .args
    ' "${tmp_dir}/kagent-chart.yaml"
})"
tool_selector="$({
	yq eval-all -N -o=json -I=0 '
      select(.kind == "Deployment" and .metadata.name == "kagent-tools")
      | .spec.selector.matchLabels
    ' "${tmp_dir}/kagent-chart.yaml" | jq -Sc .
})"
service_selector="$({
	yq eval-all -N -o=json -I=0 '
      select(.kind == "Service" and .metadata.name == "kagent-tools")
      | .spec.selector
    ' "${tmp_dir}/kagent-chart.yaml" | jq -Sc .
})"
assert_equal "${tool_image}" "$(jq -r '.servers[0].provenance.image' "${pin_path}")" \
	"pinned MCP image"
assert_contains "${tool_image}" "@sha256:" "immutable MCP image"
assert_equal "${tool_command}" "$(jq -c '.servers[0].provenance.command' "${pin_path}")" \
	"pinned MCP command"
assert_equal "${tool_arguments}" "$(jq -c '.servers[0].provenance.arguments' "${pin_path}")" \
	"pinned MCP arguments"
assert_equal "${service_selector}" "${tool_selector}" "MCP Service selects pinned Deployment"
assert_equal "$({
	yq eval-all -N -o=json -I=0 '
      select(
        .kind == "Deployment" or .kind == "StatefulSet" or
        .kind == "DaemonSet" or .kind == "Job"
      )
      | {"kind": .kind, "name": .metadata.name, "labels": .spec.template.metadata.labels}
    ' "${tmp_dir}/kagent-chart.yaml" \
		| jq -s --argjson selector "${service_selector}" '
          [.[] | select(
            .labels as $labels
            | all($selector | to_entries[]; $labels[.key] == .value)
          )] | length
        '
})" "1" "MCP Service selects exactly one rendered workload"
assert_equal "$({
	yq eval-all -N -r '
      select(.kind == "Service" and .metadata.name == "kagent-tools")
      | .spec.ports[] | select(.name == "tools")
      | [.port, .targetPort, .protocol] | join("|")
    ' "${tmp_dir}/kagent-chart.yaml"
})" "8084|8084|TCP" "MCP Service port mapping"

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
      | .spec.template.spec.containers[] | select(.name == "tools") | .image
    ' "${tmp_dir}/kagent-chart.yaml"
})"
network="fgentic-mcp-governance-$RANDOM-$$"
gateway_container="${network}-gateway"
tools_container="${network}-tools"
pin_container="${network}-pin-tools"
runtime_dir="$(mktemp -d)"

cleanup_runtime() {
	docker rm -f "${gateway_container}" "${tools_container}" "${pin_container}" >/dev/null 2>&1 || true
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
mapfile -t pinned_command < <(jq -r '.servers[0].provenance.command[]' "${pin_path}")
mapfile -t pinned_arguments < <(jq -r '.servers[0].provenance.arguments[]' "${pin_path}")
docker run --rm --name "${pin_container}" --network "${network}" \
	-p 127.0.0.1::8084 -d --entrypoint "${pinned_command[0]}" \
	"${tools_image}" "${pinned_command[@]:1}" "${pinned_arguments[@]}" >/dev/null
for _ in {1..50}; do
	docker logs "${pin_container}" 2>&1 | rg -q 'Running KAgent Tools Server' && break
	sleep 0.2
done
pin_port="$(docker port "${pin_container}" 8084/tcp 2>/dev/null \
	| sed -n 's/.*:\([0-9][0-9]*\)$/\1/p' | head -1)"
[ -n "${pin_port}" ] || fail "read-only MCP server did not publish its pin-verification port"
"${REPO_ROOT}/scripts/pin-mcp.sh" verify \
	--name kagent-tools \
	--endpoint "http://127.0.0.1:${pin_port}/mcp" \
	--pin "${pin_path}"
docker rm -f "${pin_container}" >/dev/null
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

offered_protocol_version="2025-11-25"
pinned_protocol_version="$(jq -er '
  .servers[0].initialize.object.protocolVersion
  | select(type == "string" and length > 0)
' "${pin_path}")" || fail "MCP pin has no initialize protocol version"
initialize_payload="$(jq -cn --arg version "${offered_protocol_version}" '
  {
    jsonrpc: "2.0",
    id: 1,
    method: "initialize",
    params: {
      protocolVersion: $version,
      capabilities: {},
      clientInfo: {name: "fgentic-test", version: "1"}
    }
  }
')"
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
initialize_body="${runtime_dir}/initialize-body"
initialize_metadata_path="${runtime_dir}/initialize-metadata"
curl --silent --show-error --dump-header "${headers}" \
	--output "${initialize_body}" \
	--header "Authorization: ${fixture_bearer}" \
	--header 'content-type: application/json' \
	--header 'accept: application/json, text/event-stream' \
	--data "${initialize_payload}" \
	--write-out $'%{http_code}\n%{content_type}\n' \
	"http://127.0.0.1:${host_port}/mcp" >"${initialize_metadata_path}"
mapfile -t initialize_metadata <"${initialize_metadata_path}"
[ "${#initialize_metadata[@]}" -eq 2 ] || fail "authorized MCP initialization returned invalid HTTP metadata"
initialize_status="${initialize_metadata[0]}"
initialize_content_type="${initialize_metadata[1]}"
assert_equal "${initialize_status}" "200" "authorized MCP initialization"
session_id="$(sed -n 's/^[Mm][Cc][Pp]-[Ss]ession-[Ii]d:[[:space:]]*\([^[:space:]]*\).*/\1/p' "${headers}" | tr -d '\r')"
[ -n "${session_id}" ] || fail "authorized MCP initialization returned no session ID"

if ! negotiated_protocol_version="$(decode_initialize_response \
	"${initialize_content_type}" "${initialize_body}")"; then
	fail "authorized MCP initialization returned an invalid JSON-RPC response"
fi
assert_equal "${negotiated_protocol_version}" "${pinned_protocol_version}" \
	"negotiated MCP protocol version"
echo "MCP protocol negotiation: offered ${offered_protocol_version}; negotiated ${negotiated_protocol_version}"

mcp_request() {
	local payload="$1"
	shift
	curl --silent --show-error \
		--header "Authorization: ${fixture_bearer}" \
		--header 'content-type: application/json' \
		--header 'accept: application/json, text/event-stream' \
		--header "Mcp-Session-Id: ${session_id}" \
		--header "MCP-Protocol-Version: ${negotiated_protocol_version}" \
		"$@" \
		--data "${payload}" \
		"http://127.0.0.1:${host_port}/mcp"
}

initialized_status="$(mcp_request '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
	--output /dev/null --write-out '%{http_code}')"
case "${initialized_status}" in
200 | 202 | 204) ;;
*) fail "MCP initialized notification returned ${initialized_status}" ;;
esac
unsupported_protocol_version="1900-01-01"
unsupported_protocol_status="$(curl --silent --show-error --output /dev/null --write-out '%{http_code}' \
	--header "Authorization: ${fixture_bearer}" \
	--header 'content-type: application/json' \
	--header 'accept: application/json, text/event-stream' \
	--header "Mcp-Session-Id: ${session_id}" \
	--header "MCP-Protocol-Version: ${unsupported_protocol_version}" \
	--data '{"jsonrpc":"2.0","id":99,"method":"tools/list","params":{}}' \
	"http://127.0.0.1:${host_port}/mcp")"
assert_equal "${unsupported_protocol_status}" "400" "unsupported MCP protocol version"
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
	--header "MCP-Protocol-Version: ${negotiated_protocol_version}" \
	"http://127.0.0.1:${host_port}/mcp")"
case "${terminate_status}" in
200 | 202 | 204 | 405) ;;
*) fail "authenticated MCP session termination returned ${terminate_status}" ;;
esac

echo "MCP session protocol: subsequent requests used ${negotiated_protocol_version}; ${unsupported_protocol_version} was rejected with 400"
echo "MCP governance runtime contract passed (${gateway_image}; ${tools_image})"
