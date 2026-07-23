#!/usr/bin/env bash
# shellcheck disable=SC2312 # substitutions feed fail-closed assertions or mandatory fixture execution
# Prove the opt-in Draupnir moderation component offline: default profiles carry ZERO moderation
# resources, the composed component renders a valid, non-root, network-isolated Draupnir with an
# upstream digest-pinned image and no phantom database, and the opt-in Flux wiring composes exactly
# where the bridge/admin conventions place it. Live policy-list ban propagation across homeservers
# is the runtime federation lab (issue #136 Task 3) and is deliberately NOT asserted here.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
readonly ROOT_DIR
readonly FIXTURE="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
readonly IMAGE="ghcr.io/the-draupnir-project/draupnir:v3.1.0@sha256:d164288b426e55db56d5c7784b6aaafcf7f1613a6fc08c33245e6459406d0ac2"
readonly EXAMPLE="${ROOT_DIR}/infra/secrets/draupnir.sops.yaml.example"

require_commands flux jq kubeconform kubectl yq

WORK_DIR="$(mktemp -d)"
COMPONENT_DIR="${ROOT_DIR}/.agents/tmp/moderation-components-test.$$"
cleanup() { rm -rf "${WORK_DIR}" "${COMPONENT_DIR}"; }
trap cleanup EXIT INT TERM

load_settings() {
	local profile="$1" key settings value
	settings="$(yq -r '.data | to_entries[] | .key + "=" + .value' \
		"${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")" \
		|| fail "could not read clusters/${profile} platform settings"
	while IFS='=' read -r key value; do
		export "${key}=${value}"
	done <<<"${settings}"
}

render_moderation() { # render_moderation <profile>
	(
		load_settings "$1"
		kubectl kustomize "${ROOT_DIR}/infra/moderation" | flux envsubst --strict
	)
}

# ---------------------------------------------------------------------------
# 1. Disabled-and-absent by default. clusters/base and every shipped overlay must reconcile no
#    Draupnir Flux Kustomization and never append the moderation antispam component to matrix.
# ---------------------------------------------------------------------------
base_render="${WORK_DIR}/base.yaml"
kubectl kustomize "${ROOT_DIR}/clusters/base" >"${base_render}"
if yq -e 'select(.kind == "Kustomization" and .metadata.name == "draupnir")' \
	"${base_render}" >/dev/null 2>&1; then
	fail "moderation is enabled in the base overlay"
fi
if grep -q "moderation" "${base_render}"; then
	fail "base overlay references the moderation layer"
fi
yq -e 'select(.kind == "Kustomization" and .metadata.name == "matrix") |
	(.spec.components | length) == 0' "${base_render}" >/dev/null \
	|| fail "base matrix Flux layer must expose an empty opt-in component list"

for profile in local gcp demo federation; do
	effective="$({
		cd "${ROOT_DIR}"
		flux build kustomization cluster-overlay-validation \
			--path "clusters/${profile}" \
			--kustomization-file "${FIXTURE}" \
			--dry-run --in-memory-build --strict-substitute
	})" || fail "could not flux-build clusters/${profile}"
	if yq -e 'select(.kind == "Kustomization" and .metadata.name == "draupnir")' \
		<<<"${effective}" >/dev/null 2>&1; then
		fail "clusters/${profile} reconciles a Draupnir Kustomization by default"
	fi
	matrix_components="$(yq -r 'select(.kind == "Kustomization" and .metadata.name == "matrix") |
		.spec.components // [] | .[]' <<<"${effective}")" || matrix_components=""
	if grep -q "moderation" <<<"${matrix_components}"; then
		fail "clusters/${profile} composes the moderation antispam component by default"
	fi
done

# The shared Postgres layer must not grow a Draupnir role/database (bot mode is SQLite-only).
if grep -rInE 'draupnir' "${ROOT_DIR}/infra/postgres" >/dev/null 2>&1; then
	fail "Draupnir must not add a CNPG role/database (bot mode has no PostgreSQL dependency)"
fi

# ---------------------------------------------------------------------------
# 2. The composed component renders a valid, hardened Draupnir workload.
# ---------------------------------------------------------------------------
render="${WORK_DIR}/moderation.yaml"
render_moderation local >"${render}"
kubeconform -strict -ignore-missing-schemas -summary "${render}" >/dev/null \
	|| fail "moderation render failed schema validation"
