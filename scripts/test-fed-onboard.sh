#!/usr/bin/env bash
# Deterministic, provider-free fixtures for scripts/fed-onboard.sh.
#
# The connectivity and card fetches are served by a fake xh (no network); the ES256/JCS signature
# check runs the real bridge verifier (scripts/sign-agent-card.sh -> agentcardjws.Verify) against a
# card signed here with a freshly generated P-256 key, so the acceptance semantics match the bridge.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-onboard-test.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

for command in jq rg openssl; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly ONBOARD="${ROOT_DIR}/scripts/fed-onboard.sh"
readonly SIGN="${ROOT_DIR}/scripts/sign-agent-card.sh"
readonly FIXTURE_CARD="${ROOT_DIR}/infra/federation/delegation/agent-card.json"
[ -x "${ONBOARD}" ] || fail 'scripts/fed-onboard.sh must exist and be executable'
[ -f "${FIXTURE_CARD}" ] || fail 'federation AgentCard fixture is missing'
bash -n "${ONBOARD}"

readonly A2A_URL='https://a2a.partner.example/api/a2a/kagent/docs-qa'
readonly KID_GOOD='agent-card-test-1'
readonly KID_OTHER='agent-card-test-2'
readonly TOKEN_BUDGET='https://fgentic.fmind.ai/a2a/extensions/token-budget/v1'

# Resolve the reused fixture into a self-contained, conformant unsigned card: real interface URL and
# dummy usage-receipt params so no injection markers remain.
resolve_card() {
	local interface_url="$1"
	local protocol_version="$2"
	local drop_token_budget="$3"
	jq \
		--arg url "${interface_url}" \
		--arg version "${protocol_version}" \
		--argjson drop "${drop_token_budget}" \
		--arg token_budget "${TOKEN_BUDGET}" '
    .supportedInterfaces = [{url: $url, protocolBinding: "JSONRPC", protocolVersion: $version}]
    | .capabilities.extensions = [
        .capabilities.extensions[]
        | if (.uri | endswith("/usage-receipt/v1"))
          then .params = {schema: "fgentic.usage-receipt.v1", keyId: "es256:test", publicJwk: {kty: "EC"}}
          else . end
      ]
    | if $drop
      then .capabilities.extensions |= map(select(.uri != $token_budget))
      else . end
  ' "${FIXTURE_CARD}"
}

# Sign a resolved card with a generated P-256 key; emit the signed card and its public JWK.
sign_card() {
	local resolved="$1" key_pem="$2" key_id="$3" signed_out="$4" jwk_out="$5"
	local bundle="${WORK_DIR}/bundle.json"
	openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${key_pem}" 2>/dev/null
	"${SIGN}" sign --input "${resolved}" --private-key "${key_pem}" \
		--key-id "${key_id}" --output "${bundle}" \
		|| fail "signing fixture card failed for ${key_id}"
	jq --exit-status '.agentCard | select(type == "object")' "${bundle}" >"${signed_out}" \
		|| fail 'signed bundle missing agentCard'
	jq --exit-status '.publicJwk | select(type == "object" and (has("d") | not))' \
		"${bundle}" >"${jwk_out}" || fail 'signed bundle missing public JWK'
}

# Build the card variants once.
resolve_card "${A2A_URL}" '1.0' false >"${WORK_DIR}/resolved.json"
resolve_card "${A2A_URL}" '0.9' false >"${WORK_DIR}/resolved-old.json"
resolve_card "${A2A_URL}" '1.0' true >"${WORK_DIR}/resolved-no-budget.json"

sign_card "${WORK_DIR}/resolved.json" "${WORK_DIR}/key1.pem" "${KID_GOOD}" \
	"${WORK_DIR}/good.json" "${WORK_DIR}/jwk1.json"
sign_card "${WORK_DIR}/resolved-old.json" "${WORK_DIR}/key1b.pem" "${KID_GOOD}" \
	"${WORK_DIR}/old-interface.json" "${WORK_DIR}/jwk1b.json"
sign_card "${WORK_DIR}/resolved-no-budget.json" "${WORK_DIR}/key1c.pem" "${KID_GOOD}" \
	"${WORK_DIR}/no-budget.json" "${WORK_DIR}/jwk1c.json"
# A second, unrelated key with its own kid: pinning it against the good card proves rejection on
# the kid-mismatch short-circuit.
sign_card "${WORK_DIR}/resolved.json" "${WORK_DIR}/key2.pem" "${KID_OTHER}" \
	"${WORK_DIR}/good2.json" "${WORK_DIR}/jwk2.json"
# A third, unrelated key reusing the good card's kid: pinning it exercises the ECDSA signature
# rejection itself (matching kid, wrong public key), not just the kid short-circuit.
sign_card "${WORK_DIR}/resolved.json" "${WORK_DIR}/key3.pem" "${KID_GOOD}" \
	"${WORK_DIR}/good3.json" "${WORK_DIR}/jwk3.json"

# unsigned = the resolved card with no signatures; tampered = the good card with a mutated field.
cp "${WORK_DIR}/resolved.json" "${WORK_DIR}/unsigned.json"
jq '.description = "tampered after signing"' "${WORK_DIR}/good.json" >"${WORK_DIR}/tampered.json"

# Fake xh: serves Matrix discovery/version (for the fed-check connectivity stage) and the AgentCard
# from FED_ONBOARD_CARD_FILE. Fixtures inject connectivity/card transport failures. No network.
mkdir -p "${WORK_DIR}/bin"
cat >"${WORK_DIR}/bin/xh" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
url=''
for argument in "$@"; do
	case "${argument}" in https://*) url="${argument}" ;; esac
