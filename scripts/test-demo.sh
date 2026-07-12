#!/usr/bin/env bash
# Offline contract checks for the credential-free evaluation lifecycle and its embedded model.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo-check.XXXXXX")"
readonly WORK_DIR
readonly DEMO="${ROOT_DIR}/scripts/demo.sh"
readonly -a DEMO_SOURCES=(
	"${DEMO}"
	"${ROOT_DIR}/scripts/lib.sh"
	"${ROOT_DIR}/scripts/lib/demo-config.sh"
	"${ROOT_DIR}/scripts/lib/demo-cluster.sh"
	"${ROOT_DIR}/scripts/lib/demo-secrets.sh"
	"${ROOT_DIR}/scripts/lib/demo-federation.sh"
)
readonly -a SHARED_HELPER_ENTRYPOINTS=(
	"${ROOT_DIR}/scripts/demo.sh"
	"${ROOT_DIR}/scripts/federation.sh"
	"${ROOT_DIR}/scripts/reload-federation-policy.sh"
	"${ROOT_DIR}/scripts/seed-demo.sh"
	"${ROOT_DIR}/scripts/seed-federation.sh"
)
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

assert_yq() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq --exit-status "${expression}" "${document}" >/dev/null || {
		echo "error: ${message}" >&2
		exit 1
	}
}

bash -n "${DEMO_SOURCES[@]}" "${ROOT_DIR}/scripts/seed-demo.sh"
(
	# Validate the generated cluster config without creating a cluster. Both disposable profiles
	# need the explicit disk floor; only federation moves ingress to its alternate loopback.
	# shellcheck source=scripts/lib/demo-config.sh
	source "${ROOT_DIR}/scripts/lib/demo-config.sh"
	CLUSTER_NAME=fgentic-demo-fixture
	FEDERATION_LOOPBACK=127.0.0.2
	PROFILE=demo
	render_k3d_config "${WORK_DIR}/demo-k3d.yaml"
	PROFILE=federation
	render_k3d_config "${WORK_DIR}/federation-k3d.yaml"
	for config in demo-k3d.yaml federation-k3d.yaml; do
		assert_yq \
			'.options.k3s.extraArgs[] |
        select(.arg == "--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<1Gi,imagefs.available<1Gi,nodefs.inodesFree<5%,imagefs.inodesFree<5%") |
        (.nodeFilters | ((length == 1) and (.[0] == "server:*")))' \
			"${WORK_DIR}/${config}" "${config} omits the disposable-cluster eviction floor"
	done
	assert_yq '.ports[0].port == "127.0.0.1:80:80" and .ports[1].port == "127.0.0.1:443:443"' \
		"${WORK_DIR}/demo-k3d.yaml" 'demo ingress ports changed'
	assert_yq '.ports[0].port == "127.0.0.2:80:80" and .ports[1].port == "127.0.0.2:443:443"' \
		"${WORK_DIR}/federation-k3d.yaml" 'federation ingress ports changed'
)
for entrypoint in "${SHARED_HELPER_ENTRYPOINTS[@]}"; do
	rg --fixed-strings 'source "${ROOT_DIR}/scripts/lib.sh"' "${entrypoint}" >/dev/null || {
		echo "error: ${entrypoint#"${ROOT_DIR}/"} does not source the shared script library" >&2
		exit 1
	}
done
for helper in die require_command bootstrap_secret_value request_status; do
	definitions="$(rg --files-with-matches "^${helper}\\(\\)" \
		"${ROOT_DIR}/scripts/lib.sh" "${SHARED_HELPER_ENTRYPOINTS[@]}" | wc -l)"
	[ "${definitions}" -eq 1 ] || {
		echo "error: shared helper ${helper} has ${definitions} definitions" >&2
		exit 1
	}
