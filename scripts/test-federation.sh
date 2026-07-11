#!/usr/bin/env bash
# Offline contract checks for the disposable two-homeserver federation lab.
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
readonly NAMESPACE_COMPONENT="${FEDERATION_ROOT}/namespaces"
readonly POSTGRES_COMPONENT="${FEDERATION_ROOT}/postgres"

[ -x "${LIFECYCLE}" ] || fail 'scripts/federation.sh must exist and be executable'
[ -x "${SEED}" ] || fail 'scripts/seed-federation.sh must exist and be executable'
[ -f "${CLUSTER_OVERLAY}/kustomization.yaml" ] || fail 'clusters/federation is missing'
[ -f "${FEDERATION_ROOT}/cluster/kustomization.yaml" ] || fail 'federation Flux wiring is missing'
[ -f "${MATRIX_A_COMPONENT}" ] || fail 'homeserver A component is missing'
[ -f "${MATRIX_B_LAYER}/kustomization.yaml" ] || fail 'homeserver B layer is missing'
[ -f "${NAMESPACE_COMPONENT}/kustomization.yaml" ] || fail 'federation namespace component is missing'
[ -f "${POSTGRES_COMPONENT}/kustomization.yaml" ] || fail 'federation Postgres component is missing'

bash -n "${LIFECYCLE}" "${SEED}"
"${LIFECYCLE}" --help >"${WORK_DIR}/help.txt"
for contract in \
	'fgentic-fed' \
	'org-a.fgentic.localhost' \
	'org-b.fgentic.localhost' \
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
	'select(.kind == "Kustomization" and .metadata.name == "namespaces") |
    (.spec.components | contains(["../federation/namespaces"]))' \
	"${WORK_DIR}/cluster.yaml" 'matrix-b is not owned by the early namespace layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "postgres") |
    (.spec.components | contains(["../federation/postgres"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver B database is not composed into the Postgres layer'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix") |
    (.spec.components | contains(["../federation/matrix-a"]))' \
	"${WORK_DIR}/cluster.yaml" 'homeserver A is not patched through the Matrix layer'

# The A homeserver is patched only in this disposable overlay. Its outbound trust and domain
# restrictions must mirror B, otherwise one direction of the lab can silently become open.
yq --unwrapScalar '
  .. | select(tag == "!!str" and contains("federation_domain_whitelist"))
' "${MATRIX_A_COMPONENT}" >"${WORK_DIR}/homeserver-a-config.yaml"
for contract in \
	'federation_domain_whitelist:' \
	'${server_name}' \
	'${federation_partner_server_name}' \
	'federation_custom_ca_list:' \
	'/etc/fgentic-ca/ca.crt' \
	'ip_range_whitelist:' \
	'${federation_gateway_ip}' \
	'trusted_key_servers: []' \
	'/32'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-a-config.yaml" >/dev/null ||
		fail "homeserver A federation config omits ${contract}"
done

for contract in \
	'name: fgentic-local-ca' \
	'mountPath: /etc/fgentic-ca' \
	'readOnly: true' \
	'path: /spec/values/synapse/hostAliases'; do
	rg --fixed-strings "${contract}" "${MATRIX_A_COMPONENT}" >/dev/null ||
		fail "homeserver A runtime trust wiring omits ${contract}"
done

kubectl kustomize "${MATRIX_B_LAYER}" >"${WORK_DIR}/matrix-b.yaml"
kubectl kustomize "${NAMESPACE_COMPONENT}" >"${WORK_DIR}/namespaces.yaml"
kubectl kustomize "${POSTGRES_COMPONENT}" >"${WORK_DIR}/postgres.yaml"

assert_yq \
	'select(.kind == "Namespace" and .metadata.name == "matrix-b") |
    .metadata.labels."pod-security.kubernetes.io/enforce" == "baseline" and
    .metadata.labels."pod-security.kubernetes.io/audit" == "restricted" and
    .metadata.labels."pod-security.kubernetes.io/warn" == "restricted"' \
	"${WORK_DIR}/namespaces.yaml" 'matrix-b is not owned by the namespace layer with PSS labels'

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
        "${federation_partner_server_name}", "matrix.${federation_partner_server_name}"])) and
    ([.spec.values.synapse.extraVolumes[] |
      select(.configMap.name == "fgentic-local-ca")] | length) == 1 and
    ([.spec.values.synapse.extraVolumeMounts[] |
      select(.mountPath == "/etc/fgentic-ca" and .readOnly == true)] | length) == 1' \
	"${WORK_DIR}/matrix-b.yaml" 'homeserver B is not a minimal, locally trusted Synapse-only ESS release'

