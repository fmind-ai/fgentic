#!/usr/bin/env bash
# Credential-free evaluation lifecycle. Flux still renders the canonical HelmReleases, but its
# source is an ephemeral, cluster-local snapshot of this checkout instead of GitHub.
set -euo pipefail

readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly DEFAULT_CLUSTER_NAME="fgentic-demo"
readonly DEMO_SERVER_NAME="fgentic.localhost"
readonly FEDERATION_CLUSTER_NAME="fgentic-fed"
readonly FEDERATION_SERVER_NAME="org-a.fgentic.localhost"
readonly FEDERATION_LOOPBACK="127.0.0.2"
readonly FEDERATION_POLICY_PATH="apps/synapse-federation-policy/policy/policy.json"
readonly FEDERATION_POLICY_EVENT_TYPE="com.fgentic.blocked"
readonly MAS_ADMIN_CLIENT_ID="01KX8D3M0AD3M0ADM1NC13NT01"
readonly SOURCE_BASE_IMAGE="alpine:3.23@sha256:fd791d74b68913cbb027c6546007b3f0d3bc45125f797758156952bc2d6daf40"
readonly SOURCE_GIT_PACKAGES="git=2.52.0-r0 git-daemon=2.52.0-r0 busybox-extras=1.37.0-r30"

usage() {
	cat <<'EOF'
usage: scripts/demo.sh up|down

Environment:
  FGENTIC_DEMO_CLUSTER       k3d cluster name (default: fgentic-demo)
  FGENTIC_DEMO_TIMEOUT       reconciliation timeout (default: 15m)
  FGENTIC_DEMO_CACHE_DIR     optional persistent BuildKit cache directory
  FGENTIC_FED_POLICY_PROBE   federation profile only: deny (default) or allow; allow mutates only
                             the ephemeral Git snapshot used by the disposable lab
  FGENTIC_LLM_PROVIDER       demo (default), vllm, vertex, mistral, anthropic,
                             openai, or azure-openai
  FGENTIC_LLM_MODEL          model identifier; required for API profiles except Vertex
  FGENTIC_ALLOW_PAID_PROVIDER=yes
                             required before an API/Vertex profile can make its seed request

Provider-specific settings follow docs/models.md: MISTRAL_API_KEY, ANTHROPIC_API_KEY,
OPENAI_API_KEY, AZURE_OPENAI_API_KEY, GOOGLE_APPLICATION_CREDENTIALS, FGENTIC_GCP_PROJECT,
FGENTIC_VERTEX_REGION, FGENTIC_OPENAI_HOST, and FGENTIC_AZURE_OPENAI_RESOURCE.

The default demo profile is a deterministic in-cluster response stub. It proves the complete
Matrix -> bridge -> agentgateway -> kagent path without a model account or per-token charge; it is
not a language model and is never a production option.
EOF
}

die() {
	echo "error: $*" >&2
	exit 1
}

