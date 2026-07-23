#!/usr/bin/env bash
# Ownership-guarded residual-state sweep for a retired platform agent (issue #453, step 3).
#
# Run this AFTER the agent's `agents.yaml` mapping (and any repo-owned kagent Agent CRD) has been
# removed through the reviewed GitOps path. The bridge already fails closed on the missing mapping;
# this script clears what the mapping deletion leaves behind, in the runbook's exact order:
#   (a) the local ghost leaves every room it is joined to (appservice-token masquerade);
#   (b) the ghost's Matrix account is handled through the supported MAS admin deactivation action;
#   (c) kagent sessions for every (room, ghost) contextId are purged;
#   (d) delegation audit records are NEVER touched (hard invariant).
#
# It is dry-run by default; set AGENT_RETIRE_APPLY=yes to perform the leave/deactivate/purge. The
# emergency-disable fast path is GitOps-only and documented in the runbook; this is the deliberate,
# non-urgent post-removal sweep. All evidence is content-free: only UTC timestamps, HTTP statuses,
# and resource identifiers are emitted -- never room content, tokens, or credentials.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

readonly SERVER_NAME="${FGENTIC_SERVER_NAME:-fgentic.localhost}"
readonly MATRIX_URL="${AGENT_RETIRE_MATRIX_URL:-https://matrix.${SERVER_NAME}}"
readonly AUTH_URL="${AGENT_RETIRE_AUTH_URL:-https://auth.${SERVER_NAME}}"
readonly BRIDGE_NAMESPACE="${AGENT_RETIRE_BRIDGE_NAMESPACE:-bridge}"
readonly KAGENT_NAMESPACE="${AGENT_RETIRE_KAGENT_NAMESPACE:-kagent}"
# Reach the kagent controller through the same in-cluster service proxy audit-attribution.sh uses.
readonly KAGENT_API_BASE="${AGENT_RETIRE_KAGENT_API_BASE:-/api/v1/namespaces/${KAGENT_NAMESPACE}/services/http:kagent-controller:8083/proxy}"
readonly MAS_ADMIN_CLIENT_ID="${AGENT_RETIRE_MAS_ADMIN_CLIENT_ID:-01KX8D3M0AD3M0ADM1NC13NT01}"
readonly CA_CERT="${AGENT_RETIRE_CA_CERT:-${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt}"

usage() {
	cat <<'EOF'
usage: scripts/agent-retire.sh <agent-name>

Sweeps the residual state of a retired local agent whose mapping (and any repo-owned kagent Agent)
has already been removed through GitOps. <agent-name> is the localpart segment of the ghost, i.e.
@agent-<agent-name>:<server_name>. Dry-run by default; set AGENT_RETIRE_APPLY=yes to execute.

Sweep order (runbook "retire an agent", step 3):
  a. ghost leaves every joined room via the appservice token
  b. ghost Matrix account handled via the supported MAS admin deactivation action
  c. kagent sessions for every (room, ghost) contextId purged
  d. delegation audit records retained, never deleted

Environment:
  AGENT_RETIRE_APPLY            yes performs the sweep; anything else is a no-mutation dry run (default: no)
  FGENTIC_SERVER_NAME          homeserver name for the ghost MXID (default: fgentic.localhost)
  AGENT_RETIRE_MATRIX_URL      Synapse client-server base URL (default: https://matrix.<server_name>)
  AGENT_RETIRE_AUTH_URL        MAS base URL (default: https://auth.<server_name>)
  AGENT_RETIRE_AS_TOKEN        appservice as_token; read from the bridge registration Secret when unset
  AGENT_RETIRE_MAS_ADMIN_TOKEN MAS admin bearer token; minted via client-credentials when unset
  AGENT_RETIRE_MAS_ADMIN_CLIENT_ID
                               MAS admin client id for the client-credentials grant
  AGENT_RETIRE_BRIDGE_NAMESPACE, AGENT_RETIRE_KAGENT_NAMESPACE
                               Kubernetes namespaces (defaults: bridge, kagent)
  AGENT_RETIRE_KAGENT_API_BASE kagent controller API base reached via `kubectl --raw`
  AGENT_RETIRE_CA_CERT         CA certificate for TLS to Matrix/MAS (default: local-CA path)
EOF
}

if (($# != 1)); then
	usage >&2
	exit 2
fi
case "$1" in
	-h | --help)
		usage
		exit 0
		;;
	*) ;;
esac

readonly AGENT_NAME="$1"
# One lowercase DNS label, matching the add-an-agent contract. Rejecting anything else keeps the
# value safe to interpolate into URLs and API paths and refuses accidental full-MXID input.
[[ "${AGENT_NAME}" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]] \
	|| die "invalid agent name '${AGENT_NAME}': expected one lowercase DNS label (the localpart of @agent-<name>)"
[[ "${SERVER_NAME}" =~ ^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$ ]] \
	|| die "invalid FGENTIC_SERVER_NAME '${SERVER_NAME}'"

# Federation-safe (D6): the ghost is only ever addressed as the local homeserver's own account.
# We never match or derive a user by localpart alone, and never resolve another homeserver's ghost.
readonly GHOST_LOCALPART="agent-${AGENT_NAME}"
readonly GHOST_MXID="@${GHOST_LOCALPART}:${SERVER_NAME}"

case "${AGENT_RETIRE_APPLY:-no}" in
	yes) readonly APPLY="yes" ;;
	"" | no) readonly APPLY="no" ;;
	*) die "AGENT_RETIRE_APPLY must be yes or no" ;;
