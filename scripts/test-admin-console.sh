#!/usr/bin/env bash
# Prove the optional Ketesa surface and its zero-footprint disabled profile without touching a
# cluster. Live MAS login and admin/non-admin behavior remain target-cluster acceptance evidence.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly FIXTURE="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
readonly ADMIN_DIR="${ROOT_DIR}/infra/admin/profiles"
readonly GATEWAY_DIR="${ROOT_DIR}/infra/gateway/profiles"
readonly ENABLED_OVERLAY="${ROOT_DIR}/scripts/testdata/admin-enabled-overlay"

fail() {
  echo "error: $*" >&2
  exit 1
}

for command in flux jq kubeconform kubectl yq; do
  command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

load_settings() {
  local profile="$1" key settings value
  settings="$(yq -r '.data | to_entries[] | .key + "=" + .value' \
    "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")" ||
    fail "could not read clusters/${profile} platform settings"
  while IFS='=' read -r key value; do
    export "${key}=${value}"
  done <<<"${settings}"
}

render_profile() {
  local profile="$1" path="$2"
  (
    load_settings "${profile}"
    kubectl kustomize "${path}" | flux envsubst --strict
  )
}

for profile in local gcp demo federation; do
  admin_path=""
  gateway_path=""
  setting="$(yq -er '.data.admin_console' \
    "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  [[ "${setting}" == disabled ]] ||
    fail "tracked clusters/${profile} must keep the opt-in admin console disabled"

  effective="$({
    cd "${ROOT_DIR}"
    flux build kustomization cluster-overlay-validation \
      --path "clusters/${profile}" \
      --kustomization-file "${FIXTURE}" \
      --dry-run \
      --in-memory-build \
      --strict-substitute
  })"
  admin_path="$(yq -r 'select((.kind == "Kustomization") and (.metadata.name == "admin")) | .spec.path' \
    <<<"${effective}")" || fail "could not read clusters/${profile} admin path"
  gateway_path="$(yq -r 'select((.kind == "Kustomization") and (.metadata.name == "gateway")) | .spec.path' \
    <<<"${effective}")" || fail "could not read clusters/${profile} gateway path"
  [[ "${admin_path}" == "./infra/admin/profiles/${setting}" ]] ||
    fail "clusters/${profile} did not select its admin profile"
  [[ "${gateway_path}" == "./infra/gateway/profiles/${setting}" ]] ||
    fail "clusters/${profile} did not select its matching gateway profile"

  selected_admin="$(render_profile "${profile}" "${ADMIN_DIR}/${setting}")"
  selected_gateway="$(render_profile "${profile}" "${GATEWAY_DIR}/${setting}")"
  if [[ "${setting}" == disabled ]]; then
    [[ -z "${selected_admin//[[:space:]]/}" ]] ||
      fail "clusters/${profile} disabled admin profile is not empty"
    ! yq -e 'select((.kind == "Gateway") and (.metadata.name == "fgentic-gateway")) |
      .spec.listeners[] | select(.name == "https-admin")' <<<"${selected_gateway}" >/dev/null 2>&1 ||
      fail "clusters/${profile} disabled gateway still exposes https-admin"
  fi
done

enabled_selection="$(kubectl kustomize "${ENABLED_OVERLAY}")"
enabled_admin_path="$(yq -r 'select((.kind == "Kustomization") and (.metadata.name == "admin")) | .spec.path' \
  <<<"${enabled_selection}")" || fail "could not read enabled admin path"
enabled_gateway_path="$(yq -r 'select((.kind == "Kustomization") and (.metadata.name == "gateway")) | .spec.path' \
  <<<"${enabled_selection}")" || fail "could not read enabled gateway path"
[[ "${enabled_admin_path}" == "./infra/admin/profiles/enabled" ]] ||
  fail "admin_console=enabled did not select the enabled admin profile"
[[ "${enabled_gateway_path}" == "./infra/gateway/profiles/enabled" ]] ||
  fail "admin_console=enabled did not select the enabled gateway profile"

admin_render="$(render_profile local "${ADMIN_DIR}/enabled")"
gateway_enabled="$(render_profile local "${GATEWAY_DIR}/enabled")"
gateway_disabled="$(render_profile local "${GATEWAY_DIR}/disabled")"
admin_json="$(yq eval-all -o=json '[.]' <<<"${admin_render}")"
gateway_enabled_json="$(yq eval-all -o=json '[.]' <<<"${gateway_enabled}")"
gateway_disabled_json="$(yq eval-all -o=json '[.]' <<<"${gateway_disabled}")"

namespace_count="$(jq '[.[] | select(.kind == "Namespace" and .metadata.name == "admin")] | length' \
  <<<"${admin_json}")" || fail "could not count admin Namespaces"
[[ "${namespace_count}" -eq 1 ]] || fail "enabled profile must own one admin Namespace"
jq -e '.[] | select(.kind == "Namespace" and .metadata.name == "admin") |
  .metadata.labels."fgentic.dev/managed" == "true" and
  .metadata.labels."fgentic.dev/image-policy" == "enforce" and
  .metadata.labels."fgentic.dev/quota-profile" == "small" and
  .metadata.labels."pod-security.kubernetes.io/enforce" == "restricted"' \
  <<<"${admin_json}" >/dev/null || fail "admin Namespace security labels drifted"

config_name="$(jq -r '.[] | select(.kind == "ConfigMap" and .metadata.namespace == "admin" and
  (.metadata.name | test("^ketesa-config-"))) | .metadata.name' <<<"${admin_json}")"
