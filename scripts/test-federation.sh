#!/usr/bin/env bash
# Offline contract checks for the disposable Matrix federation and cross-org A2A lab.
set -euo pipefail

readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-federation-check.XXXXXX")"
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

for command in base64 flux git jq kubectl mise openssl rg tr yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly LIFECYCLE="${ROOT_DIR}/scripts/federation.sh"
readonly SEED="${ROOT_DIR}/scripts/seed-federation.sh"
readonly RELOAD="${ROOT_DIR}/scripts/reload-federation-policy.sh"
readonly CLUSTER_OVERLAY="${ROOT_DIR}/clusters/federation"
readonly FEDERATION_ROOT="${ROOT_DIR}/infra/federation"
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

[ -x "${LIFECYCLE}" ] || fail 'scripts/federation.sh must exist and be executable'
[ -x "${SEED}" ] || fail 'scripts/seed-federation.sh must exist and be executable'
[ -x "${RELOAD}" ] || fail 'scripts/reload-federation-policy.sh must exist and be executable'
[ -f "${POLICY_APP}/kustomization.yaml" ] || fail 'federation policy component is missing'
[ -f "${POLICY_DOCUMENT}" ] || fail 'canonical federation policy is missing'
[ -f "${POLICY_MODULE}" ] || fail 'federation policy module is missing'
[ -f "${CLUSTER_OVERLAY}/kustomization.yaml" ] || fail 'clusters/federation is missing'
[ -f "${FEDERATION_ROOT}/cluster/kustomization.yaml" ] || fail 'federation Flux wiring is missing'
[ -f "${MATRIX_A_COMPONENT}" ] || fail 'homeserver A component is missing'
[ -f "${MATRIX_B_LAYER}/kustomization.yaml" ] || fail 'homeserver B layer is missing'
[ -f "${MATRIX_C_LAYER}/kustomization.yaml" ] || fail 'homeserver C layer is missing'
[ -f "${NAMESPACE_COMPONENT}/kustomization.yaml" ] || fail 'federation namespace component is missing'
[ -f "${POSTGRES_COMPONENT}/kustomization.yaml" ] || fail 'federation Postgres component is missing'
[ -f "${DELEGATION_COMPONENT}/kustomization.yaml" ] || fail 'delegation component is missing'
[ -f "${AGENT_CARD_TEMPLATE}" ] || fail 'unsigned AgentCard template is missing'
[ -x "${AGENT_CARD_SIGNER}" ] || fail 'AgentCard signer wrapper is missing or not executable'

bash -n "${LIFECYCLE}" "${SEED}" "${RELOAD}" "${ROOT_DIR}/scripts/demo.sh" \
	"${AGENT_CARD_SIGNER}"
"${LIFECYCLE}" --help >"${WORK_DIR}/help.txt"
for contract in \
	'fgentic-fed' \
	'org-a.fgentic.localhost' \
	'org-b.fgentic.localhost' \
	'org-c.fgentic.localhost' \
	'`down` deletes only'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/help.txt" >/dev/null ||
		fail "federation help omits ${contract}"
done
for task in 'fed:up' 'fed:down'; do
	rg --fixed-strings "[tasks.\"${task}\"]" "${ROOT_DIR}/mise.toml" >/dev/null ||
		fail "mise task ${task} is missing"
done

# A malformed teardown target must fail before consulting Docker or k3d. Fake binaries turn a
# future ordering regression into a harmless test failure rather than a real cluster deletion.
mkdir -p "${WORK_DIR}/bin"
for command in docker jq k3d kubectl; do
	printf '#!/usr/bin/env bash\necho "error: offline guard reached %s" >&2\nexit 99\n' \
		"${command}" >"${WORK_DIR}/bin/${command}"
	chmod +x "${WORK_DIR}/bin/${command}"
done
if PATH="${WORK_DIR}/bin:${PATH}" FGENTIC_FED_CLUSTER=fgentic \
	"${LIFECYCLE}" down >"${WORK_DIR}/reserved-cluster.txt" 2>&1; then
	fail 'federation teardown accepted a cluster other than fgentic-fed'
fi
rg --fixed-strings 'must be fgentic-fed' "${WORK_DIR}/reserved-cluster.txt" >/dev/null ||
	fail 'federation teardown did not reject the unsafe cluster name before invoking a command'
if rg --fixed-strings 'offline guard reached' "${WORK_DIR}/reserved-cluster.txt" >/dev/null; then
	fail 'federation teardown consulted the runtime before validating its cluster name'
fi

