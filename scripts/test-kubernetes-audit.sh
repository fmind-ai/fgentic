#!/usr/bin/env bash
# Validate the local k3s API-audit contract. --runtime creates an owned, no-port k3d cluster and
# never reads from or mutates the shared local cluster.
# shellcheck disable=SC2016 # yq bindings and child-shell fixture variables are intentionally literal
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
readonly ROOT_DIR
readonly POLICY="${ROOT_DIR}/infra/k3d-audit-policy.yaml"
readonly K3D_CONFIG="${ROOT_DIR}/infra/k3d-config.yaml"
readonly AUDIT_LOG_PATH="/var/log/kubernetes/audit/audit.log"
readonly OWNER_LABEL="dev.fgentic.audit-test.owner"
readonly ROTATION_BATCH_SIZE=256
readonly ROTATION_MAX_BATCHES=10
readonly ROTATION_PARALLELISM=16
RUNTIME_CLUSTER_NAME=""
RUNTIME_CLUSTER_OWNED=false
RUNTIME_NODE_NAME=""
RUNTIME_WORKDIR=""
runtime=false

case "${1:-}" in
	"") ;;
	--runtime) runtime=true ;;
	*)
		echo "usage: ${0##*/} [--runtime]" >&2
		exit 2
		;;
esac

normalized_resources() {
	local rule_index="$1"
	yq -r ".rules[${rule_index}].resources[] as \$rule |
    \$rule.resources[] | ((\$rule.group // \"\") + \"|\" + .)" "${POLICY}" | sort
}

static_contract() {
	require_commands diff rg yq

	yq -e '
    .apiVersion == "audit.k8s.io/v1" and
    .kind == "Policy" and
    .omitManagedFields == true and
    (.omitStages | length) == 1 and
    .omitStages[0] == "RequestReceived" and
    (.rules | length) == 3 and
    .rules[0].level == "Metadata" and
    .rules[1].level == "Metadata" and
    .rules[2].level == "None"
  ' "${POLICY}" >/dev/null \
		|| fail "audit policy must be Metadata-only, omit duplicate receive events, and drop unmatched traffic"
	yq -e '([.rules[].level | select(. != "Metadata" and . != "None")] | length) == 0' \
		"${POLICY}" >/dev/null || fail "audit policy contains a body-capturing level"
	local write_verbs
	write_verbs="$(yq -r '.rules[0].verbs | sort | join(",")' "${POLICY}")"
	[[ "${write_verbs}" == "create,delete,deletecollection,patch,update" ]] \
		|| fail "audit write verbs drifted"
	local read_verbs catchall_keys
	read_verbs="$(yq -r '.rules[1].verbs | join(",")' "${POLICY}")"
	[[ "${read_verbs}" == "get" ]] \
		|| fail "audit read rule must select only one-object gets"
	catchall_keys="$(yq -r '.rules[2] | keys | join(",")' "${POLICY}")"
	[[ "${catchall_keys}" == "level" ]] \
		|| fail "the final None rule must remain an unfiltered catch-all"
	local rule_index rule_keys resource_keys
	for rule_index in 0 1; do
		rule_keys="$(yq -r ".rules[${rule_index}] | keys | sort | join(\",\")" "${POLICY}")"
		[[ "${rule_keys}" == "level,resources,verbs" ]] \
			|| fail "audit rule ${rule_index} contains an unexpected selector"
		resource_keys="$(yq -r ".rules[${rule_index}].resources[] | keys | sort | join(\",\")" \
			"${POLICY}" | sort -u)"
		[[ "${resource_keys}" == "group,resources" ]] \
			|| fail "audit rule ${rule_index} contains an unexpected resource selector"
	done

	local expected_write_resources expected_read_resources
	expected_write_resources="$(
		sort <<'RESOURCES'
|configmaps
|pods/exec
|secrets
admissionregistration.k8s.io|validatingadmissionpolicies
admissionregistration.k8s.io|validatingadmissionpolicybindings
helm.toolkit.fluxcd.io|helmreleases
kagent.dev|agents
kustomize.toolkit.fluxcd.io|kustomizations
networking.k8s.io|networkpolicies
RESOURCES
	)"
	expected_read_resources="$(grep -Fv '|pods/exec' <<<"${expected_write_resources}")"
	local actual_write_resources actual_read_resources
	actual_write_resources="$(normalized_resources 0)"
	actual_read_resources="$(normalized_resources 1)"
	diff -u <(printf '%s\n' "${expected_write_resources}") \
		<(printf '%s\n' "${actual_write_resources}") >/dev/null \
		|| fail "audit write-resource allowlist drifted"
	diff -u <(printf '%s\n' "${expected_read_resources}") \
		<(printf '%s\n' "${actual_read_resources}") >/dev/null \
		|| fail "audit read-resource allowlist drifted"

	yq -e '[.files[] | select(
    .source == "k3d-audit-policy.yaml" and
    .destination == "/etc/fgentic/audit-policy.yaml" and
    (.nodeFilters | join(",")) == "server:*")] | length == 1' \
		"${K3D_CONFIG}" >/dev/null || fail "k3d does not inject the API audit policy"
	yq -e '[.files[] | select(
    .destination == "/var/log/kubernetes/audit/.fgentic-managed" and
    (.nodeFilters | join(",")) == "server:*")] | length == 1' \
		"${K3D_CONFIG}" >/dev/null || fail "k3d does not create the API audit log directory"
	local expected_args actual_args
	expected_args="$(
		cat <<'ARGS'
