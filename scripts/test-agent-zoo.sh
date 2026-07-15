#!/usr/bin/env bash
# Render every composed in-repo Agent and bridge mapping, then assert their shared authoring and
# sample-zoo security contracts. Rendered RBAC is inspected rather than trusting values names: an
# upstream chart change must not silently restore cluster-admin or write-capable tools.
set -euo pipefail

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT

fail() {
  echo "Agent authoring check failed: $*" >&2
  exit 1
}

assert_yq() {
  local expression="$1"
  local manifest="$2"
  local message="$3"
  yq eval-all -e "${expression}" "${manifest}" >/dev/null || fail "${message}"
}

echo "==> Flux-building the agent zoo"
flux build kustomization cluster-overlay-validation \
  --path infra/kagent \
  --kustomization-file scripts/testdata/flux-build-kustomization.yaml \
  --dry-run \
  --in-memory-build \
  --strict-substitute >"${tmp_dir}/agent-zoo.yaml"

actual_agents="$(yq eval-all -N -r 'select(.kind == "Agent") | .metadata.name' "${tmp_dir}/agent-zoo.yaml" | sort)"
for required_agent in docs-qa platform-helper scribe; do
  rg -q "^${required_agent}$" <<<"${actual_agents}" \
    || fail "required reference Agent ${required_agent} is absent"
done

while IFS= read -r agent; do
  assert_yq \
    "select(.kind == \"Agent\" and .metadata.name == \"${agent}\") | .spec.declarative.a2aConfig.skills | length > 0" \
    "${tmp_dir}/agent-zoo.yaml" \
    "${agent} must advertise at least one A2A skill"
  assert_yq \
    "select(.kind == \"Agent\" and .metadata.name == \"${agent}\") | .spec.declarative.deployment.serviceAccountName == \"agent-zoo-runtime\"" \
    "${tmp_dir}/agent-zoo.yaml" \
    "${agent} must use the unprivileged shared runtime ServiceAccount"
  assert_yq \
    "select(.kind == \"Agent\" and .metadata.name == \"${agent}\" and (.spec.declarative.deployment.env | length) == 3 and ([.spec.declarative.deployment.env[] | select(.value == \"false\" and (has(\"valueFrom\") | not))] | length) == 3 and ([.spec.declarative.deployment.env[] | select(.name == \"ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS\")] | length) == 1 and ([.spec.declarative.deployment.env[] | select(.name == \"OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT\")] | length) == 1 and ([.spec.declarative.deployment.env[] | select(.name == \"TRACELOOP_TRACE_CONTENT\")] | length) == 1)" \
    "${tmp_dir}/agent-zoo.yaml" \
    "${agent} must disable exactly the three reviewed GenAI trace-content paths"
  assert_yq \
    "select(.kind == \"Agent\" and .metadata.name == \"${agent}\") | .spec.declarative.systemMessage | contains(\"zoo/untrusted-content\")" \
    "${tmp_dir}/agent-zoo.yaml" \
    "${agent} must import the prompt-injection boundary"
  assert_yq \
    "select(.kind == \"Agent\" and .metadata.name == \"${agent}\") | .spec.declarative.promptTemplate.dataSources[0] as \$source | (\$source.kind == \"ConfigMap\" and \$source.name == \"agent-zoo-prompts\" and \$source.alias == \"zoo\")" \
    "${tmp_dir}/agent-zoo.yaml" \
    "${agent} must resolve its local prompt source"
done <<<"${actual_agents}"

assert_yq \
  'select(.kind == "ServiceAccount" and .metadata.name == "agent-zoo-runtime") | .automountServiceAccountToken == false' \
  "${tmp_dir}/agent-zoo.yaml" \
  "agent runtime ServiceAccount must disable the default Kubernetes API token"
