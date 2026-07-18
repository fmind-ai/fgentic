#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-shellcheck.XXXXXX")"
readonly WORK_DIR
cleanup() {
	rm -rf -- "${WORK_DIR}"
}
trap cleanup EXIT

SCRIPT_LIST="${WORK_DIR}/scripts.list"
readonly SCRIPT_LIST

cd "${ROOT_DIR}"
rg --files scripts -g '*.sh' -0 | sort -z >"${SCRIPT_LIST}"
mapfile -d '' -t shell_scripts <"${SCRIPT_LIST}"
((${#shell_scripts[@]} > 0)) || {
	echo "error: no owned shell scripts found" >&2
	exit 1
}

shellcheck -x --rcfile=/dev/null --source-path=SCRIPTDIR \
	--enable=add-default-case,check-extra-masked-returns,deprecate-which,quote-safe-variables,require-variable-braces \
	--severity=style "${shell_scripts[@]}"

echo "ShellCheck passed for ${#shell_scripts[@]} owned scripts with no exclusions."
