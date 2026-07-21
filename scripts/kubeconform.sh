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

# External chart hosts (GitHub Pages, GHCR OCI) intermittently reset the connection mid-render,
# which used to fail the whole gate on a transient fault. Pull each pinned chart once into a local
# cache with bounded retries, then render every block offline from that copy: retrying only the
# fetch keeps the gate deterministic without ever re-running (and so masking) the schema validation
# that follows, and the cache collapses the repeated ESS pulls (matrix + three federation
# homeservers) into a single download.
CHART_CACHE="$(mktemp -d)"
trap 'rm -rf "${CHART_CACHE}"' EXIT

retry() {
	local -i max="${RETRY_ATTEMPTS:-3}" delay="${RETRY_DELAY:-3}" n=1
	until "$@"; do
		if ((n >= max)); then
			echo "  command failed after ${max} attempts: $*" >&2
			return 1
		fi
		echo "  attempt ${n}/${max} failed; retrying in ${delay}s..." >&2
		sleep "${delay}"
		((n++, delay *= 2))
	done
}

# fetch_chart <cache-key> <chart-ref> [helm pull flags...] -> echoes the extracted chart directory.
# `helm pull --untar` writes the chart to <dest>/<chart-name>/; the cache key dedupes repeated pulls.
fetch_chart() {
	local key="$1"
	shift
	local dir="${CHART_CACHE}/${key}"
	if [ ! -d "${dir}" ]; then
		retry helm pull "$@" --untar --untardir "${dir}.tmp" >&2
		mv "${dir}.tmp" "${dir}"
	fi
	find "${dir}" -mindepth 1 -maxdepth 1 -type d
}

# Export every platform-settings key as an env var for flux envsubst.
settings_env="$(yq -r '.data | to_entries[] | .key + "=" + .value' "${SETTINGS}")"
while IFS='=' read -r key value; do
	export "${key}=${value}"
done <<<"${settings_env}"

# Federation-only manifests introduce overlay-scoped substitutions that intentionally do not
# belong in production settings. Export their unreachable fixture values for the raw schema pass;
# the effective org-a/org-b/org-c releases are rendered separately through the recursive overlay.
for key in federation_partner_server_name federation_denied_server_name federation_gateway_ip \
	federation_a2a_max_budget_units federation_a2a_quota_budget_units_per_minute \
	demo_bridge_tag; do
	value="$(yq -er ".data.${key}" clusters/federation/platform-settings.yaml)"
	export "${key}=${value}"
done

echo "==> Rendering + validating the pinned vLLM profile chart"
VLLM_RELEASE=infra/models/vllm/helmrelease.yaml
VLLM_REPOSITORY="$(yq -er 'select(.kind == "HelmRepository" and .metadata.name == "vllm") | .spec.url' infra/flux/sources.yaml)"
VLLM_CHART="$(yq -er '.spec.chart.spec.chart' "${VLLM_RELEASE}")"
VLLM_CHART_VERSION="$(yq -er '.spec.chart.spec.version' "${VLLM_RELEASE}")"
VLLM_CHART_DIR="$(fetch_chart vllm "${VLLM_CHART}" --repo "${VLLM_REPOSITORY}" --version "${VLLM_CHART_VERSION}")"
(
	# The committed cluster default intentionally remains Vertex; render this opt-in profile with
	# its documented served-model alias so Helm and Flux substitution see the real value shape.
	export llm_model=Qwen/Qwen2.5-0.5B-Instruct
	yq -o=yaml '.spec.values' "${VLLM_RELEASE}" \
		| flux envsubst --strict \
		| helm template vllm "${VLLM_CHART_DIR}" \
			--namespace models \
			--values - \
		| "${KUBECONFORM[@]}"
)

echo "==> Rendering + validating the opt-in sovereign embeddings + reranker profile charts"
# #340's runtime reuses the same pinned vLLM chart; render both engines so a bad value shape fails
# here rather than only in a live cluster. Model snapshots are literal (no envsubst vars needed).
for embeddings_release in \
	infra/models/embeddings/embeddings-helmrelease.yaml \
	infra/models/embeddings/reranker-helmrelease.yaml; do
	release_name="$(yq -er '.metadata.name' "${embeddings_release}")"
	yq -o=yaml '.spec.values' "${embeddings_release}" \
		| flux envsubst --strict \
		| helm template "${release_name}" "${VLLM_CHART_DIR}" \
			--namespace models \
			--values - \
		| "${KUBECONFORM[@]}"
done

echo "==> Rendering + validating the pinned tracing charts"
TRACING_RELEASE=infra/observability/tracing-helmreleases.yaml
for release in otel-collector jaeger; do
	chart="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.chart" "${TRACING_RELEASE}")"
	version="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.version" "${TRACING_RELEASE}")"
	source="$(yq -er "select(.metadata.name == \"${release}\") | .spec.chart.spec.sourceRef.name" "${TRACING_RELEASE}")"
	repository="$(yq -er "select(.kind == \"HelmRepository\" and .metadata.name == \"${source}\") | .spec.url" infra/flux/sources.yaml)"
	chart_dir="$(fetch_chart "${release}" "${chart}" --repo "${repository}" --version "${version}")"
	yq -e "select(.metadata.name == \"${release}\") | .spec.values" "${TRACING_RELEASE}" \
		| flux envsubst --strict \
		| helm template "${release}" "${chart_dir}" \
			--namespace monitoring \
			--values - \
		| "${KUBECONFORM[@]}"
done

