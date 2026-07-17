#!/usr/bin/env bash
# Staged, credential-free public onboarding preflight for a prospective federation partner.
#
# Stage 1 (connectivity) reuses scripts/fed-check.sh: public Matrix .well-known delegation and the
# federation version endpoint. Stage 2 (opt-in A2A/AgentCard conformance) fetches the partner's
# public Signed AgentCard, verifies its ES256/JCS JWS under an out-of-band-pinned public JWK with
# the same verifier the bridge uses (scripts/sign-agent-card.sh -> agentcardjws.Verify), and checks
# the advertised A2A v1.0 JSONRPC interface plus the required token-budget extension.
#
# It emits machine-readable acceptance evidence and NEVER grants trust: eligible_for_registry_review
# is true only when both stages pass, and registry admission (#349) stays an explicit reviewed step.
# It bypasses no TLS verification, reads no credentials or .netrc, and never claims a reachable,
# well-formed card proves governance.
set -euo pipefail

readonly DEFAULT_TIMEOUT_SECONDS=10
readonly MAX_TIMEOUT_SECONDS=60
# Bound the card body at the bridge's maxAgentCardBytes so this preflight cannot accept a card the
# bridge would refuse to fetch.
readonly MAX_CARD_BYTES=1048576
readonly AGENT_CARD_PATH="/.well-known/agent-card.json"
# The cross-org cost-control contract (D7/D8): a partner card must advertise a required token-budget
# extension before any reservation-bounded invocation is possible.
readonly TOKEN_BUDGET_EXTENSION="https://fgentic.fmind.ai/a2a/extensions/token-budget/v1"

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR

