#!/usr/bin/env bash
# Capture content needed to diagnose a failed disposable demo without exporting its kubeconfig.
set -uo pipefail

readonly phase="${1:-failure}"
readonly cluster_name="${FGENTIC_DEMO_CLUSTER:-fgentic-demo}"
readonly diagnostics_dir="${SMOKE_DIAGNOSTICS_DIR:-${TMPDIR:-/tmp}/fgentic-smoke-diagnostics}"

if [[ ! "${phase}" =~ ^[a-z0-9-]+$ ]]; then
	echo "error: invalid diagnostics phase: ${phase}" >&2
	exit 2
fi
mkdir -p "${diagnostics_dir}/pod-logs"

for command in docker flux k3d kubectl; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "${command} is unavailable" >"${diagnostics_dir}/${phase}-${command}-unavailable.txt"
		exit 0
	fi
done

kubeconfig="$(mktemp "${TMPDIR:-/tmp}/fgentic-smoke-kubeconfig.XXXXXX")"
cleanup() {
	rm -f "${kubeconfig}"
}
trap cleanup EXIT INT TERM

if ! k3d kubeconfig get "${cluster_name}" >"${kubeconfig}" 2>"${diagnostics_dir}/${phase}-kubeconfig.log"; then
	echo "Demo cluster ${cluster_name} is unavailable." >>"${diagnostics_dir}/${phase}-kubeconfig.log"
	exit 0
fi
export KUBECONFIG="${kubeconfig}"

{
	echo "==> Cluster"
	kubectl --request-timeout=20s cluster-info || true
	kubectl --request-timeout=20s get nodes,namespaces --output=wide || true
	echo "==> Workloads"
	kubectl --request-timeout=20s get pods,services --all-namespaces --output=wide || true
	echo "==> Flux"
	flux get all --all-namespaces || true
	echo "==> Events"
	kubectl --request-timeout=20s get events --all-namespaces --sort-by=.lastTimestamp || true
	echo "==> Docker"
	docker ps --all --filter "name=k3d-${cluster_name}" || true
} >"${diagnostics_dir}/${phase}-overview.log" 2>&1

kubectl --request-timeout=30s describe pods --all-namespaces \
	>"${diagnostics_dir}/${phase}-pods.describe.log" 2>&1 || true

while IFS=$'\t' read -r namespace pod; do
	[[ -n "${namespace}" && -n "${pod}" ]] || continue
	kubectl --request-timeout=30s --namespace "${namespace}" logs "${pod}" \
		--all-containers=true --prefix=true \
		>"${diagnostics_dir}/pod-logs/${phase}-${namespace}--${pod}.log" 2>&1 || true
	kubectl --request-timeout=30s --namespace "${namespace}" logs "${pod}" \
		--all-containers=true --prefix=true --previous=true \
		>"${diagnostics_dir}/pod-logs/${phase}-${namespace}--${pod}.previous.log" 2>&1 || true
done < <(
	kubectl --request-timeout=30s get pods --all-namespaces \
		--output=jsonpath='{range .items[*]}{.metadata.namespace}{"\t"}{.metadata.name}{"\n"}{end}' \
		2>/dev/null || true
)
