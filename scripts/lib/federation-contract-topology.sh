#!/usr/bin/env bash
# Definition-only federation topology contracts sourced by scripts/test-federation.sh.
check_federation_topology() {
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
	[ -x "${USAGE_RECEIPT_TOOL}" ] || fail 'usage-receipt wrapper is missing or not executable'

	bash -n "${LIFECYCLE}" "${SEED_SOURCES[@]}" "${RELOAD}" "${DEMO_SOURCES[@]}" \
		"${AGENT_CARD_SIGNER}" "${USAGE_RECEIPT_TOOL}"
	"${LIFECYCLE}" --help >"${WORK_DIR}/help.txt"
	for contract in \
		'fgentic-fed' \
		'org-a.fgentic.localhost' \
		'org-b.fgentic.localhost' \
		'org-c.fgentic.localhost' \
		'`down` deletes only'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/help.txt" >/dev/null \
			|| fail "federation help omits ${contract}"
	done
	for task in 'fed:up' 'fed:down'; do
		rg --fixed-strings "[tasks.\"${task}\"]" "${ROOT_DIR}/mise.toml" >/dev/null \
			|| fail "mise task ${task} is missing"
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
	rg --fixed-strings 'must be fgentic-fed' "${WORK_DIR}/reserved-cluster.txt" >/dev/null \
		|| fail 'federation teardown did not reject the unsafe cluster name before invoking a command'
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
    .data.demo_bridge_tag == "local" and
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
    .spec.traffic.buffer.request.maxBytes == "8Ki" and
    .spec.traffic.buffer.response.maxBytes == "8Ki" and
    .spec.traffic.extProc.backendRef.name == "federation-usage-receipt" and
    .spec.traffic.extProc.backendRef.port == 4444 and
    .spec.traffic.extProc.processingOptions.requestBodyMode == "Buffered" and
    .spec.traffic.extProc.processingOptions.responseBodyMode == "Buffered" and
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
		'"content-type" in request.headers' \
		'size(request.headers.raw()["content-type"]) == 1' \
		'request.headers.raw()["content-type"][0].lowerAscii().split(";")[0].trim()' \
		'== "application/json"' \
		'"a2a-version" in request.headers' \
		'request.headers["a2a-version"] == "1.0"' \
		'"a2a-extensions" in request.headers' \
		'request.headers.split()["a2a-extensions"]' \
		'body.method == "SendMessage"' \
		'body.method == "GetTask"' \
		'budget.maxTokens == int(budget.maxTokens)' \
		'budget.maxTokens <= ${federation_a2a_max_budget_units}'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/delegation-authorization.cel" >/dev/null \
			|| fail "delegation authorization omits ${contract}"
	done

	assert_yq \
		'select(.kind == "Deployment" and .metadata.name == "federation-usage-receipt") |
    .spec.replicas == 1 and .spec.strategy.type == "Recreate" and
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.containers[0].name == "usage-receipt" and
    .spec.template.spec.containers[0].image == "matrix-a2a-bridge:${demo_bridge_tag}" and
    .spec.template.spec.containers[0].imagePullPolicy == "Never" and
    .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
    .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false' \
		"${WORK_DIR}/recursive.yaml" 'usage-receipt signer is not single-writer, local, and hardened'
	assert_yq \
		'select(.kind == "PersistentVolumeClaim" and .metadata.name == "federation-usage-receipts") |
    (.spec.accessModes | length) == 1 and .spec.accessModes[0] == "ReadWriteOnce" and
    .spec.resources.requests.storage == "64Mi"' \
		"${WORK_DIR}/recursive.yaml" 'usage-receipt archive is not persistent single-writer storage'
	assert_yq \
		'select(.kind == "Service" and .metadata.name == "federation-usage-receipt") |
    (.spec.ports | length) == 1 and .spec.ports[0].appProtocol == "kubernetes.io/h2c" and
    .spec.ports[0].name == "grpc" and .spec.ports[0].port == 4444 and
    .spec.ports[0].targetPort == "grpc"' \
		"${WORK_DIR}/recursive.yaml" 'usage-receipt Service is not gRPC h2c'
	assert_yq \
		'select(.kind == "NetworkPolicy" and .metadata.name == "federation-usage-receipt") |
    (.spec.ingress | length) == 1 and (.spec.egress | length) == 0' \
		"${WORK_DIR}/recursive.yaml" 'usage-receipt signer is not isolated from untrusted workloads'
	if rg --fixed-strings '#' "${WORK_DIR}/delegation-authorization.cel" >/dev/null; then
		fail 'delegation authorization embeds YAML comments in the CEL expression'
	fi
	yq --unwrapScalar '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "federated-docs-qa") |
  .spec.traffic.rateLimit.conditional[0].policy.global.descriptors[0].cost
' "${WORK_DIR}/recursive.yaml" >"${WORK_DIR}/delegation-cost.cel"
	rg --fixed-strings 'maxTokens' "${WORK_DIR}/delegation-cost.cel" >/dev/null \
		|| fail 'delegation quota cost does not reserve the validated maxTokens budget'
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
' "${WORK_DIR}/delegation-realm.json" >/dev/null \
		|| fail 'org-B client-credentials realm does not distinguish issuer, azp, and audience controls'
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

}
