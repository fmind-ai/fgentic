#!/usr/bin/env bash
# Idempotently seed the evaluation user, lobby, mapped ghosts, and one reply proof per agent.
# The active kubectl context must point at a reconciled clusters/demo installation.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly SERVER_NAME="fgentic.localhost"
readonly MAS_ADMIN_CLIENT_ID="01KX8D3M0AD3M0ADM1NC13NT01"
readonly MATRIX_URL="https://matrix.${SERVER_NAME}"
readonly AUTH_URL="https://auth.${SERVER_NAME}"
readonly EXPECTED_DEMO_REPLY="Fgentic's deterministic evaluation model is working through agentgateway and kagent."
readonly CA_CERT="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
readonly REPLY_FILTER="${ROOT_DIR}/scripts/lib/demo-reply.jq"
readonly SEED_STATE_FILTER="${ROOT_DIR}/scripts/lib/demo-seed-state.jq"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo-seed.XXXXXX")"
readonly WORK_DIR

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

MATRIX_TOKEN=""
MAS_ADMIN_TOKEN=""
cleanup() {
	MATRIX_TOKEN=""
	MAS_ADMIN_TOKEN=""
	rm -rf "${WORK_DIR}"
}
trap cleanup EXIT INT TERM

create_lobby() {
	local output_variable="$1"
	local document status created_room_id
	document="$(jq --null-input --compact-output '{
    name: "Fgentic Lobby",
    topic: "Humans and agents collaborate here.",
    preset: "private_chat",
    visibility: "private",
    creation_content: {"m.federate": false}
  }')"
	status="$(request_status "${OUTPUT}" --request POST \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' --data "${document}" \
		"${MATRIX_URL}/_matrix/client/v3/createRoom")"
	[ "${status}" = "200" ] || die "Matrix could not create #lobby (HTTP ${status})"
	created_room_id="$(jq -er '.room_id' "${OUTPUT}")"
	printf -v "${output_variable}" '%s' "${created_room_id}"
}

set_lobby_canonical_alias() {
	local room_id="$1"
	local encoded_room canonical_document status
	encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
	canonical_document="$(jq --null-input --compact-output --arg alias "${room_alias}" \
		'{alias: $alias}')"
	status="$(request_status "${OUTPUT}" --request PUT \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' --data "${canonical_document}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.canonical_alias")"
	[ "${status}" = "200" ] || die "Matrix could not set #lobby as canonical (HTTP ${status})"
}

publish_lobby_alias() {
	local room_id="$1"
	local alias_document status
	alias_document="$(jq --null-input --compact-output --arg room_id "${room_id}" \
		'{room_id: $room_id}')"
	status="$(request_status "${OUTPUT}" --request PUT \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' --data "${alias_document}" \
		"${MATRIX_URL}/_matrix/client/v3/directory/room/${encoded_alias}")"
	[ "${status}" = "200" ] || die "Matrix could not publish #lobby (HTTP ${status})"
	set_lobby_canonical_alias "${room_id}"
}

lobby_is_local_only() {
	local room_id="$1"
	local encoded_room status
	encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
	status="$(request_status "${OUTPUT}" \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.create")"
	[ "${status}" = "200" ] && jq -e '."m.federate" == false' "${OUTPUT}" >/dev/null
}

lobby_has_canonical_alias() {
	local room_id="$1"
	local encoded_room status
	encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
	status="$(request_status "${OUTPUT}" \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.canonical_alias")"
	[ "${status}" = "200" ] &&
		jq -e --arg alias "${room_alias}" '.alias == $alias' "${OUTPUT}" >/dev/null
}

probe_has_reply() {
	local ghost="$1"
	local event_id="$2"
	local encoded_event context
	encoded_event="$(jq --null-input --raw-output --arg value "${event_id}" '$value | @uri')"
	context="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/context/${encoded_event}?limit=50")" || return 1
	jq -e --arg sender "@${ghost}:${SERVER_NAME}" --arg event_id "${event_id}" \
		--arg provider "${LLM_PROVIDER}" --arg model "${LLM_MODEL}" \
		--arg expected_demo_reply "${EXPECTED_DEMO_REPLY}" \
		--from-file "${REPLY_FILTER}" <<<"${context}" >/dev/null
}

