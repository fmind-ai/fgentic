#!/usr/bin/env bash
# Deterministic offline contract for cross-org break-glass containment (issue #350): containment drops the
# partner from the callback border and restore reverses it, the trust-registry gate stays green in both
# states, the containment record and evidence pack are content-free with the expected who/when/what fields
# in the offboarding-safe order, and the abuse-report schema accepts a reference-only report while rejecting
# content-bearing or unknown fields. No live cluster: triggering the lab and asserting every plane denies is
# the runtime-owner proof.
#
# Isolation: break-glass mutates its data tree IN PLACE (registry + rendered policy.json/platform-settings),
# so this test runs it against a SCRATCH MIRROR via FGENTIC_FED_TREE and never touches the committed files.
# That removes the race with check:fed-registry, which reads the same real files under the parallel `check`
# aggregate. Every assertion — including the FULL trust-registry gate — runs against the scratch copy.
set -euo pipefail

SCRIPT_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly SCRIPT_ROOT
# shellcheck source=scripts/lib.sh
source "${SCRIPT_ROOT}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-break-glass.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands yq jq python3 git

readonly PARTNER="org-b.fgentic.localhost"

# Build a scratch mirror of exactly the federation surface break-glass mutates and check:fed-registry audits,
# preserving the repo-relative paths so both scripts resolve their data under FGENTIC_FED_TREE. Scripts and
# libraries still resolve against the real repo; only the DATA tree is redirected here.
readonly TREE="${WORK_DIR}/tree"
mkdir -p "${TREE}/infra" "${TREE}/apps/synapse-federation-policy" "${TREE}/clusters" "${TREE}/scripts/lib"
cp -r "${SCRIPT_ROOT}/infra/federation" "${TREE}/infra/"
cp -r "${SCRIPT_ROOT}/apps/synapse-federation-policy/policy" "${TREE}/apps/synapse-federation-policy/"
cp -r "${SCRIPT_ROOT}/clusters/federation" "${TREE}/clusters/"
# check:fed-registry greps these library/seed scripts as trust-consistency DATA (not as executables).
cp "${SCRIPT_ROOT}/scripts/lib/federation-matrix.sh" "${SCRIPT_ROOT}/scripts/lib/federation-contract-signing.sh" "${TREE}/scripts/lib/"
cp "${SCRIPT_ROOT}/scripts/seed-federation.sh" "${TREE}/scripts/"

export FGENTIC_FED_TREE="${TREE}"
export FGENTIC_FED_RECORD_DIR="${WORK_DIR}/records"

readonly REGISTRY="${TREE}/infra/federation/registry/partners.yaml"
readonly POLICY="${TREE}/apps/synapse-federation-policy/policy/policy.json"
# Snapshot the scratch registry so the "only the contained: line changed" assertion has a baseline.
cp "${REGISTRY}" "${WORK_DIR}/registry.orig"

# The scratch tree must be self-consistent before we touch it (the gate is green against the committed copy).
bash "${SCRIPT_ROOT}/scripts/check-fed-registry.sh" >/dev/null 2>&1 \
	|| fail "precondition: the scratch mirror must pass the trust-registry gate before break-glass"
# The partner starts admitted at the border.
jq -e --arg p "${PARTNER}" '.allowed_servers | index($p) != null' "${POLICY}" >/dev/null \
	|| fail "precondition: ${PARTNER} must be admitted before break-glass"

# 1. Contain: border drops the partner; registry flag is a byte-safe single-line change; gate still green.
bash "${SCRIPT_ROOT}/scripts/fed-break-glass.sh" contain "${PARTNER}" >/dev/null 2>&1
jq -e --arg p "${PARTNER}" '.allowed_servers | index($p) == null' "${POLICY}" >/dev/null \
	|| fail "contain: ${PARTNER} must be dropped from policy.json allowed_servers"
grep -v 'contained:' "${WORK_DIR}/registry.orig" >"${WORK_DIR}/orig.nc" || true
grep -v 'contained:' "${REGISTRY}" >"${WORK_DIR}/new.nc" || true
diff "${WORK_DIR}/orig.nc" "${WORK_DIR}/new.nc" >/dev/null \
	|| fail "contain: only the 'contained:' line may change in the registry"
yq -e '.partners[] | select(.server_name == "'"${PARTNER}"'") | .contained == true' "${REGISTRY}" >/dev/null \
	|| fail "contain: registry contained flag must be true"
bash "${SCRIPT_ROOT}/scripts/check-fed-registry.sh" >/dev/null 2>&1 || fail "contain: trust-registry gate must stay green in the contained state"

