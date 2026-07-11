#!/usr/bin/env bash
# Selectively rotate one coherent SOPS secret class. The script changes ciphertext only; Flux
# reconciliation and consumer restarts remain explicit operator actions so review is possible
# before any running system changes.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DATA_ROOT="${FGENTIC_DATA_ROOT:-${REPO_ROOT}}"
SOPS_CONFIG="${FGENTIC_SOPS_CONFIG:-${DATA_ROOT}/.sops.yaml}"
# shellcheck source=scripts/secrets-common.sh
source "${SCRIPT_DIR}/secrets-common.sh"

usage() {
	cat >&2 <<'EOF'
usage: scripts/rotate-secrets.sh <server_name> <local|gcp> <secret-set>

Supported secret sets:
  appservice       Matrix appservice as_token + hs_token (both namespace copies)
  a2a              Bridge workload credential (agentgateway + bridge copies)
  mcp              platform-helper MCP credential (agentgateway + kagent copies)
  db-synapse       Synapse database role and both namespace copies
  db-mas           MAS database role and both namespace copies
  db-bridge        Bridge database role + derived connection URL
  db-kagent        kagent database role + derived connection URL
  db-core          All four core database roles + derived connection URLs
  provider         Selected API provider key; unsupported for ambient Vertex/self-hosted vLLM
  keycloak-db      Keycloak database role and both namespace copies
  slack            Optional mautrix-slack DB password + appservice registration tokens
  telegram         Optional mautrix-telegram DB password + appservice tokens; preserves API pair
  keycloak-client  OIDC client secret only; requires the live Keycloak client to be changed first
  all              Core automatable sets; excludes optional Slack/Telegram, keycloak-client, and bootstrap

Provider rotation reads the selected profile's key from MISTRAL_API_KEY, ANTHROPIC_API_KEY,
OPENAI_API_KEY, or AZURE_OPENAI_API_KEY. keycloak-client requires FGENTIC_CLIENT_SECRET and
KEYCLOAK_CLIENT_UPDATED=yes. No set rotates the Keycloak admin or demo-user passwords.
EOF
}

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
	usage
	exit 0
fi
if [ "$#" -ne 3 ]; then
	usage
	exit 2
fi

SERVER_NAME="$1"
ENV="$2"
SECRET_SET="$3"
validate_server_name "${SERVER_NAME}"
validate_secret_environment "${ENV}"

for command in git mktemp openssl sops yq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 1
	fi
done

PLATFORM_SETTINGS="${DATA_ROOT}/clusters/${ENV}/platform-settings.yaml"
SECRETS_DIR="${DATA_ROOT}/clusters/${ENV}/secrets"
if [ ! -f "${PLATFORM_SETTINGS}" ]; then
	echo "error: platform settings not found: ${PLATFORM_SETTINGS}" >&2
	exit 1
fi
if [ ! -d "${SECRETS_DIR}" ]; then
	echo "error: secrets directory not found: ${SECRETS_DIR}; run gen-secrets.sh first" >&2
	exit 1
fi
if [ ! -f "${SOPS_CONFIG}" ]; then
	echo "error: SOPS configuration not found: ${SOPS_CONFIG}" >&2
	exit 1
fi

declare -a TARGET_FILES=()
GEN_SET=""
DB_MODE=""
LLM_PROVIDER=""
MODEL_SECRET_ENV=""
MODEL_SECRET_FILE=""
MODEL_SECRET_NAME=""

resolve_provider_target() {
	LLM_PROVIDER="$(yq -er '.data.llm_provider' "${PLATFORM_SETTINGS}")"
	resolve_model_secret "${LLM_PROVIDER}" || {
		echo "error: invalid provider in ${PLATFORM_SETTINGS}" >&2
		exit 1
	}
	if [ -z "${MODEL_SECRET_FILE}" ]; then
		echo "error: llm_provider=${LLM_PROVIDER} has no Git-tracked API key to rotate" >&2
		exit 1
	fi
	if [ -z "${!MODEL_SECRET_ENV:-}" ]; then
		echo "error: ${MODEL_SECRET_ENV} is required for llm_provider=${LLM_PROVIDER}" >&2
		exit 1
	fi
}

case "${SECRET_SET}" in
appservice)
	GEN_SET="appservice"
	TARGET_FILES=(matrix-a2a-bridge-registration.sops.yaml)
	;;
