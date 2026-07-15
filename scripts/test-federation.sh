#!/usr/bin/env bash
# Offline contract checks for the disposable Matrix federation and cross-org A2A lab.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-federation-check.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

assert_yq() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq --exit-status "${expression}" "${document}" >/dev/null || fail "${message}"
}

for command in awk base64 cmp cp cut flux git jq kubectl mise openssl rg tr yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly LIFECYCLE="${ROOT_DIR}/scripts/federation.sh"
readonly SEED="${ROOT_DIR}/scripts/seed-federation.sh"
readonly RELOAD="${ROOT_DIR}/scripts/reload-federation-policy.sh"
readonly CLUSTER_OVERLAY="${ROOT_DIR}/clusters/federation"
readonly CONSTRAINED_OVERLAY="${ROOT_DIR}/clusters/federation-constrained"
readonly FEDERATION_ROOT="${ROOT_DIR}/infra/federation"
readonly CONSTRAINED_COMPONENT="${FEDERATION_ROOT}/constrained"
readonly RESOURCE_TRACE="${ROOT_DIR}/scripts/lib/federation-resources.sh"
readonly DEMO_CLUSTER="${ROOT_DIR}/scripts/lib/demo-cluster.sh"
readonly POLICY_APP="${ROOT_DIR}/apps/synapse-federation-policy"
readonly POLICY_DOCUMENT="${POLICY_APP}/policy/policy.json"
readonly POLICY_MODULE="${POLICY_APP}/src/fgentic_federation_policy/__init__.py"
readonly MATRIX_A_COMPONENT="${FEDERATION_ROOT}/matrix-a/kustomization.yaml"
readonly MATRIX_B_LAYER="${FEDERATION_ROOT}/matrix-b"
readonly MATRIX_C_LAYER="${FEDERATION_ROOT}/matrix-c"
readonly GATEWAY_COMPONENT="${FEDERATION_ROOT}/gateway/kustomization.yaml"
readonly NAMESPACE_COMPONENT="${FEDERATION_ROOT}/namespaces"
readonly POSTGRES_COMPONENT="${FEDERATION_ROOT}/postgres"
readonly DELEGATION_COMPONENT="${FEDERATION_ROOT}/delegation"
readonly AGENT_CARD_TEMPLATE="${DELEGATION_COMPONENT}/agent-card.json"
readonly AGENT_CARD_SIGNER="${ROOT_DIR}/scripts/sign-agent-card.sh"
readonly -a DEMO_SOURCES=(
	"${ROOT_DIR}/scripts/demo.sh"
	"${ROOT_DIR}/scripts/lib.sh"
	"${ROOT_DIR}/scripts/lib/demo-config.sh"
	"${DEMO_CLUSTER}"
	"${RESOURCE_TRACE}"
	"${ROOT_DIR}/scripts/lib/demo-secrets.sh"
	"${ROOT_DIR}/scripts/lib/demo-federation.sh"
)
readonly -a SEED_SOURCES=(
	"${SEED}"
	"${ROOT_DIR}/scripts/lib.sh"
	"${ROOT_DIR}/scripts/lib/federation-a2a.sh"
	"${ROOT_DIR}/scripts/lib/federation-matrix.sh"
	"${ROOT_DIR}/scripts/lib/federation-signing.sh"
)


# shellcheck source=scripts/lib/federation-contract-topology.sh
source "${ROOT_DIR}/scripts/lib/federation-contract-topology.sh"

# shellcheck source=scripts/lib/federation-contract-constrained.sh
source "${ROOT_DIR}/scripts/lib/federation-contract-constrained.sh"

# shellcheck source=scripts/lib/federation-contract-policy.sh
source "${ROOT_DIR}/scripts/lib/federation-contract-policy.sh"

# shellcheck source=scripts/lib/federation-contract-signing.sh
source "${ROOT_DIR}/scripts/lib/federation-contract-signing.sh"

# shellcheck source=scripts/lib/federation-contract-reload.sh
source "${ROOT_DIR}/scripts/lib/federation-contract-reload.sh"

# shellcheck source=scripts/lib/federation-contract-acceptance.sh
source "${ROOT_DIR}/scripts/lib/federation-contract-acceptance.sh"

check_federation_topology
check_federation_constrained
check_federation_policy
check_federation_signing
check_federation_reload
check_federation_acceptance

echo 'Federation topology and lifecycle contracts passed.'