require_command() {
	command -v "$1" >/dev/null 2>&1 || die "required command not found: $1 (run 'mise install')"
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

configure_provider() {
	LLM_PROVIDER="${FGENTIC_LLM_PROVIDER:-demo}"
	LLM_MODEL="${FGENTIC_LLM_MODEL:-}"
	GCP_PROJECT="${FGENTIC_GCP_PROJECT:-not-configured}"
	VERTEX_REGION="${FGENTIC_VERTEX_REGION:-europe-west1}"
	OPENAI_HOST="${FGENTIC_OPENAI_HOST:-api.openai.com}"
	AZURE_OPENAI_RESOURCE="${FGENTIC_AZURE_OPENAI_RESOURCE:-not-configured}"
	MODEL_SECRET_ENV=""
	MODEL_SECRET_NAME=""

	case "${LLM_PROVIDER}" in
	demo)
		LLM_MODEL="${LLM_MODEL:-fgentic-demo}"
		;;
	vllm)
		LLM_MODEL="${LLM_MODEL:-Qwen/Qwen2.5-0.5B-Instruct}"
		echo "Self-hosted vLLM selected: allow roughly 2.7 GB of downloads and 4-6 GiB of RAM."
		;;
	vertex)
		LLM_MODEL="${LLM_MODEL:-google/gemini-2.5-flash}"
		[ "${FGENTIC_ALLOW_PAID_PROVIDER:-}" = "yes" ] ||
			die "Vertex can incur cost; set FGENTIC_ALLOW_PAID_PROVIDER=yes explicitly"
		[ "${GCP_PROJECT}" != "not-configured" ] ||
			die "FGENTIC_GCP_PROJECT is required for the Vertex profile"
		[ -n "${GOOGLE_APPLICATION_CREDENTIALS:-}" ] ||
			die "GOOGLE_APPLICATION_CREDENTIALS is required for the Vertex profile"
		[ -r "${GOOGLE_APPLICATION_CREDENTIALS}" ] ||
			die "GOOGLE_APPLICATION_CREDENTIALS is not readable"
		;;
	mistral)
		MODEL_SECRET_ENV="MISTRAL_API_KEY"
		MODEL_SECRET_NAME="mistral-secret"
		;;
	anthropic)
		MODEL_SECRET_ENV="ANTHROPIC_API_KEY"
		MODEL_SECRET_NAME="anthropic-secret"
		;;
	openai)
		MODEL_SECRET_ENV="OPENAI_API_KEY"
		MODEL_SECRET_NAME="openai-secret"
		;;
	azure-openai)
		MODEL_SECRET_ENV="AZURE_OPENAI_API_KEY"
		MODEL_SECRET_NAME="azure-openai-secret"
		[ "${AZURE_OPENAI_RESOURCE}" != "not-configured" ] ||
			die "FGENTIC_AZURE_OPENAI_RESOURCE is required for Azure OpenAI"
		;;
	*)
		die "unsupported FGENTIC_LLM_PROVIDER: ${LLM_PROVIDER}"
		;;
	esac

	if [ -n "${MODEL_SECRET_ENV}" ]; then
		[ "${FGENTIC_ALLOW_PAID_PROVIDER:-}" = "yes" ] ||
			die "${LLM_PROVIDER} can incur cost; set FGENTIC_ALLOW_PAID_PROVIDER=yes explicitly"
		[ -n "${LLM_MODEL}" ] || die "FGENTIC_LLM_MODEL is required for ${LLM_PROVIDER}"
		MODEL_SECRET_VALUE="${!MODEL_SECRET_ENV:-}"
		[ -n "${MODEL_SECRET_VALUE}" ] || die "${MODEL_SECRET_ENV} is required for ${LLM_PROVIDER}"
	else
		MODEL_SECRET_VALUE=""
	fi
}

