#!/usr/bin/env bash
# Offline contract for the opt-in two-control-plane federation drill manifests.
# yq bindings are intentionally protected from shell expansion in single-quoted expressions.
# shellcheck disable=SC2016
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-federation-split-manifests.XXXXXX")"
readonly WORK_DIR
readonly FIXTURE="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"
readonly A_RENDER="${WORK_DIR}/a.yaml"
readonly B_RENDER="${WORK_DIR}/b.yaml"
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

fail() {
	echo "error: $*" >&2
	exit 1
}

assert_yq_all() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq eval-all --exit-status "${expression}" "${document}" >/dev/null || fail "${message}"
}

assert_yq() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq --exit-status "${expression}" "${document}" >/dev/null || fail "${message}"
}

inventory() {
	local document="$1"
	local expression="$2"
	yq eval-all -r "${expression}" "${document}"
}

assert_lines() {
	local description="$1"
	local actual="$2"
	local expected="$3"
	if [[ "${actual}" != "${expected}" ]]; then
		diff -u <(printf '%s\n' "${expected}") <(printf '%s\n' "${actual}") || true
		fail "${description} inventory changed"
	fi
}

assert_inventory() {
	local description="$1"
	local document="$2"
	local expression="$3"
	local expected="$4"
	local actual
	actual="$(inventory "${document}" "${expression}")" || fail "could not render ${description} inventory"
	assert_lines "${description}" "${actual}" "${expected}"
}

assert_no_dangling_dependencies() {
	local document="$1"
	local description="$2"
	local names dependencies missing names_file dependencies_file
	names="$(inventory "${document}" '
    [select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization") |
      .metadata.name] | unique | sort | .[]
  ')"
	dependencies="$(inventory "${document}" '
    [select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization") |
      .spec.dependsOn[]?.name] | unique | sort | .[]
  ')"
	names_file="${WORK_DIR}/$(basename "${document}").dependency-names"
	dependencies_file="${WORK_DIR}/$(basename "${document}").dependencies"
	printf '%s\n' "${names}" | sed '/^$/d' >"${names_file}"
	printf '%s\n' "${dependencies}" | sed '/^$/d' >"${dependencies_file}"
	missing="$(comm -23 "${dependencies_file}" "${names_file}")"
	[[ -z "${missing}" ]] || fail "${description} has dangling Flux dependencies: ${missing}"
}

flux_render() {
	local role="$1"
	local output="$2"
	local role_fixture="${WORK_DIR}/fixture-${role}.yaml"
	local raw_output="${WORK_DIR}/raw-${role}.yaml"
	local settings="${ROOT_DIR}/clusters/federation-split-${role}/platform-settings.yaml"
	cp "${FIXTURE}" "${role_fixture}"
	SETTINGS="${settings}" yq -i \
		'.spec.postBuild.substitute = load(strenv(SETTINGS)).data' "${role_fixture}"
	flux build kustomization cluster-overlay-validation \
		--path "clusters/federation-split-${role}" \
		--kustomization-file "${role_fixture}" \
		--dry-run \
		--in-memory-build \
		--strict-substitute \
		--recursive \
		--local-sources "GitRepository/flux-system/flux-system=${ROOT_DIR}" \
		>"${raw_output}"
	(
		local key value settings_env
		settings_env="$(yq -r '.data | to_entries[] | .key + "=" + .value' "${settings}")"
		while IFS='=' read -r key value; do
			export "${key}=${value}"
		done <<<"${settings_env}"
		flux envsubst --strict <"${raw_output}"
	) >"${output}"
}

assert_marker_count() {
	local document="$1"
	local marker="$2"
	local expected="$3"
	local actual
	actual="$(rg --only-matching --fixed-strings "${marker}" "${document}" | wc -l | tr -d '[:space:]')"
	[[ "${actual}" == "${expected}" ]] ||
		fail "$(basename "${document}") must contain ${expected} ${marker} markers, got ${actual}"
}

