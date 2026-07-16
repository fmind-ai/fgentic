#!/usr/bin/env bash
# Offline contract checks for the credential-free evaluation lifecycle and its embedded model.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo-check.XXXXXX")"
readonly WORK_DIR
readonly DEMO="${ROOT_DIR}/scripts/demo.sh"
readonly DEV="${ROOT_DIR}/scripts/dev.sh"
readonly DEMO_SETTINGS_FILE="${ROOT_DIR}/clusters/demo/platform-settings.yaml"
readonly REPLY_FILTER="${ROOT_DIR}/scripts/lib/demo-reply.jq"
readonly SEED_STATE_FILTER="${ROOT_DIR}/scripts/lib/demo-seed-state.jq"
readonly EXPECTED_DEMO_REPLY="Fgentic's deterministic evaluation model is working through agentgateway and kagent."
readonly -a DEMO_SEED_SOURCES=(
	"${ROOT_DIR}/scripts/seed-demo.sh"
	"${REPLY_FILTER}"
	"${SEED_STATE_FILTER}"
)
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

render_demo_layer() {
	local layer="$1"
	local path="$2"
	local flux_file="${WORK_DIR}/demo-${layer}-flux.yaml"
	DEMO_LAYER="${layer}" DEMO_SETTINGS_PATH="${DEMO_SETTINGS_FILE}" yq eval-all -o=yaml \
		'select(.kind == "Kustomization" and .metadata.name == strenv(DEMO_LAYER)) |
      .spec.postBuild.substitute = load(strenv(DEMO_SETTINGS_PATH)).data' \
		"${WORK_DIR}/cluster.yaml" >"${flux_file}"
	flux build kustomization "${layer}" \
		--path "${path}" \
		--kustomization-file "${flux_file}" \
		--dry-run \
		--in-memory-build \
		--strict-substitute >"${WORK_DIR}/demo-${layer}.yaml"
}

reply_fixture() {
	local body="$1"
	local sender="${2:-@agent-docs-qa:fgentic.localhost}"
	local msgtype="${3:-m.notice}"
	jq --null-input --compact-output --arg body "${body}" --arg sender "${sender}" \
		--arg msgtype "${msgtype}" '{
      events_before: [],
      events_after: [{
        event_id: "$reply",
        type: "m.room.message",
        sender: $sender,
        content: {
          msgtype: $msgtype,
          body: $body,
          "m.relates_to": {"m.in_reply_to": {event_id: "$probe"}}
        }
      }]
    }'
}

replacement_fixture() {
	local body="$1"
	local sender="${2:-@agent-docs-qa:fgentic.localhost}"
	local replacement_sender="${3:-${sender}}"
	local replaced_event_id="${4:-\$reply}"
	local replacement_msgtype="${5:-m.notice}"
	local placeholder_body="${6:---- BEGIN FGENTIC BRIDGE PROVENANCE ---
untrusted request
--- END UNTRUSTED MATRIX CONTENT ---}"
	jq --null-input --compact-output --arg body "${body}" --arg sender "${sender}" \
		--arg replacement_sender "${replacement_sender}" \
		--arg replaced_event_id "${replaced_event_id}" \
		--arg replacement_msgtype "${replacement_msgtype}" \
		--arg placeholder_body "${placeholder_body}" '{
      events_before: [],
      events_after: [
        {
          event_id: "$reply",
          type: "m.room.message",
          sender: $sender,
          content: {
            msgtype: "m.notice",
            body: $placeholder_body,
            "m.relates_to": {"m.in_reply_to": {event_id: "$probe"}}
          }
        },
        {
          event_id: "$replacement",
          type: "m.room.message",
          sender: $replacement_sender,
          content: {
            msgtype: "m.notice",
            body: ("* " + $body),
            "m.new_content": {msgtype: $replacement_msgtype, body: $body},
            "m.relates_to": {rel_type: "m.replace", event_id: $replaced_event_id}
          }
        }
      ]
    }'
}

reply_fixture_matches() {
	local provider="${1:-demo}"
	local model="${2:-fgentic-demo}"
	jq --exit-status --arg sender '@agent-docs-qa:fgentic.localhost' --arg event_id '$probe' \
		--arg provider "${provider}" --arg model "${model}" \
		--arg expected_demo_reply "${EXPECTED_DEMO_REPLY}" \
		--from-file "${REPLY_FILTER}" >/dev/null
}

