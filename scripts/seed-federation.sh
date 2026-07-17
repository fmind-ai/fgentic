#!/usr/bin/env bash
# Prove partner federation, room/ACL hardening, callback policy, and a denied third homeserver.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly FGENTIC_FED_LAYOUT="${FGENTIC_FED_LAYOUT:-canonical}"
case "${FGENTIC_FED_LAYOUT}" in
canonical)
	SERVER_A="org-a.fgentic.localhost"
	SERVER_B="org-b.fgentic.localhost"
	SERVER_C="org-c.fgentic.localhost"
	FEDERATION_LOOPBACK_A="127.0.0.2"
	FEDERATION_LOOPBACK_B="127.0.0.2"
	CA_CERT="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
	FEDERATION_KUBECONFIG_A=""
	FEDERATION_KUBECONFIG_B=""
	FEDERATION_CA_DIR_A=""
	FEDERATION_CA_DIR_B=""
	SPLIT_CA_A_CERT=""
	SPLIT_CA_B_CERT=""
	;;
split)
	SERVER_A="org-a.fgentic.test"
	SERVER_B="org-b.fgentic.test"
	SERVER_C="org-c.fgentic.test"
	FEDERATION_LOOPBACK_A="127.0.0.2"
	FEDERATION_LOOPBACK_B="127.0.0.3"
	CA_CERT="${FGENTIC_FED_HOST_CA_BUNDLE:-}"
	FEDERATION_KUBECONFIG_A="${FGENTIC_FED_A_KUBECONFIG:-}"
	FEDERATION_KUBECONFIG_B="${FGENTIC_FED_B_KUBECONFIG:-}"
	FEDERATION_CA_DIR_A="${FGENTIC_FED_CA_DIR_A:-}"
	FEDERATION_CA_DIR_B="${FGENTIC_FED_CA_DIR_B:-}"
	SPLIT_CA_A_CERT="${FEDERATION_CA_DIR_A:+${FEDERATION_CA_DIR_A}/ca.crt}"
	SPLIT_CA_B_CERT="${FEDERATION_CA_DIR_B:+${FEDERATION_CA_DIR_B}/ca.crt}"
	;;
*)
	echo "error: FGENTIC_FED_LAYOUT must be canonical or split" >&2
	exit 1
	;;
esac
readonly SERVER_A SERVER_B SERVER_C
readonly FEDERATION_LOOPBACK_A FEDERATION_LOOPBACK_B
readonly FEDERATION_LOOPBACK="${FEDERATION_LOOPBACK_A}"
readonly FEDERATION_KUBECONFIG_A FEDERATION_KUBECONFIG_B
readonly FEDERATION_CA_DIR_A FEDERATION_CA_DIR_B
readonly SPLIT_CA_A_CERT SPLIT_CA_B_CERT
readonly MATRIX_A_URL="https://matrix.${SERVER_A}"
readonly MATRIX_B_URL="https://matrix.${SERVER_B}"
readonly MATRIX_C_URL="https://matrix.${SERVER_C}"
readonly A2A_URL="https://a2a.${SERVER_A}"
readonly A2A_AGENT_PATH="/api/a2a/kagent/docs-qa"
readonly IDP_B_URL="https://id.${SERVER_B}"
readonly TOKEN_BUDGET_EXTENSION="https://fgentic.fmind.ai/a2a/extensions/token-budget/v1"
readonly USAGE_RECEIPT_EXTENSION="https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1"
readonly AGENT_CARD_CONFIGMAP="federated-docs-qa-agent-card"
readonly EXPECTED_DEMO_REPLY="Fgentic's deterministic evaluation model is working through agentgateway and kagent."
readonly CA_CERT
readonly POLICY_EVENT_TYPE="com.fgentic.blocked"
readonly POLICY_LOG_PREFIX="fgentic_federation_policy_violation "
readonly POLICY_PROBE_MODE="${FGENTIC_FED_POLICY_PROBE:-deny}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-federation-seed.XXXXXX")"
readonly WORK_DIR

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

ALICE_TOKEN=""
BOB_TOKEN=""
CHARLIE_TOKEN=""
ORG_B_A2A_TOKEN=""
UNTRUSTED_A2A_TOKEN=""
WRONG_AUDIENCE_A2A_TOKEN=""
USAGE_RECEIPT_KEY_ID=""
USAGE_RECEIPT_PUBLIC_JWK=""
CONTROL_PLANE_A_UID=""
CONTROL_PLANE_B_UID=""

