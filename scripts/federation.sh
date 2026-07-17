#!/usr/bin/env bash
# Provider-free lifecycle for the disposable federation hardening lab. The generic demo installer
# owns the shared mechanics; this wrapper fixes the profile, cluster, and deletion guard.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly FEDERATION_CLUSTER="fgentic-fed"
readonly FEDERATION_SPLIT_CLUSTER_A="fgentic-fed-a"
readonly FEDERATION_SPLIT_CLUSTER_B="fgentic-fed-b"
readonly FEDERATION_SPLIT_RELAY_A="fgentic-fed-a-to-b"
readonly FEDERATION_SPLIT_RELAY_B="fgentic-fed-b-to-a"
readonly FEDERATION_SPLIT_RELAY_OWNER="fgentic.federation-split-relay.v1"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

usage() {
	cat <<'EOF'
usage: scripts/federation.sh up|status|stop|down

Creates the owned fgentic-fed k3d cluster with three provider-free Synapse homeservers:
  org-a.fgentic.localhost
  org-b.fgentic.localhost
  org-c.fgentic.localhost (denied control)

`up` reconciles the lab, proves a bidirectional A/B exchange plus C's rejection, and leaves the
cluster running for inspection. `stop` releases CPU/RAM while retaining the exact owned cluster
and image volume for same-mode reuse. `down` deletes only that ownership-labelled cluster and its
local images; run it before switching between canonical and constrained capacity.

Environment:
  FGENTIC_FED_CLUSTER  must be fgentic-fed when set
  FGENTIC_FED_TIMEOUT  reconciliation timeout (default: 20m)
  FGENTIC_FED_CONSTRAINED
                       yes enables the opt-in serialized, right-sized laptop profile (default: no)
  FGENTIC_FED_NO_PROGRESS_TIMEOUT
                       constrained no-progress timeout (default: 20m)
  FGENTIC_FED_MAX_TIMEOUT
                       constrained absolute timeout (default: 60m)
  FGENTIC_FED_TRACE    yes writes allowlisted resource-only JSON under .agents/tmp (default: no)
  FGENTIC_FED_POLICY_PROBE
                       deny (default) or allow; allow changes only the ephemeral Git snapshot
  FGENTIC_DEMO_CACHE_DIR
                       optional persistent BuildKit cache directory for the source image
  FGENTIC_DEMO_STATE_DIR
                       optional lifecycle-state root; defaults to the user state directory
EOF
}

federation_timeout_seconds() {
	local value="${1%[smh]}"
	case "$1" in
	*s) printf '%s' "${value}" ;;
	*m) printf '%s' "$((value * 60))" ;;
	*h) printf '%s' "$((value * 3600))" ;;
	*) return 1 ;;
	esac
}

validate_federation_timeouts() {
	local maximum="${FGENTIC_FED_MAX_TIMEOUT:-60m}"
	local maximum_seconds no_progress="${FGENTIC_FED_NO_PROGRESS_TIMEOUT:-20m}"
	local no_progress_seconds
	[[ "${no_progress}" =~ ^[1-9][0-9]*[smh]$ ]] ||
		die "invalid FGENTIC_FED_NO_PROGRESS_TIMEOUT"
	[[ "${maximum}" =~ ^[1-9][0-9]*[smh]$ ]] ||
		die "invalid FGENTIC_FED_MAX_TIMEOUT"
	no_progress_seconds="$(federation_timeout_seconds "${no_progress}")"
	maximum_seconds="$(federation_timeout_seconds "${maximum}")"
	if [ "${FGENTIC_FED_CONSTRAINED:-no}" = yes ] &&
		((no_progress_seconds >= maximum_seconds)); then
		die "FGENTIC_FED_NO_PROGRESS_TIMEOUT must be shorter than FGENTIC_FED_MAX_TIMEOUT"
	fi
}