a2a)
	GEN_SET="a2a"
	TARGET_FILES=(a2a-authorization.sops.yaml)
	;;
mcp)
	GEN_SET="mcp"
	TARGET_FILES=(mcp-authorization.sops.yaml)
	;;
db-synapse)
	GEN_SET="db-core"
	DB_MODE="synapse"
	TARGET_FILES=(postgres-roles.sops.yaml)
	;;
db-mas)
	GEN_SET="db-core"
	DB_MODE="mas"
	TARGET_FILES=(postgres-roles.sops.yaml)
	;;
db-bridge)
	GEN_SET="db-core"
	DB_MODE="bridge"
	TARGET_FILES=(postgres-roles.sops.yaml matrix-a2a-bridge-db.sops.yaml)
	;;
db-kagent)
	GEN_SET="db-core"
	DB_MODE="kagent"
	TARGET_FILES=(postgres-roles.sops.yaml kagent.sops.yaml)
	;;
db-core)
	GEN_SET="db-core"
	DB_MODE="all"
	TARGET_FILES=(postgres-roles.sops.yaml matrix-a2a-bridge-db.sops.yaml kagent.sops.yaml)
	;;
provider)
	resolve_provider_target
	GEN_SET="provider"
	TARGET_FILES=("${MODEL_SECRET_FILE}")
	;;
keycloak-db)
	GEN_SET="keycloak-db"
	TARGET_FILES=(keycloak-db.sops.yaml)
	;;
slack)
	GEN_SET="slack"
	TARGET_FILES=(mautrix-slack.sops.yaml)
	;;
telegram)
	GEN_SET="telegram"
	TARGET_FILES=(mautrix-telegram.sops.yaml)
	;;
keycloak-client)
	if [ "${KEYCLOAK_CLIENT_UPDATED:-}" != "yes" ]; then
		echo "error: rotate the live Keycloak fgentic client first, then set KEYCLOAK_CLIENT_UPDATED=yes" >&2
		exit 1
	fi
	FGENTIC_CLIENT_SECRET="${FGENTIC_CLIENT_SECRET:-}"
	if [ "${#FGENTIC_CLIENT_SECRET}" -lt 32 ] || [[ "${FGENTIC_CLIENT_SECRET}" == *$'\n'* ]]; then
		echo "error: FGENTIC_CLIENT_SECRET must be a single-line secret of at least 32 characters" >&2
		exit 1
	fi
	TARGET_FILES=(keycloak-bootstrap.sops.yaml)
	;;
all)
	GEN_SET="rotatable"
	DB_MODE="all"
	TARGET_FILES=(
		postgres-roles.sops.yaml
		matrix-a2a-bridge-db.sops.yaml
		kagent.sops.yaml
		matrix-a2a-bridge-registration.sops.yaml
		a2a-authorization.sops.yaml
		mcp-authorization.sops.yaml
		keycloak-db.sops.yaml
	)
	LLM_PROVIDER="$(yq -er '.data.llm_provider' "${PLATFORM_SETTINGS}")"
	resolve_model_secret "${LLM_PROVIDER}" || {
		echo "error: invalid provider in ${PLATFORM_SETTINGS}" >&2
		exit 1
	}
	if [ -n "${MODEL_SECRET_FILE}" ]; then
		if [ -z "${!MODEL_SECRET_ENV:-}" ]; then
			echo "error: ${MODEL_SECRET_ENV} is required for llm_provider=${LLM_PROVIDER}" >&2
			exit 1
		fi
		TARGET_FILES+=("${MODEL_SECRET_FILE}")
	fi
	;;
*)
	echo "error: unsupported secret-set: ${SECRET_SET}" >&2
	usage
	exit 2
	;;
esac

decrypt() {
	sops --decrypt "$1"
}

secret_value() { # secret_value <file> <namespace> <name> <yq suffix>
	local file="$1"
	local namespace="$2"
	local name="$3"
	local suffix="$4"
	decrypt "${file}" |
		yq eval-all -er "select(.metadata.namespace == \"${namespace}\" and .metadata.name == \"${name}\") | ${suffix}" -
}

assert_equal() {
	if [ "$1" != "$2" ]; then
		echo "error: staged invariant failed: $3" >&2
		exit 1
	fi
}

assert_changed() {
	if [ "$1" = "$2" ]; then
		echo "error: staged rotation did not change $3" >&2
		exit 1
	fi
}

