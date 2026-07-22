#!/usr/bin/env bash
# Definition-only provider and core secret helpers sourced by scripts/demo.sh.
configure_provider() {
	LLM_PROVIDER="${FGENTIC_LLM_PROVIDER:-demo}"
	LLM_MODEL="${FGENTIC_LLM_MODEL:-}"
	GCP_PROJECT="${FGENTIC_GCP_PROJECT:-not-configured}"
	# shellcheck disable=SC2034 # sourced caller consumes this provider configuration global
	VERTEX_REGION="${FGENTIC_VERTEX_REGION:-europe-west1}"
	# shellcheck disable=SC2034 # sourced caller consumes this provider configuration global
	OPENAI_HOST="${FGENTIC_OPENAI_HOST:-api.openai.com}"
	AZURE_OPENAI_RESOURCE="${FGENTIC_AZURE_OPENAI_RESOURCE:-not-configured}"
	MODEL_SECRET_ENV=""
	MODEL_SECRET_NAME=""

	case "${LLM_PROVIDER}" in
		demo)
			LLM_MODEL="${LLM_MODEL:-fgentic-demo}"
			;;
		vllm)
			LLM_MODEL="${LLM_MODEL:-Qwen/Qwen2.5-0.5B-Instruct}"
			echo "Self-hosted vLLM selected: allow roughly 2.7 GB of downloads and 4-6 GiB of RAM."
			;;
		vertex)
			LLM_MODEL="${LLM_MODEL:-google/gemini-2.5-flash}"
			[ "${FGENTIC_ALLOW_PAID_PROVIDER:-}" = "yes" ] \
				|| die "Vertex can incur cost; set FGENTIC_ALLOW_PAID_PROVIDER=yes explicitly"
			[ "${GCP_PROJECT}" != "not-configured" ] \
				|| die "FGENTIC_GCP_PROJECT is required for the Vertex profile"
			[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] \
				|| die "GOOGLE_APPLICATION_CREDENTIALS is required for the Vertex profile"
			[ -r "${GOOGLE_APPLICATION_CREDENTIALS}" ] \
				|| die "GOOGLE_APPLICATION_CREDENTIALS is not readable"
			;;
		mistral)
			MODEL_SECRET_ENV="MISTRAL_API_KEY"
			MODEL_SECRET_NAME="mistral-secret"
			;;
		anthropic)
			MODEL_SECRET_ENV="ANTHROPIC_API_KEY"
			MODEL_SECRET_NAME="anthropic-secret"
			;;
		openai)
			MODEL_SECRET_ENV="OPENAI_API_KEY"
			MODEL_SECRET_NAME="openai-secret"
			;;
		azure-openai)
			MODEL_SECRET_ENV="AZURE_OPENAI_API_KEY"
			MODEL_SECRET_NAME="azure-openai-secret"
			[ "${AZURE_OPENAI_RESOURCE}" != "not-configured" ] \
				|| die "FGENTIC_AZURE_OPENAI_RESOURCE is required for Azure OpenAI"
			;;
		*)
			die "unsupported FGENTIC_LLM_PROVIDER: ${LLM_PROVIDER}"
			;;
	esac

	if [ -n "${MODEL_SECRET_ENV}" ]; then
		[ "${FGENTIC_ALLOW_PAID_PROVIDER:-}" = "yes" ] \
			|| die "${LLM_PROVIDER} can incur cost; set FGENTIC_ALLOW_PAID_PROVIDER=yes explicitly"
		[ -n "${LLM_MODEL}" ] || die "FGENTIC_LLM_MODEL is required for ${LLM_PROVIDER}"
		MODEL_SECRET_VALUE="${!MODEL_SECRET_ENV:-}"
		[ -n "${MODEL_SECRET_VALUE}" ] || die "${MODEL_SECRET_ENV} is required for ${LLM_PROVIDER}"
	else
		MODEL_SECRET_VALUE=""
	fi
}