# 2. The containment record is content-free with who/when/what in the exact offboarding-safe order.
record="${FGENTIC_FED_RECORD_DIR}/containment-${PARTNER}.json"
[ -f "${record}" ] || fail "contain: no containment record written"
jq -e --arg p "${PARTNER}" '
	.schema == "fgentic.federation.containment.v1" and .action == "contain" and
	.partner.server_name == $p and (.partner.azp == "org-b-a2a") and
	(.git_revision | test("^[0-9a-f]{40}$")) and
	(.recorded_at | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$")) and
	([.safe_order[].plane] == ["a2a-quota", "bridge-sender", "callback-border", "acl-and-whitelist"]) and
	([.safe_order[].order] == [1, 2, 3, 4]) and
	(.safe_order[] | select(.plane == "callback-border") | .rendered == true)
' "${record}" >/dev/null || fail "containment record missing expected content-free who/when/what fields"
# No content-bearing keys anywhere in the record (message bodies, prompts, event content).
if jq -e '.. | strings | select(test("(?i)(message|prompt|body|content|plaintext)"))' "${record}" >/dev/null 2>&1; then
	fail "containment record must not carry any content-bearing string"
fi

# 3. Evidence pack: content-free, verify-only identity, every named source flagged content_free.
pack="${WORK_DIR}/pack.json"
bash "${SCRIPT_ROOT}/scripts/fed-evidence-pack.sh" "${PARTNER}" >"${pack}" 2>/dev/null
jq -e --arg p "${PARTNER}" '
	.schema == "fgentic.federation.evidence.v1" and
	.partner.server_name == $p and .partner.azp == "org-b-a2a" and
	.partner.issuer == "https://id.org-b.fgentic.localhost/realms/fgentic-federation" and
	(.partner.review_by | test("^[0-9]{4}-[0-9]{2}-[0-9]{2}$")) and
	.containment.action == "contain" and
	([.evidence_sources[].content_free] | all) and
	([.evidence_sources[].stream] | sort == ["agentgateway-a2a-authz", "bridge-delegation-audit", "synapse-federation-policy-denial"]) and
	(.honest_limits | length >= 2)
' "${pack}" >/dev/null || fail "evidence pack missing content-free identity/source/limit fields"
# The pack must not embed any private key material.
if jq -e '.. | strings | select(test("PRIVATE KEY|BEGIN EC|secret"))' "${pack}" >/dev/null 2>&1; then
	fail "evidence pack must never carry private key or secret material"
fi

# 4. Restore: border re-admits the partner; gate green again.
bash "${SCRIPT_ROOT}/scripts/fed-break-glass.sh" restore "${PARTNER}" >/dev/null 2>&1
jq -e --arg p "${PARTNER}" '.allowed_servers | index($p) != null' "${POLICY}" >/dev/null \
	|| fail "restore: ${PARTNER} must be re-admitted to the border"
bash "${SCRIPT_ROOT}/scripts/check-fed-registry.sh" >/dev/null 2>&1 || fail "restore: trust-registry gate must stay green"
# Restore is a full reversal: the scratch tree is byte-identical to its pre-break-glass state.
diff "${WORK_DIR}/registry.orig" "${REGISTRY}" >/dev/null \
	|| fail "restore: the registry must return byte-for-byte to its pre-containment state"

# 5. Abuse-report schema: a reference-only report validates; content-bearing / unknown fields are rejected.
schema="${SCRIPT_ROOT}/infra/federation/registry/abuse-report.schema.json"
[ -f "${schema}" ] || fail "abuse-report schema missing"
jq -e '.["$id"] and (.required | index("subject_server_name")) and (.additionalProperties == false)' "${schema}" >/dev/null \
	|| fail "abuse-report schema is malformed"
# Structural validation mirroring check:mcp-governance (jq -e): required keys present, no unknown keys,
# every evidence ref is a reference kind (no message/body/content kind), enums respected. Applied to one
# valid report (must pass) and two malformed reports (must FAIL), so the rule genuinely discriminates.
abuse_valid='
	(["category", "reported_at", "reporter_server_name", "requested_action", "schema", "severity", "subject_server_name"]
		- (keys - ["contact", "evidence_refs"]) | length == 0) and
	((keys - ["category", "contact", "evidence_refs", "reported_at", "reporter_server_name", "requested_action", "schema", "severity", "subject_server_name"]) | length == 0) and
	(.schema == "fgentic.federation.abuse-report.v1") and
	(.category | . == "spam" or . == "harassment" or . == "policy-violation" or . == "credential-compromise" or . == "data-exfiltration" or . == "other") and
	(.requested_action | . == "investigate" or . == "rate-limit" or . == "contain") and
	([.evidence_refs[].kind] | all(. == "matrix_event_id" or . == "policy_digest" or . == "key_id" or . == "a2a_task_id" or . == "azp"))
'
valid_report="$(jq -n '{
	schema: "fgentic.federation.abuse-report.v1",
	reporter_server_name: "org-b.fgentic.localhost",
	subject_server_name: "org-a.fgentic.localhost",
	category: "policy-violation",
	severity: "high",
	requested_action: "contain",
	evidence_refs: [{kind: "matrix_event_id", value: "$abc"}, {kind: "policy_digest", value: "sha256:deadbeef"}],
	reported_at: "2026-07-21T00:00:00Z"
}')"
echo "${valid_report}" | jq -e "${abuse_valid}" >/dev/null || fail "a reference-only abuse report must validate"
# Reject 1: a content-bearing evidence kind (message content is never a valid reference).
content_report="$(echo "${valid_report}" | jq '.evidence_refs += [{kind: "message_body", value: "leaked prompt text"}]')"
if echo "${content_report}" | jq -e "${abuse_valid}" >/dev/null 2>&1; then
	fail "abuse report with a content-bearing evidence kind must be rejected"
fi
# Reject 2: an unknown top-level field (additionalProperties:false).
rogue_report="$(echo "${valid_report}" | jq '.rogue_field = "x"')"
if echo "${rogue_report}" | jq -e "${abuse_valid}" >/dev/null 2>&1; then
	fail "abuse report with an unknown field must be rejected"
fi

echo "Cross-org break-glass containment contract passed: border drop/restore, content-free record + evidence pack, abuse intake"