declare -a TARGET_PATHS=()
for file in "${TARGET_FILES[@]}"; do
	target="${SECRETS_DIR}/${file}"
	if [ ! -f "${target}" ]; then
		echo "error: required encrypted file not found: ${target}; run gen-secrets.sh first" >&2
		exit 1
	fi
	if ! decrypt "${target}" >/dev/null; then
		echo "error: cannot decrypt ${target}; no files were changed" >&2
		exit 1
	fi
	TARGET_PATHS+=("${target}")
done

# Never overwrite an uncommitted manual edit. This check is deliberately before random generation
# and staging so a failed precondition leaves no noisy ciphertext diff.
if git -C "${DATA_ROOT}" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	GIT_ROOT="$(git -C "${DATA_ROOT}" rev-parse --show-toplevel)"
	for target in "${TARGET_PATHS[@]}"; do
		if [[ "${target}" == "${GIT_ROOT}/"* ]]; then
			relative="${target#"${GIT_ROOT}/"}"
			git_status="$(git -C "${GIT_ROOT}" status --porcelain -- "${relative}")"
			if [ -n "${git_status}" ]; then
				echo "error: refusing to overwrite dirty secret file: ${relative}" >&2
				exit 1
			fi
		fi
	done
fi

# Read current database values into memory so a single-role rotation preserves every unrelated
# role. The generator then rebuilds the coherent namespace copies and derived URLs from one set of
# values instead of duplicating YAML mutation logic here.
if [ -n "${DB_MODE}" ]; then
	PG_ROLES_FILE="${SECRETS_DIR}/postgres-roles.sops.yaml"
	PG_SYNAPSE="$(secret_value "${PG_ROLES_FILE}" postgres pg-synapse '.stringData.password')"
	PG_MAS="$(secret_value "${PG_ROLES_FILE}" postgres pg-mas '.stringData.password')"
	PG_BRIDGE="$(secret_value "${PG_ROLES_FILE}" postgres pg-bridge '.stringData.password')"
	PG_KAGENT="$(secret_value "${PG_ROLES_FILE}" postgres pg-kagent '.stringData.password')"
	OLD_PG_SYNAPSE="${PG_SYNAPSE}"
	OLD_PG_MAS="${PG_MAS}"
	OLD_PG_BRIDGE="${PG_BRIDGE}"
	OLD_PG_KAGENT="${PG_KAGENT}"
	case "${DB_MODE}" in
	synapse) PG_SYNAPSE="$(openssl rand -hex 24)" ;;
	mas) PG_MAS="$(openssl rand -hex 24)" ;;
	bridge) PG_BRIDGE="$(openssl rand -hex 24)" ;;
	kagent) PG_KAGENT="$(openssl rand -hex 24)" ;;
	all)
		PG_SYNAPSE="$(openssl rand -hex 24)"
		PG_MAS="$(openssl rand -hex 24)"
		PG_BRIDGE="$(openssl rand -hex 24)"
		PG_KAGENT="$(openssl rand -hex 24)"
		;;
	*)
		echo "error: invalid database rotation mode: ${DB_MODE}" >&2
		exit 1
		;;
	esac
	export PG_SYNAPSE PG_MAS PG_BRIDGE PG_KAGENT
fi

# Registration sender localparts are generated once and become stable appservice identities.
# Optional-network rotation changes only the scoped DB password and AS/HS tokens. Telegram's API
# application pair is operator-owned and must also survive byte-for-byte.
if [ "${SECRET_SET}" = "slack" ]; then
	SLACK_SENDER_LOCALPART="$(
		secret_value "${SECRETS_DIR}/mautrix-slack.sops.yaml" matrix mautrix-slack-registration \
			'.stringData."registration.yaml" | from_yaml | .sender_localpart'
	)"
	export SLACK_SENDER_LOCALPART
elif [ "${SECRET_SET}" = "telegram" ]; then
	TELEGRAM_API_ID="$(secret_value "${SECRETS_DIR}/mautrix-telegram.sops.yaml" bridges mautrix-telegram '.stringData."api-id"')"
	TELEGRAM_API_HASH="$(secret_value "${SECRETS_DIR}/mautrix-telegram.sops.yaml" bridges mautrix-telegram '.stringData."api-hash"')"
	TELEGRAM_SENDER_LOCALPART="$(
		secret_value "${SECRETS_DIR}/mautrix-telegram.sops.yaml" matrix mautrix-telegram-registration \
			'.stringData."registration.yaml" | from_yaml | .sender_localpart'
	)"
	export TELEGRAM_API_ID TELEGRAM_API_HASH TELEGRAM_SENDER_LOCALPART