config_value() {
	local document="$1"
	local namespace="$2"
	local name="$3"
	local key="$4"
	yq -r "select(.kind == \"ConfigMap\" and .metadata.namespace == \"${namespace}\" and
    .metadata.name == \"${name}\") | .data.\"${key}\"" "${document}"
}

assert_coredns() {
	local document="$1"
	local expected="$2"
	local actual
	actual="$(config_value "${document}" kube-system coredns-custom fgentic-split.server)"
	[[ "${actual}" == "${expected}" ]] || {
		diff -u <(printf '%s\n' "${expected}") <(printf '%s\n' "${actual}") || true
		fail "$(basename "${document}") CoreDNS host inventory changed"
	}
	[[ "${actual}" != *'*'* ]] || fail "split CoreDNS must not contain wildcard records"
}

assert_certificate_bundle() {
	local document="$1"
	local namespace="$2"
	local name="$3"
	local expected_count="$4"
	local expected_roots="$5"
	local output bundle summary count
	output="${WORK_DIR}/$(basename "${document}").${namespace}.${name}.pem"
	bundle="$(config_value "${document}" "${namespace}" "${name}" ca.crt)"
	bundle="${bundle//__FGENTIC_SPLIT_ORG_A_CA_PEM__/${A_CA}}"
	bundle="${bundle//__FGENTIC_SPLIT_ORG_B_CA_PEM__/${B_CA}}"
	[[ "${bundle}" != *'__FGENTIC_SPLIT_'* ]] || fail "unreplaced CA marker in ${namespace}/${name}"
	printf '%s\n' "${bundle}" >"${output}"
	summary="$(openssl crl2pkcs7 -nocrl -certfile "${output}" |
		openssl pkcs7 -print_certs -noout)" || fail "${namespace}/${name} is not a PEM certificate bundle"
	count="$(awk '/^subject=/{count++} END{print count + 0}' <<<"${summary}")"
	[[ "${count}" == "${expected_count}" ]] ||
		fail "${namespace}/${name} must contain ${expected_count} certificates, got ${count}"
	case "${expected_roots}" in
		a-b)
		[[ "${summary}" == *'CN = split-org-a-root'* && "${summary}" == *'CN = split-org-b-root'* ]] ||
			fail "${namespace}/${name} must contain the independent A and B public roots"
		;;
		b)
		[[ "${summary}" != *'CN = split-org-a-root'* && "${summary}" == *'CN = split-org-b-root'* ]] ||
			fail "${namespace}/${name} must trust only B"
		;;
		*) fail "unknown expected root set: ${expected_roots}" ;;
	esac
}

for command in awk comm cp diff flux kubeconform kubectl openssl rg sed tr yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

echo '==> Recursively rendering both split Flux graphs'
flux_render a "${A_RENDER}"
flux_render b "${B_RENDER}"

echo '==> Checking exact Flux, namespace, controller, and database ownership'
assert_inventory 'control plane A Flux' "${A_RENDER}" '
  [select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization") |
    .metadata.name] | sort | .[]
' $'agentgateway\nagentgateway-provider\ncontrollers\ngateway\nkagent\nmatrix\nmatrix-c\nnamespaces\nplatform-secrets\npolicies\npostgres'
assert_inventory 'control plane B Flux' "${B_RENDER}" '
  [select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization") |
    .metadata.name] | sort | .[]
' $'controllers\ngateway\nkeycloak\nmatrix-b\nnamespaces\nplatform-secrets\npolicies\npostgres'
assert_no_dangling_dependencies "${A_RENDER}" 'control plane A'
assert_no_dangling_dependencies "${B_RENDER}" 'control plane B'

assert_yq '
  select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and
    .metadata.name == "agentgateway") |
  (.spec.dependsOn | map(.name) | sort | join(",")) == "controllers,gateway"
' "${A_RENDER}" 'A agentgateway must depend on controllers and gateway, never local Keycloak'
assert_yq '
  select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and
    .metadata.name == "controllers") |
  (.spec.dependsOn | length) == 1 and .spec.dependsOn[0].name == "namespaces"
