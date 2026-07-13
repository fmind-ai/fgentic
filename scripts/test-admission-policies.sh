#!/usr/bin/env bash
# Validate the policy contract statically, or exercise admission without starting workloads.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly POLICY_DIR="${ROOT_DIR}/infra/policies"
readonly -a NAMESPACE_FILES=(
  "${ROOT_DIR}/infra/namespaces/namespaces.yaml"
  "${ROOT_DIR}/infra/federation/namespaces/namespace.yaml"
  "${ROOT_DIR}/apps/activitypub-agent-gateway/deploy/namespace.yaml"
)
readonly CRD_FIXTURE="${ROOT_DIR}/scripts/testdata/admission-policies/agent-crd.yaml"
readonly DIGEST_IMAGE="registry.k8s.io/pause@sha256:7031c1b283388d2c2b555df8906cc39a3fcec4ee08d94a6af11c0cfe7e99e7f5"

fail() {
  echo "error: $*" >&2
  exit 1
}

require_commands() {
  local command
  for command in "$@"; do
    command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
  done
}

static_contract() {
  require_commands kubectl yq
  local rendered
  rendered="$(kubectl kustomize "${POLICY_DIR}")"

  local policy_count binding_count failure_policy_count invalid_namespace_count
  policy_count="$(grep -c '^kind: ValidatingAdmissionPolicy$' <<<"${rendered}")"
  binding_count="$(grep -c '^kind: ValidatingAdmissionPolicyBinding$' <<<"${rendered}")"
  failure_policy_count="$(yq -r 'select(.kind == "ValidatingAdmissionPolicy" and
    .spec.failurePolicy != "Fail") | .metadata.name' <<<"${rendered}" | wc -l)"
  [[ "${policy_count}" -eq 5 ]] ||
    fail "expected exactly five admission policies"
  [[ "${binding_count}" -eq 6 ]] ||
    fail "expected exactly six admission policy bindings"
  [[ "${failure_policy_count}" -eq 0 ]] ||
    fail "every admission policy must fail closed on CEL errors"

  require_validation_fragment() {
    local policy_name="$1"
    local validation_message="$2"
    local expression_fragment="$3"
    export POLICY_NAME="${policy_name}"
    export VALIDATION_MESSAGE="${validation_message}"
    export EXPRESSION_FRAGMENT="${expression_fragment}"
    yq -e '
      select(.kind == "ValidatingAdmissionPolicy" and
        .metadata.name == strenv(POLICY_NAME)) |
      .spec.validations[] |
      select(.message == strenv(VALIDATION_MESSAGE)) |
      .expression | contains(strenv(EXPRESSION_FRAGMENT))
    ' <<<"${rendered}" >/dev/null ||
      fail "${policy_name} no longer enforces: ${validation_message}"
  }
  require_validation_fragment "no-latest-images.fgentic.dev" \
    "init container images must not use the latest tag" "object.spec.initContainers.all"
  require_validation_fragment "no-latest-images.fgentic.dev" \
    "ephemeral container images must not use the latest tag" "object.spec.ephemeralContainers.all"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must be created only in the kagent namespace" \
    'object.metadata.namespace == "kagent"'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts" \
    'runtime == "python"'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts" \
    ".memory"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts" \
    ".context"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts" \
    ".systemMessageFrom"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents cannot override the reviewed pod runtime" ".extraContainers"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "tool references must target the reviewed kagent-tool-server RemoteMCPServer" \
    "t.mcpServer.namespace"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "tool references cannot propagate request headers or override approval policy" \
    "allowedHeaders"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "tool references cannot propagate request headers or override approval policy" \
    "requireApproval"
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "platform-helper must use only its reviewed MCP Authorization Secret reference" \
    "!has(h.value)"

  yq -e '
    select(.kind == "ValidatingAdmissionPolicyBinding" and
      .metadata.name == "digest-pinned-images-audit.fgentic.dev") |
    ((.spec.validationActions | join(",")) == "Warn,Audit" and
      .spec.matchResources.namespaceSelector.matchLabels."fgentic.dev/image-policy" == "audit")
  ' <<<"${rendered}" >/dev/null || fail "digest debt must remain observable without blocking"
  yq -e '
    select(.kind == "ValidatingAdmissionPolicyBinding" and
      .metadata.name == "digest-pinned-images-enforce.fgentic.dev") |
    ((.spec.validationActions | join(",")) == "Deny,Audit" and
      .spec.matchResources.namespaceSelector.matchLabels."fgentic.dev/image-policy" == "enforce")
  ' <<<"${rendered}" >/dev/null || fail "digest-clean namespaces must fail closed"

  local namespace_count=0
  local namespace_file namespace_file_count
  for namespace_file in "${NAMESPACE_FILES[@]}"; do
    namespace_file_count="$(yq -r 'select(.kind == "Namespace") | .metadata.name' \
      "${namespace_file}" | wc -l)"
    namespace_count=$((namespace_count + namespace_file_count))
    invalid_namespace_count="$(yq -r '
      select(.kind == "Namespace") |
      select(
        .metadata.labels."fgentic.dev/managed" != "true" or
        (.metadata.labels."fgentic.dev/image-policy" != "audit" and
          .metadata.labels."fgentic.dev/image-policy" != "enforce") or
        .metadata.labels."pod-security.kubernetes.io/enforce" == null or
        .metadata.labels."pod-security.kubernetes.io/audit" == null or
        .metadata.labels."pod-security.kubernetes.io/warn" == null
      ) |
      .metadata.name
    ' "${namespace_file}" | wc -l)"
    [[ "${invalid_namespace_count}" -eq 0 ]] ||
      fail "${namespace_file#"${ROOT_DIR}/"} has a platform namespace without admission labels"
  done
  [[ "${namespace_count}" -gt 0 ]] || fail "no managed namespaces found"
  for namespace in bridges models; do
    yq -e "select(.kind == \"Namespace\" and .metadata.name == \"${namespace}\") |
      .metadata.labels.\"fgentic.dev/image-policy\" == \"enforce\"" \
      "${ROOT_DIR}/infra/namespaces/namespaces.yaml" >/dev/null ||
      fail "${namespace} must enforce digest images"
  done

  assert_digest() {
    local image="$1"
    local source="$2"
    [[ "${image}" =~ @sha256:[0-9a-f]{64}$ ]] ||
      fail "${source} is mutable in an image-policy=enforce namespace: ${image}"
  }
  local image image_list manifest
  for manifest in \
    "${ROOT_DIR}/infra/models/demo/server.yaml" \
    "${ROOT_DIR}/infra/models/vllm/model-cache.yaml"; do
    image_list="$(yq -r '.. | select(has("image")) | .image' "${manifest}")"
    while IFS= read -r image; do
      [[ -n "${image}" ]] && assert_digest "${image}" "${manifest#"${ROOT_DIR}/"}"
    done <<<"${image_list}"
  done
  # $model is a yq binding, not a shell variable.
  # shellcheck disable=SC2016
  image_list="$(yq -r '
    .spec.values.servingEngineSpec.modelSpec[] as $model |
    ($model.repository + ":" + $model.tag), $model.initContainer.image
  ' "${ROOT_DIR}/infra/models/vllm/helmrelease.yaml")"
  while IFS= read -r image; do
    [[ -n "${image}" ]] && assert_digest "${image}" "infra/models/vllm/helmrelease.yaml"
  done <<<"${image_list}"
  for manifest in "${ROOT_DIR}"/infra/bridges/*/helmrelease.yaml; do
    image="$(yq -r '.spec.values.image.repository + "@" + .spec.values.image.digest' "${manifest}")"
    assert_digest "${image}" "${manifest#"${ROOT_DIR}/"}"
  done
  grep -Fq '{{ required "image.repository is required" .Values.image.repository }}@{{ required "image.digest is required" .Values.image.digest }}' \
    "${ROOT_DIR}/infra/bridges/chart/templates/statefulset.yaml" ||
    fail "the enforced bridges chart no longer renders repository@digest"

  yq -e '
    select(.kind == "Kustomization" and .metadata.name == "policies") |
    (.spec.path == "./infra/policies" and ((.spec.dependsOn // []) | length == 0))
  ' "${ROOT_DIR}/clusters/base/infrastructure.yaml" >/dev/null ||
    fail "the cluster-scoped policy layer must be dependency-free"
  yq -e '
    select(.kind == "Kustomization" and .metadata.name == "namespaces") |
    .spec.dependsOn[] | select(.name == "policies")
  ' "${ROOT_DIR}/clusters/base/infrastructure.yaml" >/dev/null ||
    fail "namespace bootstrap must wait for admission policy"

  echo "AdmissionPolicy static contract passed"
}

runtime_contract() {
  require_commands kubectl yq
  [[ -n "${ADMISSION_POLICY_CONTEXT:-}" ]] ||
    fail "set ADMISSION_POLICY_CONTEXT to an explicitly approved disposable/local context"

  local -a kube=(kubectl --context "${ADMISSION_POLICY_CONTEXT}")
  "${kube[@]}" get --raw=/readyz >/dev/null || fail "Kubernetes API is not ready"
  "${kube[@]}" api-resources --api-group=admissionregistration.k8s.io \
    | grep -q '^validatingadmissionpolicies' || fail "ValidatingAdmissionPolicy v1 is unavailable"

  local namespace="fgentic-admission-probe-$$"
  local enforce_namespace="${namespace}-enforce"
  ADMISSION_TEST_NAMESPACE="${namespace}"
  ADMISSION_TEST_ENFORCE_NAMESPACE="${enforce_namespace}"
  ADMISSION_TEST_AGENT_NAMESPACE_OWNED=false
  ADMISSION_TEST_POLICIES_OWNED=false
  ADMISSION_TEST_CRD_OWNED=false

  cleanup() {
    local result="$1"
    local -a cleanup_kube=(kubectl --context "${ADMISSION_POLICY_CONTEXT}")
    trap - EXIT INT TERM
    "${cleanup_kube[@]}" delete namespace "${ADMISSION_TEST_NAMESPACE}" \
      "${ADMISSION_TEST_ENFORCE_NAMESPACE}" \
      --ignore-not-found --wait=true --timeout=30s >/dev/null 2>&1 || true
    if [[ "${ADMISSION_TEST_AGENT_NAMESPACE_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete namespace kagent --ignore-not-found --wait=true --timeout=30s \
        >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_CRD_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete --filename "${CRD_FIXTURE}" --ignore-not-found --wait=false \
        >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_POLICIES_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete --kustomize "${POLICY_DIR}" --ignore-not-found \
        >/dev/null 2>&1 || true
    fi
    exit "${result}"
  }
  # Arm cleanup before the first mutation. Ownership is set before each apply so even a partial
  # API failure removes only the exact resources that were absent when this test started.
  trap 'cleanup "$?"' EXIT
  trap 'cleanup 130' INT TERM

  local existing_policies existing_bindings
  existing_policies="$("${kube[@]}" get validatingadmissionpolicy -o name 2>/dev/null |
    grep -c '\.fgentic\.dev$' || true)"
  existing_bindings="$("${kube[@]}" get validatingadmissionpolicybinding -o name 2>/dev/null |
    grep -c '\.fgentic\.dev$' || true)"
  if [[ "${existing_policies}" -eq 0 && "${existing_bindings}" -eq 0 ]]; then
    ADMISSION_TEST_POLICIES_OWNED=true
    "${kube[@]}" apply --server-side --field-manager=fgentic-admission-test -k "${POLICY_DIR}" >/dev/null
  elif [[ "${existing_policies}" -ne 5 || "${existing_bindings}" -ne 6 ]]; then
    fail "found a partial fgentic.dev admission installation; refusing to mutate it"
  fi

  local deadline=$((SECONDS + 30))
  local observed_policy_count policy_json warning_count
  while ((SECONDS < deadline)); do
    policy_json="$("${kube[@]}" get validatingadmissionpolicy -o json)"
    observed_policy_count="$(yq -r '
      [.items[] | select(.metadata.name | test("\\.fgentic\\.dev$")) |
        select(.status.observedGeneration == .metadata.generation)] | length
    ' <<<"${policy_json}")"
    if [[ "${observed_policy_count}" -eq 5 ]]; then
      break
    fi
    sleep 1
  done
  [[ "${observed_policy_count}" -eq 5 ]] ||
    fail "admission policies were not observed within 30 seconds"
  warning_count="$(yq -r '[.items[].status.typeChecking.expressionWarnings[]?] | length' \
    <<<"${policy_json}")"
  [[ "${warning_count}" -eq 0 ]] ||
    fail "admission policy type checking reported warnings"

  if ! "${kube[@]}" get customresourcedefinition agents.kagent.dev >/dev/null 2>&1; then
    ADMISSION_TEST_CRD_OWNED=true
    "${kube[@]}" apply --filename "${CRD_FIXTURE}" >/dev/null
    "${kube[@]}" wait --for=condition=Established customresourcedefinition/agents.kagent.dev \
      --timeout=30s >/dev/null
  fi

  expect_denied() {
    local expected="$1"
    shift
    local output status
    set +e
    output="$("$@" 2>&1)"
    status=$?
    set -e
    [[ "${status}" -ne 0 ]] || fail "admission unexpectedly allowed: ${expected}"
    grep -Fq "${expected}" <<<"${output}" || {
      echo "${output}" >&2
      fail "denial did not include: ${expected}"
    }
  }

  expect_manifest_denied() {
    local expected="$1"
    local output status
    set +e
    output="$("${kube[@]}" create --dry-run=server --filename=- 2>&1)"
    status=$?
    set -e
    [[ "${status}" -ne 0 ]] || fail "admission unexpectedly allowed: ${expected}"
    grep -Fq "${expected}" <<<"${output}" || {
      echo "${output}" >&2
      fail "denial did not include: ${expected}"
    }
  }

  expect_agent_denied() {
    local expected="$1"
    local output status
    set +e
    output="$("${kube[@]}" apply --server-side --force-conflicts \
      --field-manager=fgentic-admission-test --dry-run=server --filename=- 2>&1)"
    status=$?
    set -e
    [[ "${status}" -ne 0 ]] || fail "admission unexpectedly allowed: ${expected}"
    grep -Fq "${expected}" <<<"${output}" || {
      echo "${output}" >&2
      fail "denial did not include: ${expected}"
    }
  }

  namespace_manifest() {
    local name="$1"
    local image_policy="$2"
    cat <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: ${name}
  labels:
    fgentic.dev/managed: "true"
    fgentic.dev/image-policy: ${image_policy}
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
EOF
  }

  pod_manifest() {
    local target_namespace="$1"
    local name="$2"
    local image="$3"
    cat <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${target_namespace}
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: probe
      image: ${image}
      command: [sh, -c, "true"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop: [ALL]
EOF
  }

  agent_manifest() {
    local target_namespace="$1"
    cat <<EOF
apiVersion: kagent.dev/v1alpha2
kind: Agent
metadata:
  name: platform-helper
  namespace: ${target_namespace}
spec:
  type: Declarative
  declarative:
    modelConfig: agentgateway-model
    deployment:
      serviceAccountName: agent-zoo-runtime
      labels:
        fgentic.dev/agent-zoo: "true"
    tools:
      - type: McpServer
        headersFrom:
          - name: Authorization
            valueFrom:
              type: Secret
              name: platform-helper-mcp-credential
              key: authorization
        mcpServer:
          name: kagent-tool-server
          kind: RemoteMCPServer
          apiGroup: kagent.dev
          toolNames: [k8s_get_resources]
EOF
  }

  expect_denied "managed namespaces must declare a valid Pod Security enforce level" \
    bash -c "printf '%s\n' 'apiVersion: v1' 'kind: Namespace' 'metadata:' \
      '  name: ${namespace}-invalid' '  labels:' '    fgentic.dev/managed: \"true\"' \
      '    fgentic.dev/image-policy: audit' | kubectl --context '${ADMISSION_POLICY_CONTEXT}' \
      create --dry-run=server --filename=-"

  namespace_manifest "${namespace}" audit | "${kube[@]}" create --filename=- >/dev/null
  namespace_manifest "${enforce_namespace}" enforce | "${kube[@]}" create --filename=- >/dev/null
  local agent_namespace=kagent
  local agent_namespace_json
  if agent_namespace_json="$("${kube[@]}" get namespace "${agent_namespace}" -o json 2>/dev/null)"; then
    yq -e '
      .metadata.labels."fgentic.dev/managed" == "true" and
      .metadata.labels."fgentic.dev/image-policy" == "audit"
    ' <<<"${agent_namespace_json}" >/dev/null ||
      fail "existing kagent namespace is outside the managed admission profile"
  else
    ADMISSION_TEST_AGENT_NAMESPACE_OWNED=true
    namespace_manifest "${agent_namespace}" audit | "${kube[@]}" create --filename=- >/dev/null
  fi

  expect_denied "container images must not use the latest tag" \
    bash -c "$(declare -f pod_manifest); pod_manifest '${namespace}' latest nginx:latest |
      kubectl --context '${ADMISSION_POLICY_CONTEXT}' create --dry-run=server --filename=-"
  expect_denied "container images must use an explicit non-latest tag or immutable sha256 digest" \
    bash -c "$(declare -f pod_manifest); pod_manifest '${namespace}' implicit-latest nginx |
      kubectl --context '${ADMISSION_POLICY_CONTEXT}' create --dry-run=server --filename=-"
  pod_manifest "${namespace}" init-latest "${DIGEST_IMAGE}" |
    yq '.spec.initContainers = [{"name": "init-probe", "image": "nginx:latest", "securityContext": {"allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]}}}]' |
    expect_manifest_denied "init container images must not use the latest tag"
  pod_manifest "${namespace}" init-implicit-latest "${DIGEST_IMAGE}" |
    yq '.spec.initContainers = [{"name": "init-probe", "image": "nginx", "securityContext": {"allowPrivilegeEscalation": false, "capabilities": {"drop": ["ALL"]}}}]' |
    expect_manifest_denied "init container images must use an explicit non-latest tag or immutable sha256 digest"
  # Ephemeral containers can only be admitted through their subresource. Keep the target Pod
  # permanently unschedulable and Never-pull so this still starts and pulls no workload.
  pod_manifest "${namespace}" ephemeral-target "${DIGEST_IMAGE}" |
    yq '.spec.schedulerName = "fgentic-admission-test" | .spec.containers[0].imagePullPolicy = "Never"' |
    "${kube[@]}" create --filename=- >/dev/null
  expect_denied "ephemeral container images must not use the latest tag" \
    "${kube[@]}" patch pod ephemeral-target --namespace "${namespace}" \
      --subresource=ephemeralcontainers --type=merge --dry-run=server \
      --patch '{"spec":{"ephemeralContainers":[{"name":"debug-probe","image":"nginx:latest","targetContainerName":"probe","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'
  expect_denied "ephemeral container images must use an explicit non-latest tag or immutable sha256 digest" \
    "${kube[@]}" patch pod ephemeral-target --namespace "${namespace}" \
      --subresource=ephemeralcontainers --type=merge --dry-run=server \
      --patch '{"spec":{"ephemeralContainers":[{"name":"debug-probe","image":"nginx","targetContainerName":"probe","securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]}}}]}}'

  local warning_output
  warning_output="$(pod_manifest "${namespace}" tagged nginx:1.29.0 |
    "${kube[@]}" create --dry-run=server --filename=- 2>&1)"
  grep -Fq "container images should end in an immutable sha256 digest" <<<"${warning_output}" ||
    fail "tag-only image did not emit the digest warning"

  pod_manifest "${namespace}" pinned "${DIGEST_IMAGE}" |
    "${kube[@]}" create --dry-run=server --filename=- >/dev/null
  expect_denied "container images should end in an immutable sha256 digest" \
    bash -c "$(declare -f pod_manifest); pod_manifest '${enforce_namespace}' tagged nginx:1.29.0 |
      kubectl --context '${ADMISSION_POLICY_CONTEXT}' create --dry-run=server --filename=-"

  expect_denied "managed namespaces allow only ClusterIP or ExternalName Services" \
    bash -c "printf '%s\n' 'apiVersion: v1' 'kind: Service' 'metadata:' '  name: exposed' \
      '  namespace: ${namespace}' 'spec:' '  type: NodePort' '  selector:' '    app: probe' \
      '  ports:' '    - port: 80' '      targetPort: 8080' |
      kubectl --context '${ADMISSION_POLICY_CONTEXT}' create --dry-run=server --filename=-"

  printf '%s\n' 'apiVersion: v1' 'kind: Service' 'metadata:' '  name: external-ip' \
    "  namespace: ${namespace}" 'spec:' '  type: ClusterIP' '  externalIPs:' \
    '    - 203.0.113.10' '  selector:' '    app: probe' '  ports:' '    - port: 80' \
    '      targetPort: 8080' |
    expect_manifest_denied "managed namespaces forbid externalIPs outside gateway/traefik"

  printf '%s\n' 'apiVersion: v1' 'kind: Service' 'metadata:' '  name: internal' \
    "  namespace: ${namespace}" 'spec:' '  type: ClusterIP' '  selector:' '    app: probe' \
    '  ports:' '    - port: 80' '      targetPort: 8080' |
    "${kube[@]}" create --dry-run=server --filename=- >/dev/null

  agent_manifest "${namespace}" |
    expect_agent_denied "managed Agents must be created only in the kagent namespace"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.modelConfig = "bypass"' |
    expect_agent_denied "managed Agents must use modelConfig agentgateway-model"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.runtime = "go"' |
    expect_agent_denied "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.memory.modelConfig = "bypass"' |
    expect_agent_denied "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts"
  agent_manifest "${agent_namespace}" |
    yq '.metadata.name = "secret-prompt-bypass" | del(.spec.declarative.tools) | .spec.declarative.systemMessageFrom = {"type": "Secret", "name": "prompt-secret", "key": "system"}' |
    expect_agent_denied "managed Agents cannot select alternate runtimes, model references, or Secret-backed prompts"
  agent_manifest "${agent_namespace}" |
    yq '.metadata.name = "agent-tool-bypass" | .spec.declarative.tools = [{"type": "Agent", "agent": {"name": "another-agent"}}]' |
    expect_agent_denied "only platform-helper may declare exactly one governed tool server"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.deployment.env = [{"name": "ESCAPE", "value": "true"}]' |
    expect_agent_denied "managed Agents cannot override the reviewed pod runtime"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.tools[0].mcpServer.namespace = "other"' |
    expect_agent_denied "tool references must target the reviewed kagent-tool-server RemoteMCPServer"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.tools[0].mcpServer.allowedHeaders = ["Authorization"]' |
    expect_agent_denied "tool references cannot propagate request headers or override approval policy"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.tools[0].mcpServer.requireApproval = ["k8s_get_resources"]' |
    expect_agent_denied "tool references cannot propagate request headers or override approval policy"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.tools[0].mcpServer.toolNames = ["shell_exec"]' |
    expect_agent_denied "toolNames must be a non-empty subset of the reviewed read-only Kubernetes tools"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.tools[0].headersFrom[0].valueFrom.name = "other"' |
    expect_agent_denied "platform-helper must use only its reviewed MCP Authorization Secret reference"
  agent_manifest "${agent_namespace}" |
    yq 'del(.spec.declarative.tools[0].headersFrom[0].valueFrom) | .spec.declarative.tools[0].headersFrom[0].value = "Bearer bypass"' |
    expect_agent_denied "platform-helper must use only its reviewed MCP Authorization Secret reference"

  yq 'select(.kind == "Agent")' \
    "${ROOT_DIR}/infra/kagent/agent-zoo.yaml" |
    "${kube[@]}" apply --server-side --force-conflicts \
      --field-manager=fgentic-admission-test --dry-run=server --filename=- >/dev/null

  expect_denied "the managed namespace Pod Security enforce level is immutable" \
    "${kube[@]}" label namespace "${namespace}" \
      pod-security.kubernetes.io/enforce=baseline --overwrite --dry-run=server
  expect_denied "managed namespaces must use the current Pod Security version" \
    "${kube[@]}" label namespace "${namespace}" \
      pod-security.kubernetes.io/enforce-version=v1.25 --overwrite --dry-run=server
  "${kube[@]}" label namespace "${namespace}" fgentic.dev/image-policy=enforce \
    --overwrite --dry-run=server >/dev/null
  expect_denied "a managed namespace cannot downgrade image-policy from enforce to audit" \
    "${kube[@]}" label namespace "${enforce_namespace}" fgentic.dev/image-policy=audit \
      --overwrite --dry-run=server
  expect_denied "managed namespaces must retain fgentic.dev/managed=true" \
    "${kube[@]}" label namespace "${namespace}" fgentic.dev/managed- --dry-run=server

  echo "AdmissionPolicy runtime contract passed on ${ADMISSION_POLICY_CONTEXT}"
  cleanup 0
}

case "${1:-}" in
  "") static_contract ;;
  --runtime) runtime_contract ;;
  *)
    echo "usage: scripts/test-admission-policies.sh [--runtime]" >&2
    exit 2
    ;;
esac
