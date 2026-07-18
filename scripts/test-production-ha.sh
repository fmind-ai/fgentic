#!/usr/bin/env bash
# Render the opt-in production profile through Flux, then prove that every disruption budget
# protects a replicated workload and that evaluation profiles retain their one-replica posture.
# shellcheck disable=SC2016 # jq/yq bindings in rendered-manifest assertions are intentionally literal
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly FIXTURE="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
WORK_DIR="$(mktemp -d -t fgentic-production-ha.XXXXXX)"
readonly WORK_DIR
readonly GCP_RENDER="${WORK_DIR}/gcp.yaml"
readonly LOCAL_RENDER="${WORK_DIR}/local.yaml"

cleanup() {
	rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

fail() {
	echo "Error: $*" >&2
	exit 1
}

assert_yq() {
	local expression=$1
	local file=$2
	local message=$3

	yq -e "${expression}" "${file}" >/dev/null || fail "${message}"
}

assert_yq_all() {
	local expression=$1
	local file=$2
	local message=$3

	# eval-all lets inventory assertions aggregate a multi-document Kubernetes render instead of
	# accidentally evaluating one singleton array per YAML document.
	yq eval-all -e "${expression}" "${file}" >/dev/null || fail "${message}"
}

assert_yq_all_files() {
	local expression=$1
	local message=$2
	shift 2

	yq eval-all -e "${expression}" "$@" >/dev/null || fail "${message}"
}

assert_bounded_workloads() {
	local file=$1
	local description=$2

	assert_yq_all '
    [select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "DaemonSet")] as $workloads |
    ($workloads | length) > 0 and
    ([
      $workloads[] |
      (.spec.template.spec.containers[]?, .spec.template.spec.initContainers[]?) |
      select(
        .resources.requests.cpu == null or .resources.requests.memory == null or
        .resources.limits.cpu == null or .resources.limits.memory == null
      )
    ] | length) == 0
  ' "${file}" "${description} contains an unbounded container or init container"
}

