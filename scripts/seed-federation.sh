#!/usr/bin/env bash
# Prove partner federation, room/ACL hardening, callback policy, and a denied third homeserver.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly SERVER_A="org-a.fgentic.localhost"
readonly SERVER_B="org-b.fgentic.localhost"
readonly SERVER_C="org-c.fgentic.localhost"
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
readonly CA_CERT="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
readonly FEDERATION_LOOPBACK="127.0.0.2"
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
FEDERATION_BRIDGE_ACTIVE="false"
MATRIX_KUSTOMIZATION_RESUME="false"
FEDERATION_BRIDGE_NAMESPACE_OWNER="room-binding-acceptance-$$"

cleanup() {
	teardown_federation_bridge >/dev/null 2>&1 || true
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
		--resolve "a2a.${SERVER_A}:443:${FEDERATION_LOOPBACK}" \
		--resolve "id.${SERVER_B}:443:${FEDERATION_LOOPBACK}" \
		"$@"
}

# shellcheck source=scripts/lib/federation-a2a.sh
source "${ROOT_DIR}/scripts/lib/federation-a2a.sh"
# shellcheck source=scripts/lib/federation-matrix.sh
source "${ROOT_DIR}/scripts/lib/federation-matrix.sh"
# shellcheck source=scripts/lib/federation-signing.sh
source "${ROOT_DIR}/scripts/lib/federation-signing.sh"

teardown_federation_bridge() {
	[ "${FEDERATION_BRIDGE_ACTIVE}" = "true" ] || return 0
	local namespace_name namespace_owner status=0
	namespace_name="$(kubectl get namespace bridge --ignore-not-found --output name)" || status=$?
	if [ "${status}" -eq 0 ] && [ -n "${namespace_name}" ]; then
		namespace_owner="$(kubectl get namespace bridge \
			--output jsonpath='{.metadata.annotations.fgentic\.dev/acceptance-owner}')" || status=$?
		if [ "${status}" -eq 0 ] && [ "${namespace_owner}" = "${FEDERATION_BRIDGE_NAMESPACE_OWNER}" ]; then
			kubectl delete namespace bridge --wait=true --timeout=3m >/dev/null || status=$?
		elif [ "${status}" -eq 0 ]; then
			echo "error: refusing to delete bridge namespace owned by another runtime" >&2
			status=1
		fi
	fi
	kubectl --namespace matrix patch helmrelease matrix-stack --type merge \
		--patch '{"spec":{"values":{"synapse":{"appservices":[]}}}}' >/dev/null || status=$?
	flux reconcile helmrelease matrix-stack --namespace matrix --timeout=10m >/dev/null || status=$?
	kubectl --namespace matrix delete secret matrix-a2a-bridge-acceptance-registration \
		--ignore-not-found >/dev/null || status=$?
	if [ "${MATRIX_KUSTOMIZATION_RESUME}" = "true" ]; then
		kubectl --namespace flux-system patch kustomization matrix --type merge \
			--patch '{"spec":{"suspend":false}}' >/dev/null || status=$?
	fi
	if [ "${status}" -eq 0 ]; then
		FEDERATION_BRIDGE_ACTIVE="false"
		MATRIX_KUSTOMIZATION_RESUME="false"
	fi
	return "${status}"
}