snapshot_source() {
	SNAPSHOT_DIR="${WORK_DIR}/snapshot"
	mkdir -p "${SNAPSHOT_DIR}"
	if [ -n "$(git -C "${ROOT_DIR}" status --porcelain)" ]; then
		echo "Note: the ephemeral demo snapshot includes the current uncommitted working tree."
	fi
	(
		cd "${ROOT_DIR}"
		git ls-files --cached --others --exclude-standard -z |
			while IFS= read -r -d '' path; do
				[ -e "${path}" ] || [ -L "${path}" ] || continue
				printf '%s\0' "${path}"
			done |
			tar --null --files-from=- --create --file=-
	) | tar --directory "${SNAPSHOT_DIR}" --extract --file=-

	LLM_PROVIDER="${LLM_PROVIDER}" LLM_MODEL="${LLM_MODEL}" GCP_PROJECT="${GCP_PROJECT}" \
		VERTEX_REGION="${VERTEX_REGION}" OPENAI_HOST="${OPENAI_HOST}" \
		AZURE_OPENAI_RESOURCE="${AZURE_OPENAI_RESOURCE}" BRIDGE_TAG="${BRIDGE_TAG}" \
		yq --inplace '
      .data.llm_provider = strenv(LLM_PROVIDER) |
      .data.llm_model = strenv(LLM_MODEL) |
      .data.gcp_project = strenv(GCP_PROJECT) |
      .data.vertex_region = strenv(VERTEX_REGION) |
      .data.openai_host = strenv(OPENAI_HOST) |
      .data.azure_openai_resource = strenv(AZURE_OPENAI_RESOURCE) |
      .data.demo_bridge_tag = strenv(BRIDGE_TAG)
    ' "${SNAPSHOT_DIR}/${OVERLAY_PATH}/platform-settings.yaml"
	if [ "${PROFILE}" = "federation" ]; then
		FED_GATEWAY_IP="${FEDERATION_GATEWAY_IP}" yq --inplace \
			'.data.federation_gateway_ip = strenv(FED_GATEWAY_IP)' \
			"${SNAPSHOT_DIR}/${OVERLAY_PATH}/platform-settings.yaml"
		configure_federation_policy_snapshot
	fi

	# Flux reports Git artifacts as `sha1:<40 hex>` in the pinned source-controller contract.
	# Force that object format even when the caller globally defaults new repositories to SHA-256.
	git -C "${SNAPSHOT_DIR}" init --quiet --object-format=sha1 --initial-branch main
	git -C "${SNAPSHOT_DIR}" add --all
	git -C "${SNAPSHOT_DIR}" \
		-c user.name='Fgentic demo' -c user.email='demo@localhost' \
		commit --quiet --message='chore: create ephemeral demo source'
	SOURCE_REVISION="$(git -C "${SNAPSHOT_DIR}" rev-parse HEAD)"
	[[ "${SOURCE_REVISION}" =~ ^[0-9a-f]{40}$ ]] || die "invalid ephemeral Git revision"
	SOURCE_CONTEXT="${WORK_DIR}/source-image"
	mkdir -p "${SOURCE_CONTEXT}"
	git clone --quiet --bare "${SNAPSHOT_DIR}" "${SOURCE_CONTEXT}/repo.git"
	git --git-dir="${SOURCE_CONTEXT}/repo.git" update-server-info
}

configure_federation_policy_snapshot() {
	local policy_file="${SNAPSHOT_DIR}/${FEDERATION_POLICY_PATH}"
	local next_policy
	[ -f "${policy_file}" ] || die "federation policy not found: ${FEDERATION_POLICY_PATH}"
	jq -e '.allowed_event_types | type == "array"' "${policy_file}" >/dev/null ||
		die "federation policy allowed_event_types must be an array"

	case "${FEDERATION_POLICY_PROBE}" in
	deny)
		jq -e --arg event_type "${FEDERATION_POLICY_EVENT_TYPE}" \
			'.allowed_event_types | index($event_type) == null' "${policy_file}" >/dev/null ||
			die "canonical federation policy must deny ${FEDERATION_POLICY_EVENT_TYPE}"
		;;
	allow)
		next_policy="${policy_file}.next"
		jq --arg event_type "${FEDERATION_POLICY_EVENT_TYPE}" \
			'.allowed_event_types |= (. + [$event_type] | unique)' \
			"${policy_file}" >"${next_policy}"
		mv "${next_policy}" "${policy_file}"
		;;
	*) die "unsupported federation policy probe mode: ${FEDERATION_POLICY_PROBE}" ;;
	esac
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
	local images=("${SOURCE_IMAGE}")
	if [ "${PROFILE}" = "demo" ]; then
		build_image "${BRIDGE_IMAGE}" "${ROOT_DIR}/apps/matrix-a2a-bridge/Dockerfile" \
			"${ROOT_DIR}/apps/matrix-a2a-bridge" bridge
		images+=("${BRIDGE_IMAGE}")
	fi
	k3d image import --cluster "${CLUSTER_NAME}" "${images[@]}" >/dev/null
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
		flux reconcile source git flux-system --timeout=2m >/dev/null
		actual_revision="$(kubectl --namespace flux-system get gitrepository flux-system \
			--output jsonpath='{.status.artifact.revision}')"
		[ "${actual_revision}" = "${expected_revision}" ] && break
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
	local source_revision deadline kustomizations helmreleases
	source_revision="$(kubectl --namespace flux-system get gitrepository flux-system \
		--output jsonpath='{.status.artifact.revision}')"
	[ -n "${source_revision}" ] || die "the local Flux source has no artifact revision"
	deadline=$((SECONDS + $(timeout_seconds "${DEMO_TIMEOUT}")))

	while ((SECONDS < deadline)); do
		kustomizations="$(kubectl --namespace flux-system get kustomizations --output json)"
		helmreleases="$(kubectl get helmreleases --all-namespaces --output json)"
		if jq -e --arg revision "${source_revision}" '
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
	flux get kustomizations >&2 || true
	flux get helmreleases --all-namespaces >&2 || true
	return 1
}

