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
readonly FLUX_CRD_FIXTURE="${ROOT_DIR}/scripts/testdata/admission-policies/kustomization-crd.yaml"
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
  [[ "${policy_count}" -eq 8 ]] ||
    fail "expected exactly eight admission policies"
  [[ "${binding_count}" -eq 9 ]] ||
    fail "expected exactly nine admission policy bindings"
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
  require_match_condition_fragment() {
    local policy_name="$1"
    local condition_name="$2"
    local expression_fragment="$3"
    export POLICY_NAME="${policy_name}"
    export CONDITION_NAME="${condition_name}"
    export EXPRESSION_FRAGMENT="${expression_fragment}"
    yq -e '
      select(.kind == "ValidatingAdmissionPolicy" and
        .metadata.name == strenv(POLICY_NAME)) |
      .spec.matchConditions[] |
      select(.name == strenv(CONDITION_NAME)) |
      .expression | contains(strenv(EXPRESSION_FRAGMENT))
    ' <<<"${rendered}" >/dev/null ||
      fail "${policy_name} no longer matches: ${condition_name}"
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
    "managed Agents must disable every reviewed GenAI trace-content path" \
    'ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must disable every reviewed GenAI trace-content path" \
    'OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must disable every reviewed GenAI trace-content path" \
    'TRACELOOP_TRACE_CONTENT'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must disable every reviewed GenAI trace-content path" \
    'size(object.spec.declarative.deployment.env) == 3'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must disable every reviewed GenAI trace-content path" \
    'has(e.value)'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must disable every reviewed GenAI trace-content path" \
    'e.value == "false"'
  require_validation_fragment "approved-agent-references.fgentic.dev" \
    "managed Agents must disable every reviewed GenAI trace-content path" \
    '!has(e.valueFrom)'
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
  require_validation_fragment "model-provider-handoff-freeze.fgentic.dev" \
    "model-selection and topology overrides cannot be introduced while the model-residency handoff is guarded" \
    'object.metadata.name != "platform-settings-overrides"'
  require_validation_fragment "model-provider-handoff-freeze.fgentic.dev" \
    "llm_provider is frozen while the model-residency handoff is guarded" \
    'object.data["llm_provider"] == oldObject.data["llm_provider"]'
  require_validation_fragment "model-provider-handoff-freeze.fgentic.dev" \
    "llm_model is frozen while the model-residency handoff is guarded" \
    'object.data["llm_model"] == oldObject.data["llm_model"]'
  require_validation_fragment "model-provider-handoff-freeze.fgentic.dev" \
    "federation topology is frozen while the model-residency handoff is guarded" \
    'object.data["federation_partner_server_name"] == oldObject.data["federation_partner_server_name"]'
  require_validation_fragment "model-provider-handoff-freeze.fgentic.dev" \
    "cluster issuer is frozen while the model-residency handoff is guarded" \
    'object.data["cluster_issuer"] == oldObject.data["cluster_issuer"]'
  require_validation_fragment "model-provider-handoff-freeze.fgentic.dev" \
    "handoff-bearing platform settings cannot be deleted while the model-residency handoff is guarded" \
    'request.operation != "DELETE"'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomization identity is frozen while the model-residency handoff is guarded" \
    'object.metadata.labels["fgentic.dev/llm-provider"] == oldObject.metadata.labels["fgentic.dev/llm-provider"]'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "guarded provider Kustomizations require the complete platform model selection" \
    '"llm_model" in params.data'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations must retain the locked selected-provider identity" \
    'object.metadata.labels["fgentic.dev/llm-provider"] == params.data["llm_provider"]'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations must retain the locked selected-provider identity" \
    "variables.legacyProviderCreate"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations must retain the locked selected-model identity" \
    'object.metadata.annotations["fgentic.dev/llm-model"] == params.data["llm_model"]'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "model Kustomization identity is frozen while the model-residency handoff is guarded" \
    'object.metadata.annotations["fgentic.dev/llm-model"] == oldObject.metadata.annotations["fgentic.dev/llm-model"]'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomization paths are frozen while the model-residency handoff is guarded" \
    "object.spec.path == oldObject.spec.path"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations must retain their exact source and ownership contract" \
    'object.spec.sourceRef.name == "flux-system"'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "legacy provider bootstrap must match the exact selected Stage-A render" \
    "!variables.legacyProviderCreate"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "legacy provider bootstrap must match the exact selected Stage-A render" \
    "!has(object.spec.postBuild.substitute)"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations cannot redirect their guarded render inputs" \
    "size(object.spec.postBuild.substituteFrom) == 2"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations cannot redirect their guarded render inputs" \
    "object.spec.patches[0].patch == variables.localADCBackendPatch"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations must retain their exact current-generation dependency chain" \
    "object.spec.dependsOn[0].readyExpr == variables.modelTupleReadyExpr"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations must retain their exact current-generation dependency chain" \
    "variables.legacyProviderCreate"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "federation must not recreate the deleted local admission inventory" \
    'object.metadata.name != "agentgateway-admission"'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "inline provider substitution must equal the frozen provider identity" \
    'object.spec.postBuild.substitute["llm_provider"] == object.metadata.labels["fgentic.dev/llm-provider"]'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "inline model substitution must equal the frozen model identity" \
    'object.spec.postBuild.substitute["llm_model"] == object.metadata.annotations["fgentic.dev/llm-model"]'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomization patches cannot change during the exact handoff projection" \
    "object.spec.patches == oldObject.spec.patches"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomization substitution sources cannot change during the exact handoff projection" \
    "object.spec.postBuild.substituteFrom == oldObject.spec.postBuild.substituteFrom"
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomization specs are frozen after the exact handoff projection" \
    "object.spec == oldObject.spec"
  require_validation_fragment "model-provider-override-conflict.fgentic.dev" \
    "remove the llm_provider override before projecting the guarded model tuple" \
    '!("llm_provider" in params.data)'
  require_validation_fragment "model-provider-override-conflict.fgentic.dev" \
    "remove the llm_model override before projecting the guarded model tuple" \
    '!("llm_model" in params.data)'
  require_validation_fragment "model-provider-override-conflict.fgentic.dev" \
    "remove the federation topology override before projecting the guarded model tuple" \
    '!("federation_partner_server_name" in params.data)'
  require_validation_fragment "model-provider-override-conflict.fgentic.dev" \
    "remove the cluster_issuer override before projecting the guarded model tuple" \
    '!("cluster_issuer" in params.data)'
  require_validation_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "provider Kustomizations cannot be deleted while the model-residency handoff is guarded" \
    'request.operation != "DELETE"'
  require_match_condition_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "changed-or-projected-provider" \
    'request.operation == "UPDATE"'
  require_match_condition_fragment "model-provider-kustomization-freeze.fgentic.dev" \
    "changed-or-projected-provider" \
    "object.spec == oldObject.spec"
  require_match_condition_fragment "model-provider-override-conflict.fgentic.dev" \
    "changed-or-projected-provider" \
    "object.spec == oldObject.spec"
  local provider_noop_condition override_noop_condition condition_fragment
  provider_noop_condition="$(yq -r '
    select(.kind == "ValidatingAdmissionPolicy" and
      .metadata.name == "model-provider-kustomization-freeze.fgentic.dev") |
    .spec.matchConditions[] |
    select(.name == "changed-or-projected-provider") |
    .expression
  ' <<<"${rendered}")"
  override_noop_condition="$(yq -r '
    select(.kind == "ValidatingAdmissionPolicy" and
      .metadata.name == "model-provider-override-conflict.fgentic.dev") |
    .spec.matchConditions[] |
    select(.name == "changed-or-projected-provider") |
    .expression
  ' <<<"${rendered}")"
  [[ "${provider_noop_condition}" == "${override_noop_condition}" ]] ||
    fail "tuple and override policies must share the exact legacy no-op boundary"
  for condition_fragment in \
    'object.metadata.name == "agentgateway-provider"' \
    '!has(oldObject.metadata.labels)' \
    '!has(object.metadata.labels)' \
    '!has(oldObject.metadata.annotations)' \
    '!has(object.metadata.annotations)' \
    '"llm_provider" in object.spec.postBuild.substitute' \
    '"llm_model" in object.spec.postBuild.substitute' \
    'object.spec == oldObject.spec'; do
    grep -Fq "${condition_fragment}" <<<"${provider_noop_condition}" ||
      fail "legacy provider no-op boundary lost: ${condition_fragment}"
  done
  [[ "${provider_noop_condition}" != *CREATE* ]] ||
    fail "legacy provider CREATE must remain validation-gated"
  local legacy_create_variable
  legacy_create_variable="$(yq -r '
      select(.kind == "ValidatingAdmissionPolicy" and
        .metadata.name == "model-provider-kustomization-freeze.fgentic.dev") |
      .spec.variables[] |
      select(.name == "legacyProviderCreate") |
      .expression
    ' <<<"${rendered}")"
  for condition_fragment in \
    'request.operation == "CREATE"' \
    'object.metadata.name == "agentgateway-provider"' \
    '"fgentic.dev/llm-provider" in object.metadata.labels' \
    '"fgentic.dev/llm-model" in object.metadata.annotations' \
    '"llm_provider" in object.spec.postBuild.substitute' \
    '"llm_model" in object.spec.postBuild.substitute'; do
    grep -Fq "${condition_fragment}" <<<"${legacy_create_variable}" ||
      fail "model-provider-kustomization-freeze.fgentic.dev legacy CREATE boundary lost: ${condition_fragment}"
  done
  yq -e '
    select(.kind == "ValidatingAdmissionPolicy" and
      .metadata.name == "model-provider-override-conflict.fgentic.dev") |
    (has(.spec.variables) | not)
  ' <<<"${rendered}" >/dev/null ||
    fail "the override-conflict policy must not exempt legacy provider CREATE"

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
  yq -e '
    select(.kind == "ValidatingAdmissionPolicyBinding" and
      .metadata.name == "model-provider-handoff-freeze.fgentic.dev") |
    ((.spec.validationActions | join(",")) == "Deny,Audit" and
      .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name" ==
        "flux-system")
  ' <<<"${rendered}" >/dev/null ||
    fail "the temporary provider freeze must fail closed in flux-system"
  yq -e '
    select(.kind == "ValidatingAdmissionPolicy" and
      .metadata.name == "model-provider-handoff-freeze.fgentic.dev") |
    ((.spec.matchConstraints.resourceRules[0].operations | sort | join(",")) ==
      "CREATE,DELETE,UPDATE" and
      .spec.matchConditions[0].name == "platform-settings")
  ' <<<"${rendered}" >/dev/null ||
    fail "the temporary provider freeze must cover settings creation, mutation, and deletion"
  yq -e '
    select(.kind == "ValidatingAdmissionPolicy" and
      .metadata.name == "model-provider-kustomization-freeze.fgentic.dev") |
    ((.spec.matchConstraints.resourceRules[0].operations | sort | join(",")) ==
      "CREATE,DELETE,UPDATE" and
      .spec.matchConditions[0].name == "model-provider-kustomizations" and
      (.spec.matchConditions | map(.name) | sort | join(",")) ==
        "changed-or-projected-provider,model-provider-kustomizations" and
      .spec.paramKind.apiVersion == "v1" and
      .spec.paramKind.kind == "ConfigMap")
  ' <<<"${rendered}" >/dev/null ||
    fail "the temporary provider freeze must cover rendered Flux provider Kustomizations"
  yq -e '
    select(.kind == "ValidatingAdmissionPolicy" and
      .metadata.name == "model-provider-override-conflict.fgentic.dev") |
    ((.spec.matchConstraints.resourceRules[0].operations | sort | join(",")) ==
      "CREATE,UPDATE" and
      (.spec.matchConditions | map(.name) | sort | join(",")) ==
        "changed-or-projected-provider,model-provider-kustomizations")
  ' <<<"${rendered}" >/dev/null ||
    fail "override conflicts must skip only an unchanged legacy provider"
  yq -e '
    select(.kind == "ValidatingAdmissionPolicyBinding" and
      .metadata.name == "model-provider-kustomization-freeze.fgentic.dev") |
    ((.spec.validationActions | join(",")) == "Deny,Audit" and
      .spec.paramRef.name == "platform-settings" and
      .spec.paramRef.namespace == "flux-system" and
      .spec.paramRef.parameterNotFoundAction == "Deny" and
      .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name" ==
        "flux-system")
  ' <<<"${rendered}" >/dev/null ||
    fail "the rendered-provider freeze must fail closed in flux-system"
  yq -e '
    select(.kind == "ValidatingAdmissionPolicyBinding" and
      .metadata.name == "model-provider-override-conflict.fgentic.dev") |
    ((.spec.validationActions | join(",")) == "Deny,Audit" and
      .spec.paramRef.name == "platform-settings-overrides" and
      .spec.paramRef.namespace == "flux-system" and
      .spec.paramRef.parameterNotFoundAction == "Allow" and
      .spec.matchResources.namespaceSelector.matchLabels."kubernetes.io/metadata.name" ==
        "flux-system")
  ' <<<"${rendered}" >/dev/null ||
    fail "legacy model-selection overrides must block the exact tuple projection"

  local guarded_resource
  for guarded_resource in \
    "${ROOT_DIR}/infra/agentgateway/a2a-route.yaml" \
    "${ROOT_DIR}/infra/agentgateway/a2a-authorization.yaml" \
    "${ROOT_DIR}/infra/agentgateway/providers/profiles/demo/networkpolicy.yaml" \
    "${ROOT_DIR}/infra/agentgateway/providers/profiles/vllm/networkpolicy.yaml"; do
    yq -e '.metadata.annotations."kustomize.toolkit.fluxcd.io/prune" == "disabled"' \
      "${guarded_resource}" >/dev/null ||
      fail "the temporary provider freeze must remain paired with every handoff prune guard"
  done

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
  yq -e '
    .apiVersion == "apiextensions.k8s.io/v1" and
    .kind == "CustomResourceDefinition" and
    .metadata.name == "kustomizations.kustomize.toolkit.fluxcd.io" and
    .spec.scope == "Namespaced" and
    .spec.versions[0].name == "v1" and
    .spec.versions[0].served == true and
    .spec.versions[0].storage == true and
    ((.spec.versions[0].schema.openAPIV3Schema.properties.spec.required | sort | join(",")) ==
      "interval,prune,sourceRef") and
    ((.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties.sourceRef.required |
      sort | join(",")) == "kind,name")
  ' "${FLUX_CRD_FIXTURE}" >/dev/null ||
    fail "the admission test Flux Kustomization fixture must stay minimal and structural"
  for namespace in bridges models trivy-system; do
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
  image_list="$(yq -r '
    (.spec.values.image.registry + "/" + .spec.values.image.repository + ":" +
      .spec.values.image.tag),
    (.spec.values.trivy.image.registry + "/" + .spec.values.trivy.image.repository + ":" +
      .spec.values.trivy.image.tag)
  ' "${ROOT_DIR}/infra/trivy-operator/helmrelease.yaml")"
  while IFS= read -r image; do
    [[ -n "${image}" ]] && assert_digest "${image}" "infra/trivy-operator/helmrelease.yaml"
  done <<<"${image_list}"
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
  local fixture_provider=demo
  local fixture_model=fixture-model-a
  local fixture_conflicting_model=fixture-model-b
  "${kube[@]}" get --raw=/readyz >/dev/null || fail "Kubernetes API is not ready"
  "${kube[@]}" api-resources --api-group=admissionregistration.k8s.io \
    | grep -q '^validatingadmissionpolicies' || fail "ValidatingAdmissionPolicy v1 is unavailable"

  provider_kustomizations_manifest() {
    local manifest
    manifest="$(cat <<'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: agentgateway-provider
  namespace: flux-system
  labels:
    fgentic.dev/llm-provider: demo
  annotations:
    fgentic.dev/llm-model: fixture-model-a
spec:
  interval: 30m
  retryInterval: 2m
  path: ./infra/agentgateway/providers/profiles/demo
  prune: true
  wait: true
  timeout: 45m
  dependsOn:
    - name: agentgateway
      readyExpr: >-
        dep.metadata.labels['fgentic.dev/agentgateway-layout'] == 'split-v1' &&
        dep.status.observedGeneration == dep.metadata.generation &&
        dep.status.conditions.exists(
        e,
        e.type == 'Ready' && e.status == 'True'
        )
    - name: platform-secrets
  sourceRef:
    kind: GitRepository
    name: flux-system
  postBuild:
    substitute:
      llm_model: fixture-model-a
      llm_provider: demo
    substituteFrom:
      - kind: ConfigMap
        name: platform-settings
      - kind: ConfigMap
        name: platform-settings-overrides
        optional: true
  patches:
    - patch: |-
        - op: add
          path: /spec/policies/auth/gcp/secretRef
          value:
            name: gcp-adc
      target:
        group: agentgateway.dev
        version: v1alpha1
        kind: AgentgatewayBackend
        name: llm-vertex
        namespace: agentgateway-system
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: agentgateway-admission
  namespace: flux-system
  labels:
    fgentic.dev/llm-provider: demo
  annotations:
    fgentic.dev/llm-model: fixture-model-a
spec:
  interval: 30m
  retryInterval: 2m
  path: ./infra/agentgateway/admission
  prune: true
  wait: true
  timeout: 10m
  dependsOn:
    - name: agentgateway-provider
      readyExpr: >-
        dep.metadata.labels['fgentic.dev/llm-provider'] ==
        self.metadata.labels['fgentic.dev/llm-provider'] &&
        dep.metadata.annotations['fgentic.dev/llm-model'] ==
        self.metadata.annotations['fgentic.dev/llm-model'] &&
        dep.spec.postBuild.substitute['llm_provider'] ==
        self.spec.postBuild.substitute['llm_provider'] &&
        dep.spec.postBuild.substitute['llm_model'] ==
        self.spec.postBuild.substitute['llm_model'] &&
        dep.status.observedGeneration == dep.metadata.generation &&
        dep.status.conditions.exists(
        e,
        e.type == 'Ready' && e.status == 'True'
        )
  sourceRef:
    kind: GitRepository
    name: flux-system
  postBuild:
    substitute:
      llm_model: fixture-model-a
      llm_provider: demo
    substituteFrom:
      - kind: ConfigMap
        name: platform-settings
      - kind: ConfigMap
        name: platform-settings-overrides
        optional: true
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: agentgateway-provider-egress
  namespace: flux-system
  labels:
    fgentic.dev/llm-provider: demo
  annotations:
    fgentic.dev/llm-model: fixture-model-a
spec:
  interval: 30m
  retryInterval: 2m
  path: ./infra/agentgateway/providers/egress/demo
  prune: true
  wait: true
  dependsOn:
    - name: agentgateway-admission
      readyExpr: >-
        dep.metadata.labels['fgentic.dev/llm-provider'] ==
        self.metadata.labels['fgentic.dev/llm-provider'] &&
        dep.metadata.annotations['fgentic.dev/llm-model'] ==
        self.metadata.annotations['fgentic.dev/llm-model'] &&
        dep.spec.postBuild.substitute['llm_provider'] ==
        self.spec.postBuild.substitute['llm_provider'] &&
        dep.spec.postBuild.substitute['llm_model'] ==
        self.spec.postBuild.substitute['llm_model'] &&
        dep.status.observedGeneration == dep.metadata.generation &&
        dep.status.conditions.exists(
        e,
        e.type == 'Ready' && e.status == 'True'
        )
  sourceRef:
    kind: GitRepository
    name: flux-system
  postBuild:
    substitute:
      llm_model: fixture-model-a
      llm_provider: demo
EOF
    )"
    FIXTURE_PROVIDER="${fixture_provider}" FIXTURE_MODEL="${fixture_model}" yq '
      (.metadata.labels."fgentic.dev/llm-provider") = strenv(FIXTURE_PROVIDER) |
      (.metadata.annotations."fgentic.dev/llm-model") = strenv(FIXTURE_MODEL) |
      (.spec.postBuild.substitute.llm_provider) = strenv(FIXTURE_PROVIDER) |
      (.spec.postBuild.substitute.llm_model) = strenv(FIXTURE_MODEL) |
      (select(.metadata.name == "agentgateway-provider") | .spec.path) =
        "./infra/agentgateway/providers/profiles/" + strenv(FIXTURE_PROVIDER) |
      (select(.metadata.name == "agentgateway-provider-egress") | .spec.path) =
        "./infra/agentgateway/providers/egress/" + strenv(FIXTURE_PROVIDER)
    ' <<<"${manifest}"
  }

  selected_provider_kustomizations_manifest() {
    if [[ "${federation_topology}" == true ]]; then
      provider_kustomizations_manifest |
        yq '
          select(.metadata.name != "agentgateway-admission") |
          (select(.metadata.name == "agentgateway-provider") | .spec) |= del(.patches) |
          (select(.metadata.name == "agentgateway-provider-egress") |
            .spec.dependsOn[0].name) = "agentgateway-provider"
        '
      return
    fi
    provider_kustomizations_manifest
  }

  legacy_provider_kustomization_manifest() {
    local manifest
    manifest="$(cat <<'EOF'
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: agentgateway-provider
  namespace: flux-system
spec:
  interval: 30m
  retryInterval: 2m
  path: ./infra/agentgateway/providers/profiles/demo
  prune: true
  wait: true
  timeout: 45m
  dependsOn:
    - name: agentgateway
    - name: platform-secrets
  sourceRef:
    kind: GitRepository
    name: flux-system
  postBuild:
    substituteFrom:
      - kind: ConfigMap
        name: platform-settings
      - kind: ConfigMap
        name: platform-settings-overrides
        optional: true
  patches:
    - patch: |-
        - op: add
          path: /spec/policies/auth/gcp/secretRef
          value:
            name: gcp-adc
      target:
        group: agentgateway.dev
        version: v1alpha1
        kind: AgentgatewayBackend
        name: llm-vertex
        namespace: agentgateway-system
EOF
    )"
    FIXTURE_PROVIDER="${fixture_provider}" yq \
      '.spec.path = "./infra/agentgateway/providers/profiles/" + strenv(FIXTURE_PROVIDER)' \
      <<<"${manifest}"
  }

  selected_legacy_provider_kustomization_manifest() {
    if [[ "${federation_topology}" == true ]]; then
      legacy_provider_kustomization_manifest | yq 'del(.spec.patches)'
      return
    fi
    legacy_provider_kustomization_manifest
  }

  local namespace="fgentic-admission-probe-$$"
  local enforce_namespace="${namespace}-enforce"
  ADMISSION_TEST_NAMESPACE="${namespace}"
  ADMISSION_TEST_ENFORCE_NAMESPACE="${enforce_namespace}"
  ADMISSION_TEST_AGENT_NAMESPACE_OWNED=false
  ADMISSION_TEST_POLICIES_OWNED=false
  ADMISSION_TEST_CRD_OWNED=false
  ADMISSION_TEST_PLATFORM_SETTINGS_OWNED=false
  ADMISSION_TEST_PLATFORM_SETTINGS_OVERRIDE_OWNED=false
  ADMISSION_TEST_FLUX_NAMESPACE_OWNED=false
  ADMISSION_TEST_FLUX_CRD_OWNED=false
  ADMISSION_TEST_PROVIDER_KUSTOMIZATIONS_OWNED=false

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
    # Remove the owned policies before their provider-delete guard so the inert Flux fixtures can
    # be cleaned without weakening or mutating a pre-existing installation.
    if [[ "${ADMISSION_TEST_POLICIES_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete --kustomize "${POLICY_DIR}" --ignore-not-found \
        >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_PROVIDER_KUSTOMIZATIONS_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete kustomization \
        agentgateway-provider agentgateway-admission agentgateway-provider-egress \
        --namespace flux-system --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_PLATFORM_SETTINGS_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete configmap platform-settings --namespace flux-system \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_PLATFORM_SETTINGS_OVERRIDE_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete configmap platform-settings-overrides --namespace flux-system \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_FLUX_CRD_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete --filename "${FLUX_CRD_FIXTURE}" \
        --ignore-not-found --wait=false >/dev/null 2>&1 || true
    fi
    if [[ "${ADMISSION_TEST_FLUX_NAMESPACE_OWNED}" == true ]]; then
      "${cleanup_kube[@]}" delete namespace flux-system --ignore-not-found \
        --wait=true --timeout=30s >/dev/null 2>&1 || true
    fi
    exit "${result}"
  }
  # Arm cleanup before the first mutation. Ownership is set before each apply so even a partial
  # API failure removes only the exact resources that were absent when this test started.
  trap 'cleanup "$?"' EXIT
  trap 'cleanup 130' INT TERM

  local existing_policies existing_bindings fresh_policy_install=false
  existing_policies="$("${kube[@]}" get validatingadmissionpolicy -o name 2>/dev/null |
    grep -c '\.fgentic\.dev$' || true)"
  existing_bindings="$("${kube[@]}" get validatingadmissionpolicybinding -o name 2>/dev/null |
    grep -c '\.fgentic\.dev$' || true)"
  if [[ "${existing_policies}" -eq 0 && "${existing_bindings}" -eq 0 ]]; then
    fresh_policy_install=true
  elif [[ "${existing_policies}" -ne 8 || "${existing_bindings}" -ne 9 ]]; then
    fail "found a partial fgentic.dev admission installation; refusing to mutate it"
  fi
  if [[ "${fresh_policy_install}" == true ]]; then
    if "${kube[@]}" get deployment --all-namespaces -o json 2>/dev/null |
      yq -e '.items[] | select(.metadata.name == "kustomize-controller")' \
        >/dev/null; then
      fail "fresh policy fixtures require a controller-free disposable context; reconcile policies through Flux first"
    fi
    if "${kube[@]}" get gitrepository --all-namespaces -o name 2>/dev/null |
      grep -q .; then
      fail "fresh policy fixtures require a controller-free disposable context; reconcile policies through Flux first"
    fi
  fi

  if ! "${kube[@]}" get namespace flux-system >/dev/null 2>&1; then
    [[ "${fresh_policy_install}" == true ]] ||
      fail "pre-installed policies require the existing flux-system namespace"
    ADMISSION_TEST_FLUX_NAMESPACE_OWNED=true
    "${kube[@]}" create namespace flux-system >/dev/null
  fi
  if ! "${kube[@]}" get customresourcedefinition \
    kustomizations.kustomize.toolkit.fluxcd.io >/dev/null 2>&1; then
    [[ "${fresh_policy_install}" == true ]] ||
      fail "pre-installed policies require the Flux Kustomization CRD"
    ADMISSION_TEST_FLUX_CRD_OWNED=true
    "${kube[@]}" apply --filename "${FLUX_CRD_FIXTURE}" >/dev/null
    "${kube[@]}" wait --for=condition=Established \
      customresourcedefinition/kustomizations.kustomize.toolkit.fluxcd.io \
      --timeout=30s >/dev/null
  fi
  local platform_settings_json
  if ! platform_settings_json="$("${kube[@]}" get configmap platform-settings \
    --namespace flux-system -o json 2>/dev/null)"; then
    [[ "${fresh_policy_install}" == true ]] ||
      fail "pre-installed policies require platform-settings as their fail-closed parameter"
    ADMISSION_TEST_PLATFORM_SETTINGS_OWNED=true
    printf '%s\n' \
      'apiVersion: v1' \
      'kind: ConfigMap' \
      'metadata:' \
      '  name: platform-settings' \
      '  namespace: flux-system' \
      'data:' \
      '  cluster_issuer: local-ca' \
      '  llm_model: fixture-model-a' \
      '  llm_provider: demo' \
      '  server_name: fgentic.localhost' |
      "${kube[@]}" create --filename=- >/dev/null
    platform_settings_json="$("${kube[@]}" get configmap platform-settings \
      --namespace flux-system -o json)"
  fi
  fixture_provider="$(yq -r '.data.llm_provider // ""' <<<"${platform_settings_json}")"
  fixture_model="$(yq -r '.data.llm_model // ""' <<<"${platform_settings_json}")"
  [[ -n "${fixture_provider}" && -n "${fixture_model}" ]] ||
    fail "platform-settings must carry the complete guarded model tuple"
  [[ "${fixture_model}" != "${fixture_conflicting_model}" ]] ||
    fixture_conflicting_model=fixture-model-c
  if [[ "${fresh_policy_install}" == true ]] &&
    "${kube[@]}" get configmap platform-settings-overrides --namespace flux-system \
      >/dev/null 2>&1; then
    fail "fresh provider fixtures require no pre-existing platform-settings-overrides"
  fi
  if [[ "${fresh_policy_install}" == true ]]; then
    ADMISSION_TEST_PLATFORM_SETTINGS_OVERRIDE_OWNED=true
    printf '%s\n' \
      'apiVersion: v1' \
      'kind: ConfigMap' \
      'metadata:' \
      '  name: platform-settings-overrides' \
      '  namespace: flux-system' \
      'data:' \
      '  gcp_project: unchanged-provider' \
      "  llm_model: ${fixture_conflicting_model}" |
      "${kube[@]}" create --filename=- >/dev/null
  fi

  local federation_topology=false
  local -a expected_provider_kustomizations=(
    agentgateway-provider
    agentgateway-admission
    agentgateway-provider-egress
  )
  if yq -e '.data | has("federation_partner_server_name")' \
    <<<"${platform_settings_json}" >/dev/null; then
    federation_topology=true
    expected_provider_kustomizations=(
      agentgateway-provider
      agentgateway-provider-egress
    )
  fi
  local legacy_provider_topology=false
  local provider_kustomization_count=0 provider_kustomization
  local legacy_provider_json expected_legacy_provider_spec live_legacy_provider_spec
  local -a all_provider_kustomizations=(
    agentgateway-provider
    agentgateway-admission
    agentgateway-provider-egress
  )
  for provider_kustomization in "${all_provider_kustomizations[@]}"; do
    if "${kube[@]}" get kustomization --namespace flux-system \
      "${provider_kustomization}" >/dev/null 2>&1; then
      provider_kustomization_count=$((provider_kustomization_count + 1))
      if [[ ! " ${expected_provider_kustomizations[*]} " =~ [[:space:]]${provider_kustomization}[[:space:]] ]]; then
        fail "found unexpected ${provider_kustomization} in the guarded topology"
      fi
    fi
  done
  if [[ "${provider_kustomization_count}" -eq 1 ]]; then
    legacy_provider_json="$("${kube[@]}" get kustomization --namespace flux-system \
      agentgateway-provider -o json 2>/dev/null)" ||
      fail "the one-object legacy topology must contain agentgateway-provider"
    yq -e '
      (!has(.metadata.labels) or
        (.metadata.labels | has("fgentic.dev/llm-provider") | not)) and
      (!has(.metadata.annotations) or
        (.metadata.annotations | has("fgentic.dev/llm-model") | not)) and
      (!has(.spec.postBuild) or
        !has(.spec.postBuild.substitute) or
        ((.spec.postBuild.substitute | has("llm_provider") | not) and
          (.spec.postBuild.substitute | has("llm_model") | not)))
    ' <<<"${legacy_provider_json}" >/dev/null ||
      fail "the one-object provider topology is neither exact legacy nor complete projected state"
    expected_legacy_provider_spec="$(selected_legacy_provider_kustomization_manifest |
      yq -o=json -I=0 '
        del(.spec.force | select(. == false)) |
        (.spec.postBuild.substituteFrom[] | select(.optional == false)) |= del(.optional) |
        sort_keys(..) |
        .spec
      ')"
    live_legacy_provider_spec="$(yq -o=json -I=0 '
      del(.spec.force | select(. == false)) |
      (.spec.postBuild.substituteFrom[] | select(.optional == false)) |= del(.optional) |
      sort_keys(..) |
      .spec
    ' <<<"${legacy_provider_json}")"
    [[ "${live_legacy_provider_spec}" == "${expected_legacy_provider_spec}" ]] ||
      fail "the one-object legacy provider differs from the selected Stage-A contract"
    legacy_provider_topology=true
  elif [[ "${provider_kustomization_count}" -ne 0 &&
    "${provider_kustomization_count}" -ne "${#expected_provider_kustomizations[@]}" ]]; then
    fail "found a partial provider Kustomization installation; refusing to mutate it"
  fi
  if [[ "${provider_kustomization_count}" -eq 0 &&
    "${fresh_policy_install}" != true ]]; then
    fail "pre-installed policies require the complete provider Kustomization topology"
  fi
  if [[ "${provider_kustomization_count}" -eq 0 ]]; then
    if "${kube[@]}" get gitrepository --namespace flux-system flux-system \
      >/dev/null 2>&1 ||
      "${kube[@]}" get deployment --namespace flux-system kustomize-controller \
        >/dev/null 2>&1; then
      fail "fresh provider fixtures require a controller-free disposable context"
    fi
    ADMISSION_TEST_PROVIDER_KUSTOMIZATIONS_OWNED=true
    if [[ "${federation_topology}" == true ]]; then
      provider_kustomizations_manifest |
        yq 'select(.metadata.name == "agentgateway-admission")' |
        "${kube[@]}" apply --filename=- >/dev/null
    fi
  fi

  if [[ "${fresh_policy_install}" == true ]]; then
    ADMISSION_TEST_POLICIES_OWNED=true
    "${kube[@]}" apply --server-side --field-manager=fgentic-admission-test -k "${POLICY_DIR}" >/dev/null
  fi

  local deadline=$((SECONDS + 30))
  local observed_policy_count policy_json warning_count
  while ((SECONDS < deadline)); do
    policy_json="$("${kube[@]}" get validatingadmissionpolicy -o json)"
    observed_policy_count="$(yq -r '
      [.items[] | select(.metadata.name | test("\\.fgentic\\.dev$")) |
        select(.status.observedGeneration == .metadata.generation)] | length
    ' <<<"${policy_json}")"
    if [[ "${observed_policy_count}" -eq 8 ]]; then
      break
    fi
    sleep 1
  done
  [[ "${observed_policy_count}" -eq 8 ]] ||
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

  wait_manifest_denied() {
    local expected="$1"
    local manifest="$2"
    local deadline=$((SECONDS + 30)) output="" status
    while ((SECONDS < deadline)); do
      set +e
      output="$("${kube[@]}" create --dry-run=server --filename=- \
        <<<"${manifest}" 2>&1)"
      status=$?
      set -e
      if [[ "${status}" -ne 0 ]] && grep -Fq "${expected}" <<<"${output}"; then
        return
      fi
      sleep 1
    done
    printf '%s\n' "${output}" >&2
    fail "admission binding was not observed denying: ${expected}"
  }

  expect_apply_manifest_denied() {
    local expected="$1"
    local output status
    set +e
    output="$("${kube[@]}" apply --dry-run=server --filename=- 2>&1)"
    status=$?
    set -e
    [[ "${status}" -ne 0 ]] || fail "admission unexpectedly allowed: ${expected}"
    grep -Fq "${expected}" <<<"${output}" || {
      echo "${output}" >&2
      fail "denial did not include: ${expected}"
    }
  }

  if [[ "${provider_kustomization_count}" -eq 0 ]]; then
    # A pre-existing selection override must block every provider CREATE. Remove only the owned
    # conflicting key, preserve unrelated data, then mirror clean demo/Flux bootstrap with
    # admission active before the exact selected legacy provider is created.
    local legacy_provider_manifest
    legacy_provider_manifest="$(selected_legacy_provider_kustomization_manifest)"
    wait_manifest_denied \
      "remove the llm_model override before projecting the guarded model tuple" \
      "${legacy_provider_manifest}"
    selected_provider_kustomizations_manifest |
      expect_apply_manifest_denied \
        "remove the llm_model override before projecting the guarded model tuple"
    [[ "${ADMISSION_TEST_PLATFORM_SETTINGS_OVERRIDE_OWNED}" == true ]] ||
      fail "refusing to remove a platform-settings override not owned by this test"
    "${kube[@]}" patch configmap platform-settings-overrides --namespace flux-system \
      --type=json --patch='[{"op":"remove","path":"/data/llm_model"}]' >/dev/null
    local preserved_override
    preserved_override="$("${kube[@]}" get configmap platform-settings-overrides \
      --namespace flux-system -o jsonpath='{.data.gcp_project}')"
    [[ "${preserved_override}" == unchanged-provider ]] ||
      fail "guarded override cleanup changed an unrelated provider setting"
    selected_legacy_provider_kustomization_manifest |
      yq '.spec.namePrefix = "redirected-"' |
      expect_manifest_denied \
        "legacy provider bootstrap must match the exact selected Stage-A render"
    selected_legacy_provider_kustomization_manifest |
      yq '.spec.sourceRef.name = "alternate-source"' |
      expect_manifest_denied \
        "provider Kustomizations must retain their exact source and ownership contract"
    selected_legacy_provider_kustomization_manifest |
      "${kube[@]}" apply --filename=- >/dev/null
  fi

  if [[ "${provider_kustomization_count}" -eq 0 ||
    "${legacy_provider_topology}" == true ]]; then
    "${kube[@]}" get kustomization agentgateway-provider --namespace flux-system -o json |
      yq '{
        "apiVersion": .apiVersion,
        "kind": .kind,
        "metadata": {
          "name": .metadata.name,
          "namespace": .metadata.namespace
        },
        "spec": .spec
      }' |
      "${kube[@]}" apply --server-side --field-manager=fgentic-admission-test \
        --dry-run=server --filename=- >/dev/null
    expect_denied "provider Kustomizations must retain the locked selected-provider identity" \
      "${kube[@]}" patch kustomization agentgateway-provider --namespace flux-system \
      --type=merge --dry-run=server \
      --patch '{"spec":{"sourceRef":{"kind":"GitRepository","name":"alternate-source"}}}'
  fi

  if [[ "${provider_kustomization_count}" -eq 0 &&
    "${federation_topology}" == true ]]; then
    "${kube[@]}" delete kustomization agentgateway-admission --namespace flux-system \
      --wait=true >/dev/null
    provider_kustomizations_manifest |
      yq 'select(.metadata.name == "agentgateway-admission")' |
      expect_manifest_denied \
        "federation must not recreate the deleted local admission inventory"
  fi

  if [[ "${provider_kustomization_count}" -eq 0 ]]; then
    expect_denied "provider Kustomizations must retain the locked selected-model identity" \
      "${kube[@]}" patch kustomization --namespace flux-system agentgateway-provider \
      --type=merge --dry-run=server \
      --patch "{\"metadata\":{\"annotations\":{\"fgentic.dev/llm-model\":\"${fixture_conflicting_model}\"},\"labels\":{\"fgentic.dev/llm-provider\":\"${fixture_provider}\"}},\"spec\":{\"postBuild\":{\"substitute\":{\"llm_model\":\"${fixture_conflicting_model}\",\"llm_provider\":\"${fixture_provider}\"}}}}"
    selected_provider_kustomizations_manifest |
      "${kube[@]}" apply --filename=- >/dev/null
  fi

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
      env:
        - name: ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS
          value: "false"
        - name: OTEL_INSTRUMENTATION_GENAI_CAPTURE_MESSAGE_CONTENT
          value: "false"
        - name: TRACELOOP_TRACE_CONTENT
          value: "false"
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

  platform_settings_json="$("${kube[@]}" get configmap platform-settings \
    --namespace flux-system -o json)"

  local locked_provider locked_model frozen_provider alternate_provider frozen_model alternate_model
  locked_provider="$(yq -r '.data.llm_provider // ""' <<<"${platform_settings_json}")"
  locked_model="$(yq -r '.data.llm_model // ""' <<<"${platform_settings_json}")"
  [[ -n "${locked_provider}" && -n "${locked_model}" ]] ||
    fail "platform-settings must carry the complete guarded model tuple"
  frozen_provider="${locked_provider}"
  alternate_provider=openai
  [[ "${frozen_provider}" != "${alternate_provider}" ]] || alternate_provider=demo
  expect_denied "llm_provider is frozen while the model-residency handoff is guarded" \
    "${kube[@]}" patch configmap platform-settings --namespace flux-system \
    --type=merge --dry-run=server \
    --patch "{\"data\":{\"llm_provider\":\"${alternate_provider}\"}}"
  "${kube[@]}" patch configmap platform-settings --namespace flux-system \
    --type=merge --dry-run=server --patch '{"data":{"handoff_probe":"unchanged-provider"}}' \
    >/dev/null
  frozen_model="${locked_model}"
  alternate_model=fixture-model-b
  [[ "${frozen_model}" != "${alternate_model}" ]] || alternate_model=fixture-model-c
  expect_denied "llm_model is frozen while the model-residency handoff is guarded" \
    "${kube[@]}" patch configmap platform-settings --namespace flux-system \
    --type=merge --dry-run=server \
    --patch "{\"data\":{\"llm_model\":\"${alternate_model}\"}}"
  if yq -e 'has("data") and (.data | has("llm_provider"))' <<<"${platform_settings_json}" \
    >/dev/null; then
    expect_denied "llm_provider is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings --namespace flux-system \
      --type=merge --dry-run=server --patch '{"data":{"llm_provider":null}}'
  fi
  if yq -e 'has("data") and (.data | has("llm_model"))' <<<"${platform_settings_json}" \
    >/dev/null; then
    expect_denied "llm_model is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings --namespace flux-system \
      --type=merge --dry-run=server --patch '{"data":{"llm_model":null}}'
  fi
  if yq -e 'has("data") and (.data | has("federation_partner_server_name"))' \
    <<<"${platform_settings_json}" >/dev/null; then
    expect_denied "federation topology is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings --namespace flux-system \
      --type=merge --dry-run=server \
      --patch '{"data":{"federation_partner_server_name":"changed.invalid"}}'
    expect_denied "federation topology is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings --namespace flux-system \
      --type=merge --dry-run=server \
      --patch '{"data":{"federation_partner_server_name":null}}'
  else
    expect_denied "federation topology is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings --namespace flux-system \
      --type=merge --dry-run=server \
      --patch '{"data":{"federation_partner_server_name":"introduced.invalid"}}'
  fi
  expect_denied "cluster issuer is frozen while the model-residency handoff is guarded" \
    "${kube[@]}" patch configmap platform-settings --namespace flux-system \
    --type=merge --dry-run=server \
    --patch '{"data":{"cluster_issuer":"changed"}}'
  expect_denied "cluster issuer is frozen while the model-residency handoff is guarded" \
    "${kube[@]}" patch configmap platform-settings --namespace flux-system \
    --type=merge --dry-run=server --patch '{"data":{"cluster_issuer":null}}'
  if yq -e '
    has("data") and
    ((.data | has("llm_provider")) or
    (.data | has("llm_model")) or
    (.data | has("federation_partner_server_name")) or
    (.data | has("cluster_issuer")))
  ' <<<"${platform_settings_json}" >/dev/null; then
    expect_denied \
      "handoff-bearing platform settings cannot be deleted while the model-residency handoff is guarded" \
      "${kube[@]}" delete configmap platform-settings --namespace flux-system --dry-run=server
  fi

  local provider_override_exists=false
  if "${kube[@]}" get configmap platform-settings-overrides --namespace flux-system \
    >/dev/null 2>&1; then
    provider_override_exists=true
  fi
  if [[ "${provider_override_exists}" == true ]]; then
    local provider_override_json
    provider_override_json="$("${kube[@]}" get configmap platform-settings-overrides \
      --namespace flux-system -o json)"
    if yq -e 'has("data") and (.data | has("llm_provider"))' \
      <<<"${provider_override_json}" >/dev/null; then
      fail "remove llm_provider from platform-settings-overrides before the guarded handoff"
    fi
    if yq -e 'has("data") and (.data | has("llm_model"))' \
      <<<"${provider_override_json}" >/dev/null; then
      fail "remove llm_model from platform-settings-overrides before the guarded handoff"
    fi
    if yq -e 'has("data") and (.data | has("federation_partner_server_name"))' \
      <<<"${provider_override_json}" >/dev/null; then
      fail "platform-settings-overrides cannot carry the guarded federation topology"
    fi
    if yq -e 'has("data") and (.data | has("cluster_issuer"))' \
      <<<"${provider_override_json}" >/dev/null; then
      fail "platform-settings-overrides cannot carry the guarded cluster issuer"
    fi
    frozen_provider="$(yq -r '.data.llm_provider // ""' <<<"${provider_override_json}")"
    alternate_provider=openai
    [[ "${frozen_provider}" != "${alternate_provider}" ]] || alternate_provider=demo
    expect_denied "llm_provider is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings-overrides --namespace flux-system \
      --type=merge --dry-run=server \
      --patch "{\"data\":{\"llm_provider\":\"${alternate_provider}\"}}"
    frozen_model="$(yq -r '.data.llm_model // ""' <<<"${provider_override_json}")"
    alternate_model=fixture-model-b
    [[ "${frozen_model}" != "${alternate_model}" ]] || alternate_model=fixture-model-c
    expect_denied "llm_model is frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch configmap platform-settings-overrides --namespace flux-system \
      --type=merge --dry-run=server \
      --patch "{\"data\":{\"llm_model\":\"${alternate_model}\"}}"
  else
    printf '%s\n' \
      'apiVersion: v1' \
      'kind: ConfigMap' \
      'metadata:' \
      '  name: platform-settings-overrides' \
      '  namespace: flux-system' \
      'data:' \
      '  llm_model: fixture-model-b' |
      expect_manifest_denied \
        "model-selection and topology overrides cannot be introduced while the model-residency handoff is guarded"
    printf '%s\n' \
      'apiVersion: v1' \
      'kind: ConfigMap' \
      'metadata:' \
      '  name: platform-settings-overrides' \
      '  namespace: flux-system' \
      'data:' \
      '  gcp_project: unchanged-provider' |
      "${kube[@]}" create --dry-run=server --filename=- >/dev/null
  fi

  if [[ "${#expected_provider_kustomizations[@]}" -eq 2 ]]; then
    provider_kustomizations_manifest |
      yq 'select(.metadata.name == "agentgateway-admission")' |
      expect_manifest_denied \
        "federation must not recreate the deleted local admission inventory"
  fi

  local -a projected_provider_kustomizations=("${expected_provider_kustomizations[@]}")
  if [[ "${legacy_provider_topology}" == true ]]; then
    projected_provider_kustomizations=()
  fi
  local provider_kustomization provider_kustomization_json provider_label model_annotation
  local provider_path alternate_path inline_provider inline_model
  for provider_kustomization in "${projected_provider_kustomizations[@]}"; do
    provider_kustomization_json="$("${kube[@]}" get kustomization \
      --namespace flux-system "${provider_kustomization}" -o json)"
    provider_label="$(yq -r '.metadata.labels."fgentic.dev/llm-provider"' \
      <<<"${provider_kustomization_json}")"
    [[ -n "${provider_label}" && "${provider_label}" != "null" ]] ||
      fail "${provider_kustomization} has no selected-provider identity"
    [[ "${provider_label}" == "${locked_provider}" ]] ||
      fail "${provider_kustomization} provider identity differs from platform-settings"
    model_annotation="$(yq -r '.metadata.annotations."fgentic.dev/llm-model"' \
      <<<"${provider_kustomization_json}")"
    [[ -n "${model_annotation}" && "${model_annotation}" != "null" ]] ||
      fail "${provider_kustomization} has no selected-model identity"
    [[ "${model_annotation}" == "${locked_model}" ]] ||
      fail "${provider_kustomization} model identity differs from platform-settings"
    inline_provider="$(yq -r '.spec.postBuild.substitute.llm_provider' \
      <<<"${provider_kustomization_json}")"
    [[ "${inline_provider}" == "${provider_label}" ]] ||
      fail "${provider_kustomization} inline provider does not match its identity"
    inline_model="$(yq -r '.spec.postBuild.substitute.llm_model' \
      <<<"${provider_kustomization_json}")"
    [[ "${inline_model}" == "${model_annotation}" ]] ||
      fail "${provider_kustomization} inline model does not match its identity"
    alternate_provider=openai
    [[ "${provider_label}" != "${alternate_provider}" ]] || alternate_provider=demo
    alternate_model=fixture-model-b
    [[ "${model_annotation}" != "${alternate_model}" ]] || alternate_model=fixture-model-c
    provider_path="$(yq -r '.spec.path' <<<"${provider_kustomization_json}")"
    alternate_path=./infra/agentgateway/providers/profiles/openai
    [[ "${provider_path}" != "${alternate_path}" ]] ||
      alternate_path=./infra/agentgateway/providers/profiles/demo

    expect_denied \
      "provider Kustomizations must retain the locked selected-provider identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch "{\"metadata\":{\"labels\":{\"fgentic.dev/llm-provider\":\"${alternate_provider}\"}}}"
    expect_denied \
      "provider Kustomizations must retain the locked selected-provider identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch '{"metadata":{"labels":{"fgentic.dev/llm-provider":null}}}'
    expect_denied \
      "provider Kustomizations must retain the locked selected-model identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch "{\"metadata\":{\"annotations\":{\"fgentic.dev/llm-model\":\"${alternate_model}\"}}}"
    expect_denied \
      "provider Kustomizations must retain the locked selected-model identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch '{"metadata":{"annotations":{"fgentic.dev/llm-model":null}}}'
    expect_denied \
      "provider Kustomization paths are frozen while the model-residency handoff is guarded" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch "{\"spec\":{\"path\":\"${alternate_path}\"}}"
    expect_denied \
      "provider Kustomizations must retain their exact source and ownership contract" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch '{"spec":{"wait":false}}'
    expect_denied \
      "provider Kustomizations must retain their exact source and ownership contract" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch '{"spec":{"sourceRef":{"kind":"GitRepository","name":"alternate-source"}}}'
    expect_denied \
      "provider Kustomizations must retain their exact current-generation dependency chain" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=json --dry-run=server \
      --patch='[{"op":"remove","path":"/spec/dependsOn/0/readyExpr"}]'
    expect_denied \
      "provider Kustomizations cannot redirect their guarded render inputs" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch '{"spec":{"postBuild":{"substituteFrom":[{"kind":"ConfigMap","name":"alternate-settings"}]}}}'
    if yq -e '.spec | has("patches")' <<<"${provider_kustomization_json}" >/dev/null; then
      expect_denied \
        "provider Kustomization patches cannot change during the exact handoff projection" \
        "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
        --type=json --dry-run=server \
        --patch='[{"op":"remove","path":"/spec/patches"}]'
    fi
    expect_denied \
      "provider Kustomizations cannot be deleted while the model-residency handoff is guarded" \
      "${kube[@]}" delete kustomization --namespace flux-system "${provider_kustomization}" \
      --dry-run=server

    expect_denied "inline provider substitution must equal the frozen provider identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch "{\"spec\":{\"postBuild\":{\"substitute\":{\"llm_provider\":\"${alternate_provider}\"}}}}"
    expect_denied "inline provider substitution must equal the frozen provider identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=json --dry-run=server \
      --patch='[{"op":"remove","path":"/spec/postBuild/substitute/llm_provider"}]'
    expect_denied "inline model substitution must equal the frozen model identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=merge --dry-run=server \
      --patch "{\"spec\":{\"postBuild\":{\"substitute\":{\"llm_model\":\"${alternate_model}\"}}}}"
    expect_denied "inline model substitution must equal the frozen model identity" \
      "${kube[@]}" patch kustomization --namespace flux-system "${provider_kustomization}" \
      --type=json --dry-run=server \
      --patch='[{"op":"remove","path":"/spec/postBuild/substitute/llm_model"}]'
  done

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
    yq 'del(.spec.declarative.deployment.env[0])' |
    expect_agent_denied "managed Agents must disable every reviewed GenAI trace-content path"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.deployment.env[0].value = "true"' |
    expect_agent_denied "managed Agents must disable every reviewed GenAI trace-content path"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.deployment.env[2].name = "ADK_CAPTURE_MESSAGE_CONTENT_IN_SPANS"' |
    expect_agent_denied "managed Agents must disable every reviewed GenAI trace-content path"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.deployment.env[0].valueFrom.fieldRef.fieldPath = "metadata.name"' |
    expect_agent_denied "managed Agents must disable every reviewed GenAI trace-content path"
  agent_manifest "${agent_namespace}" |
    yq '.spec.declarative.deployment.env += [{"name": "ESCAPE", "value": "false"}]' |
    expect_agent_denied "managed Agents must disable every reviewed GenAI trace-content path"
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