fi

STAGE_DIR="$(mktemp -d "${SECRETS_DIR}/.rotation-${SECRET_SET}.XXXXXX")"
BACKUP_DIR="${STAGE_DIR}/backup"
mkdir -p "${BACKUP_DIR}"
COMMITTED=false

cleanup() {
	if [ "${COMMITTED}" != "true" ]; then
		for target in "${TARGET_PATHS[@]}"; do
			backup="${BACKUP_DIR}/$(basename "${target}")"
			if [ -f "${backup}" ]; then
				cp -p "${backup}" "${target}"
			fi
		done
	fi
	rm -rf "${STAGE_DIR}"
}
trap cleanup EXIT INT TERM

if [ "${SECRET_SET}" = "keycloak-client" ]; then
	target="${SECRETS_DIR}/keycloak-bootstrap.sops.yaml"
	stage="${STAGE_DIR}/keycloak-bootstrap.sops.yaml"
	export FGENTIC_CLIENT_SECRET
	decrypt "${target}" |
		yq eval-all '
      (. | select(.metadata.name == "keycloak-credentials" and .metadata.namespace == "keycloak") | .stringData.FGENTIC_CLIENT_SECRET) = strenv(FGENTIC_CLIENT_SECRET)
      | (. | select(.metadata.name == "mas-upstream-oidc" and .metadata.namespace == "matrix") | .stringData."provider.yaml") |= (
          from_yaml
          | (.upstream_oauth2.providers[] | select(.client_id == "fgentic").client_secret) = strenv(FGENTIC_CLIENT_SECRET)
          | to_yaml
        )
    ' - |
		sops --config "${SOPS_CONFIG}" --filename-override "${target}" --encrypt /dev/stdin \
			>"${stage}"
	chmod 0644 "${stage}"
else
	FGENTIC_DATA_ROOT="${DATA_ROOT}" \
		FGENTIC_SOPS_CONFIG="${SOPS_CONFIG}" \
		FGENTIC_SECRETS_DIR="${STAGE_DIR}" \
		FGENTIC_SECRET_SET="${GEN_SET}" \
		"${SCRIPT_DIR}/gen-secrets.sh" "${SERVER_NAME}" "${ENV}" --force
fi

for file in "${TARGET_FILES[@]}"; do
	stage="${STAGE_DIR}/${file}"
	if [ ! -f "${stage}" ]; then
		echo "error: generator did not stage expected file: ${file}" >&2
		exit 1
	fi
	if ! decrypt "${stage}" >/dev/null; then
		echo "error: staged file is not decryptable: ${file}" >&2
		exit 1
	fi
done

validate_appservice() {
	local old_file="${SECRETS_DIR}/matrix-a2a-bridge-registration.sops.yaml"
	local new_file="${STAGE_DIR}/matrix-a2a-bridge-registration.sops.yaml"
	local old_registration bridge_registration matrix_registration old_as new_as old_hs new_hs
	old_registration="$(secret_value "${old_file}" bridge matrix-a2a-bridge-registration '.stringData."registration.yaml"')"
	bridge_registration="$(secret_value "${new_file}" bridge matrix-a2a-bridge-registration '.stringData."registration.yaml"')"
	matrix_registration="$(secret_value "${new_file}" matrix matrix-a2a-bridge-registration '.stringData."registration.yaml"')"
	assert_equal "${bridge_registration}" "${matrix_registration}" "appservice namespace copies differ"
	old_as="$(yq -r '.as_token' <<<"${old_registration}")"
	new_as="$(yq -r '.as_token' <<<"${bridge_registration}")"
	old_hs="$(yq -r '.hs_token' <<<"${old_registration}")"
	new_hs="$(yq -r '.hs_token' <<<"${bridge_registration}")"
	assert_changed "${old_as}" "${new_as}" "appservice as_token"
	assert_changed "${old_hs}" "${new_hs}" "appservice hs_token"
}

