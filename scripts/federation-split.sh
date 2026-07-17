#!/usr/bin/env bash
# Guarded two-control-plane federation drill. The child evaluator keeps each k3d cluster's
# ownership and teardown receipt; this parent owns only stable public-root state, directional
# raw-TLS relays, ordering, and the single cross-cluster acceptance invocation.
set -Eeuo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly CLUSTER_A="fgentic-fed-a"
readonly CLUSTER_B="fgentic-fed-b"
readonly CANONICAL_CLUSTER="fgentic-fed"
readonly LAYOUT_A="split-a"
readonly LAYOUT_B="split-b"
readonly LOOPBACK_A="127.0.0.2"
readonly LOOPBACK_B="127.0.0.3"
readonly RELAY_IMAGE="ghcr.io/k3d-io/k3d-proxy:5.9.0@sha256:7be40abfeb6ac9a81d0d4157a7cbf3aacc68a64a1780ca2efb160f1aea915177"
readonly RELAY_A_TO_B="fgentic-fed-a-to-b"
readonly RELAY_B_TO_A="fgentic-fed-b-to-a"
readonly RELAY_OWNER="fgentic.federation-split-relay.v1"
readonly NETWORK_A="k3d-${CLUSTER_A}"
readonly NETWORK_B="k3d-${CLUSTER_B}"
readonly SERVERLB_A="k3d-${CLUSTER_A}-serverlb"
readonly SERVERLB_B="k3d-${CLUSTER_B}-serverlb"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

STATE_ROOT="${FGENTIC_DEMO_STATE_DIR:-${XDG_STATE_HOME:-${HOME:?}/.local/state}/fgentic}"
readonly STATE_ROOT
readonly SPLIT_STATE_DIR="${STATE_ROOT}/federation-split"
readonly CA_DIR_A="${SPLIT_STATE_DIR}/ca/org-a"
readonly CA_DIR_B="${SPLIT_STATE_DIR}/ca/org-b"
readonly HOST_CA_BUNDLE="${SPLIT_STATE_DIR}/ca/host-bundle.pem"
readonly CA_RECEIPT="${SPLIT_STATE_DIR}/ca/roots.json"
readonly RELAY_CONFIG_DIR="${SPLIT_STATE_DIR}/relay-config"
readonly RELAY_CONFIG_A_TO_B="${RELAY_CONFIG_DIR}/a-to-b.yaml"
readonly RELAY_CONFIG_B_TO_A="${RELAY_CONFIG_DIR}/b-to-a.yaml"
readonly RELAY_RECEIPT="${SPLIT_STATE_DIR}/relays.json"
readonly PARENT_TEARDOWN_RECEIPT="${SPLIT_STATE_DIR}/teardown.json"
readonly CHILD_TEARDOWN_RECEIPT_A="${STATE_ROOT}/cluster-teardown/${CLUSTER_A}.json"
readonly CHILD_TEARDOWN_RECEIPT_B="${STATE_ROOT}/cluster-teardown/${CLUSTER_B}.json"
SPLIT_UP_WORK_DIR=""

cleanup_split_up() {
	local status="$1"
	if [ -n "${SPLIT_UP_WORK_DIR}" ]; then
		rm -rf "${SPLIT_UP_WORK_DIR}"
	fi
	if [ "${status}" -ne 0 ]; then
		echo "Split federation did not complete; run fed:split:status, then fed:split:down for exact recovery." >&2
	fi
	return "${status}"
}

usage() {
	cat <<'EOF'
usage: scripts/federation-split.sh up|status|stop|down

Creates the opt-in split federation drill on exactly two owned k3d control planes:
  fgentic-fed-a  127.0.0.2  org A + denied org C + docs/A2A seller plane
  fgentic-fed-b  127.0.0.3  org B + Keycloak machine-client issuer

The canonical fgentic-fed lab must be absent. Split mode is standard-capacity only. Two exact
dual-network k3d-proxy 5.9.0 containers forward raw TLS directionally; no workload or k3d node is
attached to both Docker networks. Stable, distinct CA roots and relay receipts live below the
same parent state root as the child teardown receipts.

Environment:
  FGENTIC_FED_TIMEOUT       child reconciliation timeout (default: 20m)
  FGENTIC_DEMO_STATE_DIR   optional lifecycle-state root
  FGENTIC_DEMO_CACHE_DIR   optional caller-owned BuildKit cache
EOF
}