install_federation_bridge() {
	local managed_room_id="$1"
	local absent_room_id="$2"
	local escaped_server as_token hs_token registration patch kustomization_suspended
	local existing_namespace
	escaped_server="${SERVER_A//./\\.}"
	as_token="$(openssl rand -hex 32)"
	hs_token="$(openssl rand -hex 32)"
	registration="${WORK_DIR}/matrix-a2a-bridge-registration.yaml"
	printf '%s\n' \
		'id: matrix-a2a-bridge-acceptance' \
		'url: http://matrix-a2a-bridge-acceptance.bridge.svc.cluster.local:29331' \
		"as_token: ${as_token}" \
		"hs_token: ${hs_token}" \
		'sender_localpart: a2a-bridge' \
		'rate_limited: false' \
		'namespaces:' \
		'  users:' \
		"    - regex: '@a2a-bridge:${escaped_server}'" \
		'      exclusive: true' \
		"    - regex: '@agent-.*:${escaped_server}'" \
		'      exclusive: true' >"${registration}"
	chmod 600 "${registration}"
	as_token=""
	hs_token=""

	existing_namespace="$(kubectl get namespace bridge --ignore-not-found --output name)"
	[ -z "${existing_namespace}" ] \
		|| die "federation acceptance requires the disposable bridge namespace to be absent"
	FEDERATION_BRIDGE_ACTIVE="true"
	kubectl create --filename - >/dev/null <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: bridge
  annotations:
    fgentic.dev/acceptance-owner: ${FEDERATION_BRIDGE_NAMESPACE_OWNER}
  labels:
    fgentic.dev/managed: "true"
    fgentic.dev/image-policy: audit
    fgentic.dev/quota-profile: small
    pod-security.kubernetes.io/enforce: restricted
    pod-security.kubernetes.io/audit: restricted
    pod-security.kubernetes.io/warn: restricted
EOF
	for namespace in matrix bridge; do
		kubectl --namespace "${namespace}" create secret generic \
			matrix-a2a-bridge-acceptance-registration \
			--from-file="registration.yaml=${registration}" \
			--dry-run=client --output yaml | kubectl apply --filename - >/dev/null
	done
	kustomization_suspended="$(kubectl --namespace flux-system get kustomization matrix \
		--output jsonpath='{.spec.suspend}')"
	if [ "${kustomization_suspended}" != "true" ]; then
		MATRIX_KUSTOMIZATION_RESUME="true"
		if ! kubectl --namespace flux-system patch kustomization matrix --type merge \
			--patch '{"spec":{"suspend":true}}' >/dev/null; then
			MATRIX_KUSTOMIZATION_RESUME="false"
			return 1
		fi
	fi
	patch="$(jq --null-input --compact-output '{
    spec: {values: {synapse: {appservices: [{
      secret: "matrix-a2a-bridge-acceptance-registration",
      secretKey: "registration.yaml"
    }]}}}
  }')"
	kubectl --namespace matrix patch helmrelease matrix-stack --type merge \
		--patch "${patch}" >/dev/null
	flux reconcile helmrelease matrix-stack --namespace matrix --timeout=10m >/dev/null

	kubectl apply --filename - >/dev/null <<EOF
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: matrix-a2a-bridge-acceptance
  namespace: bridge
spec:
  interval: 30m
  chart:
    spec:
      chart: ./apps/matrix-a2a-bridge/chart
      interval: 1m
      sourceRef:
        kind: GitRepository
        name: flux-system
        namespace: flux-system
  values:
    fullnameOverride: matrix-a2a-bridge-acceptance
    image:
      repository: matrix-a2a-bridge
      tag: local
      pullPolicy: Never
    config:
      serverName: ${SERVER_A}
      homeserverURL: http://ess-synapse.matrix.svc.cluster.local:8008
      a2aBaseURL: http://kagent-controller.kagent.svc.cluster.local:8083
      accessManagerMXID: "@alice:${SERVER_A}"
      agentCardRefreshInterval: 1m
      welcomeEnabled: false
      replyScanMode: annotate
      replyScanFederatedMode: block
    database:
      enabled: false
    registration:
      secretName: matrix-a2a-bridge-acceptance-registration
      key: registration.yaml
    a2aAuthentication:
      enabled: false
    agents:
      agent-platform-helper:
        namespace: kagent
        name: platform-helper
        allowedRooms: ["${managed_room_id}", "${absent_room_id}"]
        allowedSenders: ["@nobody:${SERVER_A}"]
      agent-docs-qa:
        namespace: kagent
        name: docs-qa
        dataClassification: public
        allowedRooms: ["${managed_room_id}", "${absent_room_id}"]
        allowedServers: ["${SERVER_B}"]
        allowedSenders: ["@bob:${SERVER_B}"]
      agent-scribe:
        namespace: kagent
        name: scribe
        allowedRooms: ["${managed_room_id}", "${absent_room_id}"]
        allowedSenders: ["@nobody:${SERVER_A}"]
    metrics:
      podMonitor:
        enabled: false
    networkPolicy:
      enabled: true
      fromNamespaces: [matrix]
