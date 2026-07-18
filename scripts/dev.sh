#!/usr/bin/env bash
# Fast, repo-owned bridge loop on the disposable demo cluster.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly CLUSTER_NAME="${FGENTIC_DEMO_CLUSTER:-fgentic-demo}"
readonly OWNER_LABEL="true"
readonly PROFILE="demo"

# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
# shellcheck source=scripts/lib/demo-cluster.sh
source "${ROOT_DIR}/scripts/lib/demo-cluster.sh"

KUBECONFIG_FILE=""

cleanup() {
	[ -z "${KUBECONFIG_FILE}" ] || rm -f "${KUBECONFIG_FILE}"
}
trap cleanup EXIT INT TERM

usage() {
	cat <<'EOF'
usage: scripts/dev.sh up|reload|status|stop|down

Commands:
  up      Create and seed the lightweight demo once, or start and reuse it without reconciling.
  reload  Build, import, and restart only the bridge in the owned demo cluster.
  status  Report whether the owned demo cluster and bridge are ready.
  stop    Stop the demo cluster while preserving its containers and images.
  down    Delete only the owned demo cluster and its locally built images.

Environment:
  FGENTIC_DEMO_CLUSTER  k3d cluster name (default: fgentic-demo)
  FGENTIC_DEMO_STATE_DIR
                        optional lifecycle-state root; defaults to the user state directory

The script always uses a temporary kubeconfig. It never reads, changes, or switches the user's
default Kubernetes context. Run `mise run demo:up` after manifest or profile changes to reconcile
the local Git snapshot and repeat the full seeded acceptance proof.
EOF
}

validate_cluster_name() {
	[[ "${CLUSTER_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,47}$ ]] \
		|| die "invalid FGENTIC_DEMO_CLUSTER"
	case "${CLUSTER_NAME}" in
		fgentic-demo | fgentic-demo-*) ;;
		*) die "FGENTIC_DEMO_CLUSTER must be fgentic-demo or start with fgentic-demo-" ;;
	esac
}

require_runtime() {
	local command
	for command in docker jq k3d; do
		require_command "${command}"
	done
	docker info >/dev/null 2>&1 || die "Docker daemon is not running"
}

require_owned_cluster() {
	cluster_exists || die "${CLUSTER_NAME} does not exist (run 'mise run dev:up')"
	cluster_owned_by_demo \
		|| die "refusing to manage ${CLUSTER_NAME}: it was not created by scripts/demo.sh"
}

cluster_running() {
	k3d cluster list --output json | jq -e --arg name "${CLUSTER_NAME}" '
    any(.[]; .name == $name and .serversRunning == .serversCount and .serversCount > 0)
  ' >/dev/null
}

configure_kubeconfig() {
	KUBECONFIG_FILE="$(mktemp "${TMPDIR:-/tmp}/fgentic-dev-kubeconfig.XXXXXX")"
	k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_FILE}"
	export KUBECONFIG="${KUBECONFIG_FILE}"
}

start_cluster() {
	require_no_pending_teardown 'development reuse'
	require_owned_cluster
	k3d cluster start "${CLUSTER_NAME}" >/dev/null 2>&1 || true
	configure_kubeconfig
	kubectl wait --for=condition=Ready nodes --all --timeout=2m >/dev/null \
		|| die "${CLUSTER_NAME} did not become ready within 2 minutes"
}

print_access() {
	local password provider model
	password="$(bootstrap_secret_value demo-password)"
	provider="$(kubectl --namespace flux-system get configmap platform-settings \
		--output 'go-template={{index .data "llm_provider"}}')"
	model="$(kubectl --namespace flux-system get configmap platform-settings \
		--output 'go-template={{index .data "llm_model"}}')"
	cat <<EOF

Fgentic development cluster is ready.
URL:      https://chat.fgentic.localhost
User:     @alice:fgentic.localhost
Password: ${password}
Room:     #lobby:fgentic.localhost
Provider: ${provider} (${model})
EOF
}

dev_up() {
	require_runtime
	if ! cluster_exists; then
		exec "${ROOT_DIR}/scripts/demo.sh" up
	fi
	start_cluster
	kubectl --namespace bridge rollout status deployment/matrix-a2a-bridge \
		--timeout=2m >/dev/null || die "the demo bridge is not ready"
	print_access
}

dev_reload() {
	local image started_at
	require_runtime
	require_command kubectl
	start_cluster
	image="$(kubectl --namespace bridge get helmrelease matrix-a2a-bridge --output json \
		| jq --exit-status --raw-output '
      .spec.values.image |
      select(.repository == "matrix-a2a-bridge" and .pullPolicy == "Never") |
      "\(.repository):\(.tag)"
    ')" || die "the demo bridge HelmRelease does not request a local image"

	started_at="${SECONDS}"
	docker build --quiet --tag "${image}" \
		--label "dev.fgentic.demo.cluster=${CLUSTER_NAME}" \
		--file "${ROOT_DIR}/apps/matrix-a2a-bridge/Dockerfile" \
		"${ROOT_DIR}/apps/matrix-a2a-bridge" >/dev/null
	# Auto uses direct streaming for a local daemon and the tools node for a remote daemon such as
	# Docker Desktop, avoiding one host-specific choice in the common Linux/macOS path.
	k3d image import --mode auto --cluster "${CLUSTER_NAME}" "${image}" >/dev/null
	kubectl --namespace bridge rollout restart deployment/matrix-a2a-bridge >/dev/null
	kubectl --namespace bridge rollout status deployment/matrix-a2a-bridge \
		--timeout=2m >/dev/null || die "the reloaded demo bridge is not ready"
	echo "Reloaded ${image} in $((SECONDS - started_at))s."
}

dev_status() {
	require_runtime
	if teardown_receipt_exists; then
		exec "${ROOT_DIR}/scripts/demo.sh" status
	fi
	if ! cluster_exists; then
		echo "Fgentic development cluster is not created (run 'mise run dev:up')."
		return
	fi
	require_owned_cluster
	if ! cluster_running; then
		echo "Fgentic development cluster ${CLUSTER_NAME} is stopped (run 'mise run dev:up')."
		return
	fi
	require_command kubectl
	configure_kubeconfig
	local image ready
	image="$(kubectl --namespace bridge get deployment matrix-a2a-bridge \
		--output jsonpath='{.spec.template.spec.containers[0].image}')"
	ready="$(kubectl --namespace bridge get deployment matrix-a2a-bridge \
		--output jsonpath='{.status.readyReplicas}')"
	echo "Fgentic development cluster ${CLUSTER_NAME} is running; bridge ${image} ready=${ready:-0}/1."
}

dev_stop() {
	require_runtime
	require_no_pending_teardown 'development stop'
	require_owned_cluster
	k3d cluster stop "${CLUSTER_NAME}" >/dev/null
	echo "Stopped ${CLUSTER_NAME}; its state and images are preserved."
}

validate_cluster_name
case "${1:-}" in
	up) dev_up ;;
	reload) dev_reload ;;
	status) dev_status ;;
	stop) dev_stop ;;
	down) exec "${ROOT_DIR}/scripts/demo.sh" down ;;
	-h | --help) usage ;;
	*)
		usage >&2
		exit 2
		;;
esac