done
"${DEMO}" --help >"${WORK_DIR}/help.txt"
rg --fixed-strings 'deterministic in-cluster response stub' "${WORK_DIR}/help.txt" >/dev/null
rg --fixed-strings 'FGENTIC_ALLOW_PAID_PROVIDER=yes' "${WORK_DIR}/help.txt" >/dev/null
rg --fixed-strings 'FGENTIC_DEMO_CACHE_DIR' "${WORK_DIR}/help.txt" >/dev/null
if FGENTIC_DEMO_CLUSTER=fgentic "${DEMO}" down \
	>"${WORK_DIR}/reserved-cluster.txt" 2>&1; then
	echo 'error: demo teardown accepted the reserved fgentic cluster name' >&2
	exit 1
fi
rg --fixed-strings 'must be fgentic-demo' "${WORK_DIR}/reserved-cluster.txt" >/dev/null

fake_bin="${WORK_DIR}/fake-bin"
mkdir -p "${fake_bin}"
cat >"${fake_bin}/k3d" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_DOCKER_STATE:?}"
case "${1:-} ${2:-}" in
"cluster list")
	if [ -f "${state}/cluster" ]; then
		printf '[{"name":"%s"}]\n' "${FAKE_CLUSTER_NAME:?}"
	else
		printf '[]\n'
	fi
	;;
"cluster delete")
	printf 'cluster-delete\n' >>"${state}/commands"
	rm -f "${state}/cluster"
	if [ "${FAKE_TEARDOWN_SCENARIO:?}" = clean ]; then
		rm -f "${state}/container" "${state}/network" "${state}/volume"
	fi
	;;
*) exit 2 ;;
esac
EOF
cat >"${fake_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
state="${FAKE_DOCKER_STATE:?}"
case "${1:-}" in
inspect)
	[ -f "${state}/container" ] || exit 1
	if [ "${FAKE_TEARDOWN_SCENARIO:?}" = foreign ]; then
		printf 'foreign\n'
	else
		printf 'true\n'
	fi
	;;
ps)
	[ -f "${state}/container" ] && printf 'owned-container\n'
	;;
rm)
	shift
	[ "${1:-}" = --force ] && shift
	for container_id in "$@"; do
		printf 'container-rm:%s\n' "${container_id}" >>"${state}/commands"
	done
	rm -f "${state}/container"
	;;
network)
	case "${2:-}" in
	inspect)
		[ -f "${state}/network" ] || exit 1
		printf 'k3d\n'
		;;
	rm)
		printf 'network-rm\n' >>"${state}/commands"
		rm -f "${state}/network"
		;;
	*) exit 2 ;;
	esac
	;;
volume)
	case "${2:-}" in
	inspect)
		[ -f "${state}/volume" ] || exit 1
		printf 'k3d/%s\n' "${FAKE_CLUSTER_NAME:?}"
		;;
	rm)
		printf 'volume-rm\n' >>"${state}/commands"
		rm -f "${state}/volume"
		;;
	*) exit 2 ;;
	esac
	;;
images) ;;
image) ;;
*) exit 2 ;;
esac
EOF
chmod +x "${fake_bin}/docker" "${fake_bin}/k3d"

run_teardown_fixture() {
	local scenario="$1"
	local state="${WORK_DIR}/teardown-${scenario}"
	mkdir -p "${state}"
	touch "${state}/cluster" "${state}/container" "${state}/network" "${state}/volume"
	if [ "${scenario}" = foreign ]; then
		if PATH="${fake_bin}:${PATH}" FAKE_DOCKER_STATE="${state}" \
			FAKE_CLUSTER_NAME=fgentic-demo-teardown FAKE_TEARDOWN_SCENARIO="${scenario}" \
			FGENTIC_DEMO_CLUSTER=fgentic-demo-teardown "${DEMO}" down \
			>"${state}/output" 2>&1; then
			echo 'error: demo teardown removed a foreign control cluster' >&2
			exit 1
		fi
		rg --fixed-strings 'refusing to delete' "${state}/output" >/dev/null
		[ -f "${state}/cluster" ] && [ -f "${state}/container" ] &&
			[ -f "${state}/network" ] && [ -f "${state}/volume" ] || {
			echo 'error: foreign teardown fixture was mutated' >&2
			exit 1
		}
		return
	fi

	PATH="${fake_bin}:${PATH}" FAKE_DOCKER_STATE="${state}" \
		FAKE_CLUSTER_NAME=fgentic-demo-teardown FAKE_TEARDOWN_SCENARIO="${scenario}" \
		FGENTIC_DEMO_CLUSTER=fgentic-demo-teardown "${DEMO}" down \
		>"${state}/output"
	for resource in cluster container network volume; do
		[ ! -e "${state}/${resource}" ] || {
			echo "error: ${scenario} teardown retained ${resource}" >&2
			exit 1
		}
	done
	rg --fixed-strings 'were preserved' "${state}/output" >/dev/null
}

