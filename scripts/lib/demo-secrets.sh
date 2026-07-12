#!/usr/bin/env bash
# Definition-only provider and core secret helpers sourced by scripts/demo.sh.
configure_provider() {
	LLM_PROVIDER="${FGENTIC_LLM_PROVIDER:-demo}"
	LLM_MODEL="${FGENTIC_LLM_MODEL:-}"
	GCP_PROJECT="${FGENTIC_GCP_PROJECT:-not-configured}"
	VERTEX_REGION="${FGENTIC_VERTEX_REGION:-europe-west1}"
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
		[ "${FGENTIC_ALLOW_PAID_PROVIDER:-}" = "yes" ] ||
			die "Vertex can incur cost; set FGENTIC_ALLOW_PAID_PROVIDER=yes explicitly"
		[ "${GCP_PROJECT}" != "not-configured" ] ||
			die "FGENTIC_GCP_PROJECT is required for the Vertex profile"
		[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] ||
			die "GOOGLE_APPLICATION_CREDENTIALS is required for the Vertex profile"
		[ -r "${GOOGLE_APPLICATION_CREDENTIALS}" ] ||
			die "GOOGLE_APPLICATION_CREDENTIALS is not readable"
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
		[ "${AZURE_OPENAI_RESOURCE}" != "not-configured" ] ||
			die "FGENTIC_AZURE_OPENAI_RESOURCE is required for Azure OpenAI"
		;;
	*)
		die "unsupported FGENTIC_LLM_PROVIDER: ${LLM_PROVIDER}"
		;;
	esac

	if [ -n "${MODEL_SECRET_ENV}" ]; then
		[ "${FGENTIC_ALLOW_PAID_PROVIDER:-}" = "yes" ] ||
			die "${LLM_PROVIDER} can incur cost; set FGENTIC_ALLOW_PAID_PROVIDER=yes explicitly"
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
		--from-literal=* | --from-file=*)
			[[ "${key}" =~ ^[-._a-zA-Z0-9]+$ ]] || die "invalid Secret data key"
			encoded="$(printf '%s' "${value}" | base64 | tr -d '\r\n')"
			data="${data}${separator}\"${key}\":\"${encoded}\""
			separator=,
			;;
		esac
	done
	printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"%s"},"type":"%s","data":{%s}}\n' \
		"${name}" "${namespace}" "${type}" "${data}" | kubectl apply --filename - >/dev/null
}

create_ephemeral_secrets() {
	if [ "${PROFILE}" = "federation" ]; then
		create_federation_secrets
		return
	fi

	if ! kubectl --namespace flux-system get secret fgentic-demo-bootstrap >/dev/null 2>&1; then
		apply_secret flux-system fgentic-demo-bootstrap \
			--from-literal=pg-synapse="$(random_hex 24)" \
			--from-literal=pg-mas="$(random_hex 24)" \
			--from-literal=pg-bridge="$(random_hex 24)" \
			--from-literal=pg-kagent="$(random_hex 24)" \
			--from-literal=as-token="$(random_hex 32)" \
			--from-literal=hs-token="$(random_hex 32)" \
			--from-literal=a2a-key="$(random_hex 32)" \
			--from-literal=mcp-platform-helper-key="$(random_hex 32)" \
			--from-literal=mas-admin-client="$(random_hex 32)" \
			--from-literal=demo-password="$(random_hex 24)"
	fi

	PG_SYNAPSE="$(bootstrap_secret_value pg-synapse)"
	PG_MAS="$(bootstrap_secret_value pg-mas)"
	PG_BRIDGE="$(bootstrap_secret_value pg-bridge)"
	PG_KAGENT="$(bootstrap_secret_value pg-kagent)"
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
	apply_secret kagent kagent-db \
		--from-literal=url="postgresql://kagent:${PG_KAGENT}@platform-pg-rw.postgres.svc.cluster.local:5432/kagent?sslmode=require"
	apply_secret kagent kagent-model-auth \
		--from-literal=OPENAI_API_KEY=sk-not-used-agentgateway-holds-the-real-key
	apply_secret bridge matrix-a2a-bridge-db \
		--from-literal=url="postgres://bridge:${PG_BRIDGE}@platform-pg-rw.postgres.svc.cluster.local:5432/bridge?sslmode=require"

	local registration
	registration="$(cat <<EOF
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

	local callers
	callers="$(jq --null-input --compact-output --arg key "${A2A_KEY}" \
		'{"matrix-a2a-bridge": {key: $key, metadata: {workload: "matrix-a2a-bridge"}}}')"
	apply_secret agentgateway-system a2a-bridge-callers --from-literal=matrix-a2a-bridge="${callers}"
	apply_secret bridge a2a-bridge-credential --from-literal=token="${A2A_KEY}"

	local mcp_callers
	mcp_callers="$(jq --null-input --compact-output --arg key "${MCP_PLATFORM_HELPER_KEY}" \
		'{"platform-helper": {key: $key, metadata: {agent: "platform-helper"}}}')"
	apply_secret agentgateway-system mcp-agent-callers \
		--from-literal=platform-helper="$(jq -cer '."platform-helper"' <<<"${mcp_callers}")"
	apply_secret kagent platform-helper-mcp-credential \
		--from-literal=authorization="Bearer ${MCP_PLATFORM_HELPER_KEY}"

	local mas_admin_config
	mas_admin_config="$(cat <<EOF
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

	if [ -n "${MODEL_SECRET_NAME}" ]; then
		apply_secret agentgateway-system "${MODEL_SECRET_NAME}" \
			--from-literal=Authorization="${MODEL_SECRET_VALUE}"
	elif [ "${LLM_PROVIDER}" = "vertex" ]; then
		apply_secret agentgateway-system gcp-adc \
			--from-file="key.json=${GOOGLE_APPLICATION_CREDENTIALS}"
	fi
}