all_probes_have_replies() {
	local ghost event_id
	for ghost in "${GHOSTS[@]}"; do
		event_id="$(jq -er --arg ghost "${ghost}" '.[$ghost]' <<<"${PROBE_EVENT_IDS}")" || return 1
		probe_has_reply "${ghost}" "${event_id}" || return 1
	done
}

retire_legacy_lobby_alias() {
	local room_id="$1"
	local encoded_room status
	encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
	status="$(request_status "${OUTPUT}" --request PUT \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' --data '{}' \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/m.room.canonical_alias")"
	[ "${status}" = "200" ] || die "Matrix could not retire the legacy lobby alias (HTTP ${status})"
	status="$(request_status "${OUTPUT}" --request DELETE \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/directory/room/${encoded_alias}")"
	[ "${status}" = "200" ] || die "Matrix could not release the legacy lobby alias (HTTP ${status})"
}

for command in curl jq kubectl yq; do
	require_command "${command}"
done
[ -r "${CA_CERT}" ] || die "local CA certificate not found: ${CA_CERT}"

# Flux cannot wait for the Deployment dynamically created from the Gateway resource. Require the
# proxy's xDS-backed readiness before emitting events so a successful seed never depends on a
# control-plane startup race.
kubectl --namespace agentgateway-system rollout status deployment/agentgateway-proxy \
	--timeout=2m >/dev/null || die "agentgateway proxy did not become ready within 2 minutes"

DEMO_PASSWORD="$(bootstrap_secret_value demo-password)"
MAS_ADMIN_CLIENT_SECRET="$(bootstrap_secret_value mas-admin-client)"
LLM_PROVIDER="$(kubectl --namespace flux-system get configmap platform-settings \
	--output 'go-template={{index .data "llm_provider"}}')"
LLM_MODEL="$(kubectl --namespace flux-system get configmap platform-settings \
	--output 'go-template={{index .data "llm_model"}}')"
SOURCE_REVISION="$(kubectl --namespace flux-system get gitrepository flux-system \
	--output jsonpath='{.status.artifact.revision}')"
[[ "${SOURCE_REVISION}" =~ ^main@sha1:[0-9a-f]{40}$ ]] ||
	die "Flux source has no valid reconciled artifact revision: ${SOURCE_REVISION:-none}"
OUTPUT="${WORK_DIR}/response.json"

token_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--user "${MAS_ADMIN_CLIENT_ID}:${MAS_ADMIN_CLIENT_SECRET}" \
	--header 'Content-Type: application/x-www-form-urlencoded' \
	--data-urlencode 'grant_type=client_credentials' \
	--data-urlencode 'scope=urn:mas:admin' \
	"${AUTH_URL}/oauth2/token")"
MAS_ADMIN_TOKEN="$(jq -er '.access_token | select(type == "string" and length > 0)' <<<"${token_response}")"

status="$(request_status "${OUTPUT}" \
	--header "Authorization: Bearer ${MAS_ADMIN_TOKEN}" \
	"${AUTH_URL}/api/admin/v1/users/by-username/alice")"
case "${status}" in
200)
	user_id="$(jq -er '.data.id' "${OUTPUT}")"
	;;
404)
	create_document="$(jq --null-input --compact-output \
		'{username: "alice", displayname: "Alice Demo"}')"
	status="$(request_status "${OUTPUT}" --request POST \
		--header "Authorization: Bearer ${MAS_ADMIN_TOKEN}" \
		--header 'Content-Type: application/json' --data "${create_document}" \
		"${AUTH_URL}/api/admin/v1/users")"
	[ "${status}" = "201" ] || die "MAS could not create the demo user (HTTP ${status})"
	user_id="$(jq -er '.data.id' "${OUTPUT}")"
	;;
*)
	die "MAS demo-user lookup failed (HTTP ${status})"
	;;
esac

password_document="$(jq --null-input --compact-output --arg password "${DEMO_PASSWORD}" \
	'{password: $password, skip_password_check: true}')"
