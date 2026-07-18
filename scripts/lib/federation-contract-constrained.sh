#!/usr/bin/env bash
# Definition-only constrained-host contracts sourced by scripts/test-federation.sh.

assert_yq_all() {
	local expression="$1"
	local document="$2"
	local message="$3"
	yq eval-all --exit-status "${expression}" "${document}" >/dev/null || fail "${message}"
}

assert_federation_env_rejected() {
	local label="$1"
	local action="$2"
	local expected="$3"
	shift 3
	local output="${WORK_DIR}/invalid-${label}.txt"

	if env PATH="${WORK_DIR}/constrained-bin:${PATH}" "$@" \
		"${LIFECYCLE}" "${action}" >"${output}" 2>&1; then
		fail "federation lifecycle accepted invalid ${label} configuration"
	fi
	rg --fixed-strings "${expected}" "${output}" >/dev/null \
		|| fail "federation lifecycle did not explain invalid ${label} configuration"
	if rg --fixed-strings 'offline guard reached' "${output}" >/dev/null; then
		fail "federation lifecycle consulted the runtime before validating ${label} configuration"
	fi
}

render_federation_contract() {
	local overlay="$1"
	local stem="$2"

	kubectl kustomize "${overlay}" >"${WORK_DIR}/${stem}-cluster.yaml"
	flux build kustomization cluster-overlay-validation \
		--path "${overlay}" \
		--kustomization-file "${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml" \
		--dry-run --in-memory-build --strict-substitute --recursive \
		--local-sources "GitRepository/flux-system/flux-system=${ROOT_DIR}" \
		>"${WORK_DIR}/${stem}-recursive.yaml"
}

