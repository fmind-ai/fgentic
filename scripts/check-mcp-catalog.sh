#!/usr/bin/env bash
# Validate the versioned MCP catalog and its complete rendered resource coverage.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
CATALOG_ROOT="${MCP_CATALOG_ROOT:-${REPO_ROOT}/infra/mcp-catalog}"
SCHEMA_URL="https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json"
CATALOG_ANNOTATION="fgentic.dev/mcp-catalog-entry"

fail() {
	echo "MCP catalog check failed: $*" >&2
	exit 1
}

if [ "$#" -ne 2 ]; then
	echo "usage: scripts/check-mcp-catalog.sh <agentgateway-render.yaml> <kagent-render.yaml>" >&2
	exit 2
fi
agentgateway_render="$1"
kagent_render="$2"
[ -f "${agentgateway_render}" ] || fail "agentgateway render not found: ${agentgateway_render}"
[ -f "${kagent_render}" ] || fail "kagent render not found: ${kagent_render}"

for command in jq sha256sum yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

shopt -s nullglob
catalog_files=("${CATALOG_ROOT}"/*/server.json)
shopt -u nullglob
[ "${#catalog_files[@]}" -gt 0 ] || fail "no server.json entries found under ${CATALOG_ROOT}"

tmp_dir="$(mktemp -d)"
trap 'rm -rf "${tmp_dir}"' EXIT
catalog_json="${tmp_dir}/catalog.json"
jq -s . "${catalog_files[@]}" >"${catalog_json}"

for entry in "${catalog_files[@]}"; do
	entry_id="$(basename "$(dirname "${entry}")")"
	jq -e --arg schema "${SCHEMA_URL}" '
      type == "object" and
      .["$schema"] == $schema and
      (.name | type == "string" and test("^[A-Za-z0-9.-]+/[A-Za-z0-9._-]+$")) and
      (.description | type == "string" and length > 0 and length <= 100) and
      (.version | type == "string" and length > 0 and . != "latest") and
      (.repository | type == "object" and
        (.url | type == "string" and startswith("https://")) and
        (.source | type == "string" and length > 0) and
        (.id | type == "string" and length > 0)) and
      (.remotes | type == "array" and length > 0 and all(.[];
        .type == "streamable-http" and
        (.url | type == "string" and test("^https?://[^[:space:]]+$")) and
        ((.headers // []) | all(.[];
          (.name | type == "string" and length > 0) and
          .isRequired == true and .isSecret == true and
          (has("value") | not) and (has("default") | not))))) and
      ((keys - ["$schema", "_meta", "description", "icons", "name", "packages", "remotes", "repository", "title", "version", "websiteUrl"]) | length == 0)
    ' "${entry}" >/dev/null || fail "${entry} violates the pinned server.json profile"

	meta_filter='.["_meta"]["io.modelcontextprotocol.registry/publisher-provided"].fgentic'
	jq -e --arg id "${entry_id}" "
      ${meta_filter} as \$meta |
      (\$meta | type == \"object\") and
      \$meta.catalogVersion == 1 and \$meta.id == \$id and
      (\$meta.vettedBy | type == \"string\" and length > 0) and
      (\$meta.vettedAt | type == \"string\" and test(\"^[0-9]{4}-[0-9]{2}-[0-9]{2}$\")) and
      (\$meta.sourceLicense | type == \"string\" and length > 0) and
      (\$meta.sourceRevision | type == \"string\" and test(\"^[0-9a-f]{40}$\")) and
      (\$meta.image | type == \"string\" and test(\"@sha256:[0-9a-f]{64}$\")) and
      (\$meta.allowedTools | type == \"array\" and length > 0 and
        all(.[]; type == \"string\" and length > 0) and . == (sort | unique)) and
      (\$meta.resources.agentgatewayBackends | type == \"array\" and . == (sort | unique)) and
      (\$meta.resources.remoteMCPServers | type == \"array\" and . == (sort | unique)) and
      (\$meta.surfacePin.path | type == \"string\" and startswith(\"infra/\")) and
      (\$meta.surfacePin.sha256 | type == \"string\" and test(\"^[0-9a-f]{64}$\")) and
      (\$meta.surfacePin.surfaceSha256 | type == \"string\" and test(\"^[0-9a-f]{64}$\"))
    " "${entry}" >/dev/null || fail "${entry} has incomplete Fgentic vetting metadata"

	pin_relative="$(jq -er "${meta_filter}.surfacePin.path" "${entry}")"
	[[ "${pin_relative}" == infra/* && "${pin_relative}" != *..* ]] \
		|| fail "${entry} has an unsafe surface pin path: ${pin_relative}"
	pin_path="${REPO_ROOT}/${pin_relative}"
	[ -f "${pin_path}" ] || fail "${entry} references missing surface pin ${pin_relative}"
	expected_pin_sha="$(jq -er "${meta_filter}.surfacePin.sha256" "${entry}")"
	actual_pin_sha="$(sha256sum "${pin_path}" | awk '{print $1}')"
	[ "${actual_pin_sha}" = "${expected_pin_sha}" ] \
		|| fail "${entry} surface pin hash drifted: got ${actual_pin_sha}, want ${expected_pin_sha}"

	expected_image="$(jq -er "${meta_filter}.image" "${entry}")"
	expected_surface_sha="$(jq -er "${meta_filter}.surfacePin.surfaceSha256" "${entry}")"
	jq -e --arg id "${entry_id}" --arg image "${expected_image}" --arg surface "${expected_surface_sha}" \
		--slurpfile entry "${entry}" '
      .servers | any(.[];
        . as $server |
        $server.name == $id and $server.provenance.image == $image and $server.surfaceSha256 == $surface and
        (($entry[0]["_meta"]["io.modelcontextprotocol.registry/publisher-provided"].fgentic.allowedTools) as $allowed |
          all($allowed[]; . as $tool | any($server.tools.entries[]; .identity == $tool))))
    ' "${pin_path}" >/dev/null || fail "${entry} disagrees with its immutable MCP surface pin"
done

jq -e '
  (map(.["_meta"]["io.modelcontextprotocol.registry/publisher-provided"].fgentic.id) as $ids |
    ($ids | length) == ($ids | unique | length)) and
  (map(.name) as $names | ($names | length) == ($names | unique | length))
' "${catalog_json}" >/dev/null || fail "catalog IDs and official server names must be unique"

resources_json="${tmp_dir}/resources.json"
{
	yq eval-all -N -o=json -I=0 '
      select(.kind == "AgentgatewayBackend" and .spec.mcp != null) |
      {"kind": .kind, "name": .metadata.name,
       "catalog": (.metadata.annotations."fgentic.dev/mcp-catalog-entry" // "")}
    ' "${agentgateway_render}"
	yq eval-all -N -o=json -I=0 '
      select(.kind == "RemoteMCPServer") |
      {"kind": .kind, "name": .metadata.name,
       "catalog": (.metadata.annotations."fgentic.dev/mcp-catalog-entry" // "")}
    ' "${kagent_render}"
} | jq -s . >"${resources_json}"

[ "$(jq 'length' "${resources_json}")" -gt 0 ] || fail "rendered profiles contain no MCP resources"
while IFS=$'\t' read -r kind name catalog_id; do
	[ -n "${catalog_id}" ] || fail "${kind}/${name} has no ${CATALOG_ANNOTATION} annotation"
	jq -e --arg id "${catalog_id}" '
      any(.[]; .["_meta"]["io.modelcontextprotocol.registry/publisher-provided"].fgentic.id == $id)
    ' "${catalog_json}" >/dev/null || fail "${kind}/${name} references unknown catalog entry ${catalog_id}"
done < <(jq -r '.[] | [.kind, .name, .catalog] | @tsv' "${resources_json}")

while IFS= read -r catalog_id; do
	for mapping in 'AgentgatewayBackend:agentgatewayBackends' 'RemoteMCPServer:remoteMCPServers'; do
		kind="${mapping%%:*}"
		field="${mapping#*:}"
		expected="$({
			jq -r --arg id "${catalog_id}" --arg field "${field}" '
            .[] | select(.["_meta"]["io.modelcontextprotocol.registry/publisher-provided"].fgentic.id == $id) |
            .["_meta"]["io.modelcontextprotocol.registry/publisher-provided"].fgentic.resources[$field][]
          ' "${catalog_json}" | sort
		})"
		actual="$({
			jq -r --arg id "${catalog_id}" --arg kind "${kind}" '
            .[] | select(.catalog == $id and .kind == $kind) | .name
          ' "${resources_json}" | sort
		})"
		[ "${actual}" = "${expected}" ] \
			|| fail "${catalog_id} ${kind} coverage differs: got '${actual}', want '${expected}'"
	done
done < <(jq -r '.[]._meta["io.modelcontextprotocol.registry/publisher-provided"].fgentic.id' \
	"${catalog_json}" | sort)

echo "MCP catalog contract passed (${#catalog_files[@]} vetted server(s))"
