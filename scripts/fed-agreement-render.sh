#!/usr/bin/env bash
# Render the trust registry's machine-enforced terms from the signed bilateral agreements (issue #353).
#
# Each infra/federation/agreements/<partner>.yaml is the SIGNED SOURCE for that partner's admission
# reservation (D7), per-minute quota, and allowed data classification. This renderer syncs those signed
# values into the trust registry (#349) — the lab reservation/quota and the partner's classification —
# so fed-registry-render then propagates them to the enforcement planes. Enforcement therefore cannot
# diverge from the signed contract; check:fed-agreement verifies the signature and this consistency.
#
# Deterministic: re-rendering a clean tree is a no-op. `--out-root DIR` writes under DIR instead of the
# repo (check:fed-agreement renders into a scratch mirror and diffs against the committed registry).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

require_commands yq python3

OUT_ROOT="${ROOT_DIR}"
while [ "$#" -gt 0 ]; do
	case "$1" in
		--out-root)
			OUT_ROOT="${2:?"--out-root requires a directory"}"
			shift 2
			;;
		*) fail "unknown argument: $1" ;;
	esac
done
readonly OUT_ROOT

readonly AGREEMENTS_DIR="${ROOT_DIR}/infra/federation/agreements"
readonly REGISTRY_SRC="${ROOT_DIR}/infra/federation/registry/partners.yaml"
readonly REGISTRY_OUT="${OUT_ROOT}/infra/federation/registry/partners.yaml"

# The lab has exactly one bilateral agreement; its reservation/quota are the lab-wide values, and its
# allowed_classification is its partner's registry classification. (Per-partner reservations for N
# partners are future work; the agreement schema already carries them per partner.)
agreements_raw="$(find "${AGREEMENTS_DIR}" -maxdepth 1 -type f -name '*.yaml' | sort)"
[ -n "${agreements_raw}" ] || fail "no signed agreements found in ${AGREEMENTS_DIR}"
mapfile -t agreements <<<"${agreements_raw}"

# Collect per-partner classification/review_by/valid_until and the single lab reservation/quota.
declare -A partner_class partner_review partner_valid
max_budget=""
quota=""
for agreement in "${agreements[@]}"; do
	partner="$(yq -r '.partner_server_name' "${agreement}")"
	partner_class["${partner}"]="$(yq -r '.allowed_classification' "${agreement}")"
	partner_review["${partner}"]="$(yq -r '.review_by' "${agreement}")"
	partner_valid["${partner}"]="$(yq -r '.valid_until // ""' "${agreement}")"
	agreement_budget="$(yq -r '.a2a_max_budget_units' "${agreement}")"
	agreement_quota="$(yq -r '.a2a_quota_units_per_minute' "${agreement}")"
	if [ -z "${max_budget}" ]; then
		max_budget="${agreement_budget}"
		quota="${agreement_quota}"
	elif [ "${max_budget}" != "${agreement_budget}" ] || [ "${quota}" != "${agreement_quota}" ]; then
		fail "multiple agreements disagree on the lab reservation/quota; per-partner reservations are not yet modeled"
	fi
done

mkdir -p "$(dirname "${REGISTRY_OUT}")"
# Byte-safe: rewrite only the lab budget/quota scalars and each agreed partner's classification line,
# preserving every comment and the hand-authored formatting of the registry.
python3 - "${REGISTRY_SRC}" "${REGISTRY_OUT}" "${max_budget}" "${quota}" \
	"$(declare -p partner_class)" "$(declare -p partner_review)" "$(declare -p partner_valid)" <<'PY'
import re, sys

src, dst, max_budget, quota, class_decl, review_decl, valid_decl = sys.argv[1:8]

def parse_map(decl):
    return {m.group(1): m.group(2) for m in re.finditer(r"\[([^\]]+)\]=\"([^\"]*)\"", decl)}

partner_class = parse_map(class_decl)
partner_review = parse_map(review_decl)
partner_valid = parse_map(valid_decl)

with open(src, encoding="utf-8") as handle:
    lines = handle.readlines()

lab_keys = {"a2a_max_budget_units": max_budget, "a2a_quota_units_per_minute": quota}
# Per-partner scalar keys synced from the signed agreement (empty valid_until leaves the line untouched).
partner_keys = {"classification": partner_class, "review_by": partner_review, "valid_until": partner_valid}
current_partner = None
for index, line in enumerate(lines):
    partner_match = re.match(r"^  - server_name: (\S+)\s*$", line)
    if partner_match:
        current_partner = partner_match.group(1)
        continue
    lab_match = re.match(r"^(\s*)(a2a_max_budget_units|a2a_quota_units_per_minute): ", line)
    if lab_match:
        lines[index] = f"{lab_match.group(1)}{lab_match.group(2)}: {lab_keys[lab_match.group(2)]}\n"
        continue
    key_match = re.match(r"^(\s*)(classification|review_by|valid_until): ", line)
    if key_match:
        key = key_match.group(2)
        value = partner_keys[key].get(current_partner, "")
        if value != "":
            quote = '"' if key in ("review_by", "valid_until") else ""
            lines[index] = f"{key_match.group(1)}{key}: {quote}{value}{quote}\n"

with open(dst, "w", encoding="utf-8") as handle:
    handle.writelines(lines)
PY

echo "fed-agreement: rendered the registry reservation/quota and partner classification from the signed agreements"
