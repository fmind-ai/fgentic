#!/usr/bin/env bash
# Definition-only cross-organization A2A proof helpers sourced by scripts/seed-federation.sh.
client_credentials_token() {
	local client_id="$1"
	local client_secret="$2"
	local output_variable="$3"
	local response token
	[[ "${client_id}" =~ ^[a-z0-9-]+$ ]] || die "invalid A2A client ID"
	[[ "${client_secret}" =~ ^[0-9a-f]+$ ]] || die "invalid A2A client secret encoding"
	response="$(
		printf 'grant_type=client_credentials&client_id=%s&client_secret=%s' \
			"${client_id}" "${client_secret}" |
			curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
				--request POST --header 'Content-Type: application/x-www-form-urlencoded' \
				--data-binary @- \
				"${IDP_B_URL}/realms/fgentic-federation/protocol/openid-connect/token"
	)"
	token="$(jq -er '.access_token | select(type == "string" and length > 0)' <<<"${response}")"
	printf -v "${output_variable}" '%s' "${token}"
	response=""
	token=""
}
a2a_document() {
	local budget_json="$1"
	local method="${2:-SendMessage}"
	local message_id="federation-a2a-${RANDOM}-$$"
	jq --null-input --compact-output \
		--arg id "${message_id}" --arg method "${method}" \
		--arg extension "${TOKEN_BUDGET_EXTENSION}" --argjson budget "${budget_json}" '{
      jsonrpc: "2.0",
      id: $id,
      method: $method,
      params: {
        message: {
          messageId: $id,
          role: "ROLE_USER",
          parts: [{text: "Explain the provider-free Fgentic evaluation path."}],
          extensions: [$extension],
          metadata: {($extension): {maxTokens: $budget}}
        }
      }
    }'
}

a2a_status() {
	local output="$1"
	local token="$2"
	local document="$3"
	local authorization=()
	if [ -n "${token}" ]; then
		authorization=(--header "Authorization: Bearer ${token}")
	fi
	request_status "${output}" --request POST --header 'Content-Type: application/json' \
		--header 'A2A-Version: 1.0' \
		--header "A2A-Extensions: ${TOKEN_BUDGET_EXTENSION}" \
		"${authorization[@]}" --data "${document}" "${A2A_URL}${A2A_AGENT_PATH}"
}

expect_a2a_status() {
	local label="$1"
	local expected="$2"
	local token="$3"
	local document="$4"
	local output="${WORK_DIR}/a2a-${label}.json"
	local status
	status="$(a2a_status "${output}" "${token}" "${document}")"
	[ "${status}" = "${expected}" ] ||
		die "${label} A2A request returned HTTP ${status}, expected ${expected}"
}

agentgateway_token_total() {
	local pod metrics
	pod="$(kubectl --namespace agentgateway-system get pods \
		--selector app.kubernetes.io/name=agentgateway-proxy \
		--output jsonpath='{.items[0].metadata.name}')"
	[ -n "${pod}" ] || die "agentgateway proxy pod was not found"
	metrics="$(kubectl get --raw \
		"/api/v1/namespaces/agentgateway-system/pods/${pod}:15020/proxy/metrics")"
	awk '$1 ~ /^agentgateway_gen_ai_client_token_usage_sum([{]|$)/ {total += $2} END {print total + 0}' \
		<<<"${metrics}"
}