usage() {
	cat <<'EOF'
usage: scripts/fed-onboard.sh [options] <partner-domain>

Staged public onboarding preflight for a prospective federation partner.

Stage 1 always runs public Matrix connectivity via scripts/fed-check.sh.
Stage 2 (A2A/AgentCard conformance) runs only when --a2a-url is given and requires --public-jwk.

Options:
  --expect-server <host:port>   Pin the partner's delegated Matrix server (passed to fed-check.sh).
  --a2a-url <https-url>         Exact partner A2A agent URL; enables the conformance stage.
  --public-jwk <path>          Out-of-band-pinned partner public P-256 JOSE JWK (required with
                               --a2a-url): kty=EC, crv=P-256, alg=ES256, use=sig, key_ops=[verify],
                               plus the exported kid; a bare-coordinate key is rejected.
  --key-id <kid>               Expected JWS key id; defaults to the "kid" field of --public-jwk.

Environment:
  FGENTIC_FED_CHECK_TIMEOUT     Per-request timeout in seconds (default: 10; max: 60).

Trust is earned by passing gates, not by running this script: a passing record is evidence that
gates a reviewed registry admission (#349), never an automatic grant.
EOF
}

fail() {
	echo "error: $*" >&2
	exit 1
}

# Verify subshell-less fetch that mirrors scripts/fed-check.sh: whole-request timeout, no redirects,
# no netrc, TLS verified, and a hard body-size ceiling.
fetch_json() {
	local label="$1"
	local url="$2"
	local output="$3"

	if ! (
		ulimit -f 2048
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
	((size <= MAX_CARD_BYTES)) || fail "${label} response exceeds ${MAX_CARD_BYTES} bytes"
	jq --exit-status 'type == "object"' "${output}" >/dev/null ||
		fail "${label} response is not a JSON object"
}

expected_server=''
partner_domain=''
a2a_url=''
public_jwk=''
key_id=''
while (($# > 0)); do
	case "$1" in
	--expect-server)
		(($# >= 2)) || fail '--expect-server requires a value'
		expected_server="$2"
		shift 2
		;;
	--a2a-url)
		(($# >= 2)) || fail '--a2a-url requires a value'
		a2a_url="$2"
		shift 2
		;;
	--public-jwk)
		(($# >= 2)) || fail '--public-jwk requires a value'
		public_jwk="$2"
		shift 2
		;;
	--key-id)
		(($# >= 2)) || fail '--key-id requires a value'
		key_id="$2"
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

readonly timeout_seconds="${FGENTIC_FED_CHECK_TIMEOUT:-${DEFAULT_TIMEOUT_SECONDS}}"
[[ "${timeout_seconds}" =~ ^[1-9][0-9]*$ ]] || fail 'FGENTIC_FED_CHECK_TIMEOUT must be a positive integer'
((timeout_seconds <= MAX_TIMEOUT_SECONDS)) ||
	fail "FGENTIC_FED_CHECK_TIMEOUT must not exceed ${MAX_TIMEOUT_SECONDS} seconds"

for command in jq timeout xh; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

# Conformance is opt-in but, once requested, its inputs are mandatory: an out-of-band JWK is the only
# thing that makes card verification meaningful.
if [ -n "${a2a_url}" ]; then
	[[ "${a2a_url}" == https://* ]] || fail '--a2a-url must be an https URL'
	[[ "${a2a_url}" != *' '* ]] || fail '--a2a-url must not contain whitespace'
	[ -n "${public_jwk}" ] || fail '--a2a-url requires --public-jwk (out-of-band pinned partner key)'
	[ -f "${public_jwk}" ] || fail "public JWK file not found: ${public_jwk}"
elif [ -n "${public_jwk}" ] || [ -n "${key_id}" ]; then
	fail '--public-jwk/--key-id require --a2a-url to enable the conformance stage'
fi

readonly work_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-onboard.XXXXXX")"
trap 'rm -rf "${work_dir}"' EXIT INT TERM

# Stage 1: public Matrix connectivity, delegated to the existing preflight so its validation and
# evidence schema stay the single source of truth.
connectivity_args=("${partner_domain}")
[ -z "${expected_server}" ] || connectivity_args=(--expect-server "${expected_server}" "${partner_domain}")
if ! FGENTIC_FED_CHECK_TIMEOUT="${timeout_seconds}" \
	"${ROOT_DIR}/scripts/fed-check.sh" "${connectivity_args[@]}" \
	>"${work_dir}/connectivity.json" 2>"${work_dir}/connectivity.err"; then
	cat "${work_dir}/connectivity.err" >&2
	fail 'connectivity stage failed; the partner is not publicly reachable as a Matrix federation server'
fi

# Stage 2: A2A/AgentCard conformance (opt-in). Absent --a2a-url, connectivity alone is never enough
# for registry review, so the record is emitted with eligible_for_registry_review=false.
if [ -z "${a2a_url}" ]; then
	jq --null-input \
		--arg checked_at "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" \
		--arg partner_domain "${partner_domain}" \
		--slurpfile connectivity "${work_dir}/connectivity.json" \
		'{
      checked_at: $checked_at,
      trust_level: "public_unauthenticated_probe",
      partner_domain: $partner_domain,
      connectivity: $connectivity[0],
      agentcard_conformance: null,
      governance_verified: false,
      eligible_for_registry_review: false
    }'
	exit 0
fi

jq --exit-status 'type == "object" and (.kid | type == "string" and length > 0)' \
	"${public_jwk}" >/dev/null || fail 'public JWK must be a JSON object with a non-empty "kid"'
if [ -z "${key_id}" ]; then
	key_id="$(jq --raw-output '.kid' "${public_jwk}")"
else
	pinned_kid="$(jq --raw-output '.kid' "${public_jwk}")"
	[ "${key_id}" = "${pinned_kid}" ] ||
		fail "expected --key-id ${key_id} does not match the pinned JWK kid ${pinned_kid}"
fi

readonly card_url="${a2a_url%/}${AGENT_CARD_PATH}"
fetch_json 'AgentCard discovery' "${card_url}" "${work_dir}/agent-card.json"

# Authenticity first: the same ES256/JCS verifier the bridge uses. Fails closed on unsigned,
# tampered, or wrong-kid cards.
if ! "${ROOT_DIR}/scripts/sign-agent-card.sh" verify \
	--input "${work_dir}/agent-card.json" \
	--public-key "${public_jwk}" \
	--key-id "${key_id}" >"${work_dir}/verify.out" 2>&1; then
	cat "${work_dir}/verify.out" >&2
	fail 'AgentCard signature verification failed; the card is unsigned, tampered, or signed by an unpinned key'
fi

# Conformance: the card must advertise the exact A2A v1.0 JSONRPC interface it is served at, and the
# required token-budget extension, or a partner could pass signing while offering an uninvocable or
# unbounded route.
jq --exit-status --arg url "${a2a_url}" '
  any(.supportedInterfaces[]?;
    .url == $url and .protocolBinding == "JSONRPC" and .protocolVersion == "1.0")
' "${work_dir}/agent-card.json" >/dev/null ||
	fail "AgentCard advertises no JSONRPC A2A v1.0 interface at ${a2a_url}"

jq --exit-status --arg extension "${TOKEN_BUDGET_EXTENSION}" '
  any(.capabilities.extensions[]?; .uri == $extension and .required == true)
' "${work_dir}/agent-card.json" >/dev/null ||
	fail "AgentCard omits the required token-budget extension ${TOKEN_BUDGET_EXTENSION}"

jq --null-input \
	--arg checked_at "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" \
	--arg partner_domain "${partner_domain}" \
	--arg card_url "${card_url}" \
	--arg interface_url "${a2a_url}" \
	--arg key_id "${key_id}" \
	--arg token_budget_extension "${TOKEN_BUDGET_EXTENSION}" \
	--slurpfile connectivity "${work_dir}/connectivity.json" \
	'{
    checked_at: $checked_at,
    trust_level: "public_conformance_probe",
    partner_domain: $partner_domain,
    connectivity: $connectivity[0],
    agentcard_conformance: {
      card_url: $card_url,
      interface_url: $interface_url,
      protocol_binding: "JSONRPC",
      protocol_version: "1.0",
      key_id: $key_id,
      token_budget_extension: $token_budget_extension,
      signature_verified: true
    },
    governance_verified: false,
    eligible_for_registry_review: true
  }'