status="$(request_status "${OUTPUT}" --request POST \
	--header "Authorization: Bearer ${MAS_ADMIN_TOKEN}" \
	--header 'Content-Type: application/json' --data "${password_document}" \
	"${AUTH_URL}/api/admin/v1/users/${user_id}/set-password")"
[ "${status}" = "204" ] || die "MAS could not set the demo password (HTTP ${status})"

login_document="$(jq --null-input --compact-output --arg password "${DEMO_PASSWORD}" '{
  type: "m.login.password",
  identifier: {type: "m.id.user", user: "alice"},
  password: $password,
  initial_device_display_name: "Fgentic demo seeder"
}')"
login_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
	--header 'Content-Type: application/json' --data "${login_document}" \
	"${MATRIX_URL}/_matrix/client/v3/login")"
MATRIX_TOKEN="$(jq -er '.access_token | select(type == "string" and length > 0)' <<<"${login_response}")"

room_alias='#lobby:fgentic.localhost'
encoded_alias="$(jq --null-input --raw-output --arg value "${room_alias}" '$value | @uri')"
status="$(request_status "${OUTPUT}" \
	--header "Authorization: Bearer ${MATRIX_TOKEN}" \
	"${MATRIX_URL}/_matrix/client/v3/directory/room/${encoded_alias}")"
case "${status}" in
200)
	room_id="$(jq -er '.room_id' "${OUTPUT}")"
	if ! lobby_is_local_only "${room_id}"; then
		echo 'Migrating legacy #lobby to immutable local-only federation policy.' >&2
		legacy_room_id="${room_id}"
		create_lobby room_id
		retire_legacy_lobby_alias "${legacy_room_id}"
		publish_lobby_alias "${room_id}"
	fi
	;;
404)
	create_lobby room_id
	publish_lobby_alias "${room_id}"
	;;
*)
	die "Matrix room lookup failed (HTTP ${status})"
	;;
esac

encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
lobby_is_local_only "${room_id}" || die "#lobby is not local-only after reconciliation"
if ! lobby_has_canonical_alias "${room_id}"; then
	set_lobby_canonical_alias "${room_id}"
fi

agents_yaml="$(kubectl --namespace bridge get configmap matrix-a2a-bridge-agents \
	--output 'go-template={{index .data "agents.yaml"}}')"