verify_public_agent_card() {
	local served_card="${WORK_DIR}/served-agent-card.json"
	local expected_card="${WORK_DIR}/expected-agent-card.json"
	local public_jwk="${WORK_DIR}/public-jwk.json"
	local key_id status discovery
	status="$(request_status "${served_card}" "${A2A_URL}${A2A_AGENT_PATH}/.well-known/agent-card.json")"
	[ "${status}" = "200" ] || die "public AgentCard returned HTTP ${status}"
	kubectl --namespace agentgateway-system get configmap "${AGENT_CARD_CONFIGMAP}" \
		--output 'go-template={{index .data "agent-card.json"}}' >"${expected_card}"
	kubectl --namespace agentgateway-system get configmap "${AGENT_CARD_CONFIGMAP}" \
		--output 'go-template={{index .data "public-jwk.json"}}' >"${public_jwk}"
	cmp --silent "${served_card}" "${expected_card}" ||
		die "public AgentCard bytes differ from the signed bootstrap artifact"
	key_id="$(jq -er '.kid | select(type == "string" and length > 0)' "${public_jwk}")"
	"${ROOT_DIR}/scripts/sign-agent-card.sh" verify --input "${served_card}" \
		--public-key "${public_jwk}" --key-id "${key_id}"
	jq -e --arg url "${A2A_URL}${A2A_AGENT_PATH}" \
		--arg extension "${TOKEN_BUDGET_EXTENSION}" \
		--arg oidc "${IDP_B_URL}/realms/fgentic-federation/.well-known/openid-configuration" '
      .name == "Fgentic docs-qa" and
      .provider.organization == "Fgentic Org A" and
      any(.supportedInterfaces[]?;
        .url == $url and .protocolBinding == "JSONRPC" and .protocolVersion == "1.0") and
      any(.capabilities.extensions[]?; .uri == $extension and .required == true) and
      .securitySchemes.orgBOIDC.openIdConnectSecurityScheme.openIdConnectUrl == $oidc and
      (.securityRequirements | length) == 1 and
      .securityRequirements[0].schemes.orgBOIDC == {"list": []} and
      any(.skills[]?; .id == "fgentic-documentation") and
      (.signatures | length) >= 1
    ' "${served_card}" >/dev/null || die "public AgentCard contract is incomplete"
	discovery="$(curl --silent --show-error --fail-with-body --cacert "${CA_CERT}" \
		"${IDP_B_URL}/realms/fgentic-federation/.well-known/openid-configuration")"
	jq -e --arg issuer "${IDP_B_URL}/realms/fgentic-federation" \
		--arg jwks "${IDP_B_URL}/realms/fgentic-federation/protocol/openid-connect/certs" '
      .issuer == $issuer and .jwks_uri == $jwks
    ' <<<"${discovery}" >/dev/null || die "org-B OIDC discovery contract is inconsistent"
}

