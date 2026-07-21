#!/usr/bin/env bash
# Idempotent Keycloak Admin-API migration for OIDC backchannel logout (issue #278).
#
# The Keycloak realm import is bootstrap-only: `kc.sh start --import-realm` is skipped once the `fgentic`
# realm exists, so editing realm-config.yaml alone brings only FRESH clusters to the new backchannel-logout
# config. This migration brings an EXISTING cluster's live `fgentic` client to the exact same state —
# front-channel logout off, and the OIDC backchannel-logout endpoint pointed at the internal MAS Service
# with the session id required. It is idempotent: re-running sets the same values (a no-op).
#
# It runs `kcadm.sh` INSIDE the running Keycloak pod, so it needs no exposed admin endpoint and no extra
# NetworkPolicy (the pod reaches its own listener on localhost and already holds the bootstrap admin
# credentials). Requires a live cluster and kubectl access to the keycloak namespace. The post-migration
# session-termination proof (an explicit Keycloak logout terminating the derived MAS sessions) is the
# runtime step; this only reconciles the client config. Fresh bootstrap and a migrated realm end equal.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"

require_commands kubectl

NAMESPACE="${KEYCLOAK_NAMESPACE:-keycloak}"
POD="${KEYCLOAK_POD:-keycloak-0}"
readonly NAMESPACE POD

kubectl -n "${NAMESPACE}" get pod "${POD}" >/dev/null 2>&1 \
	|| fail "Keycloak pod ${POD} not found in namespace ${NAMESPACE} (set KEYCLOAK_POD/KEYCLOAK_NAMESPACE)"

# The inner script runs in the pod with its own env (KC_BOOTSTRAP_ADMIN_*). The quoted heredoc keeps the
# kcadm attribute expressions and the fixed internal MAS URL literal; only the pod's shell expands $VARS.
kubectl -n "${NAMESPACE}" exec -i "${POD}" -- bash -s <<'INNER'
set -euo pipefail
kcadm=/opt/keycloak/bin/kcadm.sh
"${kcadm}" config credentials --server http://localhost:8080 --realm master \
	--user "${KC_BOOTSTRAP_ADMIN_USERNAME}" --password "${KC_BOOTSTRAP_ADMIN_PASSWORD}" >/dev/null
uuid="$("${kcadm}" get clients -r fgentic -q clientId=fgentic --fields id --format csv --noquotes)"
[ -n "${uuid}" ] || { echo "fgentic client not found in realm fgentic" >&2; exit 1; }
"${kcadm}" update "clients/${uuid}" -r fgentic \
	-s frontchannelLogout=false \
	-s 'attributes."backchannel.logout.url"=http://ess-matrix-authentication-service.matrix.svc.cluster.local:8080/upstream/backchannel-logout/01H8PKNWKKRPCBW4YGH1RWV279' \
	-s 'attributes."backchannel.logout.session.required"=true' \
	-s 'attributes."backchannel.logout.revoke.offline.tokens"=false'
echo "fgentic client reconciled: backchannel logout -> internal MAS, front-channel off"
INNER

echo "Keycloak backchannel-logout migration applied to ${POD} (idempotent). Verify a live logout terminates MAS sessions."