write_federation_inventory() {
	local manifest="$1"
	local output="$2"
	yq eval-all -o=json '
    [.] |
    map(select(.apiVersion != null and .kind != null and .metadata.name != null)) |
    map([.apiVersion, .kind, (.metadata.namespace // ""), .metadata.name]) |
    sort
  ' "${manifest}" >"${output}"
}

extract_demo_functions() {
	local output="$1"
	local function_name
	shift
	: >"${output}"
	for function_name in "$@"; do
		awk -v signature="${function_name}() {" '
      $0 == signature { copying = 1 }
      copying { print }
      copying && $0 == "}" { exit }
    ' "${DEMO_CLUSTER}" >>"${output}"
		printf '\n' >>"${output}"
		rg --fixed-strings --line-regexp "${function_name}() {" "${output}" >/dev/null \
			|| fail "could not extract ${function_name} for the federation contract"
	done
}

check_federation_constrained_cli() {
	local command
	mkdir -p "${WORK_DIR}/constrained-bin"
	for command in docker jq k3d kubectl; do
		printf '#!/usr/bin/env bash\necho "error: offline guard reached %s" >&2\nexit 99\n' \
			"${command}" >"${WORK_DIR}/constrained-bin/${command}"
		chmod +x "${WORK_DIR}/constrained-bin/${command}"
	done

	"${LIFECYCLE}" --help >"${WORK_DIR}/constrained-help.txt"
	for contract in \
		'up|status|stop|down' \
		'FGENTIC_FED_CONSTRAINED' \
		'FGENTIC_FED_NO_PROGRESS_TIMEOUT' \
		'FGENTIC_FED_MAX_TIMEOUT' \
		'FGENTIC_FED_TRACE' \
		'retaining the exact owned cluster'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/constrained-help.txt" >/dev/null \
			|| fail "federation help omits ${contract}"
	done

	yq -p toml -o yaml --exit-status \
		'.tasks."fed:up:constrained".run ==
      "FGENTIC_FED_CONSTRAINED=yes bash scripts/federation.sh up"' \
		"${ROOT_DIR}/mise.toml" >/dev/null || fail 'mise task fed:up:constrained is missing or unsafe'
	yq -p toml -o yaml --exit-status \
		'.tasks."fed:status".run == "bash scripts/federation.sh status"' \
		"${ROOT_DIR}/mise.toml" >/dev/null || fail 'mise task fed:status is missing or unsafe'
	yq -p toml -o yaml --exit-status \
		'.tasks."fed:stop".run == "bash scripts/federation.sh stop"' \
		"${ROOT_DIR}/mise.toml" >/dev/null || fail 'mise task fed:stop is missing or unsafe'

	assert_federation_env_rejected constrained-value status \
		'FGENTIC_FED_CONSTRAINED must be yes or no' \
		FGENTIC_FED_CONSTRAINED=maybe
	assert_federation_env_rejected trace-value status \
		'FGENTIC_FED_TRACE must be yes or no' \
		FGENTIC_FED_TRACE=maybe
	assert_federation_env_rejected no-progress-value up \
		'invalid FGENTIC_FED_NO_PROGRESS_TIMEOUT' \
		FGENTIC_FED_CONSTRAINED=yes FGENTIC_FED_NO_PROGRESS_TIMEOUT=0m
	assert_federation_env_rejected max-timeout-value up \
		'invalid FGENTIC_FED_MAX_TIMEOUT' \
		FGENTIC_FED_CONSTRAINED=yes FGENTIC_FED_MAX_TIMEOUT=forever
	assert_federation_env_rejected timeout-order up \
		'FGENTIC_FED_NO_PROGRESS_TIMEOUT must be shorter than FGENTIC_FED_MAX_TIMEOUT' \
		FGENTIC_FED_CONSTRAINED=yes FGENTIC_FED_NO_PROGRESS_TIMEOUT=20m \
		FGENTIC_FED_MAX_TIMEOUT=20m
}

check_federation_constrained_lifecycle_guards() {
	local artifact_line create_line guard_line ownership_line stop_line trace_line
	awk '/^require_owned_evaluation_cluster\(\)/,/^}/' "${DEMO_CLUSTER}" \
		>"${WORK_DIR}/owned-cluster-guard.sh"
	awk '/^demo_status\(\)/,/^}/' "${DEMO_CLUSTER}" >"${WORK_DIR}/demo-status.sh"
	awk '/^demo_stop\(\)/,/^}/' "${DEMO_CLUSTER}" >"${WORK_DIR}/demo-stop.sh"
	awk '/^demo_up\(\)/,/^}/' "${DEMO_CLUSTER}" >"${WORK_DIR}/demo-up.sh"
	awk '/^configure_federation_flux_controllers\(\)/,/^}/' "${DEMO_CLUSTER}" \
		>"${WORK_DIR}/federation-flux.sh"
	awk '/^configure_federation_metrics_server\(\)/,/^}/' "${DEMO_CLUSTER}" \
		>"${WORK_DIR}/federation-metrics-server.sh"

	for contract in cluster_exists cluster_owned_by_demo cluster_volume_identity; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/owned-cluster-guard.sh" >/dev/null \
			|| fail "shared lifecycle ownership guard omits ${contract}"
	done
	for contract in require_owned_evaluation_cluster cluster_running_container_ids \
		cluster_retained_storage_bytes; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/demo-status.sh" >/dev/null \
			|| fail "federation status omits ${contract}"
	done
	for contract in require_owned_evaluation_cluster cluster_running_container_ids \
		cluster_volume_identity 'k3d cluster stop' 'after_identity' 'before_identity'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/demo-stop.sh" >/dev/null \
			|| fail "federation stop omits ${contract}"
	done
	guard_line="$(rg --line-number --fixed-strings 'require_owned_evaluation_cluster' \
		"${WORK_DIR}/demo-stop.sh" | cut -d: -f1)"
	stop_line="$(rg --line-number --fixed-strings 'k3d cluster stop' \
		"${WORK_DIR}/demo-stop.sh" | cut -d: -f1)"
	((guard_line < stop_line)) \
		|| fail 'federation stop mutates the cluster before proving ownership and volume identity'
	for contract in 'PROFILE' 'FEDERATION_CONSTRAINED' 'GOMAXPROCS' 'GOGC' '25' 'GOMEMLIMIT' \
		'256Mi' 'requests.memory' 'limits.memory' '1Gi' '64Mi' '"$patch":"delete"' \
		'value":null' 'rollout status'; do
		rg --fixed-strings "${contract}" "${WORK_DIR}/federation-flux.sh" >/dev/null \
			|| fail "federation Flux profile lifecycle omits ${contract}"
	done
	rg --fixed-strings 'configure_federation_flux_controllers' "${WORK_DIR}/demo-up.sh" >/dev/null \
		|| fail 'federation up does not apply the Flux runtime profile lifecycle'
	for contract in 'PROFILE' 'FEDERATION_CONSTRAINED' 'desired=1' 'desired=0' \
		'scale deployment metrics-server' '--replicas "${desired}"' 'wait --for=delete pod' \
		'rollout status deployment/metrics-server'; do
		rg --fixed-strings -- "${contract}" "${WORK_DIR}/federation-metrics-server.sh" \
			>/dev/null || fail "federation metrics-server lifecycle omits ${contract}"
	done
	rg --fixed-strings 'configure_federation_metrics_server' "${WORK_DIR}/demo-up.sh" >/dev/null \
		|| fail 'federation up does not apply the metrics-server profile lifecycle'
	rg --fixed-strings 'prune_stale_node_images "${SOURCE_IMAGE}"' \
		"${WORK_DIR}/demo-up.sh" >/dev/null \
		|| fail 'federation reuse does not prune stale node-side source images after rollout'
	trace_line="$(rg --line-number --fixed-strings 'resource_trace_start' \
		"${WORK_DIR}/demo-up.sh" | cut -d: -f1)"
	artifact_line="$(rg --line-number --fixed-strings 'cluster_artifacts_exist' \
		"${WORK_DIR}/demo-up.sh" | cut -d: -f1)"
	ownership_line="$(rg --line-number --fixed-strings 'cluster_owned_by_demo' \
		"${WORK_DIR}/demo-up.sh" | cut -d: -f1)"
	create_line="$(rg --line-number --fixed-strings 'k3d cluster create' \
		"${WORK_DIR}/demo-up.sh" | cut -d: -f1)"
	((artifact_line < trace_line && ownership_line < trace_line && trace_line < create_line)) \
		|| fail 'federation boot does not preflight ownership and orphans before tracing and creation'
	for contract in '--request-timeout="${request_timeout}"' 'deadline_timeout' \
		'sleep_before_deadline' 'deadline_diagnostic_timeout' \
		'absolute ${FEDERATION_MAX_TIMEOUT} cap" 0s'; do
		rg --fixed-strings -- "${contract}" "${DEMO_CLUSTER}" >/dev/null \
			|| fail "constrained hard deadline omits ${contract}"
	done
}

check_federation_constrained_state_transitions() {
	local expected_revision='main@sha1:deadbeef'
	local helmreleases kustomizations milestones
	local flux_patches="${WORK_DIR}/federation-flux-patches.jsonl"
	local metrics_events="${WORK_DIR}/federation-metrics-events.txt"

	awk '/^collect_platform_milestones\(\)/,/^}/' "${DEMO_CLUSTER}" \
		>"${WORK_DIR}/collect-platform-milestones.sh"
	(
		source "${WORK_DIR}/federation-flux.sh"
		kubectl() {
			local argument
			if [[ "$*" == *' get deployments '* ]]; then
				jq --null-input '{items: [{metadata: {name: "source-controller"}}]}'
				return
			fi
			if [[ "$*" == *' patch deployment '* ]]; then
				while (($# > 0)); do
					argument="$1"
					shift
					if [ "${argument}" = '--patch' ]; then
						printf '%s\n' "$1" >>"${flux_patches}"
						return
					fi
				done
			fi
		}
		PROFILE=federation
		FEDERATION_CONSTRAINED=yes
		configure_federation_flux_controllers
		FEDERATION_CONSTRAINED=no
		configure_federation_flux_controllers
	)
	jq --slurp --exit-status '
    (length == 2) and (.[0].spec.template.spec.containers[0] as $constrained |
      .[1].spec.template.spec.containers[0] as $default |
	      $constrained.resources.requests.memory == "64Mi" and
      $constrained.resources.limits.memory == "256Mi" and
      any($constrained.env[]; .name == "GOMAXPROCS" and .value == "1") and
      any($constrained.env[]; .name == "GOGC" and .value == "25") and
      any($constrained.env[]; .name == "GOMEMLIMIT" and
        .valueFrom.resourceFieldRef.resource == "requests.memory") and
      $default.resources.requests.memory == "64Mi" and
      $default.resources.limits.memory == "1Gi" and
      any($default.env[]; .name == "GOMAXPROCS" and ."$patch" == "delete") and
      any($default.env[]; .name == "GOGC" and ."$patch" == "delete") and
      any($default.env[]; .name == "GOMEMLIMIT" and
        .valueFrom.resourceFieldRef.resource == "limits.memory"))
  ' "${flux_patches}" >/dev/null \
		|| fail 'Flux runtime patches do not converge constrained and default profiles in both directions'

	(
		source "${WORK_DIR}/federation-metrics-server.sh"
		METRICS_ATTEMPTS="${WORK_DIR}/metrics-attempts"
		printf '0\n' >"${METRICS_ATTEMPTS}"
		sleep() { :; }
		kubectl() {
			local attempts
			case "$*" in
				*'get deployment metrics-server --ignore-not-found --output name'*)
					attempts="$(<"${METRICS_ATTEMPTS}")"
					printf '%s\n' "$((attempts + 1))" >"${METRICS_ATTEMPTS}"
					[ "${attempts}" -ge 2 ] && printf 'deployment.apps/metrics-server\n'
					;;
				*'get deployment metrics-server --output jsonpath'*) printf '1' ;;
				*'scale deployment metrics-server'*) printf '%s\n' "$*" >>"${metrics_events}" ;;
				*'get pods --selector k8s-app=metrics-server'*) printf 'pod/metrics-server\n' ;;
				*'wait --for=delete pod'*) printf '%s\n' "$*" >>"${metrics_events}" ;;
			esac
		}
		PROFILE=federation
		FEDERATION_CONSTRAINED=yes
		configure_federation_metrics_server
	)
	rg --fixed-strings -- '--replicas 0' "${metrics_events}" >/dev/null \
		|| fail 'cold constrained startup does not wait for and pause metrics-server'
	rg --fixed-strings -- '--for=delete pod' "${metrics_events}" >/dev/null \
		|| fail 'constrained metrics-server lifecycle does not prove Pod deletion'

	: >"${metrics_events}"
	(
		source "${WORK_DIR}/federation-metrics-server.sh"
		kubectl() {
			case "$*" in
				*'get deployment metrics-server --ignore-not-found --output name'*)
					printf 'deployment.apps/metrics-server\n'
					;;
				*'get deployment metrics-server --output jsonpath'*) printf '0' ;;
				*'scale deployment metrics-server'*) printf '%s\n' "$*" >>"${metrics_events}" ;;
				*'rollout status deployment/metrics-server'*) printf '%s\n' "$*" >>"${metrics_events}" ;;
			esac
		}
		PROFILE=federation
		FEDERATION_CONSTRAINED=no
		configure_federation_metrics_server
	)
	rg --fixed-strings -- '--replicas 1' "${metrics_events}" >/dev/null \
		|| fail 'default federation reuse does not restore metrics-server'
	rg --fixed-strings 'rollout status deployment/metrics-server' "${metrics_events}" >/dev/null \
		|| fail 'default metrics-server restoration does not wait for its rollout'

	local metrics_failure="${WORK_DIR}/metrics-query-failure.txt"
	if (
		source "${WORK_DIR}/federation-metrics-server.sh"
		die() {
			echo "error: $*" >&2
			exit 1
		}
		kubectl() {
			case "$*" in
				*'get deployment metrics-server --ignore-not-found --output name'*)
					printf 'deployment.apps/metrics-server\n'
					;;
				*'get deployment metrics-server --output jsonpath'*) printf '1' ;;
				*'scale deployment metrics-server'*) return 0 ;;
				*'get pods --selector k8s-app=metrics-server'*) return 42 ;;
				*) return 0 ;;
			esac
		}
		PROFILE=federation
		FEDERATION_CONSTRAINED=yes
		configure_federation_metrics_server
	) >"${metrics_failure}" 2>&1; then
		fail 'constrained metrics-server lifecycle ignored a failed Pod inventory query'
	fi
	rg --fixed-strings 'could not inspect metrics-server Pods after scaling to zero' \
		"${metrics_failure}" >/dev/null \
		|| fail 'constrained metrics-server lifecycle did not explain its failed Pod inventory query'

	kustomizations="$(jq --null-input --arg revision "${expected_revision}" '{items: [
      {
        metadata: {name: "stale", generation: 2},
        status: {observedGeneration: 1, lastAppliedRevision: $revision,
          conditions: [{type: "Ready", status: "True"}]}
      },
      {
        metadata: {name: "current", generation: 2},
        status: {observedGeneration: 2, lastAppliedRevision: $revision,
          conditions: [{type: "Ready", status: "True"}]}
      }
    ]}')"
	helmreleases='{"items":[
    {"metadata":{"namespace":"test","name":"stale","generation":2},
      "status":{"observedGeneration":1,"conditions":[{"type":"Ready","status":"True"}]}},
    {"metadata":{"namespace":"test","name":"current","generation":2},
      "status":{"observedGeneration":2,"conditions":[{"type":"Ready","status":"True"}]}}
  ]}'
	milestones="$({
		source "${WORK_DIR}/collect-platform-milestones.sh"
		collect_platform_milestones "${expected_revision}" "${kustomizations}" "${helmreleases}"
	})"
	if rg --fixed-strings '/stale/' <<<"${milestones}" >/dev/null; then
		fail 'stale generations can still count as constrained convergence progress'
	fi
	for milestone in \
		'kustomization/current/generation/2/observed' \
		'kustomization/current/revision/main@sha1:deadbeef' \
		'kustomization/current/generation/2/revision/main@sha1:deadbeef/ready' \
		'helmrelease/test/current/generation/2/observed' \
		'helmrelease/test/current/generation/2/ready'; do
		rg --fixed-strings --line-regexp "${milestone}" <<<"${milestones}" >/dev/null \
			|| fail "current-generation progress omits ${milestone}"
	done

	local constrained_wait_helpers="${WORK_DIR}/constrained-wait-image.sh"
	extract_demo_functions "${constrained_wait_helpers}" \
		bridge_image_wait_required load_bridge_image_for_platform wait_for_platform_constrained
	(
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${constrained_wait_helpers}"
		image_attempts=0
		load_bridge_image_if_requested() {
			image_attempts=$((image_attempts + 1))
			[ "${image_attempts}" -ge 2 ]
		}
		deadline_timeout() { printf '10s'; }
		kubectl() { printf '%s\n' '{"items":[]}'; }
		collect_platform_milestones() { :; }
		platform_is_ready() { return 0; }
		resource_trace_record_ready_layers() { :; }
		sleep_before_deadline() { :; }
		print_platform_wait_diagnostics() { :; }
		PROFILE=federation
		SOURCE_REVISION=deadbeef
		BRIDGE_IMAGE=matrix-a2a-bridge:test
		FEDERATION_NO_PROGRESS_SECONDS=10
		FEDERATION_MAX_SECONDS=10
		wait_for_platform_constrained
		[ "${image_attempts}" -eq 2 ] \
			|| fail 'constrained reconciliation returned before importing the receipt image'
	)
	local constrained_image_failure="${WORK_DIR}/constrained-image-import-failure.txt"
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${constrained_wait_helpers}"
		load_bridge_image_if_requested() { return 2; }
		flux() { :; }
		PROFILE=federation
		# shellcheck disable=SC2034 # sourced wait helper consumes this injected fixture revision
		SOURCE_REVISION=deadbeef
		BRIDGE_IMAGE=matrix-a2a-bridge:test
		# shellcheck disable=SC2034 # sourced wait helper consumes this injected progress timeout
		FEDERATION_NO_PROGRESS_SECONDS=10
		# shellcheck disable=SC2034 # sourced wait helper consumes this injected absolute timeout
		FEDERATION_MAX_SECONDS=10
		wait_for_platform_constrained
	) >"${constrained_image_failure}" 2>&1; then
		fail 'constrained reconciliation ignored a failed receipt image import'
	fi
	rg --fixed-strings 'matrix-a2a-bridge:test, but its image import failed' \
		"${constrained_image_failure}" >/dev/null \
		|| fail 'constrained receipt image import failure lacks a bounded diagnostic'
}

