#!/usr/bin/env bash
# Validate namespace compute budgets offline, or prove ResourceQuota denial in an isolated kind
# cluster. The runtime path owns a dedicated no-port cluster and never uses the active kubeconfig.
set -euo pipefail

readonly ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly NAMESPACE_DIR="${ROOT_DIR}/infra/namespaces"
readonly NAMESPACE_FILE="${NAMESPACE_DIR}/namespaces.yaml"
readonly NAMESPACE_QUOTA_FILE="${NAMESPACE_DIR}/resource-quotas.yaml"
readonly FEDERATION_NAMESPACE_DIR="${ROOT_DIR}/infra/federation/namespaces"
readonly FEDERATION_NAMESPACE_FILE="${FEDERATION_NAMESPACE_DIR}/namespace.yaml"
readonly FEDERATION_QUOTA_FILE="${FEDERATION_NAMESPACE_DIR}/resource-quotas.yaml"
readonly ACTIVITYPUB_NAMESPACE_DIR="${ROOT_DIR}/apps/activitypub-agent-gateway/deploy"
readonly ACTIVITYPUB_NAMESPACE_FILE="${ACTIVITYPUB_NAMESPACE_DIR}/namespace.yaml"
readonly ACTIVITYPUB_QUOTA_FILE="${ACTIVITYPUB_NAMESPACE_DIR}/resource-quotas.yaml"
readonly ADMIN_NAMESPACE_FILE="${ROOT_DIR}/infra/admin/base/namespace.yaml"
readonly ADMIN_QUOTA_FILE="${ROOT_DIR}/infra/admin/base/resource-quotas.yaml"
readonly FLUX_FILE="${ROOT_DIR}/clusters/base/infrastructure.yaml"
readonly FLUX_BUILD_FIXTURE="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
readonly KIND_CONFIG="${ROOT_DIR}/scripts/testdata/resource-quota-kind.yaml"
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
readonly PAUSE_IMAGE="registry.k8s.io/pause@sha256:7031c1b283388d2c2b555df8906cc39a3fcec4ee08d94a6af11c0cfe7e99e7f5"
runtime=false
# EXIT traps run after function-local variables leave scope. Keep runtime ownership at script scope
# so every terminal path can identify exactly which disposable cluster and kubeconfig it owns.
runtime_cluster_name=""
runtime_kubeconfig=""
runtime_cluster_created=false

if [[ "${1:-}" == "--runtime" ]]; then
  runtime=true
elif [[ "$#" -ne 0 ]]; then
  echo "usage: $0 [--runtime]" >&2
  exit 2
fi

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

load_profile_settings() {
  local profile="$1"
  local settings="${ROOT_DIR}/clusters/${profile}/platform-settings.yaml"
  local key value
  while IFS='=' read -r key value; do
    export "${key}=${value}"
  done < <(yq -r '.data | to_entries[] | .key + "=" + .value' "${settings}")
}

substitute_profile() {
  local profile="$1"
  (
    load_profile_settings "${profile}"
    if [[ -n "${QUOTA_CORE_PODS_OVERRIDE:-}" ]]; then
      export quota_core_pods="${QUOTA_CORE_PODS_OVERRIDE}"
    fi
    flux envsubst --strict
  )
}

render_profile() {
  local profile="$1"
  (
    {
      kubectl kustomize "${NAMESPACE_DIR}"
      if [[ "${profile}" == federation ]]; then
        printf '\n---\n'
        cat "${FEDERATION_NAMESPACE_FILE}"
        printf '\n---\n'
        cat "${FEDERATION_QUOTA_FILE}"
      fi
    } | substitute_profile "${profile}"
  )
}

render_kustomization_profile() {
  local profile="$1"
  local path="$2"
  kubectl kustomize "${path}" | substitute_profile "${profile}"
}

sorted_names() {
  local kind="$1"
  yq eval-all -o=json "[select(.kind == \"${kind}\")]" |
    yq -r '.[] | .metadata.namespace // .metadata.name' | sort
}

sorted_managed_namespaces() {
  yq eval-all -o=json \
    '[select(.kind == "Namespace" and .metadata.labels."fgentic.dev/managed" == "true")]' |
    yq -r '.[].metadata.name' | sort
}

