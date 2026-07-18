#!/usr/bin/env bash
# Bootstrap the demo administrator and initial room without entering a pod or storing an admin
# credential. MAS's supported device-authorization flow returns a short-lived, user-backed token;
# the configured `policy.data.admin_users` entry lets that user request Synapse admin scope.
set -euo pipefail

usage() {
	cat >&2 <<'EOF'
usage: scripts/bootstrap-admin.sh [options]

Options:
  --server-name NAME       Matrix server name (default: fgentic.localhost)
  --matrix-url URL         Public Matrix client URL (default: https://matrix.<server-name>)
  --client-id ID           Configured MAS client (default: 01KX8B4Z9W6Y2Q3R5T7V0C1D8F)
  --admin-localpart NAME   IdP-managed Matrix localpart (default: alice)
  --room-localpart NAME    Initial room alias localpart (default: fgentic-demo)
  --room-name NAME         Initial room display name (default: Fgentic Demo)
  -h, --help               Show this help

The command prints a one-time verification URL. Authenticate there through the configured OIDC
provider as the selected admin user. No password, client secret, or access token is read from disk.
EOF
}

SERVER_NAME="fgentic.localhost"
MATRIX_URL=""
# This public client is declaratively managed in infra/matrix/helmrelease.yaml. Keeping one stable
# client avoids leaving a new dynamic-registration record behind on every idempotent bootstrap run.
CLIENT_ID="01KX8B4Z9W6Y2Q3R5T7V0C1D8F"
ADMIN_LOCALPART="alice"
ROOM_LOCALPART="fgentic-demo"
ROOM_NAME="Fgentic Demo"

while (($#)); do
	case "$1" in
		--server-name)
			SERVER_NAME="${2:?--server-name requires a value}"
			shift 2
			;;
		--matrix-url)
			MATRIX_URL="${2:?--matrix-url requires a value}"
			shift 2
			;;
		--client-id)
			CLIENT_ID="${2:?--client-id requires a value}"
			shift 2
			;;
		--admin-localpart)
			ADMIN_LOCALPART="${2:?--admin-localpart requires a value}"
			shift 2
			;;
		--room-localpart)
			ROOM_LOCALPART="${2:?--room-localpart requires a value}"
			shift 2
			;;
		--room-name)
			ROOM_NAME="${2:?--room-name requires a value}"
			shift 2
			;;
		-h | --help)
			usage
			exit 0
			;;
		*)
			echo "error: unknown option: $1" >&2
			usage
			exit 2
			;;
	esac
done

MATRIX_URL="${MATRIX_URL:-https://matrix.${SERVER_NAME}}"
MATRIX_URL="${MATRIX_URL%/}"

case "${MATRIX_URL}" in
	https://*) ;;
	http://127.0.0.1:* | http://localhost:*) ;;
	*)
		echo "error: --matrix-url must use HTTPS (HTTP is allowed only on loopback)" >&2
		exit 2
		;;
esac

if [[ ! "${ADMIN_LOCALPART}" =~ ^[a-z][a-z0-9._=-]{2,63}$ ]]; then
	echo "error: --admin-localpart must match ^[a-z][a-z0-9._=-]{2,63}$" >&2
	exit 2
fi
if [[ ! "${ROOM_LOCALPART}" =~ ^[a-z0-9._=-]+$ ]]; then
	echo "error: --room-localpart must contain only lowercase Matrix alias characters" >&2
	exit 2
fi
if [[ ! "${CLIENT_ID}" =~ ^[0123456789ABCDEFGHJKMNPQRSTVWXYZ]{26}$ ]]; then
	echo "error: --client-id must be a 26-character Crockford Base32 ULID" >&2
	exit 2
fi

for command in curl jq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 1
	fi
done

TMP_DIR="$(mktemp -d)"
ACCESS_TOKEN=""
cleanup() {
	ACCESS_TOKEN=""
	rm -rf "${TMP_DIR}"
}
trap cleanup EXIT INT TERM

request() {
	local method="$1"
	local url="$2"
	shift 2
	curl --silent --show-error --fail-with-body \
		--request "${method}" \
		--header 'Accept: application/json' \
		"$@" \
		"${url}"
}

request_status() {
	local output="$1"
	local method="$2"
	local url="$3"
	shift 3
	curl --silent --show-error \
		--output "${output}" \
		--write-out '%{http_code}' \
		--request "${method}" \
		--header 'Accept: application/json' \
		"$@" \
		"${url}"
}