bootstrap_secret_value() {
	kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output "go-template={{index .data \"$1\" | base64decode}}"
}

apply_secret() {
	local namespace="$1"
	local name="$2"
	shift 2
	local type=Opaque argument source key value encoded data="" separator=""
	for argument in "$@"; do
		case "${argument}" in
		--type=*)
			type="${argument#--type=}"
			;;
		--from-literal=*)
			source="${argument#--from-literal=}"
			key="${source%%=*}"
			value="${source#*=}"
			;;
		--from-file=*)
			source="${argument#--from-file=}"
			key="${source%%=*}"
			value="$(<"${source#*=}")"
			;;
		*)
			die "unsupported apply_secret argument"
			;;
		esac
		case "${argument}" in
		--from-literal=* | --from-file=*)
			[[ "${key}" =~ ^[-._a-zA-Z0-9]+$ ]] || die "invalid Secret data key"
			encoded="$(printf '%s' "${value}" | base64 | tr -d '\r\n')"
			data="${data}${separator}\"${key}\":\"${encoded}\""
			separator=,
			;;
		esac
	done
	printf '{"apiVersion":"v1","kind":"Secret","metadata":{"name":"%s","namespace":"%s"},"type":"%s","data":{%s}}\n' \
		"${name}" "${namespace}" "${type}" "${data}" | kubectl apply --filename - >/dev/null
}

