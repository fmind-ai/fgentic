#!/usr/bin/env bash
# Prove partner federation, room-version/ACL policy, and rejection of a third homeserver.
set -euo pipefail

readonly SERVER_A="org-a.fgentic.localhost"
readonly SERVER_B="org-b.fgentic.localhost"
readonly SERVER_C="org-c.fgentic.localhost"
readonly MATRIX_A_URL="https://matrix.${SERVER_A}"
readonly MATRIX_B_URL="https://matrix.${SERVER_B}"
readonly MATRIX_C_URL="https://matrix.${SERVER_C}"
readonly CA_CERT="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
readonly FEDERATION_LOOPBACK="127.0.0.2"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-federation-seed.XXXXXX")"

ALICE_TOKEN=""
BOB_TOKEN=""
CHARLIE_TOKEN=""

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
	rm -rf "${WORK_DIR}"
}
trap cleanup EXIT INT TERM

die() {
	echo "error: $*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1 (run 'mise install')"
}

# `.localhost` resolves to 127.0.0.1 by definition. The normal local cluster may already own that
# address, so the isolated lab binds 127.0.0.2 and every host-side proof request resolves explicitly.
curl() {
	command curl \
		--noproxy '*' \
		--resolve "${SERVER_A}:443:${FEDERATION_LOOPBACK}" \
		--resolve "matrix.${SERVER_A}:443:${FEDERATION_LOOPBACK}" \
		--resolve "${SERVER_B}:443:${FEDERATION_LOOPBACK}" \
		--resolve "matrix.${SERVER_B}:443:${FEDERATION_LOOPBACK}" \
		--resolve "${SERVER_C}:443:${FEDERATION_LOOPBACK}" \
		--resolve "matrix.${SERVER_C}:443:${FEDERATION_LOOPBACK}" \
		"$@"
}

bootstrap_secret_value() {
	kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output "go-template={{index .data \"$1\" | base64decode}}"
}

request_status() {
	local output="$1"
	shift
	curl --silent --show-error --cacert "${CA_CERT}" --output "${output}" \
		--write-out '%{http_code}' "$@"
}

register_user() {
	local namespace="$1"
	local matrix_url="$2"
	local username="$3"
	local display_name="$4"
	local password="$5"
	local secret nonce mac digest document status registration_token
	local response="${WORK_DIR}/register-${username}.json"

	secret="$(kubectl --namespace "${namespace}" get secret ess-generated \
		--output 'go-template={{index .data "SYNAPSE_REGISTRATION_SHARED_SECRET" | base64decode}}')"
	nonce="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${matrix_url}/_synapse/admin/v1/register" | jq -er '.nonce')"
	digest="$(printf '%s\0%s\0%s\0%s' "${nonce}" "${username}" "${password}" notadmin |
		openssl sha1 -hmac "${secret}")"
	mac="${digest##* }"
	document="$(jq --null-input --compact-output \
		--arg nonce "${nonce}" --arg username "${username}" --arg displayname "${display_name}" \
		--arg password "${password}" --arg mac "${mac}" \
		'{nonce: $nonce, username: $username, displayname: $displayname, password: $password,
      admin: false, mac: $mac}')"
	status="$(request_status "${response}" --request POST \
		--header 'Content-Type: application/json' --data "${document}" \
		"${matrix_url}/_synapse/admin/v1/register")"
	secret=""
	document=""
	case "${status}" in
	200)
		# The registration API creates a bootstrap session. Revoke it; the proof logs in normally.
		registration_token="$(jq -r '.access_token // empty' "${response}")"
		if [ -n "${registration_token}" ]; then
			curl --silent --show-error --cacert "${CA_CERT}" --request POST \
				--header "Authorization: Bearer ${registration_token}" \
				"${matrix_url}/_matrix/client/v3/logout" >/dev/null
			registration_token=""
		fi
		;;
	400)
		jq -e '.errcode == "M_USER_IN_USE"' "${response}" >/dev/null ||
			die "${username} registration failed (HTTP 400)"
		;;
	*) die "${username} registration failed (HTTP ${status})" ;;
	esac
}

