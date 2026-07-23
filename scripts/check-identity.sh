#!/usr/bin/env bash
# Offline contract for the reference IdP ↔ MAS OIDC backchannel-logout wiring (issue #278): the Keycloak
# `fgentic` client declares the backchannel-logout endpoint at the INTERNAL MAS Service with the session
# id required and front-channel logout off; the MAS upstream provider template sets on_backchannel_logout:
# logout_all; and Keycloak has an exact egress path to the MAS pod on :8080 (no broader egress). No live
# cluster: a fresh-import render audit. The live session-termination proof is the runtime-owner step.
# shellcheck disable=SC2016 # jq/yq bindings and JSON attribute keys are intentionally literal
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-identity.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands yq jq

# The provider id and internal MAS backchannel endpoint MAS 1.19 serves (POST /upstream/backchannel-logout/<id>).
readonly PROVIDER_ID="01H8PKNWKKRPCBW4YGH1RWV279"
readonly MAS_BACKCHANNEL_URL="http://ess-matrix-authentication-service.matrix.svc.cluster.local:8080/upstream/backchannel-logout/${PROVIDER_ID}"

# 1. The Keycloak fgentic client: backchannel URL -> internal MAS, session id required, front-channel off.
realm="${ROOT_DIR}/infra/keycloak/realm-config.yaml"
yq -r '.data."fgentic-realm.json"' "${realm}" >"${WORK_DIR}/realm.json"
[ -s "${WORK_DIR}/realm.json" ] || fail "could not extract fgentic-realm.json from ${realm}"
jq -e --arg url "${MAS_BACKCHANNEL_URL}" '
  .clients[] | select(.clientId == "fgentic") |
  (.frontchannelLogout == false) and
  (.attributes."backchannel.logout.url" == $url) and
  (.attributes."backchannel.logout.session.required" == "true") and
  (.attributes."pkce.code.challenge.method" == "S256")
' "${WORK_DIR}/realm.json" >/dev/null \
	|| fail "fgentic client must set the internal MAS backchannel URL, session-required, front-channel off, and keep PKCE"

# 2. The MAS upstream provider template wires backchannel logout to logout_all under the matching provider id.
provider_template="${ROOT_DIR}/infra/secrets/keycloak-bootstrap.sops.yaml.example"
yq -r '.stringData."provider.yaml"' "${provider_template}" >"${WORK_DIR}/provider.yaml"
[ -s "${WORK_DIR}/provider.yaml" ] || fail "could not extract provider.yaml from ${provider_template}"
yq -e '
  .upstream_oauth2.providers[] | select(.id == "01H8PKNWKKRPCBW4YGH1RWV279") | .on_backchannel_logout == "logout_all"
' "${WORK_DIR}/provider.yaml" >/dev/null \
	|| fail "MAS upstream provider ${PROVIDER_ID} must set on_backchannel_logout: logout_all"

# 3. Keycloak has an exact egress rule to the MAS pod on :8080 — and no broader new destination.
netpol="${ROOT_DIR}/infra/keycloak/networkpolicy.yaml"
yq -e '
  select(.kind == "NetworkPolicy" and .metadata.name == "keycloak")
  | .spec.egress
  | any_c(
      (.to | any_c(
        .namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "matrix"
        and .podSelector.matchLabels."app.kubernetes.io/name" == "matrix-authentication-service"))
      and (.ports | any_c(.port == 8080 and .protocol == "TCP"))
    )
' "${netpol}" >/dev/null \
	|| fail "the keycloak NetworkPolicy must allow egress to the MAS pod on TCP 8080"

echo "IdP backchannel-logout contract passed: Keycloak fgentic client -> internal MAS, logout_all, exact egress"
