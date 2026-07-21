#!/usr/bin/env bash
# Fail-closed gate for the partner trust registry (issue #349): the registry is schema-valid, it renders
# deterministically (no hand-edits), every server_name is a FQDN (never a localpart — D6), and each of the
# five cross-org enforcement planes is byte-consistent with it — a partner admitted on one plane but absent
# from another, an unknown field, a duplicate/empty allowlist, or the denied control server leaking into any
# plane all fail the build. No live cluster: this is a static, deterministic audit of the git tree.
# shellcheck disable=SC2016 # the Flux ${...} substitution placeholders are matched as intentional literals
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-fed-registry.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands yq jq python3 diff

readonly REGISTRY="${ROOT_DIR}/infra/federation/registry/partners.yaml"
[ -f "${REGISTRY}" ] || fail "registry not found: ${REGISTRY}"
readonly REGISTRY_JSON="${WORK_DIR}/registry.json"
yq -o=json '.' "${REGISTRY}" >"${REGISTRY_JSON}"

# 1. Structural schema validation (mirrors check:mcp-governance — jq -e, no external jsonschema tool).
jq -e '
  (.["$schema"] == "./partners.schema.json") and
  (.version == 1) and
  (.lab | (keys | sort) == ["a2a_max_budget_units", "a2a_quota_units_per_minute"]) and
  (.lab.a2a_max_budget_units | type == "number" and . >= 1) and
  (.lab.a2a_quota_units_per_minute | type == "number" and . >= 1) and
  ((keys | sort) == ["$schema", "lab", "partners", "version"]) and
  (.partners | type == "array" and length >= 1)
' "${REGISTRY_JSON}" >/dev/null || fail "registry top-level schema invalid"

# Every partner: exact required fields, enum roles/classifications, and (D6) a FQDN server_name — never a
# localpart (no '@', at least one dot). A denied partner must be allowlisted:false / classification:none.
jq -e '
  def fqdn: test("^(?=.{1,255}$)([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)(\\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)+(:[0-9]{1,5})?$");
  [.partners[] |
    (["allowlisted", "classification", "role", "server_name"] - (keys - ["a2a"]) | length == 0) and
    ((keys - ["a2a", "allowlisted", "classification", "role", "server_name"]) | length == 0) and
    (.server_name | type == "string" and (contains("@") | not) and fqdn) and
    (.role | . == "host" or . == "admitted" or . == "denied") and
    (.allowlisted | type == "boolean") and
    (.classification | . == "none" or . == "public" or . == "internal" or . == "confidential") and
    (if .role == "denied" then (.allowlisted == false and .classification == "none") else true end)
  ] | all
' "${REGISTRY_JSON}" >/dev/null || fail "a partner entry violates the registry schema (fields, role/classification enum, or D6 FQDN)"

# Exactly one host, at least one admitted, exactly one denied; server_names unique; allowlist non-empty.
jq -e '
  ([.partners[] | select(.role == "host")] | length == 1) and
  ([.partners[] | select(.role == "admitted")] | length >= 1) and
  ([.partners[] | select(.role == "denied")] | length == 1) and
  (([.partners[].server_name] | length) == ([.partners[].server_name] | unique | length)) and
  ([.partners[] | select(.allowlisted == true)] | length >= 1)
' "${REGISTRY_JSON}" >/dev/null || fail "registry must have one host, ≥1 admitted, one denied, unique names, and a non-empty allowlist"

# The host exports the AgentCard/route; every admitted partner is an authorized A2A consumer. The a2a
# object is closed per role: EXACTLY its role's key set (no unknown fields — additionalProperties:false)
# with the schema's path/URI shape, so the committed partners.schema.json and this gate cannot diverge.
jq -e '
  ([.partners[] | select(.role == "host") | .a2a.role] == ["exporter"]) and
  ([.partners[] | select(.role == "admitted")] | all(.a2a.role == "consumer")) and
  ([.partners[] | select(.a2a.role == "exporter") | .a2a] | all(
    (keys | sort) == ["agent_path", "card_key_id", "provider_organization", "role", "route_host", "usage_receipt_key_id"]
    and (.agent_path | startswith("/")) and (.provider_organization | length >= 1)
    and (.card_key_id | length >= 1) and (.usage_receipt_key_id | length >= 1) and (.route_host | length >= 1)
  )) and
  ([.partners[] | select(.a2a.role == "consumer") | .a2a] | all(
    (keys | sort) == ["audience", "azp", "issuer", "jwks_path", "role"]
    and (.azp | length >= 1) and (.audience | length >= 1)
    and (.issuer | startswith("https://")) and (.jwks_path | startswith("/"))
  )) and
  ([.partners[] | select(.role == "denied") | has("a2a")] | all(. == false))
' "${REGISTRY_JSON}" >/dev/null || fail "an a2a block has an unknown field, a missing required field, or the wrong role/shape"

