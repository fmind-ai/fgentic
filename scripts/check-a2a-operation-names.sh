#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly repo_root

# ADR 0004 records the operation names used when that historical decision was accepted. Current
# code, fixtures, runbooks, and specifications must use the canonical A2A v1 method names.
legacy_pattern='message[/]send|message[/]stream|tasks[/]get|tasks[/]cancel'
set +e
matches="$(
	git -C "${repo_root}" grep -n -E "${legacy_pattern}" -- . \
		':(exclude)docs/adr/0004-a2a-delegation.md'
)"
status=$?
set -e

if ((status == 0)); then
	echo "error: legacy A2A operation names remain outside historical ADR 0004:" >&2
	echo "${matches}" >&2
	exit 1
fi
if ((status != 1)); then
	echo "error: failed to inventory A2A operation names" >&2
	exit "${status}"
fi

echo "A2A operation-name inventory passed"