apply_secret() {
	local namespace="$1"
	local name="$2"
	shift 2
	local type=Opaque argument source key value encoded data="" separator=""
	for argument in "$@"; do
		case "${argument}" in
			--type=*)
				type="${argument#--type=}"
				;;
			--from-literal=*)
				source="${argument#--from-literal=}"
				key="${source%%=*}"
				value="${source#*=}"
				;;
			--from-file=*)
				source="${argument#--from-file=}"
				key="${source%%=*}"
				value="$(<"${source#*=}")"
				;;
			*)
				die "unsupported apply_secret argument"
				;;
		esac
		case "${argument}" in
			# --type is consumed by the dispatch case above; it carries no Secret data, so it is a
			# no-op here. Without this branch a typed Secret (e.g. kubernetes.io/basic-auth) hits the
			# default and fails closed with "unsupported apply_secret argument" (regression from #624).
			--type=*) ;;
			--from-literal=* | --from-file=*)
				[[ "${key}" =~ ^[-._a-zA-Z0-9]+$ ]] || die "invalid Secret data key"
				encoded="$(printf '%s' "${value}" | base64)"
				encoded="${encoded//$'\r'/}"
				encoded="${encoded//$'\n'/}"
				data="${data}${separator}\"${key}\":\"${encoded}\""
				separator=,
				;;
			*) die "unsupported apply_secret argument" ;;
		esac
	done
	printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"%s"},"type":"%s","data":{%s}}\n' \
		"${name}" "${namespace}" "${type}" "${data}" | kubectl apply --filename - >/dev/null
}

a2a_caller_document() {
	local key="$1"
	jq --null-input --compact-output --arg key "${key}" \
		'{key: $key, metadata: {workload: "matrix-a2a-bridge"}}'
}

apply_a2a_secrets() {
	local key="$1"
	local caller
	caller="$(a2a_caller_document "${key}")"
	apply_secret agentgateway-system a2a-bridge-callers \
		--from-literal=matrix-a2a-bridge="${caller}"
	apply_secret bridge a2a-bridge-credential --from-literal=token="${key}"
}

