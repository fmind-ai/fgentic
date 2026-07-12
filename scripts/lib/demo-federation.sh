#!/usr/bin/env bash
# Definition-only federation snapshot, signing, and secret helpers sourced by scripts/demo.sh.
snapshot_source() {
	SNAPSHOT_DIR="${WORK_DIR}/snapshot"
	mkdir -p "${SNAPSHOT_DIR}"
	if [ -n "$(git -C "${ROOT_DIR}" status --porcelain)" ]; then
		echo "Note: the ephemeral demo snapshot includes the current uncommitted working tree."
	fi
	(
		cd "${ROOT_DIR}"
		git ls-files --cached --others --exclude-standard -z |
			while IFS= read -r -d '' path; do
				[ -e "${path}" ] || [ -L "${path}" ] || continue
				printf '%s\0' "${path}"
			done |
			tar --null --files-from=- --create --file=-
	) | tar --directory "${SNAPSHOT_DIR}" --extract --file=-

	LLM_PROVIDER="${LLM_PROVIDER}" LLM_MODEL="${LLM_MODEL}" GCP_PROJECT="${GCP_PROJECT}" \
		VERTEX_REGION="${VERTEX_REGION}" OPENAI_HOST="${OPENAI_HOST}" \
		AZURE_OPENAI_RESOURCE="${AZURE_OPENAI_RESOURCE}" BRIDGE_TAG="${BRIDGE_TAG}" \
		yq --inplace '
      .data.llm_provider = strenv(LLM_PROVIDER) |
      .data.llm_model = strenv(LLM_MODEL) |
      .data.gcp_project = strenv(GCP_PROJECT) |
      .data.vertex_region = strenv(VERTEX_REGION) |
      .data.openai_host = strenv(OPENAI_HOST) |
      .data.azure_openai_resource = strenv(AZURE_OPENAI_RESOURCE) |
      .data.demo_bridge_tag = strenv(BRIDGE_TAG)
    ' "${SNAPSHOT_DIR}/${OVERLAY_PATH}/platform-settings.yaml"
	if [ "${PROFILE}" = "federation" ]; then
		FED_GATEWAY_IP="${FEDERATION_GATEWAY_IP}" yq --inplace \
			'.data.federation_gateway_ip = strenv(FED_GATEWAY_IP)' \
			"${SNAPSHOT_DIR}/${OVERLAY_PATH}/platform-settings.yaml"
		configure_federation_policy_snapshot
		sign_federation_agent_card_snapshot
	fi

	# Flux reports Git artifacts as `sha1:<40 hex>` in the pinned source-controller contract.
	# Force that object format even when the caller globally defaults new repositories to SHA-256.
	git -C "${SNAPSHOT_DIR}" init --quiet --object-format=sha1 --initial-branch main
	git -C "${SNAPSHOT_DIR}" add --all
	git -C "${SNAPSHOT_DIR}" \
		-c user.name='Fgentic demo' -c user.email='demo@localhost' \
		commit --quiet --message='chore: create ephemeral demo source'
	SOURCE_REVISION="$(git -C "${SNAPSHOT_DIR}" rev-parse HEAD)"
	[[ "${SOURCE_REVISION}" =~ ^[0-9a-f]{40}$ ]] || die "invalid ephemeral Git revision"
	SOURCE_CONTEXT="${WORK_DIR}/source-image"
	mkdir -p "${SOURCE_CONTEXT}"
	git clone --quiet --bare "${SNAPSHOT_DIR}" "${SOURCE_CONTEXT}/repo.git"
	git --git-dir="${SOURCE_CONTEXT}/repo.git" update-server-info
}
prepare_federation_agent_card_key() {
	local bootstrap_json encoded_private_key public_artifacts_exist
	AGENT_CARD_PRIVATE_KEY="${WORK_DIR}/agent-card-private-key.pem"
	AGENT_CARD_KEY_ID=""
	EXISTING_AGENT_CARD_JWK_FILE=""
	chmod 700 "${WORK_DIR}"

	# The signing key is needed before the ephemeral Git snapshot exists. Keep it only in the
	# lifecycle-owned bootstrap Secret; neither the signed source nor a workload namespace ever
	# receives private material. Reusing the key keeps the public trust anchor stable across up.
	kubectl create namespace flux-system --dry-run=client --output=yaml |
		kubectl apply --filename - >/dev/null
	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output json 2>/dev/null || printf '{}')"
	public_artifacts_exist=false
	if kubectl --namespace agentgateway-system get configmap \
		"${FEDERATION_AGENT_CARD_CONFIGMAP}" >/dev/null 2>&1; then
		public_artifacts_exist=true
		EXISTING_AGENT_CARD_JWK_FILE="${WORK_DIR}/existing-public-jwk.json"
		kubectl --namespace agentgateway-system get configmap \
			"${FEDERATION_AGENT_CARD_CONFIGMAP}" \
			--output 'go-template={{index .data "public-jwk.json"}}' \
			>"${EXISTING_AGENT_CARD_JWK_FILE}"
		jq -e '
      keys == ["alg", "crv", "key_ops", "kid", "kty", "use", "x", "y"] and
      .kty == "EC" and .crv == "P-256" and .alg == "ES256" and
      .use == "sig" and .key_ops == ["verify"] and (has("d") | not)
    ' "${EXISTING_AGENT_CARD_JWK_FILE}" >/dev/null ||
			die "existing federation AgentCard public JWK is invalid"
	fi

	encoded_private_key="$(jq -r '.data["agent-card-private-key"] // ""' \
		<<<"${bootstrap_json}")"
	AGENT_CARD_KEY_ID="$(jq -r '.data["agent-card-key-id"] // "" | @base64d' \
		<<<"${bootstrap_json}")"
	if [ -n "${encoded_private_key}" ]; then
		printf '%s' "${encoded_private_key}" | base64 --decode >"${AGENT_CARD_PRIVATE_KEY}"
		[ -n "${AGENT_CARD_KEY_ID}" ] ||
			die "federation AgentCard key ID is missing from the bootstrap Secret"
	else
		[ "${public_artifacts_exist}" = false ] ||
			die "refusing to rotate a missing AgentCard key while public artifacts still exist"
		AGENT_CARD_KEY_ID="${FEDERATION_AGENT_CARD_DEFAULT_KEY_ID}"
		openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
			-out "${AGENT_CARD_PRIVATE_KEY}" 2>/dev/null
	fi
	chmod 600 "${AGENT_CARD_PRIVATE_KEY}"

	if kubectl --namespace flux-system get secret fgentic-demo-bootstrap >/dev/null 2>&1; then
		kubectl --namespace flux-system create secret generic fgentic-demo-bootstrap \
			--from-file="agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}" \
			--from-literal="agent-card-key-id=${AGENT_CARD_KEY_ID}" \
			--dry-run=client --output=json |
			jq --compact-output '{data: .data}' |
			kubectl --namespace flux-system patch secret fgentic-demo-bootstrap \
				--type=merge --patch-file /dev/stdin \
			>/dev/null
	else
		apply_secret flux-system fgentic-demo-bootstrap \
			--from-file="agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}" \
			--from-literal="agent-card-key-id=${AGENT_CARD_KEY_ID}"
	fi
	bootstrap_json=""
	encoded_private_key=""
}

