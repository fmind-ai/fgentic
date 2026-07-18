#!/usr/bin/env bash
# Run the pinned official A2A TCK through org B's authenticated exported-route contract.
set -euo pipefail
umask 077

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly TCK_VERSION="1.0.0"
readonly TCK_COMMIT="5996b79f9cefa6fc390980e383e358a66fb9e49e"
readonly TCK_ARCHIVE_SHA256="74fc0cd3e8c5fad08fb090885c5fc76228d63dd9a5ff5f29e0cc7fea56414e8c"
readonly TCK_ARCHIVE_URL="https://github.com/a2aproject/a2a-tck/archive/${TCK_COMMIT}.tar.gz"
readonly TCK_SCOPE_FILE="${ROOT_DIR}/scripts/a2a-tck-scope.json"
readonly CLUSTER_NAME="fgentic-fed"
readonly KUBE_CONTEXT="k3d-${CLUSTER_NAME}"
readonly OWNER_LABEL="federation"
readonly GATEWAY_NAMESPACE="gateway"
readonly SERVER_A="org-a.fgentic.localhost"
readonly SERVER_B="org-b.fgentic.localhost"
readonly A2A_AGENT_PATH="/api/a2a/kagent/docs-qa"
readonly TOKEN_BUDGET_EXTENSION="https://fgentic.fmind.ai/a2a/extensions/token-budget/v1"
readonly USAGE_RECEIPT_EXTENSION="https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1"
readonly CA_CERT="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
readonly CACHE_DIR="${XDG_CACHE_HOME:-${HOME}/.cache}/fgentic/a2a-tck/${TCK_COMMIT}"
readonly ARCHIVE_FILE="${CACHE_DIR}/source.tar.gz"
readonly VENV_DIR="${CACHE_DIR}/venv"
readonly ARTIFACT_DIR="${FGENTIC_FED_TCK_ARTIFACT_DIR:-${ROOT_DIR}/.agents/tmp/federation-tck}"
readonly PORT="${FGENTIC_FED_TCK_PORT:-19443}"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-a2a-tck.XXXXXX")"
readonly WORK_DIR
readonly SOURCE_DIR="${WORK_DIR}/source"
readonly PORT_FORWARD_LOG="${WORK_DIR}/port-forward.log"
readonly KUBECONFIG_FILE="${WORK_DIR}/kubeconfig"
readonly REPORT_DIR="${WORK_DIR}/reports"
readonly CREDENTIAL_PATTERNS="${WORK_DIR}/credential-patterns"

port_forward_pid=""
download=""
client_secret=""
access_token=""
unset KUBECONFIG

die() {
	echo "error: $*" >&2
	exit 1
}