json_required() {
	local document="$1"
	local expression="$2"
	local label="$3"
	local value
	value="$(jq -er "${expression} | select(type == \"string\" and length > 0)" <<<"${document}")" || {
		echo "error: server metadata is missing ${label}" >&2
		exit 1
	}
	printf '%s' "${value}"
}

uri_encode() {
	jq -nr --arg value "$1" '$value | @uri'
}

echo "Discovering MAS endpoints from ${MATRIX_URL}"
METADATA="$(request GET "${MATRIX_URL}/_matrix/client/unstable/org.matrix.msc2965/auth_metadata")"
DEVICE_ENDPOINT="$(json_required "${METADATA}" '.device_authorization_endpoint' device_authorization_endpoint)"
TOKEN_ENDPOINT="$(json_required "${METADATA}" '.token_endpoint' token_endpoint)"

SCOPES='urn:matrix:client:api:* urn:synapse:admin:*'
DEVICE_RESPONSE="$(request POST "${DEVICE_ENDPOINT}" \
	--header 'Content-Type: application/x-www-form-urlencoded' \
	--data-urlencode "client_id=${CLIENT_ID}" \
	--data-urlencode "scope=${SCOPES}")"
VERIFICATION_URL="$(json_required "${DEVICE_RESPONSE}" '.verification_uri_complete' verification_uri_complete)"
DEVICE_CODE="$(json_required "${DEVICE_RESPONSE}" '.device_code' device_code)"
INTERVAL="$(jq -er '.interval // 5 | select(type == "number" and . >= 1 and . <= 30)' <<<"${DEVICE_RESPONSE}")"
EXPIRES_IN="$(jq -er '.expires_in // 600 | select(type == "number" and . >= 30 and . <= 1800)' <<<"${DEVICE_RESPONSE}")"

cat <<EOF

Open this one-time URL and sign in as @${ADMIN_LOCALPART}:${SERVER_NAME}:

  ${VERIFICATION_URL}

Waiting for authorization (expires in ${EXPIRES_IN}s)...
EOF

DEADLINE=$((SECONDS + EXPIRES_IN))
while ((SECONDS < DEADLINE)); do
	TOKEN_RESPONSE="$(curl --silent --show-error \
		--request POST \
		--header 'Accept: application/json' \
		--header 'Content-Type: application/x-www-form-urlencoded' \
		--data-urlencode 'grant_type=urn:ietf:params:oauth:grant-type:device_code' \
		--data-urlencode "device_code=${DEVICE_CODE}" \
		--data-urlencode "client_id=${CLIENT_ID}" \
		"${TOKEN_ENDPOINT}")"

	TOKEN_ERROR="$(jq -r '.error // empty' <<<"${TOKEN_RESPONSE}")"
	case "${TOKEN_ERROR}" in
		"")
			ACCESS_TOKEN="$(json_required "${TOKEN_RESPONSE}" '.access_token' access_token)"
			break
			;;
		authorization_pending)
			sleep "${INTERVAL}"
			;;
		slow_down)
			INTERVAL=$((INTERVAL + 5))
			sleep "${INTERVAL}"
			;;
		access_denied | expired_token)
			echo "error: device authorization failed: ${TOKEN_ERROR}" >&2
			exit 1
			;;
		*)
			echo "error: token endpoint failed: ${TOKEN_ERROR:-malformed response}" >&2
			exit 1
			;;
	esac
done

if [ -z "${ACCESS_TOKEN}" ]; then
	echo "error: device authorization expired" >&2
	exit 1
fi

AUTH_HEADER="Authorization: Bearer ${ACCESS_TOKEN}"
WHOAMI="$(request GET "${MATRIX_URL}/_matrix/client/v3/account/whoami" --header "${AUTH_HEADER}")"
EXPECTED_MXID="@${ADMIN_LOCALPART}:${SERVER_NAME}"
ACTUAL_MXID="$(json_required "${WHOAMI}" '.user_id' user_id)"
if [ "${ACTUAL_MXID}" != "${EXPECTED_MXID}" ]; then
	echo "error: authenticated as ${ACTUAL_MXID}; expected ${EXPECTED_MXID}" >&2
	exit 1
fi
echo "SSO provisioned ${ACTUAL_MXID}"

