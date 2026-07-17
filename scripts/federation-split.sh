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
		"${RELAY_CONFIG_A_TO_B}" "${RELAY_CONFIG_B_TO_A}" \
		"${CA_DIR_A}/ca.crt" "${CA_DIR_A}/ca.key" \
		"${CA_DIR_B}/ca.crt" "${CA_DIR_B}/ca.key"; do
		if [ -L "${path}" ] || { [ -e "${path}" ] && [ ! -f "${path}" ]; }; then
			die "split federation state file must be regular and non-symlinked: ${path}"
		fi
	done
}

ensure_state_directory() {
	local path resolved_state
	for path in "${SPLIT_STATE_DIR}" "${SPLIT_STATE_DIR}/ca"; do
		if [ -L "${path}" ] || { [ -e "${path}" ] && [ ! -d "${path}" ]; }; then
			die "split federation state path must be a non-symlink directory: ${path}"
		fi
	done
	mkdir -p "${SPLIT_STATE_DIR}" "${SPLIT_STATE_DIR}/ca" ||
		die "could not create split federation state directory"
	[ -d "${SPLIT_STATE_DIR}" ] && [ ! -L "${SPLIT_STATE_DIR}" ] ||
		die "split federation state must be a non-symlink directory"
	chmod 700 "${SPLIT_STATE_DIR}" "${SPLIT_STATE_DIR}/ca" ||
		die "could not protect split federation state"
	resolved_state="$(cd "${SPLIT_STATE_DIR}" && pwd -P)"
	case "${resolved_state}/" in
	"${ROOT_DIR}/"*) die "split lifecycle state and private CA keys must remain outside the repository" ;;
	esac
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

split_ca_state() {
	local present=0
	local path
	for path in "${CA_DIR_A}/ca.crt" "${CA_DIR_A}/ca.key" \
		"${CA_DIR_B}/ca.crt" "${CA_DIR_B}/ca.key"; do
		if [ -e "${path}" ] || [ -L "${path}" ]; then
			present=$((present + 1))
		fi
	done
	case "${present}" in
	0) printf 'absent\n' ;;
	4) printf 'complete\n' ;;
	*) printf 'partial\n' ;;
	esac
}

