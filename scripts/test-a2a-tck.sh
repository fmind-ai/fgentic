#!/usr/bin/env bash
# Offline contract for the pinned, authenticated, deliberately scoped federation TCK gate.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly RUNNER="${ROOT_DIR}/scripts/fed-tck.sh"
readonly PLUGIN="${ROOT_DIR}/scripts/a2a_tck_plugin.py"
readonly SCOPE="${ROOT_DIR}/scripts/a2a-tck-scope.json"
readonly MISE_CONFIG="${ROOT_DIR}/mise.toml"
readonly TCK_COMMIT="5996b79f9cefa6fc390980e383e358a66fb9e49e"
readonly TCK_SHA256="74fc0cd3e8c5fad08fb090885c5fc76228d63dd9a5ff5f29e0cc7fea56414e8c"

fail() {
	echo "error: $*" >&2
	exit 1
}

for command in bash jq rg; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

bash -n "${RUNNER}" "$0"
for file in "${RUNNER}" "${PLUGIN}" "${SCOPE}"; do
	[ -s "${file}" ] || fail "A2A TCK contract file is missing: ${file}"
done

jq -e --arg commit "${TCK_COMMIT}" --arg sha256 "${TCK_SHA256}" '
  .schemaVersion == 1 and
  .tck == {archiveSha256: $sha256, commit: $commit, version: "1.0.0"} and
  .tier == "MUST" and
  .transport == "JSONRPC" and
  (.allow | length) == 17 and
  (.skip | length) == 20 and
  all(.allow[];
    (.id | type == "string" and length > 0) and
    (.pattern | type == "string" and length > 0) and
    (.reason | type == "string" and length > 0) and
    (.expectedOutcome == "passed" or .expectedOutcome == "skipped") and
    (if .expectedOutcome == "skipped" then
      (.expectedReason | type == "string" and length > 0)
    else true end)) and
  all(.skip[];
    (.id | type == "string" and length > 0) and
    (.pattern | type == "string" and length > 0) and
    (.reason | type == "string" and length > 0)) and
  ([.allow[].id, .skip[].id] | flatten | length) ==
    ([.allow[].id, .skip[].id] | flatten | unique | length) and
  any(.allow[];
    .pattern | contains("TestJsonRpcFormat::test_") and
    contains("content_type_is_application_json"))
' "${SCOPE}" >/dev/null || fail "A2A TCK scope contract is invalid"

for contract in \
	"readonly TCK_COMMIT=\"${TCK_COMMIT}\"" \
	"readonly TCK_ARCHIVE_SHA256=\"${TCK_SHA256}\"" \
	'https://github.com/a2aproject/a2a-tck/archive/${TCK_COMMIT}.tar.gz' \
	'yq --input-format toml --output-format yaml --unwrapScalar' \
	'--namespace "${GATEWAY_NAMESPACE}" port-forward' \
	'--data-binary @-' \
	'FGENTIC_TCK_BEARER_TOKEN="${access_token}"' \
	'FGENTIC_TCK_EXTENSION_URI="${TOKEN_BUDGET_EXTENSION}"' \
	'FGENTIC_TCK_USAGE_RECEIPT_URI="${USAGE_RECEIPT_EXTENSION}"' \
	'--transport jsonrpc -m must' \
	'UV_BIN="$(mise --cd "${ROOT_DIR}/apps/synapse-federation-policy" which uv)"' \
	'UV_PROJECT_ENVIRONMENT="${VENV_DIR}" "${UV_BIN}" sync --locked' \
	'"${UV_BIN}" run --no-sync python -B -m pytest' \
	'PYTHONDONTWRITEBYTECODE=1' \
	'--junitxml "${REPORT_DIR}/junitreport.xml"' \
	'--html "${REPORT_DIR}/tck_report.html"' \
	'.summary == {failed: 0, missing: 0, passed: 33, skipped: 202}' \
	'k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_FILE}"' \
	'unset KUBECONFIG' \
	'rg --quiet --fixed-strings --no-ignore --hidden --file "${CREDENTIAL_PATTERNS}" "${REPORT_DIR}"' \
	'discarded the staged reports' \
	'"dev.fgentic.demo"' \
	'kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${KUBE_CONTEXT}"'; do
	rg --quiet --fixed-strings -- "${contract}" "${RUNNER}" \
		|| fail "A2A TCK runner omits contract: ${contract}"
done
if rg --quiet --fixed-strings -- '--data-urlencode "client_secret=' "${RUNNER}"; then
	fail "A2A TCK runner exposes the client secret in process arguments"
fi
if rg --quiet --fixed-strings -- 'export KUBECONFIG' "${RUNNER}"; then
	fail "A2A TCK runner exports cluster credentials to third-party processes"
fi
for contract in \
	'unexpected TCK JSON-RPC request URL' \
	'"A2A-Extensions": f"{token_budget_uri}, {usage_receipt_uri}"' \
	'for extension_uri in (token_budget_uri, usage_receipt_uri)' \
	'http_client.post = adapted_post' \
	'http_client.build_request = adapted_build_request'; do
	rg --quiet --fixed-strings -- "${contract}" "${PLUGIN}" \
		|| fail "A2A TCK adapter omits exact-route contract: ${contract}"
done

for task in 'check:a2a-tck' 'fed:tck'; do
	rg --quiet --fixed-strings "[tasks.\"${task}\"]" "${MISE_CONFIG}" \
		|| fail "mise task is missing: ${task}"
done

echo "A2A TCK pin, scope, authentication, report, and secret-handling contracts passed."