cleanup() {
	if [ -n "${ALICE_TOKEN}" ]; then
		curl --silent --cacert "${CA_CERT}" --request POST \
			--header "Authorization: Bearer ${ALICE_TOKEN}" \
			"${MATRIX_A_URL}/_matrix/client/v3/logout" >/dev/null 2>&1 || true
	fi
	if [ -n "${BOB_TOKEN}" ]; then
		curl --silent --cacert "${CA_CERT}" --request POST \
			--header "Authorization: Bearer ${BOB_TOKEN}" \
			"${MATRIX_B_URL}/_matrix/client/v3/logout" >/dev/null 2>&1 || true
	fi
	if [ -n "${CHARLIE_TOKEN}" ]; then
		curl --silent --cacert "${CA_CERT}" --request POST \
			--header "Authorization: Bearer ${CHARLIE_TOKEN}" \
			"${MATRIX_C_URL}/_matrix/client/v3/logout" >/dev/null 2>&1 || true
	fi
	ALICE_TOKEN=""
	BOB_TOKEN=""
	CHARLIE_TOKEN=""
	ORG_B_A2A_TOKEN=""
	UNTRUSTED_A2A_TOKEN=""
	WRONG_AUDIENCE_A2A_TOKEN=""
	USAGE_RECEIPT_KEY_ID=""
	USAGE_RECEIPT_PUBLIC_JWK=""
	rm -rf "${WORK_DIR}"
}
trap cleanup EXIT INT TERM

# Canonical `.localhost` would resolve to 127.0.0.1, which another profile may own; split `.test`
# has no public resolution. Every host-side proof therefore pins the intended ingress explicitly.
curl() {
	command curl \
		--noproxy '*' \
		--resolve "${SERVER_A}:443:${FEDERATION_LOOPBACK}" \
		--resolve "matrix.${SERVER_A}:443:${FEDERATION_LOOPBACK}" \
		--resolve "${SERVER_B}:443:${FEDERATION_LOOPBACK_B}" \
		--resolve "matrix.${SERVER_B}:443:${FEDERATION_LOOPBACK_B}" \
		--resolve "${SERVER_C}:443:${FEDERATION_LOOPBACK}" \
		--resolve "matrix.${SERVER_C}:443:${FEDERATION_LOOPBACK}" \
		--resolve "a2a.${SERVER_A}:443:${FEDERATION_LOOPBACK}" \
		--resolve "id.${SERVER_B}:443:${FEDERATION_LOOPBACK_B}" \
		"$@"
}

federation_kubectl() {
	local control_plane="$1"
	shift
	case "${control_plane}" in
	A)
		if [ "${FGENTIC_FED_LAYOUT}" = split ]; then
			command kubectl --kubeconfig "${FEDERATION_KUBECONFIG_A}" "$@"
		else
			command kubectl "$@"
		fi
		;;
	B)
		if [ "${FGENTIC_FED_LAYOUT}" = split ]; then
			command kubectl --kubeconfig "${FEDERATION_KUBECONFIG_B}" "$@"
		else
			command kubectl "$@"
		fi
		;;
	*) die "federation control plane must be A or B" ;;
	esac
}

federation_matrix_control_plane() {
	local namespace="$1"
	case "${namespace}" in
	matrix | matrix-c) printf 'A\n' ;;
	matrix-b)
		if [ "${FGENTIC_FED_LAYOUT}" = split ]; then
			printf 'B\n'
		else
			printf 'A\n'
		fi
		;;
	*) die "unsupported federation Matrix namespace: ${namespace}" ;;
	esac
}

federation_matrix_kubectl() {
	local namespace="$1"
	shift
	local control_plane
	control_plane="$(federation_matrix_control_plane "${namespace}")"
	federation_kubectl "${control_plane}" --namespace "${namespace}" "$@"
}

federation_secret_value() {
	local control_plane="$1"
	local key="$2"
	[[ "${key}" =~ ^[a-z0-9-]+$ ]] || die "invalid federation bootstrap secret key"
	federation_kubectl "${control_plane}" --namespace flux-system \
		get secret fgentic-demo-bootstrap \
		--output "go-template={{index .data \"${key}\" | base64decode}}"
}