verify_kagent_not_public() {
	local services routes
	services="$(kubectl --namespace kagent get services --output json)"
	jq -e '
    (.items | length) > 0 and
    any(.items[]; .metadata.name == "kagent-controller") and
    all(.items[];
      (.spec.type // "ClusterIP") == "ClusterIP" and
      ((.spec.externalIPs // []) | length) == 0 and
      all(.spec.ports[]; has("nodePort") | not))
  ' <<<"${services}" >/dev/null ||
		die "kagent exposes a non-ClusterIP, external IP, or node port"
	routes="$(kubectl get httproutes.gateway.networking.k8s.io --all-namespaces --output json)"
	jq -e '
    all(.items[];
      all(.spec.rules[]?.backendRefs[]?;
        .name != "kagent-controller" and .name != "kagent-a2a"))
  ' <<<"${routes}" >/dev/null || die "a Gateway API route exposes kagent directly"
}

reset_delegation_quota_fixture() {
	local flushed size
	# This Redis exists only in the disposable acceptance lab. Resetting its transient counters
	# makes repeated `fed:up` and the policy-reload drill deterministic; production quotas must
	# never be reset as part of deployment or health checking.
	flushed="$(kubectl --namespace agentgateway-system exec deployment/federation-redis -- \
		redis-cli FLUSHDB)"
	[ "${flushed}" = "OK" ] || die "failed to reset the disposable delegation quota"
	size="$(kubectl --namespace agentgateway-system exec deployment/federation-redis -- \
		redis-cli DBSIZE)"
	[ "${size}" = "0" ] || die "disposable delegation quota did not reset to zero"
}

verify_cross_org_delegation() {
	local org_b_secret untrusted_secret wrong_audience_secret
	local document status response before_tokens after_tokens denied_path_status missing_extension_status
	reset_delegation_quota_fixture
	verify_public_agent_card
	verify_kagent_not_public

	org_b_secret="$(bootstrap_secret_value org-b-a2a-client-secret)"
	untrusted_secret="$(bootstrap_secret_value untrusted-a2a-client-secret)"
	wrong_audience_secret="$(bootstrap_secret_value wrong-audience-a2a-client-secret)"
	client_credentials_token org-b-a2a "${org_b_secret}" ORG_B_A2A_TOKEN
	client_credentials_token untrusted-a2a "${untrusted_secret}" UNTRUSTED_A2A_TOKEN
	client_credentials_token wrong-audience-a2a "${wrong_audience_secret}" \
		WRONG_AUDIENCE_A2A_TOKEN
	org_b_secret=""
	untrusted_secret=""
	wrong_audience_secret=""

	document="$(a2a_document 1000)"
	expect_a2a_status missing-token 401 "" "${document}"
	expect_a2a_status malformed-token 401 not-a-jwt "${document}"
	expect_a2a_status wrong-audience 401 "${WRONG_AUDIENCE_A2A_TOKEN}" "${document}"
	expect_a2a_status untrusted-consumer 403 "${UNTRUSTED_A2A_TOKEN}" "${document}"
	missing_extension_status="$(request_status "${WORK_DIR}/a2a-missing-extension.json" \
		--request POST --header 'Content-Type: application/json' \
		--header 'A2A-Version: 1.0' \
		--header "Authorization: Bearer ${ORG_B_A2A_TOKEN}" --data "${document}" \
		"${A2A_URL}${A2A_AGENT_PATH}")"
	[ "${missing_extension_status}" = "403" ] ||
		die "missing A2A extension activation returned HTTP ${missing_extension_status}, expected 403"
	for invalid in 'null' '"1000"' '1.5' '0' '-1' '4097'; do
		document="$(a2a_document "${invalid}")"
		expect_a2a_status "invalid-budget-${RANDOM}" 403 "${ORG_B_A2A_TOKEN}" "${document}"
	done
	document="$(a2a_document 1000 ListTasks)"
	expect_a2a_status unsupported-method 403 "${ORG_B_A2A_TOKEN}" "${document}"
	denied_path_status="$(request_status "${WORK_DIR}/a2a-denied-path.json" --request POST \
		--header 'Content-Type: application/json' \
		--header 'A2A-Version: 1.0' \
		--header "A2A-Extensions: ${TOKEN_BUDGET_EXTENSION}" \
		--header "Authorization: Bearer ${ORG_B_A2A_TOKEN}" \
		--data "$(a2a_document 1000)" "${A2A_URL}/api/a2a/kagent/scribe")"
	[ "${denied_path_status}" = "404" ] ||
		die "unpublished kagent path returned HTTP ${denied_path_status}, expected 404"

	before_tokens="$(agentgateway_token_total)"
	document="$(a2a_document 3000)"
	response="${WORK_DIR}/a2a-valid.json"
	status="$(a2a_status "${response}" "${ORG_B_A2A_TOKEN}" "${document}")"
	[ "${status}" = "200" ] || die "authorized org B delegation returned HTTP ${status}"
	jq -e --arg reply "${EXPECTED_DEMO_REPLY}" '
      .jsonrpc == "2.0" and .error == null and
      ([.. | objects | .text? // empty] | any(. == $reply))
    ' "${response}" >/dev/null || die "authorized org B delegation returned no model reply"
	after_tokens="$(agentgateway_token_total)"
	awk -v before="${before_tokens}" -v after="${after_tokens}" \
		'BEGIN {exit !(after > before)}' ||
		die "authorized delegation did not increase aggregate model-token metrics"

	# The limiter reserves the caller-declared maximum, not provider-reported usage. With the
	# 5,000-token window, the first 3,000-token reservation passes and the second must be denied.
	expect_a2a_status exhausted-reservation-quota 429 "${ORG_B_A2A_TOKEN}" "${document}"
}