--kube-apiserver-arg=audit-log-maxage=7
--kube-apiserver-arg=audit-log-maxbackup=3
--kube-apiserver-arg=audit-log-maxsize=10
--kube-apiserver-arg=audit-log-path=/var/log/kubernetes/audit/audit.log
--kube-apiserver-arg=audit-policy-file=/etc/fgentic/audit-policy.yaml
ARGS
	)"
	actual_args="$(yq -r '.options.k3s.extraArgs[] |
    select(.arg | test("^--kube-apiserver-arg=audit-")) |
    select((.nodeFilters | join(",")) == "server:*") | .arg' "${K3D_CONFIG}" | sort)"
	diff -u <(printf '%s\n' "${expected_args}") <(printf '%s\n' "${actual_args}") >/dev/null \
		|| fail "k3d must activate the exact bounded API audit flags on every server"
	yq -e '
    [.options.k3s.extraArgs[] | select(.arg == "--disable-network-policy")] as $args |
    (
      ($args | length) == 1 and
      ($args | .[0].nodeFilters | length) == 1 and
      ($args | .[0].nodeFilters[0]) == "server:*" and
      ($args | .[0] | keys | length) == 2 and
      ($args | .[0] | has("arg")) and
      ($args | .[0] | has("nodeFilters"))
    )
  ' "${K3D_CONFIG}" >/dev/null \
		|| fail "k3d must disable the failed local NetworkPolicy controller on every server"

	echo "Kubernetes API audit static contract passed"
}

