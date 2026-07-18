#!/usr/bin/env bash
# Content-free resource sampling for the disposable federation lab. This file is sourced by
# demo.sh; every serialized field is allowlisted here so workload data and Kubernetes messages
# can never enter a trace by accident.

resource_trace_enabled() {
	[ "${PROFILE:-}" = federation ] && [ "${FGENTIC_FED_TRACE:-no}" = yes ]
}

memory_quantity_bytes() {
	local quantity="$1"
	local number unit multiplier
	number="${quantity%%[[:alpha:]]*}"
	unit="${quantity#"${number}"}"
	case "${unit}" in
		B | "") multiplier=1 ;;
		KB) multiplier=1000 ;;
		MB) multiplier=1000000 ;;
		GB) multiplier=1000000000 ;;
		KiB) multiplier=1024 ;;
		MiB) multiplier=1048576 ;;
		GiB) multiplier=1073741824 ;;
		*) return 1 ;;
	esac
	awk -v number="${number}" -v multiplier="${multiplier}" \
		'BEGIN { printf "%.0f", number * multiplier }'
}

resource_trace_phase() {
	if [ -r "${RESOURCE_TRACE_DIR}/phase" ]; then
		tr -cd 'a-z_-' <"${RESOURCE_TRACE_DIR}/phase"
	else
		printf 'bootstrap'
	fi
}

resource_trace_set_phase() {
	resource_trace_enabled || return 0
	case "$1" in
		bootstrap | reconcile | proof | idle | stopped) ;;
		*) die "invalid federation resource-trace phase: $1" ;;
	esac
	printf '%s\n' "$1" >"${RESOURCE_TRACE_DIR}/phase"
	if ! resource_trace_sample_volume; then
		resource_trace_record_sampling_failure volume
		return 1
	fi
}

resource_trace_record_sampling_failure() {
	resource_trace_enabled || return 0
	local kind="$1" phase timestamp
	case "${kind}" in
		server | volume | sampler) ;;
		*) return 1 ;;
	esac
	timestamp="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	phase="$(resource_trace_phase)"
	jq --null-input --compact-output \
		--arg timestamp "${timestamp}" --arg phase "${phase}" --arg kind "${kind}" \
		'{timestamp: $timestamp, phase: $phase, kind: $kind}' \
		>>"${RESOURCE_TRACE_DIR}/failures.jsonl"
}