bash -n "${DEMO_SOURCES[@]}" "${DEV}" "${ROOT_DIR}/scripts/seed-demo.sh"
(
	# The Secret key already names the caller. Its value must be one Agentgateway credential record,
	# not another caller-name map, or every bridge request is rejected as an unknown API key.
	# shellcheck source=scripts/lib/demo-secrets.sh
	source "${ROOT_DIR}/scripts/lib/demo-secrets.sh"
	secret_calls=()
	apply_secret() {
		secret_calls+=("$*")
	}
	apply_a2a_secrets fixture-key
	[ "${#secret_calls[@]}" -eq 2 ]
	server_prefix='agentgateway-system a2a-bridge-callers --from-literal=matrix-a2a-bridge='
	[[ "${secret_calls[0]}" == "${server_prefix}"* ]]
	server_document="${secret_calls[0]#"${server_prefix}"}"
	jq --exit-status '
    .key == "fixture-key" and
    .metadata == {"workload": "matrix-a2a-bridge"} and
    (has("matrix-a2a-bridge") | not)
  ' <<<"${server_document}" >/dev/null
	[ "${secret_calls[1]}" = \
		'bridge a2a-bridge-credential --from-literal=token=fixture-key' ]
)
(
	# Load the pullPolicy=Never bridge only once Flux applies its exact HelmRelease tag, narrowing the
	# image-GC window even when a retained cluster still has stale release state.
	# shellcheck source=scripts/lib/demo-cluster.sh
	source "${ROOT_DIR}/scripts/lib/demo-cluster.sh"
	PROFILE=demo
	DEMO_TIMEOUT=15m
	CLUSTER_NAME=fgentic-demo-fixture
	SOURCE_CONTEXT="${WORK_DIR}/source-context"
	SOURCE_BASE_IMAGE='source-base:fixture'
	SOURCE_GIT_PACKAGES='git git-daemon busybox-extras'
	SOURCE_IMAGE='fgentic-demo-source:fixture'
	BRIDGE_IMAGE='matrix-a2a-bridge:fixture'
	mkdir -p "${SOURCE_CONTEXT}"
	early_image_calls=()
	build_image() {
		early_image_calls+=("build:$1")
	}
	k3d() {
		early_image_calls+=("k3d:$*")
	}
	resource_trace_require_volume_sample() { return 0; }
	prune_owned_host_images() {
		early_image_calls+=("prune:$1")
	}
	build_and_load_images
	[ "${early_image_calls[0]}" = 'build:fgentic-demo-source:fixture' ]
	[ "${early_image_calls[1]}" = 'build:matrix-a2a-bridge:fixture' ]
	[ "${early_image_calls[2]}" = \
		'k3d:image import --mode auto --cluster fgentic-demo-fixture fgentic-demo-source:fixture' ]
	[ "${early_image_calls[3]}" = 'prune:fgentic-demo-source' ]
	[ "${#early_image_calls[@]}" -eq 4 ]
	bridge_image_calls="${WORK_DIR}/bridge-image-calls"
	: >"${bridge_image_calls}"
	kubectl() {
		printf 'kubectl %s\n' "$*" >>"${bridge_image_calls}"
		if [[ "$*" == *'--namespace agentgateway-system get deployment federation-usage-receipt'* ]]; then
			printf '%s\n' '{"spec":{"template":{"spec":{"containers":[{"name":"usage-receipt","image":"matrix-a2a-bridge:fixture"}]}}}}'
			return
		fi
		case "$(wc -l <"${bridge_image_calls}")" in
		1)
			printf '%s\n' '{"spec":{"values":{"image":{"repository":"matrix-a2a-bridge","tag":"stale"}}}}'
			;;
		2) return 1 ;;
		*)
			printf '%s\n' '{"spec":{"values":{"image":{"repository":"matrix-a2a-bridge","tag":"fixture"}}}}'
			;;
		esac
	}
	k3d() {
		printf 'k3d %s\n' "$*" >>"${bridge_image_calls}"
	}
	bridge_image_loaded=false
	for _ in 1 2 3 4; do
		if [ "${bridge_image_loaded}" = false ] && load_bridge_image_if_requested; then
			bridge_image_loaded=true
		fi
	done
	[ "${bridge_image_loaded}" = true ]
	[ "$(rg --count '^kubectl ' "${bridge_image_calls}")" -eq 3 ]
	[ "$(rg --count '^k3d ' "${bridge_image_calls}")" -eq 1 ]
	[ "$(rg --count --fixed-strings \
		'kubectl --namespace bridge get helmrelease matrix-a2a-bridge --output json' \
		"${bridge_image_calls}")" -eq 3 ]
	tail -n 1 "${bridge_image_calls}" |
		rg --fixed-strings --line-regexp \
			'k3d image import --mode auto --cluster fgentic-demo-fixture matrix-a2a-bridge:fixture' >/dev/null
	PROFILE=federation
	load_bridge_image_if_requested
	[ "$(rg --count '^kubectl ' "${bridge_image_calls}")" -eq 4 ]
	[ "$(rg --count '^k3d ' "${bridge_image_calls}")" -eq 2 ]
	tail -n 1 "${bridge_image_calls}" |
		rg --fixed-strings --line-regexp \
			'k3d image import --mode auto --cluster fgentic-demo-fixture matrix-a2a-bridge:fixture' >/dev/null

	# The real helper must distinguish a requested image whose containerd import fails.
	PROFILE=demo
	k3d() {
		printf 'k3d %s\n' "$*" >>"${bridge_image_calls}"
		return 1
	}
	if load_bridge_image_if_requested; then
		echo 'error: bridge image import failure was reported as success' >&2
		exit 1
	else
		[ "$?" -eq 2 ]
	fi
	[ "$(wc -l <"${bridge_image_calls}")" -eq 8 ]

	# A containerd/import failure is a terminal resource error, not a request-readiness retry.
	SOURCE_REVISION=fixture
	load_attempts=0
	load_bridge_image_if_requested() {
		load_attempts=$((load_attempts + 1))
		return 2
	}
	flux_calls=0
	flux() {
		flux_calls=$((flux_calls + 1))
	}
	if wait_for_platform >/dev/null 2>&1; then
		echo 'error: bridge image import failure did not stop reconciliation' >&2
		exit 1
	fi
	[ "${load_attempts}" -eq 1 ]
	[ "${flux_calls}" -eq 2 ]
)
rg --fixed-strings \
	'kubectl apply --server-side --force-conflicts \' \
	"${ROOT_DIR}/scripts/lib/demo-cluster.sh" >/dev/null || {
	echo 'error: demo bootstrap does not reclaim admission fields on cluster reuse' >&2
	exit 1
}
rg --fixed-strings -- \
	'--kustomize "${SNAPSHOT_DIR}/infra/policies"' \
	"${ROOT_DIR}/scripts/lib/demo-cluster.sh" >/dev/null || {
	echo 'error: demo bootstrap does not install admission before namespaces' >&2
	exit 1
}
rg --fixed-strings 'render_bootstrap_namespaces | kubectl apply --filename -' \
	"${ROOT_DIR}/scripts/lib/demo-cluster.sh" >/dev/null || {
	echo 'error: demo bootstrap does not apply the rendered Namespace-only stream' >&2
	exit 1
}
(
	# Exercise the exact lifecycle helper, not a parallel test-only composition. Bootstrap creates
	# only the Namespaces needed by CA/secrets; Flux later owns substituted quota admission.
	# shellcheck source=scripts/lib/demo-cluster.sh
	source "${ROOT_DIR}/scripts/lib/demo-cluster.sh"
	SNAPSHOT_DIR="${ROOT_DIR}"
	PROFILE=demo
	render_bootstrap_namespaces >"${WORK_DIR}/demo-bootstrap-namespaces.yaml"
	PROFILE=federation
	render_bootstrap_namespaces >"${WORK_DIR}/federation-bootstrap-namespaces.yaml"
)
(
	# Profile-specific tuning must remain a successful no-op for the ordinary demo, and an empty
	# controller query is also a successful no-op while Flux is between install transitions.
	# shellcheck source=scripts/lib/demo-cluster.sh
	source "${ROOT_DIR}/scripts/lib/demo-cluster.sh"
	PROFILE=demo
	kubectl() { fail 'federation tuning queried Kubernetes for the demo profile'; }
	configure_federation_flux_controllers
	configure_federation_metrics_server

	PROFILE=federation
	FEDERATION_CONSTRAINED=no
	FLUX_LEADER_ELECTION_LEASE_DURATION=180s
	FLUX_LEADER_ELECTION_RENEW_DEADLINE=170s
	FLUX_LEADER_ELECTION_RETRY_PERIOD=30s
	kubectl() { printf '{"items":[]}\n'; }
	configure_ephemeral_flux_controllers
	configure_federation_flux_controllers
)
for stream in demo-bootstrap-namespaces.yaml federation-bootstrap-namespaces.yaml; do
	if rg --fixed-strings '${quota_' "${WORK_DIR}/${stream}" >/dev/null; then
		echo "error: ${stream} contains unresolved quota substitution" >&2
		exit 1
	fi
	yq eval-all --exit-status -o=json \
		'[select(.kind != "Namespace")] | length == 0' "${WORK_DIR}/${stream}" >/dev/null || {
		echo "error: ${stream} contains a non-Namespace bootstrap object" >&2
		exit 1
	}