assert_admission_shape() {
  local rendered="$1"
  local context="$2"
  local invalid_quota invalid_limit
  invalid_quota="$(
    yq eval-all -o=json '[select(.kind == "ResourceQuota")]' <<<"${rendered}" |
      yq -r '.[] | select(
        .metadata.name != "compute-budget" or
        (.spec.hard | length) != 5 or
        (.spec.hard | has("limits.cpu") | not) or
        (.spec.hard | has("limits.memory") | not) or
        (.spec.hard | has("pods") | not) or
        (.spec.hard | has("requests.cpu") | not) or
        (.spec.hard | has("requests.memory") | not) or
        ([.spec.hard[] | tostring | length > 0] | all) != true
      ) |
      .metadata.namespace
    '
  )"
  [[ -z "${invalid_quota}" ]] || fail "${context} has an invalid ResourceQuota: ${invalid_quota}"

  invalid_limit="$(
    yq eval-all -o=json '[select(.kind == "LimitRange")]' <<<"${rendered}" |
      yq -r '.[] | select(
        .metadata.name != "container-defaults" or
        (.spec.limits | length) != 1 or
        .spec.limits[0].type != "Container" or
        (.spec.limits[0].default | length) != 2 or
        (.spec.limits[0].default | has("cpu") | not) or
        (.spec.limits[0].default | has("memory") | not) or
        (.spec.limits[0].defaultRequest | length) != 2 or
        (.spec.limits[0].defaultRequest | has("cpu") | not) or
        (.spec.limits[0].defaultRequest | has("memory") | not) or
        ([.spec.limits[0].default[], .spec.limits[0].defaultRequest[] |
          tostring | length > 0] | all) != true
      ) |
      .metadata.namespace
    '
  )"
  [[ -z "${invalid_limit}" ]] || fail "${context} has an invalid LimitRange: ${invalid_limit}"
}

assert_profile_values() {
  local rendered="$1"
  local settings="$2"
  local context="$3"
  local namespace quota_profile resource suffix setting_key actual expected field field_path
  while IFS=$'\t' read -r namespace quota_profile; do
    for resource in pods requests.cpu requests.memory limits.cpu limits.memory; do
      case "${resource}" in
        pods) suffix=pods ;;
        requests.cpu) suffix=requests_cpu ;;
        requests.memory) suffix=requests_memory ;;
        limits.cpu) suffix=limits_cpu ;;
        limits.memory) suffix=limits_memory ;;
      esac
      setting_key="quota_${quota_profile}_${suffix}"
      actual="$(
        NAMESPACE="${namespace}" RESOURCE="${resource}" yq -rN '
          select(.kind == "ResourceQuota" and
            .metadata.namespace == strenv(NAMESPACE)) |
          .spec.hard[strenv(RESOURCE)]
        ' <<<"${rendered}"
      )"
      expected="$(SETTING_KEY="${setting_key}" yq -r '.data[strenv(SETTING_KEY)]' "${settings}")"
      [[ "${actual}" == "${expected}" ]] ||
        fail "${context} ${namespace} ${resource} does not match ${setting_key}"
    done

    for field in default.cpu default.memory defaultRequest.cpu defaultRequest.memory; do
      case "${field}" in
        default.cpu)
          setting_key=quota_default_limit_cpu
          field_path='.spec.limits[0].default.cpu'
          ;;
        default.memory)
          setting_key=quota_default_limit_memory
          field_path='.spec.limits[0].default.memory'
          ;;
        defaultRequest.cpu)
          setting_key=quota_default_request_cpu
          field_path='.spec.limits[0].defaultRequest.cpu'
          ;;
        defaultRequest.memory)
          setting_key=quota_default_request_memory
          field_path='.spec.limits[0].defaultRequest.memory'
          ;;
      esac
      actual="$(
        NAMESPACE="${namespace}" yq -rN '
          select(.kind == "LimitRange" and
            .metadata.namespace == strenv(NAMESPACE)) |
        '"${field_path}" <<<"${rendered}"
      )"
      expected="$(SETTING_KEY="${setting_key}" yq -r '.data[strenv(SETTING_KEY)]' "${settings}")"
      [[ "${actual}" == "${expected}" ]] ||
        fail "${context} ${namespace} ${field} does not match ${setting_key}"
    done
  done < <(
    yq eval-all -o=json '[select(.kind == "Namespace")]' <<<"${rendered}" |
      yq -r '.[] | [.metadata.name, .metadata.labels."fgentic.dev/quota-profile"] | @tsv'
  )
}