login_user() {
	local matrix_url="$1"
	local username="$2"
	local password="$3"
	local token_variable="$4"
	local document response token
	document="$(jq --null-input --compact-output --arg username "${username}" \
		--arg password "${password}" '{
      type: "m.login.password",
      identifier: {type: "m.id.user", user: $username},
      password: $password,
      initial_device_display_name: "Fgentic federation proof"
    }')"
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${matrix_url}/_matrix/client/v3/login")"
	token="$(jq -er '.access_token | select(type == "string" and length > 0)' <<<"${response}")"
	printf -v "${token_variable}" '%s' "${token}"
}

verify_server() {
	local server="$1"
	local matrix_url="https://matrix.${server}"
	local expected="matrix.${server}:443"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"https://${server}/.well-known/matrix/server" |
		jq -e --arg expected "${expected}" '."m.server" == $expected' >/dev/null
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${matrix_url}/_matrix/federation/v1/version" |
		jq -e '.server.name | type == "string" and length > 0' >/dev/null
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${matrix_url}/_matrix/key/v2/server" |
		jq -e --arg server "${server}" '.server_name == $server' >/dev/null
}

verify_whitelist() {
	local matrix_url="$1"
	local token="$2"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_synapse/client/v1/config/federation_whitelist" |
		jq -e --arg a "${SERVER_A}" --arg b "${SERVER_B}" \
		'.whitelist_enabled == true and (.whitelist | sort) == ([$a, $b] | sort)' >/dev/null
}

verify_control_whitelist() {
	local matrix_url="$1"
	local token="$2"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_synapse/client/v1/config/federation_whitelist" |
		jq -e --arg a "${SERVER_A}" --arg b "${SERVER_B}" --arg c "${SERVER_C}" \
		'.whitelist_enabled == true and (.whitelist | sort) == ([$a, $b, $c] | sort)' >/dev/null
}

initial_sync_token() {
	local matrix_url="$1"
	local token="$2"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/sync?timeout=0" |
		jq -er '.next_batch | select(type == "string" and length > 0)'
}

wait_for_event() {
	local matrix_url="$1"
	local token="$2"
	local room_id="$3"
	local since="$4"
	local event_id="$5"
	local sender="$6"
	local body="$7"
	local deadline=$((SECONDS + 180))
	local encoded_since sync
	while ((SECONDS < deadline)); do
		encoded_since="$(jq --null-input --raw-output --arg value "${since}" '$value | @uri')"
		sync="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
			--header "Authorization: Bearer ${token}" \
			"${matrix_url}/_matrix/client/v3/sync?timeout=1000&since=${encoded_since}")"
		if jq -e --arg room_id "${room_id}" --arg event_id "${event_id}" \
			--arg sender "${sender}" --arg body "${body}" '
        .rooms.join[$room_id].timeline.events[]? | select(
          .event_id == $event_id and .sender == $sender and
          .type == "m.room.message" and .content.msgtype == "m.text" and .content.body == $body)
      ' <<<"${sync}" >/dev/null; then
			return
		fi
		since="$(jq -er '.next_batch | select(type == "string" and length > 0)' <<<"${sync}")"
		sleep 2
	done
	die "federated event ${event_id} did not arrive within 3 minutes"
}

verify_membership() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local deadline=$((SECONDS + 120))
	local joined
	while ((SECONDS < deadline)); do
		joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
			--header "Authorization: Bearer ${token}" \
			"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
		if jq -e --arg alice "@alice:${SERVER_A}" --arg bob "@bob:${SERVER_B}" \
			'.joined | has($alice) and has($bob)' <<<"${joined}" >/dev/null; then
			return
		fi
		sleep 2
	done
	die "both users were not joined on ${matrix_url} within 2 minutes"
}