assert_yq \
  'select(.kind == "ConfigMap" and .metadata.name == "agent-zoo-prompts") | (.data."untrusted-content" | length > 0) and (.data."docs-context" | length > 0)' \
  "${tmp_dir}/agent-zoo.yaml" \
  "agent prompt fragments must both be present"
assert_yq \
  'select(.kind == "Agent" and .metadata.name == "platform-helper") | .spec.declarative.tools[0].mcpServer.toolNames as $tools | (($tools | length) == 5 and $tools[0] == "k8s_get_resources" and $tools[1] == "k8s_describe_resource" and $tools[2] == "k8s_get_events" and $tools[3] == "k8s_get_resource_yaml" and $tools[4] == "k8s_get_pod_logs")' \
  "${tmp_dir}/agent-zoo.yaml" \
  "platform-helper tool allowlist changed"
assert_yq \
  'select(.kind == "Agent" and .metadata.name == "docs-qa") | (.spec.declarative.tools // []) | length == 0' \
  "${tmp_dir}/agent-zoo.yaml" \
  "docs-qa must have no tools"
assert_yq \
  'select(.kind == "Agent" and .metadata.name == "docs-qa") | .spec.declarative.promptTemplate.dataSources as $sources | (($sources | length) == 1 and $sources[0].kind == "ConfigMap" and $sources[0].name == "agent-zoo-prompts" and $sources[0].alias == "zoo")' \
  "${tmp_dir}/agent-zoo.yaml" \
  "docs-qa must use the local ConfigMap prompt source"
assert_yq \
  'select(.kind == "Agent" and .metadata.name == "scribe") | (.spec.declarative.tools // []) | length == 0' \
  "${tmp_dir}/agent-zoo.yaml" \
  "scribe must have no tools"
assert_yq \
  'select(.kind == "NetworkPolicy" and .metadata.name == "agent-zoo-egress") | .spec.policyTypes as $types | (($types | length) == 1 and $types[0] == "Egress")' \
  "${tmp_dir}/agent-zoo.yaml" \
  "agent zoo must retain its egress allowlist"
assert_yq \
  'select(.kind == "NetworkPolicy" and .metadata.name == "agent-zoo-egress") | [.spec.egress[] | select((([.to[]? | select(.namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "monitoring")] | length) > 0) and (([.ports[]? | select(.protocol == "TCP" and .port == 4317)] | length) > 0))] | length == 1' \
  "${tmp_dir}/agent-zoo.yaml" \
  "agent zoo must be able to export traces only to monitoring OTLP/gRPC"

kagent_repository="$(yq eval-all -er 'select(.kind == "HelmRepository" and .metadata.name == "kagent") | .spec.url' infra/flux/sources.yaml)"
kagent_chart="$(yq -er '.spec.chart.spec.chart' infra/kagent/helmrelease.yaml)"
kagent_version="$(yq -er '.spec.chart.spec.version' infra/kagent/helmrelease.yaml)"
kagent_crd_version="$(yq eval-all -er 'select(.kind == "HelmRelease" and .metadata.name == "kagent-crds") | .spec.chart.spec.version' infra/flux/releases.yaml)"
[[ "${kagent_version}" == "${kagent_crd_version}" ]] || fail "kagent and its CRDs must use the same version"

echo "==> Validating Agents against the exact pinned kagent CRD schema"
helm template kagent-crds "${kagent_repository}/kagent-crds" \
  --version "${kagent_crd_version}" \
  --namespace kagent >"${tmp_dir}/kagent-crds.yaml"
yq -o=json \
  'select(.kind == "CustomResourceDefinition" and .metadata.name == "agents.kagent.dev") | .spec.versions[] | select(.name == "v1alpha2") | .schema.openAPIV3Schema' \
  "${tmp_dir}/kagent-crds.yaml" >"${tmp_dir}/agent-schema.json"
jq -e '.type == "object" and (.properties.spec | type == "object")' "${tmp_dir}/agent-schema.json" >/dev/null \
  || fail "pinned Agent v1alpha2 schema was not rendered"