sign_federation_agent_card_snapshot() {
	local template policy settings bundle marker_count signed_card card_server card_partner
	template="${SNAPSHOT_DIR}/${FEDERATION_AGENT_CARD_TEMPLATE_PATH}"
	policy="${SNAPSHOT_DIR}/${FEDERATION_AGENT_CARD_POLICY_PATH}"
	settings="${SNAPSHOT_DIR}/${OVERLAY_PATH}/platform-settings.yaml"
	bundle="${WORK_DIR}/agent-card-bundle.json"
	AGENT_CARD_PUBLIC_FILE="${WORK_DIR}/agent-card.json"
	AGENT_CARD_JWK_FILE="${WORK_DIR}/public-jwk.json"
	[ -f "${template}" ] || die "federation AgentCard template not found"
	[ -f "${policy}" ] || die "federation AgentCard policy not found"
	marker_count="$(rg --only-matching --fixed-strings "${FEDERATION_AGENT_CARD_MARKER}" \
		"${policy}" | wc -l | tr -d ' ')"
	[ "${marker_count}" = "1" ] ||
		die "federation AgentCard policy must contain exactly one signed-card marker"
	card_server="$(yq --unwrapScalar '.data.server_name' "${settings}")"
	card_partner="$(yq --unwrapScalar '.data.federation_partner_server_name' "${settings}")"
	[ -n "${card_server}" ] && [ -n "${card_partner}" ] ||
		die "federation AgentCard domains are missing from platform settings"
	CARD_SERVER="${card_server}" CARD_PARTNER="${card_partner}" yq --inplace '
      (... | select(tag == "!!str")) |=
        sub("\\$\\{server_name\\}"; strenv(CARD_SERVER)) |
      (... | select(tag == "!!str")) |=
        sub("\\$\\{federation_partner_server_name\\}"; strenv(CARD_PARTNER))
    ' "${template}"
	if rg --regexp '\$\{[^}]+\}' "${template}" >/dev/null; then
		die "federation AgentCard contains an unresolved substitution before signing"
	fi

	"${ROOT_DIR}/scripts/sign-agent-card.sh" sign \
		--input "${template}" --private-key "${AGENT_CARD_PRIVATE_KEY}" \
		--key-id "${AGENT_CARD_KEY_ID}" --output "${bundle}"
	jq --exit-status --join-output --compact-output \
		'.agentCard | select(type == "object")' "${bundle}" >"${AGENT_CARD_PUBLIC_FILE}"
	jq --exit-status --join-output --compact-output \
		'.publicJwk | select(type == "object" and has("d") == false)' \
		"${bundle}" >"${AGENT_CARD_JWK_FILE}"
	"${ROOT_DIR}/scripts/sign-agent-card.sh" verify \
		--input "${AGENT_CARD_PUBLIC_FILE}" --public-key "${AGENT_CARD_JWK_FILE}" \
		--key-id "${AGENT_CARD_KEY_ID}"
	if [ -n "${EXISTING_AGENT_CARD_JWK_FILE}" ]; then
		jq --exit-status --slurp '.[0] == .[1]' \
			"${EXISTING_AGENT_CARD_JWK_FILE}" "${AGENT_CARD_JWK_FILE}" >/dev/null ||
			die "refusing to replace the independently pinnable AgentCard public JWK"
	fi

	signed_card="$(<"${AGENT_CARD_PUBLIC_FILE}")"
	SIGNED_CARD_JSON="${signed_card}" CARD_MARKER="${FEDERATION_AGENT_CARD_MARKER}" \
		yq --inplace \
		'(... | select(tag == "!!str" and . == strenv(CARD_MARKER))) = strenv(SIGNED_CARD_JSON)' \
		"${policy}"
	if rg --fixed-strings "${FEDERATION_AGENT_CARD_MARKER}" "${policy}" >/dev/null; then
		die "signed AgentCard marker remained in the ephemeral policy"
	fi
}

