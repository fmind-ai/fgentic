#!/usr/bin/env bash
# Generate the platform's SOPS-encrypted Secrets from scratch for a given server_name:
# fresh Postgres role passwords, appservice registration tokens, and the derived connection
# URLs — every value consistent across the files that share it. Pipes plaintext directly to SOPS
# (age recipient from .sops.yaml) and atomically installs ciphertext in the working tree.
#
#   scripts/gen-secrets.sh fgentic.localhost local  # k3d cluster
#   scripts/gen-secrets.sh fgentic.fmind.ai gcp      # reference deployment
#
# Files land in clusters/<env>/secrets/ (each cluster owns its secret set — the registration
# regexes embed the server_name and credentials never span environments) and MUST be committed:
# Flux applies them from git. Only SOPS ciphertext is ever written.
# Existing *.sops.yaml files are left untouched unless --force is passed. Use rotate-secrets.sh
# for selective rotation and its consumer ordering; --force remains a full bootstrap-regeneration
# compatibility path. Keycloak's one-time realm bootstrap is the exception: an existing bootstrap
# file is never regenerated, because startup import skips an existing realm and new values would
# drift from the database.
# The selected provider comes from clusters/<env>/platform-settings.yaml. API profiles require
# their provider-specific environment variable; Vertex uses Workload Identity or local ADC, while
# vLLM is cluster-internal, so neither emits a model API-key Secret. Existing files remain untouched
# unless --force is set.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
DATA_ROOT="${FGENTIC_DATA_ROOT:-${REPO_ROOT}}"
SOPS_CONFIG="${FGENTIC_SOPS_CONFIG:-${DATA_ROOT}/.sops.yaml}"
# shellcheck source=scripts/secrets-common.sh
source "${SCRIPT_DIR}/secrets-common.sh"

SERVER_NAME="${1:?usage: gen-secrets.sh <server_name> <local|gcp> [--force]}"
ENV="${2:?usage: gen-secrets.sh <server_name> <local|gcp> [--force]}"
FORCE="${3:-}"
# Internal selector used by rotate-secrets.sh to stage only one coherent secret class. It is an
# environment variable rather than another public CLI flag so initial generation stays simple.
SECRET_SET="${FGENTIC_SECRET_SET:-all}"

validate_secret_environment "${ENV}"
case "${FORCE}" in
	"" | --force) ;;
	*)
		echo "error: third argument must be --force when set" >&2
		exit 2
		;;
esac
case "${SECRET_SET}" in
	all | rotatable | appservice | a2a | mcp | db-core | keycloak-db | knowledge-db | \
		knowledge-ingestion | \
		provider | bootstrap | slack | telegram | break-glass) ;;
	*)
		echo "error: unsupported internal secret set: ${SECRET_SET}" >&2
		exit 2
		;;
esac
validate_server_name "${SERVER_NAME}"
if [ ! -f "${SOPS_CONFIG}" ]; then
	echo "error: SOPS configuration not found: ${SOPS_CONFIG}" >&2
	exit 1
fi

want() {
	local set="$1"
	# Optional layers are generated only when explicitly selected. A normal production bootstrap
	# must not create dormant workload credentials or imply that the layer is enabled.
	if [ "${set}" = "slack" ] || [ "${set}" = "telegram" ] || [ "${set}" = "knowledge-ingestion" ] \
		|| [ "${set}" = "break-glass" ]; then
		[ "${SECRET_SET}" = "${set}" ]
		return
	fi
	[ "${SECRET_SET}" = "all" ] || [ "${SECRET_SET}" = "${set}" ] || {
		[ "${SECRET_SET}" = "rotatable" ] && [ "${set}" != "bootstrap" ]
	}
}

DIR="${FGENTIC_SECRETS_DIR:-${DATA_ROOT}/clusters/${ENV}/secrets}"
mkdir -p "${DIR}"
ESCAPED_SERVER_NAME="${SERVER_NAME//./\\.}"
PLATFORM_SETTINGS="${DATA_ROOT}/clusters/${ENV}/platform-settings.yaml"