create_ephemeral_secrets() {
	if [ "${PROFILE}" = "federation" ]; then
		create_federation_secrets
		return
	fi

	if ! kubectl --namespace flux-system get secret fgentic-demo-bootstrap >/dev/null 2>&1; then
		apply_secret flux-system fgentic-demo-bootstrap \
			--from-literal=pg-synapse="$(random_hex 24)" \
			--from-literal=pg-mas="$(random_hex 24)" \
			--from-literal=pg-bridge="$(random_hex 24)" \
			--from-literal=pg-kagent="$(random_hex 24)" \
			--from-literal=as-token="$(random_hex 32)" \
			--from-literal=hs-token="$(random_hex 32)" \
			--from-literal=a2a-key="$(random_hex 32)" \
			--from-literal=mcp-platform-helper-key="$(random_hex 32)" \
			--from-literal=mas-admin-client="$(random_hex 32)" \
			--from-literal=demo-password="$(random_hex 24)"
	fi

	PG_SYNAPSE="$(bootstrap_secret_value pg-synapse)"
	PG_MAS="$(bootstrap_secret_value pg-mas)"
	PG_BRIDGE="$(bootstrap_secret_value pg-bridge)"
	PG_KAGENT="$(bootstrap_secret_value pg-kagent)"
	AS_TOKEN="$(bootstrap_secret_value as-token)"
	HS_TOKEN="$(bootstrap_secret_value hs-token)"
	A2A_KEY="$(bootstrap_secret_value a2a-key)"
	MCP_PLATFORM_HELPER_KEY="$(bootstrap_secret_value mcp-platform-helper-key)"
	MAS_ADMIN_CLIENT_SECRET="$(bootstrap_secret_value mas-admin-client)"
	DEMO_PASSWORD="$(bootstrap_secret_value demo-password)"

	apply_secret postgres pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${PG_SYNAPSE}"
	apply_secret matrix pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${PG_SYNAPSE}"
	apply_secret postgres pg-mas --type=kubernetes.io/basic-auth \
		--from-literal=username=mas --from-literal=password="${PG_MAS}"
	apply_secret matrix pg-mas --type=kubernetes.io/basic-auth \
		--from-literal=username=mas --from-literal=password="${PG_MAS}"
	apply_secret postgres pg-bridge --type=kubernetes.io/basic-auth \
		--from-literal=username=bridge --from-literal=password="${PG_BRIDGE}"
	apply_secret postgres pg-kagent --type=kubernetes.io/basic-auth \
		--from-literal=username=kagent --from-literal=password="${PG_KAGENT}"
	apply_secret kagent kagent-db \
		--from-literal=url="postgresql://kagent:${PG_KAGENT}@platform-pg-rw.postgres.svc.cluster.local:5432/kagent?sslmode=require"
	apply_secret kagent kagent-model-auth \
		--from-literal=OPENAI_API_KEY=sk-not-used-agentgateway-holds-the-real-key
	apply_secret bridge matrix-a2a-bridge-db \
		--from-literal=url="postgres://bridge:${PG_BRIDGE}@platform-pg-rw.postgres.svc.cluster.local:5432/bridge?sslmode=require"

	local registration
	registration="$(cat <<EOF
id: matrix-a2a-bridge
url: http://matrix-a2a-bridge.bridge.svc.cluster.local:29331
as_token: ${AS_TOKEN}
hs_token: ${HS_TOKEN}
sender_localpart: a2a-bridge
rate_limited: false
namespaces:
  users:
    - regex: '@a2a-bridge:fgentic\\.localhost'
      exclusive: true
    - regex: '@agent-.*:fgentic\\.localhost'
      exclusive: true
EOF
)"
	apply_secret bridge matrix-a2a-bridge-registration \
		--from-literal=registration.yaml="${registration}"
	apply_secret matrix matrix-a2a-bridge-registration \
		--from-literal=registration.yaml="${registration}"

	local callers
	callers="$(jq --null-input --compact-output --arg key "${A2A_KEY}" \
		'{"matrix-a2a-bridge": {key: $key, metadata: {workload: "matrix-a2a-bridge"}}}')"
	apply_secret agentgateway-system a2a-bridge-callers --from-literal=matrix-a2a-bridge="${callers}"
	apply_secret bridge a2a-bridge-credential --from-literal=token="${A2A_KEY}"

	local mcp_callers
	mcp_callers="$(jq --null-input --compact-output --arg key "${MCP_PLATFORM_HELPER_KEY}" \
		'{"platform-helper": {key: $key, metadata: {agent: "platform-helper"}}}')"
	apply_secret agentgateway-system mcp-agent-callers \
		--from-literal=platform-helper="$(jq -cer '."platform-helper"' <<<"${mcp_callers}")"
	apply_secret kagent platform-helper-mcp-credential \
		--from-literal=authorization="Bearer ${MCP_PLATFORM_HELPER_KEY}"

	local mas_admin_config
	mas_admin_config="$(cat <<EOF
clients:
  - client_id: ${MAS_ADMIN_CLIENT_ID}
    client_auth_method: client_secret_basic
    client_secret: ${MAS_ADMIN_CLIENT_SECRET}
policy:
  data:
    admin_clients:
      - ${MAS_ADMIN_CLIENT_ID}
EOF
)"
	apply_secret matrix mas-demo-admin --from-literal=config.yaml="${mas_admin_config}"

	if [ -n "${MODEL_SECRET_NAME}" ]; then
		apply_secret agentgateway-system "${MODEL_SECRET_NAME}" \
			--from-literal=Authorization="${MODEL_SECRET_VALUE}"
	elif [ "${LLM_PROVIDER}" = "vertex" ]; then
		apply_secret agentgateway-system gcp-adc \
			--from-file="key.json=${GOOGLE_APPLICATION_CREDENTIALS}"
	fi
}

