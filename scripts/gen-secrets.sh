#!/usr/bin/env bash
# Generate the platform's SOPS-encrypted Secrets from scratch for a given server_name:
# fresh Postgres role passwords, appservice registration tokens, and the derived connection
# URLs — every value consistent across the files that share it. Encrypts in place with sops
# (age recipient from .sops.yaml) so only ciphertext ever exists in the working tree.
#
#   scripts/gen-secrets.sh fgentic.localhost local  # k3d cluster
#   scripts/gen-secrets.sh fgentic.fmind.ai gcp      # reference deployment
#
# Files land in clusters/<env>/secrets/ (each cluster owns its secret set — the registration
# regexes embed the server_name and credentials never span environments) and MUST be committed:
# Flux applies them from git. Only SOPS ciphertext is ever written.
# Existing *.sops.yaml files are left untouched unless --force is passed (regenerating the
# registration invalidates the one Synapse has loaded — restart ESS after rotating).
# The model key for agentgateway is read from $GEMINI_API_KEY if set (placeholder otherwise —
# Vertex AI/ADC setups do not need it).
set -euo pipefail

SERVER_NAME="${1:?usage: gen-secrets.sh <server_name> <local|gcp> [--force]}"
ENV="${2:?usage: gen-secrets.sh <server_name> <local|gcp> [--force]}"
FORCE="${3:-}"
DIR="clusters/${ENV}/secrets"
mkdir -p "${DIR}"
ESCAPED_SERVER_NAME="${SERVER_NAME//./\\.}"

PG_HOST="platform-pg-rw.postgres.svc.cluster.local"
BRIDGE_URL="http://matrix-a2a-bridge.bridge.svc.cluster.local:29331"

emit() { # emit <file> <content>: skip if present (unless --force), else write + encrypt
  local file
  file="${DIR}/$1"
  if [ -f "${file}" ] && [ "${FORCE}" != "--force" ]; then
    echo "skip (exists): ${file}"
    return
  fi
  printf '%s\n' "$2" > "${file}"
  sops -e -i "${file}"
  echo "wrote (encrypted): ${file}"
}

PG_SYNAPSE="$(openssl rand -hex 24)"
PG_MAS="$(openssl rand -hex 24)"
PG_BRIDGE="$(openssl rand -hex 24)"
PG_KAGENT="$(openssl rand -hex 24)"
AS_TOKEN="$(openssl rand -hex 32)"
HS_TOKEN="$(openssl rand -hex 32)"
MODEL_KEY="${GEMINI_API_KEY:-unused-ambient-auth}"

PG_ROLES="$(cat <<EOF
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

AGW_MODEL="$(cat <<EOF
apiVersion: v1
kind: Secret
metadata:
  name: model-secret
  namespace: agentgateway-system
type: Opaque
stringData:
  Authorization: ${MODEL_KEY}
EOF
)"
emit agentgateway-model.sops.yaml "${AGW_MODEL}"

KAGENT="$(cat <<EOF
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

BRIDGE_DB="$(cat <<EOF
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

REGISTRATION="$(cat <<EOF
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

REGISTRATION_SECRETS="$(cat <<EOF
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

echo "Done. Secrets for server_name=${SERVER_NAME} (files skipped above were kept as-is)."
