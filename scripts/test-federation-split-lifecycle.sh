#!/usr/bin/env bash
# Focused offline contracts for split-federation failure cleanup. No Docker daemon,
# Kubernetes API, credential, or network endpoint is accessed by this test.
set -euo pipefail

TEST_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly TEST_ROOT
readonly SPLIT="${TEST_ROOT}/scripts/federation-split.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-split-lifecycle-check.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

# main is guarded, so sourcing exposes only the lifecycle functions and constants.
# shellcheck source=scripts/federation-split.sh
source "${SPLIT}"

run_failure_fixture() {
	local cleanup_dir="$1"
	SPLIT_UP_WORK_DIR="${cleanup_dir}"
	trap 'cleanup_split_up "$?"' EXIT
	false
}

failure_dir="${WORK_DIR}/command-failure"
mkdir "${failure_dir}"
set +e
failure_output="$(
	exec 2>&1
	set -Eeuo pipefail
	run_failure_fixture "${failure_dir}"
)"
failure_status=$?
set -e
[ "${failure_status}" -eq 1 ] || fail "command failure status was not preserved"
[ ! -e "${failure_dir}" ] || fail "command failure left its scoped work directory"
[ "${failure_output}" = "Split federation did not complete; run fed:split:status, then fed:split:down for exact recovery." ] ||
	fail "command failure recovery guidance changed"

signal_dir="${WORK_DIR}/signal"
mkdir "${signal_dir}"
set +e
signal_output="$(
	exec 2>&1
	set -Eeuo pipefail
	# The EXIT trap consumes this global through cleanup_split_up.
	# shellcheck disable=SC2034
	SPLIT_UP_WORK_DIR="${signal_dir}"
	trap 'cleanup_split_up "$?"' EXIT
	trap 'exit 143' TERM
	kill -TERM "${BASHPID}"
)"
signal_status=$?
set -e
[ "${signal_status}" -eq 143 ] || fail "termination status was not preserved"
[ ! -e "${signal_dir}" ] || fail "termination left its scoped work directory"
[ "${signal_output}" = "Split federation did not complete; run fed:split:status, then fed:split:down for exact recovery." ] ||
	fail "termination recovery guidance changed"

echo "Split federation failure cleanup contracts passed."
