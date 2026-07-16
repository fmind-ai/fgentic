#!/usr/bin/env bash
# Credential-free evaluation lifecycle. Flux still renders the canonical HelmReleases, but its
# source is an ephemeral, cluster-local snapshot of this checkout instead of GitHub.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly DEFAULT_CLUSTER_NAME="fgentic-demo"
readonly FEDERATION_CLUSTER_NAME="fgentic-fed"
readonly FEDERATION_LOOPBACK="127.0.0.2"
readonly FEDERATION_POLICY_PATH="apps/synapse-federation-policy/policy/policy.json"
readonly FEDERATION_POLICY_EVENT_TYPE="com.fgentic.blocked"
readonly FEDERATION_AGENT_CARD_TEMPLATE_PATH="infra/federation/delegation/agent-card.json"
readonly FEDERATION_AGENT_CARD_POLICY_PATH="infra/federation/delegation/policies.yaml"
readonly FEDERATION_AGENT_CARD_MARKER="__FGENTIC_SIGNED_AGENT_CARD_JSON__"
readonly FEDERATION_AGENT_CARD_CONFIGMAP="federated-docs-qa-agent-card"
readonly FEDERATION_AGENT_CARD_DEFAULT_KEY_ID="fgentic-org-a-docs-qa-v1"
readonly FEDERATION_USAGE_RECEIPT_DEFAULT_KEY_ID="fgentic-org-a-usage-receipt-v1"
readonly FEDERATION_USAGE_RECEIPT_EXTENSION="https://fgentic.fmind.ai/a2a/extensions/usage-receipt/v1"
readonly MAS_ADMIN_CLIENT_ID="01KX8D3M0AD3M0ADM1NC13NT01"
readonly SOURCE_BASE_IMAGE="alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40"
readonly SOURCE_GIT_PACKAGES="git=2.52.0-r0 git-daemon=2.52.0-r0 busybox-extras=1.37.0-r30"
readonly FLUX_LEADER_ELECTION_LEASE_DURATION="180s"
readonly FLUX_LEADER_ELECTION_RENEW_DEADLINE="170s"
readonly FLUX_LEADER_ELECTION_RETRY_PERIOD="30s"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

usage() {
	cat <<'EOF'
usage: scripts/demo.sh up|status|stop|down

Environment:
  FGENTIC_DEMO_CLUSTER       k3d cluster name (default: fgentic-demo)
  FGENTIC_DEMO_TIMEOUT       reconciliation timeout (default: 15m)
  FGENTIC_DEMO_CACHE_DIR     optional persistent BuildKit cache directory
  FGENTIC_DEMO_STATE_DIR     optional lifecycle-state root; defaults to the user state directory
  FGENTIC_FED_CONSTRAINED    federation profile only: yes enables the opt-in laptop budget
  FGENTIC_FED_NO_PROGRESS_TIMEOUT
                             constrained federation no-progress timeout (default: 20m)
  FGENTIC_FED_MAX_TIMEOUT    constrained federation absolute timeout (default: 60m)
  FGENTIC_FED_TRACE          federation profile only: yes writes a content-free resource trace
  FGENTIC_FED_TRACE_DIR      optional trace parent directory
  FGENTIC_FED_POLICY_PROBE   federation profile only: deny (default) or allow; allow mutates only
                             the ephemeral Git snapshot used by the disposable lab
  FGENTIC_LLM_PROVIDER       demo (default), vllm, vertex, mistral, anthropic,
                             openai, or azure-openai
  FGENTIC_LLM_MODEL          model identifier; required for API profiles except Vertex
  FGENTIC_ALLOW_PAID_PROVIDER=yes
                             required before an API/Vertex profile can make its seed request

Provider-specific settings follow docs/models.md: MISTRAL_API_KEY, ANTHROPIC_API_KEY,
OPENAI_API_KEY, AZURE_OPENAI_API_KEY, GOOGLE_APPLICATION_CREDENTIALS, FGENTIC_GCP_PROJECT,
FGENTIC_VERTEX_REGION, FGENTIC_OPENAI_HOST, and FGENTIC_AZURE_OPENAI_RESOURCE.

The default demo profile is a deterministic in-cluster response stub. It proves the complete
Matrix -> bridge -> agentgateway -> kagent path without a model account or per-token charge; it is
not a language model and is never a production option.
EOF
}


# shellcheck source=scripts/lib/demo-config.sh
source "${ROOT_DIR}/scripts/lib/demo-config.sh"
# shellcheck source=scripts/lib/demo-cluster.sh
source "${ROOT_DIR}/scripts/lib/demo-cluster.sh"
# shellcheck source=scripts/lib/federation-resources.sh
source "${ROOT_DIR}/scripts/lib/federation-resources.sh"
# shellcheck source=scripts/lib/demo-secrets.sh
source "${ROOT_DIR}/scripts/lib/demo-secrets.sh"
# shellcheck source=scripts/lib/demo-federation.sh
source "${ROOT_DIR}/scripts/lib/demo-federation.sh"