verify_federated_room_policy() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local acl creation join_rules
	acl="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.server_acl")"
	jq -e --arg a "${SERVER_A}" --arg b "${SERVER_B}" '
    .allow_ip_literals == false and .deny == [] and
    (.allow | sort) == ([$a, $b] | sort)
  ' <<<"${acl}" >/dev/null || die "federated room server ACL is not partner-only"

	creation="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.create")"
	jq -e '.room_version == "12" and ."m.federate" == true' <<<"${creation}" >/dev/null ||
		die "federated room is not explicitly federated at room version 12"

	join_rules="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.join_rules")"
	jq -e '.join_rule == "public"' <<<"${join_rules}" >/dev/null ||
		die "federated proof room is not public for the negative join test"
}

verify_denied_membership() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local joined
	joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
	jq -e --arg charlie "@charlie:${SERVER_C}" '.joined | has($charlie) | not' \
		<<<"${joined}" >/dev/null || die "denied control user joined the federated room"
}

expect_forbidden() {
	local description="$1"
	local output="$2"
	shift 2
	local status
	status="$(request_status "${output}" "$@")"
	assert_forbidden_response "${description}" "${status}" "${output}"
}

assert_forbidden_response() {
	local description="$1"
	local status="$2"
	local output="$3"
	[ "${status}" = "403" ] || die "${description} was not forbidden (HTTP ${status})"
	jq -e '.errcode == "M_FORBIDDEN" and (.error | type == "string" and length > 0)' \
		"${output}" >/dev/null || die "${description} did not return a Matrix forbidden error"
}

create_federated_room() {
	local output_variable="$1"
	local document response created_room_id
	# This is the lab's only supported federated-room constructor: the ACL is initial state, so no
	# federated event can race ahead of the participant-only policy.
	document="$(jq --null-input --compact-output --arg a "${SERVER_A}" --arg b "${SERVER_B}" '{
    name: "Fgentic Federation Proof",
    preset: "private_chat",
    visibility: "private",
    room_version: "12",
    creation_content: {"m.federate": true},
    initial_state: [
      {
        type: "m.room.join_rules",
        state_key: "",
        content: {join_rule: "public"}
      },
      {
        type: "m.room.server_acl",
        state_key: "",
        content: {allow: [$a, $b], deny: [], allow_ip_literals: false}
      }
    ]
  }')"
	response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${ALICE_TOKEN}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${MATRIX_A_URL}/_matrix/client/v3/createRoom")"
	created_room_id="$(jq -er '.room_id' <<<"${response}")"
	printf -v "${output_variable}" '%s' "${created_room_id}"
}

sign_federation_request() {
	local destination="$1"
	local uri="$2"
	local content="$3"
	local output_variable="$4"
	local signable signature_line key_id signature
	signable="$(jq --null-input --compact-output \
		--arg method PUT --arg uri "${uri}" --arg origin "${SERVER_C}" \
		--arg destination "${destination}" --argjson content "${content}" \
		'{method: $method, uri: $uri, origin: $origin,
      destination: $destination, content: $content}')"
	# The private key never leaves Synapse C. Its pinned signedjson dependency produces the exact
	# canonical Matrix signature; only the public key id and request signature return to the host.
	signature_line="$(printf '%s' "${signable}" |
		kubectl --namespace matrix-c exec --stdin statefulset/ess-synapse-main -- \
			python -c '
import json
import sys

from signedjson.key import read_signing_keys
from signedjson.sign import sign_json

with open("/secrets/ess-generated/SYNAPSE_SIGNING_KEY", encoding="utf-8") as stream:
    signing_key = read_signing_keys(stream)[0]
document = json.load(sys.stdin)
origin = document["origin"]
signed = sign_json(document, origin, signing_key)
key_id = f"{signing_key.alg}:{signing_key.version}"
print(key_id + "\t" + signed["signatures"][origin][key_id])
')"
	IFS=$'\t' read -r key_id signature <<<"${signature_line}"
	[[ "${key_id}" =~ ^ed25519:[A-Za-z0-9_-]+$ ]] || die "invalid federation signing key id"
	[[ "${signature}" =~ ^[A-Za-z0-9_+/=-]+$ ]] || die "invalid federation request signature"
	printf -v "${output_variable}" \
		'X-Matrix origin="%s",destination="%s",key="%s",sig="%s"' \
		"${SERVER_C}" "${destination}" "${key_id}" "${signature}"
}