[[ -n "${config_name}" ]] || fail "enabled profile must generate a hash-named Ketesa ConfigMap"
config_json="$(jq -r '.[] | select(.kind == "ConfigMap" and .metadata.namespace == "admin" and
  (.metadata.name | test("^ketesa-config-"))) | .data."config.json"' <<<"${admin_json}")"
jq -e '
  .restrictBaseUrl == "https://fgentic.localhost" and
  .externalAuthProvider == true and
  .wellKnownDiscovery == true and
  .corsCredentials == "omit" and
  .asManagedUsers == ["^@a2a-bridge:.*$", "^@agent-[^:]+:.*$"]
' <<<"${config_json}" >/dev/null || fail "Ketesa homeserver or MAS configuration drifted"

jq -e '.[] | select(.kind == "Deployment" and .metadata.name == "ketesa") |
  .spec.replicas == 1 and
  .spec.template.spec.automountServiceAccountToken == false and
  .spec.template.spec.securityContext.runAsNonRoot == true and
  .spec.template.spec.securityContext.runAsUser == 1000 and
  .spec.template.spec.securityContext.runAsGroup == 1000 and
  .spec.template.spec.securityContext.seccompProfile.type == "RuntimeDefault" and
  .spec.template.spec.containers[0].image ==
    "ghcr.io/etkecc/ketesa:v1.3.0@sha256:609ad2e5b68e7250344929ea2c54a894a5a6be26d6b97b5578e30a935abf46e9" and
  .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false and
  .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
  .spec.template.spec.containers[0].securityContext.capabilities.drop == ["ALL"] and
  .spec.template.spec.containers[0].readinessProbe.httpGet.path == "/health" and
  .spec.template.spec.containers[0].livenessProbe.httpGet.path == "/health" and
  .spec.template.spec.containers[0].resources.requests.cpu != null and
  .spec.template.spec.containers[0].resources.requests.memory != null and
  .spec.template.spec.containers[0].resources.limits.cpu != null and
  .spec.template.spec.containers[0].resources.limits.memory != null' \
  <<<"${admin_json}" >/dev/null || fail "Ketesa Deployment security or image contract drifted"
deployment_config_name="$(jq -r '.[] | select(.kind == "Deployment" and .metadata.name == "ketesa") |
  .spec.template.spec.volumes[] | select(.name == "config") | .configMap.name' \
  <<<"${admin_json}")" || fail "could not read the Deployment config reference"
[[ "${deployment_config_name}" == "${config_name}" ]] ||
  fail "Deployment does not consume the generated config"

jq -e '.[] | select(.kind == "Service" and .metadata.name == "ketesa") |
  .spec.type == "ClusterIP" and .spec.ports ==
    [{"name":"http","port":8080,"protocol":"TCP","targetPort":"http"}]' \
  <<<"${admin_json}" >/dev/null || fail "Ketesa Service exposure drifted"
jq -e '.[] | select(.kind == "HTTPRoute" and .metadata.name == "ketesa") |
  .spec.parentRefs == [{"name":"fgentic-gateway","namespace":"gateway","sectionName":"https-admin"}] and
  .spec.hostnames == ["admin.fgentic.localhost"] and
  .spec.rules[0].backendRefs == [{"name":"ketesa","port":8080}]' \
  <<<"${admin_json}" >/dev/null || fail "Ketesa HTTPRoute drifted"
jq -e '.[] | select(.kind == "NetworkPolicy" and .metadata.name == "ketesa") |
  .spec.policyTypes == ["Ingress", "Egress"] and .spec.egress == [] and
  .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "gateway" and
  .spec.ingress[0].ports == [{"port":8080,"protocol":"TCP"}]' \
  <<<"${admin_json}" >/dev/null || fail "Ketesa NetworkPolicy is not gateway-only with denied egress"

for kind in ResourceQuota LimitRange; do
  kind_count="$(jq --arg kind "${kind}" \
    '[.[] | select(.kind == $kind and .metadata.namespace == "admin")] | length' \
    <<<"${admin_json}")" || fail "could not count admin ${kind} objects"
  [[ "${kind_count}" -eq 1 ]] || fail "enabled profile must own one admin ${kind}"
done

admin_listener_count="$(jq '[.[] | select(.kind == "Gateway" and .metadata.name == "fgentic-gateway") |
  .spec.listeners[] | select(.name == "https-admin")] | length' \
  <<<"${gateway_enabled_json}")" || fail "could not count enabled admin listeners"
[[ "${admin_listener_count}" -eq 1 ]] ||
  fail "enabled gateway must expose exactly one https-admin listener"
jq -e '.[] | select(.kind == "Gateway" and .metadata.name == "fgentic-gateway") |
  .spec.listeners[] | select(.name == "https-admin") |
  .hostname == "admin.fgentic.localhost" and .protocol == "HTTPS" and .port == 443 and
  .tls.mode == "Terminate" and .tls.certificateRefs == [{"name":"matrix-tls"}] and
  .allowedRoutes.namespaces.from == "Selector" and
  .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "admin"' \
  <<<"${gateway_enabled_json}" >/dev/null ||
  fail "enabled https-admin listener drifted"
! jq -e '.[] | select(.kind == "Gateway" and .metadata.name == "fgentic-gateway") |
  .spec.listeners[] | select(.name == "https-admin")' <<<"${gateway_disabled_json}" >/dev/null ||
  fail "disabled gateway profile still contains https-admin"

printf '%s\n---\n%s\n' "${admin_render}" "${gateway_enabled}" |
  kubeconform -strict -ignore-missing-schemas -summary

echo "Ketesa admin-console static contract passed; live MAS authorization remains a cluster gate"