generate_split_ca() {
	local cert_public directory="$1" key_public
	FGENTIC_CA_DIR="${directory}" "${ROOT_DIR}/scripts/local-ca.sh" --generate-only >/dev/null
	[ -d "${directory}" ] && [ ! -L "${directory}" ] ||
		die "split federation CA directory must not be a symlink: ${directory}"
	[ -f "${directory}/ca.crt" ] && [ ! -L "${directory}/ca.crt" ] &&
		[ -f "${directory}/ca.key" ] && [ ! -L "${directory}/ca.key" ] ||
		die "split federation CA is incomplete in ${directory}"
	if ! chmod 700 "${directory}" || ! chmod 600 "${directory}/ca.key"; then
		die "could not protect split federation CA in ${directory}"
	fi
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

write_host_ca_bundle() {
	local certificate_count fingerprint_a fingerprint_b temporary
	fingerprint_a="$(openssl x509 -in "${CA_DIR_A}/ca.crt" -noout -fingerprint -sha256)"
	fingerprint_b="$(openssl x509 -in "${CA_DIR_B}/ca.crt" -noout -fingerprint -sha256)"
	[ "${fingerprint_a}" != "${fingerprint_b}" ] ||
		die "split federation requires two distinct CA roots"
	temporary="$(mktemp "${SPLIT_STATE_DIR}/ca/.host-bundle.XXXXXX")"
	chmod 600 "${temporary}"
	{
		awk 'NF {print}' "${CA_DIR_A}/ca.crt"
		awk 'NF {print}' "${CA_DIR_B}/ca.crt"
	} >"${temporary}"
	certificate_count="$(rg --count --fixed-strings -- \
		'-----BEGIN CERTIFICATE-----' "${temporary}" || true)"
	if [ "${certificate_count}" != 2 ] ||
		rg --fixed-strings -- 'PRIVATE KEY' "${temporary}" >/dev/null; then
		rm -f "${temporary}"
		die "split federation host bundle must contain exactly two public roots"
	fi
	mv "${temporary}" "${HOST_CA_BUNDLE}"
}

ca_fingerprint() {
	openssl x509 -in "$1" -outform DER |
		openssl dgst -sha256 -r | awk '{print "sha256:" $1}'
}

validate_ca_receipt_file() {
	[ -f "${CA_RECEIPT}" ] && [ ! -L "${CA_RECEIPT}" ] || return 1
	jq --exit-status '
      keys == ["roots", "schema"] and
      .schema == "fgentic.federation-split-ca.v1" and
      (.roots | keys == ["a", "b"]) and
      (all(.roots[]; type == "string" and test("^sha256:[0-9a-f]{64}$"))) and
      .roots.a != .roots.b
    ' "${CA_RECEIPT}" >/dev/null 2>&1
}

persist_or_validate_ca_receipt() {
	local actual_a actual_b child_a_state="$1" child_b_state="$2" temporary
	actual_a="$(ca_fingerprint "${CA_DIR_A}/ca.crt")"
	actual_b="$(ca_fingerprint "${CA_DIR_B}/ca.crt")"
	[ "${actual_a}" != "${actual_b}" ] || die "split federation requires two distinct CA roots"
	if [ -e "${CA_RECEIPT}" ] || [ -L "${CA_RECEIPT}" ]; then
		validate_ca_receipt_file || die "split federation CA identity receipt is malformed"
		jq --exit-status --arg a "${actual_a}" --arg b "${actual_b}" \
			'.roots == {a: $a, b: $b}' "${CA_RECEIPT}" >/dev/null ||
			die "refusing implicit split CA rotation: trust-root fingerprint changed"
		return
	fi
	if [ "${child_a_state}" = present ] || [ "${child_b_state}" = present ]; then
		die "refusing split CA reuse without the original trust-root identity receipt"
	fi
	temporary="$(mktemp "${SPLIT_STATE_DIR}/ca/.roots.XXXXXX")"
	chmod 600 "${temporary}"
	jq --null-input --arg a "${actual_a}" --arg b "${actual_b}" '{
      schema: "fgentic.federation-split-ca.v1",
      roots: {a: $a, b: $b}
    }' >"${temporary}"
	mv "${temporary}" "${CA_RECEIPT}"
	validate_ca_receipt_file || die "could not persist split CA identity receipt"
}

