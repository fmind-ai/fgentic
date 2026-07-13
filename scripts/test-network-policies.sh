#!/usr/bin/env bash
# Prove the load-bearing kagent, governed MCP, agentgateway, and optional vLLM policies at runtime.
# A probe Pod exits successfully only when the observed connection result matches its expectation.
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
usage: scripts/test-network-policies.sh [--require-vllm]

The active kubectl context must point at an already reconciled Fgentic cluster. The vLLM probes
run automatically when the models namespace and engine Service exist; --require-vllm fails if
that profile is absent. Every temporary Pod is restricted/non-root and removed on exit.
EOF
}

REQUIRE_VLLM=false
SKIP_KAGENT_POLICY_REQUIRE="${NETWORK_POLICY_SKIP_KAGENT_POLICY_REQUIRE:-false}"
case "${1:-}" in
  "") ;;
  --require-vllm) REQUIRE_VLLM=true ;;
  -h | --help)
    usage
    exit 0
    ;;
  *)
    usage
    exit 2
    ;;
esac
if [ "$#" -gt 1 ]; then
  usage
  exit 2
fi

case "${SKIP_KAGENT_POLICY_REQUIRE}" in
  true | false) ;;
  *)
    echo "error: NETWORK_POLICY_SKIP_KAGENT_POLICY_REQUIRE must be true or false" >&2
    exit 2
    ;;
esac

if ! command -v kubectl >/dev/null 2>&1; then
  echo "error: required command not found: kubectl" >&2
  exit 2
fi

PROBE_NAMESPACE="${NETWORK_POLICY_PROBE_NAMESPACE:-fgentic-netpol-probe-$$}"
PROBE_IMAGE="docker.io/library/busybox:1.37.0@sha256:9532d8c39891ca2ecde4d30d7710e01fb739c87a8b9299685c63704296b16028"
POD_TIMEOUT_SECONDS="${NETWORK_POLICY_POD_TIMEOUT_SECONDS:-60}"
created_namespace=false
probe_pods=()

if ! [[ "${POD_TIMEOUT_SECONDS}" =~ ^[1-9][0-9]*$ ]]; then
  echo "error: NETWORK_POLICY_POD_TIMEOUT_SECONDS must be a positive integer" >&2
  exit 2
fi

cleanup() {
  local item namespace pod
  for item in "${probe_pods[@]:-}"; do
    namespace="${item%%/*}"
    pod="${item#*/}"
    kubectl -n "${namespace}" delete pod "${pod}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  done
  if [ "${created_namespace}" = true ]; then
    kubectl delete namespace "${PROBE_NAMESPACE}" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  fi
}
trap cleanup EXIT INT TERM

if ! kubectl get namespace "${PROBE_NAMESPACE}" >/dev/null 2>&1; then
  kubectl create namespace "${PROBE_NAMESPACE}" >/dev/null
  created_namespace=true
fi
kubectl label namespace "${PROBE_NAMESPACE}" \
  pod-security.kubernetes.io/enforce=restricted \
  pod-security.kubernetes.io/audit=restricted \
  pod-security.kubernetes.io/warn=restricted \
  --overwrite >/dev/null

require_resource() {
  local kind="$1"
  local namespace="$2"
  local name="$3"
  if ! kubectl -n "${namespace}" get "${kind}" "${name}" >/dev/null 2>&1; then
    echo "error: required ${kind} ${namespace}/${name} is absent; reconcile the cluster first" >&2
    exit 1
  fi
}

run_probe() {
  local namespace="$1"
  local name="$2"
  local host="$3"
  local port="$4"
  local expectation="$5"
  local labels="${6:-}"
  local command

  case "${expectation}" in
    reachable)
      command="nc -z -w 5 '${host}' '${port}'"
      ;;
    denied)
      command="if nc -z -w 5 '${host}' '${port}'; then echo 'unexpected connection succeeded' >&2; exit 1; else echo 'connection denied as expected'; fi"
      ;;
    *)
      echo "error: invalid probe expectation: ${expectation}" >&2
      exit 2
      ;;
  esac

  kubectl -n "${namespace}" delete pod "${name}" --ignore-not-found --wait=true >/dev/null
  probe_pods+=("${namespace}/${name}")
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: fgentic-netpol-probe
${labels}
spec:
  automountServiceAccountToken: false
  restartPolicy: Never
  securityContext:
    runAsNonRoot: true
    runAsUser: 65532
    runAsGroup: 65532
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: probe
      image: ${PROBE_IMAGE}
      imagePullPolicy: IfNotPresent
      command: [sh, -ec]
      args:
        - |
          ${command}
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: [ALL]
      resources:
        requests:
          cpu: 1m
          memory: 2Mi
        limits:
          cpu: 20m
          memory: 16Mi
EOF

  local deadline phase
  deadline=$((SECONDS + POD_TIMEOUT_SECONDS))
  phase=""
  while ((SECONDS < deadline)); do
    phase="$(kubectl -n "${namespace}" get pod "${name}" -o jsonpath='{.status.phase}')"
    case "${phase}" in
      Succeeded) break ;;
      Failed) break ;;
      Pending | Running | Unknown | "") ;;
      *)
        echo "error: ${namespace}/${name}: unexpected Pod phase ${phase}" >&2
        break
        ;;
    esac
    sleep 1
  done
  if [ "${phase}" != "Succeeded" ]; then
    kubectl -n "${namespace}" get pod "${name}" -o wide >&2 || true
    kubectl -n "${namespace}" logs "${name}" >&2 || true
    echo "error: ${namespace}/${name}: expected ${host}:${port} to be ${expectation}" >&2
    return 1
  fi
  echo "pass: ${namespace}/${name}: ${host}:${port} is ${expectation}"
}