EOF
	flux reconcile helmrelease matrix-a2a-bridge-acceptance \
		--namespace bridge --timeout=10m >/dev/null
	kubectl --namespace bridge rollout status deployment/matrix-a2a-bridge-acceptance \
		--timeout=3m >/dev/null
}

invite_matrix_user() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local user_id="$4"
	local document
	document="$(jq --null-input --compact-output --arg user_id "${user_id}" '{user_id: $user_id}')"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request POST \
		--header "Authorization: Bearer ${token}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/invite" >/dev/null
}

wait_for_matrix_membership() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local user_id="$4"
	local expected="$5"
	local deadline=$((SECONDS + 120))
	local encoded_user member
	encoded_user="$(jq --null-input --raw-output --arg value "${user_id}" '$value | @uri')"
	while ((SECONDS < deadline)); do
		member="$(curl --silent --show-error --cacert "${CA_CERT}" \
			--header "Authorization: Bearer ${token}" \
			"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.member/${encoded_user}" || true)"
		if jq -e --arg expected "${expected}" '.membership == $expected' \
			<<<"${member}" >/dev/null 2>&1; then
			return
		fi
		sleep 2
	done
	die "${user_id} did not reach ${expected} membership within 2 minutes"
}

send_agent_mention() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local marker="$4"
	local ghost="@agent-docs-qa:${SERVER_A}"
	local document
	document="$(jq --null-input --compact-output --arg ghost "${ghost}" --arg marker "${marker}" '{
    msgtype: "m.text",
    body: ($ghost + " " + $marker),
    "m.mentions": {user_ids: [$ghost]}
  }')"
	curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request PUT \
		--header "Authorization: Bearer ${token}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/send/m.room.message/bridge-${RANDOM}-$$" \
		| jq -er '.event_id'
}

wait_for_bridge_reply() {
	local room_id="$1"
	local since="$2"
	local input_event_id="$3"
	local deadline=$((SECONDS + 180))
	local encoded_since sync
	while ((SECONDS < deadline)); do
		encoded_since="$(jq --null-input --raw-output --arg value "${since}" '$value | @uri')"
		sync="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
			--header "Authorization: Bearer ${BOB_TOKEN}" \
			"${MATRIX_B_URL}/_matrix/client/v3/sync?timeout=1000&since=${encoded_since}")"
		if jq -e --arg room "${room_id}" --arg sender "@agent-docs-qa:${SERVER_A}" \
			--arg body "${EXPECTED_DEMO_REPLY}" --arg event "${input_event_id}" '
        .rooms.join[$room].timeline.events[]? | select(
          .sender == $sender and .type == "m.room.message" and
          .content.msgtype == "m.notice" and .content.body == $body and
          .content["m.relates_to"]["m.in_reply_to"].event_id == $event)
      ' <<<"${sync}" >/dev/null; then
			return
		fi
		since="$(jq -er '.next_batch | select(type == "string" and length > 0)' <<<"${sync}")"
		sleep 2
	done
	die "managed-room bridge reply did not federate to org B within 3 minutes"
}

bridge_audit_records() {
	kubectl --namespace bridge logs deployment/matrix-a2a-bridge-acceptance \
		--container bridge --since=10m | jq --slurp '.'
}