publish_federation_agent_card_artifacts() {
	# These are the exact public bytes served by the snapshot policy. The ConfigMap is evidence
	# for the acceptance test and a convenient public-key distribution point, never key storage.
	kubectl --namespace agentgateway-system create configmap \
		"${FEDERATION_AGENT_CARD_CONFIGMAP}" \
		--from-file="agent-card.json=${AGENT_CARD_PUBLIC_FILE}" \
		--from-file="public-jwk.json=${AGENT_CARD_JWK_FILE}" \
		--dry-run=client --output=yaml |
		kubectl apply --filename - >/dev/null
}

configure_federation_policy_snapshot() {
	local policy_file="${SNAPSHOT_DIR}/${FEDERATION_POLICY_PATH}"
	local next_policy
	[ -f "${policy_file}" ] || die "federation policy not found: ${FEDERATION_POLICY_PATH}"
	jq -e '.allowed_event_types | type == "array"' "${policy_file}" >/dev/null ||
		die "federation policy allowed_event_types must be an array"

	case "${FEDERATION_POLICY_PROBE}" in
	deny)
		jq -e --arg event_type "${FEDERATION_POLICY_EVENT_TYPE}" \
			'.allowed_event_types | index($event_type) == null' "${policy_file}" >/dev/null ||
			die "canonical federation policy must deny ${FEDERATION_POLICY_EVENT_TYPE}"
		;;
	allow)
		next_policy="${policy_file}.next"
		jq --arg event_type "${FEDERATION_POLICY_EVENT_TYPE}" \
			'.allowed_event_types |= (. + [$event_type] | unique)' \
			"${policy_file}" >"${next_policy}"
		mv "${next_policy}" "${policy_file}"
		;;
	*) die "unsupported federation policy probe mode: ${FEDERATION_POLICY_PROBE}" ;;
	esac
}

