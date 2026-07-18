#!/usr/bin/env bash
# Local Vertex AI credentials for agentgateway on k3d: copies your gcloud Application Default
# Credentials into the `gcp-adc` Secret (key credentials.json) that the local overlay wires
# into the LLM backend. The quota project is stamped into the COPY only — your global ADC file
# is never modified. Never committed; on GKE, Workload Identity replaces this entirely.
set -euo pipefail

PROJECT="${1:?usage: local-adc.sh <gcp-project-id>}"
ADC="${GOOGLE_APPLICATION_CREDENTIALS:-${HOME}/.config/gcloud/application_default_credentials.json}"
[ -f "${ADC}" ] || {
	echo "No ADC found at ${ADC} — run: gcloud auth application-default login"
	exit 1
}

tmp="$(mktemp)"
trap 'rm -f "${tmp}"' EXIT
jq --arg p "${PROJECT}" '. + {quota_project_id: $p}' "${ADC}" >"${tmp}"

kubectl get namespace agentgateway-system >/dev/null 2>&1 || kubectl create namespace agentgateway-system
secret_manifest="$(kubectl -n agentgateway-system create secret generic gcp-adc \
	--from-file=credentials.json="${tmp}" --dry-run=client -o yaml)"
kubectl apply -f - <<<"${secret_manifest}"
echo "Secret agentgateway-system/gcp-adc applied (quota project: ${PROJECT})."