wait_for_bridge_audit_reason() {
	local reason="$1"
	local deadline=$((SECONDS + 120))
	local records
	while ((SECONDS < deadline)); do
		records="$(bridge_audit_records)"
		if jq -e --arg reason "${reason}" '
      any(.[]; .audit_schema == "fgentic.delegation.v1" and
        .terminal_stage == "room_authorization" and .terminal_reason == $reason and
        .a2a_attempted == false)
    ' <<<"${records}" >/dev/null; then
			return
		fi
		sleep 2
	done
	die "bridge did not emit fail-closed ${reason} evidence within 2 minutes"
}

wait_for_managed_invite_audit() {
	local reason="$1"
	local deadline=$((SECONDS + 120))
	local records
	while ((SECONDS < deadline)); do
		records="$(bridge_audit_records)"
		if jq -e --arg reason "${reason}" '
      any(.[]; .audit_schema == "fgentic.managed_room_invite.v1" and
        .audit_stream == "managed_room_invite" and .reason == $reason)
    ' <<<"${records}" >/dev/null; then
			return
		fi
		sleep 2
	done
	die "bridge did not emit managed-invite ${reason} evidence within 2 minutes"
}

assert_matrix_user_not_joined() {
	local matrix_url="$1"
	local token="$2"
	local encoded_room="$3"
	local user_id="$4"
	local joined
	joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${token}" \
		"${matrix_url}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
	jq -e --arg user_id "${user_id}" '.joined | has($user_id) | not' \
		<<<"${joined}" >/dev/null || die "${user_id} joined without managed-room authority"
}

for command in awk cmp curl date flux jq kubectl mise openssl; do
	require_command "${command}"
done
[ -r "${CA_CERT}" ] || die "local CA certificate not found: ${CA_CERT}"
case "${POLICY_PROBE_MODE}" in
	allow | deny) ;;
	*) die "FGENTIC_FED_POLICY_PROBE must be allow or deny" ;;
esac

verify_cross_org_delegation

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
[ "${signed_control_status}" = "200" ] \
	|| die "signed federation positive control failed (HTTP ${signed_control_status})"
jq -e '.pdus == {}' "${signed_control_response}" >/dev/null \
	|| die "signed federation positive control returned an invalid transaction response"

for target in "${SERVER_A}|${MATRIX_A_URL}|A" "${SERVER_B}|${MATRIX_B_URL}|B"; do
	target_server="${target%%|*}"
	target_rest="${target#*|}"
	target_url="${target_rest%%|*}"
	target_label="${target_rest##*|}"
	denied_send_response="${WORK_DIR}/denied-send-${target_label}.json"
	denied_send_status=""
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
jq -e '.room_version == "12" and ."m.federate" == false' <<<"${local_creation}" >/dev/null \
	|| die "default room version or explicit local-only federation policy was not enforced"

# Reuse the accepted A/B room as the managed bridge room after the public denied-join proof above.
# Matrix defaults invite power to zero, so bind room administration to Alice explicitly before
# closing the join rule. Bob and the ghost retain only ordinary message authority.
managed_power_levels="$(jq --null-input --compact-output \
	--arg partner "@bob:${SERVER_B}" '{
    users: {($partner): 0},
    users_default: 0,
    events: {"m.room.message": 0},
    events_default: 0,
    state_default: 100,
    ban: 100,
    kick: 100,
    redact: 100,
    invite: 100,
    notifications: {room: 50}
  }')"
curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request PUT \
	--header "Authorization: Bearer ${ALICE_TOKEN}" \
	--header 'Content-Type: application/json' --data "${managed_power_levels}" \
	"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.power_levels" >/dev/null