# Pull the canonical identities the planes are checked against.
host_name="$(jq -r '.partners[] | select(.role == "host") | .server_name' "${REGISTRY_JSON}")"
admitted_name="$(jq -r '.partners[] | select(.role == "admitted") | .server_name' "${REGISTRY_JSON}")"
denied_name="$(jq -r '.partners[] | select(.role == "denied") | .server_name' "${REGISTRY_JSON}")"
azp="$(jq -r '.partners[] | select(.role == "admitted") | .a2a.azp' "${REGISTRY_JSON}")"
issuer="$(jq -r '.partners[] | select(.role == "admitted") | .a2a.issuer' "${REGISTRY_JSON}")"
jwks_path="$(jq -r '.partners[] | select(.role == "admitted") | .a2a.jwks_path' "${REGISTRY_JSON}")"
audience="$(jq -r '.partners[] | select(.role == "admitted") | .a2a.audience' "${REGISTRY_JSON}")"
route_host="$(jq -r '.partners[] | select(.role == "host") | .a2a.route_host' "${REGISTRY_JSON}")"
agent_path="$(jq -r '.partners[] | select(.role == "host") | .a2a.agent_path' "${REGISTRY_JSON}")"
provider_org="$(jq -r '.partners[] | select(.role == "host") | .a2a.provider_organization' "${REGISTRY_JSON}")"
card_key_id="$(jq -r '.partners[] | select(.role == "host") | .a2a.card_key_id' "${REGISTRY_JSON}")"
receipt_key_id="$(jq -r '.partners[] | select(.role == "host") | .a2a.usage_receipt_key_id' "${REGISTRY_JSON}")"
max_budget="$(jq -r '.lab.a2a_max_budget_units' "${REGISTRY_JSON}")"
quota_per_minute="$(jq -r '.lab.a2a_quota_units_per_minute' "${REGISTRY_JSON}")"
# The admitted issuer host must equal the admitted server_name under id. (D6-safe cross-plane binding).
[ "${issuer}" = "https://id.${admitted_name}/realms/fgentic-federation" ] \
	|| fail "registry admitted issuer '${issuer}' does not resolve to the admitted server_name"

# 2. Deterministic render: re-render into a scratch mirror and require byte-identical committed artifacts.
bash "${ROOT_DIR}/scripts/fed-registry-render.sh" --out-root "${WORK_DIR}/render" >/dev/null
for rel in apps/synapse-federation-policy/policy/policy.json clusters/federation/platform-settings.yaml; do
	diff -u "${ROOT_DIR}/${rel}" "${WORK_DIR}/render/${rel}" >/dev/null \
		|| fail "${rel} drifted from the registry — run 'mise run fed:registry-render' (no hand-edits)"
done

# 3. Plane 1 — Synapse federation_domain_whitelist. platform-settings is registry-rendered (step 2), so
#    the whitelist entries are substitution vars; assert each homeserver's list is EXACTLY the expected
#    registry-derived set (not merely present/absent) so an extra literal domain cannot broaden trust
#    undetected. A and B trust {host, admitted}; the denied control server C trusts all three.
extract_whitelist() {
	# Emit the '- <entry>' items directly under federation_domain_whitelist: (stop at the next key),
	# sorted, one per line — the entries are Flux vars like ${server_name} inside the embedded config.
	awk '
		/federation_domain_whitelist:/ { collecting = 1; next }
		collecting && $1 == "-" { print $2; next }
		collecting { collecting = 0 }
	' "$@" | LC_ALL=C sort
}
expected_ab="$(printf '%s\n' '${federation_partner_server_name}' '${server_name}' | LC_ALL=C sort)"
expected_c="$(printf '%s\n' '${federation_denied_server_name}' '${federation_partner_server_name}' '${server_name}' | LC_ALL=C sort)"
for env in a b; do
	actual="$(extract_whitelist "${ROOT_DIR}/infra/federation/matrix-${env}/"*.yaml)"
	[ -n "${actual}" ] || fail "matrix-${env}: federation_domain_whitelist not found"
	[ "${actual}" = "${expected_ab}" ] \
		|| fail "matrix-${env} whitelist is not exactly {host, admitted} — an unregistered domain would broaden trust"
done
actual_c="$(extract_whitelist "${ROOT_DIR}/infra/federation/matrix-c/helmrelease.yaml")"
[ "${actual_c}" = "${expected_c}" ] || fail "matrix-c whitelist is not exactly {host, admitted, denied}"

# 4. Plane 2 — room m.room.server_acl seed. allow == the admitted set (host + admitted), deny empty, and
#    the seed's SERVER_A/B/C literals equal the registry host/admitted/denied.
seed_acl="${ROOT_DIR}/scripts/lib/federation-matrix.sh"
grep -Fq 'allow: [$a, $b], deny: [], allow_ip_literals: false' "${seed_acl}" \
	|| fail "server_acl seed must be allow:[host,admitted] deny:[] allow_ip_literals:false"