runtime_contract() {
	require_commands docker jq k3d kubectl xargs yq
	docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

	local owner_token kubeconfig
	RUNTIME_CLUSTER_NAME="${K3D_AUDIT_CLUSTER_NAME:-fgentic-api-audit-${RANDOM}-$$}"
	owner_token="${RUNTIME_CLUSTER_NAME}-${RANDOM}"
	RUNTIME_NODE_NAME="k3d-${RUNTIME_CLUSTER_NAME}-server-0"
	RUNTIME_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-api-audit.XXXXXX")"
	kubeconfig="${RUNTIME_WORKDIR}/kubeconfig"

	cleanup() {
		local result=$?
		local cleanup_failed=false
		local remaining_container_ids=""
		trap - EXIT INT TERM
		if [[ "${KEEP_K3D_AUDIT_CLUSTER:-0}" == "1" && "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
			echo "==> Keeping k3d cluster ${RUNTIME_CLUSTER_NAME}; kubeconfig is ${RUNTIME_WORKDIR}/kubeconfig"
		else
			if [[ "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
				if ! k3d cluster delete "${RUNTIME_CLUSTER_NAME}" >/dev/null; then
					echo "error: failed to delete owned k3d cluster ${RUNTIME_CLUSTER_NAME}" >&2
					cleanup_failed=true
				fi
				if k3d cluster get "${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1; then
					echo "error: owned k3d cluster still exists after cleanup: ${RUNTIME_CLUSTER_NAME}" >&2
					cleanup_failed=true
				fi
				if docker inspect "${RUNTIME_NODE_NAME}" >/dev/null 2>&1; then
					echo "error: owned k3d server still exists after cleanup: ${RUNTIME_NODE_NAME}" >&2
					cleanup_failed=true
				fi
				if ! remaining_container_ids="$(docker ps --all \
					--filter "label=k3d.cluster=${RUNTIME_CLUSTER_NAME}" --quiet)"; then
					echo "error: failed to verify owned k3d containers were removed" >&2
					cleanup_failed=true
				elif [[ -n "${remaining_container_ids}" ]]; then
					echo "error: owned k3d containers still exist after cleanup: ${remaining_container_ids}" >&2
					cleanup_failed=true
				fi
				if docker network inspect "k3d-${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1; then
					echo "error: owned k3d network still exists after cleanup: k3d-${RUNTIME_CLUSTER_NAME}" >&2
					cleanup_failed=true
				fi
				if docker volume inspect "k3d-${RUNTIME_CLUSTER_NAME}-images" >/dev/null 2>&1; then
					echo "error: owned k3d image volume still exists after cleanup: k3d-${RUNTIME_CLUSTER_NAME}-images" >&2
					cleanup_failed=true
				fi
			fi
			if [[ "${cleanup_failed}" == true ]]; then
				echo "error: preserving cleanup evidence in ${RUNTIME_WORKDIR}" >&2
				result=1
			else
				rm -rf "${RUNTIME_WORKDIR}"
			fi
		fi
		exit "${result}"
	}
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	if k3d cluster get "${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1; then
		fail "k3d cluster already exists; refusing to mutate it: ${RUNTIME_CLUSTER_NAME}"
	fi

	cp "${POLICY}" "${RUNTIME_WORKDIR}/k3d-audit-policy.yaml"
	CLUSTER_NAME="${RUNTIME_CLUSTER_NAME}" AUDIT_OWNER_LABEL="${OWNER_LABEL}" \
		AUDIT_OWNER_TOKEN="${owner_token}" \
		yq '
      .metadata.name = strenv(CLUSTER_NAME) |
      del(.ports) |
      .options.k3d.disableLoadbalancer = true |
      .options.kubeconfig.updateDefaultKubeconfig = false |
      .options.kubeconfig.switchCurrentContext = false |
      (.options.k3s.extraArgs[] |
        select(.arg == "--kube-apiserver-arg=audit-log-maxsize=10") | .arg) =
        "--kube-apiserver-arg=audit-log-maxsize=1" |
      .options.runtime.labels = ((.options.runtime.labels // []) + [{
        "label": (strenv(AUDIT_OWNER_LABEL) + "=" + strenv(AUDIT_OWNER_TOKEN)),
        "nodeFilters": ["server:*"]
      }])
    ' "${K3D_CONFIG}" >"${RUNTIME_WORKDIR}/k3d-config.yaml"

	echo "==> Creating isolated no-port k3d cluster ${RUNTIME_CLUSTER_NAME}"
	k3d cluster create --config "${RUNTIME_WORKDIR}/k3d-config.yaml"
	local actual_owner
	actual_owner="$(docker inspect --format "{{ index .Config.Labels \"${OWNER_LABEL}\" }}" "${RUNTIME_NODE_NAME}")"
	[[ "${actual_owner}" == "${owner_token}" ]] \
		|| fail "created node does not carry the expected ownership label; leaving it untouched"
	RUNTIME_CLUSTER_OWNED=true

	k3d kubeconfig get "${RUNTIME_CLUSTER_NAME}" >"${kubeconfig}"
	KUBECONFIG="${kubeconfig}"
	export KUBECONFIG
	kubectl wait --for=condition=Ready node --all --timeout=2m >/dev/null

	local node_command
	node_command="$(docker inspect --format '{{json .Config.Cmd}}' "${RUNTIME_NODE_NAME}")"
	for expected_arg in \
		"audit-log-maxage=7" \
		"audit-log-maxbackup=3" \
		"audit-log-maxsize=1" \
		"audit-log-path=${AUDIT_LOG_PATH}" \
		"audit-policy-file=/etc/fgentic/audit-policy.yaml"; do
		grep -Fq "\"--kube-apiserver-arg=${expected_arg}\"" <<<"${node_command}" \
			|| fail "running k3s node is missing --kube-apiserver-arg=${expected_arg}"
	done
	rg --fixed-strings '"--disable-network-policy"' <<<"${node_command}" >/dev/null \
		|| fail "running k3s node did not disable the local NetworkPolicy controller"

	local namespace rotation_name
	namespace="fgentic-api-audit-probe"
	rotation_name="rotation-probe"
	kubectl create namespace "${namespace}" >/dev/null
	kubectl --namespace "${namespace}" create configmap "${rotation_name}" \
		--from-literal=probe=initial >/dev/null

	# Exercise create, update, and delete watches. On this host an enabled kube-router controller
	# repeatedly emits the rejected iptables ruleset; the disabled controller must remain silent.
	local policy_name="controller-disabled-probe"
	local policy_revision
	for policy_revision in 1 2 3 4; do
		kubectl apply --filename - >/dev/null <<EOF
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: ${policy_name}
  namespace: ${namespace}
  annotations:
    fgentic.dev/probe-revision: "${policy_revision}"
spec:
  podSelector: {}
  policyTypes: [Ingress, Egress]
EOF
	done
	kubectl --namespace "${namespace}" delete networkpolicy "${policy_name}" >/dev/null

	# The disposable runtime lowers only maxsize to 1 MiB. Emit bounded body-suppressed Metadata events
	# until kube-apiserver performs a real rotation; the canonical config remains 10 MiB/3/7 days.
	local backup_count=0 batch start end value patch_command
	export K3D_AUDIT_ROTATION_NAMESPACE="${namespace}"
	export K3D_AUDIT_ROTATION_CONFIGMAP="${rotation_name}"
	# The child bash, not this parent, expands its positional argument and exported kubectl target.
	patch_command='
        value="$1"
        kubectl --namespace "${K3D_AUDIT_ROTATION_NAMESPACE}" patch configmap \
          "${K3D_AUDIT_ROTATION_CONFIGMAP}" --type=merge \
          --patch "{\"metadata\":{\"annotations\":{\"fgentic.dev/rotation-probe\":\"${value}\"}}}" \
          >/dev/null
      '
	for ((batch = 1; batch <= ROTATION_MAX_BATCHES; batch++)); do
		start=$(((batch - 1) * ROTATION_BATCH_SIZE + 1))
		end=$((batch * ROTATION_BATCH_SIZE))
		for ((value = start; value <= end; value++)); do
			printf '%s\n' "${value}"
		done | xargs -P "${ROTATION_PARALLELISM}" -n 1 bash -c "${patch_command}" _
		backup_count="$(docker exec "${RUNTIME_NODE_NAME}" sh -c '
        set -- /var/log/kubernetes/audit/audit-*.log
        if [ -e "$1" ]; then echo "$#"; else echo 0; fi
      ')"
		if ((backup_count > 0)); then
			break
		fi
	done
	((backup_count >= 1 && backup_count <= 3)) \
		|| fail "audit log did not rotate within the bounded patch budget or exceeded three backups"

	local rotation_audit_file="${RUNTIME_WORKDIR}/rotation-audit.jsonl"
	docker exec "${RUNTIME_NODE_NAME}" sh -c \
		'cat /var/log/kubernetes/audit/audit.log /var/log/kubernetes/audit/audit-*.log' \
		>"${rotation_audit_file}"
	jq -e --arg namespace "${namespace}" --arg name "${rotation_name}" \
		--argjson minimum_patches "${ROTATION_BATCH_SIZE}" '
    [.[] | select(
      .objectRef.resource == "configmaps" and
      .objectRef.namespace == $namespace and
      .objectRef.name == $name and
      (.verb == "create" or .verb == "patch")
    )] as $events |
    any($events[]; .verb == "create") and
    ([$events[] | select(.verb == "patch")] | length) >= $minimum_patches and
    all($events[];
      .level == "Metadata" and
      (has("requestObject") | not) and
      (has("responseObject") | not))
  ' --slurp "${rotation_audit_file}" >/dev/null \
		|| fail "rotated ConfigMap events did not retain the body-suppressed Metadata contract"

	local secret_name secret_sentinel
	secret_name="audit-probe"
	secret_sentinel="FGENTIC_API_AUDIT_SECRET_SENTINEL_${RANDOM}_$$"
	kubectl --namespace "${namespace}" create secret generic "${secret_name}" \
		--from-literal="value=${secret_sentinel}" >/dev/null
	kubectl --namespace "${namespace}" annotate secret "${secret_name}" \
		"fgentic.dev/audit-probe=${owner_token}" >/dev/null
	kubectl --namespace "${namespace}" get secret "${secret_name}" >/dev/null

	local audit_file="${RUNTIME_WORKDIR}/audit.jsonl"
	local records_ready=false
	for _ in {1..30}; do
		docker exec "${RUNTIME_NODE_NAME}" sh -c \
			'cat /var/log/kubernetes/audit/audit.log /var/log/kubernetes/audit/audit-*.log' \
			>"${audit_file}"
		if jq -e --arg namespace "${namespace}" --arg name "${secret_name}" '
        any(.[];
          .level == "Metadata" and
          .stage == "ResponseComplete" and
          .verb == "patch" and
          (.objectRef.apiGroup // "") == "" and
          .objectRef.resource == "secrets" and
          .objectRef.namespace == $namespace and
          .objectRef.name == $name and
          .responseStatus.code == 200 and
          (.user.username | length > 0)) and
        any(.[];
          .level == "Metadata" and
          .stage == "ResponseComplete" and
          .verb == "get" and
          (.objectRef.apiGroup // "") == "" and
          .objectRef.resource == "secrets" and
          .objectRef.namespace == $namespace and
          .objectRef.name == $name and
          .responseStatus.code == 200)
      ' --slurp "${audit_file}" >/dev/null; then
			records_ready=true
			break
		fi
		sleep 1
	done
	[[ "${records_ready}" == true ]] || fail "Secret patch/read did not reach the k3s audit log"

	if grep -Fq "${secret_sentinel}" "${audit_file}"; then
		fail "Metadata audit log leaked Secret content"
	fi
	jq -e --arg namespace "${namespace}" --arg name "${secret_name}" '
    [.[] | select(
      .objectRef.resource == "secrets" and
      .objectRef.namespace == $namespace and
      .objectRef.name == $name
    )] |
    length >= 3 and
    all(.[];
      .level == "Metadata" and
      (has("requestObject") | not) and
      (has("responseObject") | not))
	' --slurp "${audit_file}" >/dev/null \
		|| fail "Secret events must remain body-suppressed Metadata records"

	local node_log="${RUNTIME_WORKDIR}/k3s-node.log"
	docker logs "${RUNTIME_NODE_NAME}" >"${node_log}" 2>&1 \
		|| fail "could not inspect the disposable k3s node log"
	if rg --quiet \
		'network_policy_controller\.go|sendmsg\(\) failed|Message too large|iptables-restore' \
		"${node_log}"; then
		fail "disabled NetworkPolicy controller emitted a kube-router reconciliation failure"
	fi

	echo "Kubernetes API audit and k3d controller-silence runtime contract passed (${RUNTIME_CLUSTER_NAME}, ${backup_count} rotated file(s))"
}

static_contract
if ${runtime}; then
	runtime_contract
fi