if [ "${SKIP_KAGENT_POLICY_REQUIRE}" = false ]; then
  require_resource networkpolicy kagent kagent-allow-platform
fi
require_resource networkpolicy kagent agent-zoo-egress
require_resource service kagent kagent-controller
require_resource service kagent kagent-tools
require_resource networkpolicy agentgateway-system agentgateway-allow-agents
require_resource networkpolicy agentgateway-system agentgateway-allow-xds
require_resource service agentgateway-system agentgateway
require_resource service agentgateway-system agentgateway-proxy

# kagent is reachable only from the namespaces listed in its load-bearing ingress policy.
run_probe "${PROBE_NAMESPACE}" netpol-kagent-denied \
  kagent-controller.kagent.svc.cluster.local 8083 denied
run_probe bridge netpol-kagent-bridge \
  kagent-controller.kagent.svc.cluster.local 8083 reachable
run_probe agentgateway-system netpol-kagent-gateway \
  kagent-controller.kagent.svc.cluster.local 8083 reachable

# Managed Agent pods cannot bypass the authenticated MCP route, while the governed gateway can
# reach the same upstream tool server. The agent label is the exact selector applied by kagent's
# Agent deployment template; the controller remains outside that selector for tool discovery.
run_probe kagent netpol-mcp-agent-denied \
  kagent-tools.kagent.svc.cluster.local 8084 denied \
  $'    fgentic.dev/agent-zoo: "true"'
run_probe agentgateway-system netpol-mcp-gateway \
  kagent-tools.kagent.svc.cluster.local 8084 reachable \
  '    app.kubernetes.io/name: agentgateway-proxy'

# agentgateway accepts the bridge and kagent data planes, but no arbitrary namespace.
run_probe "${PROBE_NAMESPACE}" netpol-gateway-denied \
  agentgateway-proxy.agentgateway-system.svc.cluster.local 8080 denied
run_probe bridge netpol-gateway-bridge \
  agentgateway-proxy.agentgateway-system.svc.cluster.local 8080 reachable
run_probe kagent netpol-gateway-kagent \
  agentgateway-proxy.agentgateway-system.svc.cluster.local 8080 reachable

# The proxy is the sole xDS client. Without this same-namespace exception the controller can be
# healthy while every proxy fails its startup probe, and an arbitrary workload must remain denied.
run_probe "${PROBE_NAMESPACE}" netpol-xds-denied \
  agentgateway.agentgateway-system.svc.cluster.local 9978 denied
run_probe agentgateway-system netpol-xds-proxy \
  agentgateway.agentgateway-system.svc.cluster.local 9978 reachable \
  '    app.kubernetes.io/name: agentgateway-proxy'

VLLM_SERVICE="vllm-qwen2-5-0-5b-engine-service"
if kubectl get namespace models >/dev/null 2>&1 \
  && kubectl -n models get service "${VLLM_SERVICE}" >/dev/null 2>&1; then
  require_resource networkpolicy models models-default-deny
  require_resource networkpolicy models vllm-allow-platform-ingress
  require_resource networkpolicy agentgateway-system agentgateway-vllm-egress

  run_probe "${PROBE_NAMESPACE}" netpol-vllm-denied \
    "${VLLM_SERVICE}.models.svc.cluster.local" 80 denied
  # The vLLM policy deliberately requires both the gateway namespace and proxy workload label.
  run_probe agentgateway-system netpol-vllm-gateway \
    "${VLLM_SERVICE}.models.svc.cluster.local" 80 reachable \
    "    app.kubernetes.io/name: agentgateway-proxy"
  VLLM_EGRESS_HOST="${NETWORK_POLICY_EGRESS_TARGET_HOST:-1.1.1.1}"
  VLLM_EGRESS_PORT="${NETWORK_POLICY_EGRESS_TARGET_PORT:-443}"
  if [ -z "${VLLM_EGRESS_HOST}" ] || ! [[ "${VLLM_EGRESS_PORT}" =~ ^[1-9][0-9]*$ ]]; then
    echo "error: the NetworkPolicy egress target must have a host and positive-integer port" >&2
    exit 2
  fi
  # A serving-labeled probe must not open an unapproved TCP connection. The live-cluster default
  # is a content-free public endpoint; the kind fixture supplies a deterministic in-cluster target.
  run_probe models netpol-vllm-egress \
    "${VLLM_EGRESS_HOST}" "${VLLM_EGRESS_PORT}" denied \
    $'    app.kubernetes.io/instance: vllm\n    app.kubernetes.io/component: serving-engine\n    app.kubernetes.io/part-of: vllm-stack'
elif [ "${REQUIRE_VLLM}" = true ]; then
  echo "error: --require-vllm was set, but ${VLLM_SERVICE} is not deployed in namespace models" >&2
  exit 1
else
  echo "skip: vLLM profile is not active (pass --require-vllm when it must be present)"
fi

echo "NetworkPolicy conformance passed"
