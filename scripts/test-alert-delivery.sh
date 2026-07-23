#!/usr/bin/env bash
# Offline contract for the opt-in sovereign alert delivery (issue #456): the enabled profile renders
# the exact inventory, the disabled profile renders zero, every cluster's flag resolves the
# alert-delivery Kustomization path to its profile, the receiver is hardened/pinned, the @alertbot
# sender is in no agent's allowedSenders (D7/D8), and the receiver's content-free rendering holds
# against a fake Matrix homeserver. No live cluster: firing an alert -> room notice is the runtime step.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-alert-delivery.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands flux jq kubectl python3 yq

# 1. The disabled profile prunes to zero objects.
kubectl kustomize "${ROOT_DIR}/infra/alert-delivery/profiles/disabled" >"${WORK_DIR}/disabled.yaml"
[ ! -s "${WORK_DIR}/disabled.yaml" ] || fail "alert-delivery disabled profile must render zero objects"

# 2. The enabled profile renders exactly the expected inventory.
kubectl kustomize "${ROOT_DIR}/infra/alert-delivery/profiles/enabled" \
	| yq ea -o=json '[.]' >"${WORK_DIR}/enabled.json"
jq -e '[.[] | .kind + "/" + .metadata.name] | sort | . == [
  "AlertmanagerConfig/matrix-ops",
  "ConfigMap/alert-receiver-code",
  "ConfigMap/alert-receiver-config",
  "Deployment/alert-receiver",
  "LimitRange/container-defaults",
  "Namespace/alert-delivery",
  "NetworkPolicy/alert-delivery-default-deny",
  "NetworkPolicy/alert-receiver",
  "ResourceQuota/compute-budget",
  "Service/alert-receiver",
  "ServiceAccount/alert-receiver"
]' "${WORK_DIR}/enabled.json" >/dev/null || fail "alert-delivery enabled inventory drifted"

# 3. The receiver Deployment is hardened and digest-pinned; the route has a catch-all + resolves.
kubectl kustomize "${ROOT_DIR}/infra/alert-delivery/profiles/enabled" >"${WORK_DIR}/enabled.yaml"
yq -e '
  select(.kind == "Deployment")
  | .spec.template.spec.automountServiceAccountToken == false
  and .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true
  and .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false
  and (.spec.template.spec.containers[0].image | test("^python:3.14-slim@sha256:[0-9a-f]{64}$"))
' "${WORK_DIR}/enabled.yaml" >/dev/null || fail "alert-receiver Deployment lost a hardened/pinned guarantee"
yq -e '
  select(.kind == "AlertmanagerConfig")
  | .metadata.labels."fgentic.dev/alertmanager-config" == "true"
  and .spec.route.receiver == "matrix-ops"
  and .spec.receivers[0].webhookConfigs[0].url == "http://alert-receiver.alert-delivery.svc.cluster.local:9095/"
' "${WORK_DIR}/enabled.yaml" >/dev/null || fail "alert-delivery route/receiver contract drifted"

# 4. The @alertbot sender can invoke no agent (D7/D8): absent from every allowedSenders and outside
#    the bridge's exclusive @a2a-bridge / @agent-.* namespace.
alertbot_mxid="@alertbot:fgentic.fmind.ai"
agents="${ROOT_DIR}/apps/matrix-a2a-bridge/agents.example.yaml"
if yq -e "[.agents[].allowedSenders[]?] | any(. == \"${alertbot_mxid}\")" "${agents}" 2>/dev/null | grep -qx true; then
	fail "@alertbot must not appear in any agent allowedSenders"
fi
if [[ "${alertbot_mxid}" == @a2a-bridge:* || "${alertbot_mxid}" == @agent-* ]]; then
	fail "@alertbot must be outside the bridge exclusive namespace"
fi

# 5. Every cluster flag is valid and resolves the alert-delivery Kustomization path to its profile;
#    demo and federation must stay disabled (zero footprint).
fixture="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
for environment in local gcp demo; do
	settings="${ROOT_DIR}/clusters/${environment}/platform-settings.yaml"
	flag="$(yq -r '.data.alert_delivery' "${settings}")"
	case "${flag}" in
		enabled | disabled) : ;;
		*) fail "clusters/${environment} alert_delivery must be enabled|disabled, got '${flag}'" ;;
	esac
	[ "${environment}" = local ] || [ "${environment}" = gcp ] || [ "${flag}" = disabled ] \
		|| fail "clusters/${environment} must keep alert_delivery disabled"
	render="${WORK_DIR}/${environment}.yaml"
	flux build kustomization cluster-overlay-validation \
		--path "clusters/${environment}" --kustomization-file "${fixture}" \
		--dry-run --in-memory-build --strict-substitute >"${render}"
	path="$(yq -r 'select(.kind == "Kustomization" and .metadata.name == "alert-delivery") | .spec.path' "${render}")"
	[ "${path}" = "./infra/alert-delivery/profiles/${flag}" ] \
		|| fail "clusters/${environment}: alert-delivery path '${path}' does not match flag '${flag}'"
done

# 6. The receiver's content-free rendering and delivery hold against a fake Matrix homeserver.
python3 "${ROOT_DIR}/infra/alert-delivery/base/receiver_test.py" >/dev/null \
	|| fail "alert receiver unit tests failed"

echo "Sovereign alert delivery contract passed"