send_signed_federation_probe() {
	local destination="$1"
	local matrix_url="$2"
	local room_id="$3"
	local output="$4"
	local status_variable="$5"
	local transaction_id uri body authorization status origin_server_ts
	transaction_id="c-probe-${RANDOM}-$$"
	uri="/_matrix/federation/v1/send/${transaction_id}"
	origin_server_ts="$(($(date +%s) * 1000))"
	body="$(jq --null-input --compact-output \
		--arg destination "${destination}" --arg origin "${SERVER_C}" \
		--arg room_id "${room_id}" --arg user_id "@charlie:${SERVER_C}" \
		--argjson timestamp "${origin_server_ts}" '{
      destination: $destination,
      origin: $origin,
      origin_server_ts: $timestamp,
      pdus: [],
      edus: [{
        edu_type: "m.typing",
        content: {room_id: $room_id, user_id: $user_id, typing: true}
      }]
    }')"
	sign_federation_request "${destination}" "${uri}" "${body}" authorization
	status="$(request_status "${output}" --request PUT \
		--header "Authorization: ${authorization}" \
		--header 'Content-Type: application/json' --data "${body}" \
		"${matrix_url}${uri}")"
	printf -v "${status_variable}" '%s' "${status}"
}

for command in curl date jq kubectl openssl; do
	require_command "${command}"
done
[ -r "${CA_CERT}" ] || die "local CA certificate not found: ${CA_CERT}"

alice_password="$(bootstrap_secret_value alice-password)"
bob_password="$(bootstrap_secret_value bob-password)"
charlie_password="$(bootstrap_secret_value charlie-password)"
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

create_federated_room room_id
encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"

invite_document="$(jq --null-input --compact-output --arg user_id "@bob:${SERVER_B}" \
	'{user_id: $user_id}')"
curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request POST \
	--header "Authorization: Bearer ${ALICE_TOKEN}" \
	--header 'Content-Type: application/json' --data "${invite_document}" \
	"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/invite" >/dev/null
encoded_a="$(jq --null-input --raw-output --arg value "${SERVER_A}" '$value | @uri')"
curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request POST \
	--header "Authorization: Bearer ${BOB_TOKEN}" \
	--header 'Content-Type: application/json' --data '{}' \
	"${MATRIX_B_URL}/_matrix/client/v3/join/${encoded_room}?server_name=${encoded_a}" >/dev/null

verify_membership "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_room}"
verify_membership "${MATRIX_B_URL}" "${BOB_TOKEN}" "${encoded_room}"
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
send_signed_federation_probe "${SERVER_C}" "${MATRIX_C_URL}" "${room_id}" \
	"${signed_control_response}" signed_control_status
[ "${signed_control_status}" = "200" ] ||
	die "signed federation positive control failed (HTTP ${signed_control_status})"
jq -e '.pdus == {}' "${signed_control_response}" >/dev/null ||
	die "signed federation positive control returned an invalid transaction response"

for target in "${SERVER_A}|${MATRIX_A_URL}|A" "${SERVER_B}|${MATRIX_B_URL}|B"; do
	target_server="${target%%|*}"
	target_rest="${target#*|}"
	target_url="${target_rest%%|*}"
	target_label="${target_rest##*|}"
	denied_send_response="${WORK_DIR}/denied-send-${target_label}.json"
	send_signed_federation_probe "${target_server}" "${target_url}" "${room_id}" \
		"${denied_send_response}" denied_send_status
	assert_forbidden_response "denied control federation send to ${target_label}" \
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
Room:        ${room_id}
A -> B:      ${event_a}
B -> A:      ${event_b}
C rejected:  join and signed federation sends
Local-only:  ${local_room_id} (default room version 12)
Homeservers: ${SERVER_A}, ${SERVER_B}; denied control ${SERVER_C}
EOF