validate_a2a() {
	local old_file="${SECRETS_DIR}/a2a-authorization.sops.yaml"
	local new_file="${STAGE_DIR}/a2a-authorization.sops.yaml"
	local old_key server_key bridge_key
	old_key="$(secret_value "${old_file}" bridge a2a-bridge-credential '.stringData.token')"
	server_key="$(secret_value "${new_file}" agentgateway-system a2a-bridge-callers '.stringData."matrix-a2a-bridge" | from_json | .key')"
	bridge_key="$(secret_value "${new_file}" bridge a2a-bridge-credential '.stringData.token')"
	assert_equal "${server_key}" "${bridge_key}" "A2A gateway and bridge credentials differ"
	assert_changed "${old_key}" "${bridge_key}" "A2A workload credential"
}

validate_mcp() {
	local old_file="${SECRETS_DIR}/mcp-authorization.sops.yaml"
	local new_file="${STAGE_DIR}/mcp-authorization.sops.yaml"
	local old_authorization server_key agent_authorization
	old_authorization="$(secret_value "${old_file}" kagent platform-helper-mcp-credential '.stringData.authorization')"
	server_key="$(secret_value "${new_file}" agentgateway-system mcp-agent-callers '.stringData."platform-helper" | from_json | .key')"
	agent_authorization="$(secret_value "${new_file}" kagent platform-helper-mcp-credential '.stringData.authorization')"
	assert_equal "Bearer ${server_key}" "${agent_authorization}" "MCP gateway and platform-helper credentials differ"
	assert_changed "${old_authorization}" "${agent_authorization}" "platform-helper MCP credential"
}

validate_databases() {
	local new_roles="${STAGE_DIR}/postgres-roles.sops.yaml"
	local new_synapse new_mas new_bridge new_kagent matrix_synapse matrix_mas bridge_url kagent_url
	new_synapse="$(secret_value "${new_roles}" postgres pg-synapse '.stringData.password')"
	new_mas="$(secret_value "${new_roles}" postgres pg-mas '.stringData.password')"
	new_bridge="$(secret_value "${new_roles}" postgres pg-bridge '.stringData.password')"
	new_kagent="$(secret_value "${new_roles}" postgres pg-kagent '.stringData.password')"
	matrix_synapse="$(secret_value "${new_roles}" matrix pg-synapse '.stringData.password')"
	matrix_mas="$(secret_value "${new_roles}" matrix pg-mas '.stringData.password')"
	assert_equal "${new_synapse}" "${matrix_synapse}" "Synapse database copies differ"
	assert_equal "${new_mas}" "${matrix_mas}" "MAS database copies differ"
	assert_equal "${new_synapse}" "${PG_SYNAPSE}" "unexpected Synapse database value"
	assert_equal "${new_mas}" "${PG_MAS}" "unexpected MAS database value"
	assert_equal "${new_bridge}" "${PG_BRIDGE}" "unexpected bridge database value"
	assert_equal "${new_kagent}" "${PG_KAGENT}" "unexpected kagent database value"
	case "${DB_MODE}" in
	synapse) assert_changed "${OLD_PG_SYNAPSE}" "${new_synapse}" "Synapse database password" ;;
	mas) assert_changed "${OLD_PG_MAS}" "${new_mas}" "MAS database password" ;;
	bridge) assert_changed "${OLD_PG_BRIDGE}" "${new_bridge}" "bridge database password" ;;
	kagent) assert_changed "${OLD_PG_KAGENT}" "${new_kagent}" "kagent database password" ;;
	all)
		assert_changed "${OLD_PG_SYNAPSE}" "${new_synapse}" "Synapse database password"
		assert_changed "${OLD_PG_MAS}" "${new_mas}" "MAS database password"
		assert_changed "${OLD_PG_BRIDGE}" "${new_bridge}" "bridge database password"
		assert_changed "${OLD_PG_KAGENT}" "${new_kagent}" "kagent database password"
		;;
	*)
		echo "error: invalid database validation mode: ${DB_MODE}" >&2
		exit 1
		;;
	esac
	if [ -f "${STAGE_DIR}/matrix-a2a-bridge-db.sops.yaml" ]; then
		bridge_url="$(secret_value "${STAGE_DIR}/matrix-a2a-bridge-db.sops.yaml" bridge matrix-a2a-bridge-db '.stringData.url')"
		assert_equal \
			"${bridge_url}" \
			"postgres://bridge:${new_bridge}@platform-pg-rw.postgres.svc.cluster.local:5432/bridge?sslmode=require" \
			"bridge database URL does not match its role password"
	fi
	if [ -f "${STAGE_DIR}/kagent.sops.yaml" ]; then
		kagent_url="$(secret_value "${STAGE_DIR}/kagent.sops.yaml" kagent kagent-db '.stringData.url')"
		assert_equal \
			"${kagent_url}" \
			"postgresql://kagent:${new_kagent}@platform-pg-rw.postgres.svc.cluster.local:5432/kagent?sslmode=require" \
			"kagent database URL does not match its role password"
	fi
}