require_split_host() {
	local command
	for command in docker jq k3d kubectl openssl rg yq; do
		require_command "${command}"
	done
	docker info >/dev/null 2>&1 || die "Docker daemon is not running"
	case "${FGENTIC_FED_CONSTRAINED:-no}" in
	no) ;;
	yes) die "constrained capacity is not supported by split federation" ;;
	*) die "FGENTIC_FED_CONSTRAINED must be no for split federation" ;;
	esac
	[ "${FGENTIC_FED_POLICY_PROBE:-deny}" = deny ] ||
		die "split federation accepts only the canonical deny policy"
	[ "${FGENTIC_FED_TRACE:-no}" = no ] ||
		die "resource tracing is not supported by split federation"
	[ -z "${FGENTIC_FED_CLUSTER:-}" ] ||
		die "split federation cluster names are fixed and accept no override"
	[[ "${STATE_ROOT}" = /* ]] ||
		die "split federation lifecycle state must use an absolute path"
	validate_existing_state_paths
	validate_parent_state_inventory
}

validate_existing_state_paths() {
	local path resolved_state
	if [ ! -e "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ]; then
		return
	fi
	[ -d "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ] ||
		die "split federation state must be a non-symlink directory"
	resolved_state="$(cd "${SPLIT_STATE_DIR}" && pwd -P)"
	case "${resolved_state}/" in
	"${ROOT_DIR}/"*) die "split lifecycle state and private CA keys must remain outside the repository" ;;
	esac
	for path in "${SPLIT_STATE_DIR}/ca" "${CA_DIR_A}" "${CA_DIR_B}" \
		"${RELAY_CONFIG_DIR}"; do
		if [ -L "${path}" ] || { [ -e "${path}" ] && [ ! -d "${path}" ]; }; then
			die "split federation state path must be a non-symlink directory: ${path}"
		fi
	done
	for path in "${HOST_CA_BUNDLE}" "${CA_RECEIPT}" "${RELAY_RECEIPT}" \
		"${PARENT_TEARDOWN_RECEIPT}" \
		"${RELAY_CONFIG_A_TO_B}" "${RELAY_CONFIG_B_TO_A}" \
		"${CA_DIR_A}/ca.crt" "${CA_DIR_A}/ca.key" \
		"${CA_DIR_B}/ca.crt" "${CA_DIR_B}/ca.key"; do
		if [ -L "${path}" ] || { [ -e "${path}" ] && [ ! -f "${path}" ]; }; then
			die "split federation state file must be regular and non-symlinked: ${path}"
		fi
	done
}

ensure_state_directory() {
	local resolved_state
	if [ -L "${SPLIT_STATE_DIR}" ] ||
		{ [ -e "${SPLIT_STATE_DIR}" ] && [ ! -d "${SPLIT_STATE_DIR}" ]; }; then
		die "split federation state path must be a non-symlink directory: ${SPLIT_STATE_DIR}"
	fi
	mkdir -p "${SPLIT_STATE_DIR}" ||
		die "could not create split federation state directory"
	[ -d "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ] ||
		die "split federation state must be a non-symlink directory"
	chmod 700 "${SPLIT_STATE_DIR}" ||
		die "could not protect split federation state"
	resolved_state="$(cd "${SPLIT_STATE_DIR}" && pwd -P)"
	case "${resolved_state}/" in
	"${ROOT_DIR}/"*) die "split lifecycle state and private CA keys must remain outside the repository" ;;
	esac
}

temporary_state_paths() {
	local path
	for path in \
		"${SPLIT_STATE_DIR}"/.ca.* \
		"${SPLIT_STATE_DIR}"/.relays.* \
		"${SPLIT_STATE_DIR}"/.teardown.* \
		"${SPLIT_STATE_DIR}"/ca/.host-bundle.* \
		"${SPLIT_STATE_DIR}"/ca/.roots.* \
		"${RELAY_CONFIG_DIR}"/.relay.*; do
		if [ -e "${path}" ] || [ -L "${path}" ]; then
			printf '%s\n' "${path}"
		fi
	done
}

validate_parent_state_inventory() {
	local path
	if [ ! -e "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ]; then
		return
	fi
	[ -d "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ] ||
		die "split parent lifecycle state must be a non-symlink directory"
	for path in "${SPLIT_STATE_DIR}"/* "${SPLIT_STATE_DIR}"/.[!.]* \
		"${SPLIT_STATE_DIR}"/..?*; do
		[ -e "${path}" ] || [ -L "${path}" ] || continue
		case "${path}" in
		"${SPLIT_STATE_DIR}/ca" | "${RELAY_CONFIG_DIR}" | "${RELAY_RECEIPT}" | \
			"${PARENT_TEARDOWN_RECEIPT}" | "${SPLIT_STATE_DIR}"/.ca.* | \
			"${SPLIT_STATE_DIR}"/.relays.* | "${SPLIT_STATE_DIR}"/.teardown.*)
			;;
		*) die "unexpected path in split parent lifecycle state: ${path}" ;;
		esac
	done
}

require_no_temporary_state() {
	local paths
	paths="$(temporary_state_paths)"
	[ -z "${paths}" ] ||
		die "split federation atomic state is recovery-pending; run fed:split:down: ${paths//$'\n'/, }"
}

validate_atomic_ca_temporary_directory() (
	local entry relative root="$1"
	[ -d "${root}" ] && [ ! -L "${root}" ] ||
		die "split CA atomic state must be a non-symlink directory: ${root}"
	shopt -s dotglob globstar nullglob
	for entry in "${root}"/**; do
		[ "${entry}" != "${root}/" ] || continue
		relative="${entry#"${root}/"}"
		if [ -L "${entry}" ]; then
			die "split CA atomic state contains a symlink: ${entry}"
		fi
		if [ -d "${entry}" ]; then
			case "${relative}" in
			org-a | org-b) ;;
			*) die "split CA atomic state contains an unexpected directory: ${entry}" ;;
			esac
		elif [ -f "${entry}" ]; then
			case "${relative}" in
			org-a/ca.crt | org-a/ca.key | org-b/ca.crt | org-b/ca.key | \
				host-bundle.pem | roots.json | .host-bundle.* | .roots.*) ;;
			*) die "split CA atomic state contains an unexpected file: ${entry}" ;;
			esac
		else
			die "split CA atomic state contains an unsupported entry: ${entry}"
		fi
	done
)

validate_temporary_state() {
	local path
	while IFS= read -r path; do
		[ -n "${path}" ] || continue
		[ ! -L "${path}" ] ||
			die "split federation atomic state must not contain symlinks: ${path}"
		case "${path}" in
		"${SPLIT_STATE_DIR}"/.ca.*)
			validate_atomic_ca_temporary_directory "${path}"
			;;
		"${SPLIT_STATE_DIR}"/.relays.* | "${SPLIT_STATE_DIR}"/.teardown.* | \
			"${SPLIT_STATE_DIR}"/ca/.host-bundle.* | \
			"${SPLIT_STATE_DIR}"/ca/.roots.* | "${RELAY_CONFIG_DIR}"/.relay.*)
			[ -f "${path}" ] ||
				die "split federation atomic state must be a regular file: ${path}"
			;;
		*) die "unsupported split federation atomic state path: ${path}" ;;
		esac
	done < <(temporary_state_paths)
}

cleanup_temporary_state() {
	local path
	validate_temporary_state
	while IFS= read -r path; do
		[ -n "${path}" ] || continue
		case "${path}" in
		"${SPLIT_STATE_DIR}"/.ca.*)
			rm -f "${path}/org-a/ca.crt" "${path}/org-a/ca.key" \
				"${path}/org-b/ca.crt" "${path}/org-b/ca.key" \
				"${path}/host-bundle.pem" "${path}/roots.json" \
				"${path}"/.host-bundle.* "${path}"/.roots.*
			rmdir "${path}/org-a" "${path}/org-b" 2>/dev/null || true
			rmdir "${path}" ||
				die "could not remove exact split CA atomic state: ${path}"
			;;
		*)
			rm -f "${path}" ||
				die "could not remove exact split federation atomic state: ${path}"
			;;
		esac
	done < <(temporary_state_paths)
}

run_child() {
	local layout="$1"
	local cluster="$2"
	local ca_dir="$3"
	local phase="$4"
	local action="$5"
	local local_gateway_ip="${6:-}"
	local remote_gateway_ip="${7:-}"
	env \
		FGENTIC_DEMO_PROFILE=federation \
		FGENTIC_FED_LAYOUT="${layout}" \
		FGENTIC_FED_CHILD_PHASE="${phase}" \
		FGENTIC_DEMO_CLUSTER="${cluster}" \
		FGENTIC_DEMO_STATE_DIR="${STATE_ROOT}" \
		FGENTIC_CA_DIR="${ca_dir}" \
		FGENTIC_FED_CA_DIR_A="${CA_DIR_A}" \
		FGENTIC_FED_CA_DIR_B="${CA_DIR_B}" \
		FGENTIC_FED_HOST_CA_BUNDLE="${HOST_CA_BUNDLE}" \
		FGENTIC_FED_LOCAL_GATEWAY_IP="${local_gateway_ip}" \
		FGENTIC_FED_REMOTE_GATEWAY_IP="${remote_gateway_ip}" \
		FGENTIC_FED_CONSTRAINED=no \
		FGENTIC_FED_TRACE=no \
		FGENTIC_FED_POLICY_PROBE=deny \
		FGENTIC_DEMO_TIMEOUT="${FGENTIC_FED_TIMEOUT:-20m}" \
		"${ROOT_DIR}/scripts/demo.sh" "${action}"
}

canonical_status() {
	env \
		FGENTIC_DEMO_PROFILE=federation \
		FGENTIC_FED_LAYOUT=canonical \
		FGENTIC_DEMO_CLUSTER="${CANONICAL_CLUSTER}" \
		FGENTIC_DEMO_STATE_DIR="${STATE_ROOT}" \
		FGENTIC_FED_CONSTRAINED=no \
		FGENTIC_FED_TRACE=no \
		"${ROOT_DIR}/scripts/demo.sh" status
}

require_canonical_absent() {
	local output
	output="$(canonical_status)" || die "could not inspect the canonical federation lab"
	rg --fixed-strings "Federation cluster ${CANONICAL_CLUSTER}: state=absent retained_bytes=0" \
		<<<"${output}" >/dev/null ||
		die "canonical ${CANONICAL_CLUSTER} must be fully absent before split federation"
}

child_preflight_state() {
	local ca_dir="$3"
	local cluster="$2"
	local layout="$1"
	local output
	output="$(run_child "${layout}" "${cluster}" "${ca_dir}" lifecycle status)" ||
		die "could not inspect split child ${cluster}"
	if rg --fixed-strings 'state=recovery-pending' <<<"${output}" >/dev/null; then
		die "${cluster} teardown recovery is pending; run fed:split:down first"
	fi
	if rg --fixed-strings "Federation cluster ${cluster}: state=absent retained_bytes=0" \
		<<<"${output}" >/dev/null; then
		printf 'absent\n'
	else
		printf 'present\n'
	fi
}

preflight_split_up() {
	local ca_state child_a_state="$1" child_b_state="$2" phase
	[ ! -e "${PARENT_TEARDOWN_RECEIPT}" ] &&
		[ ! -L "${PARENT_TEARDOWN_RECEIPT}" ] ||
		die "split federation parent teardown is pending; run fed:split:down"
	require_no_temporary_state
	ca_state="$(validate_stable_ca_state)"
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		validate_relay_receipt_file ||
			die "split relay receipt is malformed or stale"
		phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
		case "${phase}" in
		active)
			validate_receipt_networks
			validate_relay_container a-to-b yes
			validate_relay_container b-to-a yes
			assert_only_relays_dual_attached
			;;
		stopped)
			validate_receipt_networks
			validate_relay_container a-to-b stopped
			validate_relay_container b-to-a stopped
			assert_only_relays_dual_attached
			;;
		*) die "split relay recovery is pending; run fed:split:down before up" ;;
		esac
	else
		ensure_no_unreceipted_relays
		if [ -e "${RELAY_CONFIG_DIR}" ] || [ -L "${RELAY_CONFIG_DIR}" ]; then
			die "unreceipted split relay config state is recovery-pending; run fed:split:down"
		fi
		phase=absent
	fi
	case "${child_a_state}:${child_b_state}:${ca_state}:${phase}" in
	absent:absent:absent:absent | present:present:complete:active | present:present:complete:stopped)
		;;
	*) die "split federation parent and child generations are incomplete; run fed:split:down before up" ;;
	esac
}

split_ca_state() {
	local present=0 required=6
	local path
	for path in "${CA_DIR_A}/ca.crt" "${CA_DIR_A}/ca.key" \
		"${CA_DIR_B}/ca.crt" "${CA_DIR_B}/ca.key" \
		"${HOST_CA_BUNDLE}" "${CA_RECEIPT}"; do
		if [ -e "${path}" ] || [ -L "${path}" ]; then
			present=$((present + 1))
		fi
	done
	case "${present}" in
	0)
		if [ -e "${SPLIT_STATE_DIR}/ca" ] || [ -L "${SPLIT_STATE_DIR}/ca" ]; then
			printf 'partial\n'
		else
			printf 'absent\n'
		fi
		;;
	"${required}")
		if [ -d "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ] &&
			[ -d "${CA_DIR_A}" ] && [ ! -L "${CA_DIR_A}" ] &&
			[ -d "${CA_DIR_B}" ] && [ ! -L "${CA_DIR_B}" ]; then
			printf 'complete\n'
		else
			printf 'partial\n'
		fi
		;;
	*) printf 'partial\n' ;;
	esac
}

validate_split_ca_pair() {
	local cert_public directory="$1" key_public
	[ -d "${directory}" ] && [ ! -L "${directory}" ] ||
		die "split federation CA directory must not be a symlink: ${directory}"
	[ -f "${directory}/ca.crt" ] && [ ! -L "${directory}/ca.crt" ] &&
		[ -f "${directory}/ca.key" ] && [ ! -L "${directory}/ca.key" ] ||
		die "split federation CA is incomplete in ${directory}"
	openssl verify -CAfile "${directory}/ca.crt" "${directory}/ca.crt" >/dev/null 2>&1 ||
		die "split federation CA is not self-verifying in ${directory}"
	cert_public="$(openssl x509 -in "${directory}/ca.crt" -pubkey -noout |
		openssl pkey -pubin -outform DER 2>/dev/null |
		openssl dgst -sha256 -r | awk '{print $1}')"
	key_public="$(openssl pkey -in "${directory}/ca.key" -pubout -outform DER 2>/dev/null |
		openssl dgst -sha256 -r | awk '{print $1}')"
	[ -n "${cert_public}" ] && [ "${cert_public}" = "${key_public}" ] ||
		die "split federation CA certificate and private key do not match in ${directory}"
}

generate_split_ca() {
	local directory="$1"
	FGENTIC_CA_DIR="${directory}" "${ROOT_DIR}/scripts/local-ca.sh" --generate-only >/dev/null
	if ! chmod 700 "${directory}" || ! chmod 600 "${directory}/ca.key"; then
		die "could not protect split federation CA in ${directory}"
	fi
	validate_split_ca_pair "${directory}"
}

write_host_ca_bundle() {
	local ca_a="${1:-${CA_DIR_A}}"
	local ca_b="${2:-${CA_DIR_B}}"
	local output="${3:-${HOST_CA_BUNDLE}}"
	local certificate_count fingerprint_a fingerprint_b temporary
	fingerprint_a="$(openssl x509 -in "${ca_a}/ca.crt" -noout -fingerprint -sha256)"
	fingerprint_b="$(openssl x509 -in "${ca_b}/ca.crt" -noout -fingerprint -sha256)"
	[ "${fingerprint_a}" != "${fingerprint_b}" ] ||
		die "split federation requires two distinct CA roots"
	temporary="$(mktemp "${output%/*}/.host-bundle.XXXXXX")"
	chmod 600 "${temporary}"
	{
		awk 'NF {print}' "${ca_a}/ca.crt"
		awk 'NF {print}' "${ca_b}/ca.crt"
	} >"${temporary}"
	certificate_count="$(rg --count --fixed-strings -- \
		'-----BEGIN CERTIFICATE-----' "${temporary}" || true)"
	if [ "${certificate_count}" != 2 ] ||
		rg --fixed-strings -- 'PRIVATE KEY' "${temporary}" >/dev/null; then
		rm -f "${temporary}"
		die "split federation host bundle must contain exactly two public roots"
	fi
	mv "${temporary}" "${output}"
}

ca_fingerprint() {
	openssl x509 -in "$1" -outform DER |
		openssl dgst -sha256 -r | awk '{print "sha256:" $1}'
}

validate_ca_receipt_file() {
	local receipt="${1:-${CA_RECEIPT}}"
	[ -f "${receipt}" ] && [ ! -L "${receipt}" ] || return 1
	jq --exit-status '
      keys == ["roots", "schema"] and
      .schema == "fgentic.federation-split-ca.v1" and
      (.roots | keys == ["a", "b"]) and
      (all(.roots[]; type == "string" and test("^sha256:[0-9a-f]{64}$"))) and
      .roots.a != .roots.b
    ' "${receipt}" >/dev/null 2>&1
}

write_ca_receipt() {
	local ca_a="$1"
	local ca_b="$2"
	local output="$3"
	local actual_a actual_b temporary
	actual_a="$(ca_fingerprint "${ca_a}/ca.crt")"
	actual_b="$(ca_fingerprint "${ca_b}/ca.crt")"
	[ "${actual_a}" != "${actual_b}" ] || die "split federation requires two distinct CA roots"
	temporary="$(mktemp "${output%/*}/.roots.XXXXXX")"
	chmod 600 "${temporary}"
	jq --null-input --arg a "${actual_a}" --arg b "${actual_b}" '{
      schema: "fgentic.federation-split-ca.v1",
      roots: {a: $a, b: $b}
    }' >"${temporary}"
	mv "${temporary}" "${output}"
	validate_ca_receipt_file "${output}" || die "could not persist split CA identity receipt"
}

validate_ca_inventory() {
	local child entry root="$1"
	local ca_a="${root}/org-a"
	local ca_b="${root}/org-b"
	if [ ! -e "${root}" ] && [ ! -L "${root}" ]; then
		return
	fi
	[ -d "${root}" ] && [ ! -L "${root}" ] ||
		die "split CA state must be a non-symlink directory: ${root}"
	for entry in "${root}"/* "${root}"/.[!.]* "${root}"/..?*; do
		[ -e "${entry}" ] || [ -L "${entry}" ] || continue
		[ ! -L "${entry}" ] || die "split CA state contains a symlink: ${entry}"
		case "${entry}" in
		"${ca_a}" | "${ca_b}")
			[ -d "${entry}" ] ||
				die "split CA directory changed type: ${entry}"
			;;
		"${root}/host-bundle.pem" | "${root}/roots.json")
			[ -f "${entry}" ] ||
				die "split CA file changed type: ${entry}"
			;;
		"${root}"/.host-bundle.* | "${root}"/.roots.*)
			[ -f "${entry}" ] ||
				die "split CA atomic state changed type: ${entry}"
			;;
		*) die "unexpected path in split CA state: ${entry}" ;;
		esac
	done
	for entry in "${ca_a}" "${ca_b}"; do
		[ ! -e "${entry}" ] && [ ! -L "${entry}" ] && continue
		[ -d "${entry}" ] && [ ! -L "${entry}" ] ||
			die "split CA directory changed type: ${entry}"
		for child in "${entry}"/* "${entry}"/.[!.]* "${entry}"/..?*; do
			[ -e "${child}" ] || [ -L "${child}" ] || continue
			[ ! -L "${child}" ] ||
				die "split CA state contains a symlink: ${child}"
			case "${child}" in
			"${entry}/ca.crt" | "${entry}/ca.key")
				[ -f "${child}" ] ||
					die "split CA file changed type: ${child}"
				;;
			*) die "unexpected path in split CA directory: ${child}" ;;
			esac
		done
	done
}

validate_ca_tree() {
	local root="$1"
	local ca_a="${root}/org-a"
	local ca_b="${root}/org-b"
	local bundle="${root}/host-bundle.pem"
	local receipt="${root}/roots.json"
	local actual_a actual_b expected_bundle observed_bundle
	validate_ca_inventory "${root}"
	validate_split_ca_pair "${ca_a}"
	validate_split_ca_pair "${ca_b}"
	validate_ca_receipt_file "${receipt}" ||
		die "split federation CA identity receipt is malformed"
	actual_a="$(ca_fingerprint "${ca_a}/ca.crt")"
	actual_b="$(ca_fingerprint "${ca_b}/ca.crt")"
	jq --exit-status --arg a "${actual_a}" --arg b "${actual_b}" \
		'.roots == {a: $a, b: $b}' "${receipt}" >/dev/null ||
		die "refusing implicit split CA rotation: trust-root fingerprint changed"
	[ -f "${bundle}" ] && [ ! -L "${bundle}" ] ||
		die "split federation host CA bundle is missing or not regular"
	if rg --fixed-strings -- 'PRIVATE KEY' "${bundle}" >/dev/null; then
		die "split federation host CA bundle contains private key material"
	fi
	expected_bundle="$(
		awk 'NF {print}' "${ca_a}/ca.crt"
		awk 'NF {print}' "${ca_b}/ca.crt"
	)"
	observed_bundle="$(awk 'NF {print}' "${bundle}")"
	if [ "${expected_bundle}" != "${observed_bundle}" ]; then
		die "split federation host CA bundle differs from the two receipted public roots"
	fi
}

validate_stable_ca_state() {
	local state
	state="$(split_ca_state)"
	case "${state}" in
	absent)
		printf 'absent\n'
		;;
	complete)
		validate_ca_tree "${SPLIT_STATE_DIR}/ca"
		printf 'complete\n'
		;;
	partial)
		die "split federation CA state is partial or unreceipted; run fed:split:down for exact recovery"
		;;
	*) die "unsupported split federation CA state ${state}" ;;
	esac
}

build_split_ca_tree() {
	local root="$1"
	mkdir -p "${root}"
	chmod 700 "${root}"
	generate_split_ca "${root}/org-a"
	generate_split_ca "${root}/org-b"
	write_ca_receipt "${root}/org-a" "${root}/org-b" "${root}/roots.json"
	write_host_ca_bundle "${root}/org-a" "${root}/org-b" "${root}/host-bundle.pem"
	validate_ca_tree "${root}"
}

prepare_public_roots() {
	local child_a_state="$1"
	local child_b_state="$2"
	local ca_state temporary
	ca_state="$(split_ca_state)"
	case "${ca_state}" in
	absent)
		[ "${child_a_state}" = absent ] && [ "${child_b_state}" = absent ] ||
			die "refusing split CA creation while an owned child control plane exists"
		ensure_state_directory
		temporary="$(mktemp -d "${SPLIT_STATE_DIR}/.ca.XXXXXX")"
		if ! (build_split_ca_tree "${temporary}"); then
			rm -rf "${temporary}"
			die "could not construct the atomic split CA generation"
		fi
		[ ! -e "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ] ||
			die "refusing to replace existing split CA state"
		mv "${temporary}" "${SPLIT_STATE_DIR}/ca" ||
			die "could not publish the atomic split CA generation"
		validate_stable_ca_state >/dev/null
		;;
	complete)
		validate_stable_ca_state >/dev/null
		;;
	partial)
		die "split federation CA state is partial or unreceipted; run fed:split:down for exact recovery"
		;;
	*) die "unsupported split federation CA state ${ca_state}" ;;
	esac
}

file_sha256() {
	openssl dgst -sha256 -r "$1" | awk '{print "sha256:" $1}'
}

network_document() {
	local cluster="$1"
	local network="$2"
	docker network inspect "${network}" |
		jq -ce --arg cluster "${cluster}" --arg network "${network}" '
      .[0] |
      select(.Name == $network and .Labels.app == "k3d" and
        ((.Labels."k3d.cluster" // "") == "" or .Labels."k3d.cluster" == $cluster)) |
      {id: .Id, name: .Name, cluster: $cluster}
    ' || die "Docker network ${network} is missing or not owned by ${cluster}"
}

validate_receipt_networks() {
	local actual_a actual_b expected_a expected_b
	expected_a="$(jq -c '.networks.a' "${RELAY_RECEIPT}")"
	expected_b="$(jq -c '.networks.b' "${RELAY_RECEIPT}")"
	actual_a="$(network_document "${CLUSTER_A}" "${NETWORK_A}")"
	actual_b="$(network_document "${CLUSTER_B}" "${NETWORK_B}")"
	[ "${actual_a}" = "${expected_a}" ] && [ "${actual_b}" = "${expected_b}" ] ||
		die "split relay network identity changed"
}

serverlb_ip() {
	local cluster="$1"
	local network="k3d-${cluster}"
	docker inspect "k3d-${cluster}-serverlb" |
		jq -er --arg network "${network}" '
      .[0] | select(.Config.Labels."k3d.cluster" == ($network | ltrimstr("k3d-"))) |
      .NetworkSettings.Networks[$network].IPAddress |
      select(type == "string" and length > 0)
    ' || die "could not resolve owned ingress for ${cluster}"
}

write_relay_config() {
	local output="$1"
	local target="$2"
	local temporary
	if [ -L "${RELAY_CONFIG_DIR}" ] ||
		{ [ -e "${RELAY_CONFIG_DIR}" ] && [ ! -d "${RELAY_CONFIG_DIR}" ]; }; then
		die "relay config state must be a non-symlink directory"
	fi
	mkdir -p "${RELAY_CONFIG_DIR}"
	[ -d "${RELAY_CONFIG_DIR}" ] && [ ! -L "${RELAY_CONFIG_DIR}" ] ||
		die "relay config state must be a non-symlink directory"
	chmod 700 "${RELAY_CONFIG_DIR}"
	temporary="$(mktemp "${RELAY_CONFIG_DIR}/.relay.XXXXXX")"
	chmod 600 "${temporary}"
	printf 'ports:\n  443.tcp:\n    - %s\nsettings:\n  workerConnections: 1024\n  defaultProxyTimeout: 600\n' \
		"${target}" >"${temporary}"
	RELAY_TARGET="${target}" yq --exit-status '
      ((keys | length) == 2) and has("ports") and has("settings") and
      ((.ports | keys | length) == 1) and (.ports | has("443.tcp")) and
      ((.ports."443.tcp" | length) == 1) and
      .ports."443.tcp"[0] == strenv(RELAY_TARGET) and
      ((.settings | keys | length) == 2) and
      .settings.workerConnections == 1024 and .settings.defaultProxyTimeout == 600
    ' "${temporary}" >/dev/null || {
		rm -f "${temporary}"
		die "could not construct exact relay config for ${target}"
	}
	mv "${temporary}" "${output}"
	chmod 400 "${output}"
}

validate_relay_receipt_file() {
	[ -f "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ] || return 1
	validate_relay_config_inventory
	jq --exit-status \
		--arg image "${RELAY_IMAGE}" \
		--arg network_a "${NETWORK_A}" --arg network_b "${NETWORK_B}" \
		--arg relay_a "${RELAY_A_TO_B}" --arg relay_b "${RELAY_B_TO_A}" \
		--arg config_a "$(file_sha256 "${RELAY_CONFIG_A_TO_B}" 2>/dev/null || true)" \
		--arg config_b "$(file_sha256 "${RELAY_CONFIG_B_TO_A}" 2>/dev/null || true)" '
      . as $receipt |
      keys == ["image", "image_id", "networks", "phase", "relays", "schema"] and
      .schema == "fgentic.federation-split-relays.v1" and
      (.phase == "creating" or .phase == "active" or .phase == "stopped" or .phase == "removing") and
      .image == $image and (.image_id | type == "string") and
      (.networks | keys == ["a", "b"]) and
      .networks.a.name == $network_a and .networks.a.cluster == "fgentic-fed-a" and
      .networks.b.name == $network_b and .networks.b.cluster == "fgentic-fed-b" and
      (all(.networks[]; keys == ["cluster", "id", "name"] and
        (.id | type == "string" and length > 0))) and
      (.relays | length == 2) and
      (all(.relays[];
        keys == ["config_sha256", "direction", "generation", "id", "local_ip", "local_network", "name", "remote_network", "target"] and
        (.generation | type == "string" and test("^[0-9a-f]{32}$")) and
        (.id | type == "string") and (.local_ip | type == "string"))) and
      (.relays[0].generation != .relays[1].generation) and
      ([.relays[] | select(
        .direction == "a-to-b" and .name == $relay_a and
        .local_network == $network_a and .remote_network == $network_b and
        .target == "k3d-fgentic-fed-b-serverlb" and .config_sha256 == $config_a
      )] | length == 1) and
      ([.relays[] | select(
        .direction == "b-to-a" and .name == $relay_b and
        .local_network == $network_b and .remote_network == $network_a and
        .target == "k3d-fgentic-fed-a-serverlb" and .config_sha256 == $config_b
      )] | length == 1) and
      (if .phase == "active" or .phase == "stopped" then
        (.image_id | length > 0) and all(.relays[]; (.id | length > 0) and (.local_ip | length > 0))
       else true end)
    ' "${RELAY_RECEIPT}" >/dev/null 2>&1
}

atomic_receipt_update() {
	local filter="$1"
	shift
	local temporary
	temporary="$(mktemp "${SPLIT_STATE_DIR}/.relays.XXXXXX")"
	chmod 600 "${temporary}"
	if ! jq "$@" "${filter}" "${RELAY_RECEIPT}" >"${temporary}"; then
		rm -f "${temporary}"
		die "could not update split relay receipt"
	fi
	mv "${temporary}" "${RELAY_RECEIPT}"
	validate_relay_receipt_file || die "updated split relay receipt is invalid"
}

write_creating_relay_receipt() {
	local config_a config_b generation_a generation_b network_a network_b temporary
	[ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ] ||
		die "split relay receipt already exists"
	network_a="$(network_document "${CLUSTER_A}" "${NETWORK_A}")"
	network_b="$(network_document "${CLUSTER_B}" "${NETWORK_B}")"
	write_relay_config "${RELAY_CONFIG_A_TO_B}" "${SERVERLB_B}"
	write_relay_config "${RELAY_CONFIG_B_TO_A}" "${SERVERLB_A}"
	config_a="$(file_sha256 "${RELAY_CONFIG_A_TO_B}")"
	config_b="$(file_sha256 "${RELAY_CONFIG_B_TO_A}")"
	generation_a="$(openssl rand -hex 16)"
	generation_b="$(openssl rand -hex 16)"
	[[ "${generation_a}" =~ ^[0-9a-f]{32}$ ]] &&
		[[ "${generation_b}" =~ ^[0-9a-f]{32}$ ]] &&
		[ "${generation_a}" != "${generation_b}" ] ||
		die "could not generate distinct split relay identities"
	temporary="$(mktemp "${SPLIT_STATE_DIR}/.relays.XXXXXX")"
	chmod 600 "${temporary}"
	jq --null-input \
		--arg image "${RELAY_IMAGE}" \
		--argjson network_a "${network_a}" --argjson network_b "${network_b}" \
		--arg relay_a "${RELAY_A_TO_B}" --arg relay_b "${RELAY_B_TO_A}" \
		--arg config_a "${config_a}" --arg config_b "${config_b}" \
		--arg generation_a "${generation_a}" --arg generation_b "${generation_b}" '
      {
        schema: "fgentic.federation-split-relays.v1",
        phase: "creating",
        image: $image,
        image_id: "",
        networks: {a: $network_a, b: $network_b},
        relays: [
          {direction: "a-to-b", generation: $generation_a, id: "", name: $relay_a, local_network: $network_a.name,
           remote_network: $network_b.name, local_ip: "", target: "k3d-fgentic-fed-b-serverlb",
           config_sha256: $config_a},
          {direction: "b-to-a", generation: $generation_b, id: "", name: $relay_b, local_network: $network_b.name,
           remote_network: $network_a.name, local_ip: "", target: "k3d-fgentic-fed-a-serverlb",
           config_sha256: $config_b}
        ]
      }
    ' >"${temporary}"
	mv "${temporary}" "${RELAY_RECEIPT}"
	validate_relay_receipt_file || die "could not persist creating split relay receipt"
}

relay_object() {
	local direction="$1"
	jq -ce --arg direction "${direction}" '.relays[] | select(.direction == $direction)' \
		"${RELAY_RECEIPT}"
}

validate_relay_container() {
	local direction="$1"
	local strict="${2:-no}"
	local config expected_generation expected_id expected_image_id expected_local_ip inspect local_network name
	local network_a_id network_b_id object remote_network
	object="$(relay_object "${direction}")"
	name="$(jq -r '.name' <<<"${object}")"
	expected_id="$(jq -r '.id' <<<"${object}")"
	expected_generation="$(jq -r '.generation' <<<"${object}")"
	local_network="$(jq -r '.local_network' <<<"${object}")"
	remote_network="$(jq -r '.remote_network' <<<"${object}")"
	expected_local_ip="$(jq -r '.local_ip' <<<"${object}")"
	config="${RELAY_CONFIG_A_TO_B}"
	[ "${direction}" = a-to-b ] || config="${RELAY_CONFIG_B_TO_A}"
	if [ -n "${expected_id}" ]; then
		inspect="$(docker container inspect "${expected_id}" 2>/dev/null)" || {
			if docker container inspect "${name}" >/dev/null 2>&1; then
				die "relay name ${name} was reused after its recorded container disappeared"
			fi
			return 1
		}
	else
		inspect="$(docker container inspect "${name}" 2>/dev/null)" || return 1
	fi
	expected_image_id="$(jq -r '.image_id' "${RELAY_RECEIPT}")"
	network_a_id="$(jq -r '.networks.a.id' "${RELAY_RECEIPT}")"
	network_b_id="$(jq -r '.networks.b.id' "${RELAY_RECEIPT}")"
	jq --exit-status \
		--arg id "${expected_id}" --arg image_id "${expected_image_id}" \
		--arg image "${RELAY_IMAGE}" --arg name "${name}" --arg owner "${RELAY_OWNER}" \
		--arg direction "${direction}" --arg generation "${expected_generation}" \
		--arg config "${config}" \
		--arg local_network "${local_network}" --arg remote_network "${remote_network}" \
		--arg local_ip "${expected_local_ip}" \
		--arg network_a "${NETWORK_A}" --arg network_b "${NETWORK_B}" \
		--arg network_a_id "${network_a_id}" --arg network_b_id "${network_b_id}" \
		--arg strict "${strict}" '
      .[0] as $container |
      ($container.NetworkSettings.Networks | keys) as $networks |
      ($id == "" or $container.Id == $id) and
      ($image_id == "" or $container.Image == $image_id) and
      ($container.Name | ltrimstr("/")) == $name and
      $container.Config.Image == $image and
      $container.Config.Labels."dev.fgentic.federation-split" == $owner and
      $container.Config.Labels."dev.fgentic.federation-split.direction" == $direction and
      $container.Config.Labels."dev.fgentic.federation-split.generation" == $generation and
      any($container.Mounts[]?;
        .Type == "bind" and .Source == $config and
        .Destination == "/etc/confd/values.yaml" and .RW == false) and
      (($networks | index($local_network)) != null) and
      ($strict == "no" or $local_ip == "" or
        $container.NetworkSettings.Networks[$local_network].IPAddress == $local_ip) and
      (all($networks[]; . == $local_network or . == $remote_network)) and
      (if ($networks | index($network_a)) != null then
        $container.NetworkSettings.Networks[$network_a].NetworkID == $network_a_id
       else true end) and
      (if ($networks | index($network_b)) != null then
        $container.NetworkSettings.Networks[$network_b].NetworkID == $network_b_id
       else true end) and
      (if $strict == "yes" then
        ($container.State.Running == true) and $networks == ([$local_network, $remote_network] | sort)
       elif $strict == "stopped" then
        ($container.State.Running == false) and $networks == ([$local_network, $remote_network] | sort)
       else true end)
    ' <<<"${inspect}" >/dev/null || die "relay ${name} identity or ownership changed"
}

record_relay_identity() {
	local direction="$1"
	local id="$2"
	local image_id="$3"
	atomic_receipt_update \
		'(.relays[] | select(.direction == $direction).id) = $id | .image_id = $image_id' \
		--arg direction "${direction}" --arg id "${id}" --arg image_id "${image_id}"
}

create_directional_relay() {
	local config direction generation id image_id local_network name object remote_network
	direction="$1"
	object="$(relay_object "${direction}")"
	name="$(jq -r '.name' <<<"${object}")"
	generation="$(jq -r '.generation' <<<"${object}")"
	local_network="$(jq -r '.local_network' <<<"${object}")"
	remote_network="$(jq -r '.remote_network' <<<"${object}")"
	config="${RELAY_CONFIG_A_TO_B}"
	[ "${direction}" = a-to-b ] || config="${RELAY_CONFIG_B_TO_A}"
	id="$(docker create \
		--name "${name}" \
		--network "${local_network}" \
		--label "dev.fgentic.federation-split=${RELAY_OWNER}" \
		--label "dev.fgentic.federation-split.direction=${direction}" \
		--label "dev.fgentic.federation-split.generation=${generation}" \
		--mount "type=bind,src=${config},dst=/etc/confd/values.yaml,readonly" \
		"${RELAY_IMAGE}")" || die "could not create directional relay ${name}"
	[[ "${id}" =~ ^[0-9a-f]{12,64}$ ]] || die "Docker returned an invalid relay identity"
	image_id="$(docker container inspect --format '{{.Image}}' "${id}")" ||
		die "could not capture relay image identity"
	record_relay_identity "${direction}" "${id}" "${image_id}"
	docker network connect "${remote_network}" "${id}" ||
		die "could not attach ${name} to its remote control-plane network"
	docker start "${id}" >/dev/null || die "could not start directional relay ${name}"
}

assert_only_relays_dual_attached() {
	local containers_a containers_b expected intersection network_a network_b
	assert_receipted_owner_inventory
	network_a="$(docker network inspect "${NETWORK_A}")"
	network_b="$(docker network inspect "${NETWORK_B}")"
	expected="$(jq -c '[.relays[].id] | sort' "${RELAY_RECEIPT}")"
	containers_a="$(jq -c '.[0].Containers | keys' <<<"${network_a}")"
	containers_b="$(jq -c '.[0].Containers | keys' <<<"${network_b}")"
	intersection="$(jq -cn --argjson a "${containers_a}" --argjson b "${containers_b}" \
		'$a | map(select(. as $id | $b | index($id))) | sort')"
	[ "${intersection}" = "${expected}" ] ||
		die "only the two exact relay containers may attach to both split networks"
}

owner_relay_ids() {
	local ids_output
	local -a ids
	ids_output="$(docker container ls --all --quiet --no-trunc \
		--filter "label=dev.fgentic.federation-split=${RELAY_OWNER}")" ||
		die "could not inspect split relay ownership labels"
	if [ -z "${ids_output}" ]; then
		printf '[]\n'
		return
	fi
	mapfile -t ids <<<"${ids_output}"
	docker container inspect "${ids[@]}" |
		jq -ce --arg owner "${RELAY_OWNER}" '
      [.[] | select(.Config.Labels."dev.fgentic.federation-split" == $owner) | .Id] | sort
    ' || die "could not resolve exact split relay ownership identities"
}

assert_receipted_owner_inventory() {
	local actual expected
	expected="$(jq -c '[.relays[].id | select(length > 0)] | sort' "${RELAY_RECEIPT}")"
	actual="$(owner_relay_ids)"
	[ "${actual}" = "${expected}" ] ||
		die "split relay owner-label inventory differs from the receipt: ${actual}"
}

activate_relay_receipt() {
	local id_a id_b ip_a_to_b ip_b_to_a
	validate_relay_container a-to-b yes
	validate_relay_container b-to-a yes
	id_a="$(jq -r '.relays[] | select(.direction == "a-to-b").id' "${RELAY_RECEIPT}")"
	id_b="$(jq -r '.relays[] | select(.direction == "b-to-a").id' "${RELAY_RECEIPT}")"
	ip_a_to_b="$(docker inspect "${id_a}" |
		jq -er --arg network "${NETWORK_A}" '.[0].NetworkSettings.Networks[$network].IPAddress')"
	ip_b_to_a="$(docker inspect "${id_b}" |
		jq -er --arg network "${NETWORK_B}" '.[0].NetworkSettings.Networks[$network].IPAddress')"
	atomic_receipt_update '
      .phase = "active" |
      (.relays[] | select(.direction == "a-to-b").local_ip) = $ip_a |
      (.relays[] | select(.direction == "b-to-a").local_ip) = $ip_b
    ' --arg ip_a "${ip_a_to_b}" --arg ip_b "${ip_b_to_a}"
	validate_relay_container a-to-b yes
	validate_relay_container b-to-a yes
	assert_only_relays_dual_attached
}

ensure_no_unreceipted_relays() {
	local name owned
	for name in "${RELAY_A_TO_B}" "${RELAY_B_TO_A}"; do
		if docker container inspect "${name}" >/dev/null 2>&1; then
			die "refusing unreceipted same-named relay ${name}"
		fi
	done
	owned="$(owner_relay_ids)"
	[ "${owned}" = '[]' ] ||
		die "refusing unreceipted owner-labelled split relay containers: ${owned}"
}

relay_containers_present() {
	docker container inspect "${RELAY_A_TO_B}" >/dev/null 2>&1 ||
		docker container inspect "${RELAY_B_TO_A}" >/dev/null 2>&1
}

ensure_active_relays() {
	local phase
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		validate_relay_receipt_file || die "split relay receipt is malformed or stale"
		phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
		case "${phase}" in
		active)
			validate_receipt_networks
			validate_relay_container a-to-b yes
			validate_relay_container b-to-a yes
			assert_only_relays_dual_attached
			return
			;;
		stopped)
			validate_receipt_networks
			validate_relay_container a-to-b stopped
			validate_relay_container b-to-a stopped
			assert_only_relays_dual_attached
			docker start "$(jq -r '.relays[] | select(.direction == "a-to-b").id' \
				"${RELAY_RECEIPT}")" >/dev/null || die "could not restart A-to-B relay"
			docker start "$(jq -r '.relays[] | select(.direction == "b-to-a").id' \
				"${RELAY_RECEIPT}")" >/dev/null || die "could not restart B-to-A relay"
			activate_relay_receipt
			return
			;;
		*) die "split relay recovery is pending; run fed:split:down before up" ;;
		esac
	fi
	ensure_no_unreceipted_relays
	write_creating_relay_receipt
	create_directional_relay a-to-b
	create_directional_relay b-to-a
	activate_relay_receipt
}

validate_child_teardown_receipt_file() {
	local cluster="$2"
	local owner="$3"
	local receipt="$1"
	[ -f "${receipt}" ] && [ ! -L "${receipt}" ] || return 1
	jq --exit-status --arg cluster "${cluster}" --arg owner "${owner}" '
      . as $receipt |
      keys == ["cluster", "containers", "generation", "images", "network", "owner", "profile", "schema", "volumes"] and
      .schema == "fgentic.cluster-teardown.v1" and
      .profile == "federation" and .cluster == $cluster and .owner == $owner and
      (.generation | type == "string" and length > 0) and
      ([.containers[] | select(
        .id == $receipt.generation and .name == ("k3d-" + $cluster + "-server-0")
      )] | length == 1) and
      .network.name == ("k3d-" + $cluster) and
      (.network.id | type == "string" and length > 0) and
      (.network.cluster_label == "" or .network.cluster_label == $cluster) and
      ([.volumes[] | select(
        .kind == "images" and .name == ("k3d-" + $cluster + "-images") and
        (.created_at | type == "string" and length > 0)
      )] | length == 1)
    ' "${receipt}" >/dev/null 2>&1
}

child_teardown_snapshot() {
	local cluster="$2"
	local layout="$1"
	local owner="$4"
	local receipt="$3"
	if [ ! -e "${receipt}" ] && [ ! -L "${receipt}" ]; then
		jq --null-input --compact-output \
			--arg cluster "${cluster}" --arg layout "${layout}" --arg owner "${owner}" \
			'{state: "absent", layout: $layout, cluster: $cluster, owner: $owner}'
		return
	fi
	validate_child_teardown_receipt_file "${receipt}" "${cluster}" "${owner}" ||
		die "split child teardown receipt is malformed or stale: ${receipt}"
	jq --compact-output \
		--arg layout "${layout}" --arg sha256 "$(file_sha256 "${receipt}")" '
      . as $receipt |
      {
        state: "present",
        layout: $layout,
        cluster: .cluster,
        owner: .owner,
        receipt_sha256: $sha256,
        generation: .generation,
        server: (.containers[] | select(.id == $receipt.generation)),
        network: .network,
        image_volume: (.volumes[] | select(.kind == "images"))
      }
    ' "${receipt}"
}

prepare_child_teardown_snapshot() {
	local ca_dir="$3"
	local cluster="$2"
	local layout="$1"
	local owner="$5"
	local receipt="$4"
	run_child "${layout}" "${cluster}" "${ca_dir}" lifecycle prepare-down >/dev/null
	child_teardown_snapshot "${layout}" "${cluster}" "${receipt}" "${owner}"
}

ca_teardown_snapshot() {
	local state
	state="$(validate_stable_ca_state)"
	if [ "${state}" = absent ]; then
		jq --null-input --compact-output '{state: "absent"}'
		return
	fi
	jq --null-input --compact-output \
		--arg receipt_sha256 "$(file_sha256 "${CA_RECEIPT}")" \
		--argjson roots "$(jq -c '.roots' "${CA_RECEIPT}")" \
		--arg a_cert "$(file_sha256 "${CA_DIR_A}/ca.crt")" \
		--arg a_key "$(file_sha256 "${CA_DIR_A}/ca.key")" \
		--arg b_cert "$(file_sha256 "${CA_DIR_B}/ca.crt")" \
		--arg b_key "$(file_sha256 "${CA_DIR_B}/ca.key")" \
		--arg bundle "$(file_sha256 "${HOST_CA_BUNDLE}")" \
		--arg receipt "$(file_sha256 "${CA_RECEIPT}")" '{
      state: "present",
      receipt_sha256: $receipt_sha256,
      roots: $roots,
      files: [
        {path: "ca/org-a/ca.crt", sha256: $a_cert},
        {path: "ca/org-a/ca.key", sha256: $a_key},
        {path: "ca/org-b/ca.crt", sha256: $b_cert},
        {path: "ca/org-b/ca.key", sha256: $b_key},
        {path: "ca/host-bundle.pem", sha256: $bundle},
        {path: "ca/roots.json", sha256: $receipt}
      ]
    }'
}

validate_relay_config_file() {
	local expected_target="$2"
	local path="$1"
	[ -f "${path}" ] && [ ! -L "${path}" ] || return 1
	RELAY_TARGET="${expected_target}" yq --exit-status '
      ((keys | length) == 2) and has("ports") and has("settings") and
      ((.ports | keys | length) == 1) and (.ports | has("443.tcp")) and
      ((.ports."443.tcp" | length) == 1) and
      .ports."443.tcp"[0] == strenv(RELAY_TARGET) and
      ((.settings | keys | length) == 2) and
      .settings.workerConnections == 1024 and .settings.defaultProxyTimeout == 600
    ' "${path}" >/dev/null 2>&1
}

validate_relay_config_inventory() {
	local path
	if [ ! -e "${RELAY_CONFIG_DIR}" ] && [ ! -L "${RELAY_CONFIG_DIR}" ]; then
		return
	fi
	[ -d "${RELAY_CONFIG_DIR}" ] && [ ! -L "${RELAY_CONFIG_DIR}" ] ||
		die "split relay config state is not a non-symlink directory"
	for path in "${RELAY_CONFIG_DIR}"/* "${RELAY_CONFIG_DIR}"/.[!.]* \
		"${RELAY_CONFIG_DIR}"/..?*; do
		[ -e "${path}" ] || [ -L "${path}" ] || continue
		case "${path}" in
		"${RELAY_CONFIG_A_TO_B}" | "${RELAY_CONFIG_B_TO_A}")
			[ -f "${path}" ] && [ ! -L "${path}" ] ||
				die "split relay config must be a regular non-symlink file: ${path}"
			;;
		"${RELAY_CONFIG_DIR}"/.relay.*)
			[ -f "${path}" ] && [ ! -L "${path}" ] ||
				die "split relay config atomic state changed type: ${path}"
			;;
		*) die "unexpected file in split relay config state: ${path}" ;;
		esac
	done
}

relay_config_snapshot() {
	local config_a="" config_b=""
	if [ -e "${RELAY_CONFIG_A_TO_B}" ] || [ -L "${RELAY_CONFIG_A_TO_B}" ]; then
		validate_relay_config_file "${RELAY_CONFIG_A_TO_B}" "${SERVERLB_B}" ||
			die "unreceipted A-to-B relay config is malformed"
		config_a="$(file_sha256 "${RELAY_CONFIG_A_TO_B}")"
	fi
	if [ -e "${RELAY_CONFIG_B_TO_A}" ] || [ -L "${RELAY_CONFIG_B_TO_A}" ]; then
		validate_relay_config_file "${RELAY_CONFIG_B_TO_A}" "${SERVERLB_A}" ||
			die "unreceipted B-to-A relay config is malformed"
		config_b="$(file_sha256 "${RELAY_CONFIG_B_TO_A}")"
	fi
	validate_relay_config_inventory
	jq --null-input --compact-output --arg a "${config_a}" --arg b "${config_b}" \
		'{a_to_b_sha256: $a, b_to_a_sha256: $b}'
}

finalize_relay_identity_for_teardown() {
	local direction="$1"
	local id image_id name object
	object="$(relay_object "${direction}")"
	id="$(jq -r '.id' <<<"${object}")"
	name="$(jq -r '.name' <<<"${object}")"
	if [ -n "${id}" ]; then
		validate_relay_container "${direction}" no || return 1
		return
	fi
	if ! validate_relay_container "${direction}" no; then
		return 1
	fi
	id="$(docker container inspect --format '{{.Id}}' "${name}")" ||
		die "could not capture interrupted relay identity for ${name}"
	image_id="$(docker container inspect --format '{{.Image}}' "${id}")" ||
		die "could not capture interrupted relay image identity for ${name}"
	[[ "${id}" =~ ^[0-9a-f]{12,64}$ ]] ||
		die "Docker returned an invalid interrupted relay identity"
	record_relay_identity "${direction}" "${id}" "${image_id}"
	validate_relay_container "${direction}" no
}

assert_live_relay_inventory() {
	local actual direction expected id recorded_id
	expected='[]'
	for direction in a-to-b b-to-a; do
		recorded_id="$(jq -r --arg direction "${direction}" \
			'.relays[] | select(.direction == $direction).id' "${RELAY_RECEIPT}")"
		if finalize_relay_identity_for_teardown "${direction}"; then
			id="$(jq -r --arg direction "${direction}" \
				'.relays[] | select(.direction == $direction).id' "${RELAY_RECEIPT}")"
			expected="$(jq -c --arg id "${id}" '. + [$id] | sort' <<<"${expected}")"
		elif [ -n "${recorded_id}" ]; then
			die "recorded relay ${direction} disappeared before parent teardown was committed"
		fi
	done
	actual="$(owner_relay_ids)"
	[ "${actual}" = "${expected}" ] ||
		die "split relay owner-label inventory differs from the live receipt: ${actual}"
}

relay_teardown_snapshot() {
	local config_snapshot direction id inventory phase receipt_snapshot state
	if [ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ]; then
		ensure_no_unreceipted_relays
		config_snapshot="$(relay_config_snapshot)"
		state=absent
		if jq -e '.a_to_b_sha256 != "" or .b_to_a_sha256 != ""' \
			<<<"${config_snapshot}" >/dev/null; then
			state='config-only'
		fi
		jq --null-input --compact-output --arg state "${state}" \
			--argjson configs "${config_snapshot}" \
			'{state: $state, configs: $configs}'
		return
	fi
	validate_relay_receipt_file || die "split relay receipt is malformed or stale"
	validate_receipt_networks
	phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
	case "${phase}" in
	active)
		validate_relay_container a-to-b yes
		validate_relay_container b-to-a yes
		;;
	stopped)
		validate_relay_container a-to-b stopped
		validate_relay_container b-to-a stopped
		;;
	creating | removing) ;;
	*) die "unsupported split relay phase ${phase}" ;;
	esac
	assert_live_relay_inventory
	if [ "${phase}" != removing ]; then
		atomic_receipt_update '.phase = "removing"'
	fi
	assert_live_relay_inventory
	receipt_snapshot="$(jq -c . "${RELAY_RECEIPT}")"
	inventory='[]'
	for direction in a-to-b b-to-a; do
		id="$(jq -r --arg direction "${direction}" \
			'.relays[] | select(.direction == $direction).id' "${RELAY_RECEIPT}")"
		state=absent
		if [ -n "${id}" ] && validate_relay_container "${direction}" no; then
			state=present
		fi
		inventory="$(jq -c --arg direction "${direction}" --arg state "${state}" \
			--arg id "${id}" --arg name "$(jq -r --arg direction "${direction}" \
				'.relays[] | select(.direction == $direction).name' "${RELAY_RECEIPT}")" \
			'. + [{direction: $direction, state: $state, id: $id, name: $name}]' \
			<<<"${inventory}")"
	done
	jq --null-input --compact-output \
		--arg receipt_sha256 "$(file_sha256 "${RELAY_RECEIPT}")" \
		--arg source_phase "${phase}" --argjson receipt "${receipt_snapshot}" \
		--argjson inventory "${inventory}" '{
      state: "present",
      source_phase: $source_phase,
      receipt_sha256: $receipt_sha256,
      receipt: $receipt,
      inventory: $inventory
    }'
}

validate_parent_teardown_receipt_file() {
	local receipt="${1:-${PARENT_TEARDOWN_RECEIPT}}"
	[ -f "${receipt}" ] && [ ! -L "${receipt}" ] || return 1
	jq --exit-status \
		--arg cluster_a "${CLUSTER_A}" --arg cluster_b "${CLUSTER_B}" \
		--arg layout_a "${LAYOUT_A}" --arg layout_b "${LAYOUT_B}" \
		--arg owner_a federation-split-a --arg owner_b federation-split-b \
		--arg relay_image "${RELAY_IMAGE}" '
      def hash: type == "string" and test("^sha256:[0-9a-f]{64}$");
      def child($layout; $cluster; $owner):
        if .state == "absent" then
          keys == ["cluster", "layout", "owner", "state"] and
          .layout == $layout and .cluster == $cluster and .owner == $owner
        elif .state == "present" then
          keys == ["cluster", "generation", "image_volume", "layout", "network", "owner",
            "receipt_sha256", "server", "state"] and
          .layout == $layout and .cluster == $cluster and .owner == $owner and
          (.receipt_sha256 | hash) and
          (.generation | type == "string" and length > 0) and
          .server == {id: .generation, name: ("k3d-" + $cluster + "-server-0")} and
          (.network | keys == ["cluster_label", "id", "name"]) and
          .network.name == ("k3d-" + $cluster) and
          (.network.id | type == "string" and length > 0) and
          (.network.cluster_label == "" or .network.cluster_label == $cluster) and
          (.image_volume | keys == ["attachments", "created_at", "kind", "name"]) and
          .image_volume.kind == "images" and
          .image_volume.name == ("k3d-" + $cluster + "-images") and
          (.image_volume.created_at | type == "string" and length > 0)
        else false end;
      def ca:
        if .state == "absent" then keys == ["state"]
        elif .state == "present" then
          keys == ["files", "receipt_sha256", "roots", "state"] and
          (.receipt_sha256 | hash) and
          (.roots | keys == ["a", "b"]) and all(.roots[]; hash) and
          .roots.a != .roots.b and
          (.files | length == 6) and
          (all(.files[]; keys == ["path", "sha256"] and (.sha256 | hash))) and
          ([.files[].path] | sort) == ([
            "ca/host-bundle.pem", "ca/org-a/ca.crt", "ca/org-a/ca.key",
            "ca/org-b/ca.crt", "ca/org-b/ca.key", "ca/roots.json"
          ] | sort) and
          .receipt_sha256 ==
            ([.files[] | select(.path == "ca/roots.json").sha256][0])
        else false end;
      def configs:
        keys == ["a_to_b_sha256", "b_to_a_sha256"] and
        all(.[]; . == "" or hash);
      def relay_source:
        keys == ["image", "image_id", "networks", "phase", "relays", "schema"] and
        .schema == "fgentic.federation-split-relays.v1" and .phase == "removing" and
        .image == $relay_image and (.image_id | type == "string") and
        (.networks | keys == ["a", "b"]) and
        .networks.a.cluster == $cluster_a and
        .networks.a.name == ("k3d-" + $cluster_a) and
        .networks.b.cluster == $cluster_b and
        .networks.b.name == ("k3d-" + $cluster_b) and
        (all(.networks[]; keys == ["cluster", "id", "name"] and
          (.id | type == "string" and length > 0))) and
        .networks.a.id != .networks.b.id and
        (.relays | length == 2) and
        (all(.relays[];
          keys == ["config_sha256", "direction", "generation", "id", "local_ip",
            "local_network", "name", "remote_network", "target"] and
          (.config_sha256 | hash) and
          (.generation | type == "string" and test("^[0-9a-f]{32}$")) and
          (.id | type == "string") and (.local_ip | type == "string"))) and
        ([.relays[] | select(
          .direction == "a-to-b" and .name == "fgentic-fed-a-to-b" and
          .local_network == ("k3d-" + $cluster_a) and
          .remote_network == ("k3d-" + $cluster_b) and
          .target == "k3d-fgentic-fed-b-serverlb"
        )] | length == 1) and
        ([.relays[] | select(
          .direction == "b-to-a" and .name == "fgentic-fed-b-to-a" and
          .local_network == ("k3d-" + $cluster_b) and
          .remote_network == ("k3d-" + $cluster_a) and
          .target == "k3d-fgentic-fed-a-serverlb"
        )] | length == 1) and
        .relays[0].generation != .relays[1].generation;
      def relays:
        if .state == "absent" or .state == "config-only" then
          keys == ["configs", "state"] and (.configs | configs) and
          (if .state == "config-only" then
             .configs.a_to_b_sha256 != "" or .configs.b_to_a_sha256 != ""
           else
             .configs.a_to_b_sha256 == "" and .configs.b_to_a_sha256 == ""
           end)
        elif .state == "present" then
          keys == ["inventory", "receipt", "receipt_sha256", "source_phase", "state"] and
          (.receipt_sha256 | hash) and
          (.source_phase == "creating" or .source_phase == "active" or
           .source_phase == "stopped" or .source_phase == "removing") and
          (.receipt | relay_source) and
          (.inventory | length == 2) and
          (all(.inventory[];
            keys == ["direction", "id", "name", "state"] and
            (.state == "present" or .state == "absent") and
            (.id | type == "string"))) and
          ([.inventory[].direction] | sort) == ["a-to-b", "b-to-a"] and
          ([.inventory[] | select(
            .direction == "a-to-b" and .name == "fgentic-fed-a-to-b"
          )] | length == 1) and
          ([.inventory[] | select(
            .direction == "b-to-a" and .name == "fgentic-fed-b-to-a"
          )] | length == 1) and
          all(.inventory[];
            if .state == "present" then (.id | length > 0) else .id == "" end) and
          ([.inventory[] | [.direction, .id, .name]] | sort) ==
            ([.receipt.relays[] | [.direction, .id, .name]] | sort) and
          (if any(.inventory[]; .state == "present") then
            (.receipt.image_id | type == "string" and length > 0)
           else true end) and
          (if .source_phase == "active" or .source_phase == "stopped" then
            all(.inventory[]; .state == "present")
           else true end)
        else false end;
      keys == ["ca", "children", "phase", "relays", "schema"] and
      .schema == "fgentic.federation-split-teardown.v1" and .phase == "removing" and
      (.children | keys == ["a", "b"]) and
      (.children.a | child($layout_a; $cluster_a; $owner_a)) and
      (.children.b | child($layout_b; $cluster_b; $owner_b)) and
      (.ca | ca) and (.relays | relays) and
      (if .relays.state == "present" then
        .children.a.state == "present" and .children.b.state == "present" and
        .ca.state == "present" and
        .children.a.network.id == .relays.receipt.networks.a.id and
        .children.b.network.id == .relays.receipt.networks.b.id
       elif .children.a.state == "present" or .children.b.state == "present" then
        .ca.state == "present"
       else true end)
    ' "${receipt}" >/dev/null 2>&1
}

write_parent_teardown_receipt() {
	local ca_snapshot="$3"
	local child_a="$1"
	local child_b="$2"
	local relay_snapshot="$4"
	local temporary
	[ ! -e "${PARENT_TEARDOWN_RECEIPT}" ] &&
		[ ! -L "${PARENT_TEARDOWN_RECEIPT}" ] ||
		die "split parent teardown receipt already exists"
	ensure_state_directory
	temporary="$(mktemp "${SPLIT_STATE_DIR}/.teardown.XXXXXX")"
	chmod 600 "${temporary}"
	if ! jq --null-input \
		--argjson child_a "${child_a}" --argjson child_b "${child_b}" \
		--argjson ca "${ca_snapshot}" --argjson relays "${relay_snapshot}" '{
      schema: "fgentic.federation-split-teardown.v1",
      phase: "removing",
      children: {a: $child_a, b: $child_b},
      ca: $ca,
      relays: $relays
    }' >"${temporary}"; then
		rm -f "${temporary}"
		die "could not construct split parent teardown receipt"
	fi
	if ! validate_parent_teardown_receipt_file "${temporary}"; then
		rm -f "${temporary}"
		die "refusing to persist an invalid split parent teardown receipt"
	fi
	mv "${temporary}" "${PARENT_TEARDOWN_RECEIPT}" ||
		die "could not atomically persist split parent teardown receipt"
	validate_parent_teardown_receipt_file ||
		die "persisted split parent teardown receipt is invalid"
}

child_down_preflight_state() {
	local ca_dir="$3"
	local cluster="$2"
	local layout="$1"
	local output
	output="$(run_child "${layout}" "${cluster}" "${ca_dir}" lifecycle status)" ||
		die "could not inspect split child ${cluster} before teardown"
	if rg --fixed-strings "Federation cluster ${cluster}: state=absent retained_bytes=0" \
		<<<"${output}" >/dev/null; then
		printf 'absent\n'
	else
		printf 'present\n'
	fi
}

preflight_relay_teardown_state() {
	local actual direction expected id name object phase
	if [ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ]; then
		ensure_no_unreceipted_relays
		relay_config_snapshot >/dev/null
		printf 'absent\n'
		return
	fi
	validate_relay_receipt_file || die "split relay receipt is malformed or stale"
	validate_receipt_networks
	phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
	case "${phase}" in
	active)
		validate_relay_container a-to-b yes
		validate_relay_container b-to-a yes
		;;
	stopped)
		validate_relay_container a-to-b stopped
		validate_relay_container b-to-a stopped
		;;
	creating | removing) ;;
	*) die "unsupported split relay phase ${phase}" ;;
	esac
	expected='[]'
	for direction in a-to-b b-to-a; do
		object="$(relay_object "${direction}")"
		id="$(jq -r '.id' <<<"${object}")"
		name="$(jq -r '.name' <<<"${object}")"
		if validate_relay_container "${direction}" no; then
			if [ -z "${id}" ]; then
				id="$(docker container inspect --format '{{.Id}}' "${name}")" ||
					die "could not inspect interrupted relay ${name}"
			fi
			expected="$(jq -c --arg id "${id}" '. + [$id] | sort' <<<"${expected}")"
		elif [ -n "${id}" ]; then
			die "recorded relay ${name} disappeared before teardown was receipted"
		fi
	done
	actual="$(owner_relay_ids)"
	[ "${actual}" = "${expected}" ] ||
		die "split relay owner-label inventory differs before teardown: ${actual}"
	printf 'present\n'
}

prepare_parent_teardown_receipt() {
	local ca_snapshot ca_state child_a child_a_state child_b child_b_state relay_snapshot relay_state
	validate_temporary_state
	ca_state="$(validate_stable_ca_state)"
	child_a_state="$(child_down_preflight_state "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}")"
	child_b_state="$(child_down_preflight_state "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}")"
	relay_state="$(preflight_relay_teardown_state)"
	if [ "${relay_state}" = present ] &&
		{ [ "${child_a_state}" = absent ] || [ "${child_b_state}" = absent ]; }; then
		die "refusing to capture relays after an unreceipted child generation disappeared"
	fi
	if { [ "${child_a_state}" = present ] || [ "${child_b_state}" = present ] ||
		[ "${relay_state}" = present ]; } && [ "${ca_state}" != complete ]; then
		die "split child or relay resources exist without the complete receipted CA generation"
	fi
	cleanup_temporary_state
	child_b="$(prepare_child_teardown_snapshot "${LAYOUT_B}" "${CLUSTER_B}" \
		"${CA_DIR_B}" "${CHILD_TEARDOWN_RECEIPT_B}" federation-split-b)"
	child_a="$(prepare_child_teardown_snapshot "${LAYOUT_A}" "${CLUSTER_A}" \
		"${CA_DIR_A}" "${CHILD_TEARDOWN_RECEIPT_A}" federation-split-a)"
	relay_snapshot="$(relay_teardown_snapshot)"
	ca_snapshot="$(ca_teardown_snapshot)"
	write_parent_teardown_receipt "${child_a}" "${child_b}" \
		"${ca_snapshot}" "${relay_snapshot}"
}

validate_parent_child_absence() {
	local ca_dir="$5"
	local cluster="$2"
	local image_volume_created image_volume_name network_id network_name
	local object="$1"
	local layout="$4"
	local server_id server_name
	server_id="$(jq -r '.server.id' <<<"${object}")"
	server_name="$(jq -r '.server.name' <<<"${object}")"
	network_id="$(jq -r '.network.id' <<<"${object}")"
	network_name="$(jq -r '.network.name' <<<"${object}")"
	image_volume_name="$(jq -r '.image_volume.name' <<<"${object}")"
	image_volume_created="$(jq -r '.image_volume.created_at' <<<"${object}")"
	if docker container inspect "${server_id}" >/dev/null 2>&1; then
		die "recorded child server ${server_name} remains after its teardown receipt disappeared"
	fi
	if docker container inspect "${server_name}" >/dev/null 2>&1; then
		die "child server name ${server_name} was reused after the recorded generation disappeared"
	fi
	if docker network inspect "${network_id}" >/dev/null 2>&1; then
		die "recorded child network ${network_name} remains after its teardown receipt disappeared"
	fi
	if docker network inspect "${network_name}" >/dev/null 2>&1; then
		die "child network name ${network_name} was reused after the recorded generation disappeared"
	fi
	if docker volume inspect "${image_volume_name}" >/dev/null 2>&1; then
		if [ "$(docker volume inspect --format '{{.CreatedAt}}' "${image_volume_name}")" != \
			"${image_volume_created}" ]; then
			die "child image volume ${image_volume_name} was reused with a new creation identity"
		fi
		die "recorded child image volume ${image_volume_name} remains after its teardown receipt disappeared"
	fi
	require_child_absent_after_down "${layout}" "${cluster}" "${ca_dir}"
}

validate_parent_child_resources() {
	local ca_dir="$5"
	local cluster="$3"
	local key="$1"
	local layout="$2"
	local owner="$6"
	local receipt="$4"
	local object state
	object="$(jq -c --arg key "${key}" '.children[$key]' "${PARENT_TEARDOWN_RECEIPT}")"
	state="$(jq -r '.state' <<<"${object}")"
	if [ "${state}" = absent ]; then
		[ ! -e "${receipt}" ] && [ ! -L "${receipt}" ] ||
			die "unexpected child teardown receipt appeared for absent ${cluster}"
		require_child_absent_after_down "${layout}" "${cluster}" "${ca_dir}"
		return
	fi
	if [ -e "${receipt}" ] || [ -L "${receipt}" ]; then
		validate_child_teardown_receipt_file "${receipt}" "${cluster}" "${owner}" ||
			die "split child teardown receipt changed for ${cluster}"
		[ "$(file_sha256 "${receipt}")" = "$(jq -r '.receipt_sha256' <<<"${object}")" ] ||
			die "split child teardown receipt digest changed for ${cluster}"
		run_child "${layout}" "${cluster}" "${ca_dir}" lifecycle status >/dev/null ||
			die "could not validate split child receipt resources for ${cluster}"
		return
	fi
	validate_parent_child_absence "${object}" "${cluster}" "${owner}" "${layout}" "${ca_dir}"
}

validate_parent_network_object() {
	local actual actual_id cluster id name object="$1"
	id="$(jq -r '.id' <<<"${object}")"
	name="$(jq -r '.name' <<<"${object}")"
	cluster="$(jq -r '.cluster' <<<"${object}")"
	if actual="$(docker network inspect "${id}" 2>/dev/null)"; then
		jq --exit-status --arg id "${id}" --arg name "${name}" --arg cluster "${cluster}" '
        length == 1 and .[0].Id == $id and .[0].Name == $name and
        .[0].Labels.app == "k3d" and
        ((.[0].Labels."k3d.cluster" // "") == "" or
         .[0].Labels."k3d.cluster" == $cluster)
      ' <<<"${actual}" >/dev/null ||
			die "recorded split network identity or ownership changed for ${name}"
		return 0
	fi
	if actual="$(docker network inspect "${name}" 2>/dev/null)"; then
		actual_id="$(jq -r '.[0].Id' <<<"${actual}")"
		die "split network name ${name} was reused by ${actual_id}"
	fi
	return 1
}

validate_parent_relay_container() {
	local config direction expected_id expected_image_id generation inspect
	local local_network name network_a_id network_b_id object remote_network
	direction="$1"
	expected_id="$(jq -r --arg direction "${direction}" \
		'.relays.inventory[] | select(.direction == $direction).id' \
		"${PARENT_TEARDOWN_RECEIPT}")"
	object="$(jq -c --arg direction "${direction}" \
		'.relays.receipt.relays[] | select(.direction == $direction)' \
		"${PARENT_TEARDOWN_RECEIPT}")"
	name="$(jq -r '.name' <<<"${object}")"
	generation="$(jq -r '.generation' <<<"${object}")"
	local_network="$(jq -r '.local_network' <<<"${object}")"
	remote_network="$(jq -r '.remote_network' <<<"${object}")"
	config="${RELAY_CONFIG_A_TO_B}"
	[ "${direction}" = a-to-b ] || config="${RELAY_CONFIG_B_TO_A}"
	if ! inspect="$(docker container inspect "${expected_id}" 2>/dev/null)"; then
		if docker container inspect "${name}" >/dev/null 2>&1; then
			die "relay name ${name} was reused after its parent-recorded container disappeared"
		fi
		return 1
	fi
	expected_image_id="$(jq -r '.relays.receipt.image_id' "${PARENT_TEARDOWN_RECEIPT}")"
	network_a_id="$(jq -r '.relays.receipt.networks.a.id' "${PARENT_TEARDOWN_RECEIPT}")"
	network_b_id="$(jq -r '.relays.receipt.networks.b.id' "${PARENT_TEARDOWN_RECEIPT}")"
	jq --exit-status \
		--arg id "${expected_id}" --arg image_id "${expected_image_id}" \
		--arg image "${RELAY_IMAGE}" --arg name "${name}" --arg owner "${RELAY_OWNER}" \
		--arg direction "${direction}" --arg generation "${generation}" \
		--arg config "${config}" --arg local_network "${local_network}" \
		--arg remote_network "${remote_network}" --arg network_a "${NETWORK_A}" \
		--arg network_b "${NETWORK_B}" --arg network_a_id "${network_a_id}" \
		--arg network_b_id "${network_b_id}" '
      .[0] as $container |
      ($container.NetworkSettings.Networks | keys) as $networks |
      $container.Id == $id and $container.Image == $image_id and
      ($container.Name | ltrimstr("/")) == $name and
      $container.Config.Image == $image and
      $container.Config.Labels."dev.fgentic.federation-split" == $owner and
      $container.Config.Labels."dev.fgentic.federation-split.direction" == $direction and
      $container.Config.Labels."dev.fgentic.federation-split.generation" == $generation and
      any($container.Mounts[]?;
        .Type == "bind" and .Source == $config and
        .Destination == "/etc/confd/values.yaml" and .RW == false) and
      $networks == ([$local_network, $remote_network] | sort) and
      (if ($networks | index($network_a)) != null then
        $container.NetworkSettings.Networks[$network_a].NetworkID == $network_a_id
       else true end) and
      (if ($networks | index($network_b)) != null then
        $container.NetworkSettings.Networks[$network_b].NetworkID == $network_b_id
       else true end)
    ' <<<"${inspect}" >/dev/null ||
		die "parent-recorded relay ${name} identity or ownership changed"
}

validate_parent_config_file() {
	local expected_hash="$2"
	local path="$1"
	if [ -e "${path}" ] || [ -L "${path}" ]; then
		[ -f "${path}" ] && [ ! -L "${path}" ] ||
			die "parent-recorded relay config is not regular: ${path}"
		[ -n "${expected_hash}" ] && [ "$(file_sha256 "${path}")" = "${expected_hash}" ] ||
			die "parent-recorded relay config changed: ${path}"
		return 0
	fi
	[ -z "${expected_hash}" ] || return 1
	return 1
}

validate_parent_relay_resources() {
	local actual direction expected expected_hash id live='[]' name object state
	state="$(jq -r '.relays.state' "${PARENT_TEARDOWN_RECEIPT}")"
	validate_relay_config_inventory
	if [ "${state}" != present ]; then
		ensure_no_unreceipted_relays
		expected_hash="$(jq -r '.relays.configs.a_to_b_sha256' "${PARENT_TEARDOWN_RECEIPT}")"
		validate_parent_config_file "${RELAY_CONFIG_A_TO_B}" "${expected_hash}" || true
		expected_hash="$(jq -r '.relays.configs.b_to_a_sha256' "${PARENT_TEARDOWN_RECEIPT}")"
		validate_parent_config_file "${RELAY_CONFIG_B_TO_A}" "${expected_hash}" || true
		return
	fi
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		[ -f "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ] ||
			die "split relay receipt changed type during parent teardown"
		[ "$(file_sha256 "${RELAY_RECEIPT}")" = \
			"$(jq -r '.relays.receipt_sha256' "${PARENT_TEARDOWN_RECEIPT}")" ] ||
			die "split relay receipt digest changed during parent teardown"
		validate_relay_receipt_file ||
			die "split relay receipt became malformed during parent teardown"
	fi
	for direction in a-to-b b-to-a; do
		object="$(jq -c --arg direction "${direction}" \
			'.relays.inventory[] | select(.direction == $direction)' \
			"${PARENT_TEARDOWN_RECEIPT}")"
		id="$(jq -r '.id' <<<"${object}")"
		name="$(jq -r '.name' <<<"${object}")"
		if [ "$(jq -r '.state' <<<"${object}")" = present ]; then
			if validate_parent_relay_container "${direction}"; then
				live="$(jq -c --arg id "${id}" '. + [$id] | sort' <<<"${live}")"
			fi
		elif docker container inspect "${name}" >/dev/null 2>&1; then
			die "relay ${name} appeared after its absence was parent-receipted"
		fi
	done
	actual="$(owner_relay_ids)"
	expected="${live}"
	[ "${actual}" = "${expected}" ] ||
		die "split relay owner-label inventory differs from the parent receipt: ${actual}"
	validate_parent_network_object \
		"$(jq -c '.relays.receipt.networks.a' "${PARENT_TEARDOWN_RECEIPT}")" || true
	validate_parent_network_object \
		"$(jq -c '.relays.receipt.networks.b' "${PARENT_TEARDOWN_RECEIPT}")" || true
	expected_hash="$(jq -r '
      .relays.receipt.relays[] | select(.direction == "a-to-b").config_sha256
    ' "${PARENT_TEARDOWN_RECEIPT}")"
	if ! validate_parent_config_file "${RELAY_CONFIG_A_TO_B}" "${expected_hash}" &&
		[ "${live}" != '[]' ]; then
		die "live A-to-B relay lost its parent-recorded config"
	fi
	expected_hash="$(jq -r '
      .relays.receipt.relays[] | select(.direction == "b-to-a").config_sha256
    ' "${PARENT_TEARDOWN_RECEIPT}")"
	if ! validate_parent_config_file "${RELAY_CONFIG_B_TO_A}" "${expected_hash}" &&
		[ "${live}" != '[]' ]; then
		die "live B-to-A relay lost its parent-recorded config"
	fi
}

validate_parent_ca_inventory() {
	validate_ca_inventory "${SPLIT_STATE_DIR}/ca"
}

validate_parent_ca_resources() {
	local expected_hash path present=0 relative state
	state="$(jq -r '.ca.state' "${PARENT_TEARDOWN_RECEIPT}")"
	validate_parent_ca_inventory
	if [ "${state}" = absent ]; then
		[ ! -e "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ] ||
			die "split CA state appeared after its absence was parent-receipted"
		return
	fi
	while IFS= read -r relative; do
		path="${SPLIT_STATE_DIR}/${relative}"
		expected_hash="$(jq -r --arg path "${relative}" \
			'.ca.files[] | select(.path == $path).sha256' \
			"${PARENT_TEARDOWN_RECEIPT}")"
		if [ -e "${path}" ] || [ -L "${path}" ]; then
			[ -f "${path}" ] && [ ! -L "${path}" ] ||
				die "parent-recorded split CA file changed type: ${path}"
			[ "$(file_sha256 "${path}")" = "${expected_hash}" ] ||
				die "parent-recorded split CA file changed: ${path}"
			present=$((present + 1))
		fi
	done < <(jq -r '.ca.files[].path' "${PARENT_TEARDOWN_RECEIPT}")
	if [ "${present}" -eq 6 ]; then
		validate_ca_tree "${SPLIT_STATE_DIR}/ca"
	fi
	if [ -e "${CA_RECEIPT}" ] || [ -L "${CA_RECEIPT}" ]; then
		jq --exit-status \
			--argjson expected "$(jq -c '.ca.roots' "${PARENT_TEARDOWN_RECEIPT}")" \
			'.roots == $expected' "${CA_RECEIPT}" >/dev/null ||
			die "parent-recorded split CA roots changed"
	fi
}

validate_parent_teardown_resources() {
	validate_parent_teardown_receipt_file ||
		die "split parent teardown receipt is malformed"
	validate_parent_state_inventory
	validate_temporary_state
	validate_parent_relay_resources
	validate_parent_child_resources b "${LAYOUT_B}" "${CLUSTER_B}" \
		"${CHILD_TEARDOWN_RECEIPT_B}" "${CA_DIR_B}" federation-split-b
	validate_parent_child_resources a "${LAYOUT_A}" "${CLUSTER_A}" \
		"${CHILD_TEARDOWN_RECEIPT_A}" "${CA_DIR_A}" federation-split-a
	validate_parent_ca_resources
}

remove_relay_receipt_resources() {
	local direction expected_hash id object state
	validate_parent_relay_resources
	state="$(jq -r '.relays.state' "${PARENT_TEARDOWN_RECEIPT}")"
	if [ "${state}" = present ]; then
		for direction in a-to-b b-to-a; do
			object="$(jq -c --arg direction "${direction}" \
				'.relays.inventory[] | select(.direction == $direction)' \
				"${PARENT_TEARDOWN_RECEIPT}")"
			[ "$(jq -r '.state' <<<"${object}")" = present ] || continue
			id="$(jq -r '.id' <<<"${object}")"
			if validate_parent_relay_container "${direction}"; then
				docker rm --force "${id}" >/dev/null ||
					die "could not remove parent-recorded relay ${direction}"
			fi
		done
	fi
	ensure_no_unreceipted_relays
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		[ "${state}" = present ] &&
			[ "$(file_sha256 "${RELAY_RECEIPT}")" = \
				"$(jq -r '.relays.receipt_sha256' "${PARENT_TEARDOWN_RECEIPT}")" ] ||
			die "refusing to remove changed split relay receipt"
		rm -f "${RELAY_RECEIPT}"
	fi
	case "${state}" in
	present)
		expected_hash="$(jq -r '
        .relays.receipt.relays[] | select(.direction == "a-to-b").config_sha256
      ' "${PARENT_TEARDOWN_RECEIPT}")"
		;;
	absent | config-only)
		expected_hash="$(jq -r '.relays.configs.a_to_b_sha256' \
			"${PARENT_TEARDOWN_RECEIPT}")"
		;;
	esac
	if [ -e "${RELAY_CONFIG_A_TO_B}" ] || [ -L "${RELAY_CONFIG_A_TO_B}" ]; then
		validate_parent_config_file "${RELAY_CONFIG_A_TO_B}" "${expected_hash}"
		rm -f "${RELAY_CONFIG_A_TO_B}"
	fi
	case "${state}" in
	present)
		expected_hash="$(jq -r '
        .relays.receipt.relays[] | select(.direction == "b-to-a").config_sha256
      ' "${PARENT_TEARDOWN_RECEIPT}")"
		;;
	absent | config-only)
		expected_hash="$(jq -r '.relays.configs.b_to_a_sha256' \
			"${PARENT_TEARDOWN_RECEIPT}")"
		;;
	esac
	if [ -e "${RELAY_CONFIG_B_TO_A}" ] || [ -L "${RELAY_CONFIG_B_TO_A}" ]; then
		validate_parent_config_file "${RELAY_CONFIG_B_TO_A}" "${expected_hash}"
		rm -f "${RELAY_CONFIG_B_TO_A}"
	fi
	if [ -d "${RELAY_CONFIG_DIR}" ] && [ ! -L "${RELAY_CONFIG_DIR}" ]; then
		rmdir "${RELAY_CONFIG_DIR}" 2>/dev/null ||
			die "unexpected files remain in split relay config state"
	elif [ -e "${RELAY_CONFIG_DIR}" ] || [ -L "${RELAY_CONFIG_DIR}" ]; then
		die "split relay config state is not a non-symlink directory"
	fi
	validate_parent_relay_resources
}

remove_parent_ca_state() {
	local path relative state
	validate_parent_ca_resources
	state="$(jq -r '.ca.state' "${PARENT_TEARDOWN_RECEIPT}")"
	[ "${state}" = present ] || return
	while IFS= read -r relative; do
		path="${SPLIT_STATE_DIR}/${relative}"
		if [ -e "${path}" ] || [ -L "${path}" ]; then
			[ -f "${path}" ] && [ ! -L "${path}" ] ||
				die "refusing to remove changed split CA path: ${path}"
			[ "$(file_sha256 "${path}")" = "$(jq -r --arg path "${relative}" \
				'.ca.files[] | select(.path == $path).sha256' \
				"${PARENT_TEARDOWN_RECEIPT}")" ] ||
				die "refusing to remove changed split CA file: ${path}"
			rm -f "${path}"
		fi
	done < <(jq -r '.ca.files[].path' "${PARENT_TEARDOWN_RECEIPT}")
	for path in "${CA_DIR_A}" "${CA_DIR_B}"; do
		if [ -d "${path}" ] && [ ! -L "${path}" ]; then
			rmdir "${path}" 2>/dev/null ||
				die "unexpected files remain in split CA directory ${path}"
		elif [ -e "${path}" ] || [ -L "${path}" ]; then
			die "split CA path changed type: ${path}"
		fi
	done
	if [ -d "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ]; then
		rmdir "${SPLIT_STATE_DIR}/ca" 2>/dev/null ||
			die "unexpected files remain in split CA state"
	elif [ -e "${SPLIT_STATE_DIR}/ca" ] || [ -L "${SPLIT_STATE_DIR}/ca" ]; then
		die "split CA state changed type"
	fi
	validate_parent_ca_resources
}

remove_parent_child_resources() {
	local ca_dir="$5"
	local cluster="$3"
	local key="$1"
	local layout="$2"
	local receipt="$4"
	local state
	validate_parent_child_resources "${key}" "${layout}" "${cluster}" \
		"${receipt}" "${ca_dir}" "$6"
	state="$(jq -r --arg key "${key}" '.children[$key].state' \
		"${PARENT_TEARDOWN_RECEIPT}")"
	if [ "${state}" = present ] && { [ -e "${receipt}" ] || [ -L "${receipt}" ]; }; then
		run_child "${layout}" "${cluster}" "${ca_dir}" lifecycle down
	fi
	validate_parent_child_resources "${key}" "${layout}" "${cluster}" \
		"${receipt}" "${ca_dir}" "$6"
}

complete_parent_teardown_receipt() {
	validate_parent_teardown_resources
	ensure_no_unreceipted_relays
	[ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ] ||
		die "split relay receipt remains after parent teardown"
	[ ! -e "${RELAY_CONFIG_DIR}" ] && [ ! -L "${RELAY_CONFIG_DIR}" ] ||
		die "split relay config state remains after parent teardown"
	[ ! -e "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ] ||
		die "split CA state remains after parent teardown"
	require_child_absent_after_down "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}"
	require_child_absent_after_down "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}"
	require_no_temporary_state
	rm -f "${PARENT_TEARDOWN_RECEIPT}" ||
		die "could not remove completed split parent teardown receipt"
	if [ -d "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ]; then
		rmdir "${SPLIT_STATE_DIR}" 2>/dev/null ||
			die "unexpected files remain in split parent lifecycle state"
	elif [ -e "${SPLIT_STATE_DIR}" ] || [ -L "${SPLIT_STATE_DIR}" ]; then
		die "split parent lifecycle state is not a non-symlink directory"
	fi
}

relay_ip() {
	local direction="$1"
	jq -er --arg direction "${direction}" \
		'.relays[] | select(.direction == $direction).local_ip | select(length > 0)' \
		"${RELAY_RECEIPT}"
}

write_child_kubeconfig() {
	local cluster="$1"
	local output="$2"
	k3d kubeconfig get "${cluster}" >"${output}"
	chmod 600 "${output}"
	[ -s "${output}" ] && [ -r "${output}" ] ||
		die "could not write parent-owned kubeconfig for ${cluster}"
}

assert_fresh_child_has_no_managed_runtime() {
	local kubeconfig="$1"
	local state="$2"
	local unexpected
	[ "${state}" = absent ] || return 0
	unexpected="$(kubectl --kubeconfig "${kubeconfig}" get namespace \
		flux-system matrix matrix-b matrix-c agentgateway-system keycloak kagent \
		--ignore-not-found --output name)" ||
		die "could not inspect freshly prepared split control plane"
	[ -z "${unexpected}" ] ||
		die "split prepare unexpectedly created managed runtime namespaces: ${unexpected}"
}

assert_split_control_plane_boundary() {
	local kubeconfig_a="$1"
	local kubeconfig_b="$2"
	local child_a_state="$3"
	local child_b_state="$4"
	local api_a api_b fingerprint_a fingerprint_b network_a network_b
	local network_a_id network_b_id server_a server_b serverlb_a serverlb_b
	api_a="$(yq -er '.clusters[0].cluster.server | select(type == "!!str" and length > 0)' \
		"${kubeconfig_a}")"
	api_b="$(yq -er '.clusters[0].cluster.server | select(type == "!!str" and length > 0)' \
		"${kubeconfig_b}")"
	[ "${api_a}" != "${api_b}" ] || die "split control planes expose the same Kubernetes API"
	server_a="$(docker inspect "k3d-${CLUSTER_A}-server-0")"
	server_b="$(docker inspect "k3d-${CLUSTER_B}-server-0")"
	serverlb_a="$(docker inspect "${SERVERLB_A}")"
	serverlb_b="$(docker inspect "${SERVERLB_B}")"
	jq -e --arg cluster "${CLUSTER_A}" --arg owner federation-split-a '
      .[0] | .Config.Labels."k3d.cluster" == $cluster and
      .Config.Labels."dev.fgentic.demo" == $owner
    ' <<<"${server_a}" >/dev/null || die "split A server ownership is invalid"
	jq -e --arg cluster "${CLUSTER_B}" --arg owner federation-split-b '
      .[0] | .Config.Labels."k3d.cluster" == $cluster and
      .Config.Labels."dev.fgentic.demo" == $owner
    ' <<<"${server_b}" >/dev/null || die "split B server ownership is invalid"
	[ "$(jq -r '.[0].Id' <<<"${server_a}")" != "$(jq -r '.[0].Id' <<<"${server_b}")" ] &&
		[ "$(jq -r '.[0].Id' <<<"${serverlb_a}")" != "$(jq -r '.[0].Id' <<<"${serverlb_b}")" ] ||
		die "split server or ingress container identities overlap"
	jq -e --arg ip "${LOOPBACK_A}" '
      .[0].HostConfig.PortBindings as $ports |
      all(["80/tcp", "443/tcp"][];
        . as $port |
        ($ports[$port] | length == 1) and $ports[$port][0].HostIp == $ip and
        $ports[$port][0].HostPort == ($port | split("/")[0]))
    ' <<<"${serverlb_a}" >/dev/null || die "split A ingress is not fixed to ${LOOPBACK_A}"
	jq -e --arg ip "${LOOPBACK_B}" '
      .[0].HostConfig.PortBindings as $ports |
      all(["80/tcp", "443/tcp"][];
        . as $port |
        ($ports[$port] | length == 1) and $ports[$port][0].HostIp == $ip and
        $ports[$port][0].HostPort == ($port | split("/")[0]))
    ' <<<"${serverlb_b}" >/dev/null || die "split B ingress is not fixed to ${LOOPBACK_B}"
	network_a="$(network_document "${CLUSTER_A}" "${NETWORK_A}")"
	network_b="$(network_document "${CLUSTER_B}" "${NETWORK_B}")"
	network_a_id="$(jq -r '.id' <<<"${network_a}")"
	network_b_id="$(jq -r '.id' <<<"${network_b}")"
	[ "${network_a_id}" != "${network_b_id}" ] || die "split Docker network identities overlap"
	fingerprint_a="$(openssl x509 -in "${CA_DIR_A}/ca.crt" -noout -fingerprint -sha256)"
	fingerprint_b="$(openssl x509 -in "${CA_DIR_B}/ca.crt" -noout -fingerprint -sha256)"
	[ "${fingerprint_a}" != "${fingerprint_b}" ] || die "split CA fingerprints overlap"
	validate_receipt_networks
	validate_relay_container a-to-b yes
	validate_relay_container b-to-a yes
	assert_only_relays_dual_attached
	assert_fresh_child_has_no_managed_runtime "${kubeconfig_a}" "${child_a_state}"
	assert_fresh_child_has_no_managed_runtime "${kubeconfig_b}" "${child_b_state}"
	echo "Split boundary verified: distinct APIs, servers, ingresses, networks, CA roots, and directional raw-TLS relays."
}

invoke_split_seed() {
	local kubeconfig_a="$1"
	local kubeconfig_b="$2"
	[ "${kubeconfig_a}" != "${kubeconfig_b}" ] &&
		[ "${CA_DIR_A}" != "${CA_DIR_B}" ] ||
		die "split seed inputs must be distinct"
	for input in "${kubeconfig_a}" "${kubeconfig_b}" \
		"${CA_DIR_A}/ca.crt" "${CA_DIR_B}/ca.crt" "${HOST_CA_BUNDLE}"; do
		[[ "${input}" = /* ]] && [ -f "${input}" ] && [ -r "${input}" ] ||
			die "split seed input is not absolute and readable: ${input}"
	done
	env -u KUBECONFIG \
		FGENTIC_FED_LAYOUT=split \
		FGENTIC_FED_A_KUBECONFIG="${kubeconfig_a}" \
		FGENTIC_FED_B_KUBECONFIG="${kubeconfig_b}" \
		FGENTIC_FED_CA_DIR_A="${CA_DIR_A}" \
		FGENTIC_FED_CA_DIR_B="${CA_DIR_B}" \
		FGENTIC_FED_HOST_CA_BUNDLE="${HOST_CA_BUNDLE}" \
		"${ROOT_DIR}/scripts/seed-federation.sh"
}

split_up() {
	local child_a_state child_b_state local_a local_b remote_a remote_b work_dir
	local kubeconfig_a kubeconfig_b
	require_canonical_absent
	child_a_state="$(child_preflight_state "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}")"
	child_b_state="$(child_preflight_state "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}")"
	preflight_split_up "${child_a_state}" "${child_b_state}"
	trap 'cleanup_split_up "$?"' EXIT
	trap 'exit 130' INT
	trap 'exit 143' TERM
	work_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-split.XXXXXX")"
	SPLIT_UP_WORK_DIR="${work_dir}"
	prepare_public_roots "${child_a_state}" "${child_b_state}"

	# Both owned control planes and both public roots exist before either snapshot can consume peer
	# settings. This phase performs no Flux install, image build/import, Secret write, or seed.
	run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" prepare up
	run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" prepare up
	ensure_active_relays
	local_a="$(serverlb_ip "${CLUSTER_A}")"
	local_b="$(serverlb_ip "${CLUSTER_B}")"
	remote_a="$(relay_ip a-to-b)"
	remote_b="$(relay_ip b-to-a)"
	kubeconfig_a="${work_dir}/cluster-a.kubeconfig"
	kubeconfig_b="${work_dir}/cluster-b.kubeconfig"
	write_child_kubeconfig "${CLUSTER_A}" "${kubeconfig_a}"
	write_child_kubeconfig "${CLUSTER_B}" "${kubeconfig_b}"
	assert_split_control_plane_boundary "${kubeconfig_a}" "${kubeconfig_b}" \
		"${child_a_state}" "${child_b_state}"

	# Reconcile the consumer/issuer side first. It never receives seller signing material, the
	# receipt image, admission runtime drill, or a child seed. A then reconciles the seller plane.
	run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" reconcile up \
		"${local_b}" "${remote_b}"
	run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" reconcile up \
		"${local_a}" "${remote_a}"

	invoke_split_seed "${kubeconfig_a}" "${kubeconfig_b}"
	rm -rf "${work_dir}"
	SPLIT_UP_WORK_DIR=""
	trap - EXIT INT TERM
	echo "Split federation proof passed across ${CLUSTER_A} (${LOOPBACK_A}) and ${CLUSTER_B} (${LOOPBACK_B})."
}

split_status() {
	local active_a active_b ca_state parent_teardown=no phase=absent temporary_paths
	temporary_paths="$(temporary_state_paths)"
	if [ -n "${temporary_paths}" ]; then
		validate_temporary_state
		echo "Split parent: state=recovery-pending atomic_state=${temporary_paths//$'\n'/, }"
	fi
	if [ -e "${PARENT_TEARDOWN_RECEIPT}" ] || [ -L "${PARENT_TEARDOWN_RECEIPT}" ]; then
		validate_parent_teardown_receipt_file ||
			die "split parent teardown receipt is malformed"
		validate_parent_teardown_resources
		echo "Split parent: state=recovery-pending phase=removing receipt=${PARENT_TEARDOWN_RECEIPT}"
		ca_state="removing recorded=$(jq -r '.ca.state' "${PARENT_TEARDOWN_RECEIPT}")"
		parent_teardown=yes
	else
		ca_state="$(validate_stable_ca_state)"
	fi
	echo "Split CA roots: state=${ca_state}"
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		validate_relay_receipt_file || die "split relay receipt is malformed or stale"
		[ "${parent_teardown}" = yes ] ||
			preflight_relay_teardown_state >/dev/null
		phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
		if [ "${phase}" = active ]; then
			validate_receipt_networks
			validate_relay_container a-to-b yes
			validate_relay_container b-to-a yes
			assert_only_relays_dual_attached
		elif [ "${phase}" = stopped ]; then
			validate_receipt_networks
			validate_relay_container a-to-b stopped
			validate_relay_container b-to-a stopped
			assert_only_relays_dual_attached
		else
			if relay_containers_present; then
				validate_receipt_networks
				validate_relay_container a-to-b no || true
				validate_relay_container b-to-a no || true
			fi
			echo "Split relays: state=recovery-pending phase=${phase} receipt=${RELAY_RECEIPT}"
		fi
	else
		ensure_no_unreceipted_relays
	fi
	if [ "${phase}" = active ]; then
		active_a="$(relay_ip a-to-b)"
		active_b="$(relay_ip b-to-a)"
		echo "Split relays: state=active image=${RELAY_IMAGE} a_to_b=${active_a} b_to_a=${active_b}"
	fi
	[ "${phase}" != stopped ] || echo "Split relays: state=stopped image=${RELAY_IMAGE} identities=preserved"
	[ "${phase}" != absent ] || echo "Split relays: state=absent"
	run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" lifecycle status
	run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" lifecycle status
}

stop_active_relays() {
	local id_a id_b phase
	if [ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ]; then
		ensure_no_unreceipted_relays
		return
	fi
	validate_relay_receipt_file || die "split relay receipt is malformed or stale"
	phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
	case "${phase}" in
	stopped)
		validate_receipt_networks
		validate_relay_container a-to-b stopped
		validate_relay_container b-to-a stopped
		assert_only_relays_dual_attached
		return
		;;
	active) ;;
	*) die "split relay recovery is pending; run fed:split:down instead of stop" ;;
	esac
	validate_receipt_networks
	validate_relay_container a-to-b yes
	validate_relay_container b-to-a yes
	assert_only_relays_dual_attached
	id_a="$(jq -r '.relays[] | select(.direction == "a-to-b").id' "${RELAY_RECEIPT}")"
	id_b="$(jq -r '.relays[] | select(.direction == "b-to-a").id' "${RELAY_RECEIPT}")"
	docker stop "${id_a}" "${id_b}" >/dev/null || die "could not stop exact split relays"
	atomic_receipt_update '.phase = "stopped"'
	validate_relay_container a-to-b stopped
	validate_relay_container b-to-a stopped
}

split_stop() {
	local ca_state state_a state_b
	[ ! -e "${PARENT_TEARDOWN_RECEIPT}" ] &&
		[ ! -L "${PARENT_TEARDOWN_RECEIPT}" ] ||
		die "split federation parent teardown is pending; run fed:split:down"
	require_no_temporary_state
	state_a="$(child_preflight_state "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}")"
	state_b="$(child_preflight_state "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}")"
	ca_state="$(validate_stable_ca_state)"
	[ "${state_a}" = present ] && [ "${state_b}" = present ] ||
		die "both split child control planes must exist before stop"
	[ "${ca_state}" = complete ] ||
		die "both stable split CA roots must be complete before stop"
	stop_active_relays
	run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" lifecycle stop
	run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" lifecycle stop
	echo "Split federation control planes and exact relays are stopped; identities and stable CA roots remain parent-owned for same-mode reuse."
}

require_child_absent_after_down() {
	local ca_dir="$3"
	local cluster="$2"
	local layout="$1"
	local output
	output="$(run_child "${layout}" "${cluster}" "${ca_dir}" lifecycle status)" ||
		die "could not verify ${cluster} after teardown"
	rg --fixed-strings "Federation cluster ${cluster}: state=absent retained_bytes=0" \
		<<<"${output}" >/dev/null || die "${cluster} is not fully absent after teardown"
	! rg --fixed-strings 'state=recovery-pending' <<<"${output}" >/dev/null ||
		die "${cluster} teardown recovery remains pending"
}

split_down() {
	if [ ! -e "${PARENT_TEARDOWN_RECEIPT}" ] &&
		[ ! -L "${PARENT_TEARDOWN_RECEIPT}" ]; then
		prepare_parent_teardown_receipt
	else
		validate_parent_teardown_receipt_file ||
			die "split parent teardown receipt is malformed"
		cleanup_temporary_state
		run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" lifecycle cleanup-down >/dev/null
		run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" lifecycle cleanup-down >/dev/null
	fi
	validate_parent_teardown_resources
	remove_relay_receipt_resources
	validate_parent_teardown_resources
	remove_parent_child_resources b "${LAYOUT_B}" "${CLUSTER_B}" \
		"${CHILD_TEARDOWN_RECEIPT_B}" "${CA_DIR_B}" federation-split-b
	validate_parent_teardown_resources
	remove_parent_child_resources a "${LAYOUT_A}" "${CLUSTER_A}" \
		"${CHILD_TEARDOWN_RECEIPT_A}" "${CA_DIR_A}" federation-split-a
	validate_parent_teardown_resources
	remove_parent_ca_state
	complete_parent_teardown_receipt
	echo "Split federation clusters, relays, and stable split-only CA state were removed exactly."
}

main() {
	if (($# != 1)); then
		usage >&2
		return 2
	fi
	case "$1" in
	-h | --help)
		usage
		return
		;;
	up | status | stop | down) ;;
	*)
		usage >&2
		return 2
		;;
	esac

	require_split_host
	case "$1" in
	up) split_up ;;
	status) split_status ;;
	stop) split_stop ;;
	down) split_down ;;
	*) die "unsupported split federation action" ;;
	esac
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	main "$@"
fi
