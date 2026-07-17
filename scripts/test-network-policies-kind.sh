#!/usr/bin/env bash
# Run the runtime NetworkPolicy probes against a disposable kind cluster, then prove that deleting
# a load-bearing policy makes the same conformance test fail. The disposable kind cluster uses
# Calico for both networking and full ingress/egress Kubernetes NetworkPolicy enforcement.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-network-policy}"
readonly KIND_CONFIG="${KIND_CONFIG:-${ROOT_DIR}/scripts/testdata/network-policy-kind.yaml}"
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
readonly CALICO_VERSION=v3.32.1
readonly CALICO_MANIFEST_SHA256=a1df919d9721cf667accdc3e72848911b0cb25cfab7d2478ad0c996302c95744
readonly FIXTURE_MANIFEST="${ROOT_DIR}/scripts/testdata/network-policy-conformance.yaml"
readonly CONFORMANCE_SCRIPT="${ROOT_DIR}/scripts/test-network-policies.sh"
readonly CONNECTOR_NETWORK_POLICY_SOURCE="${ROOT_DIR}/infra/knowledge/connectors/git-markdown-runtime/networkpolicy.yaml"
readonly DIAGNOSTICS_DIR="${NETWORK_POLICY_DIAGNOSTICS_DIR:-${TMPDIR:-/tmp}/fgentic-network-policy-diagnostics}"
readonly EGRESS_TARGET_HOST="egress-target.network-policy-target.svc.cluster.local"
readonly EGRESS_TARGET_PORT=8443
KUBECONFIG="$(mktemp -t fgentic-network-policy-kind.XXXXXX)"
export KUBECONFIG

for command in curl docker flux jq kind kubectl; do
  if ! command -v "${command}" >/dev/null 2>&1; then
    echo "error: required command not found: ${command}" >&2
    exit 2
  fi
done

sha256_file() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | awk '{print $1}'
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | awk '{print $1}'
	else
		echo "error: required command not found: sha256sum or shasum" >&2
		return 2
	fi
}

assert_dns_reachable() {
	local namespace="$1"
	local pod="$2"
	local host="$3"
	if ! kubectl --namespace "${namespace}" exec "${pod}" -- nslookup "${host}" >/dev/null; then
		echo "error: ${namespace}/${pod}: expected DNS lookup for ${host} to be reachable" >&2
		return 1
	fi
	echo "pass: ${namespace}/${pod}: DNS lookup for ${host} is reachable"
}

assert_connection() {
	local namespace="$1"
	local pod="$2"
	local host="$3"
	local port="$4"
	local expectation="$5"
	local observed

	case "${expectation}" in
	reachable | denied) ;;
	*)
		echo "error: invalid connection expectation: ${expectation}" >&2
		return 2
		;;
	esac

	observed="$(
		kubectl --namespace "${namespace}" exec "${pod}" -- \
			sh -ec "if nc -z -w 5 \"\$1\" \"\$2\"; then printf reachable; else printf denied; fi" \
			-- "${host}" "${port}"
	)"
	if [[ "${observed}" != "${expectation}" ]]; then
		echo "error: ${namespace}/${pod}: expected ${host}:${port} to be ${expectation}, got ${observed}" >&2
		return 1
	fi
	echo "pass: ${namespace}/${pod}: ${host}:${port} is ${expectation}"
}

mkdir -p "${DIAGNOSTICS_DIR}"
readonly CALICO_MANIFEST="${DIAGNOSTICS_DIR}/calico-${CALICO_VERSION}.yaml"
readonly CONNECTOR_NETWORK_POLICY="${DIAGNOSTICS_DIR}/knowledge-connector-networkpolicy.yaml"

diagnose() {
  {
    echo "==> Cluster overview"
    kubectl cluster-info || true
    kubectl get nodes,namespaces --output=wide || true
    kubectl get pods,services,networkpolicies --all-namespaces --output=wide || true
    echo "==> Events"
    kubectl get events --all-namespaces --sort-by=.lastTimestamp || true
  } >"${DIAGNOSTICS_DIR}/cluster-overview.log" 2>&1
  kubectl get pods,services,networkpolicies --all-namespaces --output=yaml \
    >"${DIAGNOSTICS_DIR}/resources.yaml" 2>&1 || true
  kubectl describe pods --all-namespaces >"${DIAGNOSTICS_DIR}/pods.describe.log" 2>&1 || true
  kind export logs "${DIAGNOSTICS_DIR}/kind" --name "${CLUSTER_NAME}" \
    >"${DIAGNOSTICS_DIR}/kind-export.log" 2>&1 || true
}