validate_keycloak_db() {
	local old_file="${SECRETS_DIR}/keycloak-db.sops.yaml"
	local new_file="${STAGE_DIR}/keycloak-db.sops.yaml"
	local old_password postgres_password workload_password
	old_password="$(secret_value "${old_file}" postgres pg-keycloak '.stringData.password')"
	postgres_password="$(secret_value "${new_file}" postgres pg-keycloak '.stringData.password')"
	workload_password="$(secret_value "${new_file}" keycloak pg-keycloak '.stringData.password')"
	assert_equal "${postgres_password}" "${workload_password}" "Keycloak database copies differ"
	assert_changed "${old_password}" "${postgres_password}" "Keycloak database password"
}

validate_slack() {
	local old_file="${SECRETS_DIR}/mautrix-slack.sops.yaml"
	local new_file="${STAGE_DIR}/mautrix-slack.sops.yaml"
	local old_password new_password database_uri registration runtime_as runtime_hs
	local old_registration old_as old_hs old_sender new_sender
	old_password="$(secret_value "${old_file}" postgres pg-slackbridge '.stringData.password')"
	new_password="$(secret_value "${new_file}" postgres pg-slackbridge '.stringData.password')"
	database_uri="$(secret_value "${new_file}" bridges mautrix-slack '.stringData."database-uri"')"
	registration="$(secret_value "${new_file}" matrix mautrix-slack-registration '.stringData."registration.yaml"')"
	runtime_as="$(secret_value "${new_file}" bridges mautrix-slack '.stringData."as-token"')"
	runtime_hs="$(secret_value "${new_file}" bridges mautrix-slack '.stringData."hs-token"')"
	old_registration="$(secret_value "${old_file}" matrix mautrix-slack-registration '.stringData."registration.yaml"')"
	old_as="$(yq -er '.as_token' <<<"${old_registration}")"
	old_hs="$(yq -er '.hs_token' <<<"${old_registration}")"
	old_sender="$(yq -er '.sender_localpart' <<<"${old_registration}")"
	new_sender="$(yq -er '.sender_localpart' <<<"${registration}")"

	assert_changed "${old_password}" "${new_password}" "Slack bridge database password"
	assert_changed "${old_as}" "${runtime_as}" "Slack appservice as_token"
	assert_changed "${old_hs}" "${runtime_hs}" "Slack appservice hs_token"
	assert_equal "${old_sender}" "${new_sender}" "Slack sender_localpart changed during rotation"
	assert_equal \
		"${database_uri}" \
		"postgres://slackbridge:${new_password}@platform-pg-rw.postgres.svc.cluster.local:5432/slackbridge?sslmode=require" \
		"Slack database URI does not match its role password"
	assert_equal "${runtime_as}" "$(yq -er '.as_token' <<<"${registration}")" "Slack as_token copies differ"
	assert_equal "${runtime_hs}" "$(yq -er '.hs_token' <<<"${registration}")" "Slack hs_token copies differ"
	yq -e \
		'.id == "slack" and
		 .url == "http://mautrix-slack.bridges.svc.cluster.local:29335" and
		 .rate_limited == false and
		 .receive_ephemeral == true and
		 ."de.sorunome.msc2409.push_ephemeral" == true and
		 (.sender_localpart | test("^[0-9a-f]{32}$")) and
		 (.namespaces.users | length == 2)' \
		<<<"${registration}" >/dev/null || {
		echo "error: staged invariant failed: Slack registration schema drifted" >&2
		exit 1
	}
}