' "${A_RENDER}" 'A controllers must wait for namespace-owned DNS and trust'
assert_yq '
  select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and
    .metadata.name == "controllers") |
  (.spec.dependsOn | length) == 1 and .spec.dependsOn[0].name == "namespaces"
' "${B_RENDER}" 'B controllers must wait for namespace-owned DNS and trust'

assert_inventory 'control plane A namespaces' "${A_RENDER}" '
  [select(.kind == "Namespace") | .metadata.name] | sort | .[]
' $'agentgateway-system\ncert-manager\ncnpg-system\ngateway\nkagent\nmatrix\nmatrix-c\nmodels\npostgres'
assert_inventory 'control plane B namespaces' "${B_RENDER}" '
  [select(.kind == "Namespace") | .metadata.name] | sort | .[]
' $'cert-manager\ncnpg-system\ngateway\nkeycloak\nmatrix-b\npostgres'
assert_inventory 'control plane A resource namespaces' "${A_RENDER}" '
  [select(.metadata.namespace != null) | .metadata.namespace] | unique | sort | .[]
' $'agentgateway-system\ncert-manager\ncnpg-system\nflux-system\ngateway\nkagent\nkube-system\nmatrix\nmatrix-c\nmodels\npostgres'
assert_inventory 'control plane B resource namespaces' "${B_RENDER}" '
  [select(.metadata.namespace != null) | .metadata.namespace] | unique | sort | .[]
' $'cert-manager\ncnpg-system\nflux-system\ngateway\nkeycloak\nkube-system\nmatrix-b\npostgres'

assert_inventory 'control plane A chart sources' "${A_RENDER}" '
  [select(.kind == "HelmRepository" or .kind == "OCIRepository") |
    (.kind + "/" + .metadata.name)] | sort | .[]
' $'HelmRepository/cnpg\nHelmRepository/jetstack\nHelmRepository/kagent\nHelmRepository/traefik\nOCIRepository/agentgateway\nOCIRepository/agentgateway-crds\nOCIRepository/ess-matrix-stack'
assert_inventory 'control plane B chart sources' "${B_RENDER}" '
  [select(.kind == "HelmRepository" or .kind == "OCIRepository") |
    (.kind + "/" + .metadata.name)] | sort | .[]
' $'HelmRepository/cnpg\nHelmRepository/codecentric\nHelmRepository/jetstack\nHelmRepository/traefik\nOCIRepository/ess-matrix-stack'
assert_inventory 'control plane A Helm releases' "${A_RENDER}" '
  [select(.kind == "HelmRelease") | (.metadata.namespace + "/" + .metadata.name)] |
    sort | .[]
' $'agentgateway-system/agentgateway\nagentgateway-system/agentgateway-crds\ncert-manager/cert-manager\ncnpg-system/cloudnative-pg\ngateway/traefik\nkagent/kagent\nkagent/kagent-crds\nmatrix-c/matrix-stack-c\nmatrix/matrix-stack'
assert_inventory 'control plane B Helm releases' "${B_RENDER}" '
  [select(.kind == "HelmRelease") | (.metadata.namespace + "/" + .metadata.name)] |
    sort | .[]
' $'cert-manager/cert-manager\ncnpg-system/cloudnative-pg\ngateway/traefik\nkeycloak/keycloak\nmatrix-b/matrix-stack-b'

assert_inventory 'control plane A databases' "${A_RENDER}" '
  [select(.kind == "Database") | .spec.name] | sort | .[]
' $'kagent\nsynapse\nsynapse_c'
assert_inventory 'control plane B databases' "${B_RENDER}" '
  [select(.kind == "Database") | .spec.name] | sort | .[]
' $'keycloak\nsynapse_b'
assert_inventory 'control plane A managed roles' "${A_RENDER}" '
  [select(.kind == "Cluster" and .metadata.name == "platform-pg") |
    .spec.managed.roles[].name] | sort | .[]
' $'kagent\nsynapse\nsynapse_c'
assert_inventory 'control plane B managed roles' "${B_RENDER}" '
  [select(.kind == "Cluster" and .metadata.name == "platform-pg") |
    .spec.managed.roles[].name] | sort | .[]