assert_static_contract() {
  require_commands flux kubectl sort yq

  local expected_namespaces
  expected_namespaces="$(
    yq eval-all -o=json '[select(.kind == "Namespace")]' "${NAMESPACE_FILE}" |
      yq -r '.[].metadata.name' | sort
  )"
  [[ "$(wc -l <<<"${expected_namespaces}" | tr -d ' ')" -eq 14 ]] ||
    fail "expected all fourteen shared namespaces to be quota-managed"

  yq -e '
    select(.kind == "Namespace" and .metadata.name == "knowledge") |
    select(.metadata.labels."fgentic.dev/managed" == "true") |
    select(.metadata.labels."fgentic.dev/image-policy" == "enforce") |
    select(.metadata.labels."fgentic.dev/quota-profile" == "small") |
    select(.metadata.labels."pod-security.kubernetes.io/enforce" == "restricted") |
    select(.metadata.labels."pod-security.kubernetes.io/audit" == "restricted") |
    select(.metadata.labels."pod-security.kubernetes.io/warn" == "restricted")
  ' "${NAMESPACE_FILE}" >/dev/null ||
    fail "knowledge must be a managed, image-enforced, small, restricted-PSS namespace"

  local federation_namespaces repository_namespaces repository_quota_namespaces
  local repository_limit_namespaces
  federation_namespaces="$(
    yq eval-all -o=json '[select(.kind == "Namespace")]' \
      "${NAMESPACE_FILE}" "${FEDERATION_NAMESPACE_FILE}" |
      yq -r '.[].metadata.name' | sort
  )"
  [[ "$(wc -l <<<"${federation_namespaces}" | tr -d ' ')" -eq 16 ]] ||
    fail "expected the shared and federation namespace sources to own sixteen namespaces"
  repository_namespaces="$(
    yq eval-all -o=json '[select(.kind == "Namespace")]' \
      "${NAMESPACE_FILE}" "${FEDERATION_NAMESPACE_FILE}" "${ACTIVITYPUB_NAMESPACE_FILE}" \
      "${ADMIN_NAMESPACE_FILE}" |
      yq -r '.[].metadata.name' | sort
  )"
  [[ "$(wc -l <<<"${repository_namespaces}" | tr -d ' ')" -eq 18 ]] ||
    fail "expected all eighteen repository-owned namespaces to be quota-managed"
  repository_quota_namespaces="$(
    yq eval-all -o=json \
      '[select(.kind == "ResourceQuota" and .metadata.name == "compute-budget")]' \
      "${NAMESPACE_QUOTA_FILE}" "${FEDERATION_QUOTA_FILE}" "${ACTIVITYPUB_QUOTA_FILE}" \
      "${ADMIN_QUOTA_FILE}" |
      yq -r '.[].metadata.namespace' | sort
  )"
  repository_limit_namespaces="$(
    yq eval-all -o=json \
      '[select(.kind == "LimitRange" and .metadata.name == "container-defaults")]' \
      "${NAMESPACE_QUOTA_FILE}" "${FEDERATION_QUOTA_FILE}" "${ACTIVITYPUB_QUOTA_FILE}" \
      "${ADMIN_QUOTA_FILE}" |
      yq -r '.[].metadata.namespace' | sort
  )"
  [[ "${repository_quota_namespaces}" == "${repository_namespaces}" ]] ||
    fail "repository-owned ResourceQuota namespace set drifted"
  [[ "${repository_limit_namespaces}" == "${repository_namespaces}" ]] ||
    fail "repository-owned LimitRange namespace set drifted"

  local profiles
  profiles="$(
    yq eval-all -o=json '
      [select(.kind == "Namespace") |
        .metadata.labels."fgentic.dev/quota-profile"]
    ' "${NAMESPACE_FILE}" "${FEDERATION_NAMESPACE_FILE}" "${ACTIVITYPUB_NAMESPACE_FILE}" \
      "${ADMIN_NAMESPACE_FILE}"
  )"
  yq -e '
    select(length == 18) |
    [.[] | test("^(small|core|compute)$")] |
    select(all)
  ' <<<"${profiles}" >/dev/null || fail "every platform Namespace needs a known quota profile"

  local layers
  layers="$(
    yq eval-all -o=json '
      [select(.kind == "Kustomization" and .metadata.name == "namespaces")]
    ' "${FLUX_FILE}"
  )"
  yq -e '
    select(length == 1) |
    .[0].spec.postBuild.substituteFrom |
    select(length == 2) |
    select(.[0].kind == "ConfigMap") |
    select(.[0].name == "platform-settings") |
    select(.[1].kind == "ConfigMap") |
    select(.[1].name == "platform-settings-overrides") |
    select(.[1].optional == true)
  ' <<<"${layers}" >/dev/null || fail "the namespaces layer must substitute platform settings"

  local profile rendered quota_namespaces limit_namespaces activitypub_rendered
  local expected_profile_namespaces
  for profile in local gcp demo federation; do
    local settings="${ROOT_DIR}/clusters/${profile}/platform-settings.yaml"
    expected_profile_namespaces="${expected_namespaces}"
    if [[ "${profile}" == federation ]]; then
      expected_profile_namespaces="${federation_namespaces}"
    fi
    rendered="$(render_profile "${profile}")"
    [[ "${rendered}" != *'${'* ]] || fail "clusters/${profile} left quota variables unresolved"
    quota_namespaces="$(sorted_names ResourceQuota <<<"${rendered}")"
    limit_namespaces="$(sorted_names LimitRange <<<"${rendered}")"
    [[ "${quota_namespaces}" == "${expected_profile_namespaces}" ]] ||
      fail "clusters/${profile} ResourceQuota namespace set drifted"
    [[ "${limit_namespaces}" == "${expected_profile_namespaces}" ]] ||
      fail "clusters/${profile} LimitRange namespace set drifted"

    assert_admission_shape "${rendered}" "clusters/${profile}"
    assert_profile_values "${rendered}" "${settings}" "clusters/${profile}"

    activitypub_rendered="$(render_kustomization_profile "${profile}" "${ACTIVITYPUB_NAMESPACE_DIR}")"
    [[ "${activitypub_rendered}" != *'${'* ]] ||
      fail "activitypub deploy left clusters/${profile} variables unresolved"
    [[ "$(sorted_names Namespace <<<"${activitypub_rendered}")" == activitypub ]] ||
      fail "activitypub deploy Namespace set drifted"
    [[ "$(sorted_names ResourceQuota <<<"${activitypub_rendered}")" == activitypub ]] ||
      fail "activitypub deploy ResourceQuota set drifted"
    [[ "$(sorted_names LimitRange <<<"${activitypub_rendered}")" == activitypub ]] ||
      fail "activitypub deploy LimitRange set drifted"
    assert_admission_shape "${activitypub_rendered}" "activitypub deploy (${profile})"
    assert_profile_values "${activitypub_rendered}" "${settings}" "activitypub deploy (${profile})"
  done

  local federation_rendered effective_namespaces effective_quota_namespaces effective_limit_namespaces
  federation_rendered="$(
    cd "${ROOT_DIR}"
    flux build kustomization cluster-overlay-validation \
      --path clusters/federation \
      --kustomization-file "${FLUX_BUILD_FIXTURE}" \
      --dry-run \
      --in-memory-build \
      --strict-substitute \
      --recursive \
      --local-sources GitRepository/flux-system/flux-system=.
  )"
  effective_namespaces="$(
    sorted_managed_namespaces <<<"${federation_rendered}"
  )"
  [[ "$(wc -l <<<"${effective_namespaces}" | tr -d ' ')" -eq 12 ]] ||
    fail "the effective federation overlay must own exactly twelve namespaces"
  effective_quota_namespaces="$(
    sorted_names ResourceQuota <<<"${federation_rendered}"
  )"
  effective_limit_namespaces="$(
    sorted_names LimitRange <<<"${federation_rendered}"
  )"
  [[ "${effective_quota_namespaces}" == "${effective_namespaces}" ]] ||
    fail "effective federation ResourceQuota namespace set drifted"
  [[ "${effective_limit_namespaces}" == "${effective_namespaces}" ]] ||
    fail "effective federation LimitRange namespace set drifted"
  local federation_admission_rendered
  federation_admission_rendered="$(
    yq 'select(
      (.kind == "Namespace" and .metadata.labels."fgentic.dev/managed" == "true") or
      (.kind == "ResourceQuota" and .metadata.name == "compute-budget") or
      (.kind == "LimitRange" and .metadata.name == "container-defaults")
    )' <<<"${federation_rendered}" | substitute_profile federation
  )"
  [[ "${federation_admission_rendered}" != *'${'* ]] ||
    fail "the effective federation admission layer left quota variables unresolved"
  assert_admission_shape "${federation_admission_rendered}" "effective federation overlay"
  assert_profile_values \
    "${federation_admission_rendered}" \
    "${ROOT_DIR}/clusters/federation/platform-settings.yaml" \
    "effective federation overlay"

  local cert_releases
  cert_releases="$(
    yq eval-all -o=json '
      [select(.kind == "HelmRelease" and .metadata.name == "cert-manager")]
    ' "${ROOT_DIR}/infra/flux/releases.yaml"
  )"
  [[ "$(yq -r 'length' <<<"${cert_releases}")" -eq 1 ]] ||
    fail "expected exactly one cert-manager HelmRelease"
  local resource_path
  for resource_path in \
    spec.values.resources \
    spec.values.webhook.resources \
    spec.values.cainjector.resources \
    spec.values.startupapicheck.resources; do
    yq -e ".[0].${resource_path} |
      select(.requests.cpu != null) |
      select(.requests.memory != null) |
      select(.limits.cpu != null) |
      select(.limits.memory != null)" <<<"${cert_releases}" >/dev/null ||
      fail "cert-manager ${resource_path} needs explicit requests and limits"
  done
  yq -e '
    select(.kind == "HelmRelease" and .metadata.name == "agentgateway") |
    .spec.values.resources |
    select(.requests.cpu != null) |
    select(.requests.memory != null) |
    select(.limits.cpu != null) |
    select(.limits.memory != null)
  ' "${ROOT_DIR}/infra/agentgateway/base/helmrelease.yaml" >/dev/null ||
    fail "agentgateway control-plane resources are incomplete"

  echo "ResourceQuota static contract passed"
}

