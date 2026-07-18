#!/usr/bin/env bash
# Validate the pinned, structurally optional Trivy Operator contract. --runtime additionally uses
# an ownership-guarded disposable k3d cluster; it never targets the shared local clusters.
# shellcheck disable=SC2016 # jq/yq bindings in rendered-manifest assertions are intentionally literal
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly HELM_RELEASE="${ROOT_DIR}/infra/trivy-operator/helmrelease.yaml"
readonly SOURCE="${ROOT_DIR}/infra/trivy-operator/source.yaml"
readonly RBAC="${ROOT_DIR}/infra/trivy-operator/rbac.yaml"
readonly NETWORK_POLICY="${ROOT_DIR}/infra/trivy-operator/networkpolicy.yaml"
readonly SCAN_QUOTA="${ROOT_DIR}/infra/trivy-operator/quota.yaml"
readonly TRIVY_MONITOR="${ROOT_DIR}/infra/observability/monitors/trivy-alert.yaml"
readonly K3D_CONFIG="${ROOT_DIR}/infra/k3d-config.yaml"
readonly OWNER_LABEL="dev.fgentic.trivy-test.owner"
readonly CHART_VERSION="0.34.0"
readonly CHART_DIGEST="df8d44a9b9cbc2e2be60c367d93b05cd857e032bd1870e9fa99bb9cff387cdbc"
readonly CHART_URL="https://github.com/aquasecurity/helm-charts/releases/download/trivy-operator-${CHART_VERSION}/trivy-operator-${CHART_VERSION}.tgz"
readonly FIXTURE_IMAGE="mirror.gcr.io/library/alpine:3.22.1@sha256:4bcff63911fcb4448bd4fdacec207030997caf25e9bea4045fa6c8c44de311d1"
readonly OPERATOR_NAMESPACE="trivy-system"
readonly OUT_OF_SCOPE_NAMESPACE="trivy-out-of-scope"
readonly OPERATOR_MEMORY_LIMIT_BYTES=$((256 * 1024 * 1024))
readonly -a RUNTIME_K3S_ARGS=(
	'--disable=traefik@server:*'
	'--disable-network-policy@server:*'
	'--kubelet-arg=feature-gates=KubeletInUserNamespace=true@server:*'
	'--kubelet-arg=fail-cgroupv1=false@server:*'
	'--kube-proxy-arg=masquerade-all=true@server:*'
)
readonly -a TARGET_NAMESPACES=(
	cert-manager
	gateway
	cnpg-system
	postgres
	knowledge
	keycloak
	matrix
	agentgateway-system
	kagent
	bridge
	bridges
	monitoring
	models
)

RUNTIME_CLUSTER_NAME=""
RUNTIME_NODE_NAME=""
RUNTIME_OWNER_TOKEN=""
RUNTIME_WORKDIR=""
RUNTIME_CLUSTER_OWNED=false
RUNTIME_PORT_FORWARD_PID=""
RUNTIME_METRICS_PORT=""
RUNTIME_SCAN_MONITOR_PID=""
RUNTIME_VOLUME_NAMES=()

runtime=false
if [[ "${1:-}" == "--runtime" ]]; then
	runtime=true
elif (($# > 0)); then
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

assert_yq() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq --exit-status "${expression}" "${document}" >/dev/null || fail "${message}"
}

assert_jq_yaml() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq -o=json '.' "${document}" | jq --slurp --exit-status "${expression}" >/dev/null \
		|| fail "${message}"
}

