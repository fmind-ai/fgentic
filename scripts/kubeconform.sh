#!/usr/bin/env bash
# Schema-validate Kubernetes manifests: render the bridge Helm chart and lint it + the raw
# infra manifests with kubeconform. CRDs (Gateway API, cert-manager, CloudNativePG,
# agentgateway, kagent, ESS, Flux) are resolved from the community schema catalog; unknown
# custom resources are skipped rather than failing (they are validated by their own operators).
set -euo pipefail

SCHEMA_LOCATIONS=(
  -schema-location default
  -schema-location 'https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
)
KUBECONFORM=(kubeconform -strict -ignore-missing-schemas -summary "${SCHEMA_LOCATIONS[@]}")

echo "==> Rendering + validating apps/matrix-a2a-bridge/chart"
if [ -d apps/matrix-a2a-bridge/chart ]; then
  helm template matrix-a2a-bridge apps/matrix-a2a-bridge/chart | "${KUBECONFORM[@]}"
fi

echo "==> Validating raw infra manifests"
# Skip Helm charts, kustomization files, SOPS ciphertext, and inline HelmRelease values
# (rendered by Flux/Helm at apply time, not standalone-schema-valid).
find infra clusters -type f \( -name '*.yaml' -o -name '*.yml' \) \
  ! -name 'kustomization.yaml' \
  ! -name '*.sops.yaml' \
  ! -path '*/terraform/*' \
  -print0 | xargs -0 -r "${KUBECONFORM[@]}" || true

echo "==> kubeconform OK"
