#!/usr/bin/env bash
# Prove that a Flux-reconciled policy change reloads without replacing either participant Synapse.
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly FEDERATION_CLUSTER="fgentic-fed"

die() {
	echo "error: $*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1 (run 'mise install')"
}

synapse_pod_uid() {
	local namespace="$1"
	kubectl --namespace "${namespace}" get pods --output json |
		jq -er '
      [
        .items[] |
        select(any(.metadata.ownerReferences[]?;
          .apiVersion == "apps/v1" and .kind == "StatefulSet" and
          .name == "ess-synapse-main")) |
        .metadata.uid
      ] |
      if length == 1 then .[0] else
        error("expected exactly one ess-synapse-main pod")
      end
    '
}

assert_synapse_uids() {
	local phase="$1"
	local actual_a actual_b
	actual_a="$(synapse_pod_uid matrix)"
	actual_b="$(synapse_pod_uid matrix-b)"
	[ "${actual_a}" = "${SYNAPSE_A_UID}" ] ||
		die "homeserver A restarted during the ${phase} policy reconcile"
	[ "${actual_b}" = "${SYNAPSE_B_UID}" ] ||
		die "homeserver B restarted during the ${phase} policy reconcile"
}

for command in jq k3d kubectl; do
	require_command "${command}"
done

KUBECONFIG_FILE="$(mktemp "${TMPDIR:-/tmp}/fgentic-fed-policy-kubeconfig.XXXXXX")"
completed=false
cleanup() {
	local status=$?
	trap - EXIT
	if [ "${completed}" != true ]; then
		echo "Policy reload drill failed; deleting the disposable federation lab." >&2
		FGENTIC_FED_CLUSTER="${FEDERATION_CLUSTER}" \
			"${ROOT_DIR}/scripts/federation.sh" down >&2 ||
			echo "warning: federation lab cleanup did not complete" >&2
	fi
	rm -f "${KUBECONFIG_FILE}"
	exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

echo "Reconciling and proving the canonical deny policy..."
FGENTIC_FED_POLICY_PROBE=deny "${ROOT_DIR}/scripts/federation.sh" up
k3d kubeconfig get "${FEDERATION_CLUSTER}" >"${KUBECONFIG_FILE}"
export KUBECONFIG="${KUBECONFIG_FILE}"
SYNAPSE_A_UID="$(synapse_pod_uid matrix)"
SYNAPSE_B_UID="$(synapse_pod_uid matrix-b)"

echo "Reconciling the ephemeral allow policy through Flux..."
FGENTIC_FED_POLICY_PROBE=allow "${ROOT_DIR}/scripts/federation.sh" up
assert_synapse_uids allow

echo "Restoring and proving the canonical deny policy through Flux..."
FGENTIC_FED_POLICY_PROBE=deny "${ROOT_DIR}/scripts/federation.sh" up
assert_synapse_uids deny

completed=true
echo "Federation policy reload proof passed; canonical deny remains running and Synapse pods were unchanged."