kubectl kustomize "${CLUSTER_OVERLAY}" >"${WORK_DIR}/cluster.yaml"
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "platform-settings") |
    .data.server_name == "org-a.fgentic.localhost" and
    .data.federation_partner_server_name == "org-b.fgentic.localhost" and
    .data.federation_denied_server_name == "org-c.fgentic.localhost" and
    .data.federation_gateway_ip == "192.0.2.1" and
    .data.federation_a2a_max_budget_units == "4096" and
    .data.federation_a2a_quota_budget_units_per_minute == "5000" and
    .data.llm_provider == "demo" and .data.llm_model == "fgentic-demo" and
    .data.cluster_issuer == "local-ca"' \
	"${WORK_DIR}/cluster.yaml" 'federation platform domains are not explicit and distinct'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix-b") |
    .metadata.namespace == "flux-system" and
    .spec.path == "./infra/federation/matrix-b" and
    ([.spec.dependsOn[].name] | contains(["gateway", "postgres"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver B is not an ordered, independently reconcilable Flux layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix-c") |
    .metadata.namespace == "flux-system" and
    .spec.path == "./infra/federation/matrix-c" and
    ([.spec.dependsOn[].name] | contains(["gateway", "postgres"])) and
    ([.spec.postBuild.substituteFrom[].name] |
      contains(["platform-settings", "platform-settings-overrides"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver C is not an ordered, independently reconcilable Flux layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "namespaces") |
    (.spec.components | contains(["../federation/namespaces"]))' \
	"${WORK_DIR}/cluster.yaml" 'federation namespaces are not owned by the early namespace layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "postgres") |
    (.spec.components | contains(["../federation/postgres"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver B database is not composed into the Postgres layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix") |
    (.spec.components | contains(["../federation/matrix-a"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver A is not patched through the Matrix layer'

flux build kustomization cluster-overlay-validation \
	--path "${CLUSTER_OVERLAY}" \
	--kustomization-file "${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml" \
	--dry-run --in-memory-build --strict-substitute --recursive \
	--local-sources "GitRepository/flux-system/flux-system=${ROOT_DIR}" \
	>"${WORK_DIR}/recursive.yaml"
for dependency in matrix-b postgres gateway; do
	assert_yq \
		'select(.kind == "Kustomization" and .metadata.name == "keycloak") |
      .spec.dependsOn[] | select(.name == "'"${dependency}"'")' \
		"${WORK_DIR}/recursive.yaml" \
		"org-B Keycloak is not ordered after ${dependency}"
done
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "keycloak") |
    .spec.components | select(length == 1) | .[] |
    select(. == "../federation/delegation/keycloak")' \
	"${WORK_DIR}/recursive.yaml" 'org-B Keycloak component composition is not exact'
for dependency in controllers keycloak; do
	assert_yq \
		'select(.kind == "Kustomization" and .metadata.name == "agentgateway") |
      .spec.dependsOn[] | select(.name == "'"${dependency}"'")' \
		"${WORK_DIR}/recursive.yaml" \
		"delegation gateway is not ordered after ${dependency}"
done
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "agentgateway") |
    .spec.components | select(length == 1) | .[] |
    select(. == "../federation/delegation")' \
	"${WORK_DIR}/recursive.yaml" 'delegation gateway component composition is not exact'
for dependency in agentgateway-provider postgres; do
	assert_yq \
		'select(.kind == "Kustomization" and .metadata.name == "kagent") |
      .spec.dependsOn[] | select(.name == "'"${dependency}"'")' \
		"${WORK_DIR}/recursive.yaml" \
		"docs-qa is not ordered after ${dependency}"
done
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "kagent") |
    .spec.components | select(length == 1) | .[] |
    select(. == "../federation/delegation/kagent")' \
	"${WORK_DIR}/recursive.yaml" 'kagent federation component composition is not exact'
assert_yq \
	'[select(.kind == "Kustomization" and
      (.metadata.name == "bridge" or .metadata.name == "observability" or
       .metadata.name == "observability-monitors"))] | length == 0' \
	"${WORK_DIR}/recursive.yaml" 'federation profile retained an unrelated bridge or observability unit'
yq --unwrapScalar '.patches[] | select(.target.kind == "Gateway") | .patch' \
	"${GATEWAY_COMPONENT}" >"${WORK_DIR}/gateway-a-patch.yaml"
assert_yq \
	'length == 1 and .[0].path == "/spec/listeners" and (.[0].value | length) == 4 and
    .[0].value[0].name == "http" and
    .[0].value[0].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[0].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "gateway" and
    .[0].value[1].name == "https-wellknown" and
    .[0].value[1].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[1].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix" and
    .[0].value[2].name == "https-matrix" and
    .[0].value[2].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[2].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix" and
    .[0].value[3].name == "https-a2a" and
    .[0].value[3].hostname == "a2a.${server_name}" and
    .[0].value[3].protocol == "HTTPS" and
    .[0].value[3].tls.certificateRefs[0].name == "matrix-tls" and
    .[0].value[3].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[3].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "agentgateway-system"' \
	"${WORK_DIR}/gateway-a-patch.yaml" \
	'homeserver A Gateway listeners allow routes from unrelated namespaces'

assert_yq \
	'select(.kind == "Gateway" and .metadata.name == "agentgateway-proxy") |
    .metadata.namespace == "agentgateway-system" and
    (.spec.listeners | length) == 2 and
    .spec.listeners[0].name == "default" and
    .spec.listeners[0].port == 8080 and
    .spec.listeners[0].allowedRoutes.namespaces.from == "All" and
    .spec.listeners[1].name == "federation-a2a" and
    .spec.listeners[1].port == 8081 and
    .spec.listeners[1].hostname == "a2a.${server_name}" and
    .spec.listeners[1].allowedRoutes.namespaces.from == "Same"' \
	"${WORK_DIR}/recursive.yaml" 'agentgateway has no isolated cross-org listener'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "federated-docs-qa-public") |
    .metadata.namespace == "agentgateway-system" and
    (.spec.parentRefs | length) == 1 and
    .spec.parentRefs[0].name == "fgentic-gateway" and
    .spec.parentRefs[0].namespace == "gateway" and
    .spec.parentRefs[0].sectionName == "https-a2a" and
    (.spec.hostnames | length) == 1 and
    .spec.hostnames[0] == "a2a.${server_name}" and
    (.spec.rules | length) == 2 and
    (.spec.rules[0].matches | length) == 1 and
    .spec.rules[0].matches[0].method == "GET" and
    .spec.rules[0].matches[0].path.type == "Exact" and
    .spec.rules[0].matches[0].path.value ==
      "/api/a2a/kagent/docs-qa/.well-known/agent-card.json" and
    (.spec.rules[0].backendRefs | length) == 1 and
    .spec.rules[0].backendRefs[0].name == "agentgateway-proxy" and
    .spec.rules[0].backendRefs[0].port == 8081 and
    (.spec.rules[1].matches | length) == 1 and
    .spec.rules[1].matches[0].method == "POST" and
    .spec.rules[1].matches[0].path.type == "Exact" and
    .spec.rules[1].matches[0].path.value == "/api/a2a/kagent/docs-qa" and
    (.spec.rules[1].backendRefs | length) == 1 and
    .spec.rules[1].backendRefs[0].name == "agentgateway-proxy" and
    .spec.rules[1].backendRefs[0].port == 8081' \
	"${WORK_DIR}/recursive.yaml" 'public A2A route exposes more than the exact docs-qa surface'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "federated-docs-qa-card") |
    (.spec.parentRefs | length) == 1 and
    .spec.parentRefs[0].name == "agentgateway-proxy" and
    .spec.parentRefs[0].sectionName == "federation-a2a" and
    (.spec.rules | length) == 1 and
    .spec.rules[0].matches[0].method == "GET" and
    .spec.rules[0].matches[0].path.type == "Exact" and
    .spec.rules[0].backendRefs == null' \
	"${WORK_DIR}/recursive.yaml" 'anonymous AgentCard delivery is not isolated from invocation'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "federated-docs-qa") |
    (.spec.parentRefs | length) == 1 and
    .spec.parentRefs[0].name == "agentgateway-proxy" and
    .spec.parentRefs[0].sectionName == "federation-a2a" and
    (.spec.rules | length) == 1 and
    (.spec.rules[0].matches | length) == 1 and
    .spec.rules[0].matches[0].method == "POST" and
    .spec.rules[0].matches[0].path.type == "Exact" and
    .spec.rules[0].matches[0].path.value == "/api/a2a/kagent/docs-qa" and
    (.spec.rules[0].backendRefs | length) == 1 and
    .spec.rules[0].backendRefs[0].group == "agentgateway.dev" and
    .spec.rules[0].backendRefs[0].kind == "AgentgatewayBackend" and
    .spec.rules[0].backendRefs[0].name == "federated-docs-qa-a2a"' \
	"${WORK_DIR}/recursive.yaml" 'authenticated invocation route is not exact and backend-scoped'
assert_yq \
	'[select((.kind == "HTTPRoute" and
      (.metadata.name == "kagent-a2a" or .metadata.name == "kagent-tools-mcp")) or
      (.kind == "AgentgatewayBackend" and
      (.metadata.name == "kagent-a2a" or .metadata.name == "kagent-tools")))] | length == 0' \
	"${WORK_DIR}/recursive.yaml" 'federation profile retained a broad local A2A or MCP route'

assert_yq \
	'select(.kind == "AgentgatewayPolicy" and .metadata.name == "federated-docs-qa-card") |
    (.spec.targetRefs | length) == 1 and
    .spec.targetRefs[0].group == "gateway.networking.k8s.io" and
    .spec.targetRefs[0].kind == "HTTPRoute" and
    .spec.targetRefs[0].name == "federated-docs-qa-card" and
    .spec.traffic.directResponse.status == 200 and
    .spec.traffic.directResponse.body == "__FGENTIC_SIGNED_AGENT_CARD_JSON__" and
    ([.spec.traffic.directResponse.headers[] | select(
      .name == "Content-Type" and .value == "\"application/a2a+json\"")] | length) == 1' \
	"${WORK_DIR}/recursive.yaml" 'AgentCard direct response is not snapshot-signing ready'
assert_yq \
	'select(.kind == "AgentgatewayPolicy" and .metadata.name == "federated-docs-qa") |
    .spec.traffic.buffer.request.maxBytes == "64Ki" and
    .spec.traffic.jwtAuthentication.mode == "Strict" and
    .spec.traffic.jwtAuthentication.providers[0].issuer ==
      "https://id.${federation_partner_server_name}/realms/fgentic-federation" and
    (.spec.traffic.jwtAuthentication.providers[0].audiences | length) == 1 and
    .spec.traffic.jwtAuthentication.providers[0].audiences[0] == "fgentic-a2a" and
    .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.group == "" and
    .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.kind == "Service" and
    .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.name ==
      "keycloak-http" and
    .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.namespace ==
      "keycloak" and
    .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.port == 80 and
    .spec.traffic.rateLimit.conditional[0].condition ==
      "json(request.body).method == \"SendMessage\"" and
    .spec.traffic.rateLimit.conditional[0].policy.global.failureMode == "FailClosed" and
    .spec.traffic.rateLimit.conditional[0].policy.global.domain ==
      "fgentic-cross-org-a2a" and
    .spec.traffic.rateLimit.conditional[0].policy.global.backendRef.name ==
      "federation-rate-limit" and
    .spec.traffic.rateLimit.conditional[0].policy.global.descriptors[0].unit == "Requests" and
    (.spec.traffic.rateLimit.conditional[0].policy.global.descriptors[0].entries | length) == 1 and
    .spec.traffic.rateLimit.conditional[0].policy.global.descriptors[0].entries[0].name ==
      "consumer" and
    .spec.traffic.rateLimit.conditional[0].policy.global.descriptors[0].entries[0].expression ==
      "jwt.azp"' \
	"${WORK_DIR}/recursive.yaml" \
	'cross-org invocation does not fail closed on JWT, request, or quota validation'
yq --unwrapScalar '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "federated-docs-qa") |
  .spec.traffic.authorization.policy.matchExpressions[0]
' "${WORK_DIR}/recursive.yaml" >"${WORK_DIR}/delegation-authorization.cel"
for contract in \
	'jwt.azp == "org-b-a2a"' \
	'request.path == "/api/a2a/kagent/docs-qa"' \
	'"a2a-version" in request.headers' \
	'request.headers["a2a-version"] == "1.0"' \
	'"a2a-extensions" in request.headers' \
	'request.headers.split()["a2a-extensions"]' \
	'body.method == "SendMessage"' \
	'body.method == "GetTask"' \
	'budget.maxTokens == int(budget.maxTokens)' \
	'budget.maxTokens <= ${federation_a2a_max_budget_units}'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/delegation-authorization.cel" >/dev/null ||
		fail "delegation authorization omits ${contract}"
done
if rg --fixed-strings '#' "${WORK_DIR}/delegation-authorization.cel" >/dev/null; then
	fail 'delegation authorization embeds YAML comments in the CEL expression'
fi
yq --unwrapScalar '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "federated-docs-qa") |
  .spec.traffic.rateLimit.conditional[0].policy.global.descriptors[0].cost
' "${WORK_DIR}/recursive.yaml" >"${WORK_DIR}/delegation-cost.cel"
rg --fixed-strings 'maxTokens' "${WORK_DIR}/delegation-cost.cel" >/dev/null ||
	fail 'delegation quota cost does not reserve the validated maxTokens budget'
yq --unwrapScalar '
  select(.kind == "ConfigMap" and .metadata.name == "federation-rate-limit") |
  .data."config.yaml"
' "${WORK_DIR}/recursive.yaml" >"${WORK_DIR}/rate-limit-config.yaml"
assert_yq \
	'.domain == "fgentic-cross-org-a2a" and (.descriptors | length) == 1 and
    .descriptors[0].key == "consumer" and
    .descriptors[0].rate_limit.unit == "minute" and
    .descriptors[0].rate_limit.requests_per_unit ==
      "${federation_a2a_quota_budget_units_per_minute}"' \
	"${WORK_DIR}/rate-limit-config.yaml" 'global limiter is not an azp-keyed reservation quota'
assert_yq \
	'select(.kind == "Service" and .metadata.name == "federation-rate-limit") |
    (.spec.ports | length) == 1 and
    .spec.ports[0].appProtocol == "kubernetes.io/h2c" and
    .spec.ports[0].name == "grpc" and .spec.ports[0].port == 8081 and
    .spec.ports[0].targetPort == "grpc"' \
	"${WORK_DIR}/recursive.yaml" 'global limiter Service is not gRPC h2c'
assert_yq \
	'select(.kind == "Deployment" and .metadata.name == "federation-rate-limit") |
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.containers[0].image ==
      "docker.io/envoyproxy/ratelimit:3e085e5b@sha256:70453d5ca820dce0c2939d95a3eeb0f3e64ac083749bc021401c13e4233d1595" and
    .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
    .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false' \
	"${WORK_DIR}/recursive.yaml" 'global limiter workload is not pinned, isolated, and Redis-backed'
assert_yq \
	'select(.kind == "Deployment" and .metadata.name == "federation-rate-limit") |
    .spec.template.spec.containers[0].env[] |
    select(.name == "REDIS_URL" and .value == "federation-redis:6379")' \
	"${WORK_DIR}/recursive.yaml" 'global limiter does not use its isolated Redis store'
assert_yq \
	'select(.kind == "Deployment" and .metadata.name == "federation-redis") |
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.containers[0].image ==
      "docker.io/library/redis:8.8.0@sha256:2838d5524559494f6f1cd66e97e76b200d64a633a8614200620755ed395daf32" and
    .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
    .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false' \
	"${WORK_DIR}/recursive.yaml" 'reservation store is not pinned and hardened'
assert_yq \
	'select(.kind == "ReferenceGrant" and .metadata.name == "agentgateway-keycloak-jwks") |
    .metadata.namespace == "keycloak" and
    (.spec.from | length) == 1 and
    .spec.from[0].group == "agentgateway.dev" and
    .spec.from[0].kind == "AgentgatewayPolicy" and
    .spec.from[0].namespace == "agentgateway-system" and
    (.spec.to | length) == 1 and .spec.to[0].group == "" and
    .spec.to[0].kind == "Service" and .spec.to[0].name == "keycloak-http"' \
	"${WORK_DIR}/recursive.yaml" 'remote JWKS Service reference is not explicitly granted'
for policy in \
	agentgateway-allow-federation-gateway \
	agentgateway-allow-rate-limit-egress \
	federation-rate-limit \
	federation-redis \
	keycloak-allow-agentgateway-jwks; do
	assert_yq \
		'select(.kind == "NetworkPolicy") | select(.metadata.name == "'"${policy}"'")' \
		"${WORK_DIR}/recursive.yaml" \
		"delegation data-plane NetworkPolicy ${policy} is missing"
done

yq --unwrapScalar '
  select(.kind == "ConfigMap" and .metadata.name == "keycloak-realm") |
  .data."fgentic-federation-realm.json"
' "${WORK_DIR}/recursive.yaml" >"${WORK_DIR}/delegation-realm.json"
jq -e '
  .realm == "fgentic-federation" and .accessTokenLifespan == 300 and
  ([.clients[].clientId] | sort) ==
    (["org-b-a2a", "untrusted-a2a", "wrong-audience-a2a"] | sort) and
  all(.clients[]; .serviceAccountsEnabled == true and
    .standardFlowEnabled == false and .directAccessGrantsEnabled == false) and
  ([.clients[] | select(.clientId == "org-b-a2a") |
    .protocolMappers[].config."included.client.audience"] == ["fgentic-a2a"]) and
  ([.clients[] | select(.clientId == "untrusted-a2a") |
    .protocolMappers[].config."included.client.audience"] == ["fgentic-a2a"]) and
  ([.clients[] | select(.clientId == "wrong-audience-a2a") |
    .protocolMappers[].config."included.client.audience"] == ["fgentic-wrong-audience"])
' "${WORK_DIR}/delegation-realm.json" >/dev/null ||
	fail 'org-B client-credentials realm does not distinguish issuer, azp, and audience controls'
assert_yq \
	'[select(.kind == "Agent")] | length == 1' \
	"${WORK_DIR}/recursive.yaml" 'federation profile exports more than one agent'
assert_yq \
	'select(.kind == "Agent") | select(.metadata.name == "docs-qa")' \
	"${WORK_DIR}/recursive.yaml" 'federation profile does not export docs-qa'
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "kagent") |
    .spec.values."kagent-tools".enabled == false and
    .spec.values.ui.replicas == 0 and .spec.values.kmcp.enabled == false and
    .spec.values.otel.tracing.enabled == false and .spec.values.proxy == null' \
	"${WORK_DIR}/recursive.yaml" \
	'provider-free docs-qa retained UI, tools, tracing, or the MCP proxy'
assert_yq \
	'select(.kind == "Deployment" and .metadata.name == "demo-llm") |
    select(.metadata.namespace == "models") |
    .spec.template.spec.containers[0].image | select(contains("@sha256:"))' \
	"${WORK_DIR}/recursive.yaml" 'federation profile does not use the pinned provider-free model'

# The A homeserver is patched only in this disposable overlay. Its outbound trust and domain
# restrictions must mirror B, otherwise one direction of the lab can silently become open.
yq --unwrapScalar '
  .patches[] | select(.target.kind == "HelmRelease" and .target.name == "matrix-stack") | .patch
' "${MATRIX_A_COMPONENT}" >"${WORK_DIR}/homeserver-a-patch.yaml"
yq --unwrapScalar '
  .[] | select(.path == "/spec/values/synapse/additional") |
  .value."10-federation".config
' "${WORK_DIR}/homeserver-a-patch.yaml" >"${WORK_DIR}/homeserver-a-config.yaml"
yq --unwrapScalar '
  .[] | select(.path == "/spec/values/synapse/additional") |
  .value."20-federation-policy".config
' "${WORK_DIR}/homeserver-a-patch.yaml" >"${WORK_DIR}/homeserver-a-policy-config.yaml"
for contract in \
	'default_room_version: "12"' \
	'federation_domain_whitelist:' \
	'${server_name}' \
	'${federation_partner_server_name}' \
	'federation_custom_ca_list:' \
	'/etc/fgentic-ca/ca.crt' \
	'ip_range_whitelist:' \
	'${federation_gateway_ip}' \
	'trusted_key_servers:' \
	'server_name: ${federation_partner_server_name}' \
	'/32'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-a-config.yaml" >/dev/null ||
		fail "homeserver A federation config omits ${contract}"
done
assert_yq \
	'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 2 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${federation_partner_server_name}"' \
	"${WORK_DIR}/homeserver-a-config.yaml" \
	'homeserver A does not have an exact A/B domain allowlist, room v12 default, and B key notary'
if rg --fixed-strings '${federation_denied_server_name}' \
	"${WORK_DIR}/homeserver-a-config.yaml" >/dev/null; then
	fail 'homeserver A federation allowlist includes denied homeserver C'
fi
for contract in \
	'fgentic_federation_policy.FederationPolicyModule' \
	'policy_path: /etc/fgentic/federation-policy/policy.json'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-a-policy-config.yaml" >/dev/null ||
		fail "homeserver A policy module config omits ${contract}"
done

for contract in \
	'../../../apps/synapse-federation-policy' \
	'name: fgentic-local-ca' \
	'name: fgentic-synapse-federation-policy-v1' \
	'name: fgentic-federation-policy' \
	'mountPath: /etc/fgentic-ca' \
	'mountPath: /opt/fgentic/synapse-modules' \
	'mountPath: /etc/fgentic/federation-policy' \
	'name: PYTHONPATH' \
	'value: /opt/fgentic/synapse-modules' \
	'readOnly: true' \
	'${federation_denied_server_name}' \
	'matrix.${federation_denied_server_name}' \
	'path: /spec/values/synapse/hostAliases'; do
	rg --fixed-strings "${contract}" "${MATRIX_A_COMPONENT}" >/dev/null ||
		fail "homeserver A runtime trust wiring omits ${contract}"
done

kubectl kustomize "${MATRIX_B_LAYER}" >"${WORK_DIR}/matrix-b.yaml"
kubectl kustomize "${MATRIX_C_LAYER}" >"${WORK_DIR}/matrix-c.yaml"
kubectl kustomize "${NAMESPACE_COMPONENT}" >"${WORK_DIR}/namespaces.yaml"
kubectl kustomize "${POSTGRES_COMPONENT}" >"${WORK_DIR}/postgres.yaml"

assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "fgentic-synapse-federation-policy-v1") |
    .metadata.namespace == "matrix-b" and .immutable == true and
    (.data."fgentic_federation_policy.py" | contains("class FederationPolicyModule"))' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not receive the immutable policy module source'
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "fgentic-federation-policy") |
    .metadata.namespace == "matrix-b" and .immutable == null and
    (.data."policy.json" | from_json | .version == 1)' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not receive the reloadable versioned policy'