if (($# != 1)); then
	usage >&2
	exit 2
fi

PROFILE="${FGENTIC_DEMO_PROFILE:-demo}"
case "${PROFILE}" in
demo)
	CLUSTER_NAME="${FGENTIC_DEMO_CLUSTER:-${DEFAULT_CLUSTER_NAME}}"
	OVERLAY_PATH="clusters/demo"
	PLATFORM_SETTINGS_PATH="${OVERLAY_PATH}/platform-settings.yaml"
	SEED_SCRIPT="scripts/seed-demo.sh"
	OWNER_LABEL="true"
	;;
federation)
	CLUSTER_NAME="${FGENTIC_DEMO_CLUSTER:-${FEDERATION_CLUSTER_NAME}}"
	FEDERATION_CONSTRAINED="${FGENTIC_FED_CONSTRAINED:-no}"
	OVERLAY_PATH="clusters/federation"
	[ "${FEDERATION_CONSTRAINED}" = "yes" ] && OVERLAY_PATH="clusters/federation-constrained"
	PLATFORM_SETTINGS_PATH="clusters/federation/platform-settings.yaml"
	SEED_SCRIPT="scripts/seed-federation.sh"
	OWNER_LABEL="federation"
	;;
*) die "unsupported internal evaluation profile: ${PROFILE}" ;;
esac
DEMO_TIMEOUT="${FGENTIC_DEMO_TIMEOUT:-15m}"
FEDERATION_POLICY_PROBE="${FGENTIC_FED_POLICY_PROBE:-deny}"
FEDERATION_CONSTRAINED="${FEDERATION_CONSTRAINED:-no}"
FEDERATION_CAPACITY_MODE=standard
if [ "${PROFILE}" = federation ] && [ "${FEDERATION_CONSTRAINED}" = yes ]; then
	FEDERATION_CAPACITY_MODE=constrained
fi
FEDERATION_NO_PROGRESS_TIMEOUT="${FGENTIC_FED_NO_PROGRESS_TIMEOUT:-20m}"
FEDERATION_MAX_TIMEOUT="${FGENTIC_FED_MAX_TIMEOUT:-60m}"
FEDERATION_GATEWAY_IP=""
BRIDGE_TAG="demo-${RANDOM}-$$"
SOURCE_IMAGE="fgentic-demo-source-${CLUSTER_NAME}:${BRIDGE_TAG}"
SOURCE_REVISION=""
BRIDGE_IMAGE="matrix-a2a-bridge:${BRIDGE_TAG}"
[[ "${CLUSTER_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,47}$ ]] || die "invalid FGENTIC_DEMO_CLUSTER"
if [ "${PROFILE}" = "demo" ]; then
	case "${CLUSTER_NAME}" in
	fgentic-demo | fgentic-demo-*) ;;
	*) die "FGENTIC_DEMO_CLUSTER must be fgentic-demo or start with fgentic-demo-" ;;
	esac
elif [ "${CLUSTER_NAME}" != "${FEDERATION_CLUSTER_NAME}" ]; then
	die "the federation profile cluster must be ${FEDERATION_CLUSTER_NAME}"
fi
[[ "${DEMO_TIMEOUT}" =~ ^[1-9][0-9]*[smh]$ ]] || die "invalid FGENTIC_DEMO_TIMEOUT"
if [ "${PROFILE}" = "federation" ]; then
	case "${FEDERATION_POLICY_PROBE}" in
	allow | deny) ;;
	*) die "FGENTIC_FED_POLICY_PROBE must be allow or deny" ;;
	esac
	case "${FEDERATION_CONSTRAINED}" in
	yes | no) ;;
	*) die "FGENTIC_FED_CONSTRAINED must be yes or no" ;;
	esac
	case "${FGENTIC_FED_TRACE:-no}" in
	yes | no) ;;
	*) die "FGENTIC_FED_TRACE must be yes or no" ;;
	esac
	[[ "${FEDERATION_NO_PROGRESS_TIMEOUT}" =~ ^[1-9][0-9]*[smh]$ ]] ||
		die "invalid FGENTIC_FED_NO_PROGRESS_TIMEOUT"
	[[ "${FEDERATION_MAX_TIMEOUT}" =~ ^[1-9][0-9]*[smh]$ ]] ||
		die "invalid FGENTIC_FED_MAX_TIMEOUT"
	FEDERATION_NO_PROGRESS_SECONDS="$(timeout_seconds "${FEDERATION_NO_PROGRESS_TIMEOUT}")"
	FEDERATION_MAX_SECONDS="$(timeout_seconds "${FEDERATION_MAX_TIMEOUT}")"
	if [ "${FEDERATION_CONSTRAINED}" = "yes" ] &&
		((FEDERATION_NO_PROGRESS_SECONDS >= FEDERATION_MAX_SECONDS)); then
		die "FGENTIC_FED_NO_PROGRESS_TIMEOUT must be shorter than FGENTIC_FED_MAX_TIMEOUT"
	fi
fi

case "$1" in
up) demo_up ;;
status) demo_status ;;
stop) demo_stop ;;
down) demo_down ;;
-h | --help)
	usage
	;;
*)
	usage >&2
	exit 2
	;;
esac