validate_telegram() {
	local old_file="${SECRETS_DIR}/mautrix-telegram.sops.yaml"
	local new_file="${STAGE_DIR}/mautrix-telegram.sops.yaml"
	local old_password new_password database_uri registration runtime_as runtime_hs
	local old_registration old_as old_hs old_sender new_sender old_api_id new_api_id old_api_hash new_api_hash
	old_password="$(secret_value "${old_file}" postgres pg-telegrambridge '.stringData.password')"
	new_password="$(secret_value "${new_file}" postgres pg-telegrambridge '.stringData.password')"
	database_uri="$(secret_value "${new_file}" bridges mautrix-telegram '.stringData."database-uri"')"
	registration="$(secret_value "${new_file}" matrix mautrix-telegram-registration '.stringData."registration.yaml"')"
	runtime_as="$(secret_value "${new_file}" bridges mautrix-telegram '.stringData."as-token"')"
	runtime_hs="$(secret_value "${new_file}" bridges mautrix-telegram '.stringData."hs-token"')"
	old_api_id="$(secret_value "${old_file}" bridges mautrix-telegram '.stringData."api-id"')"
	new_api_id="$(secret_value "${new_file}" bridges mautrix-telegram '.stringData."api-id"')"
	old_api_hash="$(secret_value "${old_file}" bridges mautrix-telegram '.stringData."api-hash"')"
	new_api_hash="$(secret_value "${new_file}" bridges mautrix-telegram '.stringData."api-hash"')"
	old_registration="$(secret_value "${old_file}" matrix mautrix-telegram-registration '.stringData."registration.yaml"')"
	old_as="$(yq -er '.as_token' <<<"${old_registration}")"
	old_hs="$(yq -er '.hs_token' <<<"${old_registration}")"
	old_sender="$(yq -er '.sender_localpart' <<<"${old_registration}")"
	new_sender="$(yq -er '.sender_localpart' <<<"${registration}")"

	assert_changed "${old_password}" "${new_password}" "Telegram bridge database password"
	assert_changed "${old_as}" "${runtime_as}" "Telegram appservice as_token"
	assert_changed "${old_hs}" "${runtime_hs}" "Telegram appservice hs_token"
	assert_equal "${old_sender}" "${new_sender}" "Telegram sender_localpart changed during rotation"
	assert_equal "${old_api_id}" "${new_api_id}" "Telegram API ID changed during rotation"
	assert_equal "${old_api_hash}" "${new_api_hash}" "Telegram API hash changed during rotation"
	assert_equal \
		"${database_uri}" \
		"postgres://telegrambridge:${new_password}@platform-pg-rw.postgres.svc.cluster.local:5432/telegrambridge?sslmode=require" \
		"Telegram database URI does not match its role password"
	assert_equal "${runtime_as}" "$(yq -er '.as_token' <<<"${registration}")" "Telegram as_token copies differ"
	assert_equal "${runtime_hs}" "$(yq -er '.hs_token' <<<"${registration}")" "Telegram hs_token copies differ"
	yq -e \
		'.id == "telegram" and
		 .url == "http://mautrix-telegram.bridges.svc.cluster.local:29317" and
		 .rate_limited == false and
		 .receive_ephemeral == true and
		 ."de.sorunome.msc2409.push_ephemeral" == true and
		 (.sender_localpart | test("^[0-9a-f]{32}$")) and
		 (.namespaces.users | length == 2)' \
		<<<"${registration}" >/dev/null || {
		echo "error: staged invariant failed: Telegram registration schema drifted" >&2
		exit 1
	}
}

validate_provider() {
	local old_file="${SECRETS_DIR}/${MODEL_SECRET_FILE}"
	local new_file="${STAGE_DIR}/${MODEL_SECRET_FILE}"
	local old_key new_key expected_key
	old_key="$(secret_value "${old_file}" agentgateway-system "${MODEL_SECRET_NAME}" '.stringData.Authorization // (.data.Authorization | @base64d)')"
	new_key="$(secret_value "${new_file}" agentgateway-system "${MODEL_SECRET_NAME}" '.stringData.Authorization // (.data.Authorization | @base64d)')"
	expected_key="${!MODEL_SECRET_ENV}"
	assert_equal "${new_key}" "${expected_key}" "provider Secret does not contain the supplied key"
	assert_changed "${old_key}" "${new_key}" "provider API key"
}

