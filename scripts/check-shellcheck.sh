#!/usr/bin/env bash

set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
INVENTORY_JSON="$(mktemp "${TMPDIR:-/tmp}/fgentic-shellcheck.XXXXXX.json")"
readonly INVENTORY_JSON
SCRIPT_LIST="$(mktemp "${TMPDIR:-/tmp}/fgentic-shell-scripts.XXXXXX.list")"
readonly SCRIPT_LIST

# Exact counts make both debt growth and burn-down explicit in review while #550 is active.
readonly EXPECTED_COUNTS_JSON='{
  "SC1003": 3,
  "SC1091": 5,
  "SC2016": 127,
  "SC2030": 21,
  "SC2031": 81,
  "SC2248": 4,
  "SC2249": 13,
  "SC2250": 2,
  "SC2312": 345,
  "SC2329": 64
}'

cleanup() {
	rm -f -- "${INVENTORY_JSON}" "${SCRIPT_LIST}"
}
trap cleanup EXIT

cd "${ROOT_DIR}"
rg --files scripts -g '*.sh' -0 | sort -z >"${SCRIPT_LIST}"
mapfile -d '' -t shell_scripts <"${SCRIPT_LIST}"
((${#shell_scripts[@]} > 0)) || {
	echo "error: no owned shell scripts found" >&2
	exit 1
}

shellcheck_status=0
if shellcheck -x --format=json1 "${shell_scripts[@]}" >"${INVENTORY_JSON}"; then
	:
else
	shellcheck_status=$?
fi
((shellcheck_status <= 1)) || {
	echo "error: ShellCheck failed with status ${shellcheck_status}" >&2
	exit "${shellcheck_status}"
}

if ! jq -e --argjson expected "${EXPECTED_COUNTS_JSON}" '
  (reduce .comments[] as $finding (
    {};
    .["SC" + ($finding.code | tostring)] += 1
  )) == $expected
' "${INVENTORY_JSON}" >/dev/null; then
	jq -r --argjson expected "${EXPECTED_COUNTS_JSON}" '
    (reduce .comments[] as $finding (
      {};
      .["SC" + ($finding.code | tostring)] += 1
    )) as $actual
    | (($expected | keys) + ($actual | keys) | unique)[] as $code
    | select(($actual[$code] // 0) != ($expected[$code] // 0))
    | "error: \($code) inventory is \($actual[$code] // 0); expected \($expected[$code] // 0)"
  ' "${INVENTORY_JSON}" >&2
	exit 1
fi

jq -r --argjson expected "${EXPECTED_COUNTS_JSON}" '
  "ShellCheck debt inventory matches: \(.comments | length) diagnostics across \($expected | length) allowlisted codes."
' "${INVENTORY_JSON}"
