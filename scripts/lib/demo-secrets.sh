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
	local workload="$2"
	jq --null-input --compact-output --arg key "${key}" --arg workload "${workload}" \
		'{key: $key, metadata: {workload: $workload}}'
}

apply_a2a_secrets() {
	local key="$1"
	local bridge_caller ap_token ap_caller
	bridge_caller="$(a2a_caller_document "${key}" matrix-a2a-bridge)"
	# Demo also reconciles the ActivityPub agent gateway (issue #489), a second caller of the A2A
	# chokepoint. Register its workload credential alongside the bridge's in the SAME callers Secret so
	# agentgateway resolves its Authorization bearer to the `activitypub-agent-gateway` workload the
	# demo-scoped CEL admits (clusters/demo/kustomization.yaml). local/gcp use the SOPS caller Secret,
	# which has no such entry, keeping their single-caller boundary intact.
	ap_token="$(bootstrap_secret_value ap-a2a-key)"
	ap_caller="$(a2a_caller_document "${ap_token}" activitypub-agent-gateway)"
	apply_secret agentgateway-system a2a-bridge-callers \
		--from-literal=matrix-a2a-bridge="${bridge_caller}" \
		--from-literal=activitypub-agent-gateway="${ap_caller}"
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
		pg-synapse:24 pg-mas:24 pg-bridge:24 pg-kagent:24 pg-activitypub:24
		pg-knowledge-owner:24 pg-knowledge-retrieval:24
		as-token:32 hs-token:32 a2a-key:32 ap-a2a-key:32 mcp-platform-helper-key:32
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
	PG_ACTIVITYPUB="$(bootstrap_secret_value pg-activitypub)"
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
	apply_secret postgres pg-activitypub --type=kubernetes.io/basic-auth \
		--from-literal=username=activitypub --from-literal=password="${PG_ACTIVITYPUB}"
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
# across restarts; the A2A workload credential (bootstrap ap-a2a-key, also registered as an
# agentgateway caller by apply_a2a_secrets) proves the gateway is the caller to agentgateway.
# Generate one ActivityPub PEM key into the demo bootstrap Secret if (and only if) it is absent, so a
# retained cluster preserves established identities and only newly added keys are minted.
# Args: <bootstrap-key-name> <openssl-algorithm> [pkeyopt]
ensure_activitypub_bootstrap_key() {
	local bootstrap_key="$1"
	local algorithm="$2"
	local pkeyopt="${3:-}"
	local existing generated patch_document patch_data
	existing="$(bootstrap_secret_value "${bootstrap_key}")"
	[ -n "${existing}" ] && return 0
	if [ -n "${pkeyopt}" ]; then
		generated="$(openssl genpkey -algorithm "${algorithm}" -pkeyopt "${pkeyopt}" 2>/dev/null)"
	else
		generated="$(openssl genpkey -algorithm "${algorithm}" 2>/dev/null)"
	fi
	[ -n "${generated}" ] || die "failed to generate ActivityPub ${bootstrap_key} (${algorithm})"
	patch_document="$(kubectl --namespace flux-system create secret generic fgentic-demo-bootstrap \
		--from-literal="${bootstrap_key}=${generated}" --dry-run=client --output=json)"
	patch_data="$(jq --compact-output '{data: .data}' <<<"${patch_document}")"
	printf '%s\n' "${patch_data}" \
		| kubectl --namespace flux-system patch secret fgentic-demo-bootstrap \
			--type=merge --patch-file /dev/stdin >/dev/null
}

create_activitypub_secrets() {
	local ap_identity ap_signing ap_rsa ap_token ap_db_password
	# Persist each PEM key in the bootstrap Secret independently, generating ONLY the missing ones so a
	# retained cluster keeps its stable did:key / #main-key anchors while a newly required key (e.g. the
	# RSA transport key rsa.pem, #476 — the deploy enables groups + the status feed, without which the
	# pod fails to start) self-heals on upgrade without rotating the established identity keys.
	ensure_activitypub_bootstrap_key ap-identity-key EC ec_paramgen_curve:P-256
	ensure_activitypub_bootstrap_key ap-signing-key ed25519
	ensure_activitypub_bootstrap_key ap-rsa-key RSA rsa_keygen_bits:2048 # >= 2048 (chart-enforced)

	ap_identity="$(bootstrap_secret_value ap-identity-key)"
	ap_signing="$(bootstrap_secret_value ap-signing-key)"
	ap_rsa="$(bootstrap_secret_value ap-rsa-key)"
	ap_token="$(bootstrap_secret_value ap-a2a-key)"
	apply_secret activitypub activitypub-agent-gateway-identity-key \
		--from-literal="p256.pem=${ap_identity}"
	# Both object-integrity (ed25519.pem, FEP-8b32) and hop-signature (rsa.pem, #476) keys live in the
	# single signing-key Secret the deploy mounts for integrity + groups + status feed.
	apply_secret activitypub activitypub-agent-gateway-signing-key \
		--from-literal="ed25519.pem=${ap_signing}" \
		--from-literal="rsa.pem=${ap_rsa}"
	apply_secret activitypub activitypub-agent-gateway-credential \
		--from-literal="token=${ap_token}"

	# Namespace-local DATABASE_URL for the durable inbox activity ledger (#321). Same password as the
	# matching CNPG pg-activitypub Secret, mirroring the bridge's matrix-a2a-bridge-db credential.
	ap_db_password="$(bootstrap_secret_value pg-activitypub)"
	apply_secret activitypub activitypub-agent-gateway-db \
		--from-literal="url=postgres://activitypub:${ap_db_password}@platform-pg-rw.postgres.svc.cluster.local:5432/activitypub?sslmode=require"
}