create_ephemeral_secrets() {
	if [ "${PROFILE}" = "federation" ]; then
		create_federation_secrets
		return
	fi

	# A retained demo cluster may predate a newly introduced credential. Merge only missing keys so
	# upgrades self-heal without rotating any established identity or dropping unknown bootstrap data.
	local bootstrap_json generated_value key key_spec patch_data patch_document value_length
	local -a bootstrap_arguments=()
	local -a bootstrap_key_specs=(
		pg-synapse:24 pg-mas:24 pg-bridge:24 pg-kagent:24
		pg-knowledge-owner:24 pg-knowledge-retrieval:24
		as-token:32 hs-token:32 a2a-key:32 mcp-platform-helper-key:32
		mas-admin-client:32 demo-password:24
	)
	local -a missing_bootstrap_arguments=()
	if ! kubectl --namespace flux-system get secret fgentic-demo-bootstrap >/dev/null 2>&1; then
		for key_spec in "${bootstrap_key_specs[@]}"; do
			key="${key_spec%%:*}"
			value_length="${key_spec##*:}"
			generated_value="$(random_hex "${value_length}")"
			bootstrap_arguments+=("--from-literal=${key}=${generated_value}")
		done
		apply_secret flux-system fgentic-demo-bootstrap "${bootstrap_arguments[@]}"
	fi

	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap --output json)"
	for key_spec in "${bootstrap_key_specs[@]}"; do
		key="${key_spec%%:*}"
		value_length="${key_spec##*:}"
		generated_value="$(jq -r --arg key "${key}" '.data[$key] // ""' <<<"${bootstrap_json}")"
		if [ -z "${generated_value}" ]; then
			generated_value="$(random_hex "${value_length}")"
			missing_bootstrap_arguments+=("--from-literal=${key}=${generated_value}")
		fi
	done
	if [ "${#missing_bootstrap_arguments[@]}" -gt 0 ]; then
		patch_document="$(kubectl --namespace flux-system create secret generic \
			fgentic-demo-bootstrap "${missing_bootstrap_arguments[@]}" \
			--dry-run=client --output=json)"
		patch_data="$(jq --compact-output '{data: .data}' <<<"${patch_document}")"
		printf '%s\n' "${patch_data}" \
			| kubectl --namespace flux-system patch secret fgentic-demo-bootstrap \
				--type=merge --patch-file /dev/stdin >/dev/null
	fi
	bootstrap_json=""

	PG_SYNAPSE="$(bootstrap_secret_value pg-synapse)"
	PG_MAS="$(bootstrap_secret_value pg-mas)"
	PG_BRIDGE="$(bootstrap_secret_value pg-bridge)"
	PG_KAGENT="$(bootstrap_secret_value pg-kagent)"
	PG_KNOWLEDGE_OWNER="$(bootstrap_secret_value pg-knowledge-owner)"
	PG_KNOWLEDGE_RETRIEVAL="$(bootstrap_secret_value pg-knowledge-retrieval)"
	AS_TOKEN="$(bootstrap_secret_value as-token)"
	HS_TOKEN="$(bootstrap_secret_value hs-token)"
	A2A_KEY="$(bootstrap_secret_value a2a-key)"
	MCP_PLATFORM_HELPER_KEY="$(bootstrap_secret_value mcp-platform-helper-key)"
	MAS_ADMIN_CLIENT_SECRET="$(bootstrap_secret_value mas-admin-client)"
	apply_secret postgres pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${PG_SYNAPSE}"
	apply_secret matrix pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${PG_SYNAPSE}"
	apply_secret postgres pg-mas --type=kubernetes.io/basic-auth \
		--from-literal=username=mas --from-literal=password="${PG_MAS}"
	apply_secret matrix pg-mas --type=kubernetes.io/basic-auth \
		--from-literal=username=mas --from-literal=password="${PG_MAS}"
	apply_secret postgres pg-bridge --type=kubernetes.io/basic-auth \
		--from-literal=username=bridge --from-literal=password="${PG_BRIDGE}"
	apply_secret postgres pg-kagent --type=kubernetes.io/basic-auth \
		--from-literal=username=kagent --from-literal=password="${PG_KAGENT}"
	apply_secret postgres pg-knowledge-owner --type=kubernetes.io/basic-auth \
		--from-literal=username=knowledge_owner --from-literal=password="${PG_KNOWLEDGE_OWNER}"
	apply_secret postgres pg-knowledge-retrieval --type=kubernetes.io/basic-auth \
		--from-literal=username=knowledge_retrieval --from-literal=password="${PG_KNOWLEDGE_RETRIEVAL}"
	apply_secret knowledge pg-knowledge-retrieval --type=kubernetes.io/basic-auth \
		--from-literal=username=knowledge_retrieval --from-literal=password="${PG_KNOWLEDGE_RETRIEVAL}"
	apply_secret kagent kagent-db \
		--from-literal=url="postgresql://kagent:${PG_KAGENT}@platform-pg-rw.postgres.svc.cluster.local:5432/kagent?sslmode=require"
	apply_secret kagent kagent-model-auth \
		--from-literal=OPENAI_API_KEY=sk-not-used-agentgateway-holds-the-real-key
	apply_secret bridge matrix-a2a-bridge-db \
		--from-literal=url="postgres://bridge:${PG_BRIDGE}@platform-pg-rw.postgres.svc.cluster.local:5432/bridge?sslmode=require"

	local registration
	registration="$(
		cat <<EOF
id: matrix-a2a-bridge
url: http://matrix-a2a-bridge.bridge.svc.cluster.local:29331
as_token: ${AS_TOKEN}
hs_token: ${HS_TOKEN}
sender_localpart: a2a-bridge
rate_limited: false
namespaces:
  users:
    - regex: '@a2a-bridge:fgentic\\.localhost'
      exclusive: true
    - regex: '@agent-.*:fgentic\\.localhost'
      exclusive: true