run_teardown_fixture clean
run_teardown_fixture transient
run_teardown_fixture foreign
rg --fixed-strings 'container-rm:owned-container' \
	"${WORK_DIR}/teardown-transient/commands" >/dev/null
rg --fixed-strings 'network-rm' "${WORK_DIR}/teardown-transient/commands" >/dev/null
rg --fixed-strings 'volume-rm' "${WORK_DIR}/teardown-transient/commands" >/dev/null

kubectl kustomize "${ROOT_DIR}/clusters/demo" >"${WORK_DIR}/cluster.yaml"
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "platform-settings") |
    .data.llm_provider == "demo" and
    .data.llm_model == "fgentic-demo" and
    .data.demo_bridge_tag == "local" and
    .data.mas_local_login_enabled == "true" and
    .data.llm_usage_budget_15m == "100000"' \
	"${WORK_DIR}/cluster.yaml" 'demo platform settings are incomplete'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "agentgateway-provider") |
    .spec.path == "./infra/agentgateway/providers/profiles/demo"' \
	"${WORK_DIR}/cluster.yaml" 'provider-selection did not select the demo inventory'
for omitted_layer in observability observability-monitors keycloak; do
	if yq --unwrapScalar \
		'select(.kind == "Kustomization") | .metadata.name' \
		"${WORK_DIR}/cluster.yaml" | rg --fixed-strings --line-regexp "${omitted_layer}" >/dev/null; then
		echo "error: small profile still contains ${omitted_layer}" >&2
		exit 1
	fi
done
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "platform-secrets") |
    .spec.path == "./clusters/demo/empty" and (.spec.decryption == null)' \
	"${WORK_DIR}/cluster.yaml" 'demo secret inventory must be empty and non-SOPS'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "bridge") |
    ((.spec.patches[0].patch | contains("matrix-a2a-bridge")) and
     (.spec.patches[0].patch | contains("pullPolicy: Never")))' \
	"${WORK_DIR}/cluster.yaml" 'demo bridge is not pinned to the side-loaded image'

kubectl kustomize "${ROOT_DIR}/infra/flux" >"${WORK_DIR}/controllers.yaml"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "traefik") |
    .spec.timeout == "10m"' \
	"${WORK_DIR}/controllers.yaml" \
	'Traefik Helm actions must tolerate constrained-host startup'

kubectl kustomize "${ROOT_DIR}/infra/agentgateway/providers/profiles/demo" \
	>"${WORK_DIR}/provider.yaml"
assert_yq \
	"select(.kind == \"AgentgatewayBackend\" and .metadata.name == \"llm-demo\") |
    .spec.ai.provider.openai.model == \"\${llm_model}\" and
    .spec.ai.provider.host == \"demo-llm.models.svc.cluster.local\" and
    .spec.ai.provider.port == 80" \
	"${WORK_DIR}/provider.yaml" 'demo AgentgatewayBackend contract changed'
assert_yq \
	'select(.kind == "Deployment" and .metadata.name == "demo-llm") |
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
    (.spec.template.spec.containers[0].image | contains("python:3.14-slim@sha256:"))' \
	"${WORK_DIR}/provider.yaml" 'demo model workload is not pinned and hardened'

