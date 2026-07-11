#!/usr/bin/env bash
# Offline contract checks for the disposable three-homeserver federation hardening lab.
set -euo pipefail

readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-federation-check.XXXXXX")"
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

assert_yq() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq --exit-status "${expression}" "${document}" >/dev/null || fail "${message}"
}

for command in kubectl rg yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

readonly LIFECYCLE="${ROOT_DIR}/scripts/federation.sh"
readonly SEED="${ROOT_DIR}/scripts/seed-federation.sh"
readonly CLUSTER_OVERLAY="${ROOT_DIR}/clusters/federation"
readonly FEDERATION_ROOT="${ROOT_DIR}/infra/federation"
readonly MATRIX_A_COMPONENT="${FEDERATION_ROOT}/matrix-a/kustomization.yaml"
readonly MATRIX_B_LAYER="${FEDERATION_ROOT}/matrix-b"
readonly MATRIX_C_LAYER="${FEDERATION_ROOT}/matrix-c"
readonly GATEWAY_COMPONENT="${FEDERATION_ROOT}/gateway/kustomization.yaml"
readonly NAMESPACE_COMPONENT="${FEDERATION_ROOT}/namespaces"
readonly POSTGRES_COMPONENT="${FEDERATION_ROOT}/postgres"

[ -x "${LIFECYCLE}" ] || fail 'scripts/federation.sh must exist and be executable'
[ -x "${SEED}" ] || fail 'scripts/seed-federation.sh must exist and be executable'
[ -f "${CLUSTER_OVERLAY}/kustomization.yaml" ] || fail 'clusters/federation is missing'
[ -f "${FEDERATION_ROOT}/cluster/kustomization.yaml" ] || fail 'federation Flux wiring is missing'
[ -f "${MATRIX_A_COMPONENT}" ] || fail 'homeserver A component is missing'
[ -f "${MATRIX_B_LAYER}/kustomization.yaml" ] || fail 'homeserver B layer is missing'
[ -f "${MATRIX_C_LAYER}/kustomization.yaml" ] || fail 'homeserver C layer is missing'
[ -f "${NAMESPACE_COMPONENT}/kustomization.yaml" ] || fail 'federation namespace component is missing'
[ -f "${POSTGRES_COMPONENT}/kustomization.yaml" ] || fail 'federation Postgres component is missing'

bash -n "${LIFECYCLE}" "${SEED}"
"${LIFECYCLE}" --help >"${WORK_DIR}/help.txt"
for contract in \
	'fgentic-fed' \
	'org-a.fgentic.localhost' \
	'org-b.fgentic.localhost' \
	'org-c.fgentic.localhost' \
	'`down` deletes only'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/help.txt" >/dev/null ||
		fail "federation help omits ${contract}"
done
for task in 'fed:up' 'fed:down'; do
	rg --fixed-strings "[tasks.\"${task}\"]" "${ROOT_DIR}/mise.toml" >/dev/null ||
		fail "mise task ${task} is missing"
done

# A malformed teardown target must fail before consulting Docker or k3d. Fake binaries turn a
# future ordering regression into a harmless test failure rather than a real cluster deletion.
mkdir -p "${WORK_DIR}/bin"
for command in docker jq k3d kubectl; do
	printf '#!/usr/bin/env bash\necho "error: offline guard reached %s" >&2\nexit 99\n' \
		"${command}" >"${WORK_DIR}/bin/${command}"
	chmod +x "${WORK_DIR}/bin/${command}"
done
if PATH="${WORK_DIR}/bin:${PATH}" FGENTIC_FED_CLUSTER=fgentic \
	"${LIFECYCLE}" down >"${WORK_DIR}/reserved-cluster.txt" 2>&1; then
	fail 'federation teardown accepted a cluster other than fgentic-fed'
fi
rg --fixed-strings 'must be fgentic-fed' "${WORK_DIR}/reserved-cluster.txt" >/dev/null ||
	fail 'federation teardown did not reject the unsafe cluster name before invoking a command'