static_contract() {
	require_commands diff jq kubectl rg sort tr yq

	local arg network_policy_arg_count=0
	for arg in "${RUNTIME_K3S_ARGS[@]}"; do
		if [[ "${arg}" == '--disable-network-policy@server:*' ]]; then
			network_policy_arg_count=$((network_policy_arg_count + 1))
		fi
	done
	((network_policy_arg_count == 1)) \
		|| fail "Trivy runtime must disable the failed NetworkPolicy controller on every server"

	assert_yq '
    .kind == "GitRepository" and
    .metadata.name == "trivy-operator" and
    .metadata.namespace == "flux-system" and
    .spec.url == "https://github.com/aquasecurity/trivy-operator.git" and
    .spec.ref.commit == "1006872c1463e81a40d48298145625aefef2a02f" and
    (.spec.ref | has("tag") | not) and
    (.spec.ref | has("branch") | not)
  ' "${SOURCE}" "Trivy chart source is not pinned to the reviewed release commit"
	rg --fixed-strings '# trivy-operator-version: v0.32.0' "${SOURCE}" >/dev/null \
		|| fail "Trivy release metadata for Renovate drifted"

	assert_yq '
    .kind == "HelmRelease" and
    .metadata.name == "trivy-operator" and
    .metadata.namespace == "trivy-system" and
    .spec.chart.spec.chart == "./deploy/helm" and
    .spec.chart.spec.reconcileStrategy == "Revision" and
    .spec.chart.spec.sourceRef.kind == "GitRepository" and
    .spec.chart.spec.sourceRef.name == "trivy-operator" and
    .spec.chart.spec.sourceRef.namespace == "flux-system" and
    .spec.install.crds == "CreateReplace" and
    .spec.upgrade.crds == "CreateReplace"
  ' "${HELM_RELEASE}" "Trivy Helm source or CRD lifecycle drifted"

	local configured_namespace_set expected_namespace_set target_namespaces
	target_namespaces="$(yq -r '.spec.values.targetNamespaces' "${HELM_RELEASE}")"
	local -a configured_namespaces
	IFS=, read -r -a configured_namespaces <<<"${target_namespaces}"
	((${#configured_namespaces[@]} == ${#TARGET_NAMESPACES[@]})) \
		|| fail "Trivy target namespace count drifted"
	local namespace
	for namespace in "${configured_namespaces[@]}"; do
		[[ "${namespace}" =~ ^[a-z0-9-]+$ ]] \
			|| fail "invalid or whitespace-padded Trivy target namespace: ${namespace}"
	done
	expected_namespace_set="$(printf '%s\n' "${TARGET_NAMESPACES[@]}" | sort)"
	configured_namespace_set="$(printf '%s\n' "${configured_namespaces[@]}" | sort)"
	[[ "${configured_namespace_set}" == "${expected_namespace_set}" ]] \
		|| fail "Trivy target namespace set drifted"

	assert_yq '
    .spec.values.targetWorkloads == "pod,replicaset,statefulset,daemonset,cronjob,job" and
    .spec.values.operator.scanJobsConcurrentLimit == 1 and
    .spec.values.operator.scanJobTTL == "30s" and
    .spec.values.operator.scanJobTimeout == "10m" and
    .spec.values.operator.scanJobsRetryDelay == "1m" and
    ((.spec.values.operator.scanSecretTTL // "") == "") and
    .spec.values.operator.scannerReportTTL == "24h" and
    .spec.values.operator.vulnerabilityScannerEnabled == true and
    .spec.values.operator.sbomGenerationEnabled == false and
    .spec.values.operator.clusterSbomCacheEnabled == false and
    .spec.values.operator.configAuditScannerEnabled == false and
    .spec.values.operator.rbacAssessmentScannerEnabled == false and
    .spec.values.operator.infraAssessmentScannerEnabled == false and
    .spec.values.operator.clusterComplianceEnabled == false and
    .spec.values.operator.exposedSecretScannerEnabled == false and
    .spec.values.operator.accessGlobalSecretsAndServiceAccount == false and
    .spec.values.operator.metricsFindingsEnabled == true and
    .spec.values.operator.metricsVulnIdEnabled == false and
    .spec.values.trivyOperator.scanJobsInSameNamespace == false and
    .spec.values.trivyOperator.scanJobAutomountServiceAccountToken == false and
    .spec.values.trivy.severity == "HIGH,CRITICAL" and
    .spec.values.trivy.slow == true and
    .spec.values.trivy.timeout == "9m0s" and
    .spec.values.rbac.create == false and
    .spec.values.serviceAccount.create == false and
    .spec.values.serviceAccount.name == "trivy-operator" and
    (.spec.values.compliance.specs | length) == 0
  ' "${HELM_RELEASE}" "vulnerability-only scanner controls drifted"

	assert_yq '
    .spec.values.image.tag == "0.32.0@sha256:d4a61c4607e2931bd2615bf3bcf8912669d11d194c44c77edd413e6301b50c5b" and
    .spec.values.trivy.image.tag == "0.72.0@sha256:cffe3f5161a47a6823fbd23d985795b3ed72a4c806da4c4df16266c02accdd6f" and
    (.spec.values.resources | keys | sort | join(",")) == "limits,requests" and
    (.spec.values.resources.requests | keys | sort | join(",")) ==
      "cpu,ephemeral-storage,memory" and
    .spec.values.resources.requests.cpu == "50m" and
    .spec.values.resources.requests.memory == "64Mi" and
    .spec.values.resources.requests["ephemeral-storage"] == "128Mi" and
    (.spec.values.resources.limits | keys | sort | join(",")) ==
      "cpu,ephemeral-storage,memory" and
    .spec.values.resources.limits.cpu == "200m" and
    .spec.values.resources.limits.memory == "256Mi" and
    .spec.values.resources.limits["ephemeral-storage"] == "512Mi" and
    (.spec.values.trivy.resources | keys | sort | join(",")) == "limits,requests" and
    (.spec.values.trivy.resources.requests | keys | sort | join(",")) ==
      "cpu,ephemeralStorage,memory" and
    .spec.values.trivy.resources.requests.cpu == "100m" and
    .spec.values.trivy.resources.requests.memory == "128Mi" and
    .spec.values.trivy.resources.requests.ephemeralStorage == "256Mi" and
    (.spec.values.trivy.resources.limits | keys | sort | join(",")) ==
      "cpu,ephemeralStorage,memory" and
    .spec.values.trivy.resources.limits.cpu == "500m" and
    .spec.values.trivy.resources.limits.memory == "512Mi" and
    .spec.values.trivy.resources.limits.ephemeralStorage == "2Gi" and
    .spec.values.podSecurityContext.runAsNonRoot == true and
    .spec.values.podSecurityContext.seccompProfile.type == "RuntimeDefault" and
    .spec.values.securityContext.readOnlyRootFilesystem == true and
    .spec.values.trivyOperator.scanJobPodTemplatePodSecurityContext.runAsNonRoot == true and
    .spec.values.trivyOperator.scanJobPodTemplateContainerSecurityContext.readOnlyRootFilesystem == true
  ' "${HELM_RELEASE}" "Trivy image, resource, or Pod security pins drifted"

	local deleted_roles expected_deleted_roles
	deleted_roles="$(
		yq -r '.spec.postRenderers[].kustomize.patches[].target |
      select(.kind == "ClusterRole") | .name' "${HELM_RELEASE}" | sort
	)"
	expected_deleted_roles="$(printf '%s\n' \
		aggregate-config-audit-reports-view \
		aggregate-exposed-secret-reports-view \
		aggregate-vulnerability-reports-view | sort)"
	[[ "${deleted_roles}" == "${expected_deleted_roles}" ]] \
		|| fail "unconditional chart aggregate roles are not all post-render deleted"

	local bound_namespaces expected_bound_namespaces
	bound_namespaces="$(
		yq eval-all -r '
      select(.kind == "RoleBinding" and .metadata.name == "trivy-operator-target") |
      .metadata.namespace
    ' "${RBAC}" | rg -v '^---$' | sort
	)"
	expected_bound_namespaces="$(printf '%s\n' "${TARGET_NAMESPACES[@]}" | sort)"
	[[ "${bound_namespaces}" == "${expected_bound_namespaces}" ]] \
		|| fail "Trivy target RoleBindings drifted"
	assert_jq_yaml '
    [.[] | select(.kind == "ClusterRole" and .metadata.name == "trivy-operator-target")]
      as $roles |
    ($roles | length) == 1 and
    ($roles[0] as $role |
      ([ $role.rules[].resources[] ] | index("secrets") | not) and
      ([ $role.rules[].resources[] ] | index("serviceaccounts") | not) and
      any($role.rules[];
        .apiGroups == ["aquasecurity.github.io"] and
        .resources == ["vulnerabilityreports"] and
        (.verbs | sort) == ["create", "delete", "get", "list", "update", "watch"]))
  ' "${RBAC}" "target role gained secret access or lost exact report mutation verbs"
	assert_jq_yaml '
    [.[] | select(
      .kind == "ClusterRole" and .metadata.name == "trivy-operator-cluster-reports"
    )] as $roles |
    ($roles | length) == 1 and
    ($roles[0] as $role |
      ($role.rules | length) == 1 and
      $role.rules[0].apiGroups == ["aquasecurity.github.io"] and
      ($role.rules[0].resources | sort) ==
        ["clustercompliancereports", "clusterrbacassessmentreports"] and
      ($role.rules[0].verbs | sort) == ["list", "watch"])
  ' "${RBAC}" "cluster-wide report reader drifted"
	local cluster_binding_count
	cluster_binding_count="$(yq eval-all -r '[select(.kind == "ClusterRoleBinding")] | length' "${RBAC}")"
	[[ "${cluster_binding_count}" == "1" ]] || fail "expected one narrow ClusterRoleBinding"

	assert_jq_yaml '
    length == 1 and
    (.[0] as $quota |
      $quota.kind == "ResourceQuota" and
      $quota.metadata.name == "trivy-scan-serialization" and
      $quota.metadata.namespace == "trivy-system" and
      $quota.spec == {"hard": {"count/jobs.batch": "1"}})
  ' "${SCAN_QUOTA}" "Trivy scan serialization quota drifted"

	assert_jq_yaml '
    [.[] | select(.kind == "NetworkPolicy")] as $policies |
    ($policies | length) == 2 and
    ([$policies[] | select(.metadata.name == "default-deny")] | length) == 1 and
    ([$policies[] | select(.metadata.name == "trivy-operator")] | length) == 1 and
    ($policies[] | select(.metadata.name == "default-deny") |
      .metadata.namespace == "trivy-system" and
      .spec == {
        "podSelector": {},
        "policyTypes": ["Ingress", "Egress"]
      }) and
    ($policies[] | select(.metadata.name == "trivy-operator") as $policy |
      $policy.kind == "NetworkPolicy" and
      $policy.metadata.name == "trivy-operator" and
      $policy.metadata.namespace == "trivy-system" and
      $policy.spec.podSelector == {
        "matchLabels": {"fgentic.dev/trivy-network": "true"}
      } and
      $policy.spec.policyTypes == ["Ingress", "Egress"] and
      $policy.spec.ingress == [{
        "from": [{
          "namespaceSelector": {
            "matchLabels": {"kubernetes.io/metadata.name": "monitoring"}
          }
        }],
        "ports": [{"protocol": "TCP", "port": 8080}]
      }] and
      $policy.spec.egress == [
        {
          "to": [{
            "namespaceSelector": {
              "matchLabels": {"kubernetes.io/metadata.name": "kube-system"}
            },
            "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}
          }],
          "ports": [
            {"protocol": "UDP", "port": 53},
            {"protocol": "TCP", "port": 53}
          ]
        },
        {"ports": [{"protocol": "TCP", "port": 443}]}
      ])
  ' "${NETWORK_POLICY}" "Trivy NetworkPolicy exact ingress or egress shape drifted"

	assert_jq_yaml '
    [.[] | select(
      .kind == "ServiceMonitor" and
      .metadata.name == "trivy-operator" and
      .metadata.namespace == "monitoring"
    )] as $monitors |
    ($monitors | length) == 1 and
    ($monitors[0] as $monitor |
      $monitor.spec.namespaceSelector == {"matchNames": ["trivy-system"]} and
      $monitor.spec.selector == {"matchLabels": {
        "app.kubernetes.io/name": "trivy-operator",
        "app.kubernetes.io/instance": "trivy-operator"
      }} and
      $monitor.spec.endpoints == [{
        "port": "metrics",
        "path": "/metrics",
        "scheme": "http",
        "honorLabels": true
      }])
  ' "${TRIVY_MONITOR}" "Trivy ServiceMonitor selector or endpoint contract drifted"

	local environment expected count
	for environment in local gcp demo federation; do
		expected=1
		if [[ "${environment}" == demo || "${environment}" == federation ]]; then
			expected=0
		fi
		count="$(
			kubectl kustomize "${ROOT_DIR}/clusters/${environment}" \
				| yq -o=json '.' \
				| jq --slurp '[.[] | select(
              .kind == "Kustomization" and
              .metadata.namespace == "flux-system" and
              (
                (.metadata.name | contains("trivy")) or
                ((.spec.path // "") | contains("trivy")) or
                any(.spec.dependsOn[]?; (.name // "") | contains("trivy"))
              )
            )] | length'
		)"
		[[ "${count}" == "${expected}" ]] \
			|| fail "clusters/${environment} Trivy structural footprint is ${count}, expected ${expected}"
	done
	assert_yq '
    [
      ([.patches[] | select(
        .target.group == "kustomize.toolkit.fluxcd.io" and
        .target.version == "v1" and
        .target.kind == "Kustomization" and
        .target.name == "namespaces" and
        .target.namespace == "flux-system" and
        (.target | keys | sort | join(",")) == "group,kind,name,namespace,version"
      )] | length) == 1
    ] | all
  ' "${ROOT_DIR}/clusters/demo/kustomization.yaml" \
		"demo must patch the exact early namespaces Kustomization"
	assert_yq '
    .patches[] | select(
      .target.kind == "Kustomization" and .target.name == "namespaces"
    ) | .patch | from_yaml |
    [
      .apiVersion == "kustomize.toolkit.fluxcd.io/v1",
      .kind == "Kustomization",
      .metadata.name == "namespaces",
      .metadata.namespace == "flux-system",
      (.metadata | keys | sort | join(",")) == "name,namespace",
      (.spec | keys | join(",")) == "patches",
      (.spec.patches | length) == 3,
      .spec.patches[0].target.kind == "Namespace",
      .spec.patches[0].target.name == "trivy-system",
      (.spec.patches[0].target | keys | sort | join(",")) == "kind,name",
      (.spec.patches[0].patch | from_yaml | ."$patch") == "delete",
      (.spec.patches[0].patch | from_yaml | .apiVersion) == "v1",
      (.spec.patches[0].patch | from_yaml | .kind) == "Namespace",
      (.spec.patches[0].patch | from_yaml | .metadata.name) == "trivy-system",
      (.spec.patches[0].patch | from_yaml | .metadata | keys | join(",")) == "name",
      .spec.patches[1].target.kind == "ResourceQuota",
      .spec.patches[1].target.name == "compute-budget",
      .spec.patches[1].target.namespace == "trivy-system",
      (.spec.patches[1].target | keys | sort | join(",")) == "kind,name,namespace",
      (.spec.patches[1].patch | from_yaml | ."$patch") == "delete",
      (.spec.patches[1].patch | from_yaml | .apiVersion) == "v1",
      (.spec.patches[1].patch | from_yaml | .kind) == "ResourceQuota",
      (.spec.patches[1].patch | from_yaml | .metadata.name) == "compute-budget",
      (.spec.patches[1].patch | from_yaml | .metadata.namespace) == "trivy-system",
      .spec.patches[2].target.kind == "LimitRange",
      .spec.patches[2].target.name == "container-defaults",
      .spec.patches[2].target.namespace == "trivy-system",
      (.spec.patches[2].target | keys | sort | join(",")) == "kind,name,namespace",
      (.spec.patches[2].patch | from_yaml | ."$patch") == "delete",
      (.spec.patches[2].patch | from_yaml | .apiVersion) == "v1",
      (.spec.patches[2].patch | from_yaml | .kind) == "LimitRange",
      (.spec.patches[2].patch | from_yaml | .metadata.name) == "container-defaults",
      (.spec.patches[2].patch | from_yaml | .metadata.namespace) == "trivy-system"
    ] | all
  ' "${ROOT_DIR}/clusters/demo/kustomization.yaml" \
		"demo must delete the exact Trivy namespace admission bundle"

	if ! kubectl kustomize "${ROOT_DIR}/clusters/federation" \
		| yq -o=json '.' \
		| jq --slurp --exit-status '
        [.[] | select(
          .kind == "Kustomization" and
          .metadata.name == "namespaces" and
          .metadata.namespace == "flux-system"
        ) | .spec.components] == [["../federation/namespaces"]]
      ' >/dev/null; then
		fail "federation must wire the exact namespace-pruning component"
	fi
	assert_yq '
    [
      ([.patches[] | select(
        .target.kind == "Namespace" and .target.name == "trivy-system"
      )] | length) == 1,
      ([.patches[] | select(
        .target.kind == "ResourceQuota" and
        .target.name == "compute-budget" and
        .target.namespace == "bridge|bridges|monitoring|trivy-system"
      )] | length) == 1,
      ([.patches[] | select(
        .target.kind == "LimitRange" and
        .target.name == "container-defaults" and
        .target.namespace == "bridge|bridges|monitoring|trivy-system"
      )] | length) == 1
    ] | all
  ' "${ROOT_DIR}/infra/federation/namespaces/kustomization.yaml" \
		"federation namespace component does not target the exact Trivy admission bundle"
	assert_yq '
    .patches[] | select(
      .target.kind == "Namespace" and .target.name == "trivy-system"
    ) | .patch | from_yaml |
    [
      ."$patch" == "delete",
      .apiVersion == "v1",
      .kind == "Namespace",
      .metadata.name == "trivy-system",
      (.metadata | keys | join(",")) == "name"
    ] | all
  ' "${ROOT_DIR}/infra/federation/namespaces/kustomization.yaml" \
		"federation must delete the exact trivy-system Namespace"
	assert_yq '
    .patches[] | select(
      .target.kind == "ResourceQuota" and .target.name == "compute-budget"
    ) | .patch | from_yaml |
    [
      ."$patch" == "delete",
      .apiVersion == "v1",
      .kind == "ResourceQuota",
      .metadata.name == "compute-budget",
      (.metadata | keys | join(",")) == "name"
    ] | all
  ' "${ROOT_DIR}/infra/federation/namespaces/kustomization.yaml" \
		"federation must delete the exact Trivy compute quota"
	assert_yq '
    .patches[] | select(
      .target.kind == "LimitRange" and .target.name == "container-defaults"
    ) | .patch | from_yaml |
    [
      ."$patch" == "delete",
      .apiVersion == "v1",
      .kind == "LimitRange",
      .metadata.name == "container-defaults",
      (.metadata | keys | join(",")) == "name"
    ] | all
  ' "${ROOT_DIR}/infra/federation/namespaces/kustomization.yaml" \
		"federation must delete the exact Trivy container defaults"
	assert_yq '
    select(.kind == "Kustomization" and .metadata.name == "observability-monitors") |
    (([.spec.dependsOn[].name] | sort | join(",")) == "observability")
  ' "${ROOT_DIR}/clusters/base/infrastructure.yaml" \
		"observability monitors must not be availability-coupled to Trivy"

	# Docker inspection failure is unknown state, never evidence that cleanup completed.
	if (
		runtime_container_ids() { return 42; }
		runtime_artifacts_exist
	) >/dev/null 2>&1; then
		fail "runtime artifact discovery accepted a failed container inventory"
	fi
	if (
		runtime_container_ids() { return 42; }
		cleanup_runtime_cluster_artifacts
	) >/dev/null 2>&1; then
		fail "runtime cleanup accepted a failed container inventory"
	fi

	echo "Trivy Operator static contract passed"
}

runtime_cluster_exists() {
	k3d cluster get "${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1
}

runtime_container_ids() {
	docker ps --all --filter "label=k3d.cluster=${RUNTIME_CLUSTER_NAME}" --quiet
}

runtime_network_exists() {
	docker network inspect "k3d-${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1
}

runtime_volume_exists() {
	docker volume inspect "k3d-${RUNTIME_CLUSTER_NAME}-images" >/dev/null 2>&1
}

runtime_artifacts_exist() {
	local container_ids
	container_ids="$(runtime_container_ids)" \
		|| fail "could not inspect disposable Trivy containers"
	[[ -n "${container_ids}" ]] || runtime_network_exists || runtime_volume_exists \
		|| runtime_recorded_volumes_exist
}

runtime_recorded_volumes_exist() {
	local volume
	for volume in "${RUNTIME_VOLUME_NAMES[@]}"; do
		if docker volume inspect "${volume}" >/dev/null 2>&1; then
			return 0
		fi
	done
	return 1
}

runtime_owned_by_test() {
	[[ "$(docker inspect --format "{{ index .Config.Labels \"${OWNER_LABEL}\" }}" \
		"${RUNTIME_NODE_NAME}" 2>/dev/null || true)" == "${RUNTIME_OWNER_TOKEN}" ]]
}

runtime_cleanup_complete() {
	! runtime_cluster_exists && ! runtime_artifacts_exist
}

stop_runtime_processes() {
	if [[ -n "${RUNTIME_SCAN_MONITOR_PID}" ]]; then
		touch "${RUNTIME_WORKDIR}/stop-scan-monitor" 2>/dev/null || true
		wait "${RUNTIME_SCAN_MONITOR_PID}" 2>/dev/null || true
		RUNTIME_SCAN_MONITOR_PID=""
	fi
	if [[ -n "${RUNTIME_PORT_FORWARD_PID}" ]]; then
		kill "${RUNTIME_PORT_FORWARD_PID}" 2>/dev/null || true
		wait "${RUNTIME_PORT_FORWARD_PID}" 2>/dev/null || true
		RUNTIME_PORT_FORWARD_PID=""
	fi
}

collect_runtime_diagnostics() {
	local destination
	if [[ -n "${TRIVY_DIAGNOSTICS_DIR:-}" ]]; then
		destination="${TRIVY_DIAGNOSTICS_DIR%/}"
	else
		destination="${RUNTIME_WORKDIR}/diagnostics"
	fi
	mkdir -p "${destination}"
	cp "${HELM_RELEASE}" "${destination}/helmrelease.yaml"
	cp "${RBAC}" "${destination}/rbac.yaml"
	cp "${NETWORK_POLICY}" "${destination}/networkpolicy.yaml"
	cp "${SCAN_QUOTA}" "${destination}/quota.yaml"
	for artifact in chart.sha256 crds.yaml values.yaml rendered.yaml rendered-filtered.yaml \
		postrender-kustomization.yaml fixtures.yaml synthetic-high-1.json \
		synthetic-high-2.json quota-hold.yaml quota-operator.log scan-concurrency.log \
		scan-working-set.log scan-pod.json port-forward.log; do
		if [[ -f "${RUNTIME_WORKDIR}/${artifact}" ]]; then
			cp "${RUNTIME_WORKDIR}/${artifact}" "${destination}/${artifact}"
		fi
	done
	{
		k3d cluster list --output json || true
	} >"${destination}/k3d-clusters.json" 2>&1
	{
		docker ps --all --filter "label=k3d.cluster=${RUNTIME_CLUSTER_NAME}" --no-trunc
		docker inspect "${RUNTIME_NODE_NAME}"
		docker network inspect "k3d-${RUNTIME_CLUSTER_NAME}"
		docker volume inspect "k3d-${RUNTIME_CLUSTER_NAME}-images"
	} >"${destination}/docker-runtime.txt" 2>&1 || true

	if [[ "${RUNTIME_CLUSTER_OWNED}" == true && -s "${RUNTIME_WORKDIR}/kubeconfig" ]] \
		&& kubectl version --request-timeout=5s >/dev/null 2>&1; then
		kubectl get namespaces,pods,jobs,deployments,replicasets --all-namespaces \
			--output wide >"${destination}/workloads.txt" 2>&1 || true
		kubectl get events --all-namespaces --sort-by=.metadata.creationTimestamp \
			>"${destination}/events.txt" 2>&1 || true
		kubectl get vulnerabilityreports.aquasecurity.github.io --all-namespaces \
			--output yaml >"${destination}/vulnerabilityreports.yaml" 2>&1 || true
		kubectl --namespace "${OPERATOR_NAMESPACE}" describe deployment trivy-operator \
			>"${destination}/operator-describe.txt" 2>&1 || true
		kubectl --namespace "${OPERATOR_NAMESPACE}" logs deployment/trivy-operator --all-containers \
			--prefix >"${destination}/operator.log" 2>&1 || true
		kubectl --namespace "${OPERATOR_NAMESPACE}" logs \
			--selector app.kubernetes.io/managed-by=trivy-operator --all-containers \
			--prefix --max-log-requests=20 >"${destination}/scan-jobs.log" 2>&1 || true
		{
			kubectl auth can-i list pods --namespace models \
				--as="system:serviceaccount:${OPERATOR_NAMESPACE}:trivy-operator"
			kubectl auth can-i create vulnerabilityreports.aquasecurity.github.io \
				--namespace models --as="system:serviceaccount:${OPERATOR_NAMESPACE}:trivy-operator"
			kubectl auth can-i list pods --namespace "${OUT_OF_SCOPE_NAMESPACE}" \
				--as="system:serviceaccount:${OPERATOR_NAMESPACE}:trivy-operator"
			kubectl auth can-i create vulnerabilityreports.aquasecurity.github.io \
				--namespace "${OUT_OF_SCOPE_NAMESPACE}" \
				--as="system:serviceaccount:${OPERATOR_NAMESPACE}:trivy-operator"
		} >"${destination}/authorization.txt" 2>&1 || true
	fi

	if [[ -n "${TRIVY_DIAGNOSTICS_DIR:-}" ]]; then
		echo "==> Preserved Trivy runtime diagnostics in ${destination}" >&2
	else
		echo "==> Trivy Operator diagnostics" >&2
		for artifact in workloads.txt events.txt operator-describe.txt operator.log \
			scan-concurrency.log authorization.txt; do
			if [[ -s "${destination}/${artifact}" ]]; then
				echo "--- ${artifact}" >&2
				cat "${destination}/${artifact}" >&2
			fi
		done
	fi
}

cleanup_runtime_cluster_artifacts() {
	local attempt container_id_output network_owner volume volume_owner
	local -a container_ids=()
	for attempt in {1..30}; do
		container_ids=()
		if ! container_id_output="$(runtime_container_ids)"; then
			echo "error: could not inspect disposable Trivy containers during cleanup" >&2
			return 1
		fi
		if [[ -n "${container_id_output}" ]]; then
			mapfile -t container_ids <<<"${container_id_output}"
		fi
		if ((${#container_ids[@]} > 0)); then
			docker rm --force "${container_ids[@]}" >/dev/null 2>&1 || true
		fi
		if network_owner="$(docker network inspect --format '{{ index .Labels "app" }}' \
			"k3d-${RUNTIME_CLUSTER_NAME}" 2>/dev/null)"; then
			if [[ "${network_owner}" == "k3d" ]]; then
				docker network rm "k3d-${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1 || true
			fi
		fi
		if volume_owner="$(docker volume inspect \
			--format '{{ index .Labels "app" }}/{{ index .Labels "k3d.cluster" }}' \
			"k3d-${RUNTIME_CLUSTER_NAME}-images" 2>/dev/null)"; then
			if [[ "${volume_owner}" == "k3d/${RUNTIME_CLUSTER_NAME}" ]]; then
				docker volume rm "k3d-${RUNTIME_CLUSTER_NAME}-images" >/dev/null 2>&1 || true
			fi
		fi
		for volume in "${RUNTIME_VOLUME_NAMES[@]}"; do
			docker volume rm "${volume}" >/dev/null 2>&1 || true
		done
		if runtime_cleanup_complete; then
			return 0
		fi
		[[ "${attempt}" -eq 30 ]] || sleep 2
	done
	# Docker can finish removing a large anonymous containerd volume just after `volume rm`
	# returns. Give that exact owned inventory one final settle window before reporting a leak.
	sleep 5
	if runtime_cleanup_complete; then
		return 0
	fi
	return 1
}

assert_service_account_access() {
	local expected="$1"
	local verb="$2"
	local resource="$3"
	local namespace="$4"
	local actual
	actual="$(kubectl auth can-i "${verb}" "${resource}" --namespace "${namespace}" \
		--as="system:serviceaccount:${OPERATOR_NAMESPACE}:trivy-operator" || true)"
	[[ "${actual}" == "${expected}" ]] \
		|| fail "Trivy Operator ${verb} ${resource} in ${namespace}: got ${actual}, expected ${expected}"
}

wait_for_scan_quota_usage() {
	local expected="$1"
	local quota_ready=false
	for _ in {1..30}; do
		if kubectl --namespace "${OPERATOR_NAMESPACE}" get resourcequota \
			trivy-scan-serialization --output json \
			| jq --arg expected "${expected}" --exit-status '
          .status.hard["count/jobs.batch"] == "1" and
          (.status.used["count/jobs.batch"] // "0") == $expected
        ' >/dev/null; then
			quota_ready=true
			break
		fi
		sleep 1
	done
	[[ "${quota_ready}" == true ]] \
		|| fail "scan serialization quota usage did not become ${expected}"
}

create_scan_quota_hold() {
	cat >"${RUNTIME_WORKDIR}/quota-hold.yaml" <<EOF
apiVersion: batch/v1
kind: Job
metadata:
  name: trivy-quota-hold
  namespace: ${OPERATOR_NAMESPACE}
  labels:
    app.kubernetes.io/managed-by: trivy-operator
    fgentic.dev/runtime-probe: "true"
spec:
  suspend: true
  backoffLimit: 0
  template:
    metadata:
      labels:
        app.kubernetes.io/managed-by: trivy-operator
        fgentic.dev/runtime-probe: "true"
    spec:
      restartPolicy: Never
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 10000
        runAsGroup: 10000
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: hold
          image: ${FIXTURE_IMAGE}
          command: [/bin/true]
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: {drop: [ALL]}
            readOnlyRootFilesystem: true
          resources:
            requests: {cpu: 1m, memory: 1Mi}
            limits: {cpu: 10m, memory: 8Mi}
EOF
	kubectl create --filename "${RUNTIME_WORKDIR}/quota-hold.yaml" >/dev/null
	wait_for_scan_quota_usage 1
}

wait_for_operator_quota_denial() {
	for _ in {1..120}; do
		kubectl --namespace "${OPERATOR_NAMESPACE}" logs deployment/trivy-operator \
			--all-containers >"${RUNTIME_WORKDIR}/quota-operator.log"
		if rg --fixed-strings 'exceeded quota: trivy-scan-serialization' \
			"${RUNTIME_WORKDIR}/quota-operator.log" >/dev/null; then
			return
		fi
		sleep 1
	done
	fail "operator scan Job was not rejected by the occupied serialization quota"
}

release_scan_quota_hold() {
	kubectl --namespace "${OPERATOR_NAMESPACE}" delete job trivy-quota-hold --wait=true >/dev/null
	if kubectl --namespace "${OPERATOR_NAMESPACE}" get job trivy-quota-hold >/dev/null 2>&1; then
		fail "scan quota hold still exists after its blocking deletion"
	fi
}

monitor_scan_job_concurrency() {
	local active="" job_count="" jobs="" node_name="" operator_pod="" operator_pod_name=""
	local operator_pods="" pods="" snapshot="" working_set=""
	while [[ ! -e "${RUNTIME_WORKDIR}/stop-scan-monitor" ]]; do
		if jobs="$(kubectl --namespace "${OPERATOR_NAMESPACE}" get jobs \
			--selector app.kubernetes.io/managed-by=trivy-operator --output json 2>/dev/null)"; then
			active="$(jq '[.items[].status.active // 0] | add // 0' <<<"${jobs}")"
			job_count="$(jq '.items | length' <<<"${jobs}")"
			printf '%(%Y-%m-%dT%H:%M:%SZ)T active=%s jobs=%s\n' -1 "${active}" \
				"${job_count}" >>"${RUNTIME_WORKDIR}/scan-concurrency.log"
			if ((active > 0)); then
				touch "${RUNTIME_WORKDIR}/scan-job-observed"
				if [[ -z "${operator_pod_name}" || -z "${node_name}" ]] \
					&& operator_pods="$(kubectl --namespace "${OPERATOR_NAMESPACE}" get pods \
						--selector app.kubernetes.io/name=trivy-operator,app.kubernetes.io/instance=trivy-operator \
						--output json 2>/dev/null)"; then
					operator_pod="$(jq --compact-output '
                first(.items[] | select(.status.phase == "Running")) // empty
              ' <<<"${operator_pods}")"
					if [[ -n "${operator_pod}" ]]; then
						operator_pod_name="$(jq -r '.metadata.name' <<<"${operator_pod}")"
						node_name="$(jq -r '.spec.nodeName' <<<"${operator_pod}")"
					fi
				fi
				if [[ -n "${operator_pod_name}" && -n "${node_name}" ]]; then
					working_set="$(
						kubectl get --raw "/api/v1/nodes/${node_name}/proxy/stats/summary" 2>/dev/null \
							| jq -r --arg pod "${operator_pod_name}" --arg namespace \
								"${OPERATOR_NAMESPACE}" '
                    first(.pods[] | select(
                      .podRef.namespace == $namespace and .podRef.name == $pod
                    ) | .containers[] | select(.name == "trivy-operator") |
                      .memory.workingSetBytes) // empty
                  ' 2>/dev/null || true
					)"
					if [[ "${working_set}" =~ ^[1-9][0-9]*$ ]]; then
						printf '%(%Y-%m-%dT%H:%M:%SZ)T workingSetBytes=%s\n' -1 "${working_set}" \
							>>"${RUNTIME_WORKDIR}/scan-working-set.log"
						if ((working_set >= OPERATOR_MEMORY_LIMIT_BYTES)); then
							touch "${RUNTIME_WORKDIR}/scan-memory-violation"
						fi
					fi
				fi
				if [[ ! -s "${RUNTIME_WORKDIR}/scan-pod.json" ]] \
					&& pods="$(kubectl --namespace "${OPERATOR_NAMESPACE}" get pods \
						--selector app.kubernetes.io/managed-by=trivy-operator --output json \
						2>/dev/null)"; then
					snapshot="$(jq --compact-output '
                first(.items[] | select(
                  .status.phase == "Pending" or .status.phase == "Running"
                )) // empty
              ' <<<"${pods}")"
					if [[ -n "${snapshot}" ]]; then
						printf '%s\n' "${snapshot}" >"${RUNTIME_WORKDIR}/scan-pod.json"
					fi
				fi
			fi
			if ((active > 1)); then
				touch "${RUNTIME_WORKDIR}/scan-concurrency-violation"
			fi
		fi
		sleep 1
	done
}

assert_scan_pod_contract() {
	local scanner_image
	scanner_image="mirror.gcr.io/aquasec/trivy:$(
		yq -r '.spec.values.trivy.image.tag' "${HELM_RELEASE}"
	)"
	[[ -s "${RUNTIME_WORKDIR}/scan-pod.json" ]] \
		|| fail "the first active Trivy scan Pod was not captured"
	jq --exit-status --arg image "${scanner_image}" '
      .metadata.labels["app.kubernetes.io/managed-by"] == "trivy-operator" and
      .metadata.labels["fgentic.dev/trivy-network"] == "true" and
      .spec.automountServiceAccountToken == false and
      .spec.securityContext.runAsNonRoot == true and
      .spec.securityContext.runAsUser == 10000 and
      .spec.securityContext.runAsGroup == 10000 and
      .spec.securityContext.fsGroup == 10000 and
      .spec.securityContext.seccompProfile.type == "RuntimeDefault" and
      ([.spec.initContainers[]?, .spec.containers[]] | length) >= 2 and
      all(.spec.initContainers[]?, .spec.containers[];
        .image == $image and
        .resources.requests.cpu == "100m" and
        .resources.requests.memory == "128Mi" and
        .resources.requests["ephemeral-storage"] == "256Mi" and
        .resources.limits.cpu == "500m" and
        .resources.limits.memory == "512Mi" and
        .resources.limits["ephemeral-storage"] == "2Gi" and
        .securityContext.allowPrivilegeEscalation == false and
        .securityContext.privileged == false and
        .securityContext.readOnlyRootFilesystem == true and
        .securityContext.runAsNonRoot == true and
        .securityContext.runAsUser == 10000 and
        (.securityContext.capabilities.drop | sort) == ["ALL"])
    ' "${RUNTIME_WORKDIR}/scan-pod.json" >/dev/null \
		|| fail "generated Trivy scan Pod security, image, or resource contract drifted"
}

write_synthetic_report() {
	local name="$1"
	local high_count="$2"
	local output="$3"
	local updated_at
	updated_at="$(date --utc +%Y-%m-%dT%H:%M:%SZ)"
	jq --null-input \
		--arg name "${name}" \
		--arg updated_at "${updated_at}" \
		--argjson high_count "${high_count}" '
      {
        apiVersion: "aquasecurity.github.io/v1alpha1",
        kind: "VulnerabilityReport",
        metadata: {
          name: $name,
          namespace: "models",
          labels: {
            "app.kubernetes.io/managed-by": "trivy-runtime-test",
            "trivy-operator.resource.kind": "Pod",
            "trivy-operator.resource.name": "synthetic-runtime-fixture",
            "trivy-operator.container.name": "fixture"
          }
        },
        report: {
          updateTimestamp: $updated_at,
          scanner: {name: "fgentic-runtime-probe", vendor: "fgentic", version: "1"},
          registry: {server: "registry.invalid"},
          artifact: {
            repository: "fgentic/runtime-synthetic",
            digest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
          },
          os: {family: "synthetic", name: "1"},
          summary: {
            criticalCount: 0,
            highCount: $high_count,
            mediumCount: 0,
            lowCount: 0,
            unknownCount: 0
          },
          vulnerabilities: [range(0; $high_count) | {
            vulnerabilityID: ("FGENTIC-SYNTHETIC-" + ((. + 1) | tostring)),
            resource: "synthetic-package",
            installedVersion: "1",
            fixedVersion: "2",
            severity: "HIGH",
            title: "Synthetic runtime metric probe",
            publishedDate: "2026-01-01T00:00:00Z",
            lastModifiedDate: "2026-01-01T00:00:00Z"
          }]
        }
      }
    ' >"${output}"
}

start_metrics_port_forward() {
	local forwarded_port=""
	kubectl --namespace "${OPERATOR_NAMESPACE}" port-forward service/trivy-operator :80 \
		>"${RUNTIME_WORKDIR}/port-forward.log" 2>&1 &
	RUNTIME_PORT_FORWARD_PID=$!
	for _ in {1..30}; do
		if ! kill -0 "${RUNTIME_PORT_FORWARD_PID}" 2>/dev/null; then
			cat "${RUNTIME_WORKDIR}/port-forward.log" >&2
			fail "Trivy Operator metrics port-forward exited early"
		fi
		forwarded_port="$(sed -n 's/^Forwarding from 127\.0\.0\.1:\([0-9][0-9]*\) ->.*/\1/p' \
			"${RUNTIME_WORKDIR}/port-forward.log" | head -n 1)"
		[[ -n "${forwarded_port}" ]] && break
		sleep 1
	done
	[[ -n "${forwarded_port}" ]] || fail "Trivy Operator metrics port-forward did not become ready"
	RUNTIME_METRICS_PORT="${forwarded_port}"
}

synthetic_high_metric() {
	local port="$1"
	curl --fail --silent --show-error "http://127.0.0.1:${port}/metrics" \
		| awk '
      /^trivy_image_vulnerabilities\{/ &&
      /image_digest="sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"/ &&
      /image_repository="fgentic\/runtime-synthetic"/ &&
      /namespace="models"/ &&
      /severity="High"/ {print $NF}
    '
}

runtime_contract() {
	require_commands awk curl date diff docker helm jq k3d kubectl sed sha256sum sort yq
	docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

	local actual_digest actual_owner chart_path container_count container_name
	local expected_deployment_image image_volume_owner k3s_image kubeconfig node_command
	local operator_image_tag runtime_volume_output
	local k3s_arg
	local -a k3s_args=()
	local namespace expected_namespaces actual_namespaces
	local deployment_image rendered_rbac_count report_deadline reports real_reports fixture_digest
	local metrics_port metric_value report_names working_set startup_logs
	local max_active
	RUNTIME_CLUSTER_NAME="${TRIVY_OPERATOR_CLUSTER_NAME:-fgentic-trivy-${RANDOM}-$$}"
	[[ "${RUNTIME_CLUSTER_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,30}[a-z0-9]$ ]] \
		|| fail "invalid disposable cluster name: ${RUNTIME_CLUSTER_NAME}"
	[[ "${RUNTIME_CLUSTER_NAME}" != "fgentic" && "${RUNTIME_CLUSTER_NAME}" != "local" ]] \
		|| fail "refusing to target shared cluster name: ${RUNTIME_CLUSTER_NAME}"
	RUNTIME_NODE_NAME="k3d-${RUNTIME_CLUSTER_NAME}-server-0"
	RUNTIME_OWNER_TOKEN="${RUNTIME_CLUSTER_NAME}-${RANDOM}-$$"
	RUNTIME_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-trivy.XXXXXX")"
	kubeconfig="${RUNTIME_WORKDIR}/kubeconfig"

	cleanup() {
		local result=$?
		local cleanup_failed=false
		trap - EXIT INT TERM
		stop_runtime_processes
		if ((result != 0)); then
			collect_runtime_diagnostics
		fi

		if [[ "${RUNTIME_CLUSTER_OWNED}" == false ]] && runtime_owned_by_test; then
			# A failed k3d create can still leave its labeled server behind. The private token proves
			# that a partial cluster belongs to this invocation before cleanup is claimed.
			RUNTIME_CLUSTER_OWNED=true
		fi
		if [[ "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
			if ! runtime_owned_by_test; then
				echo "error: refusing cleanup because the server ownership label no longer matches" >&2
				cleanup_failed=true
			else
				k3d cluster delete "${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1 || true
				if ! cleanup_runtime_cluster_artifacts; then
					echo "error: disposable Trivy cluster cleanup did not complete" >&2
					cleanup_failed=true
				fi
			fi
		elif runtime_cluster_exists || runtime_artifacts_exist; then
			echo "error: unowned same-name k3d artifacts remain; refusing destructive cleanup" >&2
			cleanup_failed=true
		fi

		rm -rf "${RUNTIME_WORKDIR}"
		if [[ "${cleanup_failed}" == true ]]; then
			result=1
		elif [[ "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
			echo "==> Deleted isolated Trivy cluster ${RUNTIME_CLUSTER_NAME} and exact runtime artifacts"
		fi
		exit "${result}"
	}
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	if runtime_cluster_exists || runtime_artifacts_exist; then
		fail "same-name k3d resources already exist; refusing to mutate them: ${RUNTIME_CLUSTER_NAME}"
	fi

	chart_path="${RUNTIME_WORKDIR}/trivy-operator-${CHART_VERSION}.tgz"
	echo "==> Downloading and verifying Trivy Operator chart ${CHART_VERSION}"
	curl --fail --location --retry 3 --retry-all-errors --silent --show-error \
		"${CHART_URL}" --output "${chart_path}"
	actual_digest="$(sha256sum "${chart_path}" | awk '{print $1}')"
	printf '%s  %s\n' "${actual_digest}" "${chart_path##*/}" >"${RUNTIME_WORKDIR}/chart.sha256"
	[[ "${actual_digest}" == "${CHART_DIGEST}" ]] \
		|| fail "Trivy chart digest is ${actual_digest}, expected ${CHART_DIGEST}"

	k3s_image="$(yq -r '.image' "${K3D_CONFIG}")"
	[[ "${k3s_image}" == rancher/k3s:v*-k3s* ]] || fail "invalid pinned k3s image: ${k3s_image}"
	for k3s_arg in "${RUNTIME_K3S_ARGS[@]}"; do
		k3s_args+=(--k3s-arg "${k3s_arg}")
	done
	echo "==> Creating isolated no-LB k3d cluster ${RUNTIME_CLUSTER_NAME}"
	k3d cluster create "${RUNTIME_CLUSTER_NAME}" \
		--servers 1 --agents 0 --image "${k3s_image}" --no-lb \
		"${k3s_args[@]}" \
		--runtime-label "${OWNER_LABEL}=${RUNTIME_OWNER_TOKEN}@server:*" \
		--kubeconfig-update-default=false --kubeconfig-switch-context=false --timeout 3m
	actual_owner="$(docker inspect --format "{{ index .Config.Labels \"${OWNER_LABEL}\" }}" \
		"${RUNTIME_NODE_NAME}")"
	[[ "${actual_owner}" == "${RUNTIME_OWNER_TOKEN}" ]] \
		|| fail "created server lacks the private ownership token; refusing to claim cleanup ownership"
	RUNTIME_CLUSTER_OWNED=true
	node_command="$(docker inspect --format '{{json .Config.Cmd}}' "${RUNTIME_NODE_NAME}")"
	rg --fixed-strings '"--disable-network-policy"' <<<"${node_command}" >/dev/null \
		|| fail "running Trivy k3s node did not disable the local NetworkPolicy controller"
	container_count="$(runtime_container_ids | wc -l | tr -d ' ')"
	[[ "${container_count}" == "1" ]] \
		|| fail "disposable cluster created unexpected node or load-balancer containers"
	container_name="$(docker inspect --format '{{.Name}}' "${RUNTIME_NODE_NAME}")"
	[[ "${container_name}" == "/${RUNTIME_NODE_NAME}" ]] \
		|| fail "disposable cluster server identity drifted"
	runtime_volume_exists || fail "disposable cluster did not create its expected owned image volume"
	image_volume_owner="$(docker volume inspect \
		--format '{{ index .Labels "app" }}/{{ index .Labels "k3d.cluster" }}' \
		"k3d-${RUNTIME_CLUSTER_NAME}-images")"
	[[ "${image_volume_owner}" == "k3d/${RUNTIME_CLUSTER_NAME}" ]] \
		|| fail "disposable cluster image volume ownership labels drifted"
	runtime_volume_output="$(docker inspect "${RUNTIME_NODE_NAME}" \
		| jq -r '.[0].Mounts[] | select(.Type == "volume") | .Name')"
	[[ -n "${runtime_volume_output}" ]] || fail "disposable server volume inventory is empty"
	mapfile -t RUNTIME_VOLUME_NAMES <<<"${runtime_volume_output}"

	k3d kubeconfig get "${RUNTIME_CLUSTER_NAME}" >"${kubeconfig}"
	export KUBECONFIG="${kubeconfig}"
	kubectl wait --for=condition=Ready nodes --all --timeout=2m >/dev/null

	for namespace in "${TARGET_NAMESPACES[@]}" "${OUT_OF_SCOPE_NAMESPACE}" \
		"${OPERATOR_NAMESPACE}"; do
		kubectl create namespace "${namespace}" >/dev/null
	done
	expected_namespaces="$(printf '%s\n' "${TARGET_NAMESPACES[@]}" "${OUT_OF_SCOPE_NAMESPACE}" \
		"${OPERATOR_NAMESPACE}" | sort)"
	actual_namespaces="$(kubectl get namespaces --output json | jq -r '
      .items[].metadata.name |
      select(. != "default" and . != "kube-node-lease" and
        . != "kube-public" and . != "kube-system")
    ' | sort)"
	diff -u <(printf '%s\n' "${expected_namespaces}") \
		<(printf '%s\n' "${actual_namespaces}") >/dev/null \
		|| fail "disposable cluster custom namespace set drifted"

	echo "==> Applying reviewed CRDs, repository RBAC, NetworkPolicy, and Helm values"
	helm show crds "${chart_path}" >"${RUNTIME_WORKDIR}/crds.yaml"
	kubectl apply --server-side --field-manager=fgentic-trivy-runtime \
		--filename "${RUNTIME_WORKDIR}/crds.yaml" >/dev/null
	kubectl wait --for=condition=Established customresourcedefinitions --all --timeout=2m >/dev/null
	kubectl apply --filename "${RBAC}" >/dev/null
	kubectl apply --filename "${NETWORK_POLICY}" >/dev/null
	kubectl apply --filename "${SCAN_QUOTA}" >/dev/null
	wait_for_scan_quota_usage 0
	yq '.spec.values' "${HELM_RELEASE}" >"${RUNTIME_WORKDIR}/values.yaml"
	helm template trivy-operator "${chart_path}" --namespace "${OPERATOR_NAMESPACE}" \
		--values "${RUNTIME_WORKDIR}/values.yaml" >"${RUNTIME_WORKDIR}/rendered.yaml"
	mkdir "${RUNTIME_WORKDIR}/postrender"
	cp "${RUNTIME_WORKDIR}/rendered.yaml" "${RUNTIME_WORKDIR}/postrender/rendered.yaml"
	HELM_RELEASE_PATH="${HELM_RELEASE}" yq --null-input '
      .apiVersion = "kustomize.config.k8s.io/v1beta1" |
      .kind = "Kustomization" |
      .resources = ["rendered.yaml"] |
      .patches = load(strenv(HELM_RELEASE_PATH)).spec.postRenderers[0].kustomize.patches
    ' >"${RUNTIME_WORKDIR}/postrender/kustomization.yaml"
	cp "${RUNTIME_WORKDIR}/postrender/kustomization.yaml" \
		"${RUNTIME_WORKDIR}/postrender-kustomization.yaml"
	kubectl kustomize "${RUNTIME_WORKDIR}/postrender" \
		>"${RUNTIME_WORKDIR}/rendered-filtered.yaml"
	rendered_rbac_count="$(
		yq -o=json '.' "${RUNTIME_WORKDIR}/rendered-filtered.yaml" \
			| jq --slurp '[.[] | select(
          .kind == "ServiceAccount" or
          .kind == "Role" or
          .kind == "RoleBinding" or
          .kind == "ClusterRole" or
          .kind == "ClusterRoleBinding"
        )] | length'
	)"
	[[ "${rendered_rbac_count}" == "0" ]] \
		|| fail "post-rendered chart contains ${rendered_rbac_count} chart-owned RBAC objects"
	kubectl apply --filename "${RUNTIME_WORKDIR}/rendered-filtered.yaml" >/dev/null
	for report_name in aggregate-config-audit-reports-view \
		aggregate-exposed-secret-reports-view aggregate-vulnerability-reports-view; do
		if kubectl get clusterrole "${report_name}" >/dev/null 2>&1; then
			fail "Flux post-render simulation left aggregate role ${report_name} installed"
		fi
	done
	kubectl --namespace "${OPERATOR_NAMESPACE}" rollout status deployment/trivy-operator --timeout=5m
	deployment_image="$(kubectl --namespace "${OPERATOR_NAMESPACE}" get deployment trivy-operator \
		--output jsonpath='{.spec.template.spec.containers[0].image}')"
	operator_image_tag="$(yq -r '.spec.values.image.tag' "${HELM_RELEASE}")"
	expected_deployment_image="mirror.gcr.io/aquasec/trivy-operator:${operator_image_tag}"
	[[ "${deployment_image}" == "${expected_deployment_image}" ]] \
		|| fail "running operator image drifted from the HelmRelease value"
	startup_logs="$(kubectl --namespace "${OPERATOR_NAMESPACE}" logs \
		deployment/trivy-operator --all-containers)"
	if rg --ignore-case \
		'forbidden|permission denied|unauthorized|cannot (get|list|watch|create|update|patch|delete)' \
		<<<"${startup_logs}"; then
		fail "Trivy Operator startup logs contain RBAC authorization errors"
	fi

	assert_service_account_access yes list pods models
	assert_service_account_access yes create vulnerabilityreports.aquasecurity.github.io models
	assert_service_account_access yes list jobs.batch "${OPERATOR_NAMESPACE}"
	assert_service_account_access yes create jobs.batch "${OPERATOR_NAMESPACE}"
	assert_service_account_access no create vulnerabilityreports.aquasecurity.github.io \
		"${OPERATOR_NAMESPACE}"
	assert_service_account_access no list pods "${OUT_OF_SCOPE_NAMESPACE}"
	assert_service_account_access no create vulnerabilityreports.aquasecurity.github.io \
		"${OUT_OF_SCOPE_NAMESPACE}"

	cat >"${RUNTIME_WORKDIR}/fixtures.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: trivy-fixture-a
  namespace: models
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 10000
    runAsGroup: 10000
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: fixture
      image: ${FIXTURE_IMAGE}
      imagePullPolicy: IfNotPresent
      command: [/bin/sh, -c, "sleep 900"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
        readOnlyRootFilesystem: true
      resources:
        requests: {cpu: 5m, memory: 8Mi}
        limits: {cpu: 50m, memory: 32Mi}
---
apiVersion: v1
kind: Pod
metadata:
  name: trivy-fixture-b
  namespace: models
spec:
  automountServiceAccountToken: false
  securityContext:
    runAsNonRoot: true
    runAsUser: 10000
    runAsGroup: 10000
    seccompProfile: {type: RuntimeDefault}
  containers:
    - name: fixture
      image: ${FIXTURE_IMAGE}
      imagePullPolicy: IfNotPresent
      command: [/bin/sh, -c, "sleep 900"]
      securityContext:
        allowPrivilegeEscalation: false
        capabilities: {drop: [ALL]}
        readOnlyRootFilesystem: true
      resources:
        requests: {cpu: 5m, memory: 8Mi}
        limits: {cpu: 50m, memory: 32Mi}
EOF
	create_scan_quota_hold
	: >"${RUNTIME_WORKDIR}/scan-concurrency.log"
	: >"${RUNTIME_WORKDIR}/scan-working-set.log"
	monitor_scan_job_concurrency &
	RUNTIME_SCAN_MONITOR_PID=$!
	kubectl apply --filename "${RUNTIME_WORKDIR}/fixtures.yaml" >/dev/null
	wait_for_operator_quota_denial
	release_scan_quota_hold

	fixture_digest="${FIXTURE_IMAGE##*@}"
	report_deadline=$((SECONDS + 600))
	real_reports=""
	while ((SECONDS < report_deadline)); do
		[[ ! -e "${RUNTIME_WORKDIR}/scan-concurrency-violation" ]] \
			|| fail "more than one Trivy scan Job was active concurrently"
		reports="$(kubectl --namespace models get vulnerabilityreports.aquasecurity.github.io \
			--output json 2>/dev/null || printf '{"items":[]}')"
		real_reports="$(jq --compact-output --arg digest "${fixture_digest}" '
        [.items[] | select(
          ((.metadata.ownerReferences // []) |
            any(.kind == "Pod" and (.name == "trivy-fixture-a" or .name == "trivy-fixture-b"))) and
          .metadata.labels["trivy-operator.resource.kind"] == "Pod" and
          (.metadata.labels["trivy-operator.resource.name"] == "trivy-fixture-a" or
           .metadata.labels["trivy-operator.resource.name"] == "trivy-fixture-b") and
          .metadata.labels["trivy-operator.container.name"] == "fixture" and
          .report.artifact.digest == $digest and
          (.report.scanner.name | type == "string" and length > 0) and
          (.report.scanner.vendor | type == "string" and length > 0) and
          (.report.scanner.version | type == "string" and length > 0) and
          (.report.updateTimestamp | type == "string" and
            test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T")) and
          (.report.vulnerabilities | type == "array") and
          (.report.summary |
            all(.criticalCount, .highCount, .mediumCount, .lowCount, .unknownCount;
              type == "number" and . >= 0))
        )] |
        select(
          length == 2 and
          (map(.metadata.labels["trivy-operator.resource.name"]) | sort) ==
            ["trivy-fixture-a", "trivy-fixture-b"]
        )
      ' <<<"${reports}")"
		[[ -z "${real_reports}" ]] || break
		sleep 5
	done
	[[ -n "${real_reports}" ]] \
		|| fail "both digest-pinned fixtures did not produce schema-valid VulnerabilityReports within 10m"
	report_names="$(jq -r 'map(.metadata.name) | sort | join(",")' <<<"${real_reports}")"
	kubectl --namespace models wait --for=condition=Ready pod/trivy-fixture-a \
		pod/trivy-fixture-b --timeout=3m >/dev/null
	touch "${RUNTIME_WORKDIR}/stop-scan-monitor"
	wait "${RUNTIME_SCAN_MONITOR_PID}"
	RUNTIME_SCAN_MONITOR_PID=""
	[[ -e "${RUNTIME_WORKDIR}/scan-job-observed" ]] \
		|| fail "no active Trivy scan Job was observed"
	[[ ! -e "${RUNTIME_WORKDIR}/scan-concurrency-violation" ]] \
		|| fail "more than one Trivy scan Job was active concurrently"
	[[ ! -e "${RUNTIME_WORKDIR}/scan-memory-violation" ]] \
		|| fail "operator working-set memory reached the 256Mi limit during scanning"
	assert_scan_pod_contract
	max_active="$(awk -F'[ =]' '/ active=/ {if ($3 > max) max=$3} END {print max + 0}' \
		"${RUNTIME_WORKDIR}/scan-concurrency.log")"
	[[ "${max_active}" == "1" ]] || fail "maximum observed active Trivy Jobs was ${max_active}"
	working_set="$(awk -F= '/ workingSetBytes=/ {if ($2 > max) max=$2} END {print max + 0}' \
		"${RUNTIME_WORKDIR}/scan-working-set.log")"
	[[ "${working_set}" =~ ^[1-9][0-9]*$ ]] \
		|| fail "kubelet summary did not expose operator workingSetBytes during an active scan"
	((working_set < OPERATOR_MEMORY_LIMIT_BYTES)) \
		|| fail "operator workingSetBytes ${working_set} reached the 256Mi limit during scanning"

	echo "==> Proving live findings metrics change from one to two HIGH findings"
	start_metrics_port_forward
	metrics_port="${RUNTIME_METRICS_PORT}"
	write_synthetic_report synthetic-high-1 1 "${RUNTIME_WORKDIR}/synthetic-high-1.json"
	kubectl apply --filename "${RUNTIME_WORKDIR}/synthetic-high-1.json" >/dev/null
	metric_value=""
	for _ in {1..30}; do
		metric_value="$(synthetic_high_metric "${metrics_port}" || true)"
		[[ "${metric_value}" == "1" ]] && break
		sleep 1
	done
	[[ "${metric_value}" == "1" ]] \
		|| fail "live Trivy metrics did not expose the synthetic HIGH count of one"
	kubectl --namespace models delete vulnerabilityreport synthetic-high-1 --wait=true >/dev/null
	write_synthetic_report synthetic-high-2 2 "${RUNTIME_WORKDIR}/synthetic-high-2.json"
	kubectl apply --filename "${RUNTIME_WORKDIR}/synthetic-high-2.json" >/dev/null
	metric_value=""
	for _ in {1..30}; do
		metric_value="$(synthetic_high_metric "${metrics_port}" || true)"
		[[ "${metric_value}" == "2" ]] && break
		sleep 1
	done
	[[ "${metric_value}" == "2" ]] \
		|| fail "live Trivy metrics did not change to the synthetic HIGH count of two"

	startup_logs="$(kubectl --namespace "${OPERATOR_NAMESPACE}" logs \
		deployment/trivy-operator --all-containers)"
	if ! rg --ignore-case 'exceeded quota: trivy-scan-serialization' \
		<<<"${startup_logs}" >/dev/null; then
		fail "the controlled quota hold did not reject an operator scan Job"
	fi
	if rg --invert-match --ignore-case 'exceeded quota: trivy-scan-serialization' \
		<<<"${startup_logs}" \
		| rg --ignore-case \
			'forbidden|permission denied|unauthorized|cannot (get|list|watch|create|update|patch|delete)'; then
		fail "Trivy Operator logs contain an authorization failure unrelated to scan serialization"
	fi

	echo "Trivy Operator runtime contract passed (${RUNTIME_CLUSTER_NAME}; reports ${report_names}; max active Jobs ${max_active}; max workingSetBytes ${working_set})"
}

static_contract
if ${runtime}; then
	runtime_contract
fi