done
demo_bootstrap_names="$(
	yq eval-all -o=json '[select(.kind == "Namespace") | .metadata.name] | sort' \
		"${WORK_DIR}/demo-bootstrap-namespaces.yaml" | jq --compact-output .
)"
[ "${demo_bootstrap_names}" = \
	'["agentgateway-system","bridge","bridges","cert-manager","cnpg-system","gateway","kagent","keycloak","knowledge","matrix","models","monitoring","postgres"]' ] || {
	echo "error: demo bootstrap Namespace set drifted: ${demo_bootstrap_names}" >&2
	exit 1
}
federation_bootstrap_names="$(
	yq eval-all -o=json '[select(.kind == "Namespace") | .metadata.name] | sort' \
		"${WORK_DIR}/federation-bootstrap-namespaces.yaml" | jq --compact-output .
)"
[ "${federation_bootstrap_names}" = \
	'["agentgateway-system","cert-manager","cnpg-system","gateway","kagent","keycloak","knowledge","matrix","matrix-b","matrix-c","models","postgres"]' ] || {
	echo "error: federation bootstrap Namespace set drifted: ${federation_bootstrap_names}" >&2
	exit 1
}
rg --fixed-strings 'scripts/test-admission-policies.sh" --runtime' \
	"${ROOT_DIR}/scripts/lib/demo-cluster.sh" >/dev/null || {
	echo 'error: demo acceptance omits the admission runtime contract' >&2
	exit 1
}
rg --fixed-strings 'wait --for=create deployment/agentgateway-proxy' \
	"${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null || {
	echo 'error: demo seeding does not wait for the agentgateway data plane to exist' >&2
	exit 1
}
rg --fixed-strings 'rollout status deployment/agentgateway-proxy' \
	"${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null || {
	echo 'error: demo seeding does not wait for the agentgateway data plane' >&2
	exit 1
}
if (
	cd "${WORK_DIR}"
	env -u ROOT_DIR FGENTIC_CA_DIR="${WORK_DIR}/missing-ca" \
		"${ROOT_DIR}/scripts/seed-demo.sh"
) >"${WORK_DIR}/seed-startup.txt" 2>&1; then
	echo 'error: demo seeder unexpectedly started without a local CA' >&2
	exit 1
