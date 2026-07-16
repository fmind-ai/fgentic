#!/usr/bin/env bash
# Prove the deployment boundary and exact effective Synapse retention values without a cluster.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly FIXTURE="${ROOT_DIR}/scripts/testdata/retention-policy"

fail() {
  echo "retention policy check failed: $*" >&2
  exit 1
}

matrix_components() {
  kubectl kustomize "${ROOT_DIR}/clusters/$1" \
    | yq -r 'select(.kind == "Kustomization" and .metadata.name == "matrix") | .spec.components[]?'
}

assert_component() {
  local profile="$1" expected="$2" components
  components="$(matrix_components "${profile}")"
  if [ "${expected}" = present ]; then
    grep -Fxq retention <<< "${components}" \
      || fail "clusters/${profile} does not compose the Matrix retention component"
  elif grep -Fxq retention <<< "${components}"; then
    fail "clusters/${profile} unexpectedly composes the Matrix retention component"
  fi
}

render_profile() (
  local profile="$1" key value settings
  settings="$(yq -er '.data | to_entries[] | .key + "=" + .value' \
    "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  while IFS='=' read -r key value; do
    export "${key}=${value}"
  done <<< "${settings}"

  kustomize build --load-restrictor LoadRestrictionsNone "${FIXTURE}" \
    | flux envsubst --strict
)

assert_effective_values() {
  local profile="$1" rendered config default minimum maximum purge
  local local_media remote_media redaction forgotten
  rendered="$(render_profile "${profile}")"
  config="$(yq -er '
    select(.kind == "HelmRelease" and .metadata.name == "matrix-stack") |
    .spec.values.synapse.additional."10-retention".config
  ' <<< "${rendered}")"

  default="$(yq -er '.data.matrix_message_retention_default' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  minimum="$(yq -er '.data.matrix_message_retention_min' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  maximum="$(yq -er '.data.matrix_message_retention_max' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  purge="$(yq -er '.data.matrix_message_purge_interval' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  local_media="$(yq -er '.data.matrix_local_media_retention' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  remote_media="$(yq -er '.data.matrix_remote_media_retention' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  redaction="$(yq -er '.data.matrix_redaction_retention' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"
  forgotten="$(yq -er '.data.matrix_forgotten_room_retention' "${ROOT_DIR}/clusters/${profile}/platform-settings.yaml")"

  yq -o=json <<< "${config}" \
    | jq -e \
      --arg default "${default}" \
      --arg minimum "${minimum}" \
      --arg maximum "${maximum}" \
      --arg purge "${purge}" \
      --arg local_media "${local_media}" \
      --arg remote_media "${remote_media}" \
      --arg redaction "${redaction}" \
      --arg forgotten "${forgotten}" '
      .retention.enabled == true and
      .retention.default_policy == {"max_lifetime": $default} and
      .retention.allowed_lifetime_min == $minimum and
      .retention.allowed_lifetime_max == $maximum and
      (.retention.purge_jobs | length) == 1 and
      .retention.purge_jobs == [{"interval": $purge}] and
      .media_retention.local_media_lifetime == $local_media and
      .media_retention.remote_media_lifetime == $remote_media and
      .redaction_retention_period == $redaction and
      .forgotten_room_retention_period == $forgotten
    ' >/dev/null \
    || fail "clusters/${profile} effective retention values diverge from platform-settings"
}

assert_component local present
assert_component gcp present
assert_component demo absent
assert_component federation absent

# The base release—and therefore profiles that do not opt in—must remain policy-free.
base_retention="$(kubectl kustomize "${ROOT_DIR}/infra/matrix" \
  | yq -r 'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack") |
    .spec.values.synapse.additional."10-retention" // "absent"')"
[ "${base_retention}" = absent ] || fail "base Matrix release unexpectedly enables retention"

assert_effective_values local
assert_effective_values gcp

echo "retention policy contract OK (local/GCP finite; demo/federation unchanged)"