esac

for command in curl jq kubectl yq; do
	require_command "${command}"
done
[ -r "${CA_CERT}" ] || die "CA certificate not found or unreadable: ${CA_CERT}"

WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-agent-retire.XXXXXX")"
readonly WORK_DIR
readonly OUTPUT="${WORK_DIR}/response.json"
AS_TOKEN=""
MAS_ADMIN_TOKEN=""
cleanup() {
	AS_TOKEN=""
	MAS_ADMIN_TOKEN=""
	rm -rf "${WORK_DIR}"
}
# EXIT owns the actual cleanup; INT/TERM re-raise the conventional 128+signal code so a Ctrl-C
# aborts instead of falling through to writes under the just-removed work dir.
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# Content-free, UTC-timestamped evidence to stderr (federation.sh style). Callers pass only
# statuses and resource identifiers; message bodies, tokens, and credentials must never appear.
step() {
	local timestamp
	timestamp="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	printf '[%s] %s\n' "${timestamp}" "$*" >&2
}

apply_enabled() {
	[ "${APPLY}" = "yes" ]
}

mode_label() {
	if apply_enabled; then
		printf 'apply'
	else
		printf 'dry-run'
	fi
}

uri_encode() {
	jq -rn --arg value "$1" '$value | @uri'
}

load_appservice_token() {
	if [ -n "${AGENT_RETIRE_AS_TOKEN:-}" ]; then
		AS_TOKEN="${AGENT_RETIRE_AS_TOKEN}"
	else
		local registration
		registration="$(kubectl --namespace "${BRIDGE_NAMESPACE}" get secret matrix-a2a-bridge-registration \
			--output 'go-template={{index .data "registration.yaml" | base64decode}}')" \
			|| die "could not read the bridge appservice registration Secret"
		AS_TOKEN="$(yq -r '.as_token' <<<"${registration}")" \
			|| die "could not parse the appservice as_token"
	fi
	[ -n "${AS_TOKEN}" ] && [ "${AS_TOKEN}" != "null" ] || die "appservice token is empty"
}

load_mas_admin_token() {
	if [ -n "${AGENT_RETIRE_MAS_ADMIN_TOKEN:-}" ]; then
		MAS_ADMIN_TOKEN="${AGENT_RETIRE_MAS_ADMIN_TOKEN}"
		[ -n "${MAS_ADMIN_TOKEN}" ] || die "MAS admin token is empty"
		return 0
	fi
	local client_secret token_response curl_config
	client_secret="$(bootstrap_secret_value mas-admin-client)" \
		|| die "could not read the MAS admin client secret"
	# Pass the client credentials through a 0600 config file rather than argv, so the secret is
	# never visible in the process table (/proc/<pid>/cmdline, ps) on a shared host.
	curl_config="${WORK_DIR}/mas-client.config"
	(
		umask 077
		printf 'user = "%s:%s"\n' "${MAS_ADMIN_CLIENT_ID}" "${client_secret}" >"${curl_config}"
	)
	token_response="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		--config "${curl_config}" \
		--header 'Content-Type: application/x-www-form-urlencoded' \
		--data-urlencode 'grant_type=client_credentials' \
		--data-urlencode 'scope=urn:mas:admin' \
		"${AUTH_URL}/oauth2/token")" \
		|| die "MAS client-credentials grant failed"
	rm -f "${curl_config}"
	MAS_ADMIN_TOKEN="$(jq -er '.access_token | select(type == "string" and length > 0)' <<<"${token_response}")" \
		|| die "MAS did not return an admin access token"
}