EOF
	)"
	apply_secret bridge matrix-a2a-bridge-registration \
		--from-literal=registration.yaml="${registration}"
	apply_secret matrix matrix-a2a-bridge-registration \
		--from-literal=registration.yaml="${registration}"

	apply_a2a_secrets "${A2A_KEY}"

	local mcp_caller
	mcp_caller="$(jq --null-input --compact-output --arg key "${MCP_PLATFORM_HELPER_KEY}" \
		'{key: $key, metadata: {agent: "platform-helper"}}')"
	apply_secret agentgateway-system mcp-agent-callers \
		--from-literal=platform-helper="${mcp_caller}"
	apply_secret kagent platform-helper-mcp-credential \
		--from-literal=authorization="Bearer ${MCP_PLATFORM_HELPER_KEY}"

	local mas_admin_config
	mas_admin_config="$(
		cat <<EOF
clients:
  - client_id: ${MAS_ADMIN_CLIENT_ID}
    client_auth_method: client_secret_basic
    client_secret: ${MAS_ADMIN_CLIENT_SECRET}
policy:
  data:
    admin_clients:
      - ${MAS_ADMIN_CLIENT_ID}
EOF
	)"
	apply_secret matrix mas-demo-admin --from-literal=config.yaml="${mas_admin_config}"

	create_activitypub_secrets

	if [ -n "${MODEL_SECRET_NAME}" ]; then
		apply_secret agentgateway-system "${MODEL_SECRET_NAME}" \
			--from-literal=Authorization="${MODEL_SECRET_VALUE}"
	elif [ "${LLM_PROVIDER}" = "vertex" ]; then
		apply_secret agentgateway-system gcp-adc \
			--from-file="key.json=${GOOGLE_APPLICATION_CREDENTIALS}"
	fi
}

# ActivityPub agent gateway secrets (issue #489, demo profile only). These are the cluster-only
# counterparts of the SOPS templates in infra/secrets/ — generated in-cluster, never committed and
# never SOPS-encrypted (the ephemeral demo secret path). The identity (P-256) and signing (Ed25519)
# PEM keys are persisted in the bootstrap Secret so a retained cluster keeps a stable did:key anchor
# across restarts; the A2A workload credential proves the gateway is the caller to agentgateway.
create_activitypub_secrets() {
	local ap_identity ap_signing ap_token
	ap_identity="$(bootstrap_secret_value ap-identity-key)"
	if [ -z "${ap_identity}" ]; then
		local ap_identity_key ap_signing_key ap_a2a_key ap_patch_document ap_patch_data
		ap_identity_key="$(openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 2>/dev/null)"
		ap_signing_key="$(openssl genpkey -algorithm ed25519 2>/dev/null)"
		ap_a2a_key="$(random_hex 32)"
		ap_patch_document="$(kubectl --namespace flux-system create secret generic \
			fgentic-demo-bootstrap \
			--from-literal="ap-identity-key=${ap_identity_key}" \
			--from-literal="ap-signing-key=${ap_signing_key}" \
			--from-literal="ap-a2a-key=${ap_a2a_key}" \
			--dry-run=client --output=json)"
		ap_patch_data="$(jq --compact-output '{data: .data}' <<<"${ap_patch_document}")"
		printf '%s\n' "${ap_patch_data}" \
			| kubectl --namespace flux-system patch secret fgentic-demo-bootstrap \
				--type=merge --patch-file /dev/stdin >/dev/null
	fi

	ap_identity="$(bootstrap_secret_value ap-identity-key)"
	ap_signing="$(bootstrap_secret_value ap-signing-key)"
	ap_token="$(bootstrap_secret_value ap-a2a-key)"
	apply_secret activitypub activitypub-agent-gateway-identity-key \
		--from-literal="p256.pem=${ap_identity}"
	apply_secret activitypub activitypub-agent-gateway-signing-key \
		--from-literal="ed25519.pem=${ap_signing}"
	apply_secret activitypub activitypub-agent-gateway-credential \
		--from-literal="token=${ap_token}"
}