prepare_public_roots() {
	local child_a_state="$1"
	local child_b_state="$2"
	local ca_state
	ensure_state_directory
	ca_state="$(split_ca_state)"
	if { [ "${child_a_state}" = present ] || [ "${child_b_state}" = present ]; } &&
		[ "${ca_state}" != complete ]; then
		die "refusing implicit split CA rotation while an owned child control plane exists"
	fi
	[ "${ca_state}" != partial ] ||
		die "split federation CA state is partial; run fed:split:down for exact recovery"
	generate_split_ca "${CA_DIR_A}"
	generate_split_ca "${CA_DIR_B}"
	[ "${CA_DIR_A}" != "${CA_DIR_B}" ] || die "split federation CA directories overlap"
	persist_or_validate_ca_receipt "${child_a_state}" "${child_b_state}"
	write_host_ca_bundle
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
        keys == ["config_sha256", "direction", "id", "local_ip", "local_network", "name", "remote_network", "target"] and
        (.id | type == "string") and (.local_ip | type == "string"))) and
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
	local config_a config_b network_a network_b temporary
	[ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ] ||
		die "split relay receipt already exists"
	network_a="$(network_document "${CLUSTER_A}" "${NETWORK_A}")"
	network_b="$(network_document "${CLUSTER_B}" "${NETWORK_B}")"
	write_relay_config "${RELAY_CONFIG_A_TO_B}" "${SERVERLB_B}"
	write_relay_config "${RELAY_CONFIG_B_TO_A}" "${SERVERLB_A}"
	config_a="$(file_sha256 "${RELAY_CONFIG_A_TO_B}")"
	config_b="$(file_sha256 "${RELAY_CONFIG_B_TO_A}")"
	temporary="$(mktemp "${SPLIT_STATE_DIR}/.relays.XXXXXX")"
	chmod 600 "${temporary}"
	jq --null-input \
		--arg image "${RELAY_IMAGE}" \
		--argjson network_a "${network_a}" --argjson network_b "${network_b}" \
		--arg relay_a "${RELAY_A_TO_B}" --arg relay_b "${RELAY_B_TO_A}" \
		--arg config_a "${config_a}" --arg config_b "${config_b}" '
      {
        schema: "fgentic.federation-split-relays.v1",
        phase: "creating",
        image: $image,
        image_id: "",
        networks: {a: $network_a, b: $network_b},
        relays: [
          {direction: "a-to-b", id: "", name: $relay_a, local_network: $network_a.name,
           remote_network: $network_b.name, local_ip: "", target: "k3d-fgentic-fed-b-serverlb",
           config_sha256: $config_a},
          {direction: "b-to-a", id: "", name: $relay_b, local_network: $network_b.name,
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
	local config expected_id expected_image_id expected_local_ip inspect local_network name
	local network_a_id network_b_id object remote_network
	object="$(relay_object "${direction}")"
	name="$(jq -r '.name' <<<"${object}")"
	expected_id="$(jq -r '.id' <<<"${object}")"
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
		--arg direction "${direction}" --arg config "${config}" \
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
	local config direction id image_id local_network name object remote_network
	direction="$1"
	object="$(relay_object "${direction}")"
	name="$(jq -r '.name' <<<"${object}")"
	local_network="$(jq -r '.local_network' <<<"${object}")"
	remote_network="$(jq -r '.remote_network' <<<"${object}")"
	config="${RELAY_CONFIG_A_TO_B}"
	[ "${direction}" = a-to-b ] || config="${RELAY_CONFIG_B_TO_A}"
	id="$(docker create \
		--name "${name}" \
		--network "${local_network}" \
		--label "dev.fgentic.federation-split=${RELAY_OWNER}" \
		--label "dev.fgentic.federation-split.direction=${direction}" \
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

remove_relay_receipt_resources() {
	local direction id name object phase
	if [ ! -e "${RELAY_RECEIPT}" ] && [ ! -L "${RELAY_RECEIPT}" ]; then
		ensure_no_unreceipted_relays
		return
	fi
	validate_relay_receipt_file || die "split relay receipt is malformed or stale"
	if relay_containers_present; then
		validate_receipt_networks
	fi
	phase="$(jq -r '.phase' "${RELAY_RECEIPT}")"
	if [ "${phase}" != removing ]; then
		atomic_receipt_update '.phase = "removing"'
	fi
	for direction in a-to-b b-to-a; do
		object="$(relay_object "${direction}")"
		id="$(jq -r '.id' <<<"${object}")"
		name="$(jq -r '.name' <<<"${object}")"
		if validate_relay_container "${direction}" no; then
			[ -n "${id}" ] || id="${name}"
			docker rm --force "${id}" >/dev/null ||
				die "could not remove exact relay ${name}"
		fi
	done
	ensure_no_unreceipted_relays
}

complete_parent_teardown_receipt() {
	ensure_no_unreceipted_relays
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		validate_relay_receipt_file || die "split relay receipt is malformed or stale"
		[ "$(jq -r '.phase' "${RELAY_RECEIPT}")" = removing ] ||
			die "split parent teardown receipt is not in the removing phase"
		ensure_no_unreceipted_relays
		rm -f "${RELAY_RECEIPT}"
	fi
	rm -f "${RELAY_CONFIG_A_TO_B}" "${RELAY_CONFIG_B_TO_A}"
	if [ -d "${RELAY_CONFIG_DIR}" ] && [ ! -L "${RELAY_CONFIG_DIR}" ]; then
		rmdir "${RELAY_CONFIG_DIR}" 2>/dev/null ||
			die "unexpected files remain in split relay config state"
	elif [ -e "${RELAY_CONFIG_DIR}" ] || [ -L "${RELAY_CONFIG_DIR}" ]; then
		die "split relay config state is not a non-symlink directory"
	fi
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
	local kubeconfig_a kubeconfig_b completed=no
	require_canonical_absent
	child_a_state="$(child_preflight_state "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}")"
	child_b_state="$(child_preflight_state "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}")"
	prepare_public_roots "${child_a_state}" "${child_b_state}"
	work_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-split.XXXXXX")"
	cleanup_split_up() {
		local status=$?
		rm -rf "${work_dir}"
		if [ "${completed}" != yes ] && [ "${status}" -ne 0 ]; then
			echo "Split federation did not complete; run fed:split:status, then fed:split:down for exact recovery." >&2
		fi
		return "${status}"
	}
	trap cleanup_split_up EXIT INT TERM

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
	completed=yes
	rm -rf "${work_dir}"
	trap - EXIT INT TERM
	echo "Split federation proof passed across ${CLUSTER_A} (${LOOPBACK_A}) and ${CLUSTER_B} (${LOOPBACK_B})."
}

split_status() {
	local active_a active_b phase=absent
	if [ -e "${RELAY_RECEIPT}" ] || [ -L "${RELAY_RECEIPT}" ]; then
		validate_relay_receipt_file || die "split relay receipt is malformed or stale"
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
	local state_a state_b
	state_a="$(child_preflight_state "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}")"
	state_b="$(child_preflight_state "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}")"
	[ "${state_a}" = present ] && [ "${state_b}" = present ] ||
		die "both split child control planes must exist before stop"
	stop_active_relays
	run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" lifecycle stop
	run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" lifecycle stop
	echo "Split federation control planes and exact relays are stopped; identities and stable CA roots remain parent-owned for same-mode reuse."
}