assert_runtime_contract() {
  require_commands docker kind kubectl yq
  docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

  runtime_cluster_name="${KIND_CLUSTER_NAME:-fgentic-resource-quota-${RANDOM}-$$}"
  runtime_kubeconfig="$(mktemp -t fgentic-resource-quota.XXXXXX)"
  runtime_cluster_created=false

  if kind get clusters | grep -Fxq "${runtime_cluster_name}"; then
    rm -f "${runtime_kubeconfig}"
    fail "kind cluster ${runtime_cluster_name} already exists; refusing to mutate or delete it"
  fi

  cleanup() {
    local result=$?
    trap - EXIT INT TERM
    if [[ "${runtime_cluster_created}" == true ]]; then
      if kind delete cluster --name "${runtime_cluster_name}" >/dev/null 2>&1 &&
        ! kind get clusters 2>/dev/null | grep -Fxq "${runtime_cluster_name}"; then
        echo "==> Deleted isolated quota cluster ${runtime_cluster_name}"
      else
        echo "warning: failed to delete owned kind cluster ${runtime_cluster_name}" >&2
        result=1
      fi
    elif kind get clusters 2>/dev/null | grep -Fxq "${runtime_cluster_name}"; then
      echo "warning: kind partially created ${runtime_cluster_name}; ownership is ambiguous, leaving it for review" >&2
    fi
    rm -f "${runtime_kubeconfig}"
    exit "${result}"
  }
  trap cleanup EXIT
  trap 'exit 130' INT TERM

  export KUBECONFIG="${runtime_kubeconfig}"
  echo "==> Creating isolated quota cluster ${runtime_cluster_name}"
  kind create cluster \
    --name "${runtime_cluster_name}" \
    --image "${KIND_NODE_IMAGE}" \
    --config "${KIND_CONFIG}" \
    --kubeconfig "${runtime_kubeconfig}" \
    --wait 180s
  # Claim cleanup ownership only after kind reports successful creation. A same-name race must
  # never let this process delete another process's cluster.
  runtime_cluster_created=true
  kubectl wait --for=condition=Ready nodes --all --timeout=120s >/dev/null

  echo "==> Applying the real kagent quota with a two-Pod test ceiling"
  QUOTA_CORE_PODS_OVERRIDE=2 render_profile local |
    yq 'select(
      (.kind == "Namespace" and .metadata.name == "kagent") or
      ((.kind == "ResourceQuota" or .kind == "LimitRange") and
        .metadata.namespace == "kagent")
    )' |
    kubectl apply --filename - >/dev/null
  # Do not race the namespace controller's asynchronous default ServiceAccount creation.
  kubectl --namespace kagent create serviceaccount quota-test >/dev/null

  local defaulted
  defaulted="$(
    kubectl --namespace kagent run quota-default-probe \
      --image "${PAUSE_IMAGE}" \
      --restart Never \
      --dry-run=client \
      --output yaml |
      yq '
        .spec.securityContext = {
          "runAsNonRoot": true,
          "seccompProfile": {"type": "RuntimeDefault"}
        } |
        .spec.serviceAccountName = "quota-test" |
        .spec.containers[0].securityContext = {
          "allowPrivilegeEscalation": false,
          "capabilities": {"drop": ["ALL"]}
        }
      ' |
      kubectl create --dry-run=server --filename - --output json
  )"
  yq -e '
    .spec.containers[0].resources |
    select(.limits.cpu == "500m") |
    select(.limits.memory == "512Mi") |
    select(.requests.cpu == "25m") |
    select(.requests.memory == "64Mi")
  ' <<<"${defaulted}" >/dev/null || fail "LimitRange did not default the omitted resources"

  kubectl --namespace kagent create deployment quota-agent \
    --image "${PAUSE_IMAGE}" \
    --replicas 1 \
    --dry-run=client \
    --output yaml |
    yq '
      .spec.template.spec.containers[0].resources = {
        "requests": {"cpu": "10m", "memory": "8Mi"},
        "limits": {"cpu": "20m", "memory": "16Mi"}
      } |
      .spec.template.spec.securityContext = {
        "runAsNonRoot": true,
        "seccompProfile": {"type": "RuntimeDefault"}
      } |
      .spec.template.spec.serviceAccountName = "quota-test" |
      .spec.template.spec.containers[0].securityContext = {
        "allowPrivilegeEscalation": false,
        "capabilities": {"drop": ["ALL"]}
      }
    ' |
    kubectl apply --filename - >/dev/null

  local deadline=$((SECONDS + 30)) pod_count=0
  while ((SECONDS < deadline)); do
    pod_count="$(
      kubectl --namespace kagent get pods \
        --selector app=quota-agent \
        --output json | yq -r '.items | length'
    )"
    [[ "${pod_count}" -eq 1 ]] && break
    sleep 1
  done
  [[ "${pod_count}" -eq 1 ]] || fail "the baseline agent Deployment did not create one Pod"

  echo "==> Scaling the agent Deployment beyond quota"
  kubectl --namespace kagent scale deployment quota-agent --replicas 3 >/dev/null

  local failure_event=""
  deadline=$((SECONDS + 30))
  while ((SECONDS < deadline)); do
    failure_event="$(
      kubectl --namespace kagent get events \
        --field-selector reason=FailedCreate \
        --output json |
        yq -r '[.items[].message |
          select(contains("exceeded quota: compute-budget")) |
          select(contains("pods"))][0] // ""'
    )"
    [[ -n "${failure_event}" ]] && break
    sleep 1
  done
  [[ -n "${failure_event}" ]] || fail "scaled Deployment did not emit the expected FailedCreate"

  deadline=$((SECONDS + 30))
  local used_pods=""
  while ((SECONDS < deadline)); do
    used_pods="$(
      kubectl --namespace kagent get resourcequota compute-budget \
        --output jsonpath='{.status.used.pods}'
    )"
    [[ "${used_pods}" == "2" ]] && break
    sleep 1
  done
  [[ "${used_pods}" == "2" ]] || fail "quota usage did not stop at two Pods"

  echo "ResourceQuota runtime contract passed: ${failure_event}"
}

assert_static_contract
if [[ "${runtime}" == true ]]; then
  assert_runtime_contract
fi
