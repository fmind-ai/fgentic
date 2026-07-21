#!/usr/bin/env bash
# Render the derived federation enforcement planes from the partner trust registry (issue #349).
#
# The registry (infra/federation/registry/partners.yaml) is the single source of truth. This renderer
# regenerates the two authored artifacts that carry partner identity as literal, non-templated values:
#
#   * apps/synapse-federation-policy/policy/policy.json     — allowed_servers = the allowlisted partners
#   * clusters/federation/platform-settings.yaml            — the federation server-identity + budget keys
#
# Flux post-build substitution then propagates the platform-settings keys into every templated plane
# (Synapse whitelist, A2A routes, agentgateway policies, rate-limit, the signed AgentCard, Keycloak).
# The remaining pinned constants (the seed ACL literals, the org-B azp, the signing key IDs) are held
# consistent by check:fed-registry, which fails closed on any drift from the registry.
#
# Deterministic: re-rendering a clean tree is a no-op. `--out-root DIR` writes under DIR instead of the
# repo (check:fed-registry uses it to render into a scratch mirror and diff against the committed tree).
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

require_commands yq jq python3

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

readonly REGISTRY="${ROOT_DIR}/infra/federation/registry/partners.yaml"
[ -f "${REGISTRY}" ] || fail "registry not found: ${REGISTRY}"

# Exactly one host and one denied control server match the federation lab's single-partner platform
# settings; the registry schema permits more, but this renderer maps the lab's fixed variable slots.
host_name="$(yq -r '[.partners[] | select(.role == "host") | .server_name] | @csv' "${REGISTRY}")"
admitted_name="$(yq -r '[.partners[] | select(.role == "admitted") | .server_name] | @csv' "${REGISTRY}")"
denied_name="$(yq -r '[.partners[] | select(.role == "denied") | .server_name] | @csv' "${REGISTRY}")"
[ "${host_name}" != "" ] && [[ "${host_name}" != *,* ]] || fail "registry must define exactly one host partner"
[ "${admitted_name}" != "" ] && [[ "${admitted_name}" != *,* ]] || fail "registry must define exactly one admitted partner"
[ "${denied_name}" != "" ] && [[ "${denied_name}" != *,* ]] || fail "registry must define exactly one denied partner"
max_budget="$(yq -r '.lab.a2a_max_budget_units' "${REGISTRY}")"
quota_per_minute="$(yq -r '.lab.a2a_quota_units_per_minute' "${REGISTRY}")"

# 1. policy.json — allowed_servers is the sorted set of allowlisted, NON-contained partners; a break-glass
#    contained partner (issue #350) is dropped from the callback border here. The event-type allowlist and
#    invite rule are the callback policy's fixed contract, not partner trust, so they stay constant.
allowed_servers_json="$(yq -o=json -I=0 '[.partners[] | select(.allowlisted == true and (.contained // false | not)) | .server_name] | sort' "${REGISTRY}")"
policy_out="${OUT_ROOT}/apps/synapse-federation-policy/policy/policy.json"
mkdir -p "$(dirname "${policy_out}")"
jq -n --argjson allowed "${allowed_servers_json}" '{
  version: 1,
  allowed_servers: $allowed,
  allowed_event_types: [
    "m.room.create",
    "m.room.guest_access",
    "m.room.history_visibility",
    "m.room.join_rules",
    "m.room.member",
    "m.room.message",
    "m.room.name",
    "m.room.power_levels",
    "m.room.server_acl"
  ],
  invite_rule: "allow_from_allowed_servers"
}' >"${policy_out}"

# 2. platform-settings.yaml — rewrite only the five federation identity/budget scalar lines in place,
#    preserving every comment, key order, and the exact quoting style of the committed file.
settings_out="${OUT_ROOT}/clusters/federation/platform-settings.yaml"
mkdir -p "$(dirname "${settings_out}")"
python3 - "${ROOT_DIR}/clusters/federation/platform-settings.yaml" "${settings_out}" <<PY
import re, sys

src, dst = sys.argv[1], sys.argv[2]
# Unquoted server names, quoted numeric budgets — matching the committed platform-settings format.
replacements = {
    "server_name": "${host_name}",
    "federation_partner_server_name": "${admitted_name}",
    "federation_denied_server_name": "${denied_name}",
    "federation_a2a_max_budget_units": '"${max_budget}"',
    "federation_a2a_quota_budget_units_per_minute": '"${quota_per_minute}"',
}
seen = {key: False for key in replacements}
out = []
with open(src, encoding="utf-8") as handle:
    for line in handle:
        matched = re.match(r"^(\s*)([A-Za-z0-9_]+): (.*)\n$", line)
        if matched and matched.group(2) in replacements:
            indent, key = matched.group(1), matched.group(2)
            out.append(f"{indent}{key}: {replacements[key]}\n")
            seen[key] = True
        else:
            out.append(line)
missing = [key for key, ok in seen.items() if not ok]
if missing:
    sys.exit(f"platform-settings.yaml is missing federation keys: {', '.join(missing)}")
with open(dst, "w", encoding="utf-8") as handle:
    handle.writelines(out)
PY

echo "fed-registry: rendered policy.json + platform-settings federation keys from the trust registry"