invite_join_rule='{"join_rule":"invite"}'
curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" --request PUT \
	--header "Authorization: Bearer ${ALICE_TOKEN}" \
	--header 'Content-Type: application/json' --data "${invite_join_rule}" \
	"${MATRIX_A_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.join_rules" >/dev/null
join_rule_deadline=$((SECONDS + 120))
remote_room_state='[]'
remote_power_levels='{}'
remote_join_rule='{}'
while ((SECONDS < join_rule_deadline)); do
	remote_room_state="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${BOB_TOKEN}" \
		"${MATRIX_B_URL}/_matrix/client/v3/rooms/${encoded_room}/state")"
	remote_power_levels="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${BOB_TOKEN}" \
		"${MATRIX_B_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.power_levels")"
	remote_join_rule="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${BOB_TOKEN}" \
		"${MATRIX_B_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.join_rules")"
	if jq -e --arg manager "@alice:${SERVER_A}" --arg partner "@bob:${SERVER_B}" '
      (.users | has($manager) | not) and .users[$partner] == 0 and .users_default == 0 and
      .events["m.room.message"] == 0 and .events_default == 0 and .state_default == 100 and
      .invite == 100 and .kick == 100 and .ban == 100 and .redact == 100
    ' <<<"${remote_power_levels}" >/dev/null \
		&& jq -e --arg manager "@alice:${SERVER_A}" '
      any(.[]; .type == "m.room.create" and .state_key == "" and .sender == $manager and
        ((.content.additional_creators // []) | length) == 0)
    ' <<<"${remote_room_state}" >/dev/null \
		&& jq -e '.join_rule == "invite"' <<<"${remote_join_rule}" >/dev/null; then
		break
	fi
	sleep 2
done
jq -e '.join_rule == "invite"' <<<"${remote_join_rule}" >/dev/null \
	|| die "managed federation room did not converge to invite-only"
jq -e --arg manager "@alice:${SERVER_A}" --arg partner "@bob:${SERVER_B}" '
  (.users | has($manager) | not) and .users[$partner] == 0 and .users_default == 0 and
  .events["m.room.message"] == 0 and .events_default == 0 and .state_default == 100 and
  .invite == 100 and .kick == 100 and .ban == 100 and .redact == 100
' <<<"${remote_power_levels}" >/dev/null \
	|| die "managed federation room did not converge to access-manager-owned power levels"
jq -e --arg manager "@alice:${SERVER_A}" '
  any(.[]; .type == "m.room.create" and .state_key == "" and .sender == $manager and
    ((.content.additional_creators // []) | length) == 0)
' <<<"${remote_room_state}" >/dev/null \
	|| die "managed federation room access manager is not the sole room-v12 creator"

# The federation profile deliberately omits a bridge. Install this disposable acceptance instance
# from the exact ephemeral Git revision, load the same registration into Synapse A, then remove both
# before fed:up returns. This proves the policy at the real federated appservice boundary without
# turning the lab into a permanently bridge-coupled topology.
install_federation_bridge "${room_id}" "${policy_room_id}"
bridge_bot="@a2a-bridge:${SERVER_A}"
bridge_ghost="@agent-docs-qa:${SERVER_A}"

invite_matrix_user "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_policy_room}" "${bridge_bot}"
wait_for_matrix_membership "${MATRIX_A_URL}" "${ALICE_TOKEN}" \
	"${encoded_policy_room}" "${bridge_bot}" join
invite_matrix_user "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_local_room}" "${bridge_bot}"
wait_for_matrix_membership "${MATRIX_A_URL}" "${ALICE_TOKEN}" \
	"${encoded_local_room}" "${bridge_bot}" join

# A partner participant is not the access manager. Its invite remains an invite, never a join.
invite_matrix_user "${MATRIX_B_URL}" "${BOB_TOKEN}" "${encoded_policy_room}" "${bridge_ghost}"
wait_for_managed_invite_audit invite_sender_rejected
assert_matrix_user_not_joined "${MATRIX_A_URL}" "${ALICE_TOKEN}" \
	"${encoded_policy_room}" "${bridge_ghost}"

# Both a bound room with absent ghost membership and an unbound local room fail before A2A. The
# message path must not convert either event into ambient membership authority.
send_agent_mention "${MATRIX_B_URL}" "${BOB_TOKEN}" "${encoded_policy_room}" \
	"absent-membership-${RANDOM}-$$" >/dev/null
wait_for_bridge_audit_reason ghost_membership_required
assert_matrix_user_not_joined "${MATRIX_A_URL}" "${ALICE_TOKEN}" \
	"${encoded_policy_room}" "${bridge_ghost}"
send_agent_mention "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_local_room}" \
	"unbound-room-${RANDOM}-$$" >/dev/null
wait_for_bridge_audit_reason room_binding_rejected
assert_matrix_user_not_joined "${MATRIX_A_URL}" "${ALICE_TOKEN}" \
	"${encoded_local_room}" "${bridge_ghost}"

# Only Alice's exact managed invite into the declared room admits the ghost. Bob's subsequent
# federated mention reaches docs-qa once and the ghost reply federates back to org B.
invite_matrix_user "${MATRIX_A_URL}" "${ALICE_TOKEN}" "${encoded_room}" "${bridge_ghost}"
wait_for_matrix_membership "${MATRIX_A_URL}" "${ALICE_TOKEN}" \
	"${encoded_room}" "${bridge_ghost}" join
wait_for_managed_invite_audit accepted
bridge_reply_since="$(initial_sync_token "${MATRIX_B_URL}" "${BOB_TOKEN}")"
bridge_input_event="$(send_agent_mention "${MATRIX_B_URL}" "${BOB_TOKEN}" \
	"${encoded_room}" "managed-positive-${RANDOM}-$$")"
wait_for_bridge_reply "${room_id}" "${bridge_reply_since}" "${bridge_input_event}"

# The signed org-C transaction remains blocked at Synapse, and therefore cannot create another
# bridge audit or A2A attempt even while the managed ghost is active in the room.
bridge_audits_before_c="$(bridge_audit_records | jq \
	'[.[] | select(.audit_schema == "fgentic.delegation.v1")] | length')"
denied_bridge_c_response="${WORK_DIR}/denied-bridge-c.json"
denied_bridge_c_status=""
send_signed_federation_probe "${SERVER_A}" "${MATRIX_A_URL}" "${room_id}" \
	"${denied_bridge_c_response}" denied_bridge_c_status
assert_forbidden_response "denied control federation send before bridge A2A" \
	"${denied_bridge_c_status}" "${denied_bridge_c_response}"
sleep 5
bridge_records="$(bridge_audit_records)"
bridge_audits_after_c="$(jq '[.[] | select(.audit_schema == "fgentic.delegation.v1")] | length' \
	<<<"${bridge_records}")"
[ "${bridge_audits_after_c}" = "${bridge_audits_before_c}" ] \
	|| die "denied org C traffic reached bridge delegation admission"
jq -e '
  ([.[] | select(.audit_schema == "fgentic.delegation.v1" and .a2a_attempted == true)] | length) == 1 and
  ([.[] | select(.audit_schema == "fgentic.delegation.v1" and
    .terminal_stage == "room_authorization" and .a2a_attempted == false and
    (.terminal_reason == "ghost_membership_required" or
      .terminal_reason == "room_binding_rejected"))] | length) == 2 and
  ([.[] | select(.audit_schema == "fgentic.managed_room_invite.v1" and
    .reason == "invite_sender_rejected")] | length) == 1 and
  ([.[] | select(.audit_schema == "fgentic.managed_room_invite.v1" and
    .reason == "accepted")] | length) == 1 and
  all(.[] | has("body") | not) and
  all(.[] | has("content") | not) and
  all(.[] | has("prompt") | not)
' <<<"${bridge_records}" >/dev/null \
	|| die "managed-room bridge audit contract was incomplete or content-bearing"
teardown_federation_bridge

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
Bridge room: exact binding + managed invite + current membership; negative paths made zero A2A calls
Homeservers: ${SERVER_A}, ${SERVER_B}; denied control ${SERVER_C}
EOF