jq -e --arg a 'org-a.fgentic.localhost' --arg b 'org-b.fgentic.localhost' \
	--arg blocked 'com.fgentic.blocked' '
  keys == ["allowed_event_types", "allowed_servers", "invite_rule", "version"] and
  .version == 1 and .allowed_servers == [$a, $b] and
  .invite_rule == "allow_from_allowed_servers" and
  (.allowed_event_types | index($blocked)) == null and
  (["m.room.create", "m.room.join_rules", "m.room.member", "m.room.message",
    "m.room.power_levels", "m.room.server_acl"] - .allowed_event_types | length) == 0
' "${POLICY_DOCUMENT}" >/dev/null || fail 'canonical federation policy is not exact and deny-by-default'
for contract in \
	'should_drop_federated_event' \
	'federated_user_may_invite' \
	'event_type_not_allowed' \
	'run_db_interaction' \
	'federation_inbound_events_staging' \
	'WHERE room_id = ? AND event_id = ?' \
	'fgentic_federation_policy_staged_event_grandfathered' \
	'fgentic_federation_policy_violation' \
	'allowed_event_type_count' \
	'allowed_server_count' \
	'policy_digest'; do
	rg --fixed-strings "${contract}" "${POLICY_MODULE}" >/dev/null ||
		fail "federation policy module omits ${contract}"
