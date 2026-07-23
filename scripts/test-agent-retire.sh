#!/usr/bin/env bash
# Offline contract test for agent-retire.sh. This file also serves as the mock `curl` and `kubectl`
# executables through temporary symlinks, so the fixture can never make a live network or cluster
# call. It stubs the Matrix/MAS HTTP surface and the kagent session API and asserts the sweep's
# behavior: dry-run mutates nothing, apply mode drives leave -> deactivate -> purge in order, the
# ghost MXID is built from the server name (never a bare localpart), evidence is content-free,
# an already-clean re-run succeeds, and missing/invalid config fails closed.
set -euo pipefail

readonly AS_TOKEN_CANARY='CANARY-APPSERVICE-TOKEN-DO-NOT-LEAK'
readonly MAS_TOKEN_CANARY='CANARY-MAS-ADMIN-TOKEN-DO-NOT-LEAK'

# --- Mock dispatch: when invoked as curl/kubectl, answer from the selected scenario. ------------

mock_log() {
	[ -n "${MOCK_CALL_LOG:-}" ] || return 0
	printf '%s\n' "$*" >>"${MOCK_CALL_LOG}"
}

mock_curl() {
	local arg prev='' method='GET' output='' url=''
	for arg in "$@"; do
		case "${prev}" in
			--output) output="${arg}" ;;
			--request) method="${arg}" ;;
			*) ;;
		esac
		prev="${arg}"
		url="${arg}"
	done
	mock_log "${method} ${url}"

	case "${method} ${url}" in
		*"/_matrix/client/v3/joined_rooms?user_id="*)
			case "${SCENARIO:-happy}" in
				already_clean) printf '%s' '{"joined_rooms":[]}' >"${output}" ;;
				*) printf '%s' '{"joined_rooms":["!r1:example.test","!r2:example.test"]}' >"${output}" ;;
			esac
			printf '200'
			;;
		POST*"/leave?user_id="*)
			case "${SCENARIO:-happy}" in
				# A server error on room-leave must fail closed, not be swallowed.
				leave_5xx) printf '500' ;;
				*)
					printf '%s' '{}' >"${output}"
					printf '200'
					;;
			esac
			;;
		*"/api/admin/v1/users/by-username/"*)
			case "${SCENARIO:-happy}" in
				already_clean)
					printf '%s' '{"errors":[{"title":"not found"}]}' >"${output}"
					printf '404'
					;;
				mas_deactivated)
					printf '%s' '{"data":{"type":"user","id":"01GHOSTID","attributes":{"deactivated_at":"2026-07-20T00:00:00Z"}}}' >"${output}"
					printf '200'
					;;
				mas_lookup_5xx) printf '500' ;;
				*)
					printf '%s' '{"data":{"type":"user","id":"01GHOSTID","attributes":{}}}' >"${output}"
					printf '200'
					;;
			esac
			;;
		POST*"/api/admin/v1/users/"*"/deactivate")
			case "${SCENARIO:-happy}" in
				# A server error on deactivation must fail closed.
				deactivate_5xx) printf '500' ;;
				*)
					printf '%s' '{}' >"${output}"
					printf '204'
					;;
			esac
			;;
		*)
			printf 'unexpected curl call: %s %s\n' "${method}" "${url}" >&2
			return 2
			;;
	esac
}

mock_kubectl() {
	mock_log "kubectl $*"
	case "$*" in
		"get --raw "*"/api/sessions?agent_ref="*)
			case "${SCENARIO:-happy}" in
				kagent_unavailable) return 1 ;;
				already_clean) printf '%s\n' '{"error":false,"data":[]}' ;;
				*) printf '%s\n' '{"error":false,"data":[{"id":"ctx-1","user_id":"@alice:example.test"},{"id":"ctx-2","user_id":"@bob:example.test"}]}' ;;
			esac
			;;
		"delete --raw "*"/api/sessions/"*)
			case "${SCENARIO:-happy}" in
				# A transport/RBAC failure (not a 404) must fail closed, never a green idempotent claim.
				purge_transport_fail)
					printf 'Unable to connect to the server: connection refused\n' >&2
					return 1
					;;
				*) return 0 ;;
			esac
			;;
		"get --raw "*"/api/sessions/"*)
			# Existence recheck after a delete failure; not reached in the happy path.
			return 1
			;;
		*)
			printf 'unexpected kubectl call: %s\n' "$*" >&2
			return 2
			;;
	esac
}

case "${0##*/}" in
	curl)
		mock_curl "$@"
		exit
		;;
	kubectl)
		mock_kubectl "$@"
		exit
		;;
	*) ;;
esac

# --- Driver ------------------------------------------------------------------------------------

for command in jq rg; do
	command -v "${command}" >/dev/null 2>&1 || {
		printf 'error: required test command not found: %s\n' "${command}" >&2
		exit 2
	}