yq '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b")
' "${WORK_DIR}/matrix-b.yaml" >"${WORK_DIR}/homeserver-b.yaml"
yq --unwrapScalar '
  .. | select(tag == "!!str" and contains("federation_domain_whitelist"))
' "${WORK_DIR}/homeserver-b.yaml" >"${WORK_DIR}/homeserver-b-config.yaml"
for contract in \
	'federation_domain_whitelist:' \
	'${server_name}' \
	'${federation_partner_server_name}' \
	'federation_custom_ca_list:' \
	'/etc/fgentic-ca/ca.crt' \
	'ip_range_whitelist:' \
	'${federation_gateway_ip}' \
	'trusted_key_servers: []' \
	'/32'; do
	rg --fixed-strings "${contract}" "${WORK_DIR}/homeserver-b-config.yaml" >/dev/null ||
		fail "homeserver B federation config omits ${contract}"
done
if rg --fixed-strings -e '10.0.0.0/8' -e '172.16.0.0/12' -e '192.168.0.0/16' \
	"${WORK_DIR}/homeserver-a-config.yaml" "${WORK_DIR}/homeserver-b-config.yaml" >/dev/null; then
	fail 'federation config trusts a broad private network instead of the ingress /32'
fi

for homeserver in "${WORK_DIR}/homeserver-a-config.yaml" "${WORK_DIR}/homeserver-b-config.yaml"; do
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

yq --unwrapScalar '.patches[]?.patch' \
	"${POSTGRES_COMPONENT}/kustomization.yaml" >"${WORK_DIR}/postgres-patches.yaml"
for contract in \
	'name: synapse_b' \
	'name: pg-synapse-b' \
	'hostssl synapse_b synapse_b all scram-sha-256' \
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
      length) == 2' \
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

# Public CA material is copied into both namespaces at runtime; the repository and cluster
# snapshots must never carry the local signing key.
for contract in \
	'for namespace in matrix matrix-b' \
	'create configmap fgentic-local-ca' \
	'ca.crt' \
	'pg-synapse-b'; do
	rg --fixed-strings "${contract}" "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
		fail "federation lifecycle omits ${contract}"
done
if rg --fixed-strings 'ca.key' "${LIFECYCLE}" "${ROOT_DIR}/scripts/demo.sh" \
	"${CLUSTER_OVERLAY}" "${FEDERATION_ROOT}" >/dev/null; then
	fail 'federation assets reference the private local-CA key'
fi

# The up path is the acceptance proof: create an explicitly federated v12 room, exchange messages
# in both directions, and observe them through sync. It must use the free deterministic profile.
for contract in \
	'FGENTIC_DEMO_PROFILE=federation' \
	'room_version' \
	'"12"' \
	'm.federate' \
	'/_matrix/client/v3/rooms/' \
	'/invite' \
	'/join' \
	'/send/m.room.message/' \
	'/sync?timeout=1000' \
	'@alice:${SERVER_A}' \
	'@bob:${SERVER_B}' \
	'SYNAPSE_REGISTRATION_SHARED_SECRET' \
	'whitelist_enabled == true' \
	'federation-a-to-b-' \
	'federation-b-to-a-'; do
	rg --fixed-strings "${contract}" "${LIFECYCLE}" "${SEED}" >/dev/null ||
		fail "federation acceptance proof omits ${contract}"
done
rg --fixed-strings 'LLM_PROVIDER="demo"' "${ROOT_DIR}/scripts/demo.sh" >/dev/null ||
	fail 'federation profile can select a paid model provider'

echo 'Federation topology and lifecycle contracts passed.'
