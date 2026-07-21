#!/usr/bin/env bash
# Offline contract for the opt-in synthetic delegation canary (issue #454): the enabled profile
# renders the exact inventory, the disabled profile renders zero, every cluster's flag resolves the
# canary Kustomization path to its profile, and the probe's reply/timeout logic holds against a fake
# Matrix homeserver. No live cluster: the break-the-loop → alert-fires proof is the runtime step.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-canary.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands flux jq kubectl python3 yq

# 1. The disabled profile prunes to zero objects.
kubectl kustomize "${ROOT_DIR}/infra/canary/profiles/disabled" >"${WORK_DIR}/disabled.yaml"
[ ! -s "${WORK_DIR}/disabled.yaml" ] || fail "canary disabled profile must render zero objects"

# 2. The enabled profile renders exactly the expected inventory.
kubectl kustomize "${ROOT_DIR}/infra/canary/profiles/enabled" \
	| yq ea -o=json '[.]' >"${WORK_DIR}/enabled-objects.json"
jq -e '[.[] | .kind + "/" + .metadata.name] | sort | . == [
  "ConfigMap/canary-config",
  "ConfigMap/canary-probe",
  "CronJob/delegation-canary",
  "LimitRange/container-defaults",
  "Namespace/canary",
  "NetworkPolicy/canary-default-deny",
  "NetworkPolicy/canary-probe",
  "ResourceQuota/compute-budget",
  "ServiceAccount/delegation-canary"
]' "${WORK_DIR}/enabled-objects.json" >/dev/null \
	|| fail "canary enabled inventory drifted"

# 3. The CronJob is fail-closed and content-free by construction.
kubectl kustomize "${ROOT_DIR}/infra/canary/profiles/enabled" \
	| yq 'select(.kind == "CronJob")' >"${WORK_DIR}/cronjob.yaml"
yq -e '
  .spec.jobTemplate.spec.backoffLimit == 0
  and .spec.concurrencyPolicy == "Forbid"
  and .spec.jobTemplate.spec.template.spec.automountServiceAccountToken == false
  and .spec.jobTemplate.spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true
  and .spec.jobTemplate.spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false
  and (.spec.jobTemplate.spec.template.spec.containers[0].image | test("^python:3.14-slim@sha256:[0-9a-f]{64}$"))
' "${WORK_DIR}/cronjob.yaml" >/dev/null || fail "canary CronJob lost a fail-closed/pinned guarantee"

# 4. Every cluster flag is valid and resolves the canary Kustomization path to its profile; demo and
#    federation must stay disabled (zero footprint).
fixture="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
for environment in local gcp demo; do
	settings="${ROOT_DIR}/clusters/${environment}/platform-settings.yaml"
	flag="$(yq -r '.data.synthetic_canary' "${settings}")"
	case "${flag}" in
		enabled | disabled) : ;;
		*) fail "clusters/${environment} synthetic_canary must be enabled|disabled, got '${flag}'" ;;
	esac
	if [ "${environment}" != local ] && [ "${environment}" != gcp ]; then
		[ "${flag}" = disabled ] || fail "clusters/${environment} must keep synthetic_canary disabled"
	fi

	render="${WORK_DIR}/${environment}.yaml"
	flux build kustomization cluster-overlay-validation \
		--path "clusters/${environment}" --kustomization-file "${fixture}" \
		--dry-run --in-memory-build --strict-substitute >"${render}"
	path="$(yq -r 'select(.kind == "Kustomization" and .metadata.name == "canary") | .spec.path' "${render}")"
	[ "${path}" = "./infra/canary/profiles/${flag}" ] \
		|| fail "clusters/${environment}: canary path '${path}' does not match flag '${flag}'"
done

# 5. The probe's reply-detection and timeout behaviour holds against a fake Matrix homeserver.
python3 "${ROOT_DIR}/infra/canary/base/probe_test.py" >/dev/null \
	|| fail "canary probe unit tests failed"

echo "Synthetic delegation canary contract passed"
