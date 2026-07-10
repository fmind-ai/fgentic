#!/usr/bin/env bash
# Schema-validate Kubernetes manifests: render the bridge Helm chart and lint it + the raw
# infra manifests with kubeconform. Manifests carry ${vars} substituted in-cluster by Flux
# (postBuild.substituteFrom platform-settings), so each file is rendered through
# `flux envsubst --strict` first, using the GCP reference settings as the canonical value set —
# a missing/undeclared variable fails the build. CRDs (Gateway API, cert-manager, CloudNativePG,
# agentgateway, kagent, ESS, Flux) are resolved from the community schema catalog; unknown
# custom resources are skipped rather than failing (they are validated by their own operators).
set -euo pipefail

SETTINGS=clusters/gcp/platform-settings.yaml
SCHEMA_LOCATIONS=(
  -schema-location default
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
)
KUBECONFORM=(kubeconform -strict -ignore-missing-schemas -summary "${SCHEMA_LOCATIONS[@]}")

# Export every platform-settings key as an env var for flux envsubst.
settings_env="$(yq -r '.data | to_entries[] | .key + "=" + .value' "${SETTINGS}")"
while IFS='=' read -r key value; do
  export "${key}=${value}"
done <<< "${settings_env}"

echo "==> Rendering + validating apps/matrix-a2a-bridge/chart"
if [ -d apps/matrix-a2a-bridge/chart ]; then
  helm template matrix-a2a-bridge apps/matrix-a2a-bridge/chart | "${KUBECONFORM[@]}"
fi

echo "==> Substituting + validating raw infra manifests"
# Skip Helm charts, kustomization files, SOPS ciphertext and templates, and Terraform. Inline
# HelmRelease values are rendered by Flux/Helm at apply time, not standalone-schema-valid.
manifest_list="$(find infra clusters -type f \( -name '*.yaml' -o -name '*.yml' \) \
  ! -name 'kustomization.yaml' \
  ! -name '*.sops.yaml' \
  ! -path '*/terraform/*')"
while IFS= read -r manifest; do
  flux envsubst --strict < "${manifest}"
  echo "---"
done <<< "${manifest_list}" | "${KUBECONFORM[@]}"

echo "==> kubeconform OK"
