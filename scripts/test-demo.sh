#!/usr/bin/env bash
# Offline contract checks for the credential-free evaluation lifecycle and its embedded model.
set -euo pipefail

readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo-check.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

assert_yq() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq --exit-status "${expression}" "${document}" >/dev/null || {
		echo "error: ${message}" >&2
		exit 1
	}
}

bash -n "${ROOT_DIR}/scripts/demo.sh" "${ROOT_DIR}/scripts/seed-demo.sh"
"${ROOT_DIR}/scripts/demo.sh" --help >"${WORK_DIR}/help.txt"
rg --fixed-strings 'deterministic in-cluster response stub' "${WORK_DIR}/help.txt" >/dev/null
rg --fixed-strings 'FGENTIC_ALLOW_PAID_PROVIDER=yes' "${WORK_DIR}/help.txt" >/dev/null
rg --fixed-strings 'FGENTIC_DEMO_CACHE_DIR' "${WORK_DIR}/help.txt" >/dev/null
if FGENTIC_DEMO_CLUSTER=fgentic "${ROOT_DIR}/scripts/demo.sh" down \
	>"${WORK_DIR}/reserved-cluster.txt" 2>&1; then
	echo 'error: demo teardown accepted the reserved fgentic cluster name' >&2
	exit 1
fi
rg --fixed-strings 'must be fgentic-demo' "${WORK_DIR}/reserved-cluster.txt" >/dev/null

kubectl kustomize "${ROOT_DIR}/clusters/demo" >"${WORK_DIR}/cluster.yaml"
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "platform-settings") |
    .data.llm_provider == "demo" and
    .data.llm_model == "fgentic-demo" and
    .data.demo_bridge_tag == "local" and
    .data.mas_local_login_enabled == "true" and
    .data.llm_usage_budget_15m == "100000"' \
	"${WORK_DIR}/cluster.yaml" 'demo platform settings are incomplete'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "agentgateway-provider") |
    .spec.path == "./infra/agentgateway/providers/profiles/demo"' \
	"${WORK_DIR}/cluster.yaml" 'provider-selection did not select the demo inventory'
for omitted_layer in observability observability-monitors keycloak; do
	if yq --unwrapScalar \
		'select(.kind == "Kustomization") | .metadata.name' \
		"${WORK_DIR}/cluster.yaml" | rg --fixed-strings --line-regexp "${omitted_layer}" >/dev/null; then
		echo "error: small profile still contains ${omitted_layer}" >&2
		exit 1
	fi
done
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "platform-secrets") |
    .spec.path == "./clusters/demo/empty" and (.spec.decryption == null)' \
	"${WORK_DIR}/cluster.yaml" 'demo secret inventory must be empty and non-SOPS'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "bridge") |
    ((.spec.patches[0].patch | contains("matrix-a2a-bridge")) and
     (.spec.patches[0].patch | contains("pullPolicy: Never")))' \
	"${WORK_DIR}/cluster.yaml" 'demo bridge is not pinned to the side-loaded image'

kubectl kustomize "${ROOT_DIR}/infra/flux" >"${WORK_DIR}/controllers.yaml"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "traefik") |
    .spec.timeout == "10m"' \
	"${WORK_DIR}/controllers.yaml" \
	'Traefik Helm actions must tolerate constrained-host startup'

kubectl kustomize "${ROOT_DIR}/infra/agentgateway/providers/profiles/demo" \
	>"${WORK_DIR}/provider.yaml"
assert_yq \
	'select(.kind == "AgentgatewayBackend" and .metadata.name == "llm-demo") |
    .spec.ai.provider.openai.model == "${llm_model}" and
    .spec.ai.provider.host == "demo-llm.models.svc.cluster.local" and
    .spec.ai.provider.port == 80' \
	"${WORK_DIR}/provider.yaml" 'demo AgentgatewayBackend contract changed'
assert_yq \
	'select(.kind == "Deployment" and .metadata.name == "demo-llm") |
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
    (.spec.template.spec.containers[0].image | contains("python:3.14-slim@sha256:"))' \
	"${WORK_DIR}/provider.yaml" 'demo model workload is not pinned and hardened'

yq --unwrapScalar \
	'select(.kind == "ConfigMap" and .metadata.name == "demo-llm") | .data."server.py"' \
	"${WORK_DIR}/provider.yaml" >"${WORK_DIR}/server.py"
python -m py_compile "${WORK_DIR}/server.py"
rg --fixed-strings 'chat.completion' "${WORK_DIR}/server.py" >/dev/null
rg --fixed-strings 'data: [DONE]' "${WORK_DIR}/server.py" >/dev/null

rg --fixed-strings 'mcp-agent-callers' "${ROOT_DIR}/scripts/demo.sh" >/dev/null
rg --fixed-strings 'platform-helper-mcp-credential' "${ROOT_DIR}/scripts/demo.sh" >/dev/null
rg --regexp 'SOURCE_BASE_IMAGE="[^" ]+@sha256:[0-9a-f]{64}"' \
	"${ROOT_DIR}/scripts/demo.sh" >/dev/null
rg --regexp 'SOURCE_GIT_PACKAGES="git=[^ ]+ git-daemon=[^ ]+ busybox-extras=[^"]+"' \
	"${ROOT_DIR}/scripts/demo.sh" >/dev/null
rg --fixed-strings 'git-http-backend' "${ROOT_DIR}/scripts/demo.sh" >/dev/null
rg --fixed-strings 'http://fgentic-demo-source.flux-system.svc.cluster.local:8080/cgi-bin/git/repo.git' \
	"${ROOT_DIR}/scripts/demo.sh" >/dev/null
rg --fixed-strings '#lobby:fgentic.localhost' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
rg --fixed-strings 'creation_content: {"m.federate": false}' \
	"${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
rg --fixed-strings '/state/m.room.create' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
for contract in \
	'create_lobby' \
	'publish_lobby_alias' \
	'set_lobby_canonical_alias' \
	'lobby_has_canonical_alias' \
	'retire_legacy_lobby_alias' \
	'Migrating legacy #lobby to immutable local-only federation policy.' \
	'#lobby is not local-only after reconciliation' \
	'--request DELETE'; do
	rg --fixed-strings -- "${contract}" "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
done
rg --fixed-strings '/api/admin/v1/users' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null

if rg -n 'mas_password_login_enabled|llm_token_budget_15m' \
	"${ROOT_DIR}/clusters/demo" "${ROOT_DIR}/scripts/demo.sh"; then
	echo 'error: demo path uses a retired platform-setting name' >&2
	exit 1
fi

echo 'Demo install contracts passed.'