resource_trace_sample_server() {
	resource_trace_enabled || return 0
	local container container_output memory_bytes memory_usage name name_output phase stats_output
	local timestamp total_bytes
	local server_names=()
	container_output="$(docker ps --filter "label=k3d.cluster=${CLUSTER_NAME}" \
		--format '{{.Names}}')" || return 1
	name_output="$(awk '/-server-[0-9]+$/ { print }' <<<"${container_output}")" || return 1
	while IFS= read -r name; do
		[ -z "${name}" ] || server_names[${#server_names[@]}]="${name}"
	done <<<"${name_output}"
	((${#server_names[@]} > 0)) || return 0

	stats_output="$(docker stats --no-stream --format '{{.Name}}|{{.MemUsage}}' \
		"${server_names[@]}" 2>/dev/null)" || return 1
	total_bytes=0
	while IFS='|' read -r container memory_usage; do
		[ -n "${container}" ] || continue
		memory_usage="${memory_usage%% / *}"
		memory_bytes="$(memory_quantity_bytes "${memory_usage}")" || return 1
		total_bytes=$((total_bytes + memory_bytes))
	done <<<"${stats_output}"
	((total_bytes > 0)) || return 1
	timestamp="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	phase="$(resource_trace_phase)"
	jq --null-input --compact-output \
		--arg timestamp "${timestamp}" --arg phase "${phase}" \
		--argjson memory_bytes "${total_bytes}" \
		'{timestamp: $timestamp, phase: $phase, memory_bytes: $memory_bytes}' \
		>>"${RESOURCE_TRACE_DIR}/server.jsonl"
}

resource_trace_sample_workloads() {
	resource_trace_enabled || return 0
	local phase server timestamp
	server="k3d-${CLUSTER_NAME}-server-0"
	docker ps --format '{{.Names}}' | rg --fixed-strings --line-regexp "${server}" >/dev/null || return 0
	timestamp="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	phase="$(resource_trace_phase)"
	docker exec "${server}" crictl stats --output json 2>/dev/null \
		| jq --compact-output --arg timestamp "${timestamp}" --arg phase "${phase}" '
      .stats[]? |
      select(.attributes.labels."io.kubernetes.pod.namespace" != null) |
      {
        timestamp: $timestamp,
        phase: $phase,
        namespace: .attributes.labels."io.kubernetes.pod.namespace",
        pod: .attributes.labels."io.kubernetes.pod.name",
        container: .attributes.metadata.name,
        working_set_bytes: ((.memory.workingSetBytes.value // 0) | tonumber)
      }
    ' >>"${RESOURCE_TRACE_DIR}/workloads.jsonl" || true
}

resource_trace_sample_volume() {
	resource_trace_enabled || return 0
	local bytes identity local_image_bytes phase retained_cluster_bytes timestamp
	cluster_volume_exists || return 0
	identity="$(cluster_volume_identity)" || return 1
	bytes="$(cluster_volume_bytes)" || return 1
	retained_cluster_bytes="$(cluster_retained_storage_bytes)" || return 1
	local_image_bytes="$(cluster_owned_image_bytes)" || return 1
	timestamp="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	phase="$(resource_trace_phase)"
	jq --null-input --compact-output \
		--arg timestamp "${timestamp}" --arg phase "${phase}" --arg identity "${identity}" \
		--argjson bytes "${bytes}" --argjson retained_cluster_bytes "${retained_cluster_bytes}" \
		--argjson local_image_bytes "${local_image_bytes}" \
		'{timestamp: $timestamp, phase: $phase, volume_identity: $identity, bytes: $bytes,
      retained_cluster_bytes: $retained_cluster_bytes, local_image_virtual_bytes: $local_image_bytes}' \
		>>"${RESOURCE_TRACE_DIR}/volume.jsonl"
}

resource_trace_require_volume_sample() {
	resource_trace_enabled || return 0
	local after before
	before="$(wc -l <"${RESOURCE_TRACE_DIR}/volume.jsonl")" || return 1
	if ! resource_trace_sample_volume; then
		resource_trace_record_sampling_failure volume
		return 1
	fi
	after="$(wc -l <"${RESOURCE_TRACE_DIR}/volume.jsonl")" || return 1
	if ((after <= before)); then
		resource_trace_record_sampling_failure volume
		return 1
	fi
}

resource_trace_record_ready_layers() {
	resource_trace_enabled || return 0
	local expected_revision="$1"
	local kustomizations="$2"
	local timestamp
	timestamp="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	jq --compact-output --arg observed_at "${timestamp}" --arg revision "${expected_revision}" '
    .items[]? |
    select(
      .status.observedGeneration == .metadata.generation and
      .status.lastAppliedRevision == $revision and
      any(.status.conditions[]?; .type == "Ready" and .status == "True")
    ) |
    {
      layer: .metadata.name,
      ready_at: $observed_at,
      observed_at: $observed_at,
      revision: .status.lastAppliedRevision
    }
  ' <<<"${kustomizations}" >>"${RESOURCE_TRACE_DIR}/layers.jsonl"
}

resource_trace_sample_loop() {
	local sample=0
	while [ ! -e "${RESOURCE_TRACE_DIR}/stop" ]; do
		resource_trace_sample_server || resource_trace_record_sampling_failure server || return 1
		if ((sample % 15 == 0)); then
			resource_trace_sample_workloads || true
		fi
		sample=$((sample + 1))
		for _ in 1 2; do
			[ ! -e "${RESOURCE_TRACE_DIR}/stop" ] || return 0
			sleep 1
		done
	done
}

resource_trace_start() {
	resource_trace_enabled || return 0
	local mode started_at trace_root
	mode=default
	[ "${FEDERATION_CONSTRAINED:-no}" = "yes" ] && mode=constrained
	trace_root="${FGENTIC_FED_TRACE_DIR:-${ROOT_DIR}/.agents/tmp/federation-resources}"
	started_at="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	RESOURCE_TRACE_DIR="${trace_root}/$(date -u +'%Y%m%dT%H%M%SZ')-${mode}"
	RESOURCE_TRACE_FINISHED=no
	mkdir -p "${RESOURCE_TRACE_DIR}"
	printf 'bootstrap\n' >"${RESOURCE_TRACE_DIR}/phase"
	jq --null-input \
		--arg cluster "${CLUSTER_NAME}" --arg mode "${mode}" --arg started_at "${started_at}" \
		'{schema: 1, cluster: $cluster, mode: $mode, started_at: $started_at,
      sample_interval_seconds: 2, workload_interval_seconds: 30, idle_settle_seconds: 300}' \
		>"${RESOURCE_TRACE_DIR}/metadata.json"
	: >"${RESOURCE_TRACE_DIR}/server.jsonl"
	: >"${RESOURCE_TRACE_DIR}/workloads.jsonl"
	: >"${RESOURCE_TRACE_DIR}/volume.jsonl"
	: >"${RESOURCE_TRACE_DIR}/layers.jsonl"
	: >"${RESOURCE_TRACE_DIR}/failures.jsonl"
	if cluster_volume_exists; then
		resource_trace_require_volume_sample || {
			echo 'error: could not capture the existing federation image volume at trace start' >&2
			return 1
		}
	fi
	resource_trace_sample_loop &
	RESOURCE_TRACE_PID=$!
	echo "Writing content-free federation resource trace to ${RESOURCE_TRACE_DIR}."
}

resource_trace_stop_sampler() {
	resource_trace_enabled || return 0
	local sampler_status
	[ -n "${RESOURCE_TRACE_PID:-}" ] || return 0
	: >"${RESOURCE_TRACE_DIR}/stop"
	if wait "${RESOURCE_TRACE_PID}" 2>/dev/null; then
		sampler_status=0
	else
		sampler_status=$?
		resource_trace_record_sampling_failure sampler || true
	fi
	RESOURCE_TRACE_PID=""
	[ "${sampler_status}" -eq 0 ]
}

resource_trace_collect_idle() {
	resource_trace_enabled || return 0
	local sample
	resource_trace_stop_sampler
	[ ! -s "${RESOURCE_TRACE_DIR}/failures.jsonl" ] || {
		echo 'error: federation resource trace missed a required boot sample' >&2
		return 1
	}
	# Reconciliation and the proof deliberately exercise controller caches. Compare sustained idle
	# working sets after the same bounded settling window in both default and constrained traces.
	echo 'Allowing five minutes for post-proof controller caches to settle...'
	sleep 300
	resource_trace_set_phase idle
	echo 'Collecting twelve content-free idle-memory samples over two minutes...'
	for sample in 1 2 3 4 5 6 7 8 9 10 11 12; do
		if ! resource_trace_sample_server; then
			resource_trace_record_sampling_failure server
			return 1
		fi
		if ((sample % 3 == 1)); then
			resource_trace_sample_workloads || true
		fi
		[ "${sample}" -eq 12 ] || sleep 10
	done
}

resource_trace_summarize() {
	resource_trace_enabled || return 0
	local finished_at local_image_bytes summary temporary
	[ -d "${RESOURCE_TRACE_DIR:-}" ] || return 0
	finished_at="$(date -u +'%Y-%m-%dT%H:%M:%SZ')"
	local_image_bytes="$(cluster_owned_image_bytes)"
	summary="${RESOURCE_TRACE_DIR}/summary.json"
	temporary="${summary}.tmp"
	rm -f "${temporary}"
	jq --null-input --slurpfile server "${RESOURCE_TRACE_DIR}/server.jsonl" \
		--slurpfile volumes "${RESOURCE_TRACE_DIR}/volume.jsonl" \
		--slurpfile layers "${RESOURCE_TRACE_DIR}/layers.jsonl" \
		--slurpfile failures "${RESOURCE_TRACE_DIR}/failures.jsonl" \
		--slurpfile metadata "${RESOURCE_TRACE_DIR}/metadata.json" \
		--arg finished_at "${finished_at}" --argjson local_image_bytes "${local_image_bytes}" '
      def median:
        sort as $values |
        ($values | length) as $count |
        if $count == 0 then null
        elif ($count % 2) == 1 then $values[($count / 2) | floor]
        else (($values[$count / 2 - 1] + $values[$count / 2]) / 2 | floor)
        end;
      if ($failures | length) > 0 then error("required sampling failed") else {
        schema: 1,
        finished_at: $finished_at,
        idle_settle_seconds: $metadata[0].idle_settle_seconds,
        boot_peak_bytes: ([$server[] | select(.phase != "idle") | .memory_bytes] | max),
        idle_median_bytes: ([$server[] | select(.phase == "idle") | .memory_bytes] | median),
        server_samples: ($server | length),
        idle_samples: ([$server[] | select(.phase == "idle")] | length),
        image_volume_peak_bytes: ([$volumes[].bytes] | max),
        retained_cluster_peak_bytes: ([$volumes[].retained_cluster_bytes] | max),
        owned_disk_upper_bound_peak_bytes:
          ([$volumes[] | (.retained_cluster_bytes + .local_image_virtual_bytes)] | max),
        local_image_bytes: $local_image_bytes,
        layers: ($layers | sort_by(.layer, .observed_at) | unique_by(.layer))
      } end
    ' >"${temporary}"
	jq --exit-status '
    .boot_peak_bytes > 0 and .idle_median_bytes > 0 and .idle_samples == 12 and
    .owned_disk_upper_bound_peak_bytes >= 0 and (.layers | length) > 0
  ' "${temporary}" >/dev/null || {
		rm -f "${temporary}"
		echo 'error: federation resource trace is incomplete' >&2
		return 1
	}
	mv "${temporary}" "${summary}"
	echo "Federation resource summary: ${summary}"
}

resource_trace_finish() {
	resource_trace_enabled || return 0
	[ -n "${RESOURCE_TRACE_DIR:-}" ] && [ -d "${RESOURCE_TRACE_DIR}" ] || return 0
	[ "${RESOURCE_TRACE_FINISHED:-no}" != "yes" ] || return 0
	resource_trace_stop_sampler
	resource_trace_require_volume_sample
	resource_trace_summarize
	RESOURCE_TRACE_FINISHED=yes
}