render_json="$(yq eval-all -o=json '[.]' "${render}")"

jq -e '[.[] | select(.kind == "Namespace" and .metadata.name == "moderation")] | length == 1' \
	<<<"${render_json}" >/dev/null || fail "moderation must own exactly one Namespace"
jq -e '.[] | select(.kind == "Namespace" and .metadata.name == "moderation") |
	.metadata.labels."fgentic.dev/managed" == "true" and
	.metadata.labels."fgentic.dev/image-policy" == "enforce" and
	.metadata.labels."fgentic.dev/quota-profile" == "small" and
	.metadata.labels."pod-security.kubernetes.io/enforce" == "restricted"' \
	<<<"${render_json}" >/dev/null || fail "moderation Namespace security labels drifted"

jq -e '.[] | select(.kind == "Deployment" and .metadata.name == "draupnir") |
	.spec.replicas == 1 and
	.spec.strategy.type == "Recreate" and
	.spec.template.spec.automountServiceAccountToken == false and
	.spec.template.spec.securityContext.runAsNonRoot == true and
	.spec.template.spec.securityContext.runAsUser == 1000 and
	.spec.template.spec.securityContext.runAsGroup == 1000 and
	.spec.template.spec.securityContext.fsGroup == 1000 and
	.spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
	.spec.template.spec.containers[0].image == "'"${IMAGE}"'" and
	.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false and
	.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
	(.spec.template.spec.containers[0].securityContext.capabilities.drop | index("ALL")) != null and
	(.spec.template.spec.containers[0].args | index("--access-token-path")) != null and
	.spec.template.spec.containers[0].resources.limits.memory != null' \
	<<<"${render_json}" >/dev/null || fail "Draupnir Deployment security or image contract drifted"

# Upstream registry + digest pin, never a :latest tag or a mirrored project registry.
image_ref="$(jq -r '.[] | select(.kind == "Deployment" and .metadata.name == "draupnir") |
	.spec.template.spec.containers[0].image' <<<"${render_json}")"
[[ "${image_ref}" == ghcr.io/the-draupnir-project/draupnir@* ||
	"${image_ref}" == ghcr.io/the-draupnir-project/draupnir:*@sha256:* ]] \
	|| fail "Draupnir image must reference the upstream project registry by digest"
[[ "${image_ref}" == *"@sha256:"* ]] || fail "Draupnir image is not digest-pinned"
[[ "${image_ref}" != *":latest"* ]] || fail "Draupnir image uses a forbidden :latest tag"

jq -e '.[] | select(.kind == "Service" and .metadata.name == "draupnir") |
	.spec.type == "ClusterIP"' <<<"${render_json}" >/dev/null \
	|| fail "Draupnir Service must be ClusterIP"