if [ ! -f "${PLATFORM_SETTINGS}" ]; then
	echo "error: platform settings not found: ${PLATFORM_SETTINGS}" >&2
	exit 1
fi

# Telegram's upstream bridge requires the operator-owned API application pair in its deployment
# config. Validate it before random generation or the first encrypted write. Other secret sets do
# not read or require these optional credentials.
TELEGRAM_API_ID="${TELEGRAM_API_ID:-}"
TELEGRAM_API_HASH="${TELEGRAM_API_HASH:-}"
if want telegram; then
	if [[ ! "${TELEGRAM_API_ID}" =~ ^[1-9][0-9]*$ ]]; then
		echo "error: TELEGRAM_API_ID must be a positive decimal integer" >&2
		exit 1
	fi
	if [[ ! "${TELEGRAM_API_HASH}" =~ ^[0-9a-fA-F]{32}$ ]]; then
		echo "error: TELEGRAM_API_HASH must contain exactly 32 hexadecimal characters" >&2
		exit 1
	fi
fi

LLM_PROVIDER=""
MODEL_SECRET_ENV=""
MODEL_SECRET_FILE=""
MODEL_SECRET_NAME=""
MODEL_KEY=""
if want provider; then
	LLM_PROVIDER="$(yq -er '.data.llm_provider' "${PLATFORM_SETTINGS}")"
	resolve_model_secret "${LLM_PROVIDER}" || {
		echo "error: invalid provider in ${PLATFORM_SETTINGS}" >&2
		exit 1
	}

	if [ -n "${MODEL_SECRET_FILE}" ] && { [ "${FORCE}" = "--force" ] || [ ! -f "${DIR}/${MODEL_SECRET_FILE}" ]; }; then
		MODEL_KEY="${!MODEL_SECRET_ENV:-}"
		if [ -z "${MODEL_KEY}" ]; then
			echo "error: ${MODEL_SECRET_ENV} is required for llm_provider=${LLM_PROVIDER}" >&2
			exit 1
		fi
	fi
fi

PG_HOST="platform-pg-rw.postgres.svc.cluster.local"
BRIDGE_URL="http://matrix-a2a-bridge.bridge.svc.cluster.local:29331"

encrypt_to() { # encrypt_to <file> <content>: plaintext remains in the pipeline, never on disk
	local file tmp
	file="$1"
	tmp="$(mktemp "${DIR}/.encrypted.XXXXXX")"
	if ! printf '%s\n' "$2" \
		| sops --config "${SOPS_CONFIG}" --filename-override "${file}" --encrypt /dev/stdin \
			>"${tmp}"; then
		rm -f "${tmp}"
		return 1
	fi
	chmod 0644 "${tmp}"
	mv -f "${tmp}" "${file}"
}

emit() { # emit <file> <content>: skip if present (unless --force), else encrypt atomically
	local file
	file="${DIR}/$1"
	if [ -f "${file}" ] && [ "${FORCE}" != "--force" ]; then
		echo "skip (exists): ${file}"
		return
	fi
	encrypt_to "${file}" "$2"
	echo "wrote (encrypted): ${file}"
}

emit_once() { # emit_once <file> <content>: bootstrap data must never rotate implicitly
	local file
	file="${DIR}/$1"
	if [ -f "${file}" ]; then
		echo "skip (bootstrap exists): ${file}"
		return
	fi
	encrypt_to "${file}" "$2"
	echo "wrote (encrypted bootstrap): ${file}"
}