fi
rg --fixed-strings 'local CA certificate not found' "${WORK_DIR}/seed-startup.txt" >/dev/null || {
	echo 'error: demo seeder failed before validating its local CA dependency' >&2
	cat "${WORK_DIR}/seed-startup.txt" >&2
	exit 1
}
(
	# Validate the generated cluster config without creating a cluster. Every disposable profile
	# needs the explicit disk floor; federation alone moves ingress to its alternate loopback, and
	# only its explicit constrained capacity mode tunes the k3s process.
	# shellcheck source=scripts/lib/demo-config.sh
	source "${ROOT_DIR}/scripts/lib/demo-config.sh"
	CLUSTER_NAME=fgentic-demo-fixture
	FEDERATION_LOOPBACK=127.0.0.2
	PROFILE=demo
	render_k3d_config "${WORK_DIR}/demo-k3d.yaml"
	PROFILE=federation
	FEDERATION_CONSTRAINED=no
	render_k3d_config "${WORK_DIR}/federation-k3d.yaml"
	FEDERATION_CONSTRAINED=yes
	render_k3d_config "${WORK_DIR}/federation-constrained-k3d.yaml"
	for config in demo-k3d.yaml federation-k3d.yaml federation-constrained-k3d.yaml; do
		assert_yq \
			'.options.k3s.extraArgs[] |
        select(.arg == "--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<1Gi,imagefs.available<1Gi,nodefs.inodesFree<5%,imagefs.inodesFree<5%") |
        (.nodeFilters | ((length == 1) and (.[0] == "server:*")))' \
			"${WORK_DIR}/${config}" "${config} omits the disposable-cluster eviction floor"
		assert_yq \
			'[.options.k3s.extraArgs[] | select(.arg == "--disable-network-policy")] as $args |
        (
          ($args | length) == 1 and
          ($args | .[0].nodeFilters | length) == 1 and
          ($args | .[0].nodeFilters[0]) == "server:*" and
          ($args | .[0] | keys | length) == 2 and
          ($args | .[0] | has("arg")) and
          ($args | .[0] | has("nodeFilters"))
        )' \
			"${WORK_DIR}/${config}" \
			"${config} does not disable the failed local NetworkPolicy controller"
		audit_policy_source="$(yq -er '.files[] |
      select(.destination == "/etc/fgentic/audit-policy.yaml") | .source' \
			"${WORK_DIR}/${config}")"
		[[ "${audit_policy_source}" != /* ]] || {
			echo "error: ${config} embeds a host-specific audit-policy path" >&2
			exit 1
		}
		cmp "${ROOT_DIR}/infra/k3d-audit-policy.yaml" \
			"${WORK_DIR}/${audit_policy_source}" >/dev/null || {
			echo "error: ${config} has no resolvable audit-policy source" >&2
			exit 1
		}
	done
	assert_yq '.ports[0].port == "127.0.0.1:80:80" and .ports[1].port == "127.0.0.1:443:443"' \
		"${WORK_DIR}/demo-k3d.yaml" 'demo ingress ports changed'
	assert_yq '.ports[0].port == "127.0.0.2:80:80" and .ports[1].port == "127.0.0.2:443:443"' \
		"${WORK_DIR}/federation-k3d.yaml" 'federation ingress ports changed'
	assert_yq '.ports[0].port == "127.0.0.2:80:80" and .ports[1].port == "127.0.0.2:443:443"' \
		"${WORK_DIR}/federation-constrained-k3d.yaml" \
		'constrained federation ingress ports changed'
	for config in demo-k3d.yaml federation-k3d.yaml; do
		assert_yq \
			'[.env[]? | select((.envVar == "GOGC=50") or (.envVar == "GOMEMLIMIT=1GiB"))] | length == 0' \
			"${WORK_DIR}/${config}" "${config} unexpectedly tunes the k3s Go runtime"
	done
	assert_yq '
      ((.env | length) == 2) and
      (.env[0].envVar == "GOGC=50") and
      (.env[0].nodeFilters[0] == "server:*") and
      (.env[1].envVar == "GOMEMLIMIT=1GiB") and
      (.env[1].nodeFilters[0] == "server:*")
    ' "${WORK_DIR}/federation-constrained-k3d.yaml" \
		'constrained federation config omits the k3s Go runtime budget'
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
"${DEV}" --help >"${WORK_DIR}/dev-help.txt"
rg --fixed-strings 'temporary kubeconfig' "${WORK_DIR}/dev-help.txt" >/dev/null
rg --fixed-strings 'never reads, changes, or switches' "${WORK_DIR}/dev-help.txt" >/dev/null
rg --fixed-strings "exec \"\${ROOT_DIR}/scripts/demo.sh\" up" "${DEV}" >/dev/null
yq --input-format toml --output-format yaml --exit-status '
  .tools.watchexec == "2.5.1" and
  (.tasks.watch.depends | length) == 1 and .tasks.watch.depends[0] == "dev:up" and
  (.tasks.watch.run | contains("mise run dev:reload")) and
  .tasks."dev:up".run == "bash scripts/dev.sh up" and
  .tasks."dev:reload".run == "bash scripts/dev.sh reload" and
  .tasks."dev:stop".run == "bash scripts/dev.sh stop"
' "${ROOT_DIR}/mise.toml" >/dev/null || {
	echo 'error: repo-owned development tasks are incomplete' >&2
	exit 1
}
[ ! -e "${ROOT_DIR}/skaffold.yaml" ] || {
	echo 'error: obsolete Skaffold configuration is still present' >&2
	exit 1
}
if FGENTIC_DEMO_CLUSTER=fgentic "${DEMO}" down \
	>"${WORK_DIR}/reserved-cluster.txt" 2>&1; then
	echo 'error: demo teardown accepted the reserved fgentic cluster name' >&2
	exit 1
fi
rg --fixed-strings 'must be fgentic-demo' "${WORK_DIR}/reserved-cluster.txt" >/dev/null

dev_fake_bin="${WORK_DIR}/dev-fake-bin"
dev_fake_state="${WORK_DIR}/dev-fake-state"
mkdir -p "${dev_fake_bin}" "${dev_fake_state}"
cat >"${dev_fake_bin}/docker" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-}" in
info) ;;
inspect) printf '%s\n' "${FAKE_DEV_OWNER:-true}" ;;
build) printf 'docker %s\n' "$*" >>"${FAKE_DEV_COMMANDS:?}" ;;
*) exit 2 ;;
esac
EOF
cat >"${dev_fake_bin}/k3d" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
case "${1:-} ${2:-}" in
"cluster list") printf '[{"name":"fgentic-demo","serversRunning":1,"serversCount":1}]\n' ;;
"cluster start" | "cluster stop") printf 'k3d %s\n' "$*" >>"${FAKE_DEV_COMMANDS:?}" ;;
"kubeconfig get")
	printf 'k3d %s\n' "$*" >>"${FAKE_DEV_COMMANDS:?}"
	printf 'apiVersion: v1\nkind: Config\n'
	;;
"image import") printf 'k3d %s\n' "$*" >>"${FAKE_DEV_COMMANDS:?}" ;;
*) exit 2 ;;
esac
EOF
cat >"${dev_fake_bin}/kubectl" <<'EOF'
#!/usr/bin/env bash
set -euo pipefail
printf 'kubectl KUBECONFIG=%s %s\n' "${KUBECONFIG:-unset}" "$*" >>"${FAKE_DEV_COMMANDS:?}"
case "$*" in
*'get helmrelease matrix-a2a-bridge --output json')
	printf '%s\n' '{"spec":{"values":{"image":{"repository":"matrix-a2a-bridge","tag":"demo-fixture","pullPolicy":"Never"}}}}'
	;;
*'get secret fgentic-demo-bootstrap'*) printf 'fixture-password' ;;
*'llm_provider'*) printf 'demo' ;;
*'llm_model'*) printf 'fgentic-demo' ;;
*'jsonpath={.spec.template.spec.containers[0].image}'*) printf 'matrix-a2a-bridge:demo-fixture' ;;
*'jsonpath={.status.readyReplicas}'*) printf '1' ;;
esac
EOF
chmod +x "${dev_fake_bin}/docker" "${dev_fake_bin}/k3d" "${dev_fake_bin}/kubectl"

: >"${dev_fake_state}/commands"
PATH="${dev_fake_bin}:${PATH}" \
	FAKE_DEV_COMMANDS="${dev_fake_state}/commands" \
	FGENTIC_CA_DIR="${WORK_DIR}/portable-ca" \
	"${ROOT_DIR}/scripts/local-ca.sh" >"${dev_fake_state}/local-ca.txt" 2>&1
openssl x509 --in "${WORK_DIR}/portable-ca/ca.crt" --noout --text |
	rg --fixed-strings 'CA:TRUE' >/dev/null || {
	echo 'error: portable local CA generation omitted the CA constraint' >&2
	exit 1
}
rg --fixed-strings 'update-ca-certificates' "${dev_fake_state}/local-ca.txt" >/dev/null
rg --fixed-strings 'security add-trusted-cert' "${ROOT_DIR}/scripts/local-ca.sh" >/dev/null

run_dev_fixture() {
	local action="$1"
	local owner="${2:-true}"
	: >"${dev_fake_state}/commands"
	PATH="${dev_fake_bin}:${PATH}" \
		KUBECONFIG="${WORK_DIR}/must-not-be-used" \
		FAKE_DEV_COMMANDS="${dev_fake_state}/commands" \
		FAKE_DEV_OWNER="${owner}" \
		"${DEV}" "${action}" >"${dev_fake_state}/${action}-${owner}.txt" 2>&1
}

run_dev_fixture up
rg --fixed-strings 'k3d cluster start fgentic-demo' "${dev_fake_state}/commands" >/dev/null
if rg --fixed-strings 'docker build' "${dev_fake_state}/commands" >/dev/null; then
	echo 'error: reusing the development cluster rebuilt the bridge' >&2
	exit 1
fi
if rg --fixed-strings "${WORK_DIR}/must-not-be-used" "${dev_fake_state}/commands" >/dev/null; then
	echo 'error: the development loop used the caller Kubernetes context' >&2
	exit 1
fi
rg --regexp 'kubectl KUBECONFIG=.*/fgentic-dev-kubeconfig\.[^ ]+' \
	"${dev_fake_state}/commands" >/dev/null

run_dev_fixture reload
rg --fixed-strings \
	'docker build --quiet --tag matrix-a2a-bridge:demo-fixture --label dev.fgentic.demo.cluster=fgentic-demo' \
	"${dev_fake_state}/commands" >/dev/null
rg --fixed-strings \
	'k3d image import --mode auto --cluster fgentic-demo matrix-a2a-bridge:demo-fixture' \
	"${dev_fake_state}/commands" >/dev/null
rg --fixed-strings \
	'rollout restart deployment/matrix-a2a-bridge' "${dev_fake_state}/commands" >/dev/null
rg --fixed-strings \
	'rollout status deployment/matrix-a2a-bridge --timeout=2m' \
	"${dev_fake_state}/commands" >/dev/null

run_dev_fixture stop
rg --fixed-strings 'k3d cluster stop fgentic-demo' "${dev_fake_state}/commands" >/dev/null

dev_receipt_dir="${dev_fake_state}/lifecycle/cluster-teardown"
mkdir -p "${dev_receipt_dir}"
jq --null-input '{
  schema: "fgentic.cluster-teardown.v1",
  profile: "demo",
  cluster: "fgentic-demo",
  owner: "true",
  generation: "container-server-id",
  containers: [{id: "container-server-id", name: "k3d-fgentic-demo-server-0"}],
  network: {id: "network-id", name: "k3d-fgentic-demo", cluster_label: "fgentic-demo"},
  volumes: [{
    name: "k3d-fgentic-demo-images",
    created_at: "2026-07-15T00:00:00Z",
    kind: "images",
    attachments: ["container-server-id"]
  }],
  images: []
}' >"${dev_receipt_dir}/fgentic-demo.json"
for action in up reload stop; do
	: >"${dev_fake_state}/commands"
	if PATH="${dev_fake_bin}:${PATH}" \
		KUBECONFIG="${WORK_DIR}/must-not-be-used" \
		FAKE_DEV_COMMANDS="${dev_fake_state}/commands" \
		FGENTIC_DEMO_STATE_DIR="${dev_fake_state}/lifecycle" \
		"${DEV}" "${action}" >"${dev_fake_state}/${action}-pending.txt" 2>&1; then
		echo "error: dev:${action} ignored pending teardown recovery" >&2
		exit 1
	fi
	rg --fixed-strings 'teardown recovery is pending' \
		"${dev_fake_state}/${action}-pending.txt" >/dev/null
	[ ! -s "${dev_fake_state}/commands" ] || {
		echo "error: dev:${action} mutated pending teardown state" >&2
		exit 1
	}
done
[ -f "${dev_receipt_dir}/fgentic-demo.json" ] || {
	echo 'error: blocked development action cleared the teardown receipt' >&2
	exit 1
}

: >"${dev_fake_state}/commands"
if PATH="${dev_fake_bin}:${PATH}" FAKE_DEV_COMMANDS="${dev_fake_state}/commands" \
	FAKE_DEV_OWNER=foreign "${DEV}" reload >"${dev_fake_state}/foreign.txt" 2>&1; then
	echo 'error: the development loop accepted a foreign cluster' >&2
	exit 1
fi
rg --fixed-strings 'refusing to manage fgentic-demo' "${dev_fake_state}/foreign.txt" >/dev/null
if rg --regexp 'cluster start|docker build|image import|rollout restart' \
	"${dev_fake_state}/commands" >/dev/null; then
	echo 'error: the development loop mutated a foreign cluster' >&2
	exit 1
fi

"${ROOT_DIR}/scripts/test-demo-teardown.sh"

kubectl kustomize "${ROOT_DIR}/clusters/demo" >"${WORK_DIR}/cluster.yaml"
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "platform-settings") |
    .data.llm_provider == "demo" and
    .data.llm_model == "fgentic-demo" and
    .data.demo_bridge_tag == "local" and
    .data.mas_local_login_enabled == "true" and
    .data.admin_console == "disabled" and
    .data.llm_usage_budget_15m == "100000"' \
	"${WORK_DIR}/cluster.yaml" 'demo platform settings are incomplete'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "agentgateway-provider") |
    .spec.path == "./infra/agentgateway/providers/profiles/demo"' \
	"${WORK_DIR}/cluster.yaml" 'provider-selection did not select the demo inventory'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "admin") |
    .spec.path == "./infra/admin/profiles/disabled"' \
	"${WORK_DIR}/cluster.yaml" 'demo must select the zero-footprint admin profile'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "gateway") |
    .spec.path == "./infra/gateway/profiles/disabled"' \
	"${WORK_DIR}/cluster.yaml" 'demo must select the gateway without an admin listener'
expected_demo_layers=$'admin\nagentgateway\nagentgateway-provider\nbridge\ncontrollers\ngateway\nkagent\nmatrix\nnamespaces\nplatform-secrets\npolicies\npostgres'
actual_demo_layers="$(
	yq eval-all -N -r 'select(.kind == "Kustomization") | .metadata.name' \
		"${WORK_DIR}/cluster.yaml" | sort
)"
[[ "${actual_demo_layers}" == "${expected_demo_layers}" ]] || {
	echo "error: small-profile Flux inventory drifted: ${actual_demo_layers}" >&2
	exit 1
}
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "namespaces") |
    ((.spec.patches | length) == 3 and
     ([.spec.patches[] | select(
       .target.kind == "Namespace" and
       .target.name == "trivy-system" and
       (.patch | contains("$patch: delete"))
     )] | length) == 1 and
     ([.spec.patches[] | select(
       .target.kind == "ResourceQuota" and
       .target.name == "compute-budget" and
       .target.namespace == "trivy-system" and
       (.patch | contains("$patch: delete"))
     )] | length) == 1 and
     ([.spec.patches[] | select(
       .target.kind == "LimitRange" and
       .target.name == "container-defaults" and
       .target.namespace == "trivy-system" and
       (.patch | contains("$patch: delete"))
     )] | length) == 1)' \
	"${WORK_DIR}/cluster.yaml" 'demo namespace inventory still owns Trivy resources'
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "platform-secrets") |
    .spec.path == "./clusters/demo/empty" and (.spec.decryption == null)' \
	"${WORK_DIR}/cluster.yaml" 'demo secret inventory must be empty and non-SOPS'

