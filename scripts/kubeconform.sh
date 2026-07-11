#!/usr/bin/env bash
# Schema-validate Kubernetes manifests: render the in-repo bridge chart and pinned external
# vLLM, OpenTelemetry, Jaeger, ESS, and Keycloak charts, then lint them plus the raw infra
# manifests with kubeconform. Manifests carry
# ${vars} substituted in-cluster by Flux
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

echo "==> Rendering + validating the pinned vLLM profile chart"
VLLM_RELEASE=infra/models/vllm/helmrelease.yaml
VLLM_REPOSITORY="$(yq -er 'select(.kind == "HelmRepository" and .metadata.name == "vllm") | .spec.url' infra/flux/sources.yaml)"
VLLM_CHART="$(yq -er '.spec.chart.spec.chart' "${VLLM_RELEASE}")"
VLLM_CHART_VERSION="$(yq -er '.spec.chart.spec.version' "${VLLM_RELEASE}")"
(
  # The committed cluster default intentionally remains Vertex; render this opt-in profile with
  # its documented served-model alias so Helm and Flux substitution see the real value shape.
  export llm_model=Qwen/Qwen2.5-0.5B-Instruct
  yq -o=yaml '.spec.values' "${VLLM_RELEASE}" \
    | flux envsubst --strict \
    | helm template vllm "${VLLM_CHART}" \
      --repo "${VLLM_REPOSITORY}" \
      --version "${VLLM_CHART_VERSION}" \
      --namespace models \
      --values - \
    | "${KUBECONFORM[@]}"
)

echo "==> Rendering + validating the pinned tracing charts"
TRACING_RELEASE=infra/observability/tracing-helmreleases.yaml
for release in otel-collector jaeger; do
  chart="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.chart" "${TRACING_RELEASE}")"
  version="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.version" "${TRACING_RELEASE}")"
  source="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.sourceRef.name" "${TRACING_RELEASE}")"
  repository="$(yq -er "select(.kind == \"HelmRepository\" and .metadata.name == \"${source}\") | .spec.url" infra/flux/sources.yaml)"
  yq -e "select(.metadata.name == \"${release}\") | .spec.values" "${TRACING_RELEASE}" \
    | flux envsubst --strict \
    | helm template "${release}" "${chart}" \
      --repo "${repository}" \
      --version "${version}" \
      --namespace monitoring \
      --values - \
    | "${KUBECONFORM[@]}"
done

echo "==> Rendering + validating apps/matrix-a2a-bridge/chart"
if [ -d apps/matrix-a2a-bridge/chart ]; then
  helm template matrix-a2a-bridge apps/matrix-a2a-bridge/chart | "${KUBECONFORM[@]}"
fi

echo "==> Rendering + validating optional mautrix bridge releases"
for bridge_release in infra/bridges/*/helmrelease.yaml; do
  [ -f "${bridge_release}" ] || continue
  bridge_name="$(yq -er '.metadata.name' "${bridge_release}")"
  yq -e '.spec.values' "${bridge_release}" \
    | flux envsubst --strict \
    | helm template "${bridge_name}" infra/bridges/chart \
      --namespace bridges \
      --values - \
    | "${KUBECONFORM[@]}"
done

echo "==> Rendering + validating pinned ESS matrix-stack chart"
ESS_RELEASE=infra/matrix/helmrelease.yaml
ESS_SOURCE=infra/flux/sources.yaml
ESS_REPOSITORY="$(yq -er 'select(.kind == "OCIRepository" and .metadata.name == "ess-matrix-stack") | .spec.url' "${ESS_SOURCE}")"
ESS_VERSION="$(yq -er 'select(.kind == "OCIRepository" and .metadata.name == "ess-matrix-stack") | .spec.ref.tag' "${ESS_SOURCE}")"
yq -e '.spec.values' "${ESS_RELEASE}" \
  | flux envsubst --strict \
  | helm template ess "${ESS_REPOSITORY}" \
    --version "${ESS_VERSION}" \
    --namespace matrix \
    --values - \
  | sed -e '/^Pulled: /d' -e '/^Digest: /d' \
  | "${KUBECONFORM[@]}"

echo "==> Rendering + validating pinned KeycloakX chart"
keycloak_chart_version="$(yq -er '.spec.chart.spec.version' infra/keycloak/helmrelease.yaml)"
yq -e '.spec.values' infra/keycloak/helmrelease.yaml \
  | flux envsubst --strict \
  | helm template keycloak keycloakx \
    --repo https://codecentric.github.io/helm-charts \
    --version "${keycloak_chart_version}" \
    --namespace keycloak \
    --values - \
  | "${KUBECONFORM[@]}"

echo "==> Substituting + validating raw infra manifests"
# Skip Helm charts, kustomization files, SOPS ciphertext and templates, and Terraform. Other inline
# HelmRelease values are rendered by Flux/Helm at apply time, not standalone-schema-valid.
manifest_list="$(find infra clusters -type f \( -name '*.yaml' -o -name '*.yml' \) \
  ! -name 'kustomization.yaml' \
  ! -name '*.sops.yaml' \
  ! -path '*/bridges/chart/*' \
  ! -path '*/terraform/*' \
  ! -path '*/flux-system/*')"
while IFS= read -r manifest; do
  flux envsubst --strict < "${manifest}"
  echo "---"
done <<< "${manifest_list}" | "${KUBECONFORM[@]}"

echo "==> kubeconform OK"