' $'keycloak\nsynapse_b'
assert_inventory 'control plane A managed role password secrets' "${A_RENDER}" '
  [select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster") as $cluster |
    $cluster.spec.managed.roles[] |
    ($cluster.metadata.namespace + "/" + $cluster.metadata.name + ":" + .name + "=" +
      .passwordSecret.name)] | sort | .[]
' $'postgres/platform-pg:kagent=pg-kagent\npostgres/platform-pg:synapse=pg-synapse\npostgres/platform-pg:synapse_c=pg-synapse-c'
assert_inventory 'control plane B managed role password secrets' "${B_RENDER}" '
  [select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Cluster") as $cluster |
    $cluster.spec.managed.roles[] |
    ($cluster.metadata.namespace + "/" + $cluster.metadata.name + ":" + .name + "=" +
      .passwordSecret.name)] | sort | .[]
' $'postgres/platform-pg:keycloak=pg-keycloak\npostgres/platform-pg:synapse_b=pg-synapse-b'
assert_inventory 'control plane A database ownership' "${A_RENDER}" '
  [select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Database") |
    (.metadata.namespace + "/" + .metadata.name + "=" + .spec.name + ":" + .spec.owner +
      "@" + .spec.cluster.name)] | sort | .[]
' $'postgres/kagent=kagent:kagent@platform-pg\npostgres/synapse-c=synapse_c:synapse_c@platform-pg\npostgres/synapse=synapse:synapse@platform-pg'
assert_inventory 'control plane B database ownership' "${B_RENDER}" '
  [select(.apiVersion == "postgresql.cnpg.io/v1" and .kind == "Database") |
    (.metadata.namespace + "/" + .metadata.name + "=" + .spec.name + ":" + .spec.owner +
      "@" + .spec.cluster.name)] | sort | .[]
' $'postgres/keycloak=keycloak:keycloak@platform-pg\npostgres/synapse-b=synapse_b:synapse_b@platform-pg'
assert_yq_all '[select(.kind == "DatabaseRole")] | length == 0' "${A_RENDER}" \
	'A must not retain knowledge database roles'
assert_yq_all '[select(.kind == "DatabaseRole")] | length == 0' "${B_RENDER}" \
	'B must not retain knowledge database roles'

assert_inventory 'control plane A Matrix database secret wiring' "${A_RENDER}" '
  [select(.kind == "HelmRelease" and
    (.metadata.name == "matrix-stack" or .metadata.name == "matrix-stack-c")) |
    (.metadata.namespace + "/" + .metadata.name + "=" +
      .spec.values.synapse.postgres.user + ":" +
      .spec.values.synapse.postgres.database + ":" +
      .spec.values.synapse.postgres.password.secret + ":" +
      .spec.values.synapse.postgres.password.secretKey)] | sort | .[]
' $'matrix-c/matrix-stack-c=synapse_c:synapse_c:pg-synapse-c:password\nmatrix/matrix-stack=synapse:synapse:pg-synapse:password'
assert_inventory 'control plane B Matrix database secret wiring' "${B_RENDER}" '
  [select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
    (.metadata.namespace + "/" + .metadata.name + "=" +
      .spec.values.synapse.postgres.user + ":" +
      .spec.values.synapse.postgres.database + ":" +
      .spec.values.synapse.postgres.password.secret + ":" +
      .spec.values.synapse.postgres.password.secretKey)] | sort | .[]
' 'matrix-b/matrix-stack-b=synapse_b:synapse_b:pg-synapse-b:password'
assert_yq '
  select(.kind == "HelmRelease" and .metadata.namespace == "kagent" and
    .metadata.name == "kagent") |
  [.spec.values.controller.volumes[] | select(.name == "kagent-db")] as $volumes |
  [.spec.values.controller.volumeMounts[] | select(.name == "kagent-db")] as $mounts |
  [
    (.spec.values.database.postgres.urlFile == "/var/run/secrets/kagent/url"),
    (($volumes | length) == 1),
    ($volumes[0].secret.secretName == "kagent-db"),
    (($volumes[0].secret | length) == 1),
    (($mounts | length) == 1),
    ($mounts[0].mountPath == "/var/run/secrets/kagent"),
    ($mounts[0].readOnly == true)
  ] | all_c(.)