echo "==> Rendering + validating apps/matrix-a2a-bridge/chart"
if [ -d apps/matrix-a2a-bridge/chart ]; then
	helm template matrix-a2a-bridge apps/matrix-a2a-bridge/chart | "${KUBECONFORM[@]}"
fi

echo "==> Rendering + validating apps/activitypub-agent-gateway/chart"
# The ActivityPub gateway is an opt-in second federation transport (docs/adr/0014); it is not yet
# wired into the reconciled cluster DAG, so — like the mautrix bridge profiles — its chart is
# validated here directly. Render with the public route enabled so the gated HTTPRoute is checked.
if [ -d apps/activitypub-agent-gateway/chart ]; then
	# Render with the public route AND the policy border on so both gated paths are checked.
	helm template activitypub-agent-gateway apps/activitypub-agent-gateway/chart \
		--namespace activitypub \
		--set httpRoute.enabled=true \
		--set policy.enabled=true \
		--set integrity.enabled=true \
		--set integrity.requireInbound=true \
		--set identity.enabled=true \
		--set budget.enabled=true \
		--set groups.enabled=true \
		--set statusFeed.enabled=true \
		--set metrics.podMonitor.enabled=true \
		| "${KUBECONFORM[@]}"
	# Schema-validate its self-contained deploy unit (Namespace + HelmRelease) through Flux envsubst.
	activitypub_manifests="$(find apps/activitypub-agent-gateway/deploy -type f -name '*.yaml' ! -name 'kustomization.yaml' | sort)"
	[[ -n "${activitypub_manifests}" ]] || {
		echo "error: ActivityPub deploy unit contains no schema-validation manifests" >&2
		exit 1
	}
	while IFS= read -r manifest; do
		flux envsubst --strict <"${manifest}"
		echo "---"
	done <<<"${activitypub_manifests}" \
		| "${KUBECONFORM[@]}"
	# Validate the namespace-neutral federation-border policy Component (issue #211) renders.
	kubectl kustomize apps/activitypub-agent-gateway/component | "${KUBECONFORM[@]}"
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
ESS_CHART_DIR="$(fetch_chart ess "${ESS_REPOSITORY}" --version "${ESS_VERSION}")"
yq -e '.spec.values' "${ESS_RELEASE}" \
	| flux envsubst --strict \
	| helm template ess "${ESS_CHART_DIR}" \
		--namespace matrix \
		--values - \
	| "${KUBECONFORM[@]}"

echo "==> Rendering + validating all federation-lab ESS releases"
federation_render="$(flux build kustomization cluster-overlay-validation \
	--path clusters/federation \
	--kustomization-file scripts/testdata/flux-build-kustomization.yaml \
	--dry-run \
	--in-memory-build \
	--strict-substitute \
	--recursive \
	--local-sources GitRepository/flux-system/flux-system=.)"
for homeserver in \
	'matrix matrix-stack' \
	'matrix-b matrix-stack-b' \
	'matrix-c matrix-stack-c'; do
	read -r namespace release <<<"${homeserver}"
	yq -e "select(.kind == \"HelmRelease\" and .metadata.namespace == \"${namespace}\" and
    .metadata.name == \"${release}\") | .spec.values" <<<"${federation_render}" \
		| helm template ess "${ESS_CHART_DIR}" \
			--namespace "${namespace}" \
			--values - \
		| "${KUBECONFORM[@]}"
done

echo "==> Rendering + validating pinned KeycloakX chart"
keycloak_chart_version="$(yq -er '.spec.chart.spec.version' infra/keycloak/helmrelease.yaml)"
keycloak_chart_dir="$(fetch_chart keycloakx keycloakx \
	--repo https://codecentric.github.io/helm-charts --version "${keycloak_chart_version}")"
keycloak_render="${CHART_CACHE}/keycloak-render.yaml"
yq -e '.spec.values' infra/keycloak/helmrelease.yaml \
	| flux envsubst --strict \
	| helm template keycloak "${keycloak_chart_dir}" \
		--namespace keycloak \
		--values - \
		>"${keycloak_render}"
yq -e '
  select(.kind == "StatefulSet" and .metadata.name == "keycloak") |
  .metadata.labels."app.kubernetes.io/version" == "26.7.0" and
  .spec.template.spec.containers[] |
  select(.name == "keycloak") |
  .image == "quay.io/keycloak/keycloak@sha256:2eb3cd316835c990e69e26ade292ffa78f6fb0db7d5fc6377463c162e1979ac0"
' "${keycloak_render}" >/dev/null
"${KUBECONFORM[@]}" <"${keycloak_render}"

echo "==> Substituting + validating raw infra manifests"
# Skip Helm charts, kustomization files, SOPS ciphertext and templates, Terraform, and governed
# data files with their own validators. Other inline HelmRelease values are rendered by Flux/Helm
# at apply time, not standalone-schema-valid.
manifest_list="$(find infra clusters -type f \( -name '*.yaml' -o -name '*.yml' \) \
	! -name 'kustomization.yaml' \
	! -name '*.sops.yaml' \
	! -path 'infra/agentgateway/providers/model-catalog.yaml' \
	! -path 'infra/federation/registry/*' \
	! -path 'infra/federation/agreements/*' \
	! -path '*/bridges/chart/*' \
	! -path '*/terraform/*' \
	! -path '*/flux-system/*')"
while IFS= read -r manifest; do
	flux envsubst --strict <"${manifest}"
	echo "---"
done <<<"${manifest_list}" | "${KUBECONFORM[@]}"

echo "==> kubeconform OK"