create_federation_secrets() {
	local ca_cert="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
	[ -r "${ca_cert}" ] || die "local CA certificate not found: ${ca_cert}"
	local bootstrap_json key value
	local bootstrap_arguments=()
	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output json 2>/dev/null || printf '{}')"
	# Preserve existing lab identities while making upgrades self-healing when a new homeserver is
	# added to an already running, ownership-labelled cluster.
	for key in pg-synapse pg-synapse-b pg-synapse-c pg-keycloak pg-kagent \
		alice-password bob-password charlie-password keycloak-admin-password \
		fgentic-client-secret fgentic-alice-password fgentic-bob-password \
		org-b-a2a-client-secret untrusted-a2a-client-secret \
		wrong-audience-a2a-client-secret; do
		value="$(jq -r --arg key "${key}" '.data[$key] // "" | @base64d' \
			<<<"${bootstrap_json}")"
		[ -n "${value}" ] || value="$(random_hex 24)"
		bootstrap_arguments+=("--from-literal=${key}=${value}")
	done
	bootstrap_arguments+=(
		"--from-file=agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}"
		"--from-literal=agent-card-key-id=${AGENT_CARD_KEY_ID}"
	)
	apply_secret flux-system fgentic-demo-bootstrap "${bootstrap_arguments[@]}"
	bootstrap_json=""
	value=""

	local pg_synapse pg_synapse_b pg_synapse_c pg_keycloak pg_kagent namespace
	pg_synapse="$(bootstrap_secret_value pg-synapse)"
	pg_synapse_b="$(bootstrap_secret_value pg-synapse-b)"
	pg_synapse_c="$(bootstrap_secret_value pg-synapse-c)"
	pg_keycloak="$(bootstrap_secret_value pg-keycloak)"
	pg_kagent="$(bootstrap_secret_value pg-kagent)"
	apply_secret postgres pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${pg_synapse}"
	apply_secret matrix pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${pg_synapse}"
	apply_secret postgres pg-synapse-b --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_b --from-literal=password="${pg_synapse_b}"
	apply_secret matrix-b pg-synapse-b --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_b --from-literal=password="${pg_synapse_b}"
	apply_secret postgres pg-synapse-c --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_c --from-literal=password="${pg_synapse_c}"
	apply_secret matrix-c pg-synapse-c --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_c --from-literal=password="${pg_synapse_c}"
	apply_secret postgres pg-keycloak --type=kubernetes.io/basic-auth \
		--from-literal=username=keycloak --from-literal=password="${pg_keycloak}"
	apply_secret keycloak pg-keycloak --type=kubernetes.io/basic-auth \
		--from-literal=username=keycloak --from-literal=password="${pg_keycloak}"
	apply_secret postgres pg-kagent --type=kubernetes.io/basic-auth \
		--from-literal=username=kagent --from-literal=password="${pg_kagent}"
	apply_secret kagent kagent-db \
		--from-literal=url="postgresql://kagent:${pg_kagent}@platform-pg-rw.postgres.svc.cluster.local:5432/kagent?sslmode=require"
	apply_secret kagent kagent-model-auth \
		--from-literal=OPENAI_API_KEY=sk-not-used-agentgateway-holds-the-real-key
	apply_secret keycloak keycloak-credentials \
		--from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin \
		--from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(bootstrap_secret_value keycloak-admin-password)" \
		--from-literal=FGENTIC_CLIENT_SECRET="$(bootstrap_secret_value fgentic-client-secret)" \
		--from-literal=FGENTIC_ALICE_PASSWORD="$(bootstrap_secret_value fgentic-alice-password)" \
		--from-literal=FGENTIC_BOB_PASSWORD="$(bootstrap_secret_value fgentic-bob-password)" \
		--from-literal=ORG_B_A2A_CLIENT_SECRET="$(bootstrap_secret_value org-b-a2a-client-secret)" \
		--from-literal=UNTRUSTED_A2A_CLIENT_SECRET="$(bootstrap_secret_value untrusted-a2a-client-secret)" \
		--from-literal=WRONG_AUDIENCE_A2A_CLIENT_SECRET="$(bootstrap_secret_value wrong-audience-a2a-client-secret)"

	# Only the public root is mirrored into the homeserver namespaces. The CA key remains in
	# cert-manager, and both runtime and config-check pods mount this ConfigMap read-only.
	for namespace in matrix matrix-b matrix-c; do
		kubectl --namespace "${namespace}" create configmap fgentic-local-ca \
			--from-file="ca.crt=${ca_cert}" --dry-run=client --output=yaml |
			kubectl apply --filename - >/dev/null
	done
	publish_federation_agent_card_artifacts
}