done

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
readonly SCRIPT="${REPO_ROOT}/scripts/agent-retire.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-agent-retire-test.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

readonly BIN_DIR="${WORK_DIR}/bin"
mkdir -p "${BIN_DIR}"
ln -s "${REPO_ROOT}/scripts/test-agent-retire.sh" "${BIN_DIR}/curl"
ln -s "${REPO_ROOT}/scripts/test-agent-retire.sh" "${BIN_DIR}/kubectl"
readonly CA_FILE="${WORK_DIR}/ca.crt"
: >"${CA_FILE}"

fail() {
	printf 'FAIL: %s\n' "$*" >&2
	exit 1
}

# run <name> <apply> <scenario> [server_name] -> populates STDERR_FILE/CALL_LOG/RC for the run.
run() {
	local name="$1" apply="$2" scenario="$3" server="${4:-fgentic.localhost}"
	STDERR_FILE="${WORK_DIR}/${name}.err"
	CALL_LOG="${WORK_DIR}/${name}.calls"
	: >"${CALL_LOG}"
	set +e
	env -i \
		PATH="${BIN_DIR}:${PATH}" HOME="${WORK_DIR}" \
		SCENARIO="${scenario}" MOCK_CALL_LOG="${CALL_LOG}" \
		AGENT_RETIRE_APPLY="${apply}" \
		FGENTIC_SERVER_NAME="${server}" \
		AGENT_RETIRE_CA_CERT="${CA_FILE}" \
		AGENT_RETIRE_AS_TOKEN="${AS_TOKEN_CANARY}" \
		AGENT_RETIRE_MAS_ADMIN_TOKEN="${MAS_TOKEN_CANARY}" \
		bash "${SCRIPT}" retiree >"${WORK_DIR}/${name}.out" 2>"${STDERR_FILE}"
	RC=$?
	set -e
}

line_of() {
	rg --line-number --fixed-strings "$1" "${STDERR_FILE}" | head -n1 | cut -d: -f1
}

# 1. Dry-run performs no mutation.
run dry no happy
[ "${RC}" -eq 0 ] || fail "dry-run exited ${RC}"
rg --quiet 'DRY-RUN would leave room' "${STDERR_FILE}" || fail "dry-run did not preview a room leave"
rg --quiet 'DRY-RUN would deactivate MAS account' "${STDERR_FILE}" || fail "dry-run did not preview deactivation"
rg --quiet 'DRY-RUN would purge kagent session' "${STDERR_FILE}" || fail "dry-run did not preview a purge"
if rg --quiet '^POST ' "${CALL_LOG}" || rg --quiet 'kubectl delete --raw' "${CALL_LOG}"; then
	fail "dry-run issued a mutating call"
fi

# 2. Apply mode drives leave -> deactivate -> purge in order.
run apply yes happy
[ "${RC}" -eq 0 ] || fail "apply exited ${RC}"
rg --quiet 'left room !r1:example.test \(HTTP 200\)' "${STDERR_FILE}" || fail "apply did not leave a room"
rg --quiet 'deactivated MAS account 01GHOSTID' "${STDERR_FILE}" || fail "apply did not deactivate"
rg --quiet 'purged kagent session ctx-1' "${STDERR_FILE}" || fail "apply did not purge a session"
leave_line="$(line_of 'left room !r1:example.test')"
deact_line="$(line_of 'deactivated MAS account')"
purge_line="$(line_of 'purged kagent session ctx-1')"
{ [ "${leave_line}" -lt "${deact_line}" ] && [ "${deact_line}" -lt "${purge_line}" ]; } \
	|| fail "sweep order was not leave (${leave_line}) -> deactivate (${deact_line}) -> purge (${purge_line})"
rg --quiet 'kubectl delete --raw .*/api/sessions/ctx-1' "${CALL_LOG}" || fail "apply did not call the kagent purge"
rg --quiet 'audit records retained unchanged' "${STDERR_FILE}" || fail "apply did not affirm the audit invariant"

# 3. The ghost MXID is derived from the server name, never a bare localpart.
run mxid yes happy example.org
rg --quiet '@agent-retiree:example.org' "${STDERR_FILE}" || fail "MXID was not built from the server name"
rg --quiet 'user_id=%40agent-retiree%3Aexample.org' "${CALL_LOG}" \
	|| fail "Matrix call did not masquerade as the full ghost MXID"
if rg --quiet 'user_id=agent-retiree(&|$)' "${CALL_LOG}"; then
	fail "a request used a bare localpart instead of the full MXID"
fi

# 4. Evidence is content-free: no token ever leaks into the script's output.
for artifact in "${WORK_DIR}/apply.err" "${WORK_DIR}/apply.out" "${WORK_DIR}/dry.err" "${WORK_DIR}/dry.out"; do
	if rg --quiet --fixed-strings "${AS_TOKEN_CANARY}" "${artifact}" \
		|| rg --quiet --fixed-strings "${MAS_TOKEN_CANARY}" "${artifact}"; then
		fail "a token leaked into ${artifact}"
	fi
