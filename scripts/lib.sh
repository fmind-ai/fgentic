#!/usr/bin/env bash
# Shared, side-effect-free helpers for executable repository scripts.

die() {
	echo "error: $*" >&2
	exit 1
}

# `fail` is the test-harness spelling of `die` — the identical "print to stderr and exit 1"
# contract that ~18 scripts each redefined byte-for-byte. Kept as a thin alias so a script that
# needs a bespoke failure path (custom prefix, log dump) can still override `fail` locally (#318).
fail() {
	die "$@"
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1 (run 'mise install')"
}

# require_commands (plural) fails fast if any named prerequisite is missing. The ~7 test scripts
# that checked a batch of tools all carried this byte-identical body; it calls `fail` so a script
# with a bespoke `fail` still gets its own failure path (#318).
require_commands() {
	local command
	for command in "$@"; do
		command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
	done
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