cleanup() {
	local status=$?
	trap - EXIT
	client_secret=""
	access_token=""
	if [ -n "${download}" ]; then
		rm -f "${download}"
	fi
	if [ -n "${port_forward_pid}" ]; then
		kill "${port_forward_pid}" >/dev/null 2>&1 || true
		wait "${port_forward_pid}" >/dev/null 2>&1 || true
	fi
	rm -rf "${WORK_DIR}"
	exit "${status}"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

for command in curl docker jq k3d kubectl mise rg sha256sum tar yq; do
	command -v "${command}" >/dev/null 2>&1 || die "required command not found: ${command}"
done
[[ "${PORT}" =~ ^[0-9]+$ ]] || die "FGENTIC_FED_TCK_PORT must be an integer"
((10#${PORT} >= 1024 && 10#${PORT} <= 65535)) \
	|| die "FGENTIC_FED_TCK_PORT must be between 1024 and 65535"
[ -r "${CA_CERT}" ] || die "local CA certificate not found: ${CA_CERT}"
[ -r "${TCK_SCOPE_FILE}" ] || die "TCK scope document not found: ${TCK_SCOPE_FILE}"

actual_owner="$(docker inspect --format '{{index .Config.Labels "dev.fgentic.demo"}}' \
	"k3d-${CLUSTER_NAME}-server-0" 2>/dev/null || true)"
[ "${actual_owner}" = "${OWNER_LABEL}" ] \
	|| die "${CLUSTER_NAME} is absent or not the ownership-labelled federation lab; run 'mise run fed:up'"
if ! k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_FILE}"; then
	die "could not derive kubeconfig from the ownership-labelled federation lab"
fi
chmod 600 "${KUBECONFIG_FILE}"
current_context="$(kubectl --kubeconfig "${KUBECONFIG_FILE}" config current-context)"
[ "${current_context}" = "${KUBE_CONTEXT}" ] \
	|| die "derived federation kubeconfig has an unexpected current context"
kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${KUBE_CONTEXT}" get --raw=/readyz >/dev/null \
	|| die "${CLUSTER_NAME} API is not ready; run 'mise run fed:up'"

mkdir -p "${CACHE_DIR}" "${SOURCE_DIR}" "${REPORT_DIR}"
if [ ! -f "${ARCHIVE_FILE}" ]; then
	download="$(mktemp "${CACHE_DIR}/source.tar.gz.download.XXXXXX")"
	curl --fail --location --silent --show-error --retry 3 --retry-all-errors \
		--connect-timeout 10 --max-time 180 --output "${download}" "${TCK_ARCHIVE_URL}"
	download_sha256="$(sha256sum "${download}" | awk '{print $1}')"
	[ "${download_sha256}" = "${TCK_ARCHIVE_SHA256}" ] \
		|| die "downloaded A2A TCK archive checksum mismatch"
	mv "${download}" "${ARCHIVE_FILE}"
	download=""
fi
actual_archive_sha256="$(sha256sum "${ARCHIVE_FILE}" | awk '{print $1}')"
if [ "${actual_archive_sha256}" != "${TCK_ARCHIVE_SHA256}" ]; then
	rm -f "${ARCHIVE_FILE}"
	die "cached A2A TCK archive checksum mismatch; removed the poisoned cache entry"
fi
tar --extract --gzip --file "${ARCHIVE_FILE}" --directory "${SOURCE_DIR}" --strip-components=1
source_version="$(yq --input-format toml --output-format yaml --unwrapScalar '.project.version' \
	"${SOURCE_DIR}/pyproject.toml")"
[ "${source_version}" = "${TCK_VERSION}" ] \
	|| die "pinned A2A TCK source version mismatch"

UV_BIN="$(mise --cd "${ROOT_DIR}/apps/synapse-federation-policy" which uv)"
readonly UV_BIN
UV_PROJECT_ENVIRONMENT="${VENV_DIR}" "${UV_BIN}" sync --locked --project "${SOURCE_DIR}"

kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${KUBE_CONTEXT}" \
	--namespace "${GATEWAY_NAMESPACE}" port-forward \
	--address 127.0.0.1 service/traefik "${PORT}:443" >"${PORT_FORWARD_LOG}" 2>&1 &
port_forward_pid="$!"
for ((_attempt = 1; _attempt <= 30; _attempt++)); do
	if rg --quiet --fixed-strings "Forwarding from 127.0.0.1:${PORT}" "${PORT_FORWARD_LOG}"; then
		break
	fi
	kill -0 "${port_forward_pid}" >/dev/null 2>&1 || {
		cat "${PORT_FORWARD_LOG}" >&2
		die "federation Gateway port-forward exited before becoming ready"
	}
	sleep 1
done
rg --quiet --fixed-strings "Forwarding from 127.0.0.1:${PORT}" "${PORT_FORWARD_LOG}" \
	|| die "federation Gateway port-forward did not become ready"

client_secret="$(kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${KUBE_CONTEXT}" \
	--namespace flux-system \
	get secret fgentic-demo-bootstrap \
	--output 'go-template={{index .data "org-b-a2a-client-secret" | base64decode}}')"
[ -n "${client_secret}" ] || die "org-B A2A client secret is empty"
token_response="$({
	printf 'grant_type=client_credentials&client_id=org-b-a2a&client_secret=%s' "${client_secret}"
} | curl --noproxy '*' --fail --silent --show-error --cacert "${CA_CERT}" \
	--connect-timeout 10 --max-time 30 \
	--request POST --header 'Content-Type: application/x-www-form-urlencoded' \
	--data-binary @- \
	"https://id.${SERVER_B}:${PORT}/realms/fgentic-federation/protocol/openid-connect/token")"
access_token="$(jq -er '.access_token | select(type == "string" and length > 0)' \
	<<<"${token_response}")"
token_response=""