# (a) Enumerate the ghost's joined rooms and leave each one.
sweep_rooms() {
	local encoded_user status
	encoded_user="$(uri_encode "${GHOST_MXID}")"
	status="$(request_status "${OUTPUT}" \
		--header "Authorization: Bearer ${AS_TOKEN}" \
		"${MATRIX_URL}/_matrix/client/v3/joined_rooms?user_id=${encoded_user}")"
	[ "${status}" = "200" ] \
		|| die "could not enumerate joined rooms for ${GHOST_MXID} (HTTP ${status})"

	local rooms_raw
	rooms_raw="$(jq -r '.joined_rooms[]? // empty' "${OUTPUT}")"
	local -a rooms=()
	[ -z "${rooms_raw}" ] || mapfile -t rooms <<<"${rooms_raw}"
	step "step a: ${GHOST_MXID} is joined to ${#rooms[@]} room(s)"
	ROOM_IDS=("${rooms[@]}")

	local room
	for room in "${rooms[@]}"; do
		leave_room "${room}"
	done
	((${#rooms[@]} > 0)) || step "step a: no rooms to leave (idempotent)"
}

leave_room() {
	local room_id="$1" encoded_room encoded_user status
	if ! apply_enabled; then
		step "step a: DRY-RUN would leave room ${room_id}"
		return 0
	fi
	encoded_room="$(uri_encode "${room_id}")"
	encoded_user="$(uri_encode "${GHOST_MXID}")"
	status="$(request_status "${OUTPUT}" --request POST \
		--header "Authorization: Bearer ${AS_TOKEN}" \
		--header 'Content-Type: application/json' --data '{}' \
		"${MATRIX_URL}/_matrix/client/v3/rooms/${encoded_room}/leave?user_id=${encoded_user}")"
	case "${status}" in
		200) step "step a: left room ${room_id} (HTTP 200)" ;;
		# Not in the room / room gone: already-clean state is success, not an error (idempotent).
		403 | 404) step "step a: room ${room_id} already left (HTTP ${status})" ;;
		*) die "could not leave room ${room_id} (HTTP ${status})" ;;
	esac
}

# (b) Handle the ghost's Matrix account through the supported MAS admin deactivation action. An
# appservice-namespace ghost cannot be re-registered by anyone else; deactivation additionally
# invalidates its sessions and blocks any residual login.
sweep_account() {
	local encoded_localpart status user_id deactivated
	encoded_localpart="$(uri_encode "${GHOST_LOCALPART}")"
	status="$(request_status "${OUTPUT}" \
		--header "Authorization: Bearer ${MAS_ADMIN_TOKEN}" \
		"${AUTH_URL}/api/admin/v1/users/by-username/${encoded_localpart}")"
	case "${status}" in
		200) ;;
		404)
			# No MAS account for the exclusive-namespace ghost: nothing to deactivate (idempotent).
			step "step b: no MAS account for ${GHOST_MXID} (HTTP 404); deactivation not applicable"
			return 0
			;;
		*) die "MAS lookup for ${GHOST_MXID} failed (HTTP ${status})" ;;
	esac

	user_id="$(jq -er '.data.id' "${OUTPUT}")" || die "MAS account response missing id"
	deactivated="$(jq -r '.data.attributes.deactivated_at // .data.attributes.deactivated // empty' "${OUTPUT}")"
	if [ -n "${deactivated}" ]; then
		step "step b: MAS account for ${GHOST_MXID} already deactivated (idempotent)"
		return 0
	fi

	if ! apply_enabled; then
		step "step b: DRY-RUN would deactivate MAS account ${user_id} for ${GHOST_MXID}"
		return 0
	fi
	status="$(request_status "${OUTPUT}" --request POST \
		--header "Authorization: Bearer ${MAS_ADMIN_TOKEN}" \
		--header 'Content-Type: application/json' --data '{}' \
		"${AUTH_URL}/api/admin/v1/users/${user_id}/deactivate")"
	case "${status}" in
		200 | 204) step "step b: deactivated MAS account ${user_id} for ${GHOST_MXID} (HTTP ${status})" ;;
		*) die "MAS deactivation for ${GHOST_MXID} failed (HTTP ${status})" ;;
	esac
}