yq --unwrapScalar \
	'select(.kind == "ConfigMap" and .metadata.name == "demo-llm") | .data."server.py"' \
	"${WORK_DIR}/provider.yaml" >"${WORK_DIR}/server.py"
python -m py_compile "${WORK_DIR}/server.py"
rg --fixed-strings 'chat.completion' "${WORK_DIR}/server.py" >/dev/null
rg --fixed-strings 'data: [DONE]' "${WORK_DIR}/server.py" >/dev/null

rg --fixed-strings 'mcp-agent-callers' "${DEMO_SOURCES[@]}" >/dev/null
rg --fixed-strings 'platform-helper-mcp-credential' "${DEMO_SOURCES[@]}" >/dev/null
rg --regexp 'SOURCE_BASE_IMAGE="[^" ]+@sha256:[0-9a-f]{64}"' \
	"${DEMO_SOURCES[@]}" >/dev/null
rg --regexp 'SOURCE_GIT_PACKAGES="git=[^ ]+ git-daemon=[^ ]+ busybox-extras=[^"]+"' \
	"${DEMO_SOURCES[@]}" >/dev/null
rg --fixed-strings 'git-http-backend' "${DEMO_SOURCES[@]}" >/dev/null
rg --fixed-strings 'http://fgentic-demo-source.flux-system.svc.cluster.local:8080/cgi-bin/git/repo.git' \
	"${DEMO_SOURCES[@]}" >/dev/null
for retry_contract in \
	'if flux reconcile source git flux-system --timeout=2m >/dev/null &&' \
	"expected_revision=\"main@sha1:\${SOURCE_REVISION}\"" \
	"! kustomizations=\"\$(kubectl --namespace flux-system get kustomizations --output json)\"" \
	"! helmreleases=\"\$(kubectl get helmreleases --all-namespaces --output json)\""; do
	rg --fixed-strings "${retry_contract}" "${DEMO_SOURCES[@]}" >/dev/null || {
		echo "error: demo lifecycle does not retry transient API failures" >&2
		exit 1
	}
done
for lease_contract in \
	'configure_ephemeral_flux_controllers' \
	'FLUX_LEADER_ELECTION_LEASE_DURATION="180s"' \
	'FLUX_LEADER_ELECTION_RENEW_DEADLINE="170s"' \
	'FLUX_LEADER_ELECTION_RETRY_PERIOD="30s"' \
	"--leader-election-lease-duration=\${FLUX_LEADER_ELECTION_LEASE_DURATION}" \
	"--leader-election-renew-deadline=\${FLUX_LEADER_ELECTION_RENEW_DEADLINE}" \
	"--leader-election-retry-period=\${FLUX_LEADER_ELECTION_RETRY_PERIOD}"; do
	rg --fixed-strings -- "${lease_contract}" "${DEMO_SOURCES[@]}" >/dev/null || {
		echo "error: ephemeral Flux controllers omit ${lease_contract}" >&2
		exit 1
	}
done
rg --fixed-strings '#lobby:fgentic.localhost' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
rg --fixed-strings 'creation_content: {"m.federate": false}' \
	"${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
rg --fixed-strings '/state/m.room.create' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
for contract in \
	'create_lobby' \
	'publish_lobby_alias' \
	'set_lobby_canonical_alias' \
	'lobby_has_canonical_alias' \
	'retire_legacy_lobby_alias' \
	'Migrating legacy #lobby to immutable local-only federation policy.' \
	'#lobby is not local-only after reconciliation' \
	'--request DELETE'; do
	rg --fixed-strings -- "${contract}" "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null
done
rg --fixed-strings '/api/admin/v1/users' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null

if rg -n 'mas_password_login_enabled|llm_token_budget_15m' \
	"${ROOT_DIR}/clusters/demo" "${DEMO_SOURCES[@]}"; then
	echo 'error: demo path uses a retired platform-setting name' >&2
	exit 1
fi

echo 'Demo install contracts passed.'