render_demo_layer postgres "${ROOT_DIR}/infra/postgres"
assert_yq \
	'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
    .spec.monitoring.enablePodMonitor == false' \
	"${WORK_DIR}/demo-postgres.yaml" 'demo Postgres still enables its PodMonitor'
if yq eval-all -N -r \
	'select(.kind == "ScheduledBackup" or
    (.kind == "Database" and .metadata.name == "keycloak")) |
    .metadata.name' "${WORK_DIR}/demo-postgres.yaml" | rg --quiet '.'; then
	echo 'error: demo Postgres retains a backup or Keycloak database dependency' >&2
	exit 1
fi

render_demo_layer agentgateway "${ROOT_DIR}/infra/agentgateway"
if yq eval-all -N -r \
	'select(.kind == "AgentgatewayPolicy" and .metadata.name == "tracing") |
    .metadata.name' "${WORK_DIR}/demo-agentgateway.yaml" | rg --quiet '.'; then
	echo 'error: demo agentgateway still references the tracing backend' >&2
	exit 1
fi

render_demo_layer kagent "${ROOT_DIR}/infra/kagent"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "kagent") |
    .spec.values.otel.tracing.enabled == false and
    .spec.values.ui.replicas == 0 and
    .spec.values.kmcp.enabled == false' \
	"${WORK_DIR}/demo-kagent.yaml" 'demo kagent resource-budget patch is ineffective'