assert_pdbs_target_replicated_workloads() {
	local file=$1
	local expected=$2
	local description=$3

	yq eval-all -e "
    [select(.kind == \"Deployment\" or .kind == \"StatefulSet\")] as \$workloads |
    [select(.kind == \"PodDisruptionBudget\")] as \$pdbs |
    [
      ((\$pdbs | length) == ${expected}),
      (\$pdbs | all_c(. as \$pdb | [
        (((\$pdb.spec.selector.matchExpressions // []) | length) == 0),
        (((\$pdb.spec.selector.matchLabels // {}) | length) > 0),
        (([
          \$workloads[] |
          select(.metadata.namespace == \$pdb.metadata.namespace) |
          select((.spec.replicas // 1) == 2) |
          select(.spec.template.metadata.labels as \$labels |
            \$labels | contains(\$pdb.spec.selector.matchLabels // {}))
        ] | length) == 1)
      ] | all_c(.)))
    ] | all_c(.)
  " "${file}" >/dev/null || fail "${description}"
}

flux_render() {
	local environment=$1
	local output=$2

	flux build kustomization cluster-overlay-validation \
		--path "${ROOT_DIR}/clusters/${environment}" \
		--kustomization-file "${FIXTURE}" \
		--dry-run \
		--in-memory-build \
		--strict-substitute \
		--recursive \
		--local-sources "GitRepository/flux-system/flux-system=${ROOT_DIR}" \
		>"${output}"
}

render_release() {
	local release=$1
	local namespace=$2
	local chart=$3
	local version=$4
	local output=$5
	local helm_release
	shift 5

	helm_release="$(yq -er "select(.kind == \"HelmRelease\" and .metadata.name ==
    \"${release}\" and .metadata.namespace == \"${namespace}\") |
    .spec.releaseName // .metadata.name" "${GCP_RENDER}")"

	yq -e "select(.kind == \"HelmRelease\" and .metadata.name == \"${release}\" and
    .metadata.namespace == \"${namespace}\") | .spec.values" "${GCP_RENDER}" \
		| helm template "${helm_release}" "${chart}" \
			--version "${version}" \
			--namespace "${namespace}" \
			--values - \
			"$@" \
		| sed -e '/^Pulled: /d' -e '/^Digest: /d' \
			>"${output}"
}

echo "==> Rendering production and evaluation profiles"
flux_render gcp "${GCP_RENDER}"
flux_render local "${LOCAL_RENDER}"

echo "==> Checking the production Flux contract"
assert_yq 'select(.kind == "VolumeSnapshotClass" and
  .metadata.name == "fgentic-synapse-media") |
  (.driver == "pd.csi.storage.gke.io" and .deletionPolicy == "Retain")' \
	"${GCP_RENDER}" "the GKE reference must retain PD CSI media snapshots"
assert_yq 'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack" and
  .metadata.namespace == "matrix") | .spec.values.synapse.media.storage |
  (.size == "10Gi" and .storageClassName == "standard-rwo" and .resourcePolicy == "keep")' \
	"${GCP_RENDER}" "production Synapse media must use the retained snapshot-capable PVC"
assert_yq 'select(.kind == "Cluster" and .metadata.name == "platform-pg" and
  .metadata.namespace == "postgres") | (.spec.instances == 3 and
  .spec.resources.requests.cpu != null and .spec.resources.requests.memory != null and
  .spec.resources.limits.cpu != null and .spec.resources.limits.memory != null)' \
	"${GCP_RENDER}" "production CNPG must have three resource-bounded instances"
assert_yq 'select(.kind == "AgentgatewayParameters" and .metadata.name == "secured") |
  (.spec.deployment.spec.replicas == 2 and .spec.podDisruptionBudget.spec.maxUnavailable == 1 and
  .spec.deployment.spec.strategy.type == "RollingUpdate" and
  .spec.deployment.spec.strategy.rollingUpdate.maxSurge == 0 and
  .spec.deployment.spec.strategy.rollingUpdate.maxUnavailable == 1 and
  ([.spec.deployment.spec.template.spec.affinity.podAntiAffinity
    .requiredDuringSchedulingIgnoredDuringExecution[] | select(
      .topologyKey == "kubernetes.io/hostname" and
      .labelSelector.matchLabels."app.kubernetes.io/name" == "agentgateway-proxy"
    )] | length) == 1 and
  .spec.resources.requests.cpu != null and .spec.resources.requests.memory != null and
  .spec.resources.limits.cpu != null and .spec.resources.limits.memory != null)' \
	"${GCP_RENDER}" "agentgateway data plane must have two bounded replicas and a one-pod PDB"
assert_yq 'select(.kind == "Deployment" and .metadata.name == "mcp-tool-rate-limit" and
  .metadata.namespace == "agentgateway-system") |
  .spec.template.metadata.labels as $labels |
  (.spec.replicas == 2 and
  .spec.strategy.type == "RollingUpdate" and
  .spec.strategy.rollingUpdate.maxSurge == 0 and
  .spec.strategy.rollingUpdate.maxUnavailable == 1 and
  ([.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[] |
    select(.topologyKey == "kubernetes.io/hostname" and
      (.labelSelector.matchLabels as $selector |
        (($selector | length) > 0 and ($labels | contains($selector)))))] | length) == 1)' \
	"${GCP_RENDER}" "production MCP rate-limit service must be replicated across hosts"
assert_yq 'select(.kind == "PodDisruptionBudget" and .metadata.name == "mcp-tool-rate-limit" and
  .metadata.namespace == "agentgateway-system") |
  (.spec.maxUnavailable == 1 and
  .spec.selector.matchLabels."app.kubernetes.io/name" == "mcp-tool-rate-limit")' \
	"${GCP_RENDER}" "production MCP rate-limit service PDB is missing"
assert_yq 'select(.kind == "Deployment" and .metadata.name == "mcp-tool-rate-limit-redis" and
  .metadata.namespace == "agentgateway-system") |
  (.spec.replicas == 1 and .spec.strategy.type == "Recreate" and
  .spec.template.spec.volumes[0].persistentVolumeClaim.claimName ==
    "mcp-tool-rate-limit-redis")' \
	"${GCP_RENDER}" "the persistent MCP quota store must retain its explicit fast-restart posture"
assert_yq_all '[select(.kind == "PodDisruptionBudget" and
  .metadata.name == "mcp-tool-rate-limit-redis" and
  .metadata.namespace == "agentgateway-system")] | length == 0' \
	"${GCP_RENDER}" "the single RWO MCP quota store must not block voluntary drains"
assert_yq_all '[select(.kind == "PodDisruptionBudget" and
  (.metadata.namespace == "matrix" or .metadata.namespace == "kagent"))] as $pdbs |
  ($pdbs | length) == 7 and
  ([$pdbs[] | select(.spec.maxUnavailable != 1 or .spec.minAvailable != null)] | length) == 0' \
	"${GCP_RENDER}" "production Matrix/kagent PDB inventory must be exact and drain-safe"
assert_yq_all '[select(.kind == "PodDisruptionBudget" and .metadata.name == "ess-synapse-main")] |
  length == 0' "${GCP_RENDER}" "single-replica Synapse must not block voluntary drains"
assert_yq_all '[select(.kind == "PodDisruptionBudget" and .metadata.namespace == "bridge")] |
  length == 0' "${GCP_RENDER}" "the single-consumer bridge must not have a blocking PDB"
assert_yq_all '[select(.kind == "Agent")] as $agents |
  ($agents | length) == 3 and
  ([$agents[] | select(
    (.spec.declarative.deployment.replicas // 1) != 1 or
    .spec.declarative.deployment.resources.requests.cpu == null or
    .spec.declarative.deployment.resources.requests.memory == null or
    .spec.declarative.deployment.resources.limits.cpu == null or
    .spec.declarative.deployment.resources.limits.memory == null
  )] | length) == 0' "${GCP_RENDER}" \
	"generated Agents must remain one bounded replica until task affinity is proven"

echo "==> Rendering pinned production charts"
TRAEFIK_VERSION="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "traefik") |
  .spec.chart.spec.version' "${GCP_RENDER}")"
readonly TRAEFIK_VERSION
ESS_VERSION="$(yq -er 'select(.kind == "OCIRepository" and
  .metadata.name == "ess-matrix-stack") | .spec.ref.tag' "${GCP_RENDER}")"
readonly ESS_VERSION
AGENTGATEWAY_VERSION="$(yq -er 'select(.kind == "OCIRepository" and
  .metadata.name == "agentgateway") | .spec.ref.tag' "${GCP_RENDER}")"
readonly AGENTGATEWAY_VERSION
KAGENT_VERSION="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "kagent") |
  .spec.chart.spec.version' "${GCP_RENDER}")"
readonly KAGENT_VERSION
KEYCLOAK_VERSION="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "keycloak") |
  .spec.chart.spec.version' "${GCP_RENDER}")"
readonly KEYCLOAK_VERSION

render_release traefik gateway traefik "${TRAEFIK_VERSION}" "${WORK_DIR}/traefik.yaml" \
	--repo https://traefik.github.io/charts
render_release matrix-stack matrix oci://ghcr.io/element-hq/ess-helm/matrix-stack \
	"${ESS_VERSION}" "${WORK_DIR}/matrix.yaml"
render_release agentgateway agentgateway-system oci://cr.agentgateway.dev/charts/agentgateway \
	"${AGENTGATEWAY_VERSION}" "${WORK_DIR}/agentgateway.yaml"
render_release kagent kagent oci://ghcr.io/kagent-dev/kagent/helm/kagent \
	"${KAGENT_VERSION}" "${WORK_DIR}/kagent-raw.yaml"
render_release keycloak keycloak keycloakx "${KEYCLOAK_VERSION}" "${WORK_DIR}/keycloak.yaml" \
	--repo https://codecentric.github.io/helm-charts
helm template matrix-a2a-bridge "${ROOT_DIR}/apps/matrix-a2a-bridge/chart" \
	>"${WORK_DIR}/bridge.yaml"

# Apply every exact Flux Helm post-renderer to the pinned chart render. This keeps catalog and
# placement assertions coupled to the declarative release instead of duplicating its patches.
readonly KAGENT_RELEASE="${WORK_DIR}/kagent-helmrelease.yaml"
readonly KAGENT_POST_RENDER="${WORK_DIR}/kagent-post-render"
yq 'select(.kind == "HelmRelease" and .metadata.name == "kagent" and
  .metadata.namespace == "kagent")' "${GCP_RENDER}" >"${KAGENT_RELEASE}"
mkdir "${KAGENT_POST_RENDER}"
cp "${WORK_DIR}/kagent-raw.yaml" "${KAGENT_POST_RENDER}/rendered.yaml"
HELM_RELEASE_PATH="${KAGENT_RELEASE}" yq --null-input '
  .apiVersion = "kustomize.config.k8s.io/v1beta1" |
  .kind = "Kustomization" |
  .resources = ["rendered.yaml"] |
  .patches = (load(strenv(HELM_RELEASE_PATH)).spec.postRenderers |
    map(.kustomize.patches) | flatten)
' >"${KAGENT_POST_RENDER}/kustomization.yaml"
kubectl kustomize "${KAGENT_POST_RENDER}" >"${WORK_DIR}/kagent.yaml"

assert_yq 'select(.kind == "RemoteMCPServer" and .metadata.name == "kagent-tool-server") |
  .metadata.annotations."fgentic.dev/mcp-catalog-entry" == "kagent-tools"' \
	"${WORK_DIR}/kagent.yaml" "production kagent render lost its vetted MCP catalog identity"

assert_yq 'select(.kind == "Deployment" and .metadata.name == "traefik") |
  (.spec.replicas == 2 and .spec.template.spec.topologySpreadConstraints[0].topologyKey ==
  "kubernetes.io/hostname" and
  .spec.template.spec.topologySpreadConstraints[0].whenUnsatisfiable == "DoNotSchedule" and
  .spec.template.spec.topologySpreadConstraints[0].maxSkew == 1 and
  (.spec.template.spec.topologySpreadConstraints[0].labelSelector.matchLabels as $selector |
    (($selector | length) > 0 and
      (.spec.template.metadata.labels | contains($selector)))))' \
	"${WORK_DIR}/traefik.yaml" \
	"Traefik must be replicated with a matching hostname-spread selector"
assert_yq 'select(.kind == "PodDisruptionBudget" and .metadata.name == "traefik") |
  .spec.maxUnavailable == 1' "${WORK_DIR}/traefik.yaml" "Traefik PDB is missing"
assert_pdbs_target_replicated_workloads "${WORK_DIR}/traefik.yaml" 1 \
	"Traefik PDB selector must target its two-replica workload"

for workload in ess-element-web ess-haproxy ess-matrix-authentication-service; do
	assert_yq "select(.kind == \"Deployment\" and .metadata.name == \"${workload}\") |
    (.spec.replicas == 2 and
    .spec.template.spec.topologySpreadConstraints[0].topologyKey == \"kubernetes.io/hostname\" and
    .spec.template.spec.topologySpreadConstraints[0].whenUnsatisfiable == \"DoNotSchedule\" and
    .spec.template.spec.topologySpreadConstraints[0].maxSkew == 1 and
    (.spec.template.spec.topologySpreadConstraints[0].labelSelector.matchLabels as \$selector |
      ((\$selector | length) > 0 and
        (.spec.template.metadata.labels | contains(\$selector)))) and
    ([.spec.template.spec.containers[] | select(
      .resources.requests.cpu == null or .resources.requests.memory == null or
      .resources.limits.cpu == null or .resources.limits.memory == null)] | length) == 0)" \
		"${WORK_DIR}/matrix.yaml" "${workload} lost replicas, spreading, or resource bounds"
done
assert_yq 'select(.kind == "StatefulSet" and .metadata.name == "ess-synapse-main") |
  .spec.replicas == 1 and ([.spec.template.spec.containers[] | select(
    .resources.requests.cpu == null or .resources.requests.memory == null or
    .resources.limits.cpu == null or .resources.limits.memory == null)] | length) == 0' \
	"${WORK_DIR}/matrix.yaml" "Synapse must stay one bounded fast-restart instance"
assert_yq 'select(.kind == "PersistentVolumeClaim" and .metadata.name == "ess-synapse-media") |
  (.metadata.annotations."helm.sh/resource-policy" == "keep" and
  .spec.storageClassName == "standard-rwo" and .spec.resources.requests.storage == "10Gi")' \
	"${WORK_DIR}/matrix.yaml" "the rendered Synapse media PVC lost its retained CSI contract"

assert_yq 'select(.kind == "Deployment" and .metadata.name == "agentgateway") |
  .spec.template.metadata.labels as $labels |
  (.spec.replicas == 2 and
  .spec.strategy.type == "RollingUpdate" and
  .spec.strategy.rollingUpdate.maxSurge == 0 and
  .spec.strategy.rollingUpdate.maxUnavailable == 1 and
  ([.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[] |
    select(.topologyKey == "kubernetes.io/hostname" and
      (.labelSelector.matchLabels as $selector |
        (($selector | length) > 0 and ($labels | contains($selector)))))] | length) == 1)' \
	"${WORK_DIR}/agentgateway.yaml" "agentgateway controller must be replicated across hosts"
assert_yq 'select(.kind == "PodDisruptionBudget" and .metadata.name == "agentgateway") |
  .spec.maxUnavailable == 1' "${WORK_DIR}/agentgateway.yaml" \
	"agentgateway controller PDB is missing"
assert_pdbs_target_replicated_workloads "${WORK_DIR}/agentgateway.yaml" 1 \
	"agentgateway controller PDB selector must target its two-replica workload"

for workload in kagent-controller kagent-kmcp-controller-manager kagent-tools kagent-ui; do
	assert_yq "select(.kind == \"Deployment\" and .metadata.name == \"${workload}\") |
    (.spec.replicas == 2 and
    .spec.template.spec.topologySpreadConstraints[0].topologyKey == \"kubernetes.io/hostname\" and
    .spec.template.spec.topologySpreadConstraints[0].whenUnsatisfiable == \"DoNotSchedule\" and
    .spec.template.spec.topologySpreadConstraints[0].maxSkew == 1 and
    (.spec.template.spec.topologySpreadConstraints[0].labelSelector.matchLabels as \$selector |
      ((\$selector | length) > 0 and
        (.spec.template.metadata.labels | contains(\$selector)))) and
    ([.spec.template.spec.containers[] | select(
      .resources.requests.cpu == null or .resources.requests.memory == null or
      .resources.limits.cpu == null or .resources.limits.memory == null)] | length) == 0)" \
		"${WORK_DIR}/kagent.yaml" "${workload} must have two bounded, host-spread replicas"
done

assert_yq 'select(.kind == "StatefulSet" and .metadata.name == "keycloak") |
  .spec.template.metadata.labels as $labels |
  (.spec.replicas == 2 and
  ([.spec.template.spec.affinity.podAntiAffinity.requiredDuringSchedulingIgnoredDuringExecution[] |
    select(.topologyKey == "kubernetes.io/hostname" and
      (.labelSelector.matchLabels as $selector |
        (($selector | length) > 0 and ($labels | contains($selector)))))] | length) == 1 and
  ([.spec.template.spec.containers[] | select(.name == "keycloak") | select(
    .resources.requests.cpu == null or .resources.requests.memory == null or
    .resources.limits.cpu == null or .resources.limits.memory == null)] | length) == 0)' \
	"${WORK_DIR}/keycloak.yaml" "Keycloak must have two bounded, host-separated replicas"
assert_yq 'select(.kind == "PodDisruptionBudget" and .metadata.name == "keycloak") |
  .spec.maxUnavailable == 1' "${WORK_DIR}/keycloak.yaml" "Keycloak PDB is missing"
assert_pdbs_target_replicated_workloads "${WORK_DIR}/keycloak.yaml" 1 \
	"Keycloak PDB selector must target its two-replica workload"

assert_yq_all_files '
  [select(.kind == "Deployment" or .kind == "StatefulSet") |
    select(.metadata.namespace == "matrix" or .metadata.namespace == "kagent")] as $workloads |
  [select(.kind == "PodDisruptionBudget") |
    select(.metadata.namespace == "matrix" or .metadata.namespace == "kagent")] as $pdbs |
  [
    (($pdbs | length) == 7),
    ($pdbs | all_c(. as $pdb | [
      ((($pdb.spec.selector.matchExpressions // []) | length) == 0),
      ((($pdb.spec.selector.matchLabels // {}) | length) > 0),
      (([
        $workloads[] |
        select(.metadata.namespace == $pdb.metadata.namespace) |
        select((.spec.replicas // 1) == 2) |
        select(.spec.template.metadata.labels as $labels |
          $labels | contains($pdb.spec.selector.matchLabels // {}))
      ] | length) == 1)
    ] | all_c(.)))
  ] | all_c(.)' "every Matrix/kagent PDB must select exactly one two-replica workload" \
	"${GCP_RENDER}" "${WORK_DIR}/matrix.yaml" "${WORK_DIR}/kagent.yaml"

assert_yq 'select(.kind == "Deployment" and .metadata.name == "matrix-a2a-bridge") |
  .spec.replicas == 1' "${WORK_DIR}/bridge.yaml" "bridge replica invariant changed"
assert_yq_all '[select(.kind == "PodDisruptionBudget")] | length == 0' \
	"${WORK_DIR}/bridge.yaml" "bridge PDB would block its single-replica drain"

for manifest in traefik matrix agentgateway kagent keycloak bridge; do
	assert_bounded_workloads "${WORK_DIR}/${manifest}.yaml" "${manifest} production render"
done

echo "==> Proving the production component is opt-in"
assert_yq 'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack" and
  .metadata.namespace == "matrix") | .spec.values.synapse.media.storage |
  (.size == "10Gi" and .storageClassName == "local-path" and .resourcePolicy == "keep")' \
	"${LOCAL_RENDER}" "local Synapse media must stay on an explicitly retained local PVC"
assert_yq_all '[select(.kind == "VolumeSnapshotClass" and
  .metadata.name == "fgentic-synapse-media")] | length == 0' \
	"${LOCAL_RENDER}" "local must not claim the GKE media snapshot boundary"
assert_yq 'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
  .spec.instances == 1' "${LOCAL_RENDER}" "local CNPG must remain one instance"
assert_yq 'select(.kind == "HelmRelease" and .metadata.name == "traefik") |
  (.spec.values.deployment.replicas == 1 and .spec.values.podDisruptionBudget == null)' \
	"${LOCAL_RENDER}" "local Traefik must remain one replica without a PDB"
assert_yq 'select(.kind == "HelmRelease" and .metadata.name == "keycloak") |
  (.spec.values.replicas == 1 and .spec.values.podDisruptionBudget == null)' \
	"${LOCAL_RENDER}" "local Keycloak must remain one replica without a PDB"
assert_yq 'select(.kind == "AgentgatewayParameters" and .metadata.name == "secured") |
  ((.spec.deployment.spec.replicas // 1) == 1 and .spec.podDisruptionBudget == null)' \
	"${LOCAL_RENDER}" "local agentgateway proxy must remain one replica without a PDB"
assert_yq 'select(.kind == "Deployment" and .metadata.name == "mcp-tool-rate-limit" and
  .metadata.namespace == "agentgateway-system") | .spec.replicas == 1' \
	"${LOCAL_RENDER}" "local MCP rate-limit service must remain one replica"
assert_yq_all '[select(.kind == "PodDisruptionBudget" and
  .metadata.name == "mcp-tool-rate-limit" and
  .metadata.namespace == "agentgateway-system")] | length == 0' \
	"${LOCAL_RENDER}" "local MCP rate-limit service must not inherit the production PDB"
assert_yq 'select(.kind == "HelmRelease" and .metadata.name == "kagent") |
  ((.spec.values.controller.replicas // 1) == 1 and (.spec.values.ui.replicas // 1) == 1 and
  (.spec.values."kagent-tools".replicaCount // 1) == 1 and
  (.spec.values.kmcp.controller.replicaCount // 1) == 1 and
  (.spec.postRenderers | length) == 1 and
  (.spec.postRenderers[0].kustomize.patches | length) == 1 and
  .spec.postRenderers[0].kustomize.patches[0].target.kind == "RemoteMCPServer")' \
	"${LOCAL_RENDER}" "local kagent workloads must remain one replica without HA post-rendering"
assert_yq 'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack") |
  ((.spec.values.elementWeb.replicas // 1) == 1 and
  (.spec.values.haproxy.replicas // 1) == 1 and
  (.spec.values.matrixAuthenticationService.replicas // 1) == 1)' \
	"${LOCAL_RENDER}" "local Matrix workloads must remain one replica"
assert_yq_all '[select(.kind == "PodDisruptionBudget" and
  (.metadata.namespace == "matrix" or .metadata.namespace == "kagent"))] | length == 0' \
	"${LOCAL_RENDER}" "evaluation profiles must not inherit production PDBs"
if rg --files-with-matches 'production-ha' "${ROOT_DIR}/clusters" \
	| rg -v '/gcp/kustomization.yaml$' >/dev/null; then
	fail "only the GCP reference may opt into production-ha by default"
fi

echo "==> Schema-validating effective manifests"
kubeconform -strict -ignore-missing-schemas -summary "${GCP_RENDER}"
for manifest in traefik matrix agentgateway kagent keycloak bridge; do
	kubeconform -strict -ignore-missing-schemas -summary "${WORK_DIR}/${manifest}.yaml"
done

echo "==> Production HA contract OK"