remove_stable_ca_state() {
	local directory path
	if [ ! -e "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ]; then
		return
	fi
	[ -d "${SPLIT_STATE_DIR}/ca" ] && [ ! -L "${SPLIT_STATE_DIR}/ca" ] &&
		[ ! -L "${CA_DIR_A}" ] && [ ! -L "${CA_DIR_B}" ] ||
		die "refusing non-directory or symlinked split CA state"
	for path in "${HOST_CA_BUNDLE}" "${CA_RECEIPT}" \
		"${CA_DIR_A}/ca.crt" "${CA_DIR_A}/ca.key" \
		"${CA_DIR_B}/ca.crt" "${CA_DIR_B}/ca.key"; do
		[ ! -L "${path}" ] || die "refusing symlinked split CA state: ${path}"
		rm -f "${path}"
	done
	for directory in "${CA_DIR_A}" "${CA_DIR_B}"; do
		if [ -d "${directory}" ]; then
			rmdir "${directory}" 2>/dev/null ||
				die "unexpected files remain in split CA directory ${directory}"
		elif [ -e "${directory}" ] || [ -L "${directory}" ]; then
			die "split CA path is not a directory: ${directory}"
		fi
	done
	rmdir "${SPLIT_STATE_DIR}/ca" 2>/dev/null ||
		die "unexpected files remain in split CA state"
	rmdir "${SPLIT_STATE_DIR}" 2>/dev/null || true
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
}

split_down() {
	# Relay recovery always precedes the unchanged child cluster teardown receipts. A retry may find
	# either child already absent; the child lifecycle remains the sole authority for its resources.
	remove_relay_receipt_resources
	run_child "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}" lifecycle down
	run_child "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}" lifecycle down
	require_child_absent_after_down "${LAYOUT_A}" "${CLUSTER_A}" "${CA_DIR_A}"
	require_child_absent_after_down "${LAYOUT_B}" "${CLUSTER_B}" "${CA_DIR_B}"
	remove_stable_ca_state
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