expected_demo_agents=$'docs-qa\nplatform-helper\nscribe'
actual_demo_agents="$(
	yq eval-all -N -r 'select(.kind == "Agent") | .metadata.name' \
		"${WORK_DIR}/demo-kagent.yaml" | sort
)"
[[ "${actual_demo_agents}" == "${expected_demo_agents}" ]] || {
	echo "error: small profile must retain exactly the three mapped Agents" >&2
	exit 1
}

render_demo_layer bridge "${ROOT_DIR}/apps/matrix-a2a-bridge/deploy"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") |
    .spec.values.image.repository == "matrix-a2a-bridge" and
    .spec.values.image.tag == "local" and
    .spec.values.image.pullPolicy == "Never" and
    .spec.values.config.otelEndpoint == "" and
    .spec.values.metrics.podMonitor.enabled == false' \
	"${WORK_DIR}/demo-bridge.yaml" 'demo bridge resource patch is ineffective'
yq eval-all -o=yaml \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") |
    .spec.values' \
	"${WORK_DIR}/demo-bridge.yaml" |
	helm template matrix-a2a-bridge "${ROOT_DIR}/apps/matrix-a2a-bridge/chart" \
		--namespace bridge \
		--values - >"${WORK_DIR}/demo-bridge-chart.yaml"