# (c) Purge kagent sessions for every (room, ghost) contextId. The exact (room, ghost) -> contextId
# map is bridge-owned state and kagent sessions carry no room label, so a decommission-time
# "purge this agent's sessions" capability is required (issue #100). We attempt kagent's session
# API; on a clean not-available signal we log that the purge is deferred to that mechanism per
# (room, ghost) rather than fabricating success.
sweep_sessions() {
	local encoded_agent listing
	encoded_agent="$(uri_encode "${KAGENT_NAMESPACE}/${AGENT_NAME}")"
	if ! listing="$(kubectl get --raw "${KAGENT_API_BASE}/api/sessions?agent_ref=${encoded_agent}" 2>/dev/null)"; then
		defer_session_purge "kagent session API did not answer the decommission listing"
		return 0
	fi
	if ! jq -e 'has("data") and (.error != true)' <<<"${listing}" >/dev/null 2>&1; then
		defer_session_purge "kagent session API reports no decommission listing capability"
		return 0
	fi

	local sessions_raw
	sessions_raw="$(jq -rc '.data[]? | [.id, .user_id] | @tsv' <<<"${listing}")"
	local -a sessions=()
	[ -z "${sessions_raw}" ] || mapfile -t sessions <<<"${sessions_raw}"
	step "step c: kagent reports ${#sessions[@]} session(s) for ${GHOST_MXID}"
	if ((${#sessions[@]} == 0)); then
		step "step c: no kagent sessions to purge (idempotent)"
		return 0
	fi

	local line session_id user_id
	for line in "${sessions[@]}"; do
		IFS=$'\t' read -r session_id user_id <<<"${line}"
		purge_session "${session_id}" "${user_id}"
	done
}

purge_session() {
	local session_id="$1" user_id="$2" encoded_session encoded_user delete_err reason
	if ! apply_enabled; then
		step "step c: DRY-RUN would purge kagent session ${session_id}"
		return 0
	fi
	encoded_session="$(uri_encode "${session_id}")"
	encoded_user="$(uri_encode "${user_id}")"
	delete_err="${WORK_DIR}/session-delete.err"
	if kubectl delete --raw "${KAGENT_API_BASE}/api/sessions/${encoded_session}?user_id=${encoded_user}" >/dev/null 2>"${delete_err}"; then
		step "step c: purged kagent session ${session_id}"
		return 0
	fi
	# Fail closed: only a genuine not-found is already-clean state (idempotent). A transport error,
	# RBAC denial, or 5xx must abort -- `kubectl delete/get --raw` share exit code 1 for a 404 and a
	# connection failure, so a green idempotent claim on an outage would silently leave a live
	# session behind, exactly the stale-session risk the runbook warns against.
	if grep -qiE 'notfound|not found|\(404\)' "${delete_err}"; then
		step "step c: kagent session ${session_id} already absent (idempotent)"
		return 0
	fi
	reason="$(tr -d '\r\n' <"${delete_err}")"
	die "could not purge kagent session ${session_id}: ${reason}"
}

defer_session_purge() {
	local reason="$1" room
	step "step c: ${reason}; deferring (room, ghost) contextId purge to the #100 mechanism"
	for room in "${ROOM_IDS[@]}"; do
		step "step c: purge deferred for (${room}, ${GHOST_MXID})"
	done
	((${#ROOM_IDS[@]} > 0)) \
		|| step "step c: no (room, ghost) pairs recorded for deferral"
}

ROOM_IDS=()
MODE_LABEL="$(mode_label)"
step "retire sweep for ${GHOST_MXID} (mode: ${MODE_LABEL})"
load_appservice_token
load_mas_admin_token
sweep_rooms
sweep_account
sweep_sessions
step "step d: delegation audit records retained unchanged (never deleted)"
step "retire sweep complete for ${GHOST_MXID}"