check_federation_constrained_node_capacity() {
	local constrained_config="${WORK_DIR}/constrained-node-k3d.yaml"
	local standard_config="${WORK_DIR}/standard-node-k3d.yaml"
	(
		# shellcheck source=scripts/lib/demo-config.sh
		source "${ROOT_DIR}/scripts/lib/demo-config.sh"
		CLUSTER_NAME=fgentic-fed
		FEDERATION_LOOPBACK=127.0.0.2
		PROFILE=federation
		FEDERATION_CONSTRAINED=no
		render_k3d_config "${standard_config}"
		FEDERATION_CONSTRAINED=yes
		render_k3d_config "${constrained_config}"
	)
	assert_yq_all \
		'[.env[]? | select((.envVar == "GOGC=50") or (.envVar == "GOMEMLIMIT=1GiB"))] |
      length == 0' \
		"${standard_config}" 'canonical federation unexpectedly tunes the k3s Go runtime'
	assert_yq_all '
      ((.env | length) == 2) and
      (.env[0].envVar == "GOGC=50") and
      (.env[0].nodeFilters[0] == "server:*") and
      (.env[1].envVar == "GOMEMLIMIT=1GiB") and
      (.env[1].nodeFilters[0] == "server:*")
    ' "${constrained_config}" 'constrained federation omits the k3s Go runtime budget'
	for contract in \
		'dev.fgentic.demo.capacity=${FEDERATION_CAPACITY_MODE}@server:*' \
		'[ "${actual_capacity_mode}" = "${FEDERATION_CAPACITY_MODE}" ]' \
		'run fed:down first'; do
		rg --fixed-strings "${contract}" "${DEMO_CLUSTER}" >/dev/null \
			|| fail "federation capacity lifecycle omits ${contract}"
	done
}