cleanup() {
  local result=$?
  trap - EXIT
  diagnose
  if [[ "${KEEP_KIND_CLUSTER:-0}" == "1" ]]; then
    echo "==> Keeping kind cluster ${CLUSTER_NAME}; use KUBECONFIG=${KUBECONFIG}"
  else
    kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
    rm -f "${KUBECONFIG}"
  fi
  exit "${result}"
}
trap cleanup EXIT

docker info >/dev/null 2>&1 || {
  echo "error: Docker daemon is not available" >&2
  exit 1
}

{
	date -u +"started_at=%Y-%m-%dT%H:%M:%SZ"
  docker version --format 'docker_client={{.Client.Version}} docker_server={{.Server.Version}}'
  kind version
  kubectl version --client=true
  echo "calico=${CALICO_VERSION}"
} >"${DIAGNOSTICS_DIR}/versions.log" 2>&1

echo "==> Fetching the checksum-pinned Calico manifest"
curl --fail --silent --show-error --location \
  --output "${CALICO_MANIFEST}" \
  "https://raw.githubusercontent.com/projectcalico/calico/${CALICO_VERSION}/manifests/calico.yaml"
calico_manifest_digest="$(sha256_file "${CALICO_MANIFEST}")"
if [[ "${calico_manifest_digest}" != "${CALICO_MANIFEST_SHA256}" ]]; then
  echo "error: Calico manifest checksum mismatch: ${calico_manifest_digest}" >&2
  exit 1
fi

# This cluster is disposable test state. A dedicated kubeconfig prevents kind from changing the
# developer's active context (normally the shared k3d cluster).
kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
echo "==> Creating policy-capable kind cluster ${CLUSTER_NAME}"
kind create cluster \
  --name "${CLUSTER_NAME}" \
  --image "${KIND_NODE_IMAGE}" \
  --config "${KIND_CONFIG}"
api_endpoint_ip="$(
  docker inspect "${CLUSTER_NAME}-control-plane" |
    jq -er '
      .[0].NetworkSettings.Networks
      | to_entries
      | map(select(.value.IPAddress != ""))
      | if length == 1 then .[0].value.IPAddress else error("ambiguous API endpoint") end
    '
)"
kubernetes_api_egress_cidr="${api_endpoint_ip}/32" kubernetes_api_egress_port=6443 \
  flux envsubst --strict <"${CONNECTOR_NETWORK_POLICY_SOURCE}" >"${CONNECTOR_NETWORK_POLICY}"
echo "==> Installing Calico ${CALICO_VERSION}"
kubectl apply --filename "${CALICO_MANIFEST}"
kubectl --namespace kube-system rollout status daemonset/calico-node --timeout=240s
kubectl --namespace kube-system rollout status deployment/calico-kube-controllers --timeout=240s
kubectl wait --for=condition=Ready nodes --all --timeout=120s

echo "==> Applying minimal endpoints and the repository's real policies"
kubectl apply --filename "${FIXTURE_MANIFEST}"
kubectl apply --filename "${ROOT_DIR}/infra/kagent/networkpolicy.yaml"
kubectl apply --filename "${ROOT_DIR}/infra/agentgateway/networkpolicy.yaml"
kubectl apply --filename "${ROOT_DIR}/infra/models/vllm/networkpolicy.yaml"
kubectl apply --filename \
  "${ROOT_DIR}/infra/agentgateway/providers/profiles/vllm/networkpolicy.yaml"
kubectl apply --filename "${ROOT_DIR}/infra/knowledge/base/networkpolicy.yaml"
kubectl apply --filename "${CONNECTOR_NETWORK_POLICY}"
kubectl wait \
  --for=condition=Ready \
  pods \
  --all-namespaces \
  --selector=fgentic.dev/network-policy-fixture=server \
  --timeout=90s
kubectl wait \
  --for=condition=Ready \
  pods \
  --all-namespaces \
  --selector=fgentic.dev/network-policy-fixture=client \
  --timeout=90s