if rg --fixed-strings 'offline guard reached' "${WORK_DIR}/reserved-cluster.txt" >/dev/null; then
	fail 'federation teardown consulted the runtime before validating its cluster name'
fi

kubectl kustomize "${CLUSTER_OVERLAY}" >"${WORK_DIR}/cluster.yaml"
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "platform-settings") |
    .data.server_name == "org-a.fgentic.localhost" and
    .data.federation_partner_server_name == "org-b.fgentic.localhost" and
    .data.federation_denied_server_name == "org-c.fgentic.localhost" and
    .data.federation_gateway_ip == "192.0.2.1" and
    .data.cluster_issuer == "local-ca"' \
	"${WORK_DIR}/cluster.yaml" 'federation platform domains are not explicit and distinct'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix-b") |
    .metadata.namespace == "flux-system" and
    .spec.path == "./infra/federation/matrix-b" and
    ([.spec.dependsOn[].name] | contains(["gateway", "postgres"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver B is not an ordered, independently reconcilable Flux layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix-c") |
    .metadata.namespace == "flux-system" and
    .spec.path == "./infra/federation/matrix-c" and
    ([.spec.dependsOn[].name] | contains(["gateway", "postgres"])) and
    ([.spec.postBuild.substituteFrom[].name] |
      contains(["platform-settings", "platform-settings-overrides"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver C is not an ordered, independently reconcilable Flux layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "namespaces") |
    (.spec.components | contains(["../federation/namespaces"]))' \
	"${WORK_DIR}/cluster.yaml" 'federation namespaces are not owned by the early namespace layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "postgres") |
    (.spec.components | contains(["../federation/postgres"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver B database is not composed into the Postgres layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix") |
    (.spec.components | contains(["../federation/matrix-a"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver A is not patched through the Matrix layer'
yq --unwrapScalar '.patches[] | select(.target.kind == "Gateway") | .patch' \
	"${GATEWAY_COMPONENT}" >"${WORK_DIR}/gateway-a-patch.yaml"
assert_yq \
	'length == 1 and .[0].path == "/spec/listeners" and (.[0].value | length) == 3 and
    .[0].value[0].name == "http" and
    .[0].value[0].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[0].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "gateway" and
    .[0].value[1].name == "https-wellknown" and
    .[0].value[1].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[1].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix" and
    .[0].value[2].name == "https-matrix" and
    .[0].value[2].allowedRoutes.namespaces.from == "Selector" and
    .[0].value[2].allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix"' \
	"${WORK_DIR}/gateway-a-patch.yaml" \
	'homeserver A Gateway listeners allow routes from unrelated namespaces'

# The A homeserver is patched only in this disposable overlay. Its outbound trust and domain
# restrictions must mirror B, otherwise one direction of the lab can silently become open.
yq --unwrapScalar '
  .patches[] | select(.target.kind == "HelmRelease" and .target.name == "matrix-stack") | .patch
' "${MATRIX_A_COMPONENT}" >"${WORK_DIR}/homeserver-a-patch.yaml"
yq --unwrapScalar '
  .[] | select(.path == "/spec/values/synapse/additional") |
  .value."10-federation".config
' "${WORK_DIR}/homeserver-a-patch.yaml" >"${WORK_DIR}/homeserver-a-config.yaml"
for contract in \
	'default_room_version: "12"' \
	'federation_domain_whitelist:' \
	'${server_name}' \
	'${federation_partner_server_name}' \
	'federation_custom_ca_list:' \
	'/etc/fgentic-ca/ca.crt' \
	'ip_range_whitelist:' \
	'${federation_gateway_ip}' \
	'trusted_key_servers:' \
	'server_name: ${federation_partner_server_name}' \
	'/32'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-a-config.yaml" >/dev/null ||
		fail "homeserver A federation config omits ${contract}"
done
assert_yq \
	'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 2 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${federation_partner_server_name}"' \
	"${WORK_DIR}/homeserver-a-config.yaml" \
	'homeserver A does not have an exact A/B domain allowlist, room v12 default, and B key notary'
if rg --fixed-strings '${federation_denied_server_name}' \
	"${WORK_DIR}/homeserver-a-config.yaml" >/dev/null; then
	fail 'homeserver A federation allowlist includes denied homeserver C'
fi

for contract in \
	'name: fgentic-local-ca' \
	'mountPath: /etc/fgentic-ca' \
	'readOnly: true' \
	'${federation_denied_server_name}' \
	'matrix.${federation_denied_server_name}' \
	'path: /spec/values/synapse/hostAliases'; do
	rg --fixed-strings "${contract}" "${MATRIX_A_COMPONENT}" >/dev/null ||
		fail "homeserver A runtime trust wiring omits ${contract}"
done

kubectl kustomize "${MATRIX_B_LAYER}" >"${WORK_DIR}/matrix-b.yaml"
kubectl kustomize "${MATRIX_C_LAYER}" >"${WORK_DIR}/matrix-c.yaml"
kubectl kustomize "${NAMESPACE_COMPONENT}" >"${WORK_DIR}/namespaces.yaml"
kubectl kustomize "${POSTGRES_COMPONENT}" >"${WORK_DIR}/postgres.yaml"

assert_yq \
	'select(.kind == "Namespace" and .metadata.name == "matrix-b") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
	"${WORK_DIR}/namespaces.yaml" 'matrix-b is not owned by the namespace layer with PSS labels'
assert_yq \
	'select(.kind == "Namespace" and .metadata.name == "matrix-c") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
	"${WORK_DIR}/namespaces.yaml" 'matrix-c is not owned by the namespace layer with PSS labels'

assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
    .metadata.namespace == "matrix-b" and
    .spec.releaseName == "ess" and
    .spec.values.serverName == "${federation_partner_server_name}" and
    .spec.values.synapse.postgres.host == "platform-pg-rw.postgres.svc.cluster.local" and
    .spec.values.synapse.postgres.user == "synapse_b" and
    .spec.values.synapse.postgres.database == "synapse_b" and
    .spec.values.synapse.postgres.sslMode == "require" and
    .spec.values.synapse.postgres.password.secret == "pg-synapse-b" and
    .spec.values.matrixAuthenticationService.enabled == false and
    .spec.values.elementWeb.enabled == false and
    .spec.values.elementAdmin.enabled == false and
    .spec.values.matrixRTC.enabled == false and
    .spec.values.wellKnownDelegation.enabled == true and
    .spec.values.postgres.enabled == false and
    ([.spec.values.synapse.hostAliases[] |
      select(.ip == "${federation_gateway_ip}") | .hostnames[]] |
      contains(["${server_name}", "matrix.${server_name}",
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}",
        "${federation_denied_server_name}", "matrix.${federation_denied_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B is not a minimal, locally trusted Synapse-only ESS release'

yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
  .spec.values.synapse.additional."10-federation".config
' "${WORK_DIR}/matrix-b.yaml" >"${WORK_DIR}/homeserver-b-config.yaml"
for contract in \
	'default_room_version: "12"' \
	'federation_domain_whitelist:' \
	'${server_name}' \
	'${federation_partner_server_name}' \
	'federation_custom_ca_list:' \
	'/etc/fgentic-ca/ca.crt' \
	'ip_range_whitelist:' \
	'${federation_gateway_ip}' \
	'trusted_key_servers:' \
	'server_name: ${server_name}' \
	'/32'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-b-config.yaml" >/dev/null ||
		fail "homeserver B federation config omits ${contract}"
done
assert_yq \
	'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 2 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${server_name}"' \
	"${WORK_DIR}/homeserver-b-config.yaml" \
	'homeserver B does not have an exact A/B domain allowlist, room v12 default, and A key notary'
if rg --fixed-strings '${federation_denied_server_name}' \
	"${WORK_DIR}/homeserver-b-config.yaml" >/dev/null; then
	fail 'homeserver B federation allowlist includes denied homeserver C'
fi

assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-c") |
    .metadata.namespace == "matrix-c" and
    .spec.releaseName == "ess" and
    .spec.values.serverName == "${federation_denied_server_name}" and
    .spec.values.synapse.postgres.host == "platform-pg-rw.postgres.svc.cluster.local" and
    .spec.values.synapse.postgres.user == "synapse_c" and
    .spec.values.synapse.postgres.database == "synapse_c" and
    .spec.values.synapse.postgres.sslMode == "require" and
    .spec.values.synapse.postgres.password.secret == "pg-synapse-c" and
    .spec.values.matrixAuthenticationService.enabled == false and
    .spec.values.elementWeb.enabled == false and
    .spec.values.elementAdmin.enabled == false and
    .spec.values.matrixRTC.enabled == false and
    .spec.values.wellKnownDelegation.enabled == true and
    .spec.values.postgres.enabled == false and
    ([.spec.values.synapse.hostAliases[] |
      select(.ip == "${federation_gateway_ip}") | .hostnames[]] |
      contains(["${server_name}", "matrix.${server_name}",
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}",
        "${federation_denied_server_name}", "matrix.${federation_denied_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
	"${WORK_DIR}/matrix-c.yaml" \
	'homeserver C is not a minimal, locally trusted Synapse-only ESS release'
yq --unwrapScalar '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-c") |
  .spec.values.synapse.additional."10-federation".config
' "${WORK_DIR}/matrix-c.yaml" >"${WORK_DIR}/homeserver-c-config.yaml"
assert_yq \
	'.default_room_version == "12" and
    (.federation_domain_whitelist | length) == 3 and
    .federation_domain_whitelist[0] == "${server_name}" and
    .federation_domain_whitelist[1] == "${federation_partner_server_name}" and
    .federation_domain_whitelist[2] == "${federation_denied_server_name}" and
    (.trusted_key_servers | length) == 1 and
    .trusted_key_servers[0].server_name == "${server_name}"' \
	"${WORK_DIR}/homeserver-c-config.yaml" \
	'homeserver C does not have room v12, controlled local peers, and A key notary'
if rg --fixed-strings -e '10.0.0.0/8' -e '172.16.0.0/12' -e '192.168.0.0/16' \
	"${WORK_DIR}/homeserver-a-config.yaml" "${WORK_DIR}/homeserver-b-config.yaml" \
	"${WORK_DIR}/homeserver-c-config.yaml" >/dev/null; then
	fail 'federation config trusts a broad private network instead of the ingress /32'
fi

for homeserver in \
	"${WORK_DIR}/homeserver-a-config.yaml" \
	"${WORK_DIR}/homeserver-b-config.yaml" \
	"${WORK_DIR}/homeserver-c-config.yaml"; do
	if rg --regexp "^[[:space:]]*-[[:space:]]*['\"]?\\*" "${homeserver}" >/dev/null; then
		fail "${homeserver##*/} contains a wildcard federation allowlist entry"
	fi
done

assert_yq \
	'select(.kind == "Database" and .spec.name == "synapse_b") |
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.owner == "synapse_b" and
    .spec.encoding == "UTF8" and
    .spec.localeCollate == "C" and
    .spec.localeCType == "C" and
    .spec.template == "template0"' \
	"${WORK_DIR}/postgres.yaml" 'homeserver B does not have a C-locale, role-scoped CNPG database'
assert_yq \
	'select(.kind == "Database" and .spec.name == "synapse_c") |
    .metadata.name == "synapse-c" and
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.owner == "synapse_c" and
    .spec.encoding == "UTF8" and
    .spec.localeCollate == "C" and
    .spec.localeCType == "C" and
    .spec.template == "template0"' \
	"${WORK_DIR}/postgres.yaml" 'homeserver C does not have a C-locale, role-scoped CNPG database'

yq --unwrapScalar '.patches[]?.patch' \
	"${POSTGRES_COMPONENT}/kustomization.yaml" >"${WORK_DIR}/postgres-patches.yaml"
for contract in \
	'name: synapse_b' \
	'name: pg-synapse-b' \
	'name: synapse_c' \
	'name: pg-synapse-c' \
	'hostssl synapse_b synapse_b all scram-sha-256' \
	'hostssl synapse_c synapse_c all scram-sha-256' \
	'hostssl all all all reject' \
	'hostnossl all all all reject'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/postgres-patches.yaml" >/dev/null ||
		fail "homeserver B database boundary omits ${contract}"
done

assert_yq \
	'select(.kind == "Certificate" and .metadata.name == "matrix-b-tls") |
    .metadata.namespace == "gateway" and
    .spec.secretName == "matrix-b-tls" and
    .spec.issuerRef.kind == "ClusterIssuer" and
    .spec.issuerRef.name == "${cluster_issuer}" and
    (.spec.dnsNames | contains(["${federation_partner_server_name}",
      "matrix.${federation_partner_server_name}"]))' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B does not have a local-CA leaf certificate'
assert_yq \
	'select(.kind == "Gateway" and .metadata.name == "federation-b") |
    .metadata.namespace == "gateway" and
    .spec.gatewayClassName == "traefik" and
    ([.spec.listeners[] | select(.protocol == "HTTPS") | .hostname] |
      contains(["${federation_partner_server_name}",
        "matrix.${federation_partner_server_name}"])) and
    ([.spec.listeners[].tls.certificateRefs[] | select(.name == "matrix-b-tls")] |
      length) == 2 and
    ([.spec.listeners[] | select(
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix-b"
    )] | length) == 2' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B has no local-CA TLS Gateway for apex and Synapse'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "synapse-b") |
    .metadata.namespace == "matrix-b" and
    .spec.parentRefs[0].name == "federation-b" and
    (.spec.hostnames | contains(["matrix.${federation_partner_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-synapse" and .port == 8008)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B Synapse route is incomplete'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "well-known-b") |
    .metadata.namespace == "matrix-b" and
    .spec.parentRefs[0].name == "federation-b" and
    (.spec.hostnames | contains(["${federation_partner_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-well-known" and .port == 8010)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B well-known delegation route is incomplete'

assert_yq \
	'select(.kind == "Certificate" and .metadata.name == "matrix-c-tls") |
    .metadata.namespace == "gateway" and
    .spec.secretName == "matrix-c-tls" and
    .spec.issuerRef.kind == "ClusterIssuer" and
    .spec.issuerRef.name == "${cluster_issuer}" and
    (.spec.dnsNames | contains(["${federation_denied_server_name}",
      "matrix.${federation_denied_server_name}"]))' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C does not have a local-CA leaf certificate'
assert_yq \
	'select(.kind == "Gateway" and .metadata.name == "federation-c") |
    .metadata.namespace == "gateway" and
    .spec.gatewayClassName == "traefik" and
    ([.spec.listeners[] | select(.protocol == "HTTPS") | .hostname] |
      contains(["${federation_denied_server_name}",
        "matrix.${federation_denied_server_name}"])) and
    ([.spec.listeners[].tls.certificateRefs[] | select(.name == "matrix-c-tls")] |
      length) == 2 and
    ([.spec.listeners[] | select(
      .allowedRoutes.namespaces.from == "Selector" and
      .allowedRoutes.namespaces.selector.matchLabels."kubernetes.io/metadata.name" == "matrix-c"
    )] | length) == 2' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C has no local-CA TLS Gateway for apex and Synapse'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "synapse-c") |
    .metadata.namespace == "matrix-c" and
    .spec.parentRefs[0].name == "federation-c" and
    (.spec.hostnames | contains(["matrix.${federation_denied_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-synapse" and .port == 8008)] | length) == 1' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C Synapse route is incomplete'
assert_yq \
	'select(.kind == "HTTPRoute" and .metadata.name == "well-known-c") |
    .metadata.namespace == "matrix-c" and
    .spec.parentRefs[0].name == "federation-c" and
    (.spec.hostnames | contains(["${federation_denied_server_name}"])) and
    ([.spec.rules[].backendRefs[] |
      select(.name == "ess-well-known" and .port == 8010)] | length) == 1' \
	"${WORK_DIR}/matrix-c.yaml" 'homeserver C well-known delegation route is incomplete'

# Public CA material is copied into every Matrix namespace at runtime; the repository and cluster
# snapshots must never carry the local signing key.
for contract in \
	'for namespace in matrix matrix-b matrix-c' \
	'create configmap fgentic-local-ca' \
	'ca.crt' \
	'pg-synapse-b' \
	'pg-synapse-c' \
	'charlie-password' \
	'apply_secret postgres pg-synapse-c' \
	'--from-literal=username=synapse_c' \
	'apply_secret matrix-c pg-synapse-c'; do
	rg --fixed-strings -- "${contract}" "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
		fail "federation lifecycle omits ${contract}"
done
if rg --fixed-strings 'ca.key' "${LIFECYCLE}" "${ROOT_DIR}/scripts/demo.sh" \
	"${CLUSTER_OVERLAY}" "${FEDERATION_ROOT}" >/dev/null; then
	fail 'federation assets reference the private local-CA key'
fi

# The up path is the acceptance proof: create an explicitly federated v12 room with a partner-only
# ACL, exchange A/B messages, reject C's join and send, then prove the default v12 local-only room.
for contract in \
	'FGENTIC_DEMO_PROFILE=federation' \
	'room_version' \
	'"12"' \
	'creation_content: {"m.federate": true}' \
	'creation_content: {"m.federate": false}' \
	'm.room.server_acl' \
	'allow_ip_literals: false' \
	'.allow_ip_literals == false and .deny == []' \
	'(.allow | sort) == ([$a, $b] | sort)' \
	'/state/m.room.server_acl' \
	'.room_version == "12" and ."m.federate" == true' \
	'.room_version == "12" and ."m.federate" == false' \
	'/_matrix/client/v3/rooms/' \
	'/invite' \
	'/join' \
	'/send/m.room.message/' \
	'/sync?timeout=1000' \
	'@alice:${SERVER_A}' \
	'@bob:${SERVER_B}' \
	'@charlie:${SERVER_C}' \
	'MATRIX_C_URL' \
	'register_user matrix-c' \
	'create_federated_room' \
	'denied control join' \
	'send_signed_federation_probe' \
	'SYNAPSE_SIGNING_KEY' \
	'from signedjson.key import read_signing_keys' \
	'from signedjson.sign import sign_json' \
	'edu_type: "m.typing"' \
	'signed federation positive control' \
	'denied control federation send to' \
	'whitelist | sort) == ([$a, $b, $c] | sort)' \
	'SYNAPSE_REGISTRATION_SHARED_SECRET' \
	'whitelist_enabled == true' \
	'federation-a-to-b-' \
	'federation-b-to-a-'; do
	rg --fixed-strings "${contract}" "${LIFECYCLE}" "${SEED}" >/dev/null ||
		fail "federation acceptance proof omits ${contract}"
done
if rg --fixed-strings '403 | 404' "${SEED}" >/dev/null; then
	fail 'federation acceptance treats a local missing-room response as a denied federation send'
fi
if rg --fixed-strings '%3N' "${SEED}" >/dev/null; then
	fail 'federation acceptance depends on GNU date nanosecond formatting'
fi
rg --fixed-strings 'LLM_PROVIDER="demo"' "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
	fail 'federation profile can select a paid model provider'

echo 'Federation topology and lifecycle contracts passed.'
