#!/usr/bin/env bash
# Exercise real age/SOPS encryption in a disposable Git repository. No production recipient,
# cluster, or provider credential is used; every secret value is synthetic and remains encrypted
# on disk.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GENERATOR="${SCRIPT_DIR}/gen-secrets.sh"
ROTATOR="${SCRIPT_DIR}/rotate-secrets.sh"

for command in age-keygen awk date git kubectl sha256sum sops yq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required test command not found: ${command}" >&2
		exit 1
	fi
done

# Git hooks export repository-local variables such as GIT_DIR and GIT_INDEX_FILE. Clear every
# variable Git classifies as local before initializing the disposable fixture, or its synthetic
# commits can mutate the caller's branch while this test runs from pre-commit.
git_local_variables="$(git rev-parse --local-env-vars)"
while IFS= read -r git_variable; do
	[ -z "${git_variable}" ] || unset "${git_variable}"
done <<<"${git_local_variables}"
unset git_local_variables git_variable

WORK_DIR="$(mktemp -d)"
FIXTURE_ROOT="${WORK_DIR}/fixture"
trap 'rm -rf "${WORK_DIR}"' EXIT
mkdir -p "${FIXTURE_ROOT}/clusters/local/secrets"

fail() {
	echo "error: $1" >&2
	exit 1
}

assert_equal() {
	[ "$1" = "$2" ] || fail "$3"
}

assert_changed() {
	[ "$1" != "$2" ] || fail "$3 did not change"
}

expect_failure() {
	if "$@" >/dev/null 2>&1; then
		fail "command unexpectedly succeeded: $*"
	fi
}

commit_fixture() {
	git -C "${FIXTURE_ROOT}" add .
	git -C "${FIXTURE_ROOT}" commit -qm "$1"
}

assert_changed_files() {
	local actual expected
	actual="$(git -C "${FIXTURE_ROOT}" diff --name-only | sort)"
	expected="$(printf 'clusters/local/secrets/%s\n' "$@" | sort)"
	assert_equal "${actual}" "${expected}" "unexpected fixture files changed"
}

secret_value() { # secret_value <file> <namespace> <name> <yq suffix>
	local file="$1"
	local namespace="$2"
	local name="$3"
	local suffix="$4"
	sops --decrypt "${FIXTURE_ROOT}/clusters/local/secrets/${file}" |
		yq eval-all -er "select(.metadata.namespace == \"${namespace}\" and .metadata.name == \"${name}\") | ${suffix}" -
}

registration() {
	secret_value matrix-a2a-bridge-registration.sops.yaml "$1" matrix-a2a-bridge-registration '.stringData."registration.yaml"'
}

slack_registration() {
	secret_value mautrix-slack.sops.yaml matrix mautrix-slack-registration '.stringData."registration.yaml"'
}

telegram_registration() {
	secret_value mautrix-telegram.sops.yaml matrix mautrix-telegram-registration '.stringData."registration.yaml"'
}

pg_password() {
	secret_value postgres-roles.sops.yaml postgres "pg-$1" '.stringData.password'
}

exercise_db_role() { # exercise_db_role <role> <expected changed files...>
	local role="$1"
	local old_synapse old_mas old_bridge old_kagent new_value
	shift
	old_synapse="$(pg_password synapse)"
	old_mas="$(pg_password mas)"
	old_bridge="$(pg_password bridge)"
	old_kagent="$(pg_password kagent)"
	"${ROTATOR}" fixture.localhost local "db-${role}" >/dev/null
	new_value="$(pg_password "${role}")"
	case "${role}" in
	synapse)
		assert_changed "${old_synapse}" "${new_value}" "Synapse database password"
		assert_equal "${old_mas}" "$(pg_password mas)" "MAS changed during Synapse rotation"
		assert_equal "${old_bridge}" "$(pg_password bridge)" "bridge changed during Synapse rotation"
		assert_equal "${old_kagent}" "$(pg_password kagent)" "kagent changed during Synapse rotation"
		;;
	mas)
		assert_changed "${old_mas}" "${new_value}" "MAS database password"
		assert_equal "${old_synapse}" "$(pg_password synapse)" "Synapse changed during MAS rotation"
		assert_equal "${old_bridge}" "$(pg_password bridge)" "bridge changed during MAS rotation"
		assert_equal "${old_kagent}" "$(pg_password kagent)" "kagent changed during MAS rotation"
		;;
	kagent)
		assert_changed "${old_kagent}" "${new_value}" "kagent database password"
		assert_equal "${old_synapse}" "$(pg_password synapse)" "Synapse changed during kagent rotation"
		assert_equal "${old_mas}" "$(pg_password mas)" "MAS changed during kagent rotation"
		assert_equal "${old_bridge}" "$(pg_password bridge)" "bridge changed during kagent rotation"
		;;
	*) fail "unsupported database fixture role: ${role}" ;;
	esac
	assert_changed_files "$@"
	commit_fixture "rotate ${role} database fixture"
}

