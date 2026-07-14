#!/usr/bin/env bash
# Definition-only cluster lifecycle helpers sourced by scripts/demo.sh.
configure_ephemeral_flux_controllers() {
	local deployment
	local patch
	local deployments=()

	# Ephemeral profiles run on the same constrained workstation as clusters/local. Keep the
	# single-replica controllers alive through API-server I/O stalls instead of flapping every
	# dependent Kustomization after the default 15-second leader-election lease expires.
	mapfile -t deployments < <(
		kubectl --namespace flux-system get deployments \
			--selector app.kubernetes.io/part-of=flux --output json |
			jq --raw-output --arg lease "--leader-election-lease-duration=${FLUX_LEADER_ELECTION_LEASE_DURATION}" '
          .items[] |
          select((((.spec.template.spec.containers[0].args // []) | index($lease)) == null)) |
          .metadata.name
        '
	)
	((${#deployments[@]} > 0)) || return

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
random_hex() {
	openssl rand -hex "$1"
}

cluster_exists() {
	k3d cluster list --output json | jq -e --arg name "${CLUSTER_NAME}" \
		'any(.[]; .name == $name)' >/dev/null
}

cluster_owned_by_demo() {
	[ "$(docker inspect --format '{{index .Config.Labels "dev.fgentic.demo"}}' \
		"k3d-${CLUSTER_NAME}-server-0" 2>/dev/null || true)" = "${OWNER_LABEL}" ]
}

cluster_container_ids() {
	docker ps --all --filter "label=k3d.cluster=${CLUSTER_NAME}" --quiet
}

cluster_network_exists() {
	docker network inspect "k3d-${CLUSTER_NAME}" >/dev/null 2>&1
}

cluster_volume_exists() {
	docker volume inspect "k3d-${CLUSTER_NAME}-images" >/dev/null 2>&1
}

cluster_artifacts_exist() {
	[ -n "$(cluster_container_ids)" ] || cluster_network_exists || cluster_volume_exists
}

cluster_cleanup_complete() {
	! cluster_exists && ! cluster_artifacts_exist
}

cleanup_cluster_artifacts() {
	local attempt
	local container_ids=()
	local network_owner
	local volume_owner
	for attempt in 1 2 3; do
		mapfile -t container_ids < <(cluster_container_ids)
		if ((${#container_ids[@]} > 0)); then
			# Ownership was proven from the server's private runtime label before k3d deletion;
			# the exact k3d.cluster label selects the remaining nodes and load balancer only.
			docker rm --force "${container_ids[@]}" >/dev/null 2>&1 || true
		fi

		if network_owner="$(docker network inspect --format '{{index .Labels "app"}}' \
			"k3d-${CLUSTER_NAME}" 2>/dev/null)"; then
			[ "${network_owner}" = "k3d" ] ||
				die "refusing to remove foreign network k3d-${CLUSTER_NAME}"
			docker network rm "k3d-${CLUSTER_NAME}" >/dev/null 2>&1 || true
		fi

		if volume_owner="$(docker volume inspect --format '{{index .Labels "app"}}/{{index .Labels "k3d.cluster"}}' \
			"k3d-${CLUSTER_NAME}-images" 2>/dev/null)"; then
			[ "${volume_owner}" = "k3d/${CLUSTER_NAME}" ] ||
				die "refusing to remove foreign volume k3d-${CLUSTER_NAME}-images"
			docker volume rm "k3d-${CLUSTER_NAME}-images" >/dev/null 2>&1 || true
		fi

		cluster_cleanup_complete && return
		[ "${attempt}" -eq 3 ] || sleep 2
	done

	echo "error: disposable cluster cleanup did not complete; inspect exact owned resources:" >&2
	echo "  k3d cluster list --output json" >&2
	echo "  docker ps -a --filter label=k3d.cluster=${CLUSTER_NAME}" >&2
	echo "  docker network inspect k3d-${CLUSTER_NAME}" >&2
	echo "  docker volume inspect k3d-${CLUSTER_NAME}-images" >&2
	return 1
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
	if [ "${PROFILE}" = "demo" ]; then
		build_image "${BRIDGE_IMAGE}" "${ROOT_DIR}/apps/matrix-a2a-bridge/Dockerfile" \
			"${ROOT_DIR}/apps/matrix-a2a-bridge" bridge
	fi
	# The source is the first side-loaded workload requested; delay only the bridge image through
	# the much longer platform dependency chain.
	k3d image import --cluster "${CLUSTER_NAME}" "${SOURCE_IMAGE}" >/dev/null
}

load_bridge_image_if_requested() {
	local requested_image
	[ "${PROFILE}" = "demo" ] || return 0
	requested_image="$(
		kubectl --namespace bridge get helmrelease matrix-a2a-bridge --output json 2>/dev/null |
			jq --exit-status --raw-output \
				'.spec.values.image | "\(.repository):\(.tag)"' 2>/dev/null
	)" || return 1
	[ "${requested_image}" = "${BRIDGE_IMAGE}" ] || return 1

	# Loading only after Flux applies this exact HelmRelease tag leaves the long dependency wait
	# behind us and can precede Pod creation, narrowing the unused pullPolicy=Never image window.
	k3d image import --cluster "${CLUSTER_NAME}" "${BRIDGE_IMAGE}" >/dev/null || return 2
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
		if flux reconcile source git flux-system --timeout=2m >/dev/null &&
			actual_revision="$(kubectl --namespace flux-system get gitrepository flux-system \
				--output jsonpath='{.status.artifact.revision}')" &&
			[ "${actual_revision}" = "${expected_revision}" ]; then
			break
		fi
		sleep 2
	done
	[ "${actual_revision}" = "${expected_revision}" ] ||
		die "Flux fetched ${actual_revision:-no revision}, expected ${expected_revision}"
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

wait_for_platform() {
	local bridge_image_loaded bridge_image_status expected_revision deadline kustomizations helmreleases
	expected_revision="main@sha1:${SOURCE_REVISION}"
	deadline=$((SECONDS + $(timeout_seconds "${DEMO_TIMEOUT}")))
	bridge_image_loaded=false
	[ "${PROFILE}" = "demo" ] || bridge_image_loaded=true

	while ((SECONDS < deadline)); do
		if [ "${bridge_image_loaded}" = false ]; then
			if load_bridge_image_if_requested; then
				bridge_image_loaded=true
			else
				bridge_image_status=$?
				if [ "${bridge_image_status}" -eq 2 ]; then
					echo "Bridge HelmRelease requested ${BRIDGE_IMAGE}, but its image import failed." >&2
					flux get kustomizations >&2 || true
					flux get helmreleases --all-namespaces >&2 || true
					return 1
				fi
			fi
		fi
		if ! kustomizations="$(kubectl --namespace flux-system get kustomizations --output json)" ||
			! helmreleases="$(kubectl get helmreleases --all-namespaces --output json)"; then
			sleep 5
			continue
		fi
		if [ "${bridge_image_loaded}" = true ] &&
			jq -e --arg revision "${expected_revision}" '
        (.items | length > 0) and all(.items[];
          .status.observedGeneration == .metadata.generation and
          .status.lastAppliedRevision == $revision and
          any(.status.conditions[]?; .type == "Ready" and .status == "True"))
      ' <<<"${kustomizations}" >/dev/null &&
			jq -e '
          (.items | length > 0) and all(.items[];
            .status.observedGeneration == .metadata.generation and
            any(.status.conditions[]?; .type == "Ready" and .status == "True"))
        ' <<<"${helmreleases}" >/dev/null; then
			return
		fi
		sleep 5
	done

	echo "Flux did not reconcile the evaluation revision within ${DEMO_TIMEOUT}:" >&2
	if [ "${bridge_image_loaded}" = false ]; then
		echo "Bridge HelmRelease did not request the expected image ${BRIDGE_IMAGE}." >&2
	fi
	flux get kustomizations >&2 || true
	flux get helmreleases --all-namespaces >&2 || true
	return 1
}

render_bootstrap_namespaces() {
	local namespace_layer="${SNAPSHOT_DIR}/infra/namespaces"
	if [ "${PROFILE}" = "federation" ]; then
		namespace_layer="${SNAPSHOT_DIR}/infra/federation/namespace-layer"
	fi

	# Secrets and the local CA need their target Namespaces before Flux starts. Apply only those
	# cluster-scoped objects here: the early Flux namespace layer owns quotas and LimitRanges after
	# platform-settings substitution, before any dependent workload can reconcile.
	kubectl kustomize "${namespace_layer}" |
		PROFILE="${PROFILE}" yq 'select(.kind == "Namespace" and
      (strenv(PROFILE) != "demo" or .metadata.name != "trivy-system"))'
}

demo_up() {
	for command in base64 curl docker git jq k3d kubectl flux yq openssl rg tar; do
		require_command "${command}"
	done
	docker info >/dev/null 2>&1 || die "Docker daemon is not running"
	if [ -n "${FGENTIC_DEMO_CACHE_DIR:-}" ]; then
		docker buildx version >/dev/null 2>&1 ||
			die "FGENTIC_DEMO_CACHE_DIR requires Docker buildx"
	fi
	if [ "${PROFILE}" = "demo" ]; then
		configure_provider
	else
		LLM_PROVIDER="demo"
		LLM_MODEL="fgentic-demo"
		GCP_PROJECT="not-configured"
		VERTEX_REGION="europe-west1"
		OPENAI_HOST="api.openai.com"
		AZURE_OPENAI_RESOURCE="not-configured"
		MODEL_SECRET_NAME=""
		MODEL_SECRET_VALUE=""
	fi

	WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-demo.XXXXXX")"
	KUBECONFIG_FILE="$(mktemp "${TMPDIR:-/tmp}/fgentic-demo-kubeconfig.XXXXXX")"
	cleanup() {
		rm -rf "${WORK_DIR}" "${KUBECONFIG_FILE}"
	}
	trap cleanup EXIT INT TERM

	if ! cluster_exists; then
		render_k3d_config "${WORK_DIR}/k3d-config.yaml"
		k3d cluster create --config "${WORK_DIR}/k3d-config.yaml" \
			--runtime-label "dev.fgentic.demo=${OWNER_LABEL}@server:*" \
			--kubeconfig-update-default=false --kubeconfig-switch-context=false
	else
		cluster_owned_by_demo ||
			die "refusing to reuse ${CLUSTER_NAME}: it was not created by scripts/demo.sh"
		k3d cluster start "${CLUSTER_NAME}" >/dev/null 2>&1 || true
	fi
	k3d kubeconfig get "${CLUSTER_NAME}" >"${KUBECONFIG_FILE}"
	export KUBECONFIG="${KUBECONFIG_FILE}"
	if [ "${PROFILE}" = "federation" ]; then
		FEDERATION_GATEWAY_IP="$(docker inspect "k3d-${CLUSTER_NAME}-serverlb" |
			jq -er --arg network "k3d-${CLUSTER_NAME}" \
			'.[0].NetworkSettings.Networks[$network].IPAddress')"
		prepare_federation_agent_card_key
	fi

	snapshot_source
	build_and_load_images
	flux install >/dev/null
	configure_ephemeral_flux_controllers
	kubectl --namespace flux-system rollout status deployment/source-controller --timeout=2m
	if ! kubectl get customresourcedefinition \
		gateways.gateway.networking.k8s.io httproutes.gateway.networking.k8s.io \
		>/dev/null 2>&1; then
		kubectl apply --server-side --filename \
			https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml \
			>/dev/null
	fi
	# Match the production Flux DAG: admission must protect the first managed Namespace creation,
	# not only later updates. Flux becomes the steady-state field manager; force is required when a
	# retained disposable cluster hands those same canonical fields back to this bootstrap step.
	kubectl apply --server-side --force-conflicts \
		--kustomize "${SNAPSHOT_DIR}/infra/policies" >/dev/null
	render_bootstrap_namespaces | kubectl apply --filename - >/dev/null
	"${ROOT_DIR}/scripts/local-ca.sh"
	create_ephemeral_secrets
	apply_source_server
	kubectl apply --kustomize "${SNAPSHOT_DIR}/${OVERLAY_PATH}" >/dev/null

	echo "Reconciling the ${PROFILE} evaluation profile (timeout ${DEMO_TIMEOUT})..."
	wait_for_platform
	local admission_context
	admission_context="$(kubectl config current-context)"
	ADMISSION_POLICY_CONTEXT="${admission_context}" \
		"${ROOT_DIR}/scripts/test-admission-policies.sh" --runtime
	"${ROOT_DIR}/${SEED_SCRIPT}"
}

demo_down() {
	require_command docker
	require_command k3d
	require_command jq
	if cluster_exists; then
		cluster_owned_by_demo ||
			die "refusing to delete ${CLUSTER_NAME}: it was not created by scripts/demo.sh"
		k3d cluster delete "${CLUSTER_NAME}" || true
		cleanup_cluster_artifacts
		local image_id
		for image_id in $(docker images \
			--filter "label=dev.fgentic.demo.cluster=${CLUSTER_NAME}" --quiet); do
			docker image rm "${image_id}" >/dev/null 2>&1 || true
		done
		echo "The reusable local CA and FGENTIC_DEMO_CACHE_DIR, when set, were preserved."
	elif cluster_artifacts_exist; then
		die "refusing orphan cleanup for ${CLUSTER_NAME}: owner-labelled server evidence is unavailable"
	else
		echo "Demo cluster ${CLUSTER_NAME} does not exist."
	fi
}