' "${A_RENDER}" 'A kagent must consume its derived database URL from kagent/kagent-db'
assert_yq '
  select(.kind == "HelmRelease" and .metadata.namespace == "keycloak" and
    .metadata.name == "keycloak") |
  [
    (.spec.values.database.vendor == "postgres"),
    (.spec.values.database.hostname == "platform-pg-rw.postgres.svc.cluster.local"),
    (.spec.values.database.port == 5432),
    (.spec.values.database.database == "keycloak"),
    (.spec.values.database.username == "keycloak"),
    (.spec.values.database.existingSecret == "pg-keycloak"),
    (.spec.values.database.existingSecretKey == "password")
  ] | all_c(.)
' "${B_RENDER}" 'B Keycloak must consume keycloak/pg-keycloak for its scoped database role'

echo '==> Checking exact workload and cross-cluster network boundaries'
assert_inventory 'control plane A raw workloads' "${A_RENDER}" '
  [select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "DaemonSet" or
    .kind == "Job") | (.kind + "/" + (.metadata.namespace // "") + "/" + .metadata.name)] |
    sort | .[]
' $'Deployment/agentgateway-system/federation-rate-limit\nDeployment/agentgateway-system/federation-redis\nDeployment/agentgateway-system/federation-usage-receipt\nDeployment/models/demo-llm'
assert_inventory 'control plane B raw workloads' "${B_RENDER}" '
  [select(.kind == "Deployment" or .kind == "StatefulSet" or .kind == "DaemonSet" or
    .kind == "Job") | (.kind + "/" + (.metadata.namespace // "") + "/" + .metadata.name)] |
    sort | .[]
' ''
assert_inventory 'control plane A Agents' "${A_RENDER}" '
  [select(.kind == "Agent") | (.metadata.namespace + "/" + .metadata.name)] | sort | .[]
' 'kagent/docs-qa'
assert_inventory 'control plane B Agents' "${B_RENDER}" '
  [select(.kind == "Agent") | (.metadata.namespace + "/" + .metadata.name)] | sort | .[]
' ''
assert_inventory 'control plane A Gateways' "${A_RENDER}" '
  [select(.kind == "Gateway") | (.metadata.namespace + "/" + .metadata.name)] | sort | .[]
' $'agentgateway-system/agentgateway-proxy\ngateway/federation-c\ngateway/fgentic-gateway'
assert_inventory 'control plane B Gateways' "${B_RENDER}" '
  [select(.kind == "Gateway") | (.metadata.namespace + "/" + .metadata.name)] | sort | .[]
' 'gateway/federation-b'
assert_yq '
  select(.kind == "Gateway" and .metadata.namespace == "gateway" and
    .metadata.name == "federation-b") |
  [.spec.listeners[] | select(.name == "https-id-b")] as $listeners |
  [
    (($listeners | length) == 1),
    ($listeners[0].hostname == "id.org-b.fgentic.test"),
    ($listeners[0].protocol == "HTTPS"),
    ($listeners[0].port == 443),
    ($listeners[0].tls.mode == "Terminate"),
    (($listeners[0].tls.certificateRefs | length) == 1),
    ($listeners[0].tls.certificateRefs[0].name == "matrix-b-tls"),
    ($listeners[0].allowedRoutes.namespaces.from == "Selector"),
    (($listeners[0].allowedRoutes.namespaces.selector.matchLabels | length) == 1),
    ($listeners[0].allowedRoutes.namespaces.selector.matchLabels["kubernetes.io/metadata.name"] ==
      "keycloak")
  ] | all_c(.)
' "${B_RENDER}" 'B identity listener must expose only id.org-b through the Keycloak namespace'
assert_yq '
  select(.kind == "Certificate" and .metadata.namespace == "gateway" and
    .metadata.name == "matrix-b-tls") |
  [
    (.spec.secretName == "matrix-b-tls"),
    (.spec.issuerRef.kind == "ClusterIssuer"),
    (.spec.issuerRef.name == "local-ca"),
    (.spec.issuerRef.group == null),
    ((.spec.dnsNames | length) == 3),
    ((.spec.dnsNames | sort | join(",")) ==
      "id.org-b.fgentic.test,matrix.org-b.fgentic.test,org-b.fgentic.test")
  ] | all_c(.)
' "${B_RENDER}" 'B Gateway certificate must use local-ca and cover the exact B server edge'
assert_yq '
  select(.kind == "HTTPRoute" and .metadata.namespace == "keycloak" and
    .metadata.name == "keycloak") |
  [
    ((.spec.parentRefs | length) == 1),
    (.spec.parentRefs[0].name == "federation-b"),
    (.spec.parentRefs[0].namespace == "gateway"),
    (.spec.parentRefs[0].sectionName == "https-id-b"),
    ((.spec.hostnames | length) == 1),
    (.spec.hostnames[0] == "id.org-b.fgentic.test"),
    ((.spec.rules | length) == 1),
    ((.spec.rules[0].backendRefs | length) == 1),
    (.spec.rules[0].backendRefs[0].name == "keycloak-http"),
    (.spec.rules[0].backendRefs[0].port == 80)
  ] | all_c(.)
' "${B_RENDER}" 'B Keycloak route must bind the exact identity listener, hostname, and Service'
assert_yq '
  select(.kind == "NetworkPolicy" and .metadata.namespace == "keycloak" and
    .metadata.name == "keycloak") |
  [
    ((.spec.podSelector.matchLabels | length) == 2),
    (.spec.podSelector.matchLabels."app.kubernetes.io/instance" == "keycloak"),
    (.spec.podSelector.matchLabels."app.kubernetes.io/name" == "keycloak"),
    ((.spec.policyTypes | sort | join(",")) == "Egress,Ingress"),
    (.spec.ingress == [
      {
        "from": [{
          "namespaceSelector": {
            "matchLabels": {"kubernetes.io/metadata.name": "gateway"}
          }
        }],
        "ports": [{"protocol": "TCP", "port": 8080}]
      },
      {
        "from": [{
          "podSelector": {
            "matchLabels": {
              "app.kubernetes.io/instance": "keycloak",
              "app.kubernetes.io/name": "keycloak"
            }
          }
        }],
        "ports": [{"protocol": "TCP", "port": 7800}]
      }
    ]),
    (.spec.egress == [
      {
        "to": [{
          "namespaceSelector": {
            "matchLabels": {"kubernetes.io/metadata.name": "kube-system"}
          },
          "podSelector": {"matchLabels": {"k8s-app": "kube-dns"}}
        }],
        "ports": [
          {"protocol": "UDP", "port": 53},
          {"protocol": "TCP", "port": 53}
        ]
      },
      {
        "to": [{
          "namespaceSelector": {
            "matchLabels": {"kubernetes.io/metadata.name": "postgres"}
          },
          "podSelector": {"matchLabels": {"cnpg.io/cluster": "platform-pg"}}
        }],
        "ports": [{"protocol": "TCP", "port": 5432}]
      },
      {
        "to": [{
          "podSelector": {
            "matchLabels": {
              "app.kubernetes.io/instance": "keycloak",
              "app.kubernetes.io/name": "keycloak"
            }
          }
        }],
        "ports": [{"protocol": "TCP", "port": 7800}]
      }
    ])
  ] | all_c(.)
' "${B_RENDER}" 'B Keycloak policy must retain only its exact ingress and egress boundaries'

assert_yq '
  select(.kind == "HelmRelease" and .metadata.namespace == "keycloak" and
    .metadata.name == "keycloak") |
  (.spec.values.extraEnv | from_yaml) as $env |
  [
    (($env | length) == 3),
    ($env | all_c((keys | sort | join(",")) == "name,value")),
    (($env | map(.name + "=" + .value) | sort | join("\n")) ==
      "KC_DB_POOL_MAX_SIZE=20\nKC_DB_URL_PROPERTIES=?sslmode=require\nKC_HOSTNAME=https://id.org-b.fgentic.test")
  ] | all_c(.)
' "${B_RENDER}" 'B Keycloak must publish the exact partner issuer and retain its database settings'

readonly A_DNS=$'fgentic.test:53 {\n  hosts {\n    192.0.2.10 org-a.fgentic.test matrix.org-a.fgentic.test a2a.org-a.fgentic.test\n    192.0.2.10 org-c.fgentic.test matrix.org-c.fgentic.test\n    192.0.2.11 org-b.fgentic.test matrix.org-b.fgentic.test id.org-b.fgentic.test\n    fallthrough\n  }\n}'
readonly B_DNS=$'fgentic.test:53 {\n  hosts {\n    192.0.2.11 org-b.fgentic.test matrix.org-b.fgentic.test id.org-b.fgentic.test\n    192.0.2.10 org-a.fgentic.test matrix.org-a.fgentic.test a2a.org-a.fgentic.test\n    192.0.2.10 org-c.fgentic.test matrix.org-c.fgentic.test\n    fallthrough\n  }\n}'
assert_coredns "${A_RENDER}" "${A_DNS}"
assert_coredns "${B_RENDER}" "${B_DNS}"

assert_yq_all '
  [select(.kind == "HelmRelease" and
    (.metadata.name == "matrix-stack" or .metadata.name == "matrix-stack-c"))] as $matrix |
  [
    (($matrix | length) == 2),
    ($matrix | all_c(
      .spec.values.synapse.hostAliases == null and
      .spec.values.synapse.extraVolumes[0].configMap.name ==
        "fgentic-federation-ca-bundle"
    )),
    ($matrix | map(
      .spec.values.synapse.additional."10-federation".config | from_yaml |
      .ip_range_whitelist | sort | join(",")
    ) | all_c(. == "192.0.2.10/32,192.0.2.11/32"))
  ] | all_c(.)
' "${A_RENDER}" 'A/C Synapse must use split DNS, both exact gateway IPs, and bilateral trust'
assert_yq '
  select(.kind == "HelmRelease" and .metadata.name == "matrix-stack-b") |
  (.spec.values.synapse.additional."10-federation".config | from_yaml |
    .ip_range_whitelist | sort | join(",")) as $ranges |
  [
    (.spec.values.synapse.hostAliases == null),
    (.spec.values.synapse.extraVolumes[0].configMap.name ==
      "fgentic-federation-ca-bundle"),
    ($ranges == "192.0.2.10/32,192.0.2.11/32")
  ] | all_c(.)
' "${B_RENDER}" 'B Synapse must use split DNS, both exact gateway IPs, and bilateral trust'

echo '==> Checking independent public CA bundle composition'
assert_marker_count "${A_RENDER}" __FGENTIC_SPLIT_ORG_A_CA_PEM__ 2
assert_marker_count "${A_RENDER}" __FGENTIC_SPLIT_ORG_B_CA_PEM__ 3
assert_marker_count "${B_RENDER}" __FGENTIC_SPLIT_ORG_A_CA_PEM__ 1
assert_marker_count "${B_RENDER}" __FGENTIC_SPLIT_ORG_B_CA_PEM__ 1
! rg --quiet -- '-----BEGIN [A-Z ]*PRIVATE KEY-----' "${A_RENDER}" "${B_RENDER}" ||
	fail 'split manifests must never contain private CA keys'

A_CA="$(openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 \
	-subj /CN=split-org-a-root -keyout /dev/null 2>/dev/null)"
readonly A_CA
B_CA="$(openssl req -x509 -newkey rsa:2048 -nodes -sha256 -days 1 \
	-subj /CN=split-org-b-root -keyout /dev/null 2>/dev/null)"
readonly B_CA
assert_certificate_bundle "${A_RENDER}" matrix fgentic-federation-ca-bundle 2 a-b
assert_certificate_bundle "${A_RENDER}" matrix-c fgentic-federation-ca-bundle 2 a-b
assert_certificate_bundle "${B_RENDER}" matrix-b fgentic-federation-ca-bundle 2 a-b
assert_certificate_bundle "${A_RENDER}" agentgateway-system fgentic-org-b-jwks-ca 1 b

echo '==> Checking the remote JWKS TLS and issuer contract'
assert_inventory 'A Agentgateway backends' "${A_RENDER}" '
  [select(.kind == "AgentgatewayBackend") | .metadata.name] | sort | .[]
' $'federated-docs-qa-a2a\nllm-demo\norg-b-jwks'
assert_yq_all '
  [select(.kind == "OCIRepository" and
    (.metadata.name == "agentgateway" or .metadata.name == "agentgateway-crds"))] as $sources |
  [
    (($sources | length) == 2),
    ($sources | all_c(.spec.ref.tag == "v1.3.1"))
  ] | all_c(.)
' "${A_RENDER}" 'A must pin the agentgateway API and controller to v1.3.1'
assert_yq '
  select(.kind == "AgentgatewayBackend" and .metadata.name == "org-b-jwks" and
    .metadata.namespace == "agentgateway-system") |
  .spec.static.host == "id.org-b.fgentic.test" and
  .spec.static.port == 443 and
  (.spec.policies.tls.caCertificateRefs | length) == 1 and
  .spec.policies.tls.caCertificateRefs[0].name == "fgentic-org-b-jwks-ca" and
  .spec.policies.tls.sni == "id.org-b.fgentic.test" and
  (.spec.policies.tls.verifySubjectAltNames | length) == 1 and
  .spec.policies.tls.verifySubjectAltNames[0] == "id.org-b.fgentic.test" and
  .spec.policies.tls.insecureSkipVerify == null
' "${A_RENDER}" 'A remote JWKS backend must pin B CA, SNI, and SAN under spec.policies.tls'
assert_yq '
  select(.kind == "AgentgatewayPolicy" and .metadata.name == "federated-docs-qa") |
  (.spec.traffic.jwtAuthentication.providers[0].issuer ==
    "https://id.org-b.fgentic.test/realms/fgentic-federation") and
  .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.group ==
    "agentgateway.dev" and
  .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.kind ==
    "AgentgatewayBackend" and
  .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.name == "org-b-jwks" and
  .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.namespace == null and
  .spec.traffic.jwtAuthentication.providers[0].jwks.remote.backendRef.port == null
' "${A_RENDER}" 'A JWT policy must resolve B through the static TLS backend'
assert_yq_all '[select(.kind == "ReferenceGrant")] | length == 0' "${A_RENDER}" \
	'A must not retain the single-cluster Keycloak ReferenceGrant'
assert_yq_all '[select(.kind == "Service" and .metadata.name == "keycloak-http")] | length == 0' \
	"${A_RENDER}" 'A must not retain a local Keycloak Service'
assert_yq_all '[select(.metadata.namespace == "keycloak")] | length == 0' "${A_RENDER}" \
	'A must not retain any local Keycloak resource'
assert_yq '
  select(.kind == "ConfigMap" and .metadata.name == "keycloak-realm" and
    .metadata.namespace == "keycloak") |
  (.data | length) == 1 and
  (.data | has("fgentic-federation-realm.json")) and
  (.data."fgentic-federation-realm.json" | contains("\"realm\": \"fgentic-federation\""))
' "${B_RENDER}" 'B must own only the machine-client federation realm'

! rg --quiet --fixed-strings 'fgentic.localhost' "${A_RENDER}" "${B_RENDER}" ||
	fail 'split federation must use fixed .test names, never the canonical .localhost names'
! rg --quiet --fixed-strings 'knowledge-ingestion' "${A_RENDER}" "${B_RENDER}" ||
	fail 'split federation must remove the disabled knowledge-ingestion Flux layer'

echo '==> Schema-validating both composed renders'
kubeconform -strict -ignore-missing-schemas -summary "${A_RENDER}"
kubeconform -strict -ignore-missing-schemas -summary "${B_RENDER}"

echo 'Split federation manifest contracts passed.'