jq -e '.[] | select(.kind == "NetworkPolicy" and .metadata.name == "draupnir") |
	(.spec.policyTypes | sort) == ["Egress", "Ingress"] and
	([.spec.egress[].to[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] | sort |
		join(",")) == "kube-system,matrix" and
	([.spec.egress[].ports[].port] | sort | join(",")) == "53,53,8008" and
	.spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "matrix" and
	.spec.ingress[0].ports[0].port == 8082' \
	<<<"${render_json}" >/dev/null || fail "Draupnir NetworkPolicy lost its Synapse+DNS-only boundary"
# DNS egress is narrowed to CoreDNS pods, matching every other repo NetworkPolicy — the moderation
# pod must not reach an arbitrary kube-system workload on 53.
jq -e '.[] | select(.kind == "NetworkPolicy" and .metadata.name == "draupnir") |
	[.spec.egress[].to[] | select(.namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kube-system") |
		.podSelector.matchLabels."k8s-app"] == ["kube-dns"]' \
	<<<"${render_json}" >/dev/null || fail "Draupnir DNS egress must be narrowed to CoreDNS (k8s-app: kube-dns)"
# No database egress path exists, because there is no database.
if jq -e '.[] | select(.kind == "NetworkPolicy" and .metadata.name == "draupnir") |
	.spec.egress[].ports[] | select(.port == 5432)' <<<"${render_json}" >/dev/null 2>&1; then
	fail "Draupnir NetworkPolicy grants database egress it must never need"
fi

config="$(jq -r '.[] | select(.kind == "ConfigMap" and .metadata.name == "draupnir-config") |
	.data."production.yaml"' <<<"${render_json}")"
[[ -n "${config}" ]] || fail "Draupnir config ConfigMap is missing"
yq -e '.managementRoom == "#fgentic-moderation:fgentic.localhost" and
	.disableServerACL == false and
	.protectAllJoinedRooms == false and
	.admin.enableMakeRoomAdminCommand == false and
	(has("accessToken") | not) and
	.web.enabled == false and
	.web.synapseHTTPAntispam.enabled == false' \
	<<<"${config}" >/dev/null || fail "Draupnir config lost its fail-closed defaults"

for kind in ResourceQuota LimitRange; do
	count="$(jq --arg kind "${kind}" \
		'[.[] | select(.kind == $kind and .metadata.namespace == "moderation")] | length' \
		<<<"${render_json}")"
	[[ "${count}" -eq 1 ]] || fail "moderation must own exactly one ${kind}"
done

# ---------------------------------------------------------------------------
# 3. Opt-in Flux composition matches the bridge/admin conventions.
# ---------------------------------------------------------------------------
mkdir -p "${COMPONENT_DIR}"
printf '%s\n' \
	'apiVersion: kustomize.config.k8s.io/v1beta1' \
	'kind: Kustomization' \
	'resources:' \
	'  - ../../../clusters/base' \
	'components:' \
	'  - ../../../infra/moderation/cluster' >"${COMPONENT_DIR}/kustomization.yaml"
component_render="${WORK_DIR}/components.yaml"
kubectl kustomize "${COMPONENT_DIR}" >"${component_render}"
yq -e 'select(.kind == "Kustomization" and .metadata.name == "draupnir") |
	.spec.path == "./infra/moderation" and
	(([.spec.dependsOn[].name] | sort | join(",")) == "matrix,platform-secrets")' \
	"${component_render}" >/dev/null || fail "moderation cluster component did not compose the Draupnir Flux Kustomization"

# Antispam is an independent opt-in that appends a Synapse module fragment to the matrix layer.
matrix_fixture="${WORK_DIR}/matrix-flux.yaml"
printf '%s\n' \
	'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
	'kind: Kustomization' \
	'metadata: {name: matrix-test, namespace: flux-system}' \
	'spec:' \
	'  interval: 30m' \
	'  path: ./infra/matrix' \
	'  components:' \
	'    - ../moderation/antispam' \
	'  sourceRef: {kind: GitRepository, name: flux-system}' >"${matrix_fixture}"
matrix_render="${WORK_DIR}/matrix.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization matrix-test --path infra/matrix \
		--kustomization-file "${matrix_fixture}" --dry-run --in-memory-build
) >"${matrix_render}"
yq -e 'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack") |
	.spec.values.synapse.additional."30-http-antispam".configSecret == "draupnir-antispam-module" and
	.spec.values.synapse.additional."30-http-antispam".configSecretKey == "module.yaml"' \
	"${matrix_render}" >/dev/null || fail "antispam component did not wire the Synapse module fragment"

# ---------------------------------------------------------------------------
# 4. Secret template shape: one real credential (the bot access token), no database Secret.
# ---------------------------------------------------------------------------
[[ -f "${EXAMPLE}" ]] || fail "moderation SOPS example template is missing"
example_json="$(yq eval-all -o=json '[.]' "${EXAMPLE}")"
jq -e '[.[] | select(.kind == "Secret" and .metadata.namespace == "moderation" and
	.metadata.name == "draupnir")] | length == 1 and
	(.[] | select(.metadata.name == "draupnir") | .stringData | has("access-token"))' \
	<<<"${example_json}" >/dev/null || fail "Draupnir access-token Secret template drifted"
jq -e '.[] | select(.kind == "Secret" and .metadata.namespace == "matrix" and
	.metadata.name == "draupnir-antispam-module") |
	(.stringData."module.yaml" | test("synapse_http_antispam.HTTPAntispam"))' \
	<<<"${example_json}" >/dev/null || fail "antispam module Secret template drifted"
if grep -qE 'pg-draupnir|draupnirbridge' "${EXAMPLE}"; then
	fail "Draupnir has no database — the Secret template must not carry a CNPG credential"
fi

echo "Draupnir moderation component is opt-in, hardened, upstream-pinned, and DB-free offline; live ban propagation remains the #136 federation lab."