# NetworkPolicy has no status condition. Once Calico is ready, allow one reconciliation interval
# before the first negative probe so startup latency cannot look like a policy regression.
sleep 2

echo "==> Running NetworkPolicy conformance"
NETWORK_POLICY_EGRESS_TARGET_HOST="${EGRESS_TARGET_HOST}" \
  NETWORK_POLICY_EGRESS_TARGET_PORT="${EGRESS_TARGET_PORT}" \
  NETWORK_POLICY_POD_TIMEOUT_SECONDS=90 \
  NETWORK_POLICY_REQUIRE_TEST_FIXTURES=true \
  bash "${CONFORMANCE_SCRIPT}" --require-vllm \
  2>&1 | tee "${DIAGNOSTICS_DIR}/baseline.log"

echo "==> Running knowledge-ingestion NetworkPolicy conformance"
assert_dns_reachable knowledge knowledge-ingestion kubernetes.default.svc.cluster.local
assert_connection knowledge knowledge-ingestion \
	platform-pg-rw.postgres.svc.cluster.local 5432 reachable
assert_connection knowledge knowledge-ingestion \
	agentgateway-proxy.agentgateway-system.svc.cluster.local 8082 reachable
assert_connection knowledge knowledge-ingestion \
	agentgateway-proxy.agentgateway-system.svc.cluster.local 8080 denied
assert_connection knowledge knowledge-ingestion \
	knowledge-embeddings.models.svc.cluster.local 8000 denied
assert_connection knowledge knowledge-ingestion \
	unrelated-db.postgres.svc.cluster.local 5432 denied
assert_connection knowledge knowledge-ingestion \
	"${EGRESS_TARGET_HOST}" "${EGRESS_TARGET_PORT}" denied
assert_connection bridge knowledge-unrelated-caller \
	agentgateway-proxy.agentgateway-system.svc.cluster.local 8082 denied

echo "==> Running knowledge Git/Markdown acquisition NetworkPolicy conformance"
assert_dns_reachable knowledge knowledge-git-markdown-connector \
	kubernetes.default.svc.cluster.local
assert_connection knowledge knowledge-git-markdown-connector \
	kubernetes.default.svc.cluster.local 443 reachable
assert_connection knowledge knowledge-git-markdown-connector \
	source-controller.flux-system.svc.cluster.local 80 reachable
assert_connection knowledge knowledge-git-markdown-connector \
	source-controller.flux-system.svc.cluster.local 8080 denied
assert_connection knowledge knowledge-git-markdown-connector \
	platform-pg-rw.postgres.svc.cluster.local 5432 denied
assert_connection knowledge knowledge-git-markdown-connector \
	agentgateway-proxy.agentgateway-system.svc.cluster.local 8082 denied
assert_connection knowledge knowledge-git-markdown-connector \
	knowledge-embeddings.models.svc.cluster.local 8000 denied
assert_connection knowledge knowledge-git-markdown-connector \
	"${EGRESS_TARGET_HOST}" "${EGRESS_TARGET_PORT}" denied

echo "==> Proving deletion of the acquisition policy opens its denied external edge"
kubectl --namespace knowledge delete networkpolicy knowledge-git-markdown-connector
sleep 2
assert_connection knowledge knowledge-git-markdown-connector \
	"${EGRESS_TARGET_HOST}" "${EGRESS_TARGET_PORT}" reachable
kubectl apply --filename "${CONNECTOR_NETWORK_POLICY}"
sleep 2
assert_connection knowledge knowledge-git-markdown-connector \
	"${EGRESS_TARGET_HOST}" "${EGRESS_TARGET_PORT}" denied

echo "==> Proving deletion of kagent-allow-platform opens the unauthorized path"
assert_connection postgres unrelated-db \
	kagent-controller.kagent.svc.cluster.local 8083 denied
kubectl --namespace kagent delete networkpolicy kagent-allow-platform
# NetworkPolicy has no deletion status; allow Calico one reconciliation interval before the
# unauthorized probe so a stale rule cannot make the mutation look safe.
sleep 2
assert_connection postgres unrelated-db \
	kagent-controller.kagent.svc.cluster.local 8083 reachable

echo "NetworkPolicy deletion guard passed"