GHOSTS=()
ghost_names="$(yq -r '.agents | keys | .[]' <<<"${agents_yaml}")"
while IFS= read -r ghost; do
	GHOSTS[${#GHOSTS[@]}]="${ghost}"
done <<<"${ghost_names}"
((${#GHOSTS[@]} > 0)) || die "the bridge exposes no mapped demo agents"

for ghost in "${GHOSTS[@]}"; do
	ghost_mxid="@${ghost}:${SERVER_NAME}"
	joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
	if ! jq -e --arg mxid "${ghost_mxid}" '.joined | has($mxid)' <<<"${joined}" >/dev/null; then
		invite_document="$(jq --null-input --compact-output --arg mxid "${ghost_mxid}" \
			'{user_id: $mxid}')"
		status="$(request_status "${OUTPUT}" --request POST \
			--header "Authorization: Bearer ${MATRIX_TOKEN}" \
			--header 'Content-Type: application/json' --data "${invite_document}" \
			"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/invite")"
		[ "${status}" = "200" ] || die "could not invite ${ghost_mxid} (HTTP ${status})"
	fi
done

deadline=$((SECONDS + 120))
all_joined=false
while ((SECONDS < deadline)); do
	joined="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/joined_members")"
	all_joined=true
	for ghost in "${GHOSTS[@]}"; do
		jq -e --arg mxid "@${ghost}:${SERVER_NAME}" '.joined | has($mxid)' \
			<<<"${joined}" >/dev/null || all_joined=false
	done
	[ "${all_joined}" = true ] && break
	sleep 2
done
[ "${all_joined}" = true ] || die "mapped agent ghosts did not join #lobby within 2 minutes"

marker_type='dev.fgentic.demo.seed'
encoded_marker="$(jq --null-input --raw-output --arg value "${marker_type}" '$value | @uri')"
status="$(request_status "${OUTPUT}" \
	--header "Authorization: Bearer ${MATRIX_TOKEN}" \
	"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/${encoded_marker}")"
should_seed=true
PROBE_EVENT_IDS='{}'
ghosts_json="$(printf '%s\n' "${GHOSTS[@]}" | jq --raw-input --slurp --compact-output 'split("\n")[:-1]')"
if [ "${status}" = "200" ]; then
	marker_document="$(<"${OUTPUT}")"
	if jq -e --arg provider "${LLM_PROVIDER}" --arg model "${LLM_MODEL}" \
		--arg source_revision "${SOURCE_REVISION}" \
		--argjson ghosts "${ghosts_json}" \
		--from-file "${SEED_STATE_FILTER}" \
		<<<"${marker_document}" >/dev/null; then
		PROBE_EVENT_IDS="$(jq --compact-output '.probe_event_ids' <<<"${marker_document}")"
		should_seed=false
	fi
elif [ "${status}" != "404" ]; then
	die "could not read demo seed state (HTTP ${status})"
fi

if [ "${should_seed}" = false ] && ! all_probes_have_replies; then
	echo 'Refreshing stale demo agent reply proofs.' >&2
	should_seed=true
fi

if [ "${should_seed}" = true ]; then
	PROBE_EVENT_IDS='{}'
	probe_index=0
	for ghost in "${GHOSTS[@]}"; do
		probe_index=$((probe_index + 1))
		ghost_mxid="@${ghost}:${SERVER_NAME}"
		message_document="$(jq --null-input --compact-output --arg mxid "${ghost_mxid}" '{
      msgtype: "m.text",
      body: ("Fgentic evaluation probe: " + $mxid + " confirm that the evaluation path works."),
      "m.mentions": {user_ids: [$mxid]}
    }')"
		status="$(request_status "${OUTPUT}" --request PUT \
			--header "Authorization: Bearer ${MATRIX_TOKEN}" \
			--header 'Content-Type: application/json' --data "${message_document}" \
			"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/send/m.room.message/demo-${probe_index}-$$")"
		[ "${status}" = "200" ] || die "could not post the ${ghost_mxid} demo probe (HTTP ${status})"
		event_id="$(jq -er '.event_id' "${OUTPUT}")"
		PROBE_EVENT_IDS="$(jq --compact-output --arg ghost "${ghost}" --arg event_id "${event_id}" \
			'. + {($ghost): $event_id}' <<<"${PROBE_EVENT_IDS}")"
	done
fi

deadline=$((SECONDS + 120))
all_replied=false
while ((SECONDS < deadline)); do
	if all_probes_have_replies; then
		all_replied=true
		break
	fi
	sleep 2
done
[ "${all_replied}" = true ] || die "not every mapped demo agent returned a successful reply within 2 minutes"

if [ "${should_seed}" = true ]; then
	seed_state="$(jq --null-input --compact-output --argjson probe_event_ids "${PROBE_EVENT_IDS}" \
		--arg provider "${LLM_PROVIDER}" --arg model "${LLM_MODEL}" \
		--arg source_revision "${SOURCE_REVISION}" \
		'{version: 2, probe_event_ids: $probe_event_ids, provider: $provider, model: $model,
      source_revision: $source_revision}')"
	status="$(request_status "${OUTPUT}" --request PUT \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' \
		--data "${seed_state}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/${encoded_marker}")"
	[ "${status}" = "200" ] || die "could not record demo seed state (HTTP ${status})"
fi

# The seeder session is disposable; the human logs in separately with the printed password.
curl --silent --show-error --cacert "${CA_CERT}" --request POST \
	--header "Authorization: Bearer ${MATRIX_TOKEN}" \
	"${MATRIX_URL}/_matrix/client/v3/logout" >/dev/null || true
MATRIX_TOKEN=""
MAS_ADMIN_TOKEN=""

cat <<EOF

Fgentic evaluation is ready.
URL:      https://chat.${SERVER_NAME}
User:     @alice:${SERVER_NAME}
Password: ${DEMO_PASSWORD}
Room:     #lobby:${SERVER_NAME}
Provider: ${LLM_PROVIDER} (${LLM_MODEL})

If your browser does not trust the local CA, follow the instruction printed by scripts/local-ca.sh.
EOF
