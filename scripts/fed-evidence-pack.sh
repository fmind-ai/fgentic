#!/usr/bin/env bash
# Assemble a content-free cross-org incident evidence pack (issue #350). Given a contained partner, it
# combines the break-glass containment record with that partner's verify-only identity facts from the
# trust registry (#349) and names the already-emitted content-free record streams a reviewer collects
# live — the Synapse callback denial records, agentgateway authz/429 decisions, and bridge attribution
# audit. The pack reconstructs who / when / what from server_name, azp, key IDs, event IDs, policy
# digests, and the git revision — NEVER message bodies, prompts, artifact content, or room content.
#
#   scripts/fed-evidence-pack.sh <partner_server_name>   # emit the pack JSON to stdout
#
# Offline and deterministic given a containment record. The live record streams are collected separately
# by scripts/audit-attribution.sh and the streams named in evidence_sources against the reconciled lab.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

require_commands yq jq

readonly REGISTRY="${ROOT_DIR}/infra/federation/registry/partners.yaml"
readonly RECORD_DIR="${FGENTIC_FED_RECORD_DIR:-${ROOT_DIR}/.agents/tmp}"

[ "$#" -eq 1 ] || fail "usage: scripts/fed-evidence-pack.sh <partner_server_name>"
partner="$1"
record="${RECORD_DIR}/containment-${partner}.json"
[ -f "${record}" ] || fail "no containment record for ${partner}; run 'fed:break-glass contain ${partner}' first"

# Verify-only identity facts from the registry (no secrets, no private keys — server_name/azp/issuer/kids).
partner_json="$(yq -o=json '.partners[] | select(.server_name == "'"${partner}"'")' "${REGISTRY}")"
[ -n "${partner_json}" ] && [ "${partner_json}" != "null" ] || fail "partner ${partner} not found in the registry"
identity="$(jq '{
	server_name,
	classification,
	azp: (.a2a.azp // null),
	issuer: (.a2a.issuer // null),
	card_key_id: (.a2a.card_key_id // null),
	usage_receipt_key_id: (.a2a.usage_receipt_key_id // null),
	review_by: (.review_by // null),
	valid_until: (.valid_until // null)
}' <<<"${partner_json}")"

# The pack names the content-free record streams and the exact fields a reviewer extracts from each —
# a schema reference, never their content. This is what makes the pack reconstruct who/when/what offline.
jq -n \
	--slurpfile containment "${record}" \
	--argjson identity "${identity}" '{
	schema: "fgentic.federation.evidence.v1",
	partner: $identity,
	containment: ($containment[0] | {action, recorded_at, git_revision, safe_order}),
	evidence_sources: [
		{
			stream: "synapse-federation-policy-denial",
			record: "fgentic_federation_policy_violation",
			fields: ["reason", "server", "sender", "event", "room", "type", "digest"],
			content_free: true
		},
		{
			stream: "agentgateway-a2a-authz",
			record: "authorization decision + 429 rate-limit",
			fields: ["azp", "decision", "method", "path", "http_status"],
			content_free: true
		},
		{
			stream: "bridge-delegation-audit",
			record: "fgentic.delegation.v1",
			fields: ["matrix_event_id", "sender_mxid", "room_id", "a2a_user_id", "rate_limit_verdict"],
			content_free: true
		}
	],
	honest_limits: [
		"ACL/whitelist removal does not retract already-replicated Matrix history (request redaction separately).",
		"X-User-Id is attribution, not authentication (D11); an invite-path denial record is diagnostic, not authenticated attribution."
	]
}'