seed_env="${ROOT_DIR}/scripts/seed-federation.sh"
grep -Fxq "readonly SERVER_A=\"${host_name}\"" "${seed_env}" || fail "seed SERVER_A must equal the registry host"
grep -Fxq "readonly SERVER_B=\"${admitted_name}\"" "${seed_env}" || fail "seed SERVER_B must equal the registry admitted partner"
grep -Fxq "readonly SERVER_C=\"${denied_name}\"" "${seed_env}" || fail "seed SERVER_C must equal the registry denied server"

# 5. Plane 3 — callback policy.json. allowed_servers == the sorted allowlisted set (also rendered, step 2).
policy_json="${ROOT_DIR}/apps/synapse-federation-policy/policy/policy.json"
expected_allowed="$(jq -c '[.partners[] | select(.allowlisted == true) | .server_name] | sort' "${REGISTRY_JSON}")"
jq -e --argjson want "${expected_allowed}" '.allowed_servers == $want' "${policy_json}" >/dev/null \
	|| fail "policy.json allowed_servers != the registry allowlist"

# 6. Plane 4 — pinned AgentCard identity. Provider org, exported route host+path, and the admitted issuer
#    (via the substitution var) all bind to the registry; the signer's default key IDs match.
card="${ROOT_DIR}/infra/federation/delegation/agent-card.json"
jq -e --arg org "${provider_org}" --arg url "https://a2a.\${server_name}${agent_path}" '
  .provider.organization == $org and (.supportedInterfaces[0].url == $url)
' "${card}" >/dev/null || fail "agent-card provider org / exported route does not match the registry"
[ "${route_host}" = "a2a.${host_name}" ] || fail "registry route_host must be a2a.<host_name>"
grep -Fq 'openIdConnectUrl": "https://id.${federation_partner_server_name}/realms/fgentic-federation/.well-known/openid-configuration"' "${card}" \
	|| fail "agent-card orgBOIDC issuer does not bind to the admitted partner"
signing="${ROOT_DIR}/scripts/lib/federation-contract-signing.sh"
grep -Fq "${card_key_id}" "${signing}" || fail "signer default AgentCard key id != registry card_key_id"
grep -Fq "${receipt_key_id}" "${signing}" || fail "signer default usage-receipt key id != registry usage_receipt_key_id"

# 7. Plane 5 — A2A azp / issuer / quota descriptors across policies.yaml, rate-limit.yaml, usage-receipt.yaml,
#    and the Keycloak client. The azp is the literal partner client id; issuer/jwks/audience/budget bind too.
policies="${ROOT_DIR}/infra/federation/delegation/policies.yaml"
# The manifest templates the issuer host with ${federation_partner_server_name}; resolve the registry
# issuer back to that templated form so the comparison is byte-exact against the committed manifest.
templated_issuer="${issuer/${admitted_name}/\$\{federation_partner_server_name\}}"
grep -Fq "jwt.azp == \"${azp}\"" "${policies}" || fail "policies.yaml azp != registry azp"
grep -Fq "issuer: ${templated_issuer}" "${policies}" || fail "policies.yaml issuer does not bind to the admitted partner"
grep -Fq "jwksPath: ${jwks_path}" "${policies}" || fail "policies.yaml jwksPath != registry jwks_path"
grep -Fq -- "- ${audience}" "${policies}" || fail "policies.yaml audience != registry audience"
grep -Fq 'budget.maxTokens <= ${federation_a2a_max_budget_units}' "${policies}" \
	|| fail "policies.yaml maxTokens ceiling must use the rendered budget var"
grep -Fq 'requests_per_unit: ${federation_a2a_quota_budget_units_per_minute}' "${ROOT_DIR}/infra/federation/delegation/rate-limit.yaml" \
	|| fail "rate-limit.yaml quota must use the rendered quota var"
grep -Fq -- "--azp=${azp}" "${ROOT_DIR}/infra/federation/delegation/usage-receipt.yaml" \
	|| fail "usage-receipt azp != registry azp"
grep -Fq "\"clientId\": \"${azp}\"" "${ROOT_DIR}/infra/federation/delegation/keycloak/kustomization.yaml" \
	|| fail "keycloak client id != registry azp"
: "${max_budget}" "${quota_per_minute}" # rendered into platform-settings (step 2), enforced above

# 8. Deny-by-default survives rendering: the denied control server (and any denied name) must appear in NONE
#    of the derived enforcement planes — the delegation dir and the callback policy.
for plane in "${ROOT_DIR}/infra/federation/delegation" "${policy_json}"; do
	if grep -rFq -- "${denied_name}" "${plane}" 2>/dev/null; then
		fail "denied server '${denied_name}' leaked into an enforcement plane: ${plane}"
	fi
done

echo "Partner trust registry contract passed: one validated source, five consistent planes, deny-by-default intact"