check_federation_constrained_failure_guards() {
	local artifact_helpers="${WORK_DIR}/federation-artifact-helpers.sh"
	local cleanup_helpers="${WORK_DIR}/federation-cleanup-helpers.sh"
	local cleanup_status
	local image_events="${WORK_DIR}/side-load-image-events.txt"
	local image_helpers="${WORK_DIR}/federation-image-helpers.sh"
	local import_line remove_line
	local node_events="${WORK_DIR}/node-image-events.txt"
	local node_helpers="${WORK_DIR}/federation-node-image-helpers.sh"
	local node_state="${WORK_DIR}/node-image-state.txt"
	local orphan_output="${WORK_DIR}/image-only-orphan.txt"
	local storage_helpers="${WORK_DIR}/federation-storage-helpers.sh"

	extract_demo_functions "${storage_helpers}" \
		cluster_attached_volume_names cluster_retained_storage_bytes \
		cluster_owned_image_ids cluster_owned_image_bytes
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${storage_helpers}"
		cluster_container_ids() { return 41; }
		cluster_attached_volume_names
	); then
		fail 'attached-volume inventory masked a failed container producer'
	fi
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${storage_helpers}"
		cluster_attached_volume_names() { return 42; }
		cluster_retained_storage_bytes
	); then
		fail 'retained-storage accounting masked a failed volume producer'
	fi
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${storage_helpers}"
		cluster_attached_volume_names() { return 0; }
		cluster_container_ids() { return 43; }
		cluster_retained_storage_bytes
	); then
		fail 'retained-storage accounting masked a failed container producer'
	fi
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${storage_helpers}"
		docker() { return 44; }
		CLUSTER_NAME=fgentic-fed
		cluster_owned_image_ids
	); then
		fail 'owned-image inventory masked a failed Docker producer'
	fi
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${storage_helpers}"
		cluster_owned_image_ids() { return 45; }
		cluster_owned_image_bytes
	); then
		fail 'owned-image accounting masked a failed image producer'
	fi
	extract_demo_functions "${cleanup_helpers}" teardown_receipt_complete
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${cleanup_helpers}"
		cluster_exists() { return 42; }
		teardown_receipt_path() { printf '/does/not/matter\n'; }
		teardown_receipt_complete
	); then
		fail 'receipt cleanup converted a failed cluster inventory into success'
	else
		cleanup_status=$?
		[ "${cleanup_status}" -eq 42 ] \
			|| fail 'receipt cleanup did not preserve its cluster-inventory failure status'
	fi

	extract_demo_functions "${artifact_helpers}" \
		cluster_runtime_artifacts_exist cluster_artifacts_exist demo_status demo_stop demo_down
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${artifact_helpers}"
		die() {
			echo "error: $*" >&2
			exit 1
		}
		require_cluster_runtime() { return 0; }
		teardown_receipt_exists() { return 1; }
		cluster_exists() { return 1; }
		cluster_container_ids() { return 0; }
		cluster_network_exists() { return 1; }
		cluster_volume_exists() { return 1; }
		cluster_owned_image_ids() { printf 'sha256:owned-image\n'; }
		CLUSTER_NAME=fgentic-fed
		demo_status
	) >"${orphan_output}" 2>&1; then
		fail 'federation status reported absent while an owned local image remained'
	fi
	rg --fixed-strings 'refusing orphan inspection for fgentic-fed' "${orphan_output}" \
		>/dev/null || fail 'federation status did not identify an image-only orphan'
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${artifact_helpers}"
		die() {
			echo "error: $*" >&2
			exit 1
		}
		require_cluster_runtime() { return 0; }
		teardown_receipt_exists() { return 1; }
		teardown_receipt_fail() {
			echo "error: $*" >&2
			exit 1
		}
		cluster_exists() { return 1; }
		cluster_container_ids() { return 0; }
		cluster_network_exists() { return 1; }
		cluster_volume_exists() { return 1; }
		cluster_owned_image_ids() { printf 'sha256:owned-image\n'; }
		CLUSTER_NAME=fgentic-fed
		demo_down
	) >"${orphan_output}" 2>&1; then
		fail 'federation teardown ignored an image-only orphan'
	fi
	rg --fixed-strings 'refusing orphan cleanup for fgentic-fed' "${orphan_output}" \
		>/dev/null || fail 'federation teardown did not refuse an image-only orphan'
	if (
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${artifact_helpers}"
		die() {
			echo "error: $*" >&2
			exit 1
		}
		require_cluster_runtime() { return 0; }
		teardown_receipt_exists() { return 1; }
		require_owned_evaluation_cluster() { return 0; }
		cluster_container_ids() { printf 'container-id\n'; }
		cluster_volume_identity() { printf 'volume-id\n'; }
		cluster_running_container_ids() { return 46; }
		k3d() { return 0; }
		CLUSTER_NAME=fgentic-fed
		demo_stop
	) >"${orphan_output}" 2>&1; then
		fail 'federation stop masked a failed running-container inventory'
	fi
	rg --fixed-strings 'could not verify fgentic-fed stopped containers' "${orphan_output}" \
		>/dev/null || fail 'federation stop did not explain its failed container inventory'

	if ! (
		set -u
		PROFILE=demo
		FGENTIC_FED_TRACE=yes
		unset RESOURCE_TRACE_DIR
		# shellcheck source=scripts/lib/federation-resources.sh
		source "${RESOURCE_TRACE}"
		resource_trace_set_phase reconcile
		resource_trace_record_ready_layers 'main@sha1:deadbeef' '{"items":[]}'
		resource_trace_set_phase proof
		resource_trace_collect_idle
		resource_trace_finish
	); then
		fail 'a leaked federation trace setting is not isolated from the default demo profile'
	fi

	extract_demo_functions "${image_helpers}" \
		prune_owned_host_images build_and_load_images load_bridge_image_if_requested
	: >"${image_events}"
	(
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${image_helpers}"
		build_image() { printf 'build %s\n' "$1" >>"${image_events}"; }
		k3d() { printf 'k3d %s\n' "$*" >>"${image_events}"; }
		docker() {
			printf 'docker %s\n' "$*" >>"${image_events}"
			if [ "${1:-}" = images ] && [[ "$*" == *'--format'* ]]; then
				printf '%s\n' \
					fgentic-demo-source-fgentic-fed:test \
					fgentic-demo-source-fgentic-fed:stale
			fi
		}
		resource_trace_require_volume_sample() { return 0; }
		SOURCE_CONTEXT="${WORK_DIR}/source-context"
		mkdir -p "${SOURCE_CONTEXT}"
		export SOURCE_BASE_IMAGE=example.invalid/source:fixed
		export SOURCE_GIT_PACKAGES='git=fixed'
		export SOURCE_IMAGE=fgentic-demo-source-fgentic-fed:test
		export BRIDGE_IMAGE=matrix-a2a-bridge:test
		CLUSTER_NAME=fgentic-fed
		PROFILE=federation
		build_and_load_images
	)
	for event in \
		'build matrix-a2a-bridge:test' \
		'k3d image import --mode auto --cluster fgentic-fed fgentic-demo-source-fgentic-fed:test' \
		'docker image rm fgentic-demo-source-fgentic-fed:test fgentic-demo-source-fgentic-fed:stale'; do
		rg --fixed-strings --line-regexp "${event}" "${image_events}" >/dev/null \
			|| fail "successful source side-load omits ${event}"
	done
	import_line="$(rg --line-number --fixed-strings \
		'k3d image import --mode auto --cluster fgentic-fed fgentic-demo-source-fgentic-fed:test' \
		"${image_events}" | cut -d: -f1)"
	remove_line="$(rg --line-number --fixed-strings \
		'docker image rm fgentic-demo-source-fgentic-fed:test fgentic-demo-source-fgentic-fed:stale' \
		"${image_events}" | cut -d: -f1)"
	((import_line < remove_line)) || fail 'source host image is removed before its successful import'
	(
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${image_helpers}"
		kubectl() {
			jq --null-input '{spec: {values: {image: {
          repository: "matrix-a2a-bridge", tag: "test"
        }}}}'
		}
		k3d() { printf 'k3d %s\n' "$*" >>"${image_events}"; }
		docker() {
			printf 'docker %s\n' "$*" >>"${image_events}"
			if [ "${1:-}" = images ] && [[ "$*" == *'--format'* ]]; then
				printf '%s\n' matrix-a2a-bridge:test matrix-a2a-bridge:stale
			fi
		}
		resource_trace_require_volume_sample() { return 0; }
		PROFILE=demo
		export BRIDGE_IMAGE=matrix-a2a-bridge:test
		CLUSTER_NAME=fgentic-demo
		load_bridge_image_if_requested
	)
	for event in \
		'k3d image import --mode auto --cluster fgentic-demo matrix-a2a-bridge:test' \
		'docker image rm matrix-a2a-bridge:test matrix-a2a-bridge:stale'; do
		rg --fixed-strings --line-regexp "${event}" "${image_events}" >/dev/null \
			|| fail "successful bridge side-load omits ${event}"
	done
	import_line="$(rg --line-number --fixed-strings \
		'k3d image import --mode auto --cluster fgentic-demo matrix-a2a-bridge:test' \
		"${image_events}" | cut -d: -f1)"
	remove_line="$(rg --line-number --fixed-strings \
		'docker image rm matrix-a2a-bridge:test matrix-a2a-bridge:stale' \
		"${image_events}" | cut -d: -f1)"
	((import_line < remove_line)) || fail 'bridge host image is removed before its successful import'
	(
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${image_helpers}"
		kubectl() {
			jq --null-input '{spec: {template: {spec: {containers: [{
          name: "usage-receipt", image: "matrix-a2a-bridge:test"
        }]}}}}'
		}
		k3d() { printf 'k3d %s\n' "$*" >>"${image_events}"; }
		docker() {
			printf 'docker %s\n' "$*" >>"${image_events}"
			if [ "${1:-}" = images ] && [[ "$*" == *'--format'* ]]; then
				printf '%s\n' matrix-a2a-bridge:test matrix-a2a-bridge:stale
			fi
		}
		resource_trace_require_volume_sample() { return 0; }
		PROFILE=federation
		export BRIDGE_IMAGE=matrix-a2a-bridge:test
		CLUSTER_NAME=fgentic-fed
		load_bridge_image_if_requested
	)
	rg --fixed-strings --line-regexp \
		'k3d image import --mode auto --cluster fgentic-fed matrix-a2a-bridge:test' \
		"${image_events}" >/dev/null \
		|| fail 'federation receipt image is not side-loaded after its Deployment requests it'

	extract_demo_functions "${node_helpers}" prune_stale_node_images
	printf 'stale\n' >"${node_state}"
	: >"${node_events}"
	(
		# shellcheck disable=SC1090 # fixture path is generated from the extracted helper at runtime
		source "${node_helpers}"
		die() {
			echo "error: $*" >&2
			exit 1
		}
		docker() {
			case "$*" in
				'ps --filter label=k3d.cluster=fgentic-fed --format {{.Names}}')
					printf 'k3d-fgentic-fed-server-0\n'
					;;
				'exec k3d-fgentic-fed-server-0 crictl images --output json')
					if [ "$(<"${node_state}")" = stale ]; then
						jq --null-input \
							'{images: [{repoTags: ["docker.io/library/fgentic-demo-source-fgentic-fed:test"]}, {repoTags: ["docker.io/library/fgentic-demo-source-fgentic-fed:stale"]}]}'
					else
						jq --null-input \
							'{images: [{repoTags: ["docker.io/library/fgentic-demo-source-fgentic-fed:test"]}]}'
					fi
					;;
				'exec k3d-fgentic-fed-server-0 crictl rmi docker.io/library/fgentic-demo-source-fgentic-fed:stale')
					printf '%s\n' "$*" >>"${node_events}"
					printf 'clean\n' >"${node_state}"
					;;
				*) return 1 ;;
			esac
		}
		CLUSTER_NAME=fgentic-fed
		prune_stale_node_images fgentic-demo-source-fgentic-fed:test
	)
	rg --fixed-strings --line-regexp \
		'exec k3d-fgentic-fed-server-0 crictl rmi docker.io/library/fgentic-demo-source-fgentic-fed:stale' \
		"${node_events}" >/dev/null \
		|| fail 'successful federation reuse does not prune its stale node-side source image'
}