create_federation_secrets() {
	local ca_cert="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}/ca.crt"
	[ -r "${ca_cert}" ] || die "local CA certificate not found: ${ca_cert}"
	local bootstrap_json key value
	local bootstrap_arguments=()
	bootstrap_json="$(kubectl --namespace flux-system get secret fgentic-demo-bootstrap \
		--output json 2>/dev/null || printf '{}')"
	# Preserve existing lab identities while making upgrades self-healing when a new homeserver is
	# added to an already running, ownership-labelled cluster.
	for key in pg-synapse pg-synapse-b pg-synapse-c \
		alice-password bob-password charlie-password; do
		value="$(jq -r --arg key "${key}" '.data[$key] // "" | @base64d' \
			<<<"${bootstrap_json}")"
		[ -n "${value}" ] || value="$(random_hex 24)"
		bootstrap_arguments+=("--from-literal=${key}=${value}")
	done
	apply_secret flux-system fgentic-demo-bootstrap "${bootstrap_arguments[@]}"
	bootstrap_json=""
	value=""

	local pg_synapse pg_synapse_b pg_synapse_c namespace
	pg_synapse="$(bootstrap_secret_value pg-synapse)"
	pg_synapse_b="$(bootstrap_secret_value pg-synapse-b)"
	pg_synapse_c="$(bootstrap_secret_value pg-synapse-c)"
	apply_secret postgres pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${pg_synapse}"
	apply_secret matrix pg-synapse --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse --from-literal=password="${pg_synapse}"
	apply_secret postgres pg-synapse-b --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_b --from-literal=password="${pg_synapse_b}"
	apply_secret matrix-b pg-synapse-b --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_b --from-literal=password="${pg_synapse_b}"
	apply_secret postgres pg-synapse-c --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_c --from-literal=password="${pg_synapse_c}"
	apply_secret matrix-c pg-synapse-c --type=kubernetes.io/basic-auth \
		--from-literal=username=synapse_c --from-literal=password="${pg_synapse_c}"

	# Only the public root is mirrored into the homeserver namespaces. The CA key remains in
	# cert-manager, and both runtime and config-check pods mount this ConfigMap read-only.
	for namespace in matrix matrix-b matrix-c; do
		kubectl --namespace "${namespace}" create configmap fgentic-local-ca \
			--from-file="ca.crt=${ca_cert}" --dry-run=client --output=yaml |
			kubectl apply --filename - >/dev/null
	done
}