require_split_absent() {
	local artifact cluster cluster_inventory filter ids owner path state_root
	local teardown_dir
	# Split A and canonical share 127.0.0.2. Only the split lifecycle may recover or delete a
	# partial generation, so canonical up treats every split-owned identity as a reservation.
	state_root="${FGENTIC_DEMO_STATE_DIR:-${XDG_STATE_HOME:-${HOME:?}/.local/state}/fgentic}"
	teardown_dir="${state_root}/cluster-teardown"
	if [ -L "${teardown_dir}" ] ||
		{ [ -e "${teardown_dir}" ] && [ ! -d "${teardown_dir}" ]; }; then
		die "split federation teardown state reserves 127.0.0.2; run fed:split:down before canonical fed:up: ${teardown_dir}"
	fi
	for path in \
		"${state_root}/federation-split" \
		"${teardown_dir}/${FEDERATION_SPLIT_CLUSTER_A}.json" \
		"${teardown_dir}/${FEDERATION_SPLIT_CLUSTER_B}.json" \
		"${teardown_dir}/.${FEDERATION_SPLIT_CLUSTER_A}".* \
		"${teardown_dir}/.${FEDERATION_SPLIT_CLUSTER_B}".*; do
		if [ -e "${path}" ] || [ -L "${path}" ]; then
			die "split federation state reserves 127.0.0.2; run fed:split:down before canonical fed:up: ${path}"
		fi
	done

	for artifact in docker jq k3d; do
		require_command "${artifact}"
	done
	docker info >/dev/null 2>&1 || die "Docker daemon is not running"
	cluster_inventory="$(k3d cluster list --output json)" ||
		die "could not inspect split federation cluster inventory"
	jq -e 'type == "array"' <<<"${cluster_inventory}" >/dev/null 2>&1 ||
		die "k3d returned invalid cluster inventory"
	if jq -e --arg a "${FEDERATION_SPLIT_CLUSTER_A}" \
		--arg b "${FEDERATION_SPLIT_CLUSTER_B}" \
		'any(.[]; .name == $a or .name == $b)' <<<"${cluster_inventory}" >/dev/null; then
		die "split federation child cluster reserves 127.0.0.2; run fed:split:down before canonical fed:up"
	fi

	for cluster in "${FEDERATION_SPLIT_CLUSTER_A}" "${FEDERATION_SPLIT_CLUSTER_B}"; do
		case "${cluster}" in
		"${FEDERATION_SPLIT_CLUSTER_A}") owner=federation-split-a ;;
		"${FEDERATION_SPLIT_CLUSTER_B}") owner=federation-split-b ;;
		esac
		for filter in \
			"label=k3d.cluster=${cluster}" \
			"label=dev.fgentic.demo=${owner}" \
			"name=^/k3d-${cluster}-"; do
			ids="$(docker ps --all --quiet --filter "${filter}")" ||
				die "could not inspect split federation child containers"
			[ -z "${ids}" ] ||
				die "split federation child containers remain for ${cluster}; run fed:split:down before canonical fed:up"
		done

		ids="$(docker network ls --quiet --filter "name=^k3d-${cluster}$")" ||
			die "could not inspect split federation child networks"
		[ -z "${ids}" ] ||
			die "split federation child network remains for ${cluster}; run fed:split:down before canonical fed:up"

		ids="$(docker volume ls --quiet --filter "name=^k3d-${cluster}-images$")" ||
			die "could not inspect split federation child volumes"
		[ -z "${ids}" ] ||
			die "split federation child image volume remains for ${cluster}; run fed:split:down before canonical fed:up"
	done

	for artifact in "${FEDERATION_SPLIT_RELAY_A}" "${FEDERATION_SPLIT_RELAY_B}"; do
		ids="$(docker ps --all --quiet --filter "name=^/${artifact}$")" ||
			die "could not inspect split federation relay containers"
		[ -z "${ids}" ] ||
			die "split federation relay remains: ${artifact}; run fed:split:down before canonical fed:up"
	done
	ids="$(docker ps --all --quiet \
		--filter "label=dev.fgentic.federation-split=${FEDERATION_SPLIT_RELAY_OWNER}")" ||
		die "could not inspect split federation relay ownership"
	[ -z "${ids}" ] ||
		die "split federation owner-labelled relays remain; run fed:split:down before canonical fed:up"
}

if (($# != 1)); then
	usage >&2
	exit 2
fi

cluster_name="${FGENTIC_FED_CLUSTER:-${FEDERATION_CLUSTER}}"
[ "${cluster_name}" = "${FEDERATION_CLUSTER}" ] ||
	die "FGENTIC_FED_CLUSTER must be ${FEDERATION_CLUSTER}"
case "${FGENTIC_FED_CONSTRAINED:-no}" in
yes | no) ;;
*) die "FGENTIC_FED_CONSTRAINED must be yes or no" ;;
esac
case "${FGENTIC_FED_TRACE:-no}" in
yes | no) ;;
*) die "FGENTIC_FED_TRACE must be yes or no" ;;
esac

case "$1" in
up)
	case "${FGENTIC_FED_POLICY_PROBE:-deny}" in
	allow | deny) ;;
	*) die "FGENTIC_FED_POLICY_PROBE must be allow or deny" ;;
	esac
	validate_federation_timeouts
	require_split_absent
	export FGENTIC_FED_POLICY_PROBE="${FGENTIC_FED_POLICY_PROBE:-deny}"
	export FGENTIC_DEMO_PROFILE=federation
	export FGENTIC_DEMO_CLUSTER="${cluster_name}"
	export FGENTIC_DEMO_TIMEOUT="${FGENTIC_FED_TIMEOUT:-20m}"
	exec "${ROOT_DIR}/scripts/demo.sh" up
	;;
status | stop)
	export FGENTIC_DEMO_PROFILE=federation
	export FGENTIC_DEMO_CLUSTER="${cluster_name}"
	exec "${ROOT_DIR}/scripts/demo.sh" "$1"
	;;
down)
	export FGENTIC_DEMO_PROFILE=federation
	export FGENTIC_DEMO_CLUSTER="${cluster_name}"
	export FGENTIC_DEMO_TIMEOUT="${FGENTIC_FED_TIMEOUT:-20m}"
	exec "${ROOT_DIR}/scripts/demo.sh" down
	;;
-h | --help)
	usage
	;;
*)
	usage >&2
	exit 2
	;;
esac