check_federation_constrained_render() {
	[ -f "${CONSTRAINED_OVERLAY}/kustomization.yaml" ] \
		|| fail 'clusters/federation-constrained is missing'
	[ -f "${CONSTRAINED_COMPONENT}/kustomization.yaml" ] \
		|| fail 'constrained federation component is missing'
	for component in agentgateway controllers kagent keycloak matrix postgres; do
		[ -f "${CONSTRAINED_COMPONENT}/${component}/kustomization.yaml" ] \
			|| fail "constrained ${component} component is missing"
	done

	render_federation_contract "${CLUSTER_OVERLAY}" default
	render_federation_contract "${CONSTRAINED_OVERLAY}" constrained
	local overlay profile
	for profile in demo local gcp; do
		overlay="${ROOT_DIR}/clusters/${profile}"
		kubectl kustomize "${overlay}" >"${WORK_DIR}/${profile}-cluster.yaml"
		assert_yq_all \
			'[.] | map(select(.kind == "Kustomization")) |
        map(.spec.components[]? // "") | map(select(contains("constrained"))) | length == 0' \
			"${WORK_DIR}/${profile}-cluster.yaml" \
			"${profile} profile unexpectedly composes constrained federation tuning"
	done

	assert_yq_all \
		'[.] | map(select(.kind == "Kustomization")) |
      map(.spec.components[]? // "") | map(select(contains("constrained"))) | length == 0' \
		"${WORK_DIR}/default-cluster.yaml" \
		'canonical federation profile unexpectedly composes constrained-host tuning'
	assert_yq_all \
		'[.] | map(select(.kind == "Kustomization")) |
	  map(.spec.components[]? // "") | map(select(contains("constrained"))) |
	  length == 8' \
		"${WORK_DIR}/constrained-cluster.yaml" \
		'constrained profile does not compose all eight workload tunings'

	write_federation_inventory "${WORK_DIR}/default-cluster.yaml" \
		"${WORK_DIR}/default-cluster-inventory.json"
	write_federation_inventory "${WORK_DIR}/constrained-cluster.yaml" \
		"${WORK_DIR}/constrained-cluster-inventory.json"
	cmp -s "${WORK_DIR}/default-cluster-inventory.json" \
		"${WORK_DIR}/constrained-cluster-inventory.json" \
		|| fail 'constrained profile changes the canonical Flux-layer inventory'
	write_federation_inventory "${WORK_DIR}/default-recursive.yaml" \
		"${WORK_DIR}/default-resource-inventory.json"
	write_federation_inventory "${WORK_DIR}/constrained-recursive.yaml" \
		"${WORK_DIR}/constrained-resource-inventory.json"
	cmp -s "${WORK_DIR}/default-resource-inventory.json" \
		"${WORK_DIR}/constrained-resource-inventory.json" \
		|| fail 'constrained profile adds or removes a canonical runtime object'

	for homeserver in matrix-stack matrix-stack-b matrix-stack-c; do
		assert_yq \
			'select(.kind == "HelmRelease" and .metadata.name == "'"${homeserver}"'") |
        .spec.values.synapse.resources == null and
        .spec.values.synapse.additional."05-constrained-memory" == null and
        .spec.values.haproxy == null and
        .spec.postRenderers == null' \
			"${WORK_DIR}/default-recursive.yaml" \
			"canonical ${homeserver} unexpectedly inherited constrained resources"
	done

	local layer predecessor
	while IFS='|' read -r layer predecessor; do
		assert_yq \
			'select(.kind == "Kustomization" and .metadata.name == "'"${layer}"'") |
        [.spec.dependsOn[]?.name] | contains(["'"${predecessor}"'"])' \
			"${WORK_DIR}/constrained-cluster.yaml" \
			"constrained ${layer} is not serialized after ${predecessor}"
	done <<'EOF'
postgres|gateway
matrix|postgres
matrix-b|matrix
matrix-c|matrix-b
keycloak|matrix-c
agentgateway|keycloak
agentgateway-provider|agentgateway
kagent|agentgateway-provider
EOF
}

check_federation_constrained_resources() {
	local recursive="${WORK_DIR}/constrained-recursive.yaml"
	local homeserver
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "traefik") |
      .spec.values.deployment.goMemLimitPercentage == 0.5 and
      ([.spec.values.env[] | select(.name == "GOGC" and .value == "25")] | length) == 1' \
		"${recursive}" 'Traefik does not have the constrained Go runtime envelope'
	for controller in cert-manager cloudnative-pg; do
		assert_yq \
			'select(.kind == "HelmRelease" and .metadata.name == "'"${controller}"'") |
        .spec.postRenderers[0].kustomize.patches[0] |
        select(.target.group == "apps" and .target.version == "v1" and
          .target.kind == "Deployment") |
        .patch | from_yaml |
        select((map(.value.name) | join("|")) == "GOMAXPROCS|GOGC|GOMEMLIMIT" and
          (map(.value.value) | join("|")) == "1|25|64MiB")' \
			"${recursive}" "${controller} does not have the constrained Go runtime envelope"
	done
	for homeserver in matrix-stack matrix-stack-b matrix-stack-c; do
		assert_yq \
			'select(.kind == "HelmRelease" and .metadata.name == "'"${homeserver}"'") |
        .spec.values.synapse.resources.requests.memory == "128Mi" and
        .spec.values.synapse.resources.limits.memory == "640Mi" and
        .spec.values.synapse.additional."05-constrained-memory".config ==
          "caches:\n  global_factor: 0.1\n# Ten idle database connections per homeserver dominate a tiny three-room lab.\n# Keep one warm connection and two slots for short concurrent proof bursts.\ndatabase:\n  args:\n    cp_min: 1\n    cp_max: 2\n" and
        .spec.values.haproxy.resources.requests.memory == "32Mi" and
        .spec.values.haproxy.resources.limits.memory == "96Mi"' \
			"${recursive}" "${homeserver} does not have the constrained Matrix envelope"
		assert_yq \
			'select(.kind == "HelmRelease" and .metadata.name == "'"${homeserver}"'") |
        .spec.postRenderers[0].kustomize.patches[0] |
        select(.target.group == "apps" and .target.version == "v1" and
          .target.kind == "Deployment" and .target.name == "ess-haproxy") |
        .patch | from_yaml |
        select(.spec.template.spec.containers[0].name == "haproxy" and
          (.spec.template.spec.containers[0].args | length) == 5 and
          (.spec.template.spec.containers[0].args | join("|")) ==
            "-f|/usr/local/etc/haproxy/haproxy.cfg|-dW|-m|32")' \
			"${recursive}" "${homeserver} does not enforce the constrained HAProxy allocator"
		assert_yq \
			'select(.kind == "HelmRelease" and .metadata.name == "'"${homeserver}"'") |
        .spec.postRenderers[0].kustomize.patches[1] |
        select(.target.group == "apps" and .target.version == "v1" and
          .target.kind == "StatefulSet" and .target.name == "ess-synapse-main") |
        .patch | from_yaml |
        select(.[0].op == "add" and
          .[0].path == "/spec/template/spec/containers/0/env/-" and
          .[0].value.name == "MALLOC_CONF" and
          .[0].value.value ==
            "narenas:2,background_thread:true,dirty_decay_ms:1000,muzzy_decay_ms:1000")' \
			"${recursive}" "${homeserver} does not enforce constrained jemalloc settings"
	done

	assert_yq \
		'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
      .spec.resources.requests.memory == "192Mi" and
      .spec.resources.limits.memory == "384Mi" and
      .spec.postgresql.parameters.shared_buffers == "8MB"' \
		"${recursive}" 'platform Postgres does not have the constrained memory envelope'
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "keycloak") |
      .spec.values.resources.requests.memory == "256Mi" and
      .spec.values.resources.limits.memory == "640Mi" and
      .spec.values.cache.stack == "custom" and
      (.spec.values.extraEnv | contains("KC_DB_POOL_MAX_SIZE")) and
      (.spec.values.extraEnv | contains("value: \"5\"")) and
      (.spec.values.extraEnv | contains("KC_CACHE")) and
      (.spec.values.extraEnv | contains("value: local")) and
      (.spec.values.extraEnv | contains("JAVA_OPTS_KC_HEAP")) and
      (.spec.values.extraEnv | contains("-XX:InitialRAMPercentage=8.0")) and
      (.spec.values.extraEnv | contains("-XX:MaxRAMPercentage=30.0")) and
      (.spec.values.extraEnv | contains("-XX:ActiveProcessorCount=1")) and
      (.spec.values.extraEnv | contains("-Xss512k")) and
      (.spec.values.extraEnv | contains("-XX:ReservedCodeCacheSize=64m")) and
      (.spec.values.extraEnv | contains("-XX:MinHeapFreeRatio=10")) and
      (.spec.values.extraEnv | contains("-XX:MaxHeapFreeRatio=30")) and
      (.spec.values.extraEnv | contains("-XX:G1PeriodicGCInterval=30000")) and
      (.spec.values.extraEnv | contains("MALLOC_ARENA_MAX")) and
      (.spec.values.extraEnv | contains("MALLOC_TRIM_THRESHOLD_"))' \
		"${recursive}" 'Keycloak does not have the constrained local-cache JVM envelope'
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "agentgateway") |
      .spec.values.resources.requests.memory == "64Mi" and
      .spec.values.resources.limits.memory == "192Mi"' \
		"${recursive}" 'agentgateway controller does not have the constrained memory envelope'
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "agentgateway") |
      .spec.postRenderers[0].kustomize.patches[0] |
      select(.target.name == "agentgateway") |
      .patch | from_yaml |
      select(.[0].op == "replace" and
        .[0].path ==
          "/spec/template/spec/containers/0/env/0/valueFrom/resourceFieldRef/resource" and
        .[0].value == "requests.memory" and
        .[1].value.name == "GOGC" and .[1].value.value == "25")' \
		"${recursive}" 'agentgateway controller does not have the constrained Go runtime envelope'
	assert_yq \
		'select(.kind == "AgentgatewayParameters" and .metadata.name == "secured") |
      .spec.resources.requests.memory == "64Mi" and
      .spec.resources.limits.memory == "192Mi"' \
		"${recursive}" 'agentgateway proxy does not have the constrained memory envelope'
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "kagent") |
      .spec.values.controller.resources.requests.memory == "64Mi" and
      .spec.values.controller.resources.limits.memory == "256Mi" and
      (.spec.values.controller.env | map(.name + "=" + .value) | join("|")) ==
        "GOMAXPROCS=1|GOGC=25|GOMEMLIMIT=64MiB"' \
		"${recursive}" 'kagent controller does not have the constrained memory envelope'
	assert_yq \
		'select(.kind == "Agent" and .metadata.name == "docs-qa") |
      .spec.declarative.deployment.resources.requests.memory == "64Mi" and
      .spec.declarative.deployment.resources.limits.memory == "256Mi"' \
		"${recursive}" 'docs-qa does not have the constrained memory envelope'
}