demo_up() {
	for command in base64 curl docker git jq k3d kubectl flux yq openssl tar; do
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
		LLM_MODEL="unused-federation-profile"
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
		if [ "${PROFILE}" = "federation" ]; then
			CLUSTER_NAME="${CLUSTER_NAME}" FED_LOOPBACK="${FEDERATION_LOOPBACK}" yq '
          .metadata.name = strenv(CLUSTER_NAME) |
          .ports[0].port = (strenv(FED_LOOPBACK) + ":80:80") |
          .ports[1].port = (strenv(FED_LOOPBACK) + ":443:443") |
          .options.k3s.extraArgs += [{
            "arg": "--kubelet-arg=eviction-hard=memory.available<100Mi,nodefs.available<1Gi,imagefs.available<1Gi,nodefs.inodesFree<5%,imagefs.inodesFree<5%",
            "nodeFilters": ["server:*"]
          }]
        ' "${ROOT_DIR}/infra/k3d-config.yaml" >"${WORK_DIR}/k3d-config.yaml"
		else
			CLUSTER_NAME="${CLUSTER_NAME}" yq '.metadata.name = strenv(CLUSTER_NAME)' \
				"${ROOT_DIR}/infra/k3d-config.yaml" \
				>"${WORK_DIR}/k3d-config.yaml"
		fi
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
	fi

	snapshot_source
	build_and_load_images
	flux install >/dev/null
	kubectl --namespace flux-system rollout status deployment/source-controller --timeout=2m
	if ! kubectl get customresourcedefinition \
		gateways.gateway.networking.k8s.io httproutes.gateway.networking.k8s.io \
		>/dev/null 2>&1; then
		kubectl apply --server-side --filename \
			https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml \
			>/dev/null
	fi
	kubectl apply --kustomize "${SNAPSHOT_DIR}/infra/namespaces" >/dev/null
	if [ "${PROFILE}" = "federation" ]; then
		kubectl apply --filename \
			"${SNAPSHOT_DIR}/infra/federation/namespaces/namespace.yaml" >/dev/null
	fi
	"${ROOT_DIR}/scripts/local-ca.sh"
	create_ephemeral_secrets
	apply_source_server
	kubectl apply --kustomize "${SNAPSHOT_DIR}/${OVERLAY_PATH}" >/dev/null

	echo "Reconciling the ${PROFILE} evaluation profile (timeout ${DEMO_TIMEOUT})..."
	wait_for_platform
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

if (($# != 1)); then
	usage >&2
	exit 2
fi

PROFILE="${FGENTIC_DEMO_PROFILE:-demo}"
case "${PROFILE}" in
demo)
	CLUSTER_NAME="${FGENTIC_DEMO_CLUSTER:-${DEFAULT_CLUSTER_NAME}}"
	SERVER_NAME="${DEMO_SERVER_NAME}"
	OVERLAY_PATH="clusters/demo"
	SEED_SCRIPT="scripts/seed-demo.sh"
	OWNER_LABEL="true"
	;;
federation)
	CLUSTER_NAME="${FGENTIC_DEMO_CLUSTER:-${FEDERATION_CLUSTER_NAME}}"
	SERVER_NAME="${FEDERATION_SERVER_NAME}"
	OVERLAY_PATH="clusters/federation"
	SEED_SCRIPT="scripts/seed-federation.sh"
	OWNER_LABEL="federation"
	;;
*) die "unsupported internal evaluation profile: ${PROFILE}" ;;
esac
DEMO_TIMEOUT="${FGENTIC_DEMO_TIMEOUT:-15m}"
FEDERATION_POLICY_PROBE="${FGENTIC_FED_POLICY_PROBE:-deny}"
FEDERATION_GATEWAY_IP=""
BRIDGE_TAG="demo-${RANDOM}-$$"
SOURCE_IMAGE="fgentic-demo-source-${CLUSTER_NAME}:${BRIDGE_TAG}"
SOURCE_REVISION=""
BRIDGE_IMAGE="matrix-a2a-bridge:${BRIDGE_TAG}"
[[ "${CLUSTER_NAME}" =~ ^[a-z0-9][a-z0-9-]{0,47}$ ]] || die "invalid FGENTIC_DEMO_CLUSTER"
if [ "${PROFILE}" = "demo" ]; then
	case "${CLUSTER_NAME}" in
	fgentic-demo | fgentic-demo-*) ;;
	*) die "FGENTIC_DEMO_CLUSTER must be fgentic-demo or start with fgentic-demo-" ;;
	esac
elif [ "${CLUSTER_NAME}" != "${FEDERATION_CLUSTER_NAME}" ]; then
	die "the federation profile cluster must be ${FEDERATION_CLUSTER_NAME}"
fi
[[ "${DEMO_TIMEOUT}" =~ ^[1-9][0-9]*[smh]$ ]] || die "invalid FGENTIC_DEMO_TIMEOUT"
if [ "${PROFILE}" = "federation" ]; then
	case "${FEDERATION_POLICY_PROBE}" in
	allow | deny) ;;
	*) die "FGENTIC_FED_POLICY_PROBE must be allow or deny" ;;
	esac
fi

case "$1" in
up) demo_up ;;
down) demo_down ;;
-h | --help)
	usage
	;;
*)
	usage >&2
	exit 2
	;;
esac