done

# 5. Idempotent re-run on already-clean state succeeds (no rooms, no MAS account, no sessions).
run clean yes already_clean
[ "${RC}" -eq 0 ] || fail "already-clean re-run exited ${RC}"
rg --quiet 'no rooms to leave \(idempotent\)' "${STDERR_FILE}" || fail "clean run did not report zero rooms"
rg --quiet 'no MAS account for @agent-retiree:fgentic.localhost' "${STDERR_FILE}" || fail "clean run did not tolerate a missing account"
rg --quiet 'no kagent sessions to purge \(idempotent\)' "${STDERR_FILE}" || fail "clean run did not report zero sessions"

# 5b. An already-deactivated MAS account is idempotent, not an error.
run deact yes mas_deactivated
[ "${RC}" -eq 0 ] || fail "already-deactivated re-run exited ${RC}"
rg --quiet 'already deactivated \(idempotent\)' "${STDERR_FILE}" || fail "did not treat an existing deactivation as idempotent"

# 5c. A missing kagent purge capability degrades honestly (deferred to #100), not silent success.
run defer yes kagent_unavailable
[ "${RC}" -eq 0 ] || fail "kagent-unavailable run exited ${RC}"
rg --quiet 'deferring \(room, ghost\) contextId purge to the #100 mechanism' "${STDERR_FILE}" \
	|| fail "did not defer the purge on a not-available kagent signal"
rg --quiet 'purge deferred for \(!r1:example.test, @agent-retiree:fgentic.localhost\)' "${STDERR_FILE}" \
	|| fail "did not emit per-(room, ghost) deferral evidence"
if rg --quiet 'purged kagent session' "${STDERR_FILE}"; then
	fail "claimed a purge while the mechanism was unavailable"
fi

# 5d. Genuine API failures fail closed with a wrapped diagnostic (never swallowed). Each scenario
# aborts the sweep at a different step, proving every mutating call's error branch exits non-zero.
assert_sweep_fails() {
	local name="$1" scenario="$2" needle="$3"
	run "${name}" yes "${scenario}"
	[ "${RC}" -ne 0 ] || fail "${name}: expected non-zero exit for scenario ${scenario}"
	rg --quiet --fixed-strings "${needle}" "${STDERR_FILE}" \
		|| fail "${name}: missing wrapped diagnostic '${needle}'"
}
assert_sweep_fails leave5xx leave_5xx 'could not leave room'
assert_sweep_fails maslookup5xx mas_lookup_5xx 'MAS lookup'
assert_sweep_fails deact5xx deactivate_5xx 'MAS deactivation'
# A delete transport failure must abort, NOT report "already absent (idempotent)".
assert_sweep_fails purgefail purge_transport_fail 'could not purge kagent session'
if rg --quiet 'already absent \(idempotent\)' "${WORK_DIR}/purgefail.err"; then
	fail "a transport failure was misreported as an idempotent purge"
fi

# 6. Missing/invalid config fails closed with a clear message.
assert_fail() {
	local desc="$1" needle="$2"
	shift 2
	set +e
	local err="${WORK_DIR}/failclosed.err"
	env -i PATH="${BIN_DIR}:${PATH}" HOME="${WORK_DIR}" SCENARIO=happy \
		AGENT_RETIRE_AS_TOKEN="${AS_TOKEN_CANARY}" \
		AGENT_RETIRE_MAS_ADMIN_TOKEN="${MAS_TOKEN_CANARY}" \
		"$@" bash "${SCRIPT}" retiree 2>"${err}" >/dev/null
	local rc=$?
	set -e
	[ "${rc}" -ne 0 ] || fail "${desc}: expected non-zero exit"
	rg --quiet --fixed-strings "${needle}" "${err}" || fail "${desc}: missing diagnostic '${needle}'"
}

assert_fail "unreadable CA certificate" 'CA certificate not found' \
	AGENT_RETIRE_CA_CERT="${WORK_DIR}/absent-ca.crt"
assert_fail "invalid apply flag" 'AGENT_RETIRE_APPLY must be yes or no' \
	AGENT_RETIRE_CA_CERT="${CA_FILE}" AGENT_RETIRE_APPLY=maybe

# Missing positional argument exits with the usage code.
set +e
env -i PATH="${BIN_DIR}:${PATH}" HOME="${WORK_DIR}" bash "${SCRIPT}" >/dev/null 2>&1
missing_arg_rc=$?
set -e
[ "${missing_arg_rc}" -eq 2 ] || fail "missing argument did not exit 2 (got ${missing_arg_rc})"

echo "agent-retire.sh offline contract tests passed"
