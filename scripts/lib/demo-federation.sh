#!/usr/bin/env bash
# Definition-only federation snapshot, signing, and secret helpers sourced by scripts/demo.sh.
replace_split_ca_marker() {
	local marker="$1"
	local certificate="$2"
	local marker_files=()
	local marker_count marker_output path pem
	[ -f "${certificate}" ] && [ ! -L "${certificate}" ] && [ -r "${certificate}" ] ||
		die "split federation CA certificate is not a readable regular file: ${certificate}"
	openssl x509 -in "${certificate}" -noout >/dev/null 2>&1 ||
		die "split federation CA certificate is invalid: ${certificate}"
	marker_output="$(rg --files-with-matches --fixed-strings --glob '*.yaml' \
		"${marker}" "${SNAPSHOT_DIR}" || true)"
	while IFS= read -r path; do
		[ -z "${path}" ] || marker_files[${#marker_files[@]}]="${path}"
	done <<<"${marker_output}"
	((${#marker_files[@]} > 0)) || die "split federation CA marker is missing: ${marker}"
	marker_count="$(rg --only-matching --fixed-strings "${marker}" \
		"${marker_files[@]}" | wc -l | tr -d ' ')"
	[[ "${marker_count}" =~ ^[1-9][0-9]*$ ]] ||
		die "split federation CA marker inventory is invalid: ${marker}"
	pem="$(<"${certificate}")"
	for path in "${marker_files[@]}"; do
		CA_MARKER="${marker}" CA_PEM="${pem}" yq --inplace \
			'(... | select(tag == "!!str" and contains(strenv(CA_MARKER)))) |=
        sub(strenv(CA_MARKER); strenv(CA_PEM))' \
			"${path}"
	done
	if rg --fixed-strings --glob '*.yaml' "${marker}" "${SNAPSHOT_DIR}" >/dev/null; then
		die "split federation CA marker remained after snapshot injection: ${marker}"
	fi
}

assert_no_split_ca_markers() {
	if rg --fixed-strings --glob '*.yaml' '__FGENTIC_SPLIT_ORG_' \
		"${SNAPSHOT_DIR}" >/dev/null; then
		die "split federation public-root marker remained in the YAML snapshot"
	fi
}

configure_split_snapshot() {
	local ca_a="${FGENTIC_FED_CA_DIR_A:?}/ca.crt"
	local ca_b="${FGENTIC_FED_CA_DIR_B:?}/ca.crt"
	local settings="${SNAPSHOT_DIR}/${PLATFORM_SETTINGS_PATH}"
	[ -f "${settings}" ] || die "split federation platform settings not found"
	[[ "${FEDERATION_LOCAL_GATEWAY_IP}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
		die "split federation local gateway IP is invalid"
	[[ "${FEDERATION_REMOTE_GATEWAY_IP}" =~ ^[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+$ ]] ||
		die "split federation remote gateway IP is invalid"
	FED_LOCAL_GATEWAY_IP="${FEDERATION_LOCAL_GATEWAY_IP}" \
		FED_REMOTE_GATEWAY_IP="${FEDERATION_REMOTE_GATEWAY_IP}" yq --inplace '
      .data.federation_local_gateway_ip = strenv(FED_LOCAL_GATEWAY_IP) |
      .data.federation_remote_gateway_ip = strenv(FED_REMOTE_GATEWAY_IP)
    ' "${settings}"
	replace_split_ca_marker '__FGENTIC_SPLIT_ORG_A_CA_PEM__' "${ca_a}"
	replace_split_ca_marker '__FGENTIC_SPLIT_ORG_B_CA_PEM__' "${ca_b}"
	assert_no_split_ca_markers
}

snapshot_source() {
	SNAPSHOT_DIR="${WORK_DIR}/snapshot"
	mkdir -p "${SNAPSHOT_DIR}"
	if [ -n "$(git -C "${ROOT_DIR}" status --porcelain)" ]; then
		echo "Note: the ephemeral demo snapshot includes the current uncommitted working tree."
	fi
	(
		cd "${ROOT_DIR}" || exit
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
    ' "${SNAPSHOT_DIR}/${PLATFORM_SETTINGS_PATH}"
	if [ "${PROFILE}" = "federation" ]; then
		if [ "${FEDERATION_LAYOUT:-canonical}" = canonical ]; then
			FED_GATEWAY_IP="${FEDERATION_GATEWAY_IP}" yq --inplace \
				'.data.federation_gateway_ip = strenv(FED_GATEWAY_IP)' \
				"${SNAPSHOT_DIR}/${PLATFORM_SETTINGS_PATH}"
		else
			configure_split_snapshot
		fi
		configure_federation_policy_snapshot
		if federation_seller_runtime_enabled; then
			sign_federation_agent_card_snapshot
		fi
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
	local bootstrap_json configmap_json encoded_private_key encoded_receipt_key
	local existing_receipt_jwk public_artifacts_exist
	AGENT_CARD_PRIVATE_KEY="${WORK_DIR}/agent-card-private-key.pem"
	AGENT_CARD_KEY_ID=""
	EXISTING_AGENT_CARD_JWK_FILE=""
	USAGE_RECEIPT_PRIVATE_KEY="${WORK_DIR}/usage-receipt-private-key.pem"
	USAGE_RECEIPT_KEY_ID=""
	EXISTING_USAGE_RECEIPT_JWK_FILE=""
	chmod 700 "${WORK_DIR}"

	# The signing key is needed before the ephemeral Git snapshot exists. Keep it only in the
	# lifecycle-owned bootstrap Secret; neither the signed source nor a workload namespace ever
	# receives private material. Reusing the key keeps the public trust anchor stable across up.
	kubectl create namespace flux-system --dry-run=client --output=yaml |
		kubectl apply --filename - >/dev/null
	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output json 2>/dev/null || printf '{}')"
	public_artifacts_exist=false
	if configmap_json="$(kubectl --namespace agentgateway-system get configmap \
		"${FEDERATION_AGENT_CARD_CONFIGMAP}" --output json 2>/dev/null)"; then
		public_artifacts_exist=true
		EXISTING_AGENT_CARD_JWK_FILE="${WORK_DIR}/existing-public-jwk.json"
		jq -er '.data["public-jwk.json"] | select(type == "string" and length > 0)' \
			<<<"${configmap_json}" >"${EXISTING_AGENT_CARD_JWK_FILE}" ||
			die "existing federation AgentCard public JWK is invalid"
		jq -e '
      keys == ["alg", "crv", "key_ops", "kid", "kty", "use", "x", "y"] and
      .kty == "EC" and .crv == "P-256" and .alg == "ES256" and
      .use == "sig" and .key_ops == ["verify"] and (has("d") | not)
    ' "${EXISTING_AGENT_CARD_JWK_FILE}" >/dev/null ||
			die "existing federation AgentCard public JWK is invalid"
		existing_receipt_jwk="${WORK_DIR}/existing-usage-receipt-public-jwk.json"
		if jq -er '
          .data["usage-receipt-public-jwk.json"] |
          select(type == "string" and length > 0)
        ' <<<"${configmap_json}" >"${existing_receipt_jwk}"; then
			jq -e '
          keys == ["alg", "crv", "key_ops", "kid", "kty", "use", "x", "y"] and
          .kty == "EC" and .crv == "P-256" and .alg == "ES256" and
          .use == "sig" and .key_ops == ["verify"] and (has("d") | not)
        ' "${existing_receipt_jwk}" >/dev/null ||
				die "existing federation usage-receipt public JWK is invalid"
			EXISTING_USAGE_RECEIPT_JWK_FILE="${existing_receipt_jwk}"
		else
			rm -f "${existing_receipt_jwk}"
		fi
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

	encoded_receipt_key="$(jq -r '.data["usage-receipt-private-key"] // ""' \
		<<<"${bootstrap_json}")"
	USAGE_RECEIPT_KEY_ID="$(jq -r '.data["usage-receipt-key-id"] // "" | @base64d' \
		<<<"${bootstrap_json}")"
	if [ -n "${encoded_receipt_key}" ]; then
		printf '%s' "${encoded_receipt_key}" | base64 --decode >"${USAGE_RECEIPT_PRIVATE_KEY}"
		[ -n "${USAGE_RECEIPT_KEY_ID}" ] ||
			die "federation usage-receipt key ID is missing from the bootstrap Secret"
	else
		[ -z "${EXISTING_USAGE_RECEIPT_JWK_FILE}" ] ||
			die "refusing to rotate a missing usage-receipt key while public artifacts still exist"
		USAGE_RECEIPT_KEY_ID="${FEDERATION_USAGE_RECEIPT_DEFAULT_KEY_ID}"
		openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 \
			-out "${USAGE_RECEIPT_PRIVATE_KEY}" 2>/dev/null
	fi
	chmod 600 "${USAGE_RECEIPT_PRIVATE_KEY}"

	if kubectl --namespace flux-system get secret fgentic-demo-bootstrap >/dev/null 2>&1; then
		kubectl --namespace flux-system create secret generic fgentic-demo-bootstrap \
			--from-file="agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}" \
			--from-literal="agent-card-key-id=${AGENT_CARD_KEY_ID}" \
			--from-file="usage-receipt-private-key=${USAGE_RECEIPT_PRIVATE_KEY}" \
			--from-literal="usage-receipt-key-id=${USAGE_RECEIPT_KEY_ID}" \
			--dry-run=client --output=json |
			jq --compact-output '{data: .data}' |
			kubectl --namespace flux-system patch secret fgentic-demo-bootstrap \
				--type=merge --patch-file /dev/stdin \
			>/dev/null
	else
		apply_secret flux-system fgentic-demo-bootstrap \
			--from-file="agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}" \
			--from-literal="agent-card-key-id=${AGENT_CARD_KEY_ID}" \
			--from-file="usage-receipt-private-key=${USAGE_RECEIPT_PRIVATE_KEY}" \
			--from-literal="usage-receipt-key-id=${USAGE_RECEIPT_KEY_ID}"
	fi
	bootstrap_json=""
	configmap_json=""
	encoded_private_key=""
	encoded_receipt_key=""
}

sign_federation_agent_card_snapshot() {
	local template policy settings bundle marker_count signed_card card_server card_partner
	template="${SNAPSHOT_DIR}/${FEDERATION_AGENT_CARD_TEMPLATE_PATH}"
	policy="${SNAPSHOT_DIR}/${FEDERATION_AGENT_CARD_POLICY_PATH}"
	settings="${SNAPSHOT_DIR}/${PLATFORM_SETTINGS_PATH}"
	bundle="${WORK_DIR}/agent-card-bundle.json"
	AGENT_CARD_PUBLIC_FILE="${WORK_DIR}/agent-card.json"
	AGENT_CARD_JWK_FILE="${WORK_DIR}/public-jwk.json"
	USAGE_RECEIPT_JWK_FILE="${WORK_DIR}/usage-receipt-public-jwk.json"
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
	"${ROOT_DIR}/scripts/usage-receipt.sh" public-jwk \
		--private-key "${USAGE_RECEIPT_PRIVATE_KEY}" --key-id "${USAGE_RECEIPT_KEY_ID}" \
		>"${USAGE_RECEIPT_JWK_FILE}"
	jq -e '
      keys == ["alg", "crv", "key_ops", "kid", "kty", "use", "x", "y"] and
      .kty == "EC" and .crv == "P-256" and .alg == "ES256" and
      .use == "sig" and .key_ops == ["verify"] and (has("d") | not)
    ' "${USAGE_RECEIPT_JWK_FILE}" >/dev/null ||
		die "generated federation usage-receipt public JWK is invalid"
	RECEIPT_EXTENSION="${FEDERATION_USAGE_RECEIPT_EXTENSION}" \
		RECEIPT_KEY_ID="${USAGE_RECEIPT_KEY_ID}" \
		RECEIPT_JWK_FILE="${USAGE_RECEIPT_JWK_FILE}" yq --inplace '
      (.capabilities.extensions[] | select(.uri == strenv(RECEIPT_EXTENSION)).params.keyId) =
        strenv(RECEIPT_KEY_ID) |
      (.capabilities.extensions[] | select(.uri == strenv(RECEIPT_EXTENSION)).params.publicJwk) =
        load(strenv(RECEIPT_JWK_FILE))
    ' "${template}"
	if rg --fixed-strings '__FGENTIC_USAGE_RECEIPT_' "${template}" >/dev/null; then
		die "usage-receipt public material was not injected before AgentCard signing"
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
	if [ -n "${EXISTING_USAGE_RECEIPT_JWK_FILE}" ]; then
		jq --exit-status --slurp '.[0] == .[1]' \
			"${EXISTING_USAGE_RECEIPT_JWK_FILE}" "${USAGE_RECEIPT_JWK_FILE}" >/dev/null ||
			die "refusing to replace the independently pinnable usage-receipt public JWK"
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
		--from-file="usage-receipt-public-jwk.json=${USAGE_RECEIPT_JWK_FILE}" \
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

create_canonical_federation_secrets() {
	local ca_cert="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
	[ -r "${ca_cert}" ] || die "local CA certificate not found: ${ca_cert}"
	local bootstrap_json key value
	local bootstrap_arguments=()
	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output json 2>/dev/null || printf '{}')"
	# Preserve existing lab identities while making upgrades self-healing when a new homeserver is
	# added to an already running, ownership-labelled cluster.
	for key in pg-synapse pg-synapse-b pg-synapse-c pg-keycloak pg-kagent \
		pg-knowledge-owner pg-knowledge-retrieval \
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
		"--from-file=usage-receipt-private-key=${USAGE_RECEIPT_PRIVATE_KEY}"
		"--from-literal=usage-receipt-key-id=${USAGE_RECEIPT_KEY_ID}"
	)
	apply_secret flux-system fgentic-demo-bootstrap "${bootstrap_arguments[@]}"
	apply_secret agentgateway-system federated-usage-receipt-signing \
		--from-file="private-key.pem=${USAGE_RECEIPT_PRIVATE_KEY}" \
		--from-literal="key-id=${USAGE_RECEIPT_KEY_ID}"
	bootstrap_json=""
	value=""

	local pg_synapse pg_synapse_b pg_synapse_c pg_keycloak pg_kagent
	local pg_knowledge_owner pg_knowledge_retrieval namespace
	pg_synapse="$(bootstrap_secret_value pg-synapse)"
	pg_synapse_b="$(bootstrap_secret_value pg-synapse-b)"
	pg_synapse_c="$(bootstrap_secret_value pg-synapse-c)"
	pg_keycloak="$(bootstrap_secret_value pg-keycloak)"
	pg_kagent="$(bootstrap_secret_value pg-kagent)"
	pg_knowledge_owner="$(bootstrap_secret_value pg-knowledge-owner)"
	pg_knowledge_retrieval="$(bootstrap_secret_value pg-knowledge-retrieval)"
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
	apply_secret postgres pg-knowledge-owner --type=kubernetes.io/basic-auth \
		--from-literal=username=knowledge_owner --from-literal=password="${pg_knowledge_owner}"
	apply_secret postgres pg-knowledge-retrieval --type=kubernetes.io/basic-auth \
		--from-literal=username=knowledge_retrieval --from-literal=password="${pg_knowledge_retrieval}"
	apply_secret knowledge pg-knowledge-retrieval --type=kubernetes.io/basic-auth \
		--from-literal=username=knowledge_retrieval --from-literal=password="${pg_knowledge_retrieval}"
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

create_split_federation_secrets() {
	local bootstrap_json key value
	local bootstrap_arguments=()
	local keys=()
	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output json 2>/dev/null || printf '{}')"
	case "${FEDERATION_LAYOUT}" in
	split-a)
		keys=(pg-synapse pg-synapse-c pg-kagent alice-password charlie-password)
		;;
	split-b)
		keys=(
			pg-synapse-b pg-keycloak bob-password keycloak-admin-password
			fgentic-client-secret fgentic-alice-password fgentic-bob-password
			org-b-a2a-client-secret untrusted-a2a-client-secret
			wrong-audience-a2a-client-secret
		)
		;;
	*) die "split federation secret dispatch requires split-a or split-b" ;;
	esac
	for key in "${keys[@]}"; do
		value="$(jq -r --arg key "${key}" '.data[$key] // "" | @base64d' \
			<<<"${bootstrap_json}")"
		[ -n "${value}" ] || value="$(random_hex 24)"
		bootstrap_arguments+=("--from-literal=${key}=${value}")
	done
	if [ "${FEDERATION_LAYOUT}" = split-a ]; then
		bootstrap_arguments+=(
			"--from-file=agent-card-private-key=${AGENT_CARD_PRIVATE_KEY}"
			"--from-literal=agent-card-key-id=${AGENT_CARD_KEY_ID}"
			"--from-file=usage-receipt-private-key=${USAGE_RECEIPT_PRIVATE_KEY}"
			"--from-literal=usage-receipt-key-id=${USAGE_RECEIPT_KEY_ID}"
		)
	fi
	apply_secret flux-system fgentic-demo-bootstrap "${bootstrap_arguments[@]}"
	bootstrap_json=""
	value=""

	case "${FEDERATION_LAYOUT}" in
	split-a)
		local pg_synapse pg_synapse_c pg_kagent
		pg_synapse="$(bootstrap_secret_value pg-synapse)"
		pg_synapse_c="$(bootstrap_secret_value pg-synapse-c)"
		pg_kagent="$(bootstrap_secret_value pg-kagent)"
		apply_secret agentgateway-system federated-usage-receipt-signing \
			--from-file="private-key.pem=${USAGE_RECEIPT_PRIVATE_KEY}" \
			--from-literal="key-id=${USAGE_RECEIPT_KEY_ID}"
		apply_secret postgres pg-synapse --type=kubernetes.io/basic-auth \
			--from-literal=username=synapse --from-literal=password="${pg_synapse}"
		apply_secret matrix pg-synapse --type=kubernetes.io/basic-auth \
			--from-literal=username=synapse --from-literal=password="${pg_synapse}"
		apply_secret postgres pg-synapse-c --type=kubernetes.io/basic-auth \
			--from-literal=username=synapse_c --from-literal=password="${pg_synapse_c}"
		apply_secret matrix-c pg-synapse-c --type=kubernetes.io/basic-auth \
			--from-literal=username=synapse_c --from-literal=password="${pg_synapse_c}"
		apply_secret postgres pg-kagent --type=kubernetes.io/basic-auth \
			--from-literal=username=kagent --from-literal=password="${pg_kagent}"
		apply_secret kagent kagent-db \
			--from-literal=url="postgresql://kagent:${pg_kagent}@platform-pg-rw.postgres.svc.cluster.local:5432/kagent?sslmode=require"
		apply_secret kagent kagent-model-auth \
			--from-literal=OPENAI_API_KEY=sk-not-used-agentgateway-holds-the-real-key
		publish_federation_agent_card_artifacts
		;;
	split-b)
		local pg_synapse_b pg_keycloak
		pg_synapse_b="$(bootstrap_secret_value pg-synapse-b)"
		pg_keycloak="$(bootstrap_secret_value pg-keycloak)"
		apply_secret postgres pg-synapse-b --type=kubernetes.io/basic-auth \
			--from-literal=username=synapse_b --from-literal=password="${pg_synapse_b}"
		apply_secret matrix-b pg-synapse-b --type=kubernetes.io/basic-auth \
			--from-literal=username=synapse_b --from-literal=password="${pg_synapse_b}"
		apply_secret postgres pg-keycloak --type=kubernetes.io/basic-auth \
			--from-literal=username=keycloak --from-literal=password="${pg_keycloak}"
		apply_secret keycloak pg-keycloak --type=kubernetes.io/basic-auth \
			--from-literal=username=keycloak --from-literal=password="${pg_keycloak}"
		apply_secret keycloak keycloak-credentials \
			--from-literal=KC_BOOTSTRAP_ADMIN_USERNAME=admin \
			--from-literal=KC_BOOTSTRAP_ADMIN_PASSWORD="$(bootstrap_secret_value keycloak-admin-password)" \
			--from-literal=FGENTIC_CLIENT_SECRET="$(bootstrap_secret_value fgentic-client-secret)" \
			--from-literal=FGENTIC_ALICE_PASSWORD="$(bootstrap_secret_value fgentic-alice-password)" \
			--from-literal=FGENTIC_BOB_PASSWORD="$(bootstrap_secret_value fgentic-bob-password)" \
			--from-literal=ORG_B_A2A_CLIENT_SECRET="$(bootstrap_secret_value org-b-a2a-client-secret)" \
			--from-literal=UNTRUSTED_A2A_CLIENT_SECRET="$(bootstrap_secret_value untrusted-a2a-client-secret)" \
			--from-literal=WRONG_AUDIENCE_A2A_CLIENT_SECRET="$(bootstrap_secret_value wrong-audience-a2a-client-secret)"
		;;
	esac
}

create_federation_secrets() {
	case "${FEDERATION_LAYOUT:-canonical}" in
	canonical) create_canonical_federation_secrets ;;
	split-a | split-b) create_split_federation_secrets ;;
	*) die "unsupported federation secret layout: ${FEDERATION_LAYOUT}" ;;
	esac
}
