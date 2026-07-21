#!/usr/bin/env bash
# Cross-org break-glass containment (issue #350): revoke a compromised partner across the federation
# trust planes in the offboarding-safe order, GitOps-reversibly, and emit a content-free containment
# record. Containment is registry-native: flipping the partner's `contained` flag in the trust registry
# (#349) and re-rendering drops it from the callback border (policy.json allowed_servers) so every
# federated event from it is denied. The transport whitelist and per-room ACL are additionally revoked
# by the ordered runtime commands this prints for the operator to run against the live lab.
#
#   scripts/fed-break-glass.sh contain <partner_server_name>   # engage break-glass (contained := true)
#   scripts/fed-break-glass.sh restore <partner_server_name>   # reverse it (contained := false)
#   scripts/fed-break-glass.sh status                          # list currently contained partners
#
# This step is offline and GitOps-pure: it edits the registry, re-renders, and writes a record. It never
# touches a live cluster — commit the render and let Flux reconcile, then run the printed runtime steps.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

require_commands yq jq python3 git

readonly REGISTRY="${ROOT_DIR}/infra/federation/registry/partners.yaml"
readonly RECORD_DIR="${FGENTIC_FED_RECORD_DIR:-${ROOT_DIR}/.agents/tmp}"

usage() {
	cat >&2 <<'EOF'
usage: scripts/fed-break-glass.sh contain <partner_server_name>
       scripts/fed-break-glass.sh restore <partner_server_name>
       scripts/fed-break-glass.sh status
EOF
}

# Flip the `contained:` line inside exactly the target partner's block, preserving all comments and
# formatting (yq -i drops blank lines, so a scoped line edit keeps the hand-authored registry byte-clean).
set_contained() {
	local partner="$1" value="$2"
	python3 - "${REGISTRY}" "${partner}" "${value}" <<'PY'
import re, sys

path, partner, value = sys.argv[1], sys.argv[2], sys.argv[3]
with open(path, encoding="utf-8") as handle:
    lines = handle.readlines()

start = None
for index, line in enumerate(lines):
    if re.match(rf"^  - server_name: {re.escape(partner)}\s*$", line):
        start = index
        break
if start is None:
    sys.exit(f"partner not found in registry: {partner}")

# The block runs until the next partner list item or end of file.
end = len(lines)
for index in range(start + 1, len(lines)):
    if re.match(r"^  - server_name: ", lines[index]):
        end = index
        break

for index in range(start, end):
    matched = re.match(r"^(\s*)contained: (true|false)\s*$", lines[index])
    if matched:
        lines[index] = f"{matched.group(1)}contained: {value}\n"
        break
else:
    # No contained line yet (a partner authored without it): insert one right after `allowlisted:`,
    # matching that key's indentation, so break-glass works on any admitted partner.
    for index in range(start, end):
        allow = re.match(r"^(\s*)allowlisted: ", lines[index])
        if allow:
            lines.insert(index + 1, f"{allow.group(1)}contained: {value}\n")
            break
    else:
        sys.exit(f"partner {partner} has neither 'contained' nor 'allowlisted' to anchor containment")

with open(path, "w", encoding="utf-8") as handle:
    handle.writelines(lines)
PY
}

partner_field() {
	yq -r ".partners[] | select(.server_name == \"$1\") | $2 // \"\"" "${REGISTRY}"
}

require_admitted_partner() {
	local partner="$1" role
	role="$(partner_field "${partner}" ".role")"
	[ -n "${role}" ] || fail "unknown partner: ${partner} (not in ${REGISTRY})"
	[ "${role}" = "admitted" ] || fail "break-glass targets an admitted partner; '${partner}' is role '${role}'"
}

# One content-free containment record: who (server_name, azp, classification), when (per-step UTC), what
# (the safe-order planes), and the current git revision. Never event content, prompts, or message bodies.
write_record() {
	local partner="$1" action="$2"
	local azp classification revision now
	azp="$(partner_field "${partner}" ".a2a.azp")"
	classification="$(partner_field "${partner}" ".classification")"
	revision="$(git -C "${ROOT_DIR}" rev-parse HEAD)"
	now="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
	mkdir -p "${RECORD_DIR}"
	local record="${RECORD_DIR}/containment-${partner}.json"
	jq -n \
		--arg action "${action}" --arg partner "${partner}" --arg azp "${azp}" \
		--arg classification "${classification}" --arg revision "${revision}" --arg now "${now}" '{
		schema: "fgentic.federation.containment.v1",
		action: $action,
		partner: {server_name: $partner, azp: $azp, classification: $classification},
		git_revision: $revision,
		recorded_at: $now,
		safe_order: [
			{order: 1, plane: "a2a-quota",         control: "revoke the azp reservation + rate-limit at agentgateway"},
			{order: 2, plane: "bridge-sender",     control: "remove allowedServers/allowedSenders and the public A2A route"},
			{order: 3, plane: "callback-border",   control: "drop from policy.json allowed_servers (rendered here)", rendered: true},
			{order: 4, plane: "acl-and-whitelist", control: "remove from m.room.server_acl and federation_domain_whitelist"}
		]
	}' >"${record}"
	echo "${record}"
}

print_runtime_steps() {
	local partner="$1" azp="$2"
	cat >&2 <<EOF

Break-glass safe-order runtime steps (run against the reconciled lab, in order):
  1. a2a-quota       — the rendered border already denies '${partner}'; confirm the azp '${azp}' A2A
                       route returns fail-closed (agentgateway authz denies the dropped border).
  2. bridge-sender   — remove '${partner}' from every agent allowedServers/allowedSenders and drop its
                       public A2A route (this lab has no local bridge; a production cluster does).
  3. callback-border — commit the re-rendered policy.json and let Flux reconcile the callback module.
  4. acl-whitelist   — remove '${partner}' from each room m.room.server_acl and, if desired, from
                       federation_domain_whitelist. Already-replicated Matrix history is NOT retracted
                       by ACL/whitelist removal — request redaction separately, stated honestly.
EOF
}

main() {
	[ "$#" -ge 1 ] || {
		usage
		exit 2
	}
	case "$1" in
		contain)
			[ "$#" -eq 2 ] || {
				usage
				exit 2
			}
			local partner="$2"
			require_admitted_partner "${partner}"
			set_contained "${partner}" true
			bash "${ROOT_DIR}/scripts/fed-registry-render.sh" >/dev/null
			local record
			record="$(write_record "${partner}" contain)"
			echo "break-glass ENGAGED for ${partner}: dropped from the callback border; record ${record}"
			local azp
			azp="$(partner_field "${partner}" ".a2a.azp")"
			print_runtime_steps "${partner}" "${azp}"
			echo "Commit the re-render (registry + policy.json) so Flux reconciles the containment." >&2
			;;
		restore)
			[ "$#" -eq 2 ] || {
				usage
				exit 2
			}
			local partner="$2"
			require_admitted_partner "${partner}"
			set_contained "${partner}" false
			bash "${ROOT_DIR}/scripts/fed-registry-render.sh" >/dev/null
			write_record "${partner}" restore >/dev/null
			echo "break-glass RESTORED for ${partner}: re-admitted to the callback border (commit the re-render)."
			;;
		status)
			local contained
			contained="$(yq -r '[.partners[] | select(.contained == true) | .server_name] | join(", ")' "${REGISTRY}")"
			if [ -n "${contained}" ]; then
				echo "contained partners: ${contained}"
			else
				echo "no partners are contained"
			fi
			;;
		*)
			usage
			exit 2
			;;
	esac
}

main "$@"