provider_key() {
	secret_value agentgateway-openai.sops.yaml agentgateway-system openai-secret '.stringData.Authorization // (.data.Authorization | @base64d)'
}

assert_secret_inventory() {
	local actual expected
	actual="$(yq -r '.resources[]' "${FIXTURE_ROOT}/clusters/local/secrets/kustomization.yaml" | sort)"
	expected="$({
		for file in "${FIXTURE_ROOT}"/clusters/local/secrets/*.sops.yaml; do
			basename "${file}"
		done
	} | sort)"
	assert_equal "${actual}" "${expected}" "generated Secret inventory is incomplete"
	kubectl kustomize "${FIXTURE_ROOT}/clusters/local/secrets" >/dev/null
}

exercise_provider_generation() { # exercise_provider_generation <provider> <env> <file> <secret>
	local provider="$1"
	local key_env="$2"
	local file="$3"
	local secret="$4"
	local value="fixture-${provider}-key-000000000000000000000000"
	local before

	yq -i ".data.llm_provider = \"${provider}\"" "${FIXTURE_ROOT}/clusters/local/platform-settings.yaml"
	before="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/kustomization.yaml" | awk '{print $1}')"
	expect_failure env -u "${key_env}" FGENTIC_SECRET_SET=provider \
		"${GENERATOR}" fixture.localhost local
	[ ! -e "${FIXTURE_ROOT}/clusters/local/secrets/${file}" ] || fail "failed ${provider} preflight emitted ciphertext"
	assert_equal "${before}" \
		"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/kustomization.yaml" | awk '{print $1}')" \
		"failed ${provider} preflight changed the Secret inventory"

	env "${key_env}=${value}" FGENTIC_SECRET_SET=provider \
		"${GENERATOR}" fixture.localhost local >/dev/null
	assert_equal "${value}" \
		"$(secret_value "${file}" agentgateway-system "${secret}" '.data.Authorization | @base64d')" \
		"${provider} Secret does not contain the supplied key"
	assert_secret_inventory
}

age-keygen -o "${WORK_DIR}/age.key" >/dev/null 2>&1
RECIPIENT="$(age-keygen -y "${WORK_DIR}/age.key")"
printf 'creation_rules:\n  - path_regex: \\.sops\\.ya?ml$\n    encrypted_regex: ^(data|stringData)$\n    age: %s\n' \
	"${RECIPIENT}" >"${FIXTURE_ROOT}/.sops.yaml"
printf '%s\n' \
	'apiVersion: v1' \
	'kind: ConfigMap' \
	'metadata:' \
	'  name: platform-settings' \
	'data:' \
	'  llm_provider: vertex' \
	>"${FIXTURE_ROOT}/clusters/local/platform-settings.yaml"

export SOPS_AGE_KEY_FILE="${WORK_DIR}/age.key"
export FGENTIC_DATA_ROOT="${FIXTURE_ROOT}"

"${GENERATOR}" fixture.localhost local >/dev/null
FGENTIC_SECRET_SET=slack "${GENERATOR}" fixture.localhost local >/dev/null
# Optional Telegram generation fails before emitting a file unless the operator supplies the exact
# upstream API application pair. Core generation above neither required nor produced this set.
[ ! -e "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-telegram.sops.yaml" ] || fail "core generation emitted Telegram secrets"
expect_failure env -u TELEGRAM_API_ID -u TELEGRAM_API_HASH \
	FGENTIC_SECRET_SET=telegram "${GENERATOR}" fixture.localhost local
[ ! -e "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-telegram.sops.yaml" ] || fail "failed Telegram preflight emitted ciphertext"
TELEGRAM_API_ID=123456 TELEGRAM_API_HASH=00000000000000000000000000000000 \
	FGENTIC_SECRET_SET=telegram "${GENERATOR}" fixture.localhost local >/dev/null
assert_secret_inventory
git -C "${FIXTURE_ROOT}" init -q
git -C "${FIXTURE_ROOT}" config user.name rotation-fixture
git -C "${FIXTURE_ROOT}" config user.email rotation-fixture@example.invalid
commit_fixture "initial encrypted fixture"

# Every API profile must fail before writing without its own key, then emit a namespace-local
# ciphertext file that the generated Kustomization actually reconciles.
exercise_provider_generation mistral MISTRAL_API_KEY agentgateway-mistral.sops.yaml mistral-secret
commit_fixture "add Mistral provider fixture"
exercise_provider_generation anthropic ANTHROPIC_API_KEY agentgateway-anthropic.sops.yaml anthropic-secret
commit_fixture "add Anthropic provider fixture"
exercise_provider_generation openai OPENAI_API_KEY agentgateway-openai.sops.yaml openai-secret
commit_fixture "add OpenAI provider fixture"
exercise_provider_generation azure-openai AZURE_OPENAI_API_KEY agentgateway-azure-openai.sops.yaml azure-openai-secret
commit_fixture "add Azure OpenAI provider fixture"
yq -i '.data.llm_provider = "openai"' "${FIXTURE_ROOT}/clusters/local/platform-settings.yaml"
commit_fixture "restore OpenAI provider selection"

# Every precondition fails before staging or changing ciphertext.
expect_failure "${ROTATOR}" fixture.localhost local unsupported
expect_failure env -u OPENAI_API_KEY "${ROTATOR}" fixture.localhost local provider
[ -z "$(git -C "${FIXTURE_ROOT}" status --porcelain)" ] || fail "preflight failure changed the fixture"

# Appservice rehearsal: old/old works, a mixed restart is rejected in both token directions, and
# the two new namespace copies converge before any tracked file is replaced.
OLD_BRIDGE_REGISTRATION="$(registration bridge)"
OLD_AS_TOKEN="$(yq -r '.as_token' <<<"${OLD_BRIDGE_REGISTRATION}")"
OLD_HS_TOKEN="$(yq -r '.hs_token' <<<"${OLD_BRIDGE_REGISTRATION}")"
APP_START_NS="$(date +%s%N)"
"${ROTATOR}" fixture.localhost local appservice >/dev/null
APP_END_NS="$(date +%s%N)"
NEW_BRIDGE_REGISTRATION="$(registration bridge)"
NEW_MATRIX_REGISTRATION="$(registration matrix)"
NEW_AS_TOKEN="$(yq -r '.as_token' <<<"${NEW_BRIDGE_REGISTRATION}")"
NEW_HS_TOKEN="$(yq -r '.hs_token' <<<"${NEW_BRIDGE_REGISTRATION}")"
assert_equal "${NEW_BRIDGE_REGISTRATION}" "${NEW_MATRIX_REGISTRATION}" "new appservice copies differ"
assert_changed "${OLD_AS_TOKEN}" "${NEW_AS_TOKEN}" "appservice as_token"
assert_changed "${OLD_HS_TOKEN}" "${NEW_HS_TOKEN}" "appservice hs_token"
# These mismatches model the two bearer-token checks during a mixed restart. Reloading both
# consumers from the equal new copies restores both directions.
[ "${OLD_AS_TOKEN}" != "${NEW_AS_TOKEN}" ] || fail "mixed bridge-to-Synapse credentials were accepted"
[ "${OLD_HS_TOKEN}" != "${NEW_HS_TOKEN}" ] || fail "mixed Synapse-to-bridge credentials were accepted"
assert_changed_files matrix-a2a-bridge-registration.sops.yaml
commit_fixture "rotate appservice fixture"

# The optional Slack set keeps its role password, runtime tokens, and Synapse registration in one
# ciphertext transaction. Slack app/workspace tokens are runtime login state and never appear here.
OLD_SLACK_PASSWORD="$(secret_value mautrix-slack.sops.yaml postgres pg-slackbridge '.stringData.password')"
OLD_SLACK_AS="$(yq -r '.as_token' <<<"$(slack_registration)")"
OLD_SLACK_HS="$(yq -r '.hs_token' <<<"$(slack_registration)")"
OLD_SLACK_SENDER="$(yq -r '.sender_localpart' <<<"$(slack_registration)")"
"${ROTATOR}" fixture.localhost local slack >/dev/null
NEW_SLACK_PASSWORD="$(secret_value mautrix-slack.sops.yaml postgres pg-slackbridge '.stringData.password')"
NEW_SLACK_AS="$(secret_value mautrix-slack.sops.yaml bridges mautrix-slack '.stringData."as-token"')"
NEW_SLACK_HS="$(secret_value mautrix-slack.sops.yaml bridges mautrix-slack '.stringData."hs-token"')"
assert_changed "${OLD_SLACK_PASSWORD}" "${NEW_SLACK_PASSWORD}" "Slack database password"
assert_changed "${OLD_SLACK_AS}" "${NEW_SLACK_AS}" "Slack as_token"
assert_changed "${OLD_SLACK_HS}" "${NEW_SLACK_HS}" "Slack hs_token"
assert_equal "${NEW_SLACK_AS}" "$(yq -r '.as_token' <<<"$(slack_registration)")" "Slack as_token copies differ"
assert_equal "${NEW_SLACK_HS}" "$(yq -r '.hs_token' <<<"$(slack_registration)")" "Slack hs_token copies differ"
assert_equal "${OLD_SLACK_SENDER}" "$(yq -r '.sender_localpart' <<<"$(slack_registration)")" "Slack sender changed during rotation"
assert_equal \
	"$(secret_value mautrix-slack.sops.yaml bridges mautrix-slack '.stringData."database-uri"')" \
	"postgres://slackbridge:${NEW_SLACK_PASSWORD}@platform-pg-rw.postgres.svc.cluster.local:5432/slackbridge?sslmode=require" \
	"Slack database URI drifted"
assert_changed_files mautrix-slack.sops.yaml
commit_fixture "rotate Slack fixture"

OLD_TELEGRAM_PASSWORD="$(secret_value mautrix-telegram.sops.yaml postgres pg-telegrambridge '.stringData.password')"
OLD_TELEGRAM_AS="$(yq -r '.as_token' <<<"$(telegram_registration)")"
OLD_TELEGRAM_HS="$(yq -r '.hs_token' <<<"$(telegram_registration)")"
OLD_TELEGRAM_SENDER="$(yq -r '.sender_localpart' <<<"$(telegram_registration)")"
OLD_TELEGRAM_API_ID="$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."api-id"')"
OLD_TELEGRAM_API_HASH="$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."api-hash"')"
"${ROTATOR}" fixture.localhost local telegram >/dev/null
NEW_TELEGRAM_PASSWORD="$(secret_value mautrix-telegram.sops.yaml postgres pg-telegrambridge '.stringData.password')"
NEW_TELEGRAM_AS="$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."as-token"')"
NEW_TELEGRAM_HS="$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."hs-token"')"
assert_changed "${OLD_TELEGRAM_PASSWORD}" "${NEW_TELEGRAM_PASSWORD}" "Telegram database password"
assert_changed "${OLD_TELEGRAM_AS}" "${NEW_TELEGRAM_AS}" "Telegram as_token"
assert_changed "${OLD_TELEGRAM_HS}" "${NEW_TELEGRAM_HS}" "Telegram hs_token"
assert_equal "${NEW_TELEGRAM_AS}" "$(yq -r '.as_token' <<<"$(telegram_registration)")" "Telegram as_token copies differ"
assert_equal "${NEW_TELEGRAM_HS}" "$(yq -r '.hs_token' <<<"$(telegram_registration)")" "Telegram hs_token copies differ"
assert_equal "${OLD_TELEGRAM_SENDER}" "$(yq -r '.sender_localpart' <<<"$(telegram_registration)")" "Telegram sender changed during rotation"
assert_equal "${OLD_TELEGRAM_API_ID}" "$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."api-id"')" "Telegram API ID changed"
assert_equal "${OLD_TELEGRAM_API_HASH}" "$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."api-hash"')" "Telegram API hash changed"
assert_equal \
	"$(secret_value mautrix-telegram.sops.yaml bridges mautrix-telegram '.stringData."database-uri"')" \
	"postgres://telegrambridge:${NEW_TELEGRAM_PASSWORD}@platform-pg-rw.postgres.svc.cluster.local:5432/telegrambridge?sslmode=require" \
	"Telegram database URI drifted"
assert_changed_files mautrix-telegram.sops.yaml
commit_fixture "rotate Telegram fixture"

OLD_A2A="$(secret_value a2a-authorization.sops.yaml bridge a2a-bridge-credential '.stringData.token')"
"${ROTATOR}" fixture.localhost local a2a >/dev/null
NEW_A2A="$(secret_value a2a-authorization.sops.yaml bridge a2a-bridge-credential '.stringData.token')"
GATEWAY_A2A="$(secret_value a2a-authorization.sops.yaml agentgateway-system a2a-bridge-callers '.stringData."matrix-a2a-bridge" | from_json | .key')"
assert_changed "${OLD_A2A}" "${NEW_A2A}" "A2A workload key"
assert_equal "${NEW_A2A}" "${GATEWAY_A2A}" "A2A workload copies differ"
assert_changed_files a2a-authorization.sops.yaml
commit_fixture "rotate A2A fixture"

OLD_MCP="$(secret_value mcp-authorization.sops.yaml kagent platform-helper-mcp-credential '.stringData.authorization')"
"${ROTATOR}" fixture.localhost local mcp >/dev/null
NEW_MCP="$(secret_value mcp-authorization.sops.yaml kagent platform-helper-mcp-credential '.stringData.authorization')"
GATEWAY_MCP="$(secret_value mcp-authorization.sops.yaml agentgateway-system mcp-agent-callers '.stringData."platform-helper" | from_json | .key')"
assert_changed "${OLD_MCP}" "${NEW_MCP}" "platform-helper MCP credential"
assert_equal "${NEW_MCP}" "Bearer ${GATEWAY_MCP}" "MCP credential copies differ"
assert_changed_files mcp-authorization.sops.yaml
commit_fixture "rotate MCP fixture"

OLD_SYNAPSE="$(pg_password synapse)"
OLD_MAS="$(pg_password mas)"
OLD_BRIDGE="$(pg_password bridge)"
OLD_KAGENT="$(pg_password kagent)"
"${ROTATOR}" fixture.localhost local db-bridge >/dev/null
NEW_BRIDGE="$(pg_password bridge)"
assert_changed "${OLD_BRIDGE}" "${NEW_BRIDGE}" "bridge database password"
assert_equal "${OLD_SYNAPSE}" "$(pg_password synapse)" "Synapse password changed during bridge-only rotation"
assert_equal "${OLD_MAS}" "$(pg_password mas)" "MAS password changed during bridge-only rotation"
assert_equal "${OLD_KAGENT}" "$(pg_password kagent)" "kagent password changed during bridge-only rotation"
assert_equal \
	"$(secret_value matrix-a2a-bridge-db.sops.yaml bridge matrix-a2a-bridge-db '.stringData.url')" \
	"postgres://bridge:${NEW_BRIDGE}@platform-pg-rw.postgres.svc.cluster.local:5432/bridge?sslmode=require" \
	"bridge database URL drifted"
assert_changed_files matrix-a2a-bridge-db.sops.yaml postgres-roles.sops.yaml
commit_fixture "rotate bridge database fixture"

exercise_db_role synapse postgres-roles.sops.yaml
exercise_db_role mas postgres-roles.sops.yaml
exercise_db_role kagent kagent.sops.yaml postgres-roles.sops.yaml

OLD_PROVIDER="$(provider_key)"
OPENAI_API_KEY="fixture-provider-rotated-1111111111111111"
export OPENAI_API_KEY
"${ROTATOR}" fixture.localhost local provider >/dev/null
assert_changed "${OLD_PROVIDER}" "$(provider_key)" "provider key"
assert_equal "${OPENAI_API_KEY}" "$(provider_key)" "provider key does not match supplied fixture value"
assert_changed_files agentgateway-openai.sops.yaml
commit_fixture "rotate provider fixture"

OLD_KEYCLOAK_DB="$(secret_value keycloak-db.sops.yaml postgres pg-keycloak '.stringData.password')"
"${ROTATOR}" fixture.localhost local keycloak-db >/dev/null
NEW_KEYCLOAK_DB="$(secret_value keycloak-db.sops.yaml postgres pg-keycloak '.stringData.password')"
assert_changed "${OLD_KEYCLOAK_DB}" "${NEW_KEYCLOAK_DB}" "Keycloak database password"
assert_equal \
	"${NEW_KEYCLOAK_DB}" \
	"$(secret_value keycloak-db.sops.yaml keycloak pg-keycloak '.stringData.password')" \
	"Keycloak database copies differ"
assert_changed_files keycloak-db.sops.yaml
commit_fixture "rotate Keycloak database fixture"

OLD_CLIENT="$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.FGENTIC_CLIENT_SECRET')"
OLD_ADMIN="$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.KC_BOOTSTRAP_ADMIN_PASSWORD')"
OLD_ALICE="$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.FGENTIC_ALICE_PASSWORD')"
OLD_BOB="$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.FGENTIC_BOB_PASSWORD')"
expect_failure "${ROTATOR}" fixture.localhost local keycloak-client
NEW_CLIENT="fixture-keycloak-client-22222222222222222222"
KEYCLOAK_CLIENT_UPDATED=yes FGENTIC_CLIENT_SECRET="${NEW_CLIENT}" \
	"${ROTATOR}" fixture.localhost local keycloak-client >/dev/null
assert_changed "${OLD_CLIENT}" \
	"$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.FGENTIC_CLIENT_SECRET')" \
	"Keycloak client secret"
assert_equal "${NEW_CLIENT}" \
	"$(secret_value keycloak-bootstrap.sops.yaml matrix mas-upstream-oidc '.stringData."provider.yaml" | from_yaml | .upstream_oauth2.providers[0].client_secret')" \
	"MAS client secret differs from Keycloak"
assert_equal "${OLD_ADMIN}" "$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.KC_BOOTSTRAP_ADMIN_PASSWORD')" "bootstrap admin changed"
assert_equal "${OLD_ALICE}" "$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.FGENTIC_ALICE_PASSWORD')" "Alice changed"
assert_equal "${OLD_BOB}" "$(secret_value keycloak-bootstrap.sops.yaml keycloak keycloak-credentials '.stringData.FGENTIC_BOB_PASSWORD')" "Bob changed"
assert_changed_files keycloak-bootstrap.sops.yaml
commit_fixture "rotate Keycloak client fixture"

# Full automatable rotation changes every operational class but leaves the bootstrap-only realm
# identities/client file byte-for-byte untouched.
BOOTSTRAP_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/keycloak-bootstrap.sops.yaml" | awk '{print $1}')"
SLACK_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-slack.sops.yaml" | awk '{print $1}')"
TELEGRAM_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-telegram.sops.yaml" | awk '{print $1}')"
BEFORE_ALL_AS="$(yq -r '.as_token' <<<"$(registration bridge)")"
BEFORE_ALL_A2A="$(secret_value a2a-authorization.sops.yaml bridge a2a-bridge-credential '.stringData.token')"
BEFORE_ALL_MCP="$(secret_value mcp-authorization.sops.yaml kagent platform-helper-mcp-credential '.stringData.authorization')"
BEFORE_ALL_SYNAPSE="$(pg_password synapse)"
BEFORE_ALL_MAS="$(pg_password mas)"
BEFORE_ALL_BRIDGE="$(pg_password bridge)"
BEFORE_ALL_KAGENT="$(pg_password kagent)"
BEFORE_ALL_KEYCLOAK="$(secret_value keycloak-db.sops.yaml postgres pg-keycloak '.stringData.password')"
BEFORE_ALL_PROVIDER="$(provider_key)"
OPENAI_API_KEY="fixture-provider-full-3333333333333333333"
export OPENAI_API_KEY
"${ROTATOR}" fixture.localhost local all >/dev/null
assert_equal "${BOOTSTRAP_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/keycloak-bootstrap.sops.yaml" | awk '{print $1}')" \
	"full rotation changed bootstrap-only identities"
assert_equal "${SLACK_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-slack.sops.yaml" | awk '{print $1}')" \
	"core full rotation changed the optional Slack set"
assert_equal "${TELEGRAM_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-telegram.sops.yaml" | awk '{print $1}')" \
	"core full rotation changed the optional Telegram set"
assert_changed "${BEFORE_ALL_AS}" "$(yq -r '.as_token' <<<"$(registration bridge)")" "full appservice token"
assert_changed "${BEFORE_ALL_A2A}" "$(secret_value a2a-authorization.sops.yaml bridge a2a-bridge-credential '.stringData.token')" "full A2A key"
assert_changed "${BEFORE_ALL_MCP}" "$(secret_value mcp-authorization.sops.yaml kagent platform-helper-mcp-credential '.stringData.authorization')" "full MCP key"
assert_changed "${BEFORE_ALL_SYNAPSE}" "$(pg_password synapse)" "full Synapse DB password"
assert_changed "${BEFORE_ALL_MAS}" "$(pg_password mas)" "full MAS DB password"
assert_changed "${BEFORE_ALL_BRIDGE}" "$(pg_password bridge)" "full bridge DB password"
assert_changed "${BEFORE_ALL_KAGENT}" "$(pg_password kagent)" "full kagent DB password"
assert_changed "${BEFORE_ALL_KEYCLOAK}" "$(secret_value keycloak-db.sops.yaml postgres pg-keycloak '.stringData.password')" "full Keycloak DB password"
assert_changed "${BEFORE_ALL_PROVIDER}" "$(provider_key)" "full provider key"
assert_changed_files \
	a2a-authorization.sops.yaml \
	agentgateway-openai.sops.yaml \
	kagent.sops.yaml \
	keycloak-db.sops.yaml \
	matrix-a2a-bridge-db.sops.yaml \
	mcp-authorization.sops.yaml \
	matrix-a2a-bridge-registration.sops.yaml \
	postgres-roles.sops.yaml

# A second rotation must refuse the dirty target rather than overwrite an unreviewed ciphertext
# diff. The existing full-rotation diff remains unchanged.
DIRTY_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/matrix-a2a-bridge-registration.sops.yaml" | awk '{print $1}')"
expect_failure "${ROTATOR}" fixture.localhost local appservice
assert_equal "${DIRTY_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/matrix-a2a-bridge-registration.sops.yaml" | awk '{print $1}')" \
	"dirty-file preflight overwrote ciphertext"

# The legacy full-generation compatibility flag still must not rewrite bootstrap-only identities.
GEN_BOOTSTRAP_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/keycloak-bootstrap.sops.yaml" | awk '{print $1}')"
GEN_SLACK_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-slack.sops.yaml" | awk '{print $1}')"
GEN_TELEGRAM_HASH="$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-telegram.sops.yaml" | awk '{print $1}')"
"${GENERATOR}" fixture.localhost local --force >/dev/null
assert_equal "${GEN_BOOTSTRAP_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/keycloak-bootstrap.sops.yaml" | awk '{print $1}')" \
	"gen-secrets.sh --force changed bootstrap-only identities"
assert_equal "${GEN_SLACK_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-slack.sops.yaml" | awk '{print $1}')" \
	"gen-secrets.sh --force changed the optional Slack set"
assert_equal "${GEN_TELEGRAM_HASH}" \
	"$(sha256sum "${FIXTURE_ROOT}/clusters/local/secrets/mautrix-telegram.sops.yaml" | awk '{print $1}')" \
	"gen-secrets.sh --force changed the optional Telegram set"

for file in "${FIXTURE_ROOT}"/clusters/local/secrets/*.sops.yaml; do
	sops --decrypt "${file}" >/dev/null
done

APP_FIXTURE_MS="$(((APP_END_NS - APP_START_NS) / 1000000))"
printf 'Secret rotation fixture passed; appservice ciphertext transition took %d ms (not service downtime).\n' "${APP_FIXTURE_MS}"
