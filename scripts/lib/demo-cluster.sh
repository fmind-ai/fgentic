#!/usr/bin/env bash
# Definition-only cluster lifecycle helpers sourced by scripts/demo.sh.
configure_ephemeral_flux_controllers() {
	local deployment
	local deployment_json
	local deployment_output
	local patch
	local deployments=()

	# Ephemeral profiles run on the same constrained workstation as clusters/local. Keep the
	# single-replica controllers alive through API-server I/O stalls instead of flapping every
	# dependent Kustomization after the default 15-second leader-election lease expires.
	deployment_json="$(kubectl --namespace flux-system get deployments \
		--selector app.kubernetes.io/part-of=flux --output json)" \
		|| die 'could not inspect Flux controllers'
	deployment_output="$(jq --raw-output --arg lease \
		"--leader-election-lease-duration=${FLUX_LEADER_ELECTION_LEASE_DURATION}" '
        .items[] |
        select((((.spec.template.spec.containers[0].args // []) | index($lease)) == null)) |
        .metadata.name
      ' <<<"${deployment_json}")" || die 'could not inspect Flux controller leader-election settings'
	while IFS= read -r deployment; do
		[ -z "${deployment}" ] || deployments[${#deployments[@]}]="${deployment}"
	done <<<"${deployment_output}"
	((${#deployments[@]} > 0)) || return 0

	patch="$(jq --null-input --compact-output \
		--arg lease "--leader-election-lease-duration=${FLUX_LEADER_ELECTION_LEASE_DURATION}" \
		--arg renew "--leader-election-renew-deadline=${FLUX_LEADER_ELECTION_RENEW_DEADLINE}" \
		--arg retry "--leader-election-retry-period=${FLUX_LEADER_ELECTION_RETRY_PERIOD}" '
      [
        {op: "add", path: "/spec/template/spec/containers/0/args/-", value: $lease},
        {op: "add", path: "/spec/template/spec/containers/0/args/-", value: $renew},
        {op: "add", path: "/spec/template/spec/containers/0/args/-", value: $retry}
      ]
    ')"
	for deployment in "${deployments[@]}"; do
		kubectl --namespace flux-system patch deployment "${deployment}" \
			--type json --patch "${patch}" >/dev/null
	done
	for deployment in "${deployments[@]}"; do
		kubectl --namespace flux-system rollout status deployment "${deployment}" --timeout=3m
	done
}

configure_federation_flux_controllers() {
	local deployment
	local deployment_json
	local deployment_output
	local patch
	local deployments=()

	[ "${PROFILE}" = federation ] || return 0

	# Flux's generated controller Pods see every host CPU and derive a 1 GiB Go memory target. The
	# constrained lab uses one scheduler thread and a request-sized soft heap, while the default
	# profile explicitly restores the upstream runtime and resource values after constrained reuse.
	deployment_json="$(kubectl --namespace flux-system get deployments \
		--selector app.kubernetes.io/part-of=flux --output json)" \
		|| die 'could not inspect Flux controllers for federation runtime tuning'
	deployment_output="$(jq --raw-output '.items[].metadata.name' <<<"${deployment_json}")" \
		|| die 'Flux returned an invalid controller inventory'
	while IFS= read -r deployment; do
		[ -z "${deployment}" ] || deployments[${#deployments[@]}]="${deployment}"
	done <<<"${deployment_output}"
	((${#deployments[@]} > 0)) || return 0

	if [ "${FEDERATION_CONSTRAINED}" = yes ]; then
		# Keep a 256 MiB cgroup ceiling for short reconcile bursts; GOMEMLIMIT is a soft target.
		patch='{"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"GOMAXPROCS","value":"1"},{"name":"GOGC","value":"25"},{"name":"GOMEMLIMIT","value":null,"valueFrom":{"resourceFieldRef":{"containerName":"manager","resource":"requests.memory"}}}],"resources":{"limits":{"memory":"256Mi"},"requests":{"memory":"64Mi"}}}]}}}}'
	else
		# shellcheck disable=SC2016 # $patch is the literal strategic-merge deletion key
		patch='{"spec":{"template":{"spec":{"containers":[{"name":"manager","env":[{"name":"GOMAXPROCS","$patch":"delete"},{"name":"GOGC","$patch":"delete"},{"name":"GOMEMLIMIT","value":null,"valueFrom":{"resourceFieldRef":{"containerName":"manager","resource":"limits.memory"}}}],"resources":{"limits":{"memory":"1Gi"},"requests":{"memory":"64Mi"}}}]}}}}'
	fi
	for deployment in "${deployments[@]}"; do
		kubectl --namespace flux-system patch deployment "${deployment}" \
			--type strategic --patch "${patch}" >/dev/null
	done
	for deployment in "${deployments[@]}"; do
		kubectl --namespace flux-system rollout status deployment "${deployment}" --timeout=3m
	done
}

configure_federation_metrics_server() {
	local attempt changed=no current deployment desired pods

	[ "${PROFILE}" = federation ] || return 0
	desired=1
	[ "${FEDERATION_CONSTRAINED}" = yes ] && desired=0
	deployment=""
	for attempt in 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 19 20 21 22 23 24; do
		if deployment="$(kubectl --namespace kube-system get deployment metrics-server \
			--ignore-not-found --output name 2>/dev/null)"; then
			[ -n "${deployment}" ] && break
			[ "${desired}" = 1 ] && return
		fi
		[ "${attempt}" -eq 24 ] || sleep 5
	done
	[ -n "${deployment}" ] || die 'the constrained federation profile could not find metrics-server'
	current="$(kubectl --namespace kube-system get deployment metrics-server \
		--output jsonpath='{.spec.replicas}')"
	if [ "${current}" != "${desired}" ]; then
		kubectl --namespace kube-system scale deployment metrics-server \
			--replicas "${desired}" >/dev/null
		changed=yes
	fi
	if [ "${desired}" = 0 ]; then
		if ! pods="$(kubectl --namespace kube-system get pods \
			--selector k8s-app=metrics-server --output name)"; then
			die 'could not inspect metrics-server Pods after scaling to zero'
		fi
		if [ -n "${pods}" ]; then
			kubectl --namespace kube-system wait --for=delete pod \
				--selector k8s-app=metrics-server --timeout=2m >/dev/null
		fi
		return
	fi
	[ "${changed}" = no ] \
		|| kubectl --namespace kube-system rollout status deployment/metrics-server --timeout=2m
}
random_hex() {
	openssl rand -hex "$1"
}

cluster_exists() {
	local cluster_output query_status
	cluster_output="$(k3d cluster list --output json)" \
		|| die 'could not inspect the k3d cluster inventory'
	if jq -e --arg name "${CLUSTER_NAME}" \
		'any(.[]; .name == $name)' <<<"${cluster_output}" >/dev/null; then
		return 0
	else
		query_status=$?
		[ "${query_status}" -eq 1 ] || die 'k3d returned an invalid cluster inventory'
		return 1
	fi
}

cluster_owned_by_demo() {
	[ "$(docker inspect --format '{{index .Config.Labels "dev.fgentic.demo"}}' \
		"k3d-${CLUSTER_NAME}-server-0" 2>/dev/null || true)" = "${OWNER_LABEL}" ]
}

cluster_capacity_mode() {
	docker inspect --format '{{index .Config.Labels "dev.fgentic.demo.capacity"}}' \
		"k3d-${CLUSTER_NAME}-server-0"
}

cluster_container_ids() {
	docker ps --all --filter "label=k3d.cluster=${CLUSTER_NAME}" --quiet
}

cluster_running_container_ids() {
	docker ps --filter "label=k3d.cluster=${CLUSTER_NAME}" --quiet
}

cluster_running() {
	local cluster_output query_status running running_output total total_output
	total_output="$(cluster_container_ids)" || return 2
	running_output="$(cluster_running_container_ids)" || return 2
	total="$(sort <<<"${total_output}")" || return 2
	running="$(sort <<<"${running_output}")" || return 2
	[ -n "${total}" ] && [ "${running}" = "${total}" ] || return 1
	cluster_output="$(k3d cluster list --output json)" || return 2
	if jq -e --arg name "${CLUSTER_NAME}" '
      any(.[]; .name == $name and .serversRunning == .serversCount and .serversCount > 0)
    ' <<<"${cluster_output}" >/dev/null; then
		return 0
	else
		query_status=$?
		[ "${query_status}" -eq 1 ] || return 2
		return 1
	fi
}

cluster_network_exists() {
	docker network inspect "k3d-${CLUSTER_NAME}" >/dev/null 2>&1
}

cluster_volume_exists() {
	docker volume inspect "k3d-${CLUSTER_NAME}-images" >/dev/null 2>&1
}

cluster_volume_identity() {
	local volume_output
	volume_output="$(docker volume inspect "k3d-${CLUSTER_NAME}-images" 2>/dev/null)" || return 1
	jq --exit-status --raw-output --arg cluster "${CLUSTER_NAME}" '
      .[0] |
      select(.Labels.app == "k3d" and .Labels."k3d.cluster" == $cluster) |
      (.Name + "@" + .CreatedAt)
    ' <<<"${volume_output}"
}

docker_volume_bytes() {
	local bytes du_output mountpoint reader_image volume="$1"
	docker volume inspect "${volume}" >/dev/null 2>&1 || return 1
	# Linux exposes the named-volume mount directly. Docker Desktop does not, so use an already
	# present lab image as a pull-free, network-free reader while the cluster is stopped.
	mountpoint="$(docker volume inspect --format '{{.Mountpoint}}' \
		"${volume}" 2>/dev/null || true)"
	if [ -n "${mountpoint}" ] && [ -d "${mountpoint}" ] && du -sb "${mountpoint}" >/dev/null 2>&1; then
		du_output="$(du -sb "${mountpoint}")" || return 1
		bytes="$(awk 'NR == 1 { print $1 }' <<<"${du_output}")"
		[[ "${bytes}" =~ ^[0-9]+$ ]] || return 1
		printf '%s\n' "${bytes}"
		return
	fi
	reader_image="${SOURCE_BASE_IMAGE:-}"
	if [ -z "${reader_image}" ] || ! docker image inspect "${reader_image}" >/dev/null 2>&1; then
		reader_image="$(docker inspect --format '{{.Image}}' \
			"k3d-${CLUSTER_NAME}-server-0" 2>/dev/null || true)"
	fi
	[ -n "${reader_image}" ] || return 1
	docker image inspect "${reader_image}" >/dev/null 2>&1 || return 1
	du_output="$(docker run --rm --pull never --network none --entrypoint /bin/du \
		--volume "${volume}:/volume:ro" "${reader_image}" -sb /volume \
		2>/dev/null)" || return 1
	bytes="$(awk 'NR == 1 { print $1 }' <<<"${du_output}")"
	[[ "${bytes}" =~ ^[0-9]+$ ]] || return 1
	printf '%s\n' "${bytes}"
}

cluster_volume_bytes() {
	docker_volume_bytes "k3d-${CLUSTER_NAME}-images"
}

cluster_attached_volume_names() {
	local container_id container_output inspect_output volume_output
	local container_ids=()
	container_output="$(cluster_container_ids)" || return 1
	while IFS= read -r container_id; do
		[ -z "${container_id}" ] || container_ids[${#container_ids[@]}]="${container_id}"
	done <<<"${container_output}"
	((${#container_ids[@]} > 0)) || return 0
	inspect_output="$(docker container inspect "${container_ids[@]}")" || return 1
	volume_output="$(jq --raw-output \
		'.[].Mounts[]? | select(.Type == "volume") | .Name' <<<"${inspect_output}")" || return 1
	volume_output="$(awk 'NF' <<<"${volume_output}")" || return 1
	[ -n "${volume_output}" ] || return 0
	sort -u <<<"${volume_output}"
}

cluster_retained_storage_bytes() {
	local attached_volumes bytes container_id container_output total=0 volume
	attached_volumes="$(cluster_attached_volume_names)" || return 1
	while IFS= read -r volume; do
		[ -n "${volume}" ] || continue
		bytes="$(docker_volume_bytes "${volume}")" || return 1
		total=$((total + bytes))
	done <<<"${attached_volumes}"
	container_output="$(cluster_container_ids)" || return 1
	while IFS= read -r container_id; do
		[ -n "${container_id}" ] || continue
		bytes="$(docker container inspect --size --format '{{.SizeRw}}' "${container_id}")" \
			|| return 1
		[[ "${bytes}" =~ ^[0-9]+$ ]] || return 1
		total=$((total + bytes))
	done <<<"${container_output}"
	printf '%s\n' "${total}"
}

cluster_owned_image_ids() {
	local image_output
	image_output="$(docker images \
		--filter "label=dev.fgentic.demo.cluster=${CLUSTER_NAME}" --quiet)" || return 1
	image_output="$(awk 'NF' <<<"${image_output}")" || return 1
	[ -n "${image_output}" ] || return 0
	sort -u <<<"${image_output}"
}

cluster_owned_image_bytes() {
	local image_id image_output size total=0
	image_output="$(cluster_owned_image_ids)" || return 1
	while IFS= read -r image_id; do
		[ -n "${image_id}" ] || continue
		size="$(docker image inspect --format '{{.Size}}' "${image_id}")" || return 1
		[[ "${size}" =~ ^[0-9]+$ ]] || return 1
		total=$((total + size))
	done <<<"${image_output}"
	printf '%s\n' "${total}"
}

teardown_receipt_path() {
	local state_root
	state_root="${FGENTIC_DEMO_STATE_DIR:-${XDG_STATE_HOME:-${HOME:?}/.local/state}/fgentic}"
	printf '%s/cluster-teardown/%s.json\n' "${state_root}" "${CLUSTER_NAME}"
}

teardown_receipt_exists() {
	local receipt
	receipt="$(teardown_receipt_path)"
	[ -e "${receipt}" ]
}

validate_teardown_receipt_file() {
	local receipt="$1"
	[ -f "${receipt}" ] && [ ! -L "${receipt}" ] || return 1
	jq --exit-status --arg cluster "${CLUSTER_NAME}" --arg owner "${OWNER_LABEL}" \
		--arg profile "${PROFILE}" '
      . as $receipt |
      (keys == ["cluster", "containers", "generation", "images", "network", "owner", "profile", "schema", "volumes"]) and
      (.schema == "fgentic.cluster-teardown.v1") and
      (.cluster == $cluster) and
      (.owner == $owner) and
      (.profile == $profile) and
      (.generation | type == "string" and length > 0) and
      (.containers | type == "array" and length > 0) and
      (all(.containers[];
        (keys == ["id", "name"]) and
        (.id | type == "string" and length > 0) and
        (.name | type == "string" and length > 0))) and
      ([.containers[] | select(.id == $receipt.generation and .name == ("k3d-" + $cluster + "-server-0"))] | length == 1) and
      (.network | type == "object") and
      (.network | keys == ["cluster_label", "id", "name"]) and
      (.network.id | type == "string" and length > 0) and
      (.network.name == ("k3d-" + $cluster)) and
      (.network.cluster_label == "" or .network.cluster_label == $cluster) and
      (.volumes | type == "array" and length > 0) and
      (all(.volumes[];
        (keys == ["attachments", "created_at", "kind", "name"]) and
        (.name | type == "string" and length > 0) and
        (.created_at | type == "string" and length > 0) and
        (.kind == "images" or .kind == "anonymous") and
        (.attachments | type == "array" and length > 0) and
        (all(.attachments[]; type == "string" and length > 0)) and
        (all(.attachments[]; . as $id | any($receipt.containers[]; .id == $id))) and
        (if .kind == "images" then .name == ("k3d-" + $cluster + "-images") else true end))) and
      ([.volumes[] | select(.kind == "images")] | length == 1) and
      (.images | type == "array") and
      (all(.images[];
        (keys == ["id", "repo_tags"]) and
        (.id | type == "string" and length > 0) and
        (.repo_tags | type == "array") and
        (all(.repo_tags[]; type == "string" and length > 0))))
    ' "${receipt}" >/dev/null 2>&1
}

teardown_receipt_fail() {
	local receipt
	receipt="$(teardown_receipt_path)"
	echo "error: $*" >&2
	echo "error: no resource was adopted; inspect only: jq . ${receipt}" >&2
	exit 1
}

require_valid_teardown_receipt() {
	local receipt
	receipt="$(teardown_receipt_path)"
	validate_teardown_receipt_file "${receipt}" \
		|| teardown_receipt_fail "malformed or stale teardown receipt for ${CLUSTER_NAME}"
}

require_no_pending_teardown() {
	local operation="$1"
	teardown_receipt_exists || return 0
	require_valid_teardown_receipt
	die "${CLUSTER_NAME} teardown recovery is pending; run the matching down command before ${operation}"
}

write_teardown_receipt() {
	local container_id container_output image_id image_output network_output receipt state_dir
	local server_id temporary volume name volume_output
	local container_ids=()
	local image_ids=()
	local volume_names=()
	receipt="$(teardown_receipt_path)"
	state_dir="${receipt%/*}"
	[ ! -e "${receipt}" ] || teardown_receipt_fail "teardown receipt already exists for ${CLUSTER_NAME}"

	container_output="$(cluster_container_ids)" \
		|| die "could not inspect ${CLUSTER_NAME} containers before receipt creation"
	while IFS= read -r container_id; do
		[ -z "${container_id}" ] || container_ids[${#container_ids[@]}]="${container_id}"
	done <<<"${container_output}"
	((${#container_ids[@]} > 0)) || die "${CLUSTER_NAME} has no containers to record"
	container_output="$(docker container inspect "${container_ids[@]}")" \
		|| die "could not capture ${CLUSTER_NAME} container identities"
	server_id="$(jq --exit-status --raw-output --arg cluster "${CLUSTER_NAME}" \
		--arg owner "${OWNER_LABEL}" '
      [.[] | select(
        (.Name | ltrimstr("/")) == ("k3d-" + $cluster + "-server-0") and
        .Config.Labels."k3d.cluster" == $cluster and
        .Config.Labels."dev.fgentic.demo" == $owner
      )] | if length == 1 then .[0].Id else empty end
    ' <<<"${container_output}")" || die "could not prove ${CLUSTER_NAME} receipt generation"
	jq --exit-status --arg cluster "${CLUSTER_NAME}" '
      all(.[]; .Config.Labels."k3d.cluster" == $cluster)
    ' <<<"${container_output}" >/dev/null \
		|| die "refusing to record containers outside ${CLUSTER_NAME}"

	network_output="$(docker network inspect "k3d-${CLUSTER_NAME}")" \
		|| die "could not capture ${CLUSTER_NAME} network identity"
	jq --exit-status --arg cluster "${CLUSTER_NAME}" '
      length == 1 and
      .[0].Name == ("k3d-" + $cluster) and
      .[0].Labels.app == "k3d" and
      ((.[0].Labels."k3d.cluster" // "") == "" or
       .[0].Labels."k3d.cluster" == $cluster)
    ' <<<"${network_output}" >/dev/null \
		|| die "refusing to record foreign network k3d-${CLUSTER_NAME}"

	volume_output="$(cluster_attached_volume_names)" \
		|| die "could not inspect ${CLUSTER_NAME} attached volumes before receipt creation"
	while IFS= read -r name; do
		[ -z "${name}" ] || volume_names[${#volume_names[@]}]="${name}"
	done <<<"${volume_output}"
	((${#volume_names[@]} > 0)) || die "${CLUSTER_NAME} has no attached volumes to record"
	volume_output="$(docker volume inspect "${volume_names[@]}")" \
		|| die "could not capture ${CLUSTER_NAME} volume identities"
	jq --exit-status --arg cluster "${CLUSTER_NAME}" '
      all(.[];
        if .Name == ("k3d-" + $cluster + "-images") then
          .Labels.app == "k3d" and .Labels."k3d.cluster" == $cluster
        else
          ((.Labels // {}) | has("com.docker.volume.anonymous"))
        end)
    ' <<<"${volume_output}" >/dev/null \
		|| die "refusing to record foreign volume attached to ${CLUSTER_NAME}"

	image_output="$(cluster_owned_image_ids)" \
		|| die "could not inspect ${CLUSTER_NAME} local images before receipt creation"
	while IFS= read -r image_id; do
		[ -z "${image_id}" ] || image_ids[${#image_ids[@]}]="${image_id}"
	done <<<"${image_output}"
	if ((${#image_ids[@]} > 0)); then
		image_output="$(docker image inspect "${image_ids[@]}")" \
			|| die "could not capture ${CLUSTER_NAME} local-image identities"
		jq --exit-status --arg cluster "${CLUSTER_NAME}" '
        all(.[]; .Config.Labels."dev.fgentic.demo.cluster" == $cluster)
      ' <<<"${image_output}" >/dev/null \
			|| die "refusing to record a foreign local image for ${CLUSTER_NAME}"
	else
		image_output='[]'
	fi

	mkdir -p "${state_dir}" \
		|| die "could not create ${CLUSTER_NAME} teardown state directory"
	chmod 700 "${state_dir}" \
		|| die "could not protect ${CLUSTER_NAME} teardown state directory"
	temporary="$(mktemp "${state_dir}/.${CLUSTER_NAME}.XXXXXX")" \
		|| die "could not create temporary ${CLUSTER_NAME} teardown receipt"
	chmod 600 "${temporary}" || {
		rm -f "${temporary}"
		die "could not protect temporary ${CLUSTER_NAME} teardown receipt"
	}
	if ! jq --null-input \
		--arg schema 'fgentic.cluster-teardown.v1' \
		--arg profile "${PROFILE}" --arg cluster "${CLUSTER_NAME}" \
		--arg owner "${OWNER_LABEL}" --arg generation "${server_id}" \
		--argjson containers "${container_output}" \
		--argjson network "${network_output}" \
		--argjson volumes "${volume_output}" \
		--argjson images "${image_output}" '
      {
        schema: $schema,
        profile: $profile,
        cluster: $cluster,
        owner: $owner,
        generation: $generation,
        containers: ($containers | map({id: .Id, name: (.Name | ltrimstr("/"))}) | sort_by(.name)),
        network: ($network[0] | {
          id: .Id,
          name: .Name,
          cluster_label: (.Labels."k3d.cluster" // "")
        }),
        volumes: ($volumes | map(. as $volume | {
          name: .Name,
          created_at: .CreatedAt,
          kind: (if .Name == ("k3d-" + $cluster + "-images") then "images" else "anonymous" end),
          attachments: ($containers | [
            .[] |
            select(any(.Mounts[]?; .Type == "volume" and .Name == $volume.Name)) |
            .Id
          ] | sort)
        }) | sort_by(.name)),
        images: ($images | map({id: .Id, repo_tags: ((.RepoTags // []) | sort)}) | sort_by(.id))
      }
    ' >"${temporary}"; then
		rm -f "${temporary}"
		die "could not construct ${CLUSTER_NAME} teardown receipt"
	fi
	if ! validate_teardown_receipt_file "${temporary}"; then
		rm -f "${temporary}"
		die "refusing to persist an invalid ${CLUSTER_NAME} teardown receipt"
	fi
	if ! mv "${temporary}" "${receipt}"; then
		rm -f "${temporary}"
		die "could not atomically persist ${CLUSTER_NAME} teardown receipt"
	fi
}

validate_receipt_container() {
	local actual actual_id generation id name object="$1" receipt
	id="$(jq --raw-output '.id' <<<"${object}")"
	name="$(jq --raw-output '.name' <<<"${object}")"
	if actual="$(docker container inspect "${id}" 2>/dev/null)"; then
		jq --exit-status --arg id "${id}" --arg name "${name}" \
			--arg cluster "${CLUSTER_NAME}" '
          length == 1 and .[0].Id == $id and
          (.[0].Name | ltrimstr("/")) == $name and
          .[0].Config.Labels."k3d.cluster" == $cluster
        ' <<<"${actual}" >/dev/null \
			|| teardown_receipt_fail "container identity or ownership changed for ${name}"
		receipt="$(teardown_receipt_path)"
		generation="$(jq --raw-output '.generation' "${receipt}")"
		if [ "${id}" = "${generation}" ]; then
			jq --exit-status --arg owner "${OWNER_LABEL}" \
				'.[0].Config.Labels."dev.fgentic.demo" == $owner' \
				<<<"${actual}" >/dev/null \
				|| teardown_receipt_fail "server ownership changed for ${name}"
		fi
		return 0
	fi
	if actual="$(docker container inspect "${name}" 2>/dev/null)"; then
		actual_id="$(jq --raw-output '.[0].Id' <<<"${actual}")"
		teardown_receipt_fail "container name ${name} was reused by ${actual_id}"
	fi
	return 1
}

validate_receipt_network() {
	local actual actual_id cluster_label id name object="$1"
	id="$(jq --raw-output '.id' <<<"${object}")"
	name="$(jq --raw-output '.name' <<<"${object}")"
	cluster_label="$(jq --raw-output '.cluster_label' <<<"${object}")"
	if actual="$(docker network inspect "${id}" 2>/dev/null)"; then
		jq --exit-status --arg id "${id}" --arg name "${name}" \
			--arg cluster_label "${cluster_label}" '
          length == 1 and .[0].Id == $id and .[0].Name == $name and
          (.[0].Labels."k3d.cluster" // "") == $cluster_label and
          .[0].Labels.app == "k3d"
        ' <<<"${actual}" >/dev/null \
			|| teardown_receipt_fail "network identity or ownership changed for ${name}"
		return 0
	fi
	if actual="$(docker network inspect "${name}" 2>/dev/null)"; then
		actual_id="$(jq --raw-output '.[0].Id' <<<"${actual}")"
		teardown_receipt_fail "network name ${name} was reused by ${actual_id}"
	fi
	return 1
}

validate_receipt_volume() {
	local actual actual_created actual_kind attachment_id attachments created kind name object="$1"
	name="$(jq --raw-output '.name' <<<"${object}")"
	created="$(jq --raw-output '.created_at' <<<"${object}")"
	kind="$(jq --raw-output '.kind' <<<"${object}")"
	if ! actual="$(docker volume inspect "${name}" 2>/dev/null)"; then
		return 1
	fi
	actual_created="$(jq --raw-output '.[0].CreatedAt' <<<"${actual}")"
	[ "${actual_created}" = "${created}" ] \
		|| teardown_receipt_fail "volume name ${name} was reused with a different creation identity"
	if jq --exit-status '.[0].Labels.app == "k3d"' <<<"${actual}" >/dev/null; then
		actual_kind=images
	elif jq --exit-status '((.[0].Labels // {}) | has("com.docker.volume.anonymous"))' \
		<<<"${actual}" >/dev/null; then
		actual_kind=anonymous
	else
		teardown_receipt_fail "volume ownership changed for ${name}"
	fi
	[ "${actual_kind}" = "${kind}" ] \
		|| teardown_receipt_fail "volume kind changed for ${name}"
	if [ "${kind}" = images ]; then
		jq --exit-status --arg cluster "${CLUSTER_NAME}" \
			'.[0].Labels."k3d.cluster" == $cluster' <<<"${actual}" >/dev/null \
			|| teardown_receipt_fail "image-volume ownership changed for ${name}"
	fi
	attachments="$(jq --raw-output '.attachments[]' <<<"${object}")" \
		|| teardown_receipt_fail "volume receipt attachments are invalid for ${name}"
	while IFS= read -r attachment_id; do
		[ -n "${attachment_id}" ] || continue
		if actual="$(docker container inspect "${attachment_id}" 2>/dev/null)"; then
			jq --exit-status --arg volume "${name}" \
				'any(.[0].Mounts[]?; .Type == "volume" and .Name == $volume)' \
				<<<"${actual}" >/dev/null \
				|| teardown_receipt_fail "recorded attachment from ${attachment_id} to ${name} changed"
		fi
	done <<<"${attachments}"
	return 0
}

validate_receipt_image() {
	local actual actual_id id object="$1" ref repo_tags
	id="$(jq --raw-output '.id' <<<"${object}")"
	if actual="$(docker image inspect "${id}" 2>/dev/null)"; then
		actual_id="$(jq --raw-output '.[0].Id' <<<"${actual}")"
		if [ "${actual_id}" != "${id}" ] \
			|| ! jq --exit-status --arg cluster "${CLUSTER_NAME}" \
				'.[0].Config.Labels."dev.fgentic.demo.cluster" == $cluster' \
				<<<"${actual}" >/dev/null; then
			teardown_receipt_fail "local-image identity or ownership changed for ${id}"
		fi
	fi
	repo_tags="$(jq --raw-output '.repo_tags[]' <<<"${object}")" \
		|| teardown_receipt_fail "local-image receipt references are invalid for ${id}"
	while IFS= read -r ref; do
		[ -n "${ref}" ] || continue
		if actual="$(docker image inspect "${ref}" 2>/dev/null)"; then
			actual_id="$(jq --raw-output '.[0].Id' <<<"${actual}")"
			[ "${actual_id}" = "${id}" ] \
				|| teardown_receipt_fail "local-image reference ${ref} was reused by ${actual_id}"
		fi
	done <<<"${repo_tags}"
	docker image inspect "${id}" >/dev/null 2>&1
}

validate_teardown_receipt_resources() {
	local cluster_status containers generation generation_present=no images network object object_id
	local receipt volumes
	receipt="$(teardown_receipt_path)"
	generation="$(jq --raw-output '.generation' "${receipt}")"
	containers="$(jq --compact-output '.containers[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt container inventory is invalid'
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		if validate_receipt_container "${object}"; then
			object_id="$(jq --raw-output '.id' <<<"${object}")" \
				|| teardown_receipt_fail 'teardown receipt container identity is invalid'
			[ "${object_id}" != "${generation}" ] \
				|| generation_present=yes
		fi
	done <<<"${containers}"
	network="$(jq --compact-output '.network' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt network inventory is invalid'
	validate_receipt_network "${network}" || true
	volumes="$(jq --compact-output '.volumes[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt volume inventory is invalid'
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		validate_receipt_volume "${object}" || true
	done <<<"${volumes}"
	images="$(jq --compact-output '.images[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt image inventory is invalid'
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		validate_receipt_image "${object}" || true
	done <<<"${images}"
	if cluster_exists; then
		[ "${generation_present}" = yes ] \
			|| teardown_receipt_fail "live k3d metadata no longer matches receipt generation ${generation}"
	else
		cluster_status=$?
		[ "${cluster_status}" -eq 1 ] \
			|| teardown_receipt_fail "could not inspect k3d metadata during recovery"
	fi
}

teardown_receipt_complete() {
	local cluster_status containers images network object receipt volumes
	receipt="$(teardown_receipt_path)"
	if cluster_exists; then
		return 1
	else
		cluster_status=$?
		[ "${cluster_status}" -eq 1 ] || return "${cluster_status}"
	fi
	containers="$(jq --compact-output '.containers[]' "${receipt}")" || return 2
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		validate_receipt_container "${object}" && return 1
	done <<<"${containers}"
	network="$(jq --compact-output '.network' "${receipt}")" || return 2
	validate_receipt_network "${network}" && return 1
	volumes="$(jq --compact-output '.volumes[]' "${receipt}")" || return 2
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		validate_receipt_volume "${object}" && return 1
	done <<<"${volumes}"
	images="$(jq --compact-output '.images[]' "${receipt}")" || return 2
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		validate_receipt_image "${object}" && return 1
	done <<<"${images}"
	return 0
}

print_teardown_recovery_diagnostics() {
	local containers id images name network_id object receipt volumes
	receipt="$(teardown_receipt_path)"
	echo "error: ${CLUSTER_NAME} teardown remains pending; inspect exact recorded identities:" >&2
	echo "  jq . ${receipt}" >&2
	containers="$(jq --compact-output '.containers[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt container inventory is invalid'
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		id="$(jq --raw-output '.id' <<<"${object}")" \
			|| teardown_receipt_fail 'teardown receipt container identity is invalid'
		echo "  docker container inspect ${id}" >&2
	done <<<"${containers}"
	network_id="$(jq --raw-output '.network.id' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt network identity is invalid'
	echo "  docker network inspect ${network_id}" >&2
	volumes="$(jq --compact-output '.volumes[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt volume inventory is invalid'
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		name="$(jq --raw-output '.name' <<<"${object}")" \
			|| teardown_receipt_fail 'teardown receipt volume identity is invalid'
		echo "  docker volume inspect ${name}" >&2
	done <<<"${volumes}"
	images="$(jq --compact-output '.images[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt image inventory is invalid'
	while IFS= read -r object; do
		[ -n "${object}" ] || continue
		id="$(jq --raw-output '.id' <<<"${object}")" \
			|| teardown_receipt_fail 'teardown receipt image identity is invalid'
		echo "  docker image inspect ${id}" >&2
	done <<<"${images}"
}

recover_teardown_receipt() {
	local attempt cluster_status containers id images name network object receipt status volumes
	receipt="$(teardown_receipt_path)"
	require_valid_teardown_receipt
	validate_teardown_receipt_resources
	if cluster_exists; then
		k3d cluster delete "${CLUSTER_NAME}" || true
	else
		cluster_status=$?
		[ "${cluster_status}" -eq 1 ] \
			|| teardown_receipt_fail "could not inspect k3d metadata before recovery"
	fi
	containers="$(jq --compact-output '.containers[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt container inventory is invalid'
	network="$(jq --compact-output '.network' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt network inventory is invalid'
	volumes="$(jq --compact-output '.volumes[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt volume inventory is invalid'
	images="$(jq --compact-output '.images[]' "${receipt}")" \
		|| teardown_receipt_fail 'teardown receipt image inventory is invalid'

	for attempt in 1 2 3; do
		while IFS= read -r object; do
			[ -n "${object}" ] || continue
			if validate_receipt_container "${object}"; then
				id="$(jq --raw-output '.id' <<<"${object}")"
				docker rm --force --volumes "${id}" >/dev/null 2>&1 || true
			fi
		done <<<"${containers}"
		if validate_receipt_network "${network}"; then
			id="$(jq --raw-output '.id' <<<"${network}")" \
				|| teardown_receipt_fail 'teardown receipt network identity is invalid'
			docker network rm "${id}" >/dev/null 2>&1 || true
		fi
		while IFS= read -r object; do
			[ -n "${object}" ] || continue
			if validate_receipt_volume "${object}"; then
				name="$(jq --raw-output '.name' <<<"${object}")" \
					|| teardown_receipt_fail 'teardown receipt volume identity is invalid'
				docker volume rm "${name}" >/dev/null 2>&1 || true
			fi
		done <<<"${volumes}"
		while IFS= read -r object; do
			[ -n "${object}" ] || continue
			if validate_receipt_image "${object}"; then
				id="$(jq --raw-output '.id' <<<"${object}")" \
					|| teardown_receipt_fail 'teardown receipt image identity is invalid'
				docker image rm --force "${id}" >/dev/null 2>&1 || true
			fi
		done <<<"${images}"
		if teardown_receipt_complete; then
			rm -f "${receipt}"
			rmdir "${receipt%/*}" >/dev/null 2>&1 || true
			return 0
		else
			status=$?
			[ "${status}" -eq 1 ] || return "${status}"
		fi
		[ "${attempt}" -eq 3 ] || sleep 2
	done
	print_teardown_recovery_diagnostics
	return 1
}

require_owned_evaluation_cluster() {
	cluster_exists || die "${CLUSTER_NAME} does not exist"
	cluster_owned_by_demo \
		|| die "refusing to manage ${CLUSTER_NAME}: it was not created by scripts/demo.sh"
	cluster_volume_identity >/dev/null \
		|| die "refusing to manage ${CLUSTER_NAME}: its image volume is missing or foreign"
}

cluster_runtime_artifacts_exist() {
	local container_output
	container_output="$(cluster_container_ids)" || return 2
	[ -n "${container_output}" ] || cluster_network_exists || cluster_volume_exists
}

cluster_artifacts_exist() {
	local artifact_status image_output
	if cluster_runtime_artifacts_exist; then
		return 0
	else
		artifact_status=$?
		[ "${artifact_status}" -eq 1 ] || return "${artifact_status}"
	fi
	image_output="$(cluster_owned_image_ids)" || return 2
	[ -n "${image_output}" ]
}

prune_owned_host_images() {
	local image_output image_ref remaining_output repository="$1"
	local image_refs=()
	image_output="$(docker images \
		--filter "label=dev.fgentic.demo.cluster=${CLUSTER_NAME}" \
		--filter "reference=${repository}:*" --format '{{.Repository}}:{{.Tag}}')" \
		|| die "could not inspect ${CLUSTER_NAME} host images for ${repository}"
	while IFS= read -r image_ref; do
		[ -z "${image_ref}" ] || image_refs[${#image_refs[@]}]="${image_ref}"
	done <<<"${image_output}"
	((${#image_refs[@]} > 0)) || return 0
	docker image rm "${image_refs[@]}" >/dev/null \
		|| die "could not remove imported ${CLUSTER_NAME} host images for ${repository}"
	remaining_output="$(docker images \
		--filter "label=dev.fgentic.demo.cluster=${CLUSTER_NAME}" \
		--filter "reference=${repository}:*" --quiet)" \
		|| die "could not verify ${CLUSTER_NAME} host-image pruning for ${repository}"
	[ -z "${remaining_output}" ] \
		|| die "stale ${CLUSTER_NAME} host images remain for ${repository}"
}

prune_stale_node_images() {
	local active_image="$1" active_ref image_output node node_output repository stale_output
	local stale_ref
	local nodes=()
	repository="${active_image%:*}"
	active_ref="docker.io/library/${active_image}"
	node_output="$(docker ps --filter "label=k3d.cluster=${CLUSTER_NAME}" \
		--format '{{.Names}}')" || die "could not inspect ${CLUSTER_NAME} runtime nodes"
	while IFS= read -r node; do
		case "${node}" in
			*-server-[0-9]* | *-agent-[0-9]*) nodes[${#nodes[@]}]="${node}" ;;
			*) continue ;;
		esac
	done <<<"${node_output}"
	((${#nodes[@]} > 0)) || die "${CLUSTER_NAME} has no running runtime node"
	for node in "${nodes[@]}"; do
		image_output="$(docker exec "${node}" crictl images --output json)" \
			|| die "could not inspect imported images on ${node}"
		jq --exit-status --arg active "${active_ref}" \
			'any(.images[].repoTags[]?; . == $active)' <<<"${image_output}" >/dev/null \
			|| die "active imported image ${active_image} is missing from ${node}"
		stale_output="$(jq --raw-output --arg active "${active_ref}" \
			--arg prefix "docker.io/library/${repository}:" '
          .images[].repoTags[]? |
          select(startswith($prefix) and . != $active)
        ' <<<"${image_output}")" || die "could not parse imported images on ${node}"
		while IFS= read -r stale_ref; do
			[ -z "${stale_ref}" ] \
				|| docker exec "${node}" crictl rmi "${stale_ref}" >/dev/null \
				|| die "could not prune stale image ${stale_ref} from ${node}"
		done <<<"${stale_output}"
		image_output="$(docker exec "${node}" crictl images --output json)" \
			|| die "could not verify imported images on ${node}"
		jq --exit-status --arg active "${active_ref}" \
			--arg prefix "docker.io/library/${repository}:" '
          all(.images[].repoTags[]?; . as $ref |
            (($ref | startswith($prefix) | not) or $ref == $active))
        ' <<<"${image_output}" >/dev/null \
			|| die "stale ${repository} images remain on ${node}"
	done
}

build_and_load_images() {
	cat >"${SOURCE_CONTEXT}/Dockerfile" <<EOF
FROM ${SOURCE_BASE_IMAGE}
RUN apk add --no-cache ${SOURCE_GIT_PACKAGES}
RUN mkdir -p /www/cgi-bin && ln -s /usr/libexec/git-core/git-http-backend /www/cgi-bin/git
COPY --chown=65532:65532 repo.git /srv/repo.git
ENV GIT_PROJECT_ROOT=/srv GIT_HTTP_EXPORT_ALL=1
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["httpd", "-f", "-v", "-p", "8080", "-h", "/www"]
EOF

	build_image "${SOURCE_IMAGE}" "${SOURCE_CONTEXT}/Dockerfile" "${SOURCE_CONTEXT}" source
	if [ "${PROFILE}" = "demo" ] || [ "${PROFILE}" = "federation" ]; then
		build_image "${BRIDGE_IMAGE}" "${ROOT_DIR}/apps/matrix-a2a-bridge/Dockerfile" \
			"${ROOT_DIR}/apps/matrix-a2a-bridge" bridge
	fi
	# The source is the first side-loaded workload requested; delay only the bridge image through
	# the much longer platform dependency chain.
	k3d image import --mode auto --cluster "${CLUSTER_NAME}" "${SOURCE_IMAGE}" >/dev/null
	resource_trace_require_volume_sample
	# k3d copied the image into its owned image volume. Dropping the transient host image prevents
	# random-tagged images from accumulating on every stop/up reuse, including pre-fix leftovers.
	prune_owned_host_images "${SOURCE_IMAGE%:*}"
}

load_bridge_image_if_requested() {
	local requested_image workload_json
	case "${PROFILE}" in
		demo)
			workload_json="$(kubectl --namespace bridge get helmrelease \
				matrix-a2a-bridge --output json 2>/dev/null)" || return 1
			requested_image="$(jq --exit-status --raw-output \
				'.spec.values.image | "\(.repository):\(.tag)"' \
				<<<"${workload_json}" 2>/dev/null)" || return 1
			;;
		federation)
			workload_json="$(kubectl --namespace agentgateway-system get deployment \
				federation-usage-receipt --output json 2>/dev/null)" || return 1
			requested_image="$(jq --exit-status --raw-output \
				'.spec.template.spec.containers[] | select(.name == "usage-receipt") | .image' \
				<<<"${workload_json}" 2>/dev/null)" || return 1
			;;
		*) return 0 ;;
	esac
	[ "${requested_image}" = "${BRIDGE_IMAGE}" ] || return 1

	# Loading only after Flux applies the exact workload image leaves the long dependency wait
	# behind us and can precede Pod creation, narrowing the unused pullPolicy=Never image window.
	k3d image import --mode auto --cluster "${CLUSTER_NAME}" "${BRIDGE_IMAGE}" >/dev/null || return 2
	resource_trace_require_volume_sample
	prune_owned_host_images "${BRIDGE_IMAGE%:*}"
}

build_image() {
	local image="$1"
	local dockerfile="$2"
	local context="$3"
	local cache_name="$4"
	if [ -z "${FGENTIC_DEMO_CACHE_DIR:-}" ]; then
		docker build --quiet --tag "${image}" \
			--label "dev.fgentic.demo.cluster=${CLUSTER_NAME}" \
			--file "${dockerfile}" "${context}" >/dev/null
		return
	fi

	local cache_dir="${FGENTIC_DEMO_CACHE_DIR%/}/${cache_name}"
	local next_cache="${cache_dir}.next-${BRIDGE_TAG}"
	mkdir -p "${FGENTIC_DEMO_CACHE_DIR}"
	rm -rf "${next_cache}"
	local cache_from=()
	if [ -f "${cache_dir}/index.json" ]; then
		cache_from=(--cache-from "type=local,src=${cache_dir}")
	fi
	docker buildx build --quiet --load --tag "${image}" \
		--label "dev.fgentic.demo.cluster=${CLUSTER_NAME}" --file "${dockerfile}" \
		"${cache_from[@]}" \
		--cache-to "type=local,dest=${next_cache},mode=max" \
		"${context}" >/dev/null
	rm -rf "${cache_dir}"
	mv "${next_cache}" "${cache_dir}"
}

apply_source_server() {
	local actual_revision
	local expected_revision="main@sha1:${SOURCE_REVISION}"
	local source_deadline
	kubectl apply --filename - >/dev/null <<EOF
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fgentic-demo-source
  namespace: flux-system
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/name: fgentic-demo-source
  template:
    metadata:
      labels:
        app.kubernetes.io/name: fgentic-demo-source
    spec:
      automountServiceAccountToken: false
      securityContext:
        runAsNonRoot: true
        runAsUser: 65532
        runAsGroup: 65532
        seccompProfile: {type: RuntimeDefault}
      containers:
        - name: source
          image: ${SOURCE_IMAGE}
          imagePullPolicy: Never
          ports:
            - {name: http, containerPort: 8080}
          readinessProbe:
            tcpSocket: {port: http}
            periodSeconds: 2
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: {drop: [ALL]}
          resources:
            requests: {cpu: 5m, memory: 16Mi}
            limits: {cpu: 50m, memory: 48Mi}
---
apiVersion: v1
kind: Service
metadata:
  name: fgentic-demo-source
  namespace: flux-system
spec:
  selector:
    app.kubernetes.io/name: fgentic-demo-source
  ports:
    - {name: http, port: 8080, targetPort: http}
---
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata:
  name: fgentic-demo-source
  namespace: flux-system
spec:
  podSelector:
    matchLabels:
      app.kubernetes.io/name: fgentic-demo-source
  policyTypes: [Ingress]
  ingress:
    - from:
        - podSelector:
            matchLabels:
              app.kubernetes.io/component: source-controller
      ports:
        - {protocol: TCP, port: 8080}
EOF
	kubectl --namespace flux-system rollout status deployment/fgentic-demo-source --timeout=2m

	kubectl apply --filename - >/dev/null <<'EOF'
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: flux-system
  namespace: flux-system
spec:
  interval: 1m
  url: http://fgentic-demo-source.flux-system.svc.cluster.local:8080/cgi-bin/git/repo.git
  ref:
    branch: main
EOF
	# A replaced in-cluster source pod can briefly serve the previous Git artifact through an
	# existing connection. Reconcile until Flux proves it fetched this snapshot's exact commit.
	actual_revision=""
	source_deadline=$((SECONDS + 120))
	while ((SECONDS < source_deadline)); do
		if flux reconcile source git flux-system --timeout=2m >/dev/null \
			&& actual_revision="$(kubectl --namespace flux-system get gitrepository flux-system \
				--output jsonpath='{.status.artifact.revision}')" \
			&& [ "${actual_revision}" = "${expected_revision}" ]; then
			break
		fi
		sleep 2
	done
	[ "${actual_revision}" = "${expected_revision}" ] \
		|| die "Flux fetched ${actual_revision:-no revision}, expected ${expected_revision}"
	echo "Flux fetched exact ephemeral revision ${expected_revision}."
}

timeout_seconds() {
	local value="${1%[smh]}"
	case "$1" in
		*s) printf '%s' "${value}" ;;
		*m) printf '%s' "$((value * 60))" ;;
		*h) printf '%s' "$((value * 3600))" ;;
		*) return 1 ;;
	esac
}

platform_is_ready() {
	local expected_revision="$1"
	local kustomizations="$2"
	local helmreleases="$3"
	jq -e --arg revision "${expected_revision}" '
    (.items | length > 0) and all(.items[];
      .status.observedGeneration == .metadata.generation and
      .status.lastAppliedRevision == $revision and
      any(.status.conditions[]?; .type == "Ready" and .status == "True"))
  ' <<<"${kustomizations}" >/dev/null \
		&& jq -e '
      (.items | length > 0) and all(.items[];
        .status.observedGeneration == .metadata.generation and
        any(.status.conditions[]?; .type == "Ready" and .status == "True"))
    ' <<<"${helmreleases}" >/dev/null
}

print_platform_wait_diagnostics() {
	local reason="$1"
	local timeout="${2:-10s}"
	echo "${reason}:" >&2
	[ "${timeout}" != 0s ] || return 0
	flux get kustomizations --timeout="${timeout}" >&2 || true
	flux get helmreleases --all-namespaces --timeout="${timeout}" >&2 || true
}

deadline_timeout() {
	local deadline="$1"
	local maximum="$2"
	local remaining=$((deadline - SECONDS))
	((remaining > 0)) || return 1
	((remaining <= maximum)) || remaining="${maximum}"
	printf '%ss' "${remaining}"
}

sleep_before_deadline() {
	local deadline="$1"
	local remaining=$((deadline - SECONDS))
	((remaining > 0)) || return 0
	((remaining <= 5)) || remaining=5
	sleep "${remaining}"
}

deadline_diagnostic_timeout() {
	local deadline="$1"
	local remaining=$((deadline - SECONDS))
	((remaining > 2)) || {
		printf '0s'
		return
	}
	remaining=$((remaining / 2))
	((remaining <= 5)) || remaining=5
	printf '%ss' "${remaining}"
}

bridge_image_wait_required() {
	case "${PROFILE}" in
		demo | federation) return 0 ;;
		*) return 1 ;;
	esac
}

load_bridge_image_for_platform() {
	local status
	if load_bridge_image_if_requested; then
		return 0
	else
		status=$?
	fi
	[ "${status}" -ne 2 ] || {
		echo "The ${PROFILE} profile requested ${BRIDGE_IMAGE}, but its image import failed." >&2
		flux get kustomizations >&2 || true
		flux get helmreleases --all-namespaces >&2 || true
		return 2
	}
	return 1
}

wait_for_platform_fixed() {
	local bridge_image_loaded bridge_image_status expected_revision deadline kustomizations helmreleases
	expected_revision="main@sha1:${SOURCE_REVISION}"
	deadline=$((SECONDS + $(timeout_seconds "${DEMO_TIMEOUT}")))
	bridge_image_loaded=true
	if bridge_image_wait_required; then
		bridge_image_loaded=false
	fi

	while ((SECONDS < deadline)); do
		if [ "${bridge_image_loaded}" = false ]; then
			if load_bridge_image_for_platform; then
				bridge_image_loaded=true
			else
				bridge_image_status=$?
				[ "${bridge_image_status}" -ne 2 ] || return 1
			fi
		fi
		if ! kustomizations="$(kubectl --request-timeout=10s --namespace flux-system \
			get kustomizations --output json)" \
			|| ! helmreleases="$(kubectl --request-timeout=10s get helmreleases \
				--all-namespaces --output json)"; then
			sleep 5
			continue
		fi
		resource_trace_record_ready_layers "${expected_revision}" "${kustomizations}"
		if [ "${bridge_image_loaded}" = true ] \
			&& platform_is_ready "${expected_revision}" "${kustomizations}" "${helmreleases}"; then
			return
		fi
		sleep 5
	done

	if [ "${bridge_image_loaded}" = false ]; then
		echo "The ${PROFILE} profile did not request the expected image ${BRIDGE_IMAGE}." >&2
	fi
	print_platform_wait_diagnostics \
		"Flux did not reconcile the evaluation revision within ${DEMO_TIMEOUT}"
	return 1
}

collect_platform_milestones() {
	local expected_revision="$1"
	local kustomizations="$2"
	local helmreleases="$3"
	jq --raw-output --arg revision "${expected_revision}" '
    .items[]? as $item |
    if $item.status.observedGeneration == $item.metadata.generation then
      "kustomization/\($item.metadata.name)/generation/\($item.metadata.generation)/observed"
    else empty end,
    if
      $item.status.observedGeneration == $item.metadata.generation and
      $item.status.lastAppliedRevision == $revision
    then
      "kustomization/\($item.metadata.name)/revision/\($revision)"
    else empty end,
    if
      $item.status.observedGeneration == $item.metadata.generation and
      $item.status.lastAppliedRevision == $revision and
      any($item.status.conditions[]?; .type == "Ready" and .status == "True")
    then
      "kustomization/\($item.metadata.name)/generation/\($item.metadata.generation)/revision/\($revision)/ready"
    else empty end
  ' <<<"${kustomizations}"
	jq --raw-output '
    .items[]? as $item |
    ($item.metadata.namespace + "/" + $item.metadata.name) as $name |
    if $item.status.observedGeneration == $item.metadata.generation then
      "helmrelease/\($name)/generation/\($item.metadata.generation)/observed"
    else empty end,
    if
      $item.status.observedGeneration == $item.metadata.generation and
      any($item.status.conditions[]?; .type == "Ready" and .status == "True")
    then
      "helmrelease/\($name)/generation/\($item.metadata.generation)/ready"
    else empty end
  ' <<<"${helmreleases}"
}

wait_for_platform_constrained() {
	local after before bridge_image_loaded bridge_image_status expected_revision hard_deadline
	local diagnostic_timeout helmreleases kustomizations
	local last_progress milestones no_progress_seconds max_seconds now request_timeout
	expected_revision="main@sha1:${SOURCE_REVISION}"
	no_progress_seconds="${FEDERATION_NO_PROGRESS_SECONDS}"
	max_seconds="${FEDERATION_MAX_SECONDS}"
	hard_deadline=$((SECONDS + max_seconds))
	last_progress="${SECONDS}"
	milestones="${WORK_DIR}/platform-milestones"
	: >"${milestones}"
	bridge_image_loaded=true
	if bridge_image_wait_required; then
		bridge_image_loaded=false
	fi

	while ((SECONDS < hard_deadline)); do
		now="${SECONDS}"
		if [ "${bridge_image_loaded}" = false ]; then
			if load_bridge_image_for_platform; then
				bridge_image_loaded=true
				last_progress="${SECONDS}"
			else
				bridge_image_status=$?
				[ "${bridge_image_status}" -ne 2 ] || return 1
			fi
		fi
		if ! request_timeout="$(deadline_timeout "${hard_deadline}" 10)" \
			|| ! kustomizations="$(kubectl --request-timeout="${request_timeout}" \
				--namespace flux-system get kustomizations --output json)" \
			|| ! request_timeout="$(deadline_timeout "${hard_deadline}" 10)" \
			|| ! helmreleases="$(kubectl --request-timeout="${request_timeout}" get \
				helmreleases --all-namespaces --output json)"; then
			now="${SECONDS}"
			if ((now - last_progress >= no_progress_seconds)); then
				diagnostic_timeout="$(deadline_diagnostic_timeout "${hard_deadline}")"
				print_platform_wait_diagnostics \
					"Flux made no observable progress for ${FEDERATION_NO_PROGRESS_TIMEOUT}" \
					"${diagnostic_timeout}"
				return 1
			fi
			sleep_before_deadline "${hard_deadline}"
			continue
		fi

		before="$(awk 'END { print NR }' "${milestones}")"
		collect_platform_milestones "${expected_revision}" "${kustomizations}" \
			"${helmreleases}" >>"${milestones}"
		LC_ALL=C sort -u "${milestones}" >"${milestones}.next"
		mv "${milestones}.next" "${milestones}"
		after="$(awk 'END { print NR }' "${milestones}")"
		if ((after > before)); then
			last_progress="${SECONDS}"
			echo "Flux convergence progress: ${after} immutable milestones observed."
			resource_trace_record_ready_layers "${expected_revision}" "${kustomizations}"
		fi
		if [ "${bridge_image_loaded}" = true ] \
			&& platform_is_ready "${expected_revision}" "${kustomizations}" "${helmreleases}"; then
			return
		fi
		if ((SECONDS - last_progress >= no_progress_seconds)); then
			diagnostic_timeout="$(deadline_diagnostic_timeout "${hard_deadline}")"
			print_platform_wait_diagnostics \
				"Flux made no observable progress for ${FEDERATION_NO_PROGRESS_TIMEOUT}" \
				"${diagnostic_timeout}"
			return 1
		fi
		sleep_before_deadline "${hard_deadline}"
	done

	print_platform_wait_diagnostics \
		"Flux did not reconcile within the absolute ${FEDERATION_MAX_TIMEOUT} cap" 0s
	return 1
}

wait_for_platform() {
	if [ "${PROFILE}" = "federation" ] && [ "${FEDERATION_CONSTRAINED}" = "yes" ]; then
		wait_for_platform_constrained
	else
		wait_for_platform_fixed
	fi
}

render_bootstrap_namespaces() {
	local namespace_layer="${SNAPSHOT_DIR}/infra/namespaces"
	local rendered_namespaces
	if [ "${PROFILE}" = "federation" ]; then
		namespace_layer="${SNAPSHOT_DIR}/infra/federation/namespace-layer"
	fi

	# Secrets and the local CA need their target Namespaces before Flux starts. Apply only those
	# cluster-scoped objects here: the early Flux namespace layer owns quotas and LimitRanges after
	# platform-settings substitution, before any dependent workload can reconcile.
	rendered_namespaces="$(kubectl kustomize "${namespace_layer}")" || return 1
	PROFILE="${PROFILE}" yq 'select(.kind == "Namespace" and
      (strenv(PROFILE) != "demo" or .metadata.name != "trivy-system"))' \
		<<<"${rendered_namespaces}"
}

demo_up() {
	local actual_capacity_mode artifact_status bootstrap_namespaces cluster_present=no container_output
	local current_container_ids federation_gateway_json
	local current_volume_identity reused_container_ids="" reused_volume_identity="" running_status
	local runtime_labels=()
	for command in base64 curl docker git jq k3d kubectl flux yq openssl rg tar; do
		require_command "${command}"
	done
	docker info >/dev/null 2>&1 || die "Docker daemon is not running"
	require_no_pending_teardown up
	if [ -n "${FGENTIC_DEMO_CACHE_DIR:-}" ]; then
		docker buildx version >/dev/null 2>&1 \
			|| die "FGENTIC_DEMO_CACHE_DIR requires Docker buildx"
	fi
	if [ "${PROFILE}" = "demo" ]; then
		configure_provider
	else
		# shellcheck disable=SC2034 # sourced render helpers consume this default provider global
		LLM_PROVIDER="demo"
		# shellcheck disable=SC2034 # sourced render helpers consume this default model global
		LLM_MODEL="fgentic-demo"
		# shellcheck disable=SC2034 # sourced render helpers consume this default project global
		GCP_PROJECT="not-configured"
		# shellcheck disable=SC2034 # sourced render helpers consume this default provider global
		VERTEX_REGION="europe-west1"
		# shellcheck disable=SC2034 # sourced render helpers consume this default provider global
		OPENAI_HOST="api.openai.com"
		# shellcheck disable=SC2034 # sourced render helpers consume this default provider global
		AZURE_OPENAI_RESOURCE="not-configured"
		# shellcheck disable=SC2034 # sourced secret helpers consume this optional Secret name
		MODEL_SECRET_NAME=""
		# shellcheck disable=SC2034 # sourced secret helpers consume this optional Secret value
		MODEL_SECRET_VALUE=""
	fi

	WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo.XXXXXX")"
	KUBECONFIG_FILE="$(mktemp "${TMPDIR:-/tmp}/fgentic-demo-kubeconfig.XXXXXX")"
	# shellcheck disable=SC2329 # cleanup is invoked indirectly by the trap below
	cleanup() {
		resource_trace_finish || true
		rm -rf "${WORK_DIR}" "${KUBECONFIG_FILE}"
	}
	trap cleanup EXIT INT TERM

	# Refuse foreign or interrupted same-name state before tracing or mutation. In particular, do
	# not let k3d silently adopt a retained image volume from a previously interrupted deletion.
	if cluster_exists; then
		cluster_present=yes
		cluster_owned_by_demo \
			|| die "refusing to reuse ${CLUSTER_NAME}: it was not created by scripts/demo.sh"
		if [ "${PROFILE}" = federation ]; then
			actual_capacity_mode="$(cluster_capacity_mode)" \
				|| die "could not inspect ${CLUSTER_NAME} capacity mode"
			[ "${actual_capacity_mode}" = "${FEDERATION_CAPACITY_MODE}" ] \
				|| die "refusing to switch ${CLUSTER_NAME} from ${actual_capacity_mode:-unlabelled} to ${FEDERATION_CAPACITY_MODE} capacity in place; run fed:down first"
		fi
		container_output="$(cluster_container_ids)" \
			|| die "could not inspect ${CLUSTER_NAME} containers before reuse"
		reused_container_ids="$(sort <<<"${container_output}")" \
			|| die "could not order ${CLUSTER_NAME} container identities"
		reused_volume_identity="$(cluster_volume_identity)" \
			|| die "refusing to reuse ${CLUSTER_NAME}: its image volume is missing or foreign"
	else
		if cluster_artifacts_exist; then
			die "refusing orphan reuse for ${CLUSTER_NAME}: owner-labelled server evidence is unavailable"
		else
			artifact_status=$?
			[ "${artifact_status}" -eq 1 ] \
				|| die "could not inspect retained artifacts for ${CLUSTER_NAME}"
		fi
		render_k3d_config "${WORK_DIR}/k3d-config.yaml"
	fi
	if [ "${PROFILE}" = "federation" ]; then
		# Ownership is proven above; begin before create/start to include the actual boot peak.
		resource_trace_start
	fi

	if [ "${cluster_present}" = no ]; then
		runtime_labels=(--runtime-label "dev.fgentic.demo=${OWNER_LABEL}@server:*")
		if [ "${PROFILE}" = federation ]; then
			runtime_labels+=(--runtime-label
				"dev.fgentic.demo.capacity=${FEDERATION_CAPACITY_MODE}@server:*")
		fi
		k3d cluster create --config "${WORK_DIR}/k3d-config.yaml" \
			"${runtime_labels[@]}" \
			--kubeconfig-update-default=false --kubeconfig-switch-context=false
	else
		if cluster_running; then
			:
		else
			running_status=$?
			[ "${running_status}" -eq 1 ] \
				|| die "could not inspect ${CLUSTER_NAME} runtime state before reuse"
			k3d cluster start "${CLUSTER_NAME}" >/dev/null
		fi
		cluster_running || die "${CLUSTER_NAME} did not become ready after k3d start"
		container_output="$(cluster_container_ids)" \
			|| die "could not inspect ${CLUSTER_NAME} containers after start"
		current_container_ids="$(sort <<<"${container_output}")" \
			|| die "could not order ${CLUSTER_NAME} container identities after start"
		[ "${current_container_ids}" = "${reused_container_ids}" ] \
			|| die "refusing to continue: k3d replaced retained containers while starting ${CLUSTER_NAME}"
		current_volume_identity="$(cluster_volume_identity)" \
			|| die "could not inspect ${CLUSTER_NAME} image volume after start"
		[ "${current_volume_identity}" = "${reused_volume_identity}" ] \
			|| die "refusing to continue: k3d replaced the retained image volume while starting ${CLUSTER_NAME}"
	fi
	k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_FILE}"
	export KUBECONFIG="${KUBECONFIG_FILE}"
	configure_federation_metrics_server
	if [ "${PROFILE}" = "federation" ]; then
		federation_gateway_json="$(docker inspect "k3d-${CLUSTER_NAME}-serverlb")" \
			|| die 'could not inspect the federation gateway address'
		# shellcheck disable=SC2034 # sourced federation helpers consume the discovered gateway address
		FEDERATION_GATEWAY_IP="$(jq -er --arg network "k3d-${CLUSTER_NAME}" \
			'.[0].NetworkSettings.Networks[$network].IPAddress' \
			<<<"${federation_gateway_json}")" \
			|| die 'the federation gateway address is invalid'
		prepare_federation_agent_card_key
	fi

	snapshot_source
	build_and_load_images
	flux install >/dev/null
	configure_ephemeral_flux_controllers
	configure_federation_flux_controllers
	kubectl --namespace flux-system rollout status deployment/source-controller --timeout=2m
	if ! kubectl get customresourcedefinition \
		gateways.gateway.networking.k8s.io httproutes.gateway.networking.k8s.io \
		>/dev/null 2>&1; then
		kubectl apply --server-side --filename \
			https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/experimental-install.yaml \
			>/dev/null
	fi
	# Match the production Flux DAG: admission must protect the first managed Namespace creation,
	# not only later updates. Flux becomes the steady-state field manager; force is required when a
	# retained disposable cluster hands those same canonical fields back to this bootstrap step.
	kubectl apply --server-side --force-conflicts \
		--kustomize "${SNAPSHOT_DIR}/infra/policies" >/dev/null
	bootstrap_namespaces="$(render_bootstrap_namespaces)" \
		|| die 'could not render bootstrap Namespaces'
	printf '%s\n' "${bootstrap_namespaces}" | kubectl apply --filename - >/dev/null
	"${ROOT_DIR}/scripts/local-ca.sh"
	create_ephemeral_secrets
	apply_source_server
	kubectl apply --kustomize "${SNAPSHOT_DIR}/${OVERLAY_PATH}" >/dev/null

	resource_trace_set_phase reconcile
	if [ "${PROFILE}" = "federation" ] && [ "${FEDERATION_CONSTRAINED}" = "yes" ]; then
		echo "Reconciling the constrained federation profile (no progress ${FEDERATION_NO_PROGRESS_TIMEOUT}; absolute cap ${FEDERATION_MAX_TIMEOUT})..."
	else
		echo "Reconciling the ${PROFILE} evaluation profile (timeout ${DEMO_TIMEOUT})..."
	fi
	wait_for_platform
	prune_stale_node_images "${SOURCE_IMAGE}"
	if [ "${PROFILE}" = demo ] || [ "${PROFILE}" = federation ]; then
		prune_stale_node_images "${BRIDGE_IMAGE}"
	fi
	local admission_context
	admission_context="$(kubectl config current-context)"
	ADMISSION_POLICY_CONTEXT="${admission_context}" \
		"${ROOT_DIR}/scripts/test-admission-policies.sh" --runtime
	resource_trace_set_phase proof
	"${ROOT_DIR}/${SEED_SCRIPT}"
	if [ "${PROFILE}" = "federation" ]; then
		resource_trace_collect_idle
		resource_trace_finish
	fi
}

require_cluster_runtime() {
	local command
	for command in docker jq k3d rg; do
		require_command "${command}"
	done
	docker info >/dev/null 2>&1 || die "Docker daemon is not running"
}

demo_status() {
	local artifact_status capacity_mode container_output image_bytes receipt retained_bytes running_output state
	local total_containers running_containers volume_bytes
	require_cluster_runtime
	if teardown_receipt_exists; then
		require_valid_teardown_receipt
		validate_teardown_receipt_resources
		receipt="$(teardown_receipt_path)"
		echo "Cluster ${CLUSTER_NAME}: state=recovery-pending receipt=${receipt}; run the matching down command to resume exact cleanup."
		return
	fi
	if ! cluster_exists; then
		if cluster_artifacts_exist; then
			die "refusing orphan inspection for ${CLUSTER_NAME}: owner-labelled server evidence is unavailable"
		else
			artifact_status=$?
			[ "${artifact_status}" -eq 1 ] \
				|| die "could not inspect retained artifacts for ${CLUSTER_NAME}"
		fi
		echo "Federation cluster ${CLUSTER_NAME}: state=absent retained_bytes=0"
		return
	fi
	require_owned_evaluation_cluster
	container_output="$(cluster_container_ids)" || die "could not inspect ${CLUSTER_NAME} containers"
	running_output="$(cluster_running_container_ids)" \
		|| die "could not inspect running ${CLUSTER_NAME} containers"
	total_containers="$(awk 'NF { count++ } END { print count + 0 }' <<<"${container_output}")"
	running_containers="$(awk 'NF { count++ } END { print count + 0 }' <<<"${running_output}")"
	state=partial
	[ "${running_containers}" -gt 0 ] || state=stopped
	[ "${running_containers}" -ne "${total_containers}" ] || state=running
	volume_bytes="$(cluster_volume_bytes || printf 'unknown')"
	retained_bytes="$(cluster_retained_storage_bytes || printf 'unknown')"
	image_bytes="$(cluster_owned_image_bytes)" \
		|| die "could not inspect ${CLUSTER_NAME} local images"
	capacity_mode=standard
	if [ "${PROFILE}" = federation ]; then
		capacity_mode="$(cluster_capacity_mode)" \
			|| die "could not inspect ${CLUSTER_NAME} capacity mode"
	fi
	echo "Federation cluster ${CLUSTER_NAME}: state=${state} capacity_mode=${capacity_mode} running_containers=${running_containers}/${total_containers} image_volume_bytes=${volume_bytes} retained_cluster_bytes=${retained_bytes} local_image_virtual_bytes=${image_bytes}"
}

demo_stop() {
	local after_container_ids after_identity after_output before_container_ids before_identity
	local image_volume_bytes retained_bytes
	local before_output running_output
	require_cluster_runtime
	require_no_pending_teardown stop
	require_owned_evaluation_cluster
	before_output="$(cluster_container_ids)" \
		|| die "could not inspect ${CLUSTER_NAME} containers before stopping"
	before_container_ids="$(sort <<<"${before_output}")" \
		|| die "could not order ${CLUSTER_NAME} container identities before stopping"
	before_identity="$(cluster_volume_identity)" \
		|| die "could not inspect ${CLUSTER_NAME} image volume before stopping"
	k3d cluster stop "${CLUSTER_NAME}" >/dev/null
	running_output="$(cluster_running_container_ids)" \
		|| die "could not verify ${CLUSTER_NAME} stopped containers"
	[ -z "${running_output}" ] \
		|| die "${CLUSTER_NAME} still has running containers after k3d stop"
	after_output="$(cluster_container_ids)" \
		|| die "could not inspect ${CLUSTER_NAME} retained containers after stopping"
	after_container_ids="$(sort <<<"${after_output}")" \
		|| die "could not order ${CLUSTER_NAME} retained container identities"
	[ "${after_container_ids}" = "${before_container_ids}" ] \
		|| die "${CLUSTER_NAME} container identity changed while stopping"
	after_identity="$(cluster_volume_identity)" \
		|| die "${CLUSTER_NAME} lost its owned image volume while stopping"
	[ "${after_identity}" = "${before_identity}" ] \
		|| die "${CLUSTER_NAME} image volume identity changed while stopping"
	image_volume_bytes="$(cluster_volume_bytes || printf 'unknown')"
	retained_bytes="$(cluster_retained_storage_bytes || printf 'unknown')"
	echo "Federation cluster ${CLUSTER_NAME}: state=stopped running_containers=0 image_volume_bytes=${image_volume_bytes} retained_cluster_bytes=${retained_bytes}; the exact image volume is preserved."
}

demo_down() {
	local artifact_status
	require_cluster_runtime
	if teardown_receipt_exists; then
		recover_teardown_receipt || die "could not finish ${CLUSTER_NAME} teardown recovery"
		echo "Recovered teardown for ${CLUSTER_NAME}. The reusable local CA and FGENTIC_DEMO_CACHE_DIR, when set, were preserved."
		return
	fi
	if cluster_exists; then
		cluster_owned_by_demo \
			|| die "refusing to delete ${CLUSTER_NAME}: it was not created by scripts/demo.sh"
		write_teardown_receipt
		recover_teardown_receipt || die "could not finish ${CLUSTER_NAME} teardown"
		echo "The reusable local CA and FGENTIC_DEMO_CACHE_DIR, when set, were preserved."
	else
		if cluster_artifacts_exist; then
			teardown_receipt_fail "refusing orphan cleanup for ${CLUSTER_NAME}: teardown receipt and owner-labelled server evidence are unavailable"
		else
			artifact_status=$?
			[ "${artifact_status}" -eq 1 ] \
				|| die "could not inspect retained artifacts for ${CLUSTER_NAME}"
		fi
		echo "Demo cluster ${CLUSTER_NAME} does not exist."
	fi
}