validate_absolute_readable_file() {
	local label="$1"
	local path="$2"
	[ -n "${path}" ] || die "${label} is required for split federation"
	case "${path}" in
	/*) ;;
	*) die "${label} must be an absolute path" ;;
	esac
	[ -f "${path}" ] && [ -r "${path}" ] || die "${label} must be a readable file"
}

validate_absolute_readable_directory() {
	local label="$1"
	local path="$2"
	[ -n "${path}" ] || die "${label} is required for split federation"
	case "${path}" in
	/*) ;;
	*) die "${label} must be an absolute path" ;;
	esac
	[ -d "${path}" ] && [ -r "${path}" ] && [ -x "${path}" ] ||
		die "${label} must be a readable directory"
}

validate_split_trust() {
	local certificate_count fingerprint_a fingerprint_b
	validate_absolute_readable_directory FGENTIC_FED_CA_DIR_A "${FEDERATION_CA_DIR_A}"
	validate_absolute_readable_directory FGENTIC_FED_CA_DIR_B "${FEDERATION_CA_DIR_B}"
	validate_absolute_readable_file FGENTIC_FED_CA_DIR_A/ca.crt "${SPLIT_CA_A_CERT}"
	validate_absolute_readable_file FGENTIC_FED_CA_DIR_B/ca.crt "${SPLIT_CA_B_CERT}"
	validate_absolute_readable_file FGENTIC_FED_HOST_CA_BUNDLE "${CA_CERT}"
	if rg --quiet --regexp 'PRIVATE KEY' "${CA_CERT}"; then
		die "FGENTIC_FED_HOST_CA_BUNDLE must contain public certificates only"
	fi
	certificate_count="$(awk '/-----BEGIN CERTIFICATE-----/ {count++} END {print count + 0}' \
		"${CA_CERT}")"
	[ "${certificate_count}" = 2 ] ||
		die "FGENTIC_FED_HOST_CA_BUNDLE must contain exactly two public roots"
	for certificate in "${SPLIT_CA_A_CERT}" "${SPLIT_CA_B_CERT}"; do
		openssl x509 -in "${certificate}" -noout >/dev/null 2>&1 ||
			die "split federation CA certificate is not valid PEM"
		openssl verify -CAfile "${certificate}" "${certificate}" >/dev/null 2>&1 ||
			die "split federation CA certificate is not a self-trusted root"
		openssl verify -CAfile "${CA_CERT}" "${certificate}" >/dev/null 2>&1 ||
			die "FGENTIC_FED_HOST_CA_BUNDLE does not trust both split roots"
	done
	fingerprint_a="$(openssl x509 -in "${SPLIT_CA_A_CERT}" -noout -fingerprint -sha256)"
	fingerprint_b="$(openssl x509 -in "${SPLIT_CA_B_CERT}" -noout -fingerprint -sha256)"
	[ "${fingerprint_a}" != "${fingerprint_b}" ] ||
		die "split federation CA roots must be distinct"
}

validate_federation_layout() {
	case "${FGENTIC_FED_LAYOUT}" in
	canonical)
		[ -r "${CA_CERT}" ] || die "local CA certificate not found: ${CA_CERT}"
		;;
	split)
		validate_absolute_readable_file FGENTIC_FED_A_KUBECONFIG \
			"${FEDERATION_KUBECONFIG_A}"
		validate_absolute_readable_file FGENTIC_FED_B_KUBECONFIG \
			"${FEDERATION_KUBECONFIG_B}"
		[ "${FEDERATION_KUBECONFIG_A}" != "${FEDERATION_KUBECONFIG_B}" ] ||
			die "split federation kubeconfig paths must be distinct"
		validate_split_trust
		CONTROL_PLANE_A_UID="$(federation_kubectl A --namespace kube-system \
			get namespace kube-system --output jsonpath='{.metadata.uid}' 2>/dev/null)" ||
			die "unable to read control plane A identity"
		CONTROL_PLANE_B_UID="$(federation_kubectl B --namespace kube-system \
			get namespace kube-system --output jsonpath='{.metadata.uid}' 2>/dev/null)" ||
			die "unable to read control plane B identity"
		[ -n "${CONTROL_PLANE_A_UID}" ] && [ -n "${CONTROL_PLANE_B_UID}" ] ||
			die "split federation control-plane identities must be non-empty"
		[ "${CONTROL_PLANE_A_UID}" != "${CONTROL_PLANE_B_UID}" ] ||
			die "split federation kubeconfigs resolve to the same control plane"
		;;
	*) die "unsupported federation layout: ${FGENTIC_FED_LAYOUT}" ;;
	esac
}

verify_control_plane_inventory() {
	local control_plane="$1"
	local namespaces="$2"
	local pods="$3"
	local required_list="$4"
	local forbidden_list="$5"
	local -a required forbidden
	local namespace
	read -r -a required <<<"${required_list}"
	read -r -a forbidden <<<"${forbidden_list}"
	for namespace in "${required[@]}"; do
		jq -e --arg namespace "${namespace}" \
			'any(.items[]?; .metadata.name == $namespace)' <<<"${namespaces}" >/dev/null ||
			die "control plane ${control_plane} is missing required namespace ${namespace}"
		jq -e --arg namespace "${namespace}" \
			'any(.items[]?; .metadata.namespace == $namespace)' <<<"${pods}" >/dev/null ||
			die "control plane ${control_plane} has no workload pod in ${namespace}"
	done
	for namespace in "${forbidden[@]}"; do
		jq -e --arg namespace "${namespace}" '
      all(.items[]?; .metadata.name != $namespace)
    ' <<<"${namespaces}" >/dev/null ||
			die "control plane ${control_plane} unexpectedly contains namespace ${namespace}"
		jq -e --arg namespace "${namespace}" '
      all(.items[]?; .metadata.namespace != $namespace)
    ' <<<"${pods}" >/dev/null ||
			die "control plane ${control_plane} unexpectedly runs a pod in ${namespace}"
	done
}

verify_split_runtime_inventory() {
	[ "${FGENTIC_FED_LAYOUT}" = split ] || return 0
	local namespaces_a namespaces_b pods_a pods_b
	namespaces_a="$(federation_kubectl A get namespaces --output json)"
	pods_a="$(federation_kubectl A get pods --all-namespaces --output json)"
	namespaces_b="$(federation_kubectl B get namespaces --output json)"
	pods_b="$(federation_kubectl B get pods --all-namespaces --output json)"
	verify_control_plane_inventory A "${namespaces_a}" "${pods_a}" \
		'matrix matrix-c agentgateway-system kagent' 'matrix-b keycloak'
	verify_control_plane_inventory B "${namespaces_b}" "${pods_b}" \
		'matrix-b keycloak' 'matrix matrix-c agentgateway-system kagent'
}

# shellcheck source=scripts/lib/federation-a2a.sh
source "${ROOT_DIR}/scripts/lib/federation-a2a.sh"
# shellcheck source=scripts/lib/federation-matrix.sh
source "${ROOT_DIR}/scripts/lib/federation-matrix.sh"
# shellcheck source=scripts/lib/federation-signing.sh
source "${ROOT_DIR}/scripts/lib/federation-signing.sh"

if [ "${BASH_SOURCE[0]}" != "$0" ]; then
	return 0
fi

for command in awk cmp curl date jq kubectl mise openssl rg; do
	require_command "${command}"
done
case "${POLICY_PROBE_MODE}" in
allow | deny) ;;
*) die "FGENTIC_FED_POLICY_PROBE must be allow or deny" ;;
esac
validate_federation_layout
verify_split_runtime_inventory

# The host is the delegation client in both layouts. Split mode proves that org B's issuer and
# secrets live on control plane B, but deliberately makes no B-workload-origin A2A claim.
verify_cross_org_delegation

alice_password="$(federation_secret_value A alice-password)"
bob_password="$(federation_secret_value B bob-password)"
charlie_password="$(federation_secret_value A charlie-password)"
register_user matrix "${MATRIX_A_URL}" alice 'Alice Federation' "${alice_password}"
register_user matrix-b "${MATRIX_B_URL}" bob 'Bob Federation' "${bob_password}"
register_user matrix-c "${MATRIX_C_URL}" charlie 'Charlie Denied Control' "${charlie_password}"
login_user "${MATRIX_A_URL}" alice "${alice_password}" ALICE_TOKEN
login_user "${MATRIX_B_URL}" bob "${bob_password}" BOB_TOKEN
login_user "${MATRIX_C_URL}" charlie "${charlie_password}" CHARLIE_TOKEN
alice_password=""
bob_password=""
charlie_password=""

verify_server "${SERVER_A}"
verify_server "${SERVER_B}"
verify_server "${SERVER_C}"
verify_whitelist "${MATRIX_A_URL}" "${ALICE_TOKEN}"
verify_whitelist "${MATRIX_B_URL}" "${BOB_TOKEN}"
verify_control_whitelist "${MATRIX_C_URL}" "${CHARLIE_TOKEN}"
wait_for_mounted_policy_mode matrix
wait_for_mounted_policy_mode matrix-b

create_federated_room room_id
encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
invite_and_join_partner "${encoded_room}"
encoded_a="$(jq --null-input --raw-output --arg value "${SERVER_A}" '$value | @uri')"
verify_federated_room_policy "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_room}"
verify_federated_room_policy "${MATRIX_B_URL}" "${BOB_TOKEN}" "${encoded_room}"

denied_join_response="${WORK_DIR}/denied-join.json"
expect_forbidden "denied control join" "${denied_join_response}" --request POST \
	--header "Authorization: Bearer ${CHARLIE_TOKEN}" \
	--header 'Content-Type: application/json' --data '{}' \
	"${MATRIX_C_URL}/_matrix/client/v3/join/${encoded_room}?server_name=${encoded_a}"
verify_denied_membership "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_room}"
verify_denied_membership "${MATRIX_B_URL}" "${BOB_TOKEN}" "${encoded_room}"

signed_control_response="${WORK_DIR}/signed-control.json"
signed_control_status=""
send_signed_federation_probe "${SERVER_C}" "${MATRIX_C_URL}" "${room_id}" \
	"${signed_control_response}" signed_control_status
[ "${signed_control_status}" = "200" ] ||
	die "signed federation positive control failed (HTTP ${signed_control_status})"
jq -e '.pdus == {}' "${signed_control_response}" >/dev/null ||
	die "signed federation positive control returned an invalid transaction response"

if [ "${FGENTIC_FED_LAYOUT}" = split ]; then
	denied_targets=(
		"${SERVER_A}|${MATRIX_A_URL}|A|A (local C-to-A denial)"
		"${SERVER_B}|${MATRIX_B_URL}|B|B (signed by C on A; rejected at B's distinct ingress)"
	)
else
	denied_targets=(
		"${SERVER_A}|${MATRIX_A_URL}|A|A"
		"${SERVER_B}|${MATRIX_B_URL}|B|B"
	)
fi
for target in "${denied_targets[@]}"; do
	target_server="${target%%|*}"
	target_rest="${target#*|}"
	target_url="${target_rest%%|*}"
	target_rest="${target_rest#*|}"
	target_label="${target_rest%%|*}"
	target_description="${target_rest#*|}"
	denied_send_response="${WORK_DIR}/denied-send-${target_label}.json"
	denied_send_status=""
	send_signed_federation_probe "${target_server}" "${target_url}" "${room_id}" \
		"${denied_send_response}" denied_send_status
	assert_forbidden_response "denied control federation send to ${target_description}" \
		"${denied_send_status}" "${denied_send_response}"
done

bob_since="$(initial_sync_token "${MATRIX_B_URL}" "${BOB_TOKEN}")"
alice_since="$(initial_sync_token "${MATRIX_A_URL}" "${ALICE_TOKEN}")"

marker_a="federation-a-to-b-${RANDOM}-$$"
message_a="$(jq --null-input --compact-output --arg body "${marker_a}" \
	'{msgtype: "m.text", body: $body}')"
event_a_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--request PUT --header "Authorization: Bearer ${ALICE_TOKEN}" \
	--header 'Content-Type: application/json' --data "${message_a}" \
	"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/send/m.room.message/a-${RANDOM}-$$")"
event_a="$(jq -er '.event_id' <<<"${event_a_response}")"
wait_for_event "${MATRIX_B_URL}" "${BOB_TOKEN}" "${room_id}" "${bob_since}" "${event_a}" \
	"@alice:${SERVER_A}" "${marker_a}"

marker_b="federation-b-to-a-${RANDOM}-$$"
message_b="$(jq --null-input --compact-output --arg body "${marker_b}" \
	'{msgtype: "m.text", body: $body}')"
event_b_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--request PUT --header "Authorization: Bearer ${BOB_TOKEN}" \
	--header 'Content-Type: application/json' --data "${message_b}" \
	"${MATRIX_B_URL}/_matrix/client/v3/rooms/${encoded_room}/send/m.room.message/b-${RANDOM}-$$")"
event_b="$(jq -er '.event_id' <<<"${event_b_response}")"
wait_for_event "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${room_id}" "${alice_since}" "${event_b}" \
	"@bob:${SERVER_B}" "${marker_b}"

# The drop callback intentionally splits the local DAG. Keep the denied event as the final event
# in a throwaway room so this proof cannot poison the bidirectional acceptance room above.
policy_room_id=""
create_federated_room policy_room_id "Fgentic Federation Policy Probe"
encoded_policy_room="$(jq --null-input --raw-output --arg value "${policy_room_id}" '$value | @uri')"
invite_and_join_partner "${encoded_policy_room}"
policy_marker="policy-content-must-not-be-logged-${RANDOM}-$$"
policy_document="$(jq --null-input --compact-output --arg marker "${policy_marker}" \
	--arg expected "${POLICY_PROBE_MODE}" '{probe_id: $marker, expected: $expected}')"
policy_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--request PUT --header "Authorization: Bearer ${BOB_TOKEN}" \
	--header 'Content-Type: application/json' --data "${policy_document}" \
	"${MATRIX_B_URL}/_matrix/client/v3/rooms/${encoded_policy_room}/send/${POLICY_EVENT_TYPE}/policy-${RANDOM}-$$")"
policy_event_id="$(jq -er '.event_id' <<<"${policy_response}")"
encoded_policy_event="$(jq --null-input --raw-output --arg value "${policy_event_id}" '$value | @uri')"
verify_local_policy_event "${encoded_policy_room}" "${encoded_policy_event}" \
	"${policy_event_id}" "${policy_marker}"
case "${POLICY_PROBE_MODE}" in
allow)
	wait_for_remote_policy_event "${encoded_policy_room}" "${encoded_policy_event}" \
		"${policy_event_id}" "${policy_marker}"
	policy_outcome="allowed on A after Flux policy reconcile"
	;;
deny)
	wait_for_policy_violation "${policy_room_id}" "${policy_event_id}" "${policy_marker}"
	verify_remote_policy_event_absent "${encoded_policy_room}" "${encoded_policy_event}" \
		"${policy_event_id}"
	policy_outcome="retained on B, absent on A"
	;;
*) die "unsupported federation policy probe mode: ${POLICY_PROBE_MODE}" ;;
esac

local_room_document="$(jq --null-input --compact-output '{
  name: "Fgentic Local-only Proof",
  preset: "private_chat",
  visibility: "private",
  creation_content: {"m.federate": false}
}')"
local_room_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--header "Authorization: Bearer ${ALICE_TOKEN}" \
	--header 'Content-Type: application/json' --data "${local_room_document}" \
	"${MATRIX_A_URL}/_matrix/client/v3/createRoom")"
local_room_id="$(jq -er '.room_id' <<<"${local_room_response}")"
encoded_local_room="$(jq --null-input --raw-output --arg value "${local_room_id}" '$value | @uri')"
local_creation="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--header "Authorization: Bearer ${ALICE_TOKEN}" \
	"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_local_room}/state/m.room.create")"
jq -e '.room_version == "12" and ."m.federate" == false' <<<"${local_creation}" >/dev/null ||
	die "default room version or explicit local-only federation policy was not enforced"

for session in "${MATRIX_A_URL}|${ALICE_TOKEN}" "${MATRIX_B_URL}|${BOB_TOKEN}" \
	"${MATRIX_C_URL}|${CHARLIE_TOKEN}"; do
	matrix_url="${session%%|*}"
	token="${session#*|}"
	curl --silent --show-error --cacert "${CA_CERT}" --request POST \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/logout" >/dev/null || true
done
ALICE_TOKEN=""
BOB_TOKEN=""
CHARLIE_TOKEN=""

cat <<EOF

Federation proof passed without a provider connection.
A2A org B:    verified JWT -> signed docs-qa -> deterministic model reply
A2A quota:    3,000-token reservation accepted, second reservation rejected
A2A metrics:  aggregate provider-reported token count increased
Room:        ${room_id}
A -> B:      ${event_a}
B -> A:      ${event_b}
Policy ${POLICY_PROBE_MODE}: ${policy_event_id} (${policy_outcome})
C rejected:  join and signed federation sends
Local-only:  ${local_room_id} (default room version 12)
Homeservers: ${SERVER_A}, ${SERVER_B}; denied control ${SERVER_C}
EOF

if [ "${FGENTIC_FED_LAYOUT}" = split ]; then
	cat <<EOF
Control planes: A ${CONTROL_PLANE_A_UID}; B ${CONTROL_PLANE_B_UID} (distinct kube-system UIDs)
Host trust:    distinct A/B public roots verified in the explicit two-root bundle
Inventory:     A owns Matrix A/C + agent plane; B owns Matrix B + Keycloak only
C -> A deny:   local to control plane A
Matrix relay:  bidirectional A/B messages are the cross-network relay proof
C -> B deny:   signed by C on A; rejected at B's distinct ingress
A2A driver:    host process; no B-workload-origin A2A claim is made
EOF
fi
