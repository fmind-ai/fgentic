#!/usr/bin/env bash
# Serialize the final repository gates across worktrees sharing one development host.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir

mise_bin="$(command -v mise || true)"
if [[ -z "${mise_bin}" && -x "${HOME}/.local/bin/mise" ]]; then
	mise_bin="${HOME}/.local/bin/mise"
fi
[[ -n "${mise_bin}" ]] || {
	echo "error: mise is required" >&2
	exit 2
}

wait_seconds="${FGENTIC_AGENT_GATE_WAIT_SECONDS:-3600}"
[[ "${wait_seconds}" =~ ^[1-9][0-9]*$ ]] || {
	echo "error: FGENTIC_AGENT_GATE_WAIT_SECONDS must be a positive integer" >&2
	exit 2
}

lock_dir="${TMPDIR:-/tmp}"
lock_dir="${lock_dir%/}/fgentic-agent-gate.lock"
owner_file="${lock_dir}/owner"
host="$(hostname)"
started_epoch="$(date +%s)"
readonly lock_dir owner_file host started_epoch wait_seconds

cleanup() {
	rm -rf -- "${lock_dir}"
}

waiting_reported=false
while ! mkdir "${lock_dir}" 2>/dev/null; do
	if [[ -r "${owner_file}" ]]; then
		IFS=$'\t' read -r owner_host owner_pid owner_started owner_worktree <"${owner_file}" || true
		if [[ "${owner_host:-}" == "${host}" && "${owner_pid:-}" =~ ^[1-9][0-9]*$ ]] &&
			! kill -0 "${owner_pid}" 2>/dev/null; then
			stale_dir="${lock_dir}.stale.$$"
			if mv "${lock_dir}" "${stale_dir}" 2>/dev/null; then
				rm -rf -- "${stale_dir}"
				continue
			fi
		fi
	fi

	now_epoch="$(date +%s)"
	if ((now_epoch - started_epoch >= wait_seconds)); then
		echo "error: timed out waiting for the Fgentic agent gate" >&2
		if [[ -r "${owner_file}" ]]; then
			echo "current owner: $(<"${owner_file}")" >&2
		fi
		exit 1
	fi
	if [[ "${waiting_reported}" == false ]]; then
		echo "Another worktree owns the final repository gates; waiting up to ${wait_seconds}s." >&2
		waiting_reported=true
	fi
	sleep 2
done

trap cleanup EXIT HUP INT TERM
printf '%s\t%s\t%s\t%s\n' "${host}" "$$" "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "${root_dir}" >"${owner_file}"

echo "Acquired the Fgentic agent gate; running check then test." >&2
"${mise_bin}" --cd "${root_dir}" run check
"${mise_bin}" --cd "${root_dir}" run test
