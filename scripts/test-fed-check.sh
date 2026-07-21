#!/usr/bin/env bash
# Deterministic offline fixtures for scripts/fed-check.sh.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-check-test.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

for command in jq rg; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly CHECK="${ROOT_DIR}/scripts/fed-check.sh"
[ -x "${CHECK}" ] || fail 'scripts/fed-check.sh must exist and be executable'
bash -n "${CHECK}"

mkdir -p "${WORK_DIR}/bin"
cat >"${WORK_DIR}/bin/xh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail

url=''
for argument in "$@"; do
	case "${argument}" in
	https://*) url="${argument}" ;;
	esac
done

case "${FED_CHECK_FIXTURE:-success}:${url}" in
request-failure:*/.well-known/matrix/server) exit 1 ;;
malformed:*/.well-known/matrix/server) printf '{not-json' ;;
non-https:*/.well-known/matrix/server) printf '{"m.server":"http://matrix.partner.example"}' ;;
missing-port:*/.well-known/matrix/server) printf '{"m.server":"matrix.partner.example"}' ;;
missing-server:*/.well-known/matrix/server) printf '{"irrelevant":true}' ;;
*:*/.well-known/matrix/server) printf '{"m.server":"matrix.partner.example:443"}' ;;
version-failure:*/_matrix/federation/v1/version) exit 1 ;;
missing-version:*/_matrix/federation/v1/version) printf '{"server":{"name":"Synapse"}}' ;;
*:*/_matrix/federation/v1/version) printf '{"server":{"name":"Synapse","version":"1.155.0"}}' ;;
*) exit 2 ;;
esac
EOF
chmod +x "${WORK_DIR}/bin/xh"

run_check() {
	local fixture="$1"
	shift
	PATH="${WORK_DIR}/bin:${PATH}" FED_CHECK_FIXTURE="${fixture}" \
		FGENTIC_FED_CHECK_TIMEOUT=2 "${CHECK}" "$@"
}

expect_failure() {
	local fixture="$1"
	local expected="$2"
	shift 2
	if run_check "${fixture}" "$@" >"${WORK_DIR}/failure.out" 2>"${WORK_DIR}/failure.err"; then
		fail "fixture ${fixture} unexpectedly passed"
	fi
	rg --fixed-strings "${expected}" "${WORK_DIR}/failure.err" >/dev/null \
		|| fail "fixture ${fixture} omitted expected failure: ${expected}"
}

run_check success --expect-server matrix.partner.example:443 partner.example >"${WORK_DIR}/success.json"
jq --exit-status '
  .trust_level == "public_unauthenticated_probe" and
  .partner_domain == "partner.example" and
  .delegated_server == "matrix.partner.example:443" and
  .federation_software == {"name": "Synapse", "version": "1.155.0"} and
  .governance_verified == false
' "${WORK_DIR}/success.json" >/dev/null || fail 'success evidence is incomplete or overclaims trust'

expect_failure request-failure 'Matrix server discovery request failed' partner.example
expect_failure malformed 'Matrix server discovery response is not a JSON object' partner.example
expect_failure non-https 'invalid DNS server name' partner.example
expect_failure missing-port 'must include an explicit port' partner.example
expect_failure missing-server 'omits a non-empty m.server' partner.example
expect_failure success 'Matrix server delegation mismatch' \
	--expect-server unexpected.partner.example:443 partner.example
expect_failure version-failure 'Matrix federation version request failed' partner.example
expect_failure missing-version 'Matrix federation version omits server.version' partner.example
expect_failure success 'invalid partner DNS domain' 'https://partner.example'

echo 'Federation partner public-preflight fixtures passed; no network request was made.'