check_federation_trace_helpers() (
	set -euo pipefail
	local sentinel='trace-content-must-not-appear'
	RESOURCE_TRACE_DIR="${WORK_DIR}/synthetic-trace"
	CLUSTER_NAME=fgentic-fed
	PROFILE=federation
	FGENTIC_FED_TRACE=yes
	mkdir -p "${RESOURCE_TRACE_DIR}"
	printf 'bootstrap\n' >"${RESOURCE_TRACE_DIR}/phase"
	: >"${RESOURCE_TRACE_DIR}/failures.jsonl"

	# The fixtures deliberately carry a sentinel in unapproved fields. Its absence proves that the
	# helpers construct allowlisted objects instead of serializing raw Docker or Kubernetes input.
	docker() {
		case "${1:-}" in
			ps)
				if [ "${2:-}" = '--filter' ]; then
					printf '%s\n' k3d-fgentic-fed-server-0 k3d-fgentic-fed-server-1 \
						k3d-fgentic-fed-agent-0
				else
					printf '%s\n' k3d-fgentic-fed-server-0
				fi
				;;
			stats)
				printf '%s\n' \
					'k3d-fgentic-fed-server-0|1MiB / 4GiB' \
					'k3d-fgentic-fed-server-1|2MiB / 4GiB'
				;;
			exec)
				jq --null-input --compact-output --arg sentinel "${sentinel}" '{
          stats: [{
            attributes: {
              labels: {
                "io.kubernetes.pod.namespace": "matrix",
                "io.kubernetes.pod.name": "synapse-0"
              },
              metadata: {name: "synapse"},
              annotations: {unapproved: $sentinel}
            },
            memory: {workingSetBytes: {value: "4194304"}},
            unapproved: $sentinel
          }]
        }'
				;;
			*) return 1 ;;
		esac
	}
	date() {
		printf '2026-07-15T00:00:00Z\n'
	}
	cluster_volume_exists() { return 0; }
	cluster_volume_identity() { printf 'fgentic-fed-images\n'; }
	cluster_volume_bytes() { printf '2048\n'; }
	cluster_retained_storage_bytes() { printf '8192\n'; }
	cluster_owned_image_bytes() { printf '4096\n'; }

	# shellcheck source=scripts/lib/federation-resources.sh
	source "${RESOURCE_TRACE}"
	resource_trace_sample_server
	resource_trace_sample_workloads
	resource_trace_sample_volume
	local kustomizations
	kustomizations="$(jq --null-input --arg sentinel "${sentinel}" '{
      items: [
        {
          metadata: {name: "matrix", generation: 2},
          status: {
            observedGeneration: 2,
            conditions: [{
              type: "Ready", status: "True",
              lastTransitionTime: "2026-07-15T00:00:00Z",
              message: $sentinel
            }],
            lastAppliedRevision: "main@sha1:deadbeef",
            unapproved: $sentinel
          }
        },
        {
          metadata: {name: "stale-ready", generation: 2, annotations: {unapproved: $sentinel}},
          status: {
            observedGeneration: 1,
            lastAppliedRevision: "main@sha1:deadbeef",
            conditions: [{type: "Ready", status: "True", message: $sentinel}]
          }
        }
      ]
    }')"
	resource_trace_record_ready_layers 'main@sha1:deadbeef' "${kustomizations}"

	jq --slurp --exit-status '
    length == 1 and .[0].memory_bytes == 3145728 and
    (.[0] | keys) == ["memory_bytes", "phase", "timestamp"]
  ' "${RESOURCE_TRACE_DIR}/server.jsonl" >/dev/null \
		|| fail 'server resource trace is not a minimal deterministic allowlisted record'
	jq --slurp --exit-status '
    length == 1 and .[0].working_set_bytes == 4194304 and
    (.[0] | keys) ==
      ["container", "namespace", "phase", "pod", "timestamp", "working_set_bytes"]
  ' "${RESOURCE_TRACE_DIR}/workloads.jsonl" >/dev/null \
		|| fail 'workload resource trace is not a minimal deterministic allowlisted record'
	jq --slurp --exit-status '
    length == 1 and .[0].bytes == 2048 and .[0].volume_identity == "fgentic-fed-images" and
    .[0].retained_cluster_bytes == 8192 and .[0].local_image_virtual_bytes == 4096 and
    (.[0] | keys) == [
      "bytes", "local_image_virtual_bytes", "phase", "retained_cluster_bytes", "timestamp",
      "volume_identity"
    ]
  ' "${RESOURCE_TRACE_DIR}/volume.jsonl" >/dev/null \
		|| fail 'volume resource trace is not a minimal deterministic allowlisted record'
	jq --slurp --exit-status '
    length == 1 and .[0].layer == "matrix" and
    (.[0] | keys) == ["layer", "observed_at", "ready_at", "revision"]
  ' "${RESOURCE_TRACE_DIR}/layers.jsonl" >/dev/null \
		|| fail 'layer resource trace is not a minimal deterministic allowlisted record'
	if rg --fixed-strings "${sentinel}" "${RESOURCE_TRACE_DIR}" >/dev/null; then
		fail 'resource trace exposed unapproved runtime or Kubernetes content'
	fi

	printf '%s\n' \
		'{"schema":1,"cluster":"fgentic-fed","mode":"constrained","started_at":"2026-07-15T00:00:00Z","idle_settle_seconds":300}' \
		>"${RESOURCE_TRACE_DIR}/metadata.json"
	printf '%s\n' \
		'{"timestamp":"2026-07-15T00:00:00Z","phase":"bootstrap","memory_bytes":900}' \
		'{"timestamp":"2026-07-15T00:00:00Z","phase":"proof","memory_bytes":1200}' \
		>"${RESOURCE_TRACE_DIR}/server.jsonl"
	local sample
	for sample in 100 200 300 400 500 600 700 800 900 1000 1100 1200; do
		jq --null-input --compact-output --argjson memory_bytes "${sample}" \
			'{timestamp: "2026-07-15T00:00:00Z", phase: "idle", memory_bytes: $memory_bytes}' \
			>>"${RESOURCE_TRACE_DIR}/server.jsonl"
	done
	printf '%s\n' \
		'{"timestamp":"2026-07-15T00:00:00Z","phase":"bootstrap","volume_identity":"fgentic-fed-images","bytes":2048,"retained_cluster_bytes":8192,"local_image_virtual_bytes":4096}' \
		'{"timestamp":"2026-07-15T00:00:00Z","phase":"idle","volume_identity":"fgentic-fed-images","bytes":3072,"retained_cluster_bytes":12288,"local_image_virtual_bytes":4096}' \
		>"${RESOURCE_TRACE_DIR}/volume.jsonl"
	resource_trace_summarize
	jq --exit-status '
    .schema == 1 and .boot_peak_bytes == 1200 and .idle_median_bytes == 650 and
	    .idle_settle_seconds == 300 and
	    .server_samples == 14 and .idle_samples == 12 and
	    .image_volume_peak_bytes == 3072 and .retained_cluster_peak_bytes == 12288 and
	    .owned_disk_upper_bound_peak_bytes == 16384 and .local_image_bytes == 4096 and
    (.layers | length) == 1 and .layers[0].layer == "matrix" and
    (keys) == [
	      "boot_peak_bytes", "finished_at", "idle_median_bytes", "idle_samples",
	      "idle_settle_seconds",
	      "image_volume_peak_bytes", "layers", "local_image_bytes",
	      "owned_disk_upper_bound_peak_bytes", "retained_cluster_peak_bytes", "schema",
	      "server_samples"
    ]
  ' "${RESOURCE_TRACE_DIR}/summary.json" >/dev/null \
		|| fail 'resource trace summary does not report deterministic peaks and twelve-sample idle median'

	local complete_trace="${RESOURCE_TRACE_DIR}"
	RESOURCE_TRACE_DIR="${WORK_DIR}/failed-sample-trace"
	mkdir -p "${RESOURCE_TRACE_DIR}"
	cp "${complete_trace}/metadata.json" "${complete_trace}/server.jsonl" \
		"${complete_trace}/volume.jsonl" "${complete_trace}/layers.jsonl" \
		"${RESOURCE_TRACE_DIR}/"
	printf '%s\n' \
		'{"timestamp":"2026-07-15T00:00:00Z","phase":"bootstrap","kind":"server"}' \
		>"${RESOURCE_TRACE_DIR}/failures.jsonl"
	if resource_trace_summarize >/dev/null 2>&1; then
		fail 'resource trace accepted a run with a required sampling failure'
	fi
	[ ! -e "${RESOURCE_TRACE_DIR}/summary.json" ] \
		&& [ ! -e "${RESOURCE_TRACE_DIR}/summary.json.tmp" ] \
		|| fail 'failed resource sampling published a partial summary artifact'

	RESOURCE_TRACE_DIR="${WORK_DIR}/incomplete-trace"
	mkdir -p "${RESOURCE_TRACE_DIR}"
	cp "${complete_trace}/metadata.json" "${complete_trace}/server.jsonl" \
		"${complete_trace}/volume.jsonl" "${complete_trace}/failures.jsonl" \
		"${RESOURCE_TRACE_DIR}/"
	: >"${RESOURCE_TRACE_DIR}/layers.jsonl"
	if resource_trace_summarize >/dev/null 2>&1; then
		fail 'resource trace accepted a summary without current-revision layer evidence'
	fi
	[ ! -e "${RESOURCE_TRACE_DIR}/summary.json" ] \
		&& [ ! -e "${RESOURCE_TRACE_DIR}/summary.json.tmp" ] \
		|| fail 'failed resource trace published a partial summary artifact'

	FGENTIC_FED_TRACE_DIR="${WORK_DIR}/existing-volume-trace"
	FEDERATION_CONSTRAINED=yes
	cluster_volume_identity() { return 71; }
	if resource_trace_start >/dev/null 2>&1; then
		fail 'resource trace start ignored a failed existing-volume sample'
	fi
	jq --slurp --exit-status '
	  length == 1 and .[0].kind == "volume" and
	  (.[0] | keys) == ["kind", "phase", "timestamp"]
	' "${RESOURCE_TRACE_DIR}/failures.jsonl" >/dev/null \
		|| fail 'failed existing-volume trace start did not record its sampling failure'

	RESOURCE_TRACE_DIR="${WORK_DIR}/normal-sampler-stop"
	mkdir -p "${RESOURCE_TRACE_DIR}"
	local sample_calls=0
	resource_trace_sample_server() { sample_calls=$((sample_calls + 1)); }
	resource_trace_sample_workloads() { :; }
	resource_trace_record_sampling_failure() {
		fail "normal sampler stop was recorded as a $1 failure"
	}
	sleep() { : >"${RESOURCE_TRACE_DIR}/stop"; }
	resource_trace_sample_loop || fail 'normal sampler stop returned a failure status'
	[ "${sample_calls}" -eq 1 ] || fail 'normal sampler stop took an unexpected extra sample'
)

check_federation_constrained() {
	check_federation_constrained_cli
	check_federation_constrained_lifecycle_guards
	check_federation_constrained_state_transitions
	check_federation_constrained_node_capacity
	check_federation_constrained_failure_guards
	check_federation_constrained_render
	check_federation_constrained_resources
	check_federation_trace_helpers
}