validate_keycloak_client() {
	local old_file="${SECRETS_DIR}/keycloak-bootstrap.sops.yaml"
	local new_file="${STAGE_DIR}/keycloak-bootstrap.sops.yaml"
	local old_client keycloak_client mas_client
	local field old_value new_value
	old_client="$(secret_value "${old_file}" keycloak keycloak-credentials '.stringData.FGENTIC_CLIENT_SECRET')"
	keycloak_client="$(secret_value "${new_file}" keycloak keycloak-credentials '.stringData.FGENTIC_CLIENT_SECRET')"
	mas_client="$(secret_value "${new_file}" matrix mas-upstream-oidc '.stringData."provider.yaml" | from_yaml | .upstream_oauth2.providers[] | select(.client_id == "fgentic") | .client_secret')"
	assert_equal "${keycloak_client}" "${mas_client}" "Keycloak and MAS OIDC client secrets differ"
	assert_equal "${keycloak_client}" "${FGENTIC_CLIENT_SECRET}" "OIDC client secret differs from the acknowledged live value"
	assert_changed "${old_client}" "${keycloak_client}" "OIDC client secret"
	for field in KC_BOOTSTRAP_ADMIN_USERNAME KC_BOOTSTRAP_ADMIN_PASSWORD FGENTIC_ALICE_PASSWORD FGENTIC_BOB_PASSWORD; do
		old_value="$(secret_value "${old_file}" keycloak keycloak-credentials ".stringData.${field}")"
		new_value="$(secret_value "${new_file}" keycloak keycloak-credentials ".stringData.${field}")"
		assert_equal "${old_value}" "${new_value}" "bootstrap identity field ${field} changed"
	done
}

case "${SECRET_SET}" in
appservice) validate_appservice ;;
a2a) validate_a2a ;;
mcp) validate_mcp ;;
db-*) validate_databases ;;
provider) validate_provider ;;
keycloak-db) validate_keycloak_db ;;
slack) validate_slack ;;
telegram) validate_telegram ;;
keycloak-client) validate_keycloak_client ;;
all)
	validate_databases
	validate_appservice
	validate_a2a
	validate_mcp
	validate_keycloak_db
	if [ -n "${MODEL_SECRET_FILE}" ]; then
		validate_provider
	fi
	;;
*)
	echo "error: no validation rule for ${SECRET_SET}" >&2
	exit 1
	;;
esac

# Every output is valid before the first tracked file changes. Ciphertext backups make the small
# multi-file replacement transaction recoverable even if the process is interrupted mid-move.
for target in "${TARGET_PATHS[@]}"; do
	cp -p "${target}" "${BACKUP_DIR}/$(basename "${target}")"
done
for target in "${TARGET_PATHS[@]}"; do
	mv -f "${STAGE_DIR}/$(basename "${target}")" "${target}"
done
COMMITTED=true

cat <<EOF
Rotated ${SECRET_SET} as SOPS ciphertext only:
$(printf '  %s\n' "${TARGET_PATHS[@]#"${DATA_ROOT}/"}")

No cluster was changed. Review the encrypted diff, commit and push it, reconcile
platform-secrets, then follow the ordered restart plan in:
  .agents/skills/matrix-agents/SKILL.md#runbook-rotate-secrets
EOF

case "${SECRET_SET}" in
appservice)
	echo "Restart order: Synapse (reload registration), then bridge (load matching tokens)."
	;;
a2a)
	echo "Restart order: wait for agentgateway policy acceptance, then restart the bridge."
	;;
mcp)
	echo "Restart order: wait for agentgateway policy acceptance, then restart the kagent controller so it regenerates and rolls platform-helper."
	;;
db-synapse | db-mas | db-bridge | db-kagent | db-core | keycloak-db)
	echo "Restart order: wait for CNPG managedRolesStatus to match the Secret resourceVersion, then restart the named consumer(s)."
	;;
slack)
	echo "Restart order: wait for the slackbridge CNPG role, restart Synapse, then restart mautrix-slack."
	;;
telegram)
	echo "Restart order: wait for the telegrambridge CNPG role, restart Synapse, then restart mautrix-telegram."
	;;
provider)
	echo "Restart order: wait for the provider backend to report Accepted; no workload restart is normally required."
	;;
keycloak-client)
	echo "Restart order: live Keycloak was changed first; reconcile the matching ciphertext, then restart MAS."
	;;
all)
	echo "Restart order: CNPG roles, Keycloak/Synapse/MAS/kagent, then bridge last; the runbook has exact commands."
	;;
*)
	echo "error: no restart plan for ${SECRET_SET}" >&2
	exit 1
	;;
esac
