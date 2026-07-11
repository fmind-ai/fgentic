#!/usr/bin/env bash
# Idempotently seed the evaluation user, lobby, mapped ghosts, welcome mention, and reply proof.
# The active kubectl context must point at a reconciled clusters/demo installation.
set -euo pipefail

readonly SERVER_NAME="fgentic.localhost"
readonly MAS_ADMIN_CLIENT_ID="01KX8D3M0AD3M0ADM1NC13NT01"
readonly MATRIX_URL="https://matrix.${SERVER_NAME}"
readonly AUTH_URL="https://auth.${SERVER_NAME}"
readonly CA_CERT="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo-seed.XXXXXX")"

MATRIX_TOKEN=""
MAS_ADMIN_TOKEN=""
cleanup() {
	MATRIX_TOKEN=""
	MAS_ADMIN_TOKEN=""
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

for command in curl jq kubectl yq; do
	require_command "${command}"
done
[ -r "${CA_CERT}" ] || die "local CA certificate not found: ${CA_CERT}"

DEMO_PASSWORD="$(bootstrap_secret_value demo-password)"
MAS_ADMIN_CLIENT_SECRET="$(bootstrap_secret_value mas-admin-client)"
LLM_PROVIDER="$(kubectl --namespace flux-system get configmap platform-settings \
	--output 'go-template={{index .data "llm_provider"}}')"
LLM_MODEL="$(kubectl --namespace flux-system get configmap platform-settings \
	--output 'go-template={{index .data "llm_model"}}')"
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
	;;
404)
	room_document="$(jq --null-input --compact-output '{
    room_alias_name: "lobby",
    name: "Fgentic Lobby",
    topic: "Humans and agents collaborate here.",
    preset: "private_chat",
    visibility: "private"
  }')"
	status="$(request_status "${OUTPUT}" --request POST \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' --data "${room_document}" \
		"${MATRIX_URL}/_matrix/client/v3/createRoom")"
	[ "${status}" = "200" ] || die "Matrix could not create #lobby (HTTP ${status})"
	room_id="$(jq -er '.room_id' "${OUTPUT}")"
	;;
*)
	die "Matrix room lookup failed (HTTP ${status})"
	;;
esac

encoded_room="$(jq --null-input --raw-output --arg value "${room_id}" '$value | @uri')"
agents_yaml="$(kubectl --namespace bridge get configmap matrix-a2a-bridge-agents \
	--output 'go-template={{index .data "agents.yaml"}}')"
GHOSTS=()
while IFS= read -r ghost; do
	GHOSTS[${#GHOSTS[@]}]="${ghost}"
done < <(yq -r '.agents | keys | .[]' <<<"${agents_yaml}")
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
first_ghost="${GHOSTS[0]}"
first_mxid="@${first_ghost}:${SERVER_NAME}"
should_seed=true
event_id=""
if [ "${status}" = "200" ]; then
	marker_document="$(<"${OUTPUT}")"
	event_id="$(jq -r '.welcome_event_id // empty' <<<"${marker_document}")"
	if jq -e --arg provider "${LLM_PROVIDER}" --arg model "${LLM_MODEL}" \
		'.version == 1 and .provider == $provider and .model == $model and
     (.welcome_event_id | type == "string" and length > 0)' \
		<<<"${marker_document}" >/dev/null; then
		should_seed=false
	fi
elif [ "${status}" != "404" ]; then
	die "could not read demo seed state (HTTP ${status})"
fi

if [ "${should_seed}" = true ]; then
	message_document="$(jq --null-input --compact-output --arg mxid "${first_mxid}" '{
    msgtype: "m.text",
    body: ("Welcome to Fgentic. Try " + $mxid + " confirm that the evaluation path works."),
    "m.mentions": {user_ids: [$mxid]}
  }')"
	status="$(request_status "${OUTPUT}" --request PUT \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' --data "${message_document}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/send/m.room.message/demo-$$")"
	[ "${status}" = "200" ] || die "could not post the demo welcome message (HTTP ${status})"
	event_id="$(jq -er '.event_id' "${OUTPUT}")"
	status="$(request_status "${OUTPUT}" --request PUT \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		--header 'Content-Type: application/json' \
		--data "$(jq --null-input --compact-output --arg event_id "${event_id}" \
			--arg provider "${LLM_PROVIDER}" --arg model "${LLM_MODEL}" \
			'{version: 1, welcome_event_id: $event_id, provider: $provider, model: $model}')" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/state/${encoded_marker}")"
	[ "${status}" = "200" ] || die "could not record demo seed state (HTTP ${status})"
fi

deadline=$((SECONDS + 120))
while ((SECONDS < deadline)); do
	messages="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--header "Authorization: Bearer ${MATRIX_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/messages?dir=b&limit=30")"
	if jq -e --arg sender "${first_mxid}" --arg event_id "${event_id}" \
		'.chunk[] | select(
      .type == "m.room.message" and
      .sender == $sender and
      .content."m.relates_to"."m.in_reply_to".event_id == $event_id
    )' \
		<<<"${messages}" >/dev/null; then
		break
	fi
	sleep 2
done
((SECONDS < deadline)) || die "the seeded agent mention did not receive a reply within 2 minutes"

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