done

assert_yq \
	'select(.kind == "Namespace" and .metadata.name == "matrix-b") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
	"${WORK_DIR}/namespaces.yaml" 'matrix-b is not owned by the namespace layer with PSS labels'
assert_yq \
	'select(.kind == "Namespace" and .metadata.name == "matrix-c") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
	"${WORK_DIR}/namespaces.yaml" 'matrix-c is not owned by the namespace layer with PSS labels'

assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
    .metadata.namespace == "matrix-b" and
    .spec.releaseName == "ess" and
    .spec.values.serverName == "${federation_partner_server_name}" and
    .spec.values.synapse.postgres.host == "platform-pg-rw.postgres.svc.cluster.local" and
    .spec.values.synapse.postgres.user == "synapse_b" and
    .spec.values.synapse.postgres.database == "synapse_b" and
    .spec.values.synapse.postgres.sslMode == "require" and
    .spec.values.synapse.postgres.password.secret == "pg-synapse-b" and
    .spec.values.matrixAuthenticationService.enabled == false and
    .spec.values.elementWeb.enabled == false and
    .spec.values.elementAdmin.enabled == false and
    .spec.values.matrixRTC.enabled == false and
    .spec.values.wellKnownDelegation.enabled == true and
    .spec.values.postgres.enabled == false and
    ([.spec.values.synapse.hostAliases[] |
      select(.ip == "${federation_gateway_ip}") | .hostnames[]] |
      contains(["${server_name}", "matrix.${server_name}",
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}",
        "${federation_denied_server_name}", "matrix.${federation_denied_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B is not a minimal, locally trusted Synapse-only ESS release'

yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
  .spec.values.synapse.additional."10-federation".config
' "${WORK_DIR}/matrix-b.yaml" >"${WORK_DIR}/homeserver-b-config.yaml"
yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
  .spec.values.synapse.additional."20-federation-policy".config
' "${WORK_DIR}/matrix-b.yaml" >"${WORK_DIR}/homeserver-b-policy-config.yaml"
for contract in \
	'default_room_version: "12"' \
	'federation_domain_whitelist:' \
	'${server_name}' \
	'${federation_partner_server_name}' \
	'federation_custom_ca_list:' \
	'/etc/fgentic-ca/ca.crt' \
	'ip_range_whitelist:' \
	'${federation_gateway_ip}' \
	'trusted_key_servers:' \
	'server_name: ${server_name}' \
	'/32'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-b-config.yaml" >/dev/null ||
		fail "homeserver B federation config omits ${contract}"
done
assert_yq \
	'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 2 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${server_name}"' \
	"${WORK_DIR}/homeserver-b-config.yaml" \
	'homeserver B does not have an exact A/B domain allowlist, room v12 default, and A key notary'
if rg --fixed-strings '${federation_denied_server_name}' \
	"${WORK_DIR}/homeserver-b-config.yaml" >/dev/null; then
	fail 'homeserver B federation allowlist includes denied homeserver C'
fi
for contract in \
	'fgentic_federation_policy.FederationPolicyModule' \
	'policy_path: /etc/fgentic/federation-policy/policy.json'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-b-policy-config.yaml" >/dev/null ||
		fail "homeserver B policy module config omits ${contract}"
done
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
    ([.spec.values.synapse.extraVolumes[] | select(
      .configMap.name == "fgentic-synapse-federation-policy-v1" or
      .configMap.name == "fgentic-federation-policy")] | length) == 2 and
    ([.spec.values.synapse.extraVolumeMounts[] | select(
      .mountPath == "/opt/fgentic/synapse-modules" or
      .mountPath == "/etc/fgentic/federation-policy")] | length) == 2 and
    ([.spec.values.synapse.extraEnv[] | select(
      .name == "PYTHONPATH" and .value == "/opt/fgentic/synapse-modules")] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B policy source, data, or Python path is not mounted'

assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-c") |
    .metadata.namespace == "matrix-c" and
    .spec.releaseName == "ess" and
    .spec.values.serverName == "${federation_denied_server_name}" and
    .spec.values.synapse.postgres.host == "platform-pg-rw.postgres.svc.cluster.local" and
    .spec.values.synapse.postgres.user == "synapse_c" and
    .spec.values.synapse.postgres.database == "synapse_c" and
    .spec.values.synapse.postgres.sslMode == "require" and
    .spec.values.synapse.postgres.password.secret == "pg-synapse-c" and
    .spec.values.matrixAuthenticationService.enabled == false and
    .spec.values.elementWeb.enabled == false and
    .spec.values.elementAdmin.enabled == false and
    .spec.values.matrixRTC.enabled == false and
    .spec.values.wellKnownDelegation.enabled == true and
    .spec.values.postgres.enabled == false and
    ([.spec.values.synapse.hostAliases[] |
      select(.ip == "${federation_gateway_ip}") | .hostnames[]] |
      contains(["${server_name}", "matrix.${server_name}",
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}",
        "${federation_denied_server_name}", "matrix.${federation_denied_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
	"${WORK_DIR}/matrix-c.yaml" \
	'homeserver C is not a minimal, locally trusted Synapse-only ESS release'
yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-c") |
  .spec.values.synapse.additional."10-federation".config
' "${WORK_DIR}/matrix-c.yaml" >"${WORK_DIR}/homeserver-c-config.yaml"
assert_yq \
	'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 3 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    .federation_domain_whitelist[2] == "${federation_denied_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${server_name}"' \
	"${WORK_DIR}/homeserver-c-config.yaml" \
	'homeserver C does not have room v12, controlled local peers, and A key notary'
if rg --fixed-strings -e '10.0.0.0/8' -e '172.16.0.0/12' -e '192.168.0.0/16' \
	"${WORK_DIR}/homeserver-a-config.yaml" "${WORK_DIR}/homeserver-b-config.yaml" \
	"${WORK_DIR}/homeserver-c-config.yaml" >/dev/null; then
	fail 'federation config trusts a broad private network instead of the ingress /32'
fi

for homeserver in \
	"${WORK_DIR}/homeserver-a-config.yaml" \
	"${WORK_DIR}/homeserver-b-config.yaml" \
	"${WORK_DIR}/homeserver-c-config.yaml"; do
	if rg --regexp "^[[:space:]]*-[[:space:]]*['\"]?\\*" "${homeserver}" >/dev/null; then
		fail "${homeserver##*/} contains a wildcard federation allowlist entry"
	fi
done

assert_yq \
	'select(.kind == "Database" and .spec.name == "synapse_b") |
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.owner == "synapse_b" and
    .spec.encoding == "UTF8" and
    .spec.localeCollate == "C" and
    .spec.localeCType == "C" and
    .spec.template == "template0"' \
	"${WORK_DIR}/postgres.yaml" 'homeserver B does not have a C-locale, role-scoped CNPG database'
assert_yq \
	'select(.kind == "Database" and .spec.name == "synapse_c") |
    .metadata.name == "synapse-c" and
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.owner == "synapse_c" and
    .spec.encoding == "UTF8" and
    .spec.localeCollate == "C" and
    .spec.localeCType == "C" and
    .spec.template == "template0"' \
	"${WORK_DIR}/postgres.yaml" 'homeserver C does not have a C-locale, role-scoped CNPG database'

yq --unwrapScalar '.patches[]?.patch' \
	"${POSTGRES_COMPONENT}/kustomization.yaml" >"${WORK_DIR}/postgres-patches.yaml"
for contract in \
	'name: synapse_b' \
	'name: pg-synapse-b' \
	'name: synapse_c' \
	'name: pg-synapse-c' \
	'hostssl synapse_b synapse_b all scram-sha-256' \
	'hostssl synapse_c synapse_c all scram-sha-256' \
	'hostssl all all all reject' \
	'hostnossl all all all reject'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/postgres-patches.yaml" >/dev/null ||
		fail "homeserver B database boundary omits ${contract}"
done

assert_yq \
	'select(.kind == "Certificate" and .metadata.name == "matrix-b-tls") |
    .metadata.namespace == "gateway" and
    .spec.secretName == "matrix-b-tls" and
    .spec.issuerRef.kind == "ClusterIssuer" and
    .spec.issuerRef.name == "${cluster_issuer}" and
    (.spec.dnsNames | contains(["${federation_partner_server_name}",
      "matrix.${federation_partner_server_name}",
      "id.${federation_partner_server_name}"]))' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not have a local-CA leaf certificate'
assert_yq \
	'select(.kind == "Gateway" and .metadata.name == "federation-b") |
    .metadata.namespace == "gateway" and
    .spec.gatewayClassName == "traefik" and
    ([.spec.listeners[] | select(.protocol == "HTTPS") | .hostname] |
      contains(["${federation_partner_server_name}",
        "matrix.${federation_partner_server_name}",
        "id.${federation_partner_server_name}"])) and
    ([.spec.listeners[].tls.certificateRefs[] | select(.name == "matrix-b-tls")] |
      length) == 3 and
    ([.spec.listeners[] | select(
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix-b"
    )] | length) == 2 and
    ([.spec.listeners[] | select(
      .name == "https-id-b" and
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "keycloak"
    )] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" \
	'homeserver B has no isolated local-CA Gateway listeners for Matrix and Keycloak'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "synapse-b") |
    .metadata.namespace == "matrix-b" and
    .spec.parentRefs[0].name == "federation-b" and
    (.spec.hostnames | contains(["matrix.${federation_partner_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-synapse" and .port == 8008)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B Synapse route is incomplete'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "well-known-b") |
    .metadata.namespace == "matrix-b" and
    .spec.parentRefs[0].name == "federation-b" and
    (.spec.hostnames | contains(["${federation_partner_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-well-known" and .port == 8010)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B well-known delegation route is incomplete'

assert_yq \
	'select(.kind == "Certificate" and .metadata.name == "matrix-c-tls") |
    .metadata.namespace == "gateway" and
    .spec.secretName == "matrix-c-tls" and
    .spec.issuerRef.kind == "ClusterIssuer" and
    .spec.issuerRef.name == "${cluster_issuer}" and
    (.spec.dnsNames | contains(["${federation_denied_server_name}",
      "matrix.${federation_denied_server_name}"]))' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C does not have a local-CA leaf certificate'
assert_yq \
	'select(.kind == "Gateway" and .metadata.name == "federation-c") |
    .metadata.namespace == "gateway" and
    .spec.gatewayClassName == "traefik" and
    ([.spec.listeners[] | select(.protocol == "HTTPS") | .hostname] |
      contains(["${federation_denied_server_name}",
        "matrix.${federation_denied_server_name}"])) and
    ([.spec.listeners[].tls.certificateRefs[] | select(.name == "matrix-c-tls")] |
      length) == 2 and
    ([.spec.listeners[] | select(
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix-c"
    )] | length) == 2' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C has no local-CA TLS Gateway for apex and Synapse'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "synapse-c") |
    .metadata.namespace == "matrix-c" and
    .spec.parentRefs[0].name == "federation-c" and
    (.spec.hostnames | contains(["matrix.${federation_denied_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-synapse" and .port == 8008)] | length) == 1' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C Synapse route is incomplete'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "well-known-c") |
    .metadata.namespace == "matrix-c" and
    .spec.parentRefs[0].name == "federation-c" and
    (.spec.hostnames | contains(["${federation_denied_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-well-known" and .port == 8010)] | length) == 1' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C well-known delegation route is incomplete'

# Exercise the same offline signer that the lifecycle uses. The fixture is rendered to its final
# public domains before signing, then verified and tampered without ever writing a key in the repo.
cp "${AGENT_CARD_TEMPLATE}" "${WORK_DIR}/unsigned-agent-card.json"
CARD_SERVER=org-a.fgentic.localhost CARD_PARTNER=org-b.fgentic.localhost yq --inplace '
  (... | select(tag == "!!str")) |=
    sub("\\$\\{server_name\\}"; strenv(CARD_SERVER)) |
  (... | select(tag == "!!str")) |=
    sub("\\$\\{federation_partner_server_name\\}"; strenv(CARD_PARTNER))
' "${WORK_DIR}/unsigned-agent-card.json"
if rg --regexp '\$\{[^}]+\}' "${WORK_DIR}/unsigned-agent-card.json" >/dev/null; then
	fail 'AgentCard signing fixture retained a post-sign substitution'
fi
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
	-out "${WORK_DIR}/agent-card-private.pem" 2>/dev/null
chmod 600 "${WORK_DIR}/agent-card-private.pem"
"${AGENT_CARD_SIGNER}" sign --input "${WORK_DIR}/unsigned-agent-card.json" \
	--private-key "${WORK_DIR}/agent-card-private.pem" \
	--key-id fgentic-org-a-docs-qa-v1 --output "${WORK_DIR}/agent-card-bundle.json"
jq --join-output --compact-output '.agentCard' "${WORK_DIR}/agent-card-bundle.json" \
	>"${WORK_DIR}/signed-agent-card.json"
jq --join-output --compact-output '.publicJwk' "${WORK_DIR}/agent-card-bundle.json" \
	>"${WORK_DIR}/agent-card-public-jwk.json"
"${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/signed-agent-card.json" \
	--public-key "${WORK_DIR}/agent-card-public-jwk.json" \
	--key-id fgentic-org-a-docs-qa-v1
jq -e '
  (.agentCard.signatures | length) == 1 and
  (.agentCard.signatures[0].header == null) and
  .publicJwk.kty == "EC" and .publicJwk.crv == "P-256" and
  .publicJwk.alg == "ES256" and .publicJwk.use == "sig" and
  .publicJwk.key_ops == ["verify"] and (.publicJwk | has("d") | not)
' "${WORK_DIR}/agent-card-bundle.json" >/dev/null ||
	fail 'AgentCard signer did not emit the exact public ES256 contract'
protected="$(jq -er '.agentCard.signatures[0].protected' \
	"${WORK_DIR}/agent-card-bundle.json" | tr '_-' '/+')"
case "$((${#protected} % 4))" in
0) ;;
2) protected="${protected}==" ;;
3) protected="${protected}=" ;;
*) fail 'AgentCard protected header has invalid base64url length' ;;
esac
printf '%s' "${protected}" | base64 --decode >"${WORK_DIR}/protected-header.json"
jq -e '
  keys == ["alg", "kid", "typ"] and
  .alg == "ES256" and .kid == "fgentic-org-a-docs-qa-v1" and .typ == "JOSE"
' "${WORK_DIR}/protected-header.json" >/dev/null ||
	fail 'AgentCard JWS identity fields are not all protected'
jq '.description = "signature-tamper-must-not-be-logged"' \
	"${WORK_DIR}/signed-agent-card.json" >"${WORK_DIR}/tampered-agent-card.json"
if "${AGENT_CARD_SIGNER}" verify --input "${WORK_DIR}/tampered-agent-card.json" \
	--public-key "${WORK_DIR}/agent-card-public-jwk.json" \
	--key-id fgentic-org-a-docs-qa-v1 \
	>"${WORK_DIR}/tamper-output.txt" 2>&1; then
	fail 'AgentCard verifier accepted a tampered signed payload'
fi
if rg --fixed-strings 'signature-tamper-must-not-be-logged' \
	"${WORK_DIR}/tamper-output.txt" >/dev/null; then
	fail 'AgentCard verifier logged tampered card content'
fi
for private_suffix in pem key; do
	git -C "${ROOT_DIR}" check-ignore --quiet --no-index \
		"${ROOT_DIR}/do-not-create-agent-card-test.${private_suffix}" ||
		fail "*.${private_suffix} private keys are not git-ignored"
done
if jq -e 'has("signatures")' "${AGENT_CARD_TEMPLATE}" >/dev/null; then
	fail 'tracked AgentCard template is already signed'
fi

# Public CA material is copied into every Matrix namespace at runtime; the repository and cluster
# snapshots must never carry the local signing key.
for contract in \
	'for namespace in matrix matrix-b matrix-c' \
	'create configmap fgentic-local-ca' \
	'ca.crt' \
	'pg-synapse-b' \
		'pg-synapse-c' \
		'pg-keycloak' \
		'pg-kagent' \
		'charlie-password' \
		'org-b-a2a-client-secret' \
		'untrusted-a2a-client-secret' \
		'wrong-audience-a2a-client-secret' \
		'prepare_federation_agent_card_key' \
		'refusing to rotate a missing AgentCard key while public artifacts still exist' \
		'existing federation AgentCard public JWK is invalid' \
		'refusing to replace the independently pinnable AgentCard public JWK' \
		'--patch-file /dev/stdin' \
		'sign_federation_agent_card_snapshot' \
		'federation AgentCard contains an unresolved substitution before signing' \
		'publish_federation_agent_card_artifacts' \
		'agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}' \
		'agent-card.json=${AGENT_CARD_PUBLIC_FILE}' \
		'public-jwk.json=${AGENT_CARD_JWK_FILE}' \
		'apply_secret postgres pg-synapse-c' \
	'--from-literal=username=synapse_c' \
	'apply_secret matrix-c pg-synapse-c'; do
	rg --fixed-strings -- "${contract}" "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
		fail "federation lifecycle omits ${contract}"
	done
if rg --regexp 'apply_secret[[:space:]]+agentgateway-system.*agent-card-private' \
	"${ROOT_DIR}/scripts/demo.sh" >/dev/null; then
	fail 'AgentCard private key is published into the runtime gateway namespace'
fi
if rg --regexp='--arg.*encoded_private_key' "${ROOT_DIR}/scripts/demo.sh" >/dev/null; then
	fail 'AgentCard private key is exposed through a process argument'
fi
if rg --fixed-strings 'ca.key' "${LIFECYCLE}" "${ROOT_DIR}/scripts/demo.sh" \
	"${CLUSTER_OVERLAY}" "${FEDERATION_ROOT}" >/dev/null; then
	fail 'federation assets reference the private local-CA key'
fi

# The reload drill must make its allow revision only in the disposable Git snapshot, reconcile it
# through the normal source, prove both Synapse pods survive, and restore the tracked deny policy.
for contract in \
	'FEDERATION_POLICY_PATH="apps/synapse-federation-policy/policy/policy.json"' \
	'local policy_file="${SNAPSHOT_DIR}/${FEDERATION_POLICY_PATH}"' \
	'.allowed_event_types |= (. + [$event_type] | unique)' \
	'mv "${next_policy}" "${policy_file}"' \
	'canonical federation policy must deny' \
	'init --quiet --object-format=sha1 --initial-branch main' \
	'expected_revision="main@sha1:${SOURCE_REVISION}"' \
	'[ "${actual_revision}" = "${expected_revision}" ]' \
	'Flux fetched exact ephemeral revision'; do
	rg --fixed-strings "${contract}" "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
		fail "ephemeral federation policy snapshot omits ${contract}"
done
for contract in \
	'FGENTIC_FED_POLICY_PROBE=deny "${ROOT_DIR}/scripts/federation.sh" up' \
	'FGENTIC_FED_POLICY_PROBE=allow "${ROOT_DIR}/scripts/federation.sh" up' \
	'SYNAPSE_A_UID="$(synapse_pod_uid matrix)"' \
	'SYNAPSE_B_UID="$(synapse_pod_uid matrix-b)"' \
	'assert_synapse_uids allow' \
	'assert_synapse_uids deny' \
	'Policy reload drill failed; deleting the disposable federation lab.' \
	'canonical deny remains running'; do
	rg --fixed-strings "${contract}" "${RELOAD}" >/dev/null ||
		fail "federation policy reload drill omits ${contract}"
done
rg --fixed-strings '[tasks."fed:policy-reload"]' "${ROOT_DIR}/mise.toml" >/dev/null ||
	fail 'mise task fed:policy-reload is missing'
if rg --regexp 'kubectl.*(patch|replace).*fgentic-federation-policy' \
	"${ROOT_DIR}/scripts/demo.sh" "${RELOAD}" >/dev/null; then
	fail 'policy reload bypasses Git and mutates the live ConfigMap directly'
fi

# The up path proves both boundaries: the hardened v12 room still exchanges A/B messages and
# rejects C, while a final custom event in a throwaway room is retained by B but dropped by A with
# a content-free, event-addressable policy record. The explicit allow mode is used only by the
# ephemeral-Git reload drill; deny remains the canonical default.
for contract in \
		'FGENTIC_DEMO_PROFILE=federation' \
		'A2A_URL="https://a2a.${SERVER_A}"' \
		'IDP_B_URL="https://id.${SERVER_B}"' \
		'verify_public_agent_card' \
		'public AgentCard bytes differ from the signed bootstrap artifact' \
		'.securitySchemes.orgBOIDC.openIdConnectSecurityScheme.openIdConnectUrl' \
		'org-B OIDC discovery contract is inconsistent' \
		'verify_kagent_not_public' \
		'(.spec.type // "ClusterIP") == "ClusterIP"' \
		'a Gateway API route exposes kagent directly' \
		'reset_delegation_quota_fixture' \
		'redis-cli FLUSHDB' \
		'production quotas must' \
		'client_credentials_token org-b-a2a' \
		'client_credentials_token untrusted-a2a' \
		'client_credentials_token wrong-audience-a2a' \
		'expect_a2a_status missing-token 401' \
		'expect_a2a_status malformed-token 401' \
		'expect_a2a_status wrong-audience 401' \
		'expect_a2a_status untrusted-consumer 403' \
		'A2A-Version: 1.0' \
		'A2A-Extensions: ${TOKEN_BUDGET_EXTENSION}' \
		'missing A2A extension activation returned HTTP' \
		'expect_a2a_status unsupported-method 403' \
		'invalid-budget-' \
		'unpublished kagent path returned HTTP' \
		'agentgateway_token_total' \
		'authorized org B delegation returned HTTP' \
		'authorized delegation did not increase aggregate model-token metrics' \
		'expect_a2a_status exhausted-reservation-quota 429' \
		'The limiter reserves the caller-declared maximum' \
		'room_version' \
	'"12"' \
	'creation_content: {"m.federate": true}' \
	'creation_content: {"m.federate": false}' \
	'm.room.server_acl' \
	'allow_ip_literals: false' \
	'.allow_ip_literals == false and .deny == []' \
	'(.allow | sort) == ([$a, $b] | sort)' \
	'/state/m.room.server_acl' \
	'.room_version == "12" and ."m.federate" == true' \
	'.room_version == "12" and ."m.federate" == false' \
	'/_matrix/client/v3/rooms/' \
	'/invite' \
	'/join' \
	'/send/m.room.message/' \
	'/sync?timeout=1000' \
	'@alice:${SERVER_A}' \
	'@bob:${SERVER_B}' \
	'@charlie:${SERVER_C}' \
	'MATRIX_C_URL' \
	'register_user matrix-c' \
	'create_federated_room' \
	'denied control join' \
	'send_signed_federation_probe' \
	'SYNAPSE_SIGNING_KEY' \
	'from signedjson.key import read_signing_keys' \
	'from signedjson.sign import sign_json' \
	'edu_type: "m.typing"' \
	'signed federation positive control' \
	'denied control federation send to' \
	'whitelist | sort) == ([$a, $b, $c] | sort)' \
	'SYNAPSE_REGISTRATION_SHARED_SECRET' \
	'whitelist_enabled == true' \
	'federation-a-to-b-' \
	'federation-b-to-a-' \
	'POLICY_EVENT_TYPE="com.fgentic.blocked"' \
	'POLICY_PROBE_MODE="${FGENTIC_FED_POLICY_PROBE:-deny}"' \
	'FGENTIC_FED_POLICY_PROBE must be allow or deny' \
	'wait_for_mounted_policy_mode matrix' \
	'wait_for_mounted_policy_mode matrix-b' \
	'/etc/fgentic/federation-policy/policy.json' \
	'(.allowed_event_types | index($type)) != null' \
	'(.allowed_event_types | index($type)) == null' \
	'Fgentic Federation Policy Probe' \
	'policy-content-must-not-be-logged-' \
	'verify_local_policy_event' \
	'wait_for_policy_violation' \
	'select(.event == $event_id)' \
	'fgentic_federation_policy_violation ' \
	'event_type_not_allowed' \
	'allowed_event_type_count' \
	'policy_digest' \
	'federation policy logs exposed denied event content' \
	'verify_remote_policy_event_absent' \
	'.errcode == "M_NOT_FOUND"' \
	'wait_for_remote_policy_event' \
	'allowed on A after Flux policy reconcile'; do
	rg --fixed-strings "${contract}" "${LIFECYCLE}" "${SEED}" >/dev/null ||
		fail "federation acceptance proof omits ${contract}"
done
if rg --fixed-strings '403 | 404' "${SEED}" >/dev/null; then
	fail 'federation acceptance treats a local missing-room response as a denied federation send'
fi
if rg --fixed-strings -- '--data-urlencode "client_secret=' "${SEED}" >/dev/null; then
	fail 'federation acceptance exposes a confidential-client secret in process arguments'
fi
if rg --fixed-strings '%3N' "${SEED}" >/dev/null; then
	fail 'federation acceptance depends on GNU date nanosecond formatting'
fi
rg --fixed-strings 'LLM_PROVIDER="demo"' "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
	fail 'federation profile can select a paid model provider'

echo 'Federation topology and lifecycle contracts passed.'
