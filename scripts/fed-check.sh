#!/usr/bin/env bash
# Read-only public preflight for a prospective Matrix federation partner.
set -euo pipefail

readonly DEFAULT_TIMEOUT_SECONDS=10
readonly MAX_TIMEOUT_SECONDS=60
readonly MAX_RESPONSE_BYTES=65536

usage() {
	cat <<'EOF'
usage: scripts/fed-check.sh [--expect-server <host:port>] <partner-domain>

Checks the partner's public Matrix server delegation and federation version endpoint.
This unauthenticated probe does not prove mutual allowlists, room policy, or governance.

Environment:
  FGENTIC_FED_CHECK_TIMEOUT  Per-request connection and total timeout in seconds (default: 10; max: 60).
EOF
}

fail() {
	echo "error: $*" >&2
	exit 1
}

validate_dns_name() {
	local value="$1"
	[ "${#value}" -le 253 ] || return 1
	[[ "${value}" =~ ^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$ ]]
}

validate_server_name() {
	local value="$1"
	local host="${value}"
	local port=''

	[[ "${value}" != *'/'* && "${value}" != *'@'* && "${value}" != *' '* ]] || return 1
	if [[ "${value}" == *:* ]]; then
		host="${value%:*}"
		port="${value##*:}"
		[[ "${host}" != *:* && "${port}" =~ ^[0-9]+$ ]] || return 1
		((${#port} <= 5)) || return 1
		((10#${port} >= 1 && 10#${port} <= 65535)) || return 1
	fi
	validate_dns_name "${host}"
}

fetch_json() {
	local label="$1"
	local url="$2"
	local output="$3"

	# A whole-request timeout bounds slow peers; the file limit bounds unexpectedly large bodies.
	if ! (
		ulimit -f 64
		timeout --foreground "${timeout_seconds}s" xh \
			--ignore-stdin \
			--ignore-netrc \
			--check-status \
			--no-follow \
			--timeout "${timeout_seconds}" \
			--body \
			--pretty none \
			"${url}" >"${output}"
	); then
		fail "${label} request failed: ${url}"
	fi

	local size
	size="$(wc -c <"${output}")"
	((size <= MAX_RESPONSE_BYTES)) || fail "${label} response exceeds ${MAX_RESPONSE_BYTES} bytes"
	jq --exit-status 'type == "object"' "${output}" >/dev/null ||
		fail "${label} response is not a JSON object"
}

expected_server=''
partner_domain=''
while (($# > 0)); do
	case "$1" in
	--expect-server)
		(($# >= 2)) || fail '--expect-server requires a value'
		expected_server="${2,,}"
		shift 2
		;;
	-h | --help)
		usage
		exit 0
		;;
	-*) fail "unknown option: $1" ;;
	*)
		[ -z "${partner_domain}" ] || fail 'exactly one partner domain is required'
		partner_domain="${1,,}"
		shift
		;;
	esac
done

[ -n "${partner_domain}" ] || {
	usage >&2
	exit 2
}
validate_dns_name "${partner_domain}" || fail "invalid partner DNS domain: ${partner_domain}"
if [ -n "${expected_server}" ]; then
	validate_server_name "${expected_server}" || fail "invalid expected Matrix server: ${expected_server}"
	[[ "${expected_server}" == *:* ]] || fail 'expected Matrix server must include an explicit port'
fi

readonly timeout_seconds="${FGENTIC_FED_CHECK_TIMEOUT:-${DEFAULT_TIMEOUT_SECONDS}}"
[[ "${timeout_seconds}" =~ ^[1-9][0-9]*$ ]] || fail 'FGENTIC_FED_CHECK_TIMEOUT must be a positive integer'
((timeout_seconds <= MAX_TIMEOUT_SECONDS)) ||
	fail "FGENTIC_FED_CHECK_TIMEOUT must not exceed ${MAX_TIMEOUT_SECONDS} seconds"

for command in jq timeout xh; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly work_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-check.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT INT TERM

readonly well_known_url="https://${partner_domain}/.well-known/matrix/server"
fetch_json 'Matrix server discovery' "${well_known_url}" "${work_dir}/well-known.json"
delegated_server="$(jq --raw-output --exit-status '."m.server" | select(type == "string" and length > 0)' \
	"${work_dir}/well-known.json")" || fail 'Matrix server discovery omits a non-empty m.server'
delegated_server="${delegated_server,,}"
validate_server_name "${delegated_server}" ||
	fail "Matrix server discovery returned an invalid DNS server name: ${delegated_server}"
[[ "${delegated_server}" == *:* ]] ||
	fail 'Matrix server discovery must include an explicit port; SRV-only delegation is outside this bounded preflight'

if [ -n "${expected_server}" ] && [ "${delegated_server}" != "${expected_server}" ]; then
	fail "Matrix server delegation mismatch: expected ${expected_server}, received ${delegated_server}"
fi

readonly version_url="https://${delegated_server}/_matrix/federation/v1/version"
fetch_json 'Matrix federation version' "${version_url}" "${work_dir}/version.json"
software_name="$(jq --raw-output --exit-status '.server.name | select(type == "string" and length > 0)' \
	"${work_dir}/version.json")" || fail 'Matrix federation version omits server.name'
software_version="$(jq --raw-output --exit-status '.server.version | select(type == "string" and length > 0)' \
	"${work_dir}/version.json")" || fail 'Matrix federation version omits server.version'

jq --null-input \
	--arg checked_at "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" \
	--arg partner_domain "${partner_domain}" \
	--arg delegated_server "${delegated_server}" \
	--arg software_name "${software_name}" \
	--arg software_version "${software_version}" \
	'{
    checked_at: $checked_at,
    trust_level: "public_unauthenticated_probe",
    partner_domain: $partner_domain,
    delegated_server: $delegated_server,
    federation_software: {name: $software_name, version: $software_version},
    governance_verified: false
  }'