yq eval-all 'select(.kind == "Agent")' "${tmp_dir}/agent-zoo.yaml" \
  | kubeconform -strict -summary -schema-location "${tmp_dir}/agent-schema.json"

echo "==> Rendering kagent 0.9.11 and checking the effective tool-server RBAC"
export llm_model=google/gemini-2.5-flash
yq -o=yaml '.spec.values' infra/kagent/helmrelease.yaml \
  | flux envsubst --strict \
  | helm template kagent "${kagent_repository}/${kagent_chart}" \
    --version "${kagent_version}" \
    --namespace kagent \
    --values - >"${tmp_dir}/kagent.yaml"

# check:demo proves the nested overlay produces these exact values without network access. Render
# the pinned upstream chart here so a renamed/ignored value cannot silently restore laptop load.
yq -o=yaml '.spec.values' infra/kagent/helmrelease.yaml \
  | flux envsubst --strict \
  | helm template kagent "${kagent_repository}/${kagent_chart}" \
    --version "${kagent_version}" \
    --namespace kagent \
    --set otel.tracing.enabled=false \
    --set ui.replicas=0 \
    --set kmcp.enabled=false \
    --values - >"${tmp_dir}/kagent-demo.yaml"
expected_demo_deployments=$'kagent-controller\t1\nkagent-tools\t1\nkagent-ui\t0'
actual_demo_deployments="$(
  yq eval-all -o=json -I=0 \
    'select(.kind == "Deployment") |
    {"name": .metadata.name, "replicas": (.spec.replicas // 1)}' \
    "${tmp_dir}/kagent-demo.yaml" \
    | jq --raw-output '[.name, .replicas] | @tsv' \
    | sort
)"
[[ "${actual_demo_deployments}" == "${expected_demo_deployments}" ]] \
  || fail "demo must retain controller/tools, scale UI to zero, and disable KMCP"

assert_yq \
  'select(.kind == "Deployment" and .metadata.name == "kagent-tools") | .spec.template.spec.containers[0].args as $args | (($args | contains(["--tools=k8s"])) and ($args | contains(["--read-only"])))' \
  "${tmp_dir}/kagent.yaml" \
  "kagent tool server must expose only k8s in read-only mode"
assert_yq \
  'select(.kind == "ConfigMap" and .metadata.name == "kagent-controller") | .data.OTEL_TRACING_ENABLED == "true" and .data.OTEL_EXPORTER_OTLP_TRACES_ENDPOINT == "http://otel-collector.monitoring.svc.cluster.local:4317" and .data.OTEL_EXPORTER_OTLP_TRACES_PROTOCOL == "grpc"' \
  "${tmp_dir}/kagent.yaml" \
  "kagent controller must export traces to the central Collector over OTLP/gRPC"

expected_namespaces=$'agentgateway-system\nbridge\ngateway\nkagent\nmatrix\npostgres'
actual_namespaces="$(yq eval-all -N -r 'select(.kind == "Role" and .metadata.name == "kagent-tools-read-role") | .metadata.namespace' "${tmp_dir}/kagent.yaml" | sort -u)"
[[ "${actual_namespaces}" == "${expected_namespaces}" ]] || fail "kagent tool RBAC escaped the six evaluation namespaces"

cluster_admin="$(yq eval-all -N -r 'select(.kind == "ClusterRole" and (.metadata.name | test("^kagent-tools"))) | .metadata.name' "${tmp_dir}/kagent.yaml")"
[[ -z "${cluster_admin}" ]] || fail "kagent tools rendered cluster-scoped RBAC: ${cluster_admin}"
secret_access="$(yq eval-all -N -r 'select(.kind == "Role" and .metadata.name == "kagent-tools-read-role") | .rules[].resources[]? | select(. == "secrets")' "${tmp_dir}/kagent.yaml")"
[[ -z "${secret_access}" ]] || fail "kagent tool RBAC can read Secrets"
write_verbs="$(yq eval-all -N -r 'select(.kind == "Role" and .metadata.name == "kagent-tools-read-role") | .rules[].verbs[] | select(. != "get" and . != "list" and . != "watch")' "${tmp_dir}/kagent.yaml")"
[[ -z "${write_verbs}" ]] || fail "kagent tool RBAC contains write verbs: ${write_verbs}"

echo "==> Rendering the bridge agent map and startup profiles"
export server_name=ci.fgentic.example
kustomize build apps/matrix-a2a-bridge/deploy >"${tmp_dir}/bridge-release.yaml"
yq eval-all -o=yaml \
  'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") | .spec.values' \
  "${tmp_dir}/bridge-release.yaml" \
  | flux envsubst --strict \
  | helm template matrix-a2a-bridge apps/matrix-a2a-bridge/chart \
    --values - >"${tmp_dir}/bridge.yaml"
yq eval-all -er \
  'select(.kind == "ConfigMap" and .metadata.name == "matrix-a2a-bridge-agents") | .data."agents.yaml"' \
  "${tmp_dir}/bridge.yaml" >"${tmp_dir}/agents.yaml"
mise --cd apps/matrix-a2a-bridge exec -- \
  go run ./cmd/validate-agents \
  --schema agents.schema.json \
  --config "${tmp_dir}/agents.yaml"
expected_mappings="agent-${actual_agents//$'\n'/$'\n'agent-}"
actual_mappings="$(yq -N -r '.agents | keys | .[]' "${tmp_dir}/agents.yaml" | sort)"
[[ "${actual_mappings}" == "${expected_mappings}" ]] \
  || fail "every effective bridge mapping must match exactly one rendered in-repo Agent"
while IFS= read -r agent; do
  mapping="agent-${agent}"
  assert_yq \
    "select(.kind == \"ConfigMap\" and .metadata.name == \"matrix-a2a-bridge-agents\") | .data.\"agents.yaml\" | from_yaml | .agents.\"${mapping}\" as \$mapping | (\$mapping.namespace == \"kagent\" and \$mapping.name == \"${agent}\")" \
    "${tmp_dir}/bridge.yaml" \
    "${mapping} must resolve to the matching kagent Agent"
  assert_yq \
    "select(.kind == \"ConfigMap\" and .metadata.name == \"matrix-a2a-bridge-agents\") | .data.\"agents.yaml\" | from_yaml | .agents.\"${mapping}\".allowedSenders as \$senders | ((\$senders | length) == 1 and \$senders[0] == \"@alice:ci.fgentic.example\")" \
    "${tmp_dir}/bridge.yaml" \
    "${mapping} must carry exactly one explicit full-MXID sender (wildcards and widened allowlists are forbidden)"
  assert_yq \
    "select(.kind == \"ConfigMap\" and .metadata.name == \"matrix-a2a-bridge-agents\") | .data.\"agents.yaml\" | from_yaml | .agents.\"${mapping}\".stage as \$stage | (\$stage == \"dev\" or \$stage == \"prod\")" \
    "${tmp_dir}/bridge.yaml" \
    "${mapping} must carry an explicit dev or prod stage"
  assert_yq \
    "select(.kind == \"ConfigMap\" and .metadata.name == \"matrix-a2a-bridge-agents\") | .data.\"agents.yaml\" | from_yaml | .agents.\"${mapping}\".description | length > 0" \
    "${tmp_dir}/bridge.yaml" \
    "${mapping} must carry a startup profile fallback"
done <<<"${actual_agents}"
assert_yq \
  'select(.kind == "ConfigMap" and .metadata.name == "matrix-a2a-bridge-agents") | (.data | has("welcome.txt") | not)' \
  "${tmp_dir}/bridge.yaml" \
  "static welcome copy must not bypass sender-filtered runtime discovery"

echo "==> all in-repo Agent authoring and sample-zoo contracts OK"
