#!/usr/bin/env bash
# Shared, side-effect-free helpers for executable repository scripts.

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