ENCODED_MXID="$(uri_encode "${EXPECTED_MXID}")"
ADMIN_URL="${MATRIX_URL}/_synapse/admin/v2/users/${ENCODED_MXID}"
ADMIN_STATUS=""
for ((attempt = 0; attempt < 30; attempt++)); do
	ADMIN_STATUS="$(request_status "${TMP_DIR}/admin.json" GET "${ADMIN_URL}" --header "${AUTH_HEADER}")"
	[ "${ADMIN_STATUS}" != "404" ] && break
	sleep 1
done

if [ "${ADMIN_STATUS}" != "200" ]; then
	echo "error: Synapse did not expose the provisioned user (HTTP ${ADMIN_STATUS})" >&2
	sed -n '1,20p' "${TMP_DIR}/admin.json" >&2
	exit 1
fi

if ! jq -e '.admin | type == "boolean"' "${TMP_DIR}/admin.json" >/dev/null; then
	echo "error: Synapse returned a malformed administrator flag" >&2
	exit 1
fi
IS_ADMIN="$(jq -r '.admin' "${TMP_DIR}/admin.json")"
if [ "${IS_ADMIN}" != "true" ]; then
	ADMIN_STATUS="$(request_status "${TMP_DIR}/admin-update.json" PUT "${ADMIN_URL}" \
		--header "${AUTH_HEADER}" \
		--header 'Content-Type: application/json' \
		--data '{"admin":true,"logout_devices":false}')"
	if [ "${ADMIN_STATUS}" != "200" ]; then
		echo "error: could not promote ${EXPECTED_MXID} (HTTP ${ADMIN_STATUS})" >&2
		sed -n '1,20p' "${TMP_DIR}/admin-update.json" >&2
		exit 1
	fi
	echo "Promoted ${EXPECTED_MXID} to Synapse administrator"
else
	echo "Administrator already configured: ${EXPECTED_MXID}"
fi

ROOM_ALIAS="#${ROOM_LOCALPART}:${SERVER_NAME}"
ENCODED_ROOM_ALIAS="$(uri_encode "${ROOM_ALIAS}")"
DIRECTORY_URL="${MATRIX_URL}/_matrix/client/v3/directory/room/${ENCODED_ROOM_ALIAS}"
ROOM_STATUS="$(request_status "${TMP_DIR}/room.json" GET "${DIRECTORY_URL}" --header "${AUTH_HEADER}")"

if [ "${ROOM_STATUS}" = "404" ]; then
	ROOM_DOCUMENT="$(jq -nc \
		--arg alias "${ROOM_LOCALPART}" \
		--arg name "${ROOM_NAME}" \
		'{
      room_alias_name: $alias,
      name: $name,
      topic: "Humans and AI agents collaborate here.",
      preset: "private_chat",
      visibility: "private"
    }')"
	ROOM_STATUS="$(request_status "${TMP_DIR}/room-create.json" POST "${MATRIX_URL}/_matrix/client/v3/createRoom" \
		--header "${AUTH_HEADER}" \
		--header 'Content-Type: application/json' \
		--data "${ROOM_DOCUMENT}")"
	if [ "${ROOM_STATUS}" != "200" ] && [ "${ROOM_STATUS}" != "201" ]; then
		# A concurrent idempotent run may have won the alias race. Accept only if the alias now exists.
		ROOM_STATUS="$(request_status "${TMP_DIR}/room.json" GET "${DIRECTORY_URL}" --header "${AUTH_HEADER}")"
		if [ "${ROOM_STATUS}" != "200" ]; then
			echo "error: could not create ${ROOM_ALIAS} (HTTP ${ROOM_STATUS})" >&2
			sed -n '1,20p' "${TMP_DIR}/room-create.json" >&2
			exit 1
		fi
		ROOM_ID="$(json_required "$(<"${TMP_DIR}/room.json")" '.room_id' room_id)"
	else
		ROOM_ID="$(json_required "$(<"${TMP_DIR}/room-create.json")" '.room_id' room_id)"
	fi
	echo "Created ${ROOM_ALIAS} (${ROOM_ID})"
elif [ "${ROOM_STATUS}" = "200" ]; then
	ROOM_ID="$(json_required "$(<"${TMP_DIR}/room.json")" '.room_id' room_id)"
	echo "Room already exists: ${ROOM_ALIAS} (${ROOM_ID})"
else
	echo "error: room directory lookup failed (HTTP ${ROOM_STATUS})" >&2
	sed -n '1,20p' "${TMP_DIR}/room.json" >&2
	exit 1
fi

echo "Bootstrap complete. Open https://chat.${SERVER_NAME} and join ${ROOM_ALIAS}."