sync_kustomization() {
	local LC_ALL=C
	local file kustomization tmp base
	local -a secret_files=()
	# An explicit resource inventory keeps an empty reference environment buildable while ensuring
	# newly generated ciphertext is actually reconciled. The list contains filenames only; no
	# decrypted material or provider values cross this boundary.
	for file in "${DIR}"/*.sops.yaml; do
		[ -e "${file}" ] || continue
		base="$(basename "${file}")"
		# The break-glass recovery credential (issue #467) is consumed out-of-band during a
		# deliberate window (sops -d -> MAS Admin API / mas-cli manage), never mounted by a workload.
		# It must NOT be globbed into the Flux-reconciled inventory, or Flux would decrypt it into an
		# always-standing, unmounted recovery Secret in etcd -- a standing superuser the SSO-only
		# posture forbids. It stays SOPS ciphertext in clusters/<env>/secrets/ only.
		[ "${base}" = "break-glass-recovery.sops.yaml" ] && continue
		secret_files+=("${base}")
	done

	kustomization="${DIR}/kustomization.yaml"
	tmp="$(mktemp "${DIR}/.kustomization.XXXXXX")"
	{
		printf '%s\n' \
			'apiVersion: kustomize.config.k8s.io/v1beta1' \
			'kind: Kustomization'
		if [ "${#secret_files[@]}" -eq 0 ]; then
			printf '%s\n' 'resources: []'
		else
			printf '%s\n' 'resources:'
			printf '  - %s\n' "${secret_files[@]}"
		fi
	} >"${tmp}"
	chmod 0644 "${tmp}"
	mv -f "${tmp}" "${kustomization}"
}

PG_SYNAPSE="${PG_SYNAPSE:-$(openssl rand -hex 24)}"
PG_MAS="${PG_MAS:-$(openssl rand -hex 24)}"
PG_BRIDGE="${PG_BRIDGE:-$(openssl rand -hex 24)}"
PG_KAGENT="${PG_KAGENT:-$(openssl rand -hex 24)}"
PG_KEYCLOAK="${PG_KEYCLOAK:-$(openssl rand -hex 24)}"
PG_KNOWLEDGE_OWNER="${PG_KNOWLEDGE_OWNER:-$(openssl rand -hex 24)}"
PG_KNOWLEDGE_INGESTION="${PG_KNOWLEDGE_INGESTION:-$(openssl rand -hex 24)}"
PG_KNOWLEDGE_CONNECTOR="${PG_KNOWLEDGE_CONNECTOR:-$(openssl rand -hex 24)}"
PG_KNOWLEDGE_RETRIEVAL="${PG_KNOWLEDGE_RETRIEVAL:-$(openssl rand -hex 24)}"
PG_SLACKBRIDGE="${PG_SLACKBRIDGE:-$(openssl rand -hex 24)}"
PG_TELEGRAMBRIDGE="${PG_TELEGRAMBRIDGE:-$(openssl rand -hex 24)}"
AS_TOKEN="${AS_TOKEN:-$(openssl rand -hex 32)}"
HS_TOKEN="${HS_TOKEN:-$(openssl rand -hex 32)}"
SLACK_AS_TOKEN="${SLACK_AS_TOKEN:-$(openssl rand -hex 32)}"
SLACK_HS_TOKEN="${SLACK_HS_TOKEN:-$(openssl rand -hex 32)}"
# bridge-v2 v0.28.1's GenerateRegistration uses a random 32-character sender localpart that is
# distinct from the configured `slackbot`; keep the generated homeserver contract exact.
SLACK_SENDER_LOCALPART="${SLACK_SENDER_LOCALPART:-$(openssl rand -hex 16)}"
TELEGRAM_AS_TOKEN="${TELEGRAM_AS_TOKEN:-$(openssl rand -hex 32)}"
TELEGRAM_HS_TOKEN="${TELEGRAM_HS_TOKEN:-$(openssl rand -hex 32)}"
TELEGRAM_SENDER_LOCALPART="${TELEGRAM_SENDER_LOCALPART:-$(openssl rand -hex 16)}"
A2A_WORKLOAD_KEY="${A2A_WORKLOAD_KEY:-$(openssl rand -hex 32)}"
MCP_PLATFORM_HELPER_KEY="${MCP_PLATFORM_HELPER_KEY:-$(openssl rand -hex 32)}"
KNOWLEDGE_INGESTION_KEY="${KNOWLEDGE_INGESTION_KEY:-$(openssl rand -hex 32)}"
KEYCLOAK_ADMIN_PASSWORD="${KEYCLOAK_ADMIN_PASSWORD:-$(openssl rand -hex 24)}"
FGENTIC_CLIENT_SECRET="${FGENTIC_CLIENT_SECRET:-$(openssl rand -hex 32)}"
FGENTIC_ALICE_PASSWORD="${FGENTIC_ALICE_PASSWORD:-$(openssl rand -hex 24)}"
FGENTIC_BOB_PASSWORD="${FGENTIC_BOB_PASSWORD:-$(openssl rand -hex 24)}"
BREAK_GLASS_RECOVERY_PASSWORD="${BREAK_GLASS_RECOVERY_PASSWORD:-$(openssl rand -hex 24)}"

if want db-core; then
	PG_ROLES="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: pg-synapse
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: synapse
  password: ${PG_SYNAPSE}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-mas
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: mas
  password: ${PG_MAS}
---
# ESS reads the synapse/MAS DB passwords from ITS namespace (matrix) while CNPG manages the
# roles from the postgres namespace — same credentials, two Secrets each.
apiVersion: v1
kind: Secret
metadata:
  name: pg-synapse
  namespace: matrix
type: kubernetes.io/basic-auth
stringData:
  username: synapse
  password: ${PG_SYNAPSE}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-mas
  namespace: matrix
type: kubernetes.io/basic-auth
stringData:
  username: mas
  password: ${PG_MAS}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-bridge
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: bridge
  password: ${PG_BRIDGE}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-kagent
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: kagent
  password: ${PG_KAGENT}
EOF
	)"
	emit postgres-roles.sops.yaml "${PG_ROLES}"

	KAGENT="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: kagent-db
  namespace: kagent
type: Opaque
stringData:
  url: postgresql://kagent:${PG_KAGENT}@${PG_HOST}:5432/kagent?sslmode=require
---
apiVersion: v1
kind: Secret
metadata:
  name: kagent-model-auth
  namespace: kagent
type: Opaque
stringData:
  OPENAI_API_KEY: sk-not-used-agentgateway-holds-the-real-key
EOF
	)"
	emit kagent.sops.yaml "${KAGENT}"

	BRIDGE_DB="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: matrix-a2a-bridge-db
  namespace: bridge
type: Opaque
stringData:
  url: postgres://bridge:${PG_BRIDGE}@${PG_HOST}:5432/bridge?sslmode=require
EOF
	)"
	emit matrix-a2a-bridge-db.sops.yaml "${BRIDGE_DB}"
fi

# Keep the Keycloak role's CNPG and workload copies in one independently generated file. This lets
# existing clusters add the optional IdP without rotating established roles, while --force can
# still rotate the database password and both consumers together.
if want keycloak-db; then
	KEYCLOAK_DB="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: pg-keycloak
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: keycloak
  password: ${PG_KEYCLOAK}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-keycloak
  namespace: keycloak
type: kubernetes.io/basic-auth
stringData:
  username: keycloak
  password: ${PG_KEYCLOAK}
EOF
	)"
	emit keycloak-db.sops.yaml "${KEYCLOAK_DB}"
fi

# Knowledge ingestion owns the schema while retrieval is read-only. Keep the two independent role
# passwords and the retrieval workload copy in one coherent ciphertext transaction; never project
# the owner credential outside the postgres namespace.
if want knowledge-db; then
	KNOWLEDGE_DB="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-owner
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_owner
  password: ${PG_KNOWLEDGE_OWNER}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-retrieval
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_retrieval
  password: ${PG_KNOWLEDGE_RETRIEVAL}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-retrieval
  namespace: knowledge
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_retrieval
  password: ${PG_KNOWLEDGE_RETRIEVAL}
EOF
	)"
	emit knowledge-db.sops.yaml "${KNOWLEDGE_DB}"
fi

# The ingestion and connector database logins plus the agentgateway caller key are separate
# credentials in one optional workload boundary. Keep each namespace pair coherent without ever
# projecting the schema owner or a model/provider key into the knowledge namespace.
if want knowledge-ingestion; then
	KNOWLEDGE_INGESTION="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-ingestion
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_ingestion
  password: ${PG_KNOWLEDGE_INGESTION}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-connector
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_connector
  password: ${PG_KNOWLEDGE_CONNECTOR}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-connector
  namespace: knowledge
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_connector
  password: ${PG_KNOWLEDGE_CONNECTOR}
---
apiVersion: v1
kind: Secret
metadata:
  name: pg-knowledge-ingestion
  namespace: knowledge
type: kubernetes.io/basic-auth
stringData:
  username: knowledge_ingestion
  password: ${PG_KNOWLEDGE_INGESTION}
---
apiVersion: v1
kind: Secret
metadata:
  name: knowledge-ingestion-callers
  namespace: agentgateway-system
type: Opaque
stringData:
  knowledge-ingestion: |
    {
      "key": "${KNOWLEDGE_INGESTION_KEY}",
      "metadata": {
        "workload": "knowledge-ingestion"
      }
    }
---
apiVersion: v1
kind: Secret
metadata:
  name: knowledge-ingestion-credential
  namespace: knowledge
type: Opaque
stringData:
  authorization: Bearer ${KNOWLEDGE_INGESTION_KEY}
EOF
	)"
	emit knowledge-ingestion.sops.yaml "${KNOWLEDGE_INGESTION}"
fi

# Startup import is bootstrap-only: Keycloak skips an existing realm. Preserve this file even on
# --force so the encrypted source of truth never claims credentials the live realm did not adopt.
if want bootstrap; then
	KEYCLOAK_BOOTSTRAP="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: keycloak-credentials
  namespace: keycloak
type: Opaque
stringData:
  KC_BOOTSTRAP_ADMIN_USERNAME: admin
  KC_BOOTSTRAP_ADMIN_PASSWORD: ${KEYCLOAK_ADMIN_PASSWORD}
  FGENTIC_CLIENT_SECRET: ${FGENTIC_CLIENT_SECRET}
  FGENTIC_ALICE_PASSWORD: ${FGENTIC_ALICE_PASSWORD}
  FGENTIC_BOB_PASSWORD: ${FGENTIC_BOB_PASSWORD}
---
# Keep the MAS provider and Keycloak client credential in the same bootstrap-only SOPS file.
# ESS mounts this fragment through matrixAuthenticationService.additional.configSecret.
apiVersion: v1
kind: Secret
metadata:
  name: mas-upstream-oidc
  namespace: matrix
type: Opaque
stringData:
  provider.yaml: |
    upstream_oauth2:
      providers:
        - id: 01H8PKNWKKRPCBW4YGH1RWV279
          issuer: https://id.${SERVER_NAME}/realms/fgentic
          human_name: Fgentic
          client_id: fgentic
          client_secret: ${FGENTIC_CLIENT_SECRET}
          token_endpoint_auth_method: client_secret_basic
          scope: openid fgentic-groups
          claims_imports:
            skip_confirmation: true
            localpart:
              action: require
              template: "{{ user.matrix_localpart }}"
              on_conflict: fail
            displayname:
              action: force
              template: "{{ user.name }}"
            email:
              action: force
              template: "{{ user.email }}"
EOF
	)"
	emit_once keycloak-bootstrap.sops.yaml "${KEYCLOAK_BOOTSTRAP}"
fi

# Optional break-glass recovery credential (issue #467): a pre-provisioned local recovery admin for
# MAS-plane administration when the upstream IdP (Keycloak) is unreachable and every SSO login is
# locked out. Absent by default — emitted only with FGENTIC_SECRET_SET=break-glass, never by `all`
# or a `rotatable` sweep — so a normal SSO-first bootstrap ships no standing superuser and nothing
# plaintext in git. Written with emit_once because, once this password provisions the live MAS
# recovery account during a break-glass window, it becomes that account's source of truth; silently
# regenerating it (via --force or a bulk rotation) would drift the sealed value from the live
# account — exactly the keycloak-bootstrap hazard. Rotation is therefore deliberate and live-first
# (change the MAS account, then re-seal); rotate-secrets.sh does not sweep it, matching the Keycloak
# admin/demo-user bootstrap precedent. "Disabled by default" is layered and enforced elsewhere: the
# secret is absent, `mas_local_login_enabled` stays "false" on SSO-only clusters, and the account is
# not provisioned or enabled until the deliberate, audited GitOps flip. See docs/identity.md.
if want break-glass; then
	BREAK_GLASS_RECOVERY="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: break-glass-recovery
  namespace: matrix
type: kubernetes.io/basic-auth
stringData:
  username: break-glass
  password: ${BREAK_GLASS_RECOVERY_PASSWORD}
EOF
	)"
	emit_once break-glass-recovery.sops.yaml "${BREAK_GLASS_RECOVERY}"
fi

if want provider && [ -n "${MODEL_SECRET_FILE}" ]; then
	if [ -z "${MODEL_KEY}" ]; then
		echo "skip (exists): ${DIR}/${MODEL_SECRET_FILE}"
	else
		MODEL_KEY_BASE64="$(printf '%s' "${MODEL_KEY}" | base64 | tr -d '\n')"
		AGW_MODEL="$(
			cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: ${MODEL_SECRET_NAME}
  namespace: agentgateway-system
type: Opaque
data:
  Authorization: ${MODEL_KEY_BASE64}
EOF
		)"
		emit "${MODEL_SECRET_FILE}" "${AGW_MODEL}"
	fi
fi

if want a2a; then
	A2A_AUTHORIZATION="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: a2a-bridge-callers
  namespace: agentgateway-system
type: Opaque
stringData:
  matrix-a2a-bridge: |
    {
      "key": "${A2A_WORKLOAD_KEY}",
      "metadata": {
        "workload": "matrix-a2a-bridge"
      }
    }
---
apiVersion: v1
kind: Secret
metadata:
  name: a2a-bridge-credential
  namespace: bridge
type: Opaque
stringData:
  token: ${A2A_WORKLOAD_KEY}
EOF
	)"
	emit a2a-authorization.sops.yaml "${A2A_AUTHORIZATION}"
fi

if want mcp; then
	MCP_AUTHORIZATION="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: mcp-agent-callers
  namespace: agentgateway-system
type: Opaque
stringData:
  platform-helper: |
    {
      "key": "${MCP_PLATFORM_HELPER_KEY}",
      "metadata": {
        "agent": "platform-helper"
      }
    }
---
apiVersion: v1
kind: Secret
metadata:
  name: platform-helper-mcp-credential
  namespace: kagent
type: Opaque
stringData:
  authorization: Bearer ${MCP_PLATFORM_HELPER_KEY}
EOF
	)"
	emit mcp-authorization.sops.yaml "${MCP_AUTHORIZATION}"
fi

if want appservice; then
	REGISTRATION="$(
		cat <<EOF
id: matrix-a2a-bridge
url: ${BRIDGE_URL}
as_token: ${AS_TOKEN}
hs_token: ${HS_TOKEN}
sender_localpart: a2a-bridge
rate_limited: false
namespaces:
  users:
    - regex: '@a2a-bridge:${ESCAPED_SERVER_NAME}'
      exclusive: true
    - regex: '@agent-.*:${ESCAPED_SERVER_NAME}'
      exclusive: true
EOF
	)"
	REGISTRATION_INDENTED="$(printf '%s\n' "${REGISTRATION}" | sed 's/^/    /')"

	REGISTRATION_SECRETS="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: matrix-a2a-bridge-registration
  namespace: bridge
type: Opaque
stringData:
  registration.yaml: |
${REGISTRATION_INDENTED}
---
apiVersion: v1
kind: Secret
metadata:
  name: matrix-a2a-bridge-registration
  namespace: matrix
type: Opaque
stringData:
  registration.yaml: |
${REGISTRATION_INDENTED}
EOF
	)"
	emit matrix-a2a-bridge-registration.sops.yaml "${REGISTRATION_SECRETS}"
fi

if want slack; then
	SLACK_REGISTRATION="$(
		cat <<EOF
id: slack
url: http://mautrix-slack.bridges.svc.cluster.local:29335
as_token: ${SLACK_AS_TOKEN}
hs_token: ${SLACK_HS_TOKEN}
sender_localpart: ${SLACK_SENDER_LOCALPART}
rate_limited: false
namespaces:
  users:
    - regex: ^@slackbot:${ESCAPED_SERVER_NAME}\$
      exclusive: true
    - regex: ^@slack_.*:${ESCAPED_SERVER_NAME}\$
      exclusive: true
de.sorunome.msc2409.push_ephemeral: true
receive_ephemeral: true
EOF
	)"
	SLACK_REGISTRATION_INDENTED="$(printf '%s\n' "${SLACK_REGISTRATION}" | sed 's/^/    /')"

	SLACK_SECRETS="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: pg-slackbridge
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: slackbridge
  password: ${PG_SLACKBRIDGE}
---
apiVersion: v1
kind: Secret
metadata:
  name: mautrix-slack
  namespace: bridges
type: Opaque
stringData:
  database-uri: postgres://slackbridge:${PG_SLACKBRIDGE}@${PG_HOST}:5432/slackbridge?sslmode=require
  as-token: ${SLACK_AS_TOKEN}
  hs-token: ${SLACK_HS_TOKEN}
---
apiVersion: v1
kind: Secret
metadata:
  name: mautrix-slack-registration
  namespace: matrix
type: Opaque
stringData:
  registration.yaml: |
${SLACK_REGISTRATION_INDENTED}
EOF
	)"
	emit mautrix-slack.sops.yaml "${SLACK_SECRETS}"
fi

if want telegram; then
	TELEGRAM_REGISTRATION="$(
		cat <<EOF
id: telegram
url: http://mautrix-telegram.bridges.svc.cluster.local:29317
as_token: ${TELEGRAM_AS_TOKEN}
hs_token: ${TELEGRAM_HS_TOKEN}
sender_localpart: ${TELEGRAM_SENDER_LOCALPART}
rate_limited: false
namespaces:
  users:
    - regex: ^@telegrambot:${ESCAPED_SERVER_NAME}\$
      exclusive: true
    - regex: ^@telegram_.*:${ESCAPED_SERVER_NAME}\$
      exclusive: true
de.sorunome.msc2409.push_ephemeral: true
receive_ephemeral: true
EOF
	)"
	TELEGRAM_REGISTRATION_INDENTED="$(printf '%s\n' "${TELEGRAM_REGISTRATION}" | sed 's/^/    /')"

	TELEGRAM_SECRETS="$(
		cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: pg-telegrambridge
  namespace: postgres
type: kubernetes.io/basic-auth
stringData:
  username: telegrambridge
  password: ${PG_TELEGRAMBRIDGE}
---
apiVersion: v1
kind: Secret
metadata:
  name: mautrix-telegram
  namespace: bridges
type: Opaque
stringData:
  database-uri: postgres://telegrambridge:${PG_TELEGRAMBRIDGE}@${PG_HOST}:5432/telegrambridge?sslmode=require
  api-id: "${TELEGRAM_API_ID}"
  api-hash: "${TELEGRAM_API_HASH}"
  as-token: ${TELEGRAM_AS_TOKEN}
  hs-token: ${TELEGRAM_HS_TOKEN}
---
apiVersion: v1
kind: Secret
metadata:
  name: mautrix-telegram-registration
  namespace: matrix
type: Opaque
stringData:
  registration.yaml: |
${TELEGRAM_REGISTRATION_INDENTED}
EOF
	)"
	emit mautrix-telegram.sops.yaml "${TELEGRAM_SECRETS}"
fi

sync_kustomization
echo "Done. Secret set ${SECRET_SET} for server_name=${SERVER_NAME} (files skipped above were kept as-is)."
