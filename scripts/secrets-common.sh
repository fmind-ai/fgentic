#!/usr/bin/env bash
# Shared, side-effect-free secret metadata used by initial generation and selective rotation.
# Callers own argument parsing and mutation; this file only validates names and resolves the
# provider-specific Secret contract.
# shellcheck disable=SC2034 # globals set by resolve_model_secret are return values to callers

validate_secret_environment() {
	case "$1" in
		local | gcp) ;;
		*)
			echo "error: environment must be local or gcp" >&2
			return 2
			;;
	esac
}

validate_server_name() {
	if [[ ! "$1" =~ ^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$ ]]; then
		echo "error: server_name must be a lowercase DNS name" >&2
		return 2
	fi
}

resolve_model_secret() {
	MODEL_SECRET_ENV=""
	MODEL_SECRET_FILE=""
	MODEL_SECRET_NAME=""
	case "$1" in
		vertex | vllm) ;;
		mistral)
			MODEL_SECRET_ENV="MISTRAL_API_KEY"
			MODEL_SECRET_FILE="agentgateway-mistral.sops.yaml"
			MODEL_SECRET_NAME="mistral-secret"
			;;
		anthropic)
			MODEL_SECRET_ENV="ANTHROPIC_API_KEY"
			MODEL_SECRET_FILE="agentgateway-anthropic.sops.yaml"
			MODEL_SECRET_NAME="anthropic-secret"
			;;
		openai)
			MODEL_SECRET_ENV="OPENAI_API_KEY"
			MODEL_SECRET_FILE="agentgateway-openai.sops.yaml"
			MODEL_SECRET_NAME="openai-secret"
			;;
		azure-openai)
			MODEL_SECRET_ENV="AZURE_OPENAI_API_KEY"
			MODEL_SECRET_FILE="agentgateway-azure-openai.sops.yaml"
			MODEL_SECRET_NAME="azure-openai-secret"
			;;
		*)
			echo "error: unsupported llm_provider: $1" >&2
			return 1
			;;
	esac
}
