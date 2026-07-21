#!/usr/bin/env bash
# Fail-closed gate for the signed bilateral federation agreements (issue #353): each agreement's detached
# signature verifies over its exact committed bytes, its terms are schema-valid with an ADR-0015-bounded
# classification, and the trust registry's enforced reservation/quota/classification match the signed
# values (re-render is a no-op). A tampered or unsigned agreement, an out-of-bound classification, or a
# registry that diverges from the signed contract all fail the build. No live cluster: a static audit.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-agreement.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands yq jq openssl diff find

# Paths default to the real tree; test-fed-agreement.sh overrides them to exercise tampered/out-of-bound
# fixtures against the same gate logic.
readonly AGREEMENTS_DIR="${FGENTIC_AGREEMENTS_DIR:-${ROOT_DIR}/infra/federation/agreements}"
readonly PUBKEY="${FGENTIC_AGREEMENT_PUBKEY:-${AGREEMENTS_DIR}/signing.pub}"
readonly REGISTRY="${FGENTIC_AGREEMENT_REGISTRY:-${ROOT_DIR}/infra/federation/registry/partners.yaml}"
[ -f "${PUBKEY}" ] || fail "agreement signing public key missing: ${PUBKEY}"

agreements_raw="$(find "${AGREEMENTS_DIR}" -maxdepth 1 -type f -name '*.yaml' | sort)"
[ -n "${agreements_raw}" ] || fail "no signed agreements found in ${AGREEMENTS_DIR}"
mapfile -t agreements <<<"${agreements_raw}"

registry_json="${WORK_DIR}/registry.json"
yq -o=json '.' "${REGISTRY}" >"${registry_json}"

for agreement in "${agreements[@]}"; do
	name="$(basename "${agreement}")"
	sig="${agreement}.sig"
	[ -f "${sig}" ] || fail "${name}: missing detached signature ${name}.sig"

	# 1. The detached signature must verify over the agreement's EXACT bytes (tamper -> fail closed).
	openssl base64 -d -A -in "${sig}" -out "${WORK_DIR}/sig.bin" 2>/dev/null \
		|| fail "${name}: signature is not valid base64"
	openssl dgst -sha256 -verify "${PUBKEY}" -signature "${WORK_DIR}/sig.bin" "${agreement}" >/dev/null 2>&1 \
		|| fail "${name}: signature does not verify against signing.pub — the agreement was edited without re-signing"

	# 2. Schema + ADR-0015 classification bound (public | internal only; never restricted/regulated/secret).
	agreement_json="${WORK_DIR}/agreement.json"
	yq -o=json '.' "${agreement}" >"${agreement_json}"
	jq -e '
		def fqdn: test("^(?=.{1,255}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+(:[0-9]{1,5})?$");
		.schema == "fgentic.federation.agreement.v1" and
		(.partner_server_name | type == "string" and (contains("@") | not) and fqdn) and
		(.azp | type == "string" and length >= 1) and
		(.a2a_max_budget_units | type == "number" and . >= 1) and
		(.a2a_quota_units_per_minute | type == "number" and . >= 1) and
		(.allowed_classification | . == "public" or . == "internal") and
		(.residency.basis == "contractual") and
		(.residency.region | type == "string" and length >= 1) and
		((keys - ["a2a_max_budget_units", "a2a_quota_units_per_minute", "allowed_classification", "azp", "partner_server_name", "residency", "schema"]) | length == 0)
	' "${agreement_json}" >/dev/null \
		|| fail "${name}: agreement schema invalid, unknown field, or classification exceeds the ADR-0015 bound (public|internal)"

	# 3. The signed terms must match the registry (which renders the enforcement planes). The agreement is
	#    the sole source: editing the registry's reservation/quota/classification without re-signing fails.
	partner="$(jq -r '.partner_server_name' "${agreement_json}")"
	azp="$(jq -r '.azp' "${agreement_json}")"
	budget="$(jq -r '.a2a_max_budget_units' "${agreement_json}")"
	quota="$(jq -r '.a2a_quota_units_per_minute' "${agreement_json}")"
	classification="$(jq -r '.allowed_classification' "${agreement_json}")"
	jq -e --arg p "${partner}" --arg azp "${azp}" --arg cls "${classification}" \
		--argjson budget "${budget}" --argjson quota "${quota}" '
		([.partners[] | select(.server_name == $p)] | length == 1) and
		(.partners[] | select(.server_name == $p) | .role == "admitted" and .a2a.azp == $azp and .classification == $cls) and
		(.lab.a2a_max_budget_units == $budget) and
		(.lab.a2a_quota_units_per_minute == $quota)
	' "${registry_json}" >/dev/null \
		|| fail "${name}: the registry's reservation/quota/classification/azp diverge from the signed agreement"
done

# 4. Deterministic render: re-render the registry from the agreements and require a byte-identical result.
bash "${ROOT_DIR}/scripts/fed-agreement-render.sh" --out-root "${WORK_DIR}/render" >/dev/null
diff -u "${REGISTRY}" "${WORK_DIR}/render/infra/federation/registry/partners.yaml" >/dev/null \
	|| fail "the registry drifted from the signed agreements — run 'mise run fed:agreement-render' (no hand-edits)"

# 5. The reservation is admission accounting, never consumption (D7/D8), and residency is contractual —
#    both stated in the signed artifact so the honest framing travels with the contract.
for agreement in "${agreements[@]}"; do
	grep -Fqi "admission accounting" "${agreement}" \
		|| fail "$(basename "${agreement}"): must state the reservation is admission accounting, not consumption"
	grep -Fq "CONTRACTUAL control" "${agreement}" \
		|| fail "$(basename "${agreement}"): must state residency is a contractual control, not a technical guarantee"
done

echo "Signed bilateral agreement contract passed: signatures verify, terms ADR-0015-bounded, registry matches the contract"