assert_yq \
	'select(.kind == "ConfigMap" and .metadata.name == "matrix-a2a-bridge-agents") |
    .data."agents.yaml" | from_yaml | .agents as $agents |
    (($agents | length) == 3 and
     ($agents | has("agent-docs-qa")) and
     ($agents | has("agent-platform-helper")) and
     ($agents | has("agent-scribe")) and
     $agents."agent-docs-qa".name == "docs-qa" and
     $agents."agent-platform-helper".name == "platform-helper" and
     $agents."agent-scribe".name == "scribe" and
     ([$agents[] | select(
       .namespace == "kagent" and
       (.allowedSenders | length) == 1 and
       .allowedSenders[0] == "@alice:fgentic.localhost"
     )] | length) == 3)' \
	"${WORK_DIR}/demo-bridge-chart.yaml" \
	'demo bridge must retain exactly the three local kagent mappings'

kubectl kustomize "${ROOT_DIR}/infra/agentgateway" >"${WORK_DIR}/agentgateway.yaml"
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-default-deny-ingress") |
    (((.spec.podSelector | tag) == "!!map") and
     ((.spec.podSelector | length) == 0) and
     ((.spec.policyTypes | length) == 1) and
     (.spec.policyTypes[0] == "Ingress") and
     (((.spec | has("ingress")) | not)))' \
	"${WORK_DIR}/agentgateway.yaml" 'agentgateway namespace does not fail closed for ingress'
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-allow-agents") |
    (.spec.podSelector.matchLabels."app.kubernetes.io/name" == "agentgateway-proxy" and
     .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "bridge" and
     .spec.ingress[0].from[1].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "kagent" and
     .spec.ingress[0].ports[0].port == 8080 and .spec.ingress[0].ports[0].protocol == "TCP" and
     (.spec.ingress[0].from | length) == 2 and
     (.spec.ingress[0].ports | length) == 1 and
     .spec.ingress[1].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "monitoring" and
     .spec.ingress[1].ports[0].port == 15020 and .spec.ingress[1].ports[0].protocol == "TCP" and
     (.spec.ingress[1].from | length) == 1 and
     (.spec.ingress[1].ports | length) == 1 and
     (.spec.ingress | length) == 2)' \
	"${WORK_DIR}/agentgateway.yaml" 'agentgateway data-plane ingress is not port scoped'
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-allow-xds") |
    (.spec.podSelector.matchLabels."app.kubernetes.io/name" == "agentgateway" and
     .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "agentgateway-system" and
     .spec.ingress[0].from[0].podSelector.matchLabels."app.kubernetes.io/name" == "agentgateway-proxy" and
     .spec.ingress[0].ports[0].port == 9978 and .spec.ingress[0].ports[0].protocol == "TCP" and
     (.spec.ingress[0].from | length) == 1 and
     (.spec.ingress[0].ports | length) == 1 and
     (.spec.ingress | length) == 1)' \
	"${WORK_DIR}/agentgateway.yaml" 'agentgateway xDS ingress is not restricted to its proxy'

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
	"! kustomizations=\"\$(kubectl --request-timeout=10s --namespace flux-system \\" \
	"! helmreleases=\"\$(kubectl --request-timeout=10s get helmreleases \\"; do
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
for contract in \
	'all_probes_have_replies' \
	'probe_event_ids' \
	'/_matrix/client/v3/rooms/${encoded_room}/context/${encoded_event}?limit=50' \
	'.source_revision == $source_revision' \
	'Fgentic'"'"'s deterministic evaluation model is working through agentgateway and kagent.' \
	'.content.msgtype == "m.notice"' \
	'source_revision: $source_revision'; do
	rg --fixed-strings -- "${contract}" "${DEMO_SEED_SOURCES[@]}" >/dev/null || {
		echo "error: demo seeder omits the per-agent reply contract: ${contract}" >&2
		exit 1
	}
