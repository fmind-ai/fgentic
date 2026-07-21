#!/usr/bin/env bash
# Deterministic offline contract for the signed bilateral agreements (issue #353): the real tree passes,
# and the gate fails closed on a tampered agreement, a missing signature, an ADR-0015-out-of-bound
# classification (validly signed), and a registry that diverges from the signed terms. Fixture agreements
# are signed at runtime with a throwaway key so the negative paths are proven in CI without the real
# out-of-band private key. No live cluster.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-agreement-test.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands openssl yq

readonly GATE="${ROOT_DIR}/scripts/check-fed-agreement.sh"
readonly REAL_AGREEMENT="${ROOT_DIR}/infra/federation/agreements/org-b.fgentic.localhost.yaml"
readonly REGISTRY="${ROOT_DIR}/infra/federation/registry/partners.yaml"

# A throwaway fixture signer: its public key verifies fixtures we sign here; its private key never leaves
# this run. This proves the gate rejects a VALIDLY-SIGNED but non-conformant agreement, not just a bad sig.
fixture_key="${WORK_DIR}/fixture.key"
fixture_pub="${WORK_DIR}/fixture.pub"
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out "${fixture_key}" 2>/dev/null
openssl pkey -in "${fixture_key}" -pubout -out "${fixture_pub}" 2>/dev/null

# Write a signed fixture agreement (yaml transformed by a yq expression) into its own dir, return the dir.
make_signed_fixture() {
	local dir="$1" yq_expr="$2"
	mkdir -p "${dir}"
	local agreement="${dir}/org-b.fgentic.localhost.yaml"
	yq "${yq_expr}" "${REAL_AGREEMENT}" >"${agreement}"
	openssl dgst -sha256 -sign "${fixture_key}" "${agreement}" | openssl base64 -A >"${agreement}.sig"
	printf '%s' "${dir}"
}

run_gate() { # dir pubkey -> exit code of the gate
	FGENTIC_AGREEMENTS_DIR="$1" FGENTIC_AGREEMENT_PUBKEY="$2" FGENTIC_AGREEMENT_REGISTRY="${REGISTRY}" \
		bash "${GATE}" >"${WORK_DIR}/gate.log" 2>&1
}

# 1. The real, committed tree passes.
bash "${GATE}" >/dev/null 2>&1 || fail "the committed agreements must pass the gate"

# 2. Tampering the agreement bytes without re-signing fails the signature check.
tamper="${WORK_DIR}/tamper"
mkdir -p "${tamper}"
cp "${REAL_AGREEMENT}" "${tamper}/org-b.fgentic.localhost.yaml"
cp "${REAL_AGREEMENT}.sig" "${tamper}/org-b.fgentic.localhost.yaml.sig"
yq -i '.a2a_max_budget_units = 9999' "${tamper}/org-b.fgentic.localhost.yaml"
if run_gate "${tamper}" "${ROOT_DIR}/infra/federation/agreements/signing.pub"; then
	fail "a tampered agreement must fail the signature check"
fi

# 3. A missing detached signature fails closed.
unsigned="${WORK_DIR}/unsigned"
mkdir -p "${unsigned}"
cp "${REAL_AGREEMENT}" "${unsigned}/org-b.fgentic.localhost.yaml"
if run_gate "${unsigned}" "${ROOT_DIR}/infra/federation/agreements/signing.pub"; then
	fail "an unsigned agreement must fail closed"
fi

# 4. A validly-signed agreement whose classification exceeds the ADR-0015 bound is rejected.
confidential="$(make_signed_fixture "${WORK_DIR}/confidential" '.allowed_classification = "confidential"')"
if run_gate "${confidential}" "${fixture_pub}"; then
	fail "a signed agreement with an out-of-bound classification must be rejected"
fi
grep -Fq "ADR-0015 bound" "${WORK_DIR}/gate.log" || fail "confidential rejection must cite the ADR-0015 bound"

# 5. A validly-signed agreement whose terms diverge from the registry is rejected.
diverged="$(make_signed_fixture "${WORK_DIR}/diverged" '.a2a_max_budget_units = 8192')"
if run_gate "${diverged}" "${fixture_pub}"; then
	fail "a signed agreement that diverges from the registry must be rejected"
fi
grep -Fq "diverge from the signed agreement" "${WORK_DIR}/gate.log" || fail "divergence rejection must cite the registry mismatch"

echo "Signed bilateral agreement contract passed: real tree verifies; tamper, unsigned, out-of-bound, and divergence all fail closed"