done
case "${FED_ONBOARD_FIXTURE:-success}:${url}" in
connectivity-failure:*/.well-known/matrix/server) exit 1 ;;
*:*/.well-known/matrix/server) printf '{"m.server":"matrix.partner.example:443"}' ;;
*:*/_matrix/federation/v1/version) printf '{"server":{"name":"Synapse","version":"1.155.0"}}' ;;
card-failure:*"/.well-known/agent-card.json") exit 1 ;;
*:*"/.well-known/agent-card.json") cat "${FED_ONBOARD_CARD_FILE:?}" ;;
*) exit 2 ;;
esac
EOF
chmod +x "${WORK_DIR}/bin/xh"

run_onboard() {
	local fixture="$1" card_file="$2"
	shift 2
	# The fake xh always responds instantly; this timeout only has to survive scheduler starvation
	# under heavy aggregate host load without spuriously firing (a delayed stub previously surfaced
	# as a false "Matrix federation version request failed"). Generous but finite, well under the
	# 60s max; production defaults stay untouched (#789).
	PATH="${WORK_DIR}/bin:${PATH}" FED_ONBOARD_FIXTURE="${fixture}" \
		FED_ONBOARD_CARD_FILE="${card_file}" FGENTIC_FED_CHECK_TIMEOUT=30 \
		"${ONBOARD}" "$@"
}

expect_failure() {
	local label="$1" expected="$2" failure_output
	shift 2
	if "$@" >"${WORK_DIR}/failure.out" 2>"${WORK_DIR}/failure.err"; then
		fail "${label} unexpectedly passed"
	fi
	failure_output="$(<"${WORK_DIR}/failure.err")"
	rg --fixed-strings "${expected}" "${WORK_DIR}/failure.err" >/dev/null \
		|| fail "${label} omitted expected failure: ${expected} (got: ${failure_output})"
}

# Success: connectivity + conformance both pass -> eligible for a reviewed registry admission.
run_onboard success "${WORK_DIR}/good.json" \
	--expect-server matrix.partner.example:443 \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1.json" \
	partner.example >"${WORK_DIR}/success.json" || fail 'success run failed'
jq --exit-status --arg url "${A2A_URL}" --arg budget "${TOKEN_BUDGET}" '
  .trust_level == "public_conformance_probe" and
  .partner_domain == "partner.example" and
  .connectivity.delegated_server == "matrix.partner.example:443" and
  .connectivity.governance_verified == false and
  .agentcard_conformance.interface_url == $url and
  .agentcard_conformance.protocol_version == "1.0" and
  .agentcard_conformance.token_budget_extension == $budget and
  .agentcard_conformance.signature_verified == true and
  .governance_verified == false and
  .eligible_for_registry_review == true
' "${WORK_DIR}/success.json" >/dev/null || fail 'success evidence is incomplete or overclaims trust'

# Connectivity-only: opt-in conformance omitted -> never eligible, and it does not overclaim.
run_onboard success "${WORK_DIR}/good.json" \
	--expect-server matrix.partner.example:443 partner.example >"${WORK_DIR}/probe.json" \
	|| fail 'connectivity-only run failed'
jq --exit-status '
  .trust_level == "public_unauthenticated_probe" and
  .agentcard_conformance == null and
  .eligible_for_registry_review == false
' "${WORK_DIR}/probe.json" >/dev/null || fail 'connectivity-only record overclaims eligibility'

# Fail-closed cases.
expect_failure 'connectivity failure' 'connectivity stage failed' \
	run_onboard connectivity-failure "${WORK_DIR}/good.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1.json" partner.example
expect_failure 'card fetch failure' 'AgentCard discovery request failed' \
	run_onboard card-failure "${WORK_DIR}/good.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1.json" partner.example
expect_failure 'unsigned card' 'signature verification failed' \
	run_onboard success "${WORK_DIR}/unsigned.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1.json" partner.example
expect_failure 'tampered card' 'signature verification failed' \
	run_onboard success "${WORK_DIR}/tampered.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1.json" partner.example
expect_failure 'wrong pinned key (kid mismatch)' 'signature verification failed' \
	run_onboard success "${WORK_DIR}/good.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk2.json" partner.example
expect_failure 'wrong pinned key (same kid, ECDSA)' 'signature verification failed' \
	run_onboard success "${WORK_DIR}/good.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk3.json" partner.example
expect_failure 'wrong interface version' 'no JSONRPC A2A v1.0 interface' \
	run_onboard success "${WORK_DIR}/old-interface.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1b.json" partner.example
expect_failure 'missing token budget' 'omits the required token-budget extension' \
	run_onboard success "${WORK_DIR}/no-budget.json" \
	--a2a-url "${A2A_URL}" --public-jwk "${WORK_DIR}/jwk1c.json" partner.example
expect_failure 'conformance requires JWK' 'requires --public-jwk' \
	run_onboard success "${WORK_DIR}/good.json" --a2a-url "${A2A_URL}" partner.example
expect_failure 'orphan JWK without a2a-url' 'require --a2a-url' \
	run_onboard success "${WORK_DIR}/good.json" --public-jwk "${WORK_DIR}/jwk1.json" partner.example

echo 'Federation partner onboarding preflight fixtures passed; no network request was made.'