done
if rg --fixed-strings 'first_ghost=' "${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null; then
	echo 'error: demo seeder still proves only the first mapped agent' >&2
	exit 1
fi
if rg --fixed-strings '/send/m.room.message/demo-${ghost}' \
	"${ROOT_DIR}/scripts/seed-demo.sh" >/dev/null; then
	echo 'error: demo seeder puts an unescaped ghost localpart in a Matrix transaction path' >&2
	exit 1
fi

reply_fixture "${EXPECTED_DEMO_REPLY}" | reply_fixture_matches || {
	echo 'error: demo reply predicate rejected the exact deterministic model response' >&2
	exit 1
}
replacement_fixture "${EXPECTED_DEMO_REPLY}" | reply_fixture_matches || {
	echo 'error: demo reply predicate rejected the streamed Matrix replacement response' >&2
	exit 1
}
replacement_fixture 'A useful model response.' |
	reply_fixture_matches vertex google/gemini-2.5-flash || {
	echo 'error: demo reply predicate rejected a successful configured-model replacement' >&2
	exit 1
}
if replacement_fixture "${EXPECTED_DEMO_REPLY}" \
	'@agent-docs-qa:fgentic.localhost' '@agent-scribe:fgentic.localhost' |
	reply_fixture_matches; then
	echo 'error: demo reply predicate accepted a replacement from the wrong ghost' >&2
	exit 1
fi
if replacement_fixture "${EXPECTED_DEMO_REPLY}" \
	'@agent-docs-qa:fgentic.localhost' '@agent-docs-qa:fgentic.localhost' '$other' |
	reply_fixture_matches; then
	echo 'error: demo reply predicate accepted a replacement for another event' >&2
	exit 1
fi
if replacement_fixture "${EXPECTED_DEMO_REPLY}" \
	'@agent-docs-qa:fgentic.localhost' '@agent-docs-qa:fgentic.localhost' '$reply' 'm.text' |
	reply_fixture_matches; then
	echo 'error: demo reply predicate accepted a replacement with the wrong message type' >&2
	exit 1
fi
if reply_fixture '--- BEGIN FGENTIC BRIDGE PROVENANCE ---' |
	reply_fixture_matches vertex google/gemini-2.5-flash; then
	echo 'error: demo reply predicate accepted the streaming provenance envelope' >&2
	exit 1
fi
if replacement_fixture '⚠️ agent failed after starting.' \
	'@agent-docs-qa:fgentic.localhost' '@agent-docs-qa:fgentic.localhost' \
	'$reply' 'm.notice' 'Processing request' |
	reply_fixture_matches vertex google/gemini-2.5-flash; then
	echo 'error: demo reply predicate accepted a placeholder before a terminal failure' >&2
	exit 1
fi
for rejected_reply in \
	'⚠️ could not reach agent "agent-docs-qa" — see the bridge logs.' \
	'🛑 canceled by @alice:fgentic.localhost.' \
	'⏳ working on it…' \
	'(the agent returned no content)' \
	'   '; do
	if reply_fixture "${rejected_reply}" |
		reply_fixture_matches vertex google/gemini-2.5-flash; then
		echo "error: demo reply predicate accepted a non-success notice: ${rejected_reply}" >&2
		exit 1
	fi
done
if jq --null-input --compact-output '{events_before: [], events_after: []}' |
	reply_fixture_matches; then
	echo 'error: demo reply predicate accepted a missing reply' >&2
	exit 1
fi

ghosts_json='["agent-docs-qa","agent-platform-helper","agent-scribe"]'
seed_state='{"version":2,"provider":"demo","model":"fgentic-demo","source_revision":"main@sha1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","probe_event_ids":{"agent-docs-qa":"$1","agent-platform-helper":"$2","agent-scribe":"$3"}}'
seed_state_matches() {
	local source_revision="$1"
	jq --exit-status --arg provider demo --arg model fgentic-demo \
		--arg source_revision "${source_revision}" --argjson ghosts "${ghosts_json}" \
		--from-file "${SEED_STATE_FILTER}" >/dev/null <<<"${seed_state}"
}
seed_state_matches 'main@sha1:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa' || {
	echo 'error: demo seed-state predicate rejected the reconciled source revision' >&2
	exit 1
}
if seed_state_matches 'main@sha1:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb'; then
	echo 'error: demo seed-state predicate reused proof from an older source revision' >&2
	exit 1
fi

if rg -n 'mas_password_login_enabled|llm_token_budget_15m' \
	"${ROOT_DIR}/clusters/demo" "${DEMO_SOURCES[@]}"; then
	echo 'error: demo path uses a retired platform-setting name' >&2
	exit 1
fi

echo 'Demo install contracts passed.'