flushed="$(kubectl --kubeconfig "${KUBECONFIG_FILE}" --context "${KUBE_CONTEXT}" \
	--namespace agentgateway-system \
	exec deployment/federation-redis -- redis-cli FLUSHDB)"
[ "${flushed}" = "OK" ] || die "failed to reset the disposable federation quota fixture"

readonly SUT_HOST="https://a2a.${SERVER_A}:${PORT}${A2A_AGENT_PATH}"
set +e
(
	cd "${SOURCE_DIR}"
	FGENTIC_TCK_INTERFACE_URL="${SUT_HOST}" \
		FGENTIC_TCK_BEARER_TOKEN="${access_token}" \
		FGENTIC_TCK_EXTENSION_URI="${TOKEN_BUDGET_EXTENSION}" \
		FGENTIC_TCK_USAGE_RECEIPT_URI="${USAGE_RECEIPT_EXTENSION}" \
		FGENTIC_TCK_MAX_TOKENS="1" \
		FGENTIC_TCK_SCOPE_FILE="${TCK_SCOPE_FILE}" \
		FGENTIC_TCK_SCOPE_REPORT="${REPORT_DIR}/fgentic-scope.json" \
		NO_PROXY="*" no_proxy="*" SSL_CERT_FILE="${CA_CERT}" \
		UV_PROJECT_ENVIRONMENT="${VENV_DIR}" PYTHONDONTWRITEBYTECODE=1 \
		PYTHONPATH="${ROOT_DIR}/scripts${PYTHONPATH:+:${PYTHONPATH}}" \
		"${UV_BIN}" run --no-sync python -B -m pytest tests/compatibility \
		--sut-host "${SUT_HOST}" --transport jsonrpc -m must -q \
		-p a2a_tck_plugin \
		--compatibility-report "${REPORT_DIR}/compatibility" \
		--html "${REPORT_DIR}/tck_report.html" --self-contained-html \
		--junitxml "${REPORT_DIR}/junitreport.xml"
)
tck_status="$?"
set -e

printf '%s\n%s\n' "${client_secret}" "${access_token}" >"${CREDENTIAL_PATTERNS}"
set +e
rg --quiet --fixed-strings --no-ignore --hidden --file "${CREDENTIAL_PATTERNS}" "${REPORT_DIR}"
credential_scan_status="$?"
set -e
: >"${CREDENTIAL_PATTERNS}"
client_secret=""
access_token=""
case "${credential_scan_status}" in
	0)
		rm -rf "${REPORT_DIR}"
		die "A2A TCK report contained a runtime credential; discarded the staged reports"
		;;
	1) ;;
	*) die "could not scan the staged A2A TCK reports for runtime credentials" ;;
esac

mkdir -p "${ARTIFACT_DIR}"
for artifact in compatibility.html compatibility.json tck_report.html junitreport.xml fgentic-scope.json; do
	rm -f "${ARTIFACT_DIR}/${artifact}"
	if [ -f "${REPORT_DIR}/${artifact}" ]; then
		cp "${REPORT_DIR}/${artifact}" "${ARTIFACT_DIR}/${artifact}"
	fi
done
for artifact in compatibility.html compatibility.json tck_report.html junitreport.xml fgentic-scope.json; do
	[ -s "${ARTIFACT_DIR}/${artifact}" ] || die "A2A TCK omitted artifact: ${artifact}"
done

((tck_status == 0)) || die "A2A TCK failed; inspect ${ARTIFACT_DIR}"
jq -e '
  .tck.version == "1.0.0" and
  .tck.commit == "5996b79f9cefa6fc390980e383e358a66fb9e49e" and
  .tck.archiveSha256 == "74fc0cd3e8c5fad08fb090885c5fc76228d63dd9a5ff5f29e0cc7fea56414e8c" and
  .tier == "MUST" and
  .transport == "JSONRPC" and
  .summary == {failed: 0, missing: 0, passed: 33, skipped: 202} and
  .mismatches == []
' "${ARTIFACT_DIR}/fgentic-scope.json" >/dev/null \
	|| die "A2A TCK pass/skip set drifted from the reviewed exported-route scope"

echo "A2A TCK ${TCK_VERSION} MUST/JSONRPC scope passed: 33 passed, 202 annotated skips."
echo "Reports: ${ARTIFACT_DIR}"
