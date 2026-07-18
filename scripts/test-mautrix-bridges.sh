#!/usr/bin/env bash
# Validate the optional mautrix bridge layers without enabling them or contacting their networks.
# `--runtime` additionally checks each exact pinned image's registration schema and proves its
# non-root/read-only config reaches database initialization with mounted secret-file overrides.
# shellcheck disable=SC2016 # yq bindings and bridged-sender fixture placeholders are intentionally literal
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
RUNTIME=false
if [ "${1:-}" = "--runtime" ]; then
	RUNTIME=true
elif [ "$#" -ne 0 ]; then
	echo "usage: scripts/test-mautrix-bridges.sh [--runtime]" >&2
	exit 2
fi

for command in flux helm jq kubeconform kubectl yq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 1
	fi
done
if [ "${RUNTIME}" = "true" ] && ! command -v docker >/dev/null 2>&1; then
	echo "error: required runtime command not found: docker" >&2
	exit 1
fi

WORK_DIR="$(mktemp -d)"
COMPONENT_DIR="${ROOT_DIR}/.agents/tmp/mautrix-components-test.$$"
cleanup() {
	rm -rf "${WORK_DIR}" "${COMPONENT_DIR}"
}
trap cleanup EXIT INT TERM
export server_name=ci.fgentic.example

fail() {
	echo "error: $1" >&2
	exit 1
}

assert_yq() { # assert_yq <expression> <file> <message>
	local result
	if ! result="$(yq eval -o=json -I=0 '.' "$2" | jq -c "$1")"; then
		fail "$3"
	fi
	# Each contract names one resource. Empty, duplicate, or false results are all failures.
	[ "${result}" = 'true' ] || fail "$3"
}

set_bridge() { # set_bridge <slack|telegram>
	NETWORK="$1"
	case "${NETWORK}" in
		slack)
			DISPLAY_NAME=Slack
			PORT=29335
			ROLE=slackbridge
			BOT=slackbot
			GHOST_PREFIX=slack_
			IMAGE="dock.mau.dev/mautrix/slack@sha256:f1de44e723a13484a6b09a26b93127e494c25a70d4d21c2300bfddf49a7dae03"
			BINARY=/usr/bin/mautrix-slack
			TAG=v0.2606.0
			COMMIT=813cabaa9382a07ac3515b0dbc484fb0fe138385
			ENV_PREFIX=MAUTRIX_SLACK_
			EXPECTED_ENV="MAUTRIX_SLACK_APPSERVICE__AS_TOKEN_FILE,MAUTRIX_SLACK_APPSERVICE__HS_TOKEN_FILE,MAUTRIX_SLACK_DATABASE__URI_FILE"
			EXPECTED_SECRET_KEYS="as-token,database-uri,hs-token"
			;;
		telegram)
			DISPLAY_NAME=Telegram
			PORT=29317
			ROLE=telegrambridge
			BOT=telegrambot
			GHOST_PREFIX=telegram_
			IMAGE="dock.mau.dev/mautrix/telegram@sha256:8c6c559446f049c1f3c4cbc4b284aed14c27aefde9b88a785d262633bdafe510"
			BINARY=/usr/bin/mautrix-telegram
			TAG=v0.2606.0
			COMMIT=dbcbfc66dec816d56fa3373e93f1a0c8483baa1f
			ENV_PREFIX=MAUTRIX_TELEGRAM_
			EXPECTED_ENV="MAUTRIX_TELEGRAM_APPSERVICE__AS_TOKEN_FILE,MAUTRIX_TELEGRAM_APPSERVICE__HS_TOKEN_FILE,MAUTRIX_TELEGRAM_DATABASE__URI_FILE,MAUTRIX_TELEGRAM_NETWORK__API_HASH_FILE,MAUTRIX_TELEGRAM_NETWORK__API_ID_FILE"
			EXPECTED_SECRET_KEYS="api-hash,api-id,as-token,database-uri,hs-token"
			;;
		*) fail "unsupported bridge fixture: ${NETWORK}" ;;
	esac
	NAME="mautrix-${NETWORK}"
	RELEASE="${ROOT_DIR}/infra/bridges/${NETWORK}/helmrelease.yaml"
	EXAMPLE="${ROOT_DIR}/infra/secrets/mautrix-${NETWORK}.sops.yaml.example"
	RENDERED="${WORK_DIR}/${NETWORK}-runtime.yaml"
	CONFIG="${WORK_DIR}/${NETWORK}-config.yaml"
}

render_and_validate_bridge() {
	yq -e '.spec.values' "${RELEASE}" \
		| helm template "${NAME}" "${ROOT_DIR}/infra/bridges/chart" \
			--namespace bridges --values - >"${RENDERED}"
	kubeconform -strict -ignore-missing-schemas -summary "${RENDERED}" >/dev/null

	assert_yq \
		'select(.kind == "StatefulSet" and .metadata.name == "'"${NAME}"'") |
		 (.spec.replicas == 1 and
		 .spec.template.spec.automountServiceAccountToken == false and
		 .spec.template.spec.securityContext.runAsNonRoot == true and
		 .spec.template.spec.securityContext.runAsUser == 1337 and
		 ([.spec.template.spec.volumes[] | select(.name == "tmp") | .emptyDir.sizeLimit] | join(",") == "256Mi") and
		 .spec.template.spec.containers[0].image == "'"${IMAGE}"'" and
		 .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem == true and
		 .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation == false and
		 .spec.template.spec.containers[0].resources.requests."ephemeral-storage" == "64Mi" and
		 .spec.template.spec.containers[0].resources.limits."ephemeral-storage" == "512Mi" and
		 (.spec.template.spec.containers[0].securityContext.capabilities.drop | join(",") == "ALL") and
		 (.spec.template.spec.containers[0].command | join(",") == "'"${BINARY}"'") and
		 (.spec.template.spec.containers[0].args | join(",") == "--config,/data/config.yaml,--no-update") and
		 ([.spec.template.spec.containers[0].env[].name] | sort | join(",") == "'"${EXPECTED_ENV}"'"))' \
		"${RENDERED}" "${DISPLAY_NAME} StatefulSet lost its immutable/non-root/read-only contract"
	assert_yq \
		'select(.kind == "Service" and .metadata.name == "'"${NAME}"'") |
		 (.spec.publishNotReadyAddresses == true and .spec.ports[0].port == '"${PORT}"')' \
		"${RENDERED}" "${DISPLAY_NAME} Service lost the upstream startup contract"
	assert_yq \
		'select(.kind == "NetworkPolicy" and .metadata.name == "'"${NAME}"'") |
		 ((.spec.policyTypes | join(",") == "Ingress,Egress") and
		 .spec.ingress[0].from[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "matrix" and
		 .spec.ingress[0].from[0].podSelector.matchLabels."k8s.element.io/synapse-instance" == "ess-synapse" and
		 .spec.egress[1].to[0].namespaceSelector.matchLabels."kubernetes.io/metadata.name" == "matrix" and
		 .spec.egress[1].to[0].podSelector.matchLabels."app.kubernetes.io/instance" == "ess-haproxy" and
		 ([.spec.egress[].ports[]?.port] | sort | join(",") == "53,53,443,5432,8008") and
		 ([.spec.egress[].to[] | select(has("ipBlock")) | .ipBlock.except[]] | sort | join(",") == "10.0.0.0/8,100.64.0.0/10,127.0.0.0/8,169.254.0.0/16,172.16.0.0/12,192.168.0.0/16"))' \
		"${RENDERED}" "${DISPLAY_NAME} NetworkPolicy lost its scoped internal or public-IPv4 TLS boundary"

	yq -e 'select(.kind == "ConfigMap" and .metadata.name == "'"${NAME}"'") | .data."config.yaml"' \
		"${RENDERED}" | flux envsubst --strict >"${CONFIG}"
	assert_yq \
		'.bridge.bridge_notices == true and
		 .bridge.relay.enabled == true and
		 .bridge.relay.admin_only == true and
		 .bridge.relay.prefer_default == false and
		 .bridge.relay.allow_bridge == false and
		 (.bridge.relay.default_relays | length) == 0 and
		 (.bridge.relay.message_formats."m.notice" | length) > 0 and
		 .bridge.portal_create_filter.mode == "allow" and
		 (.bridge.portal_create_filter.list | length) == 0 and
		 .bridge.permissions."*" == "block" and
		 .bridge.permissions."@a2a-bridge:ci.fgentic.example" == "relay" and
		 .bridge.permissions."@agent-docs-qa:ci.fgentic.example" == "relay" and
		 .bridge.permissions."@agent-platform-helper:ci.fgentic.example" == "relay" and
		 .bridge.permissions."@agent-scribe:ci.fgentic.example" == "relay" and
		 .bridge.permissions."@alice:ci.fgentic.example" == "admin" and
		 (.bridge.permissions | length) == 6 and
		 .matrix.federate_rooms == false and
		 .provisioning.shared_secret == "disable" and
		 .backfill.enabled == false and
		 .encryption.allow == false and
		 .env_config_prefix == "'"${ENV_PREFIX}"'" and
		 (.logging.writers | length) == 1 and
		 .logging.writers[0].type == "stdout" and .logging.writers[0].format == "json"' \
		"${CONFIG}" "${DISPLAY_NAME} config lost its common fail-closed defaults"
	case "${NETWORK}" in
		slack)
			assert_yq '.network.backfill.conversation_count == 0' "${CONFIG}" \
				"Slack login would enumerate visible conversations"
			;;
		telegram)
			assert_yq \
				'.network.sync.update_limit == 0 and
			 .network.sync.create_limit == 0 and
			 .network.sync.login_sync_limit == 0 and
			 .network.sync.direct_chats == false and
			 .network.takeout.dialog_sync == false and
			 .network.takeout.forward_backfill == false and
			 .network.takeout.backward_backfill == false' \
				"${CONFIG}" "Telegram login/history sync widened beyond explicit portal selection"
			;;
		*) fail "unsupported mautrix bridge network: ${NETWORK}" ;;
	esac
	if yq -e '.. | select(tag == "!!str" and . == "generate")' "${CONFIG}" >/dev/null 2>&1; then
		fail "read-only ${DISPLAY_NAME} config contains a generated-at-startup value"
	fi

	assert_yq \
		'select(.kind == "Secret" and .metadata.namespace == "postgres" and .metadata.name == "pg-'"${ROLE}"'") |
		 .stringData.username == "'"${ROLE}"'"' \
		"${EXAMPLE}" "${DISPLAY_NAME} CNPG credential template is missing"
	assert_yq \
		'select(.kind == "Secret" and .metadata.namespace == "bridges" and .metadata.name == "'"${NAME}"'") |
		 (has("stringData") and
		 (.stringData | keys | sort | join(",") == "'"${EXPECTED_SECRET_KEYS}"'"))' \
		"${EXAMPLE}" "${DISPLAY_NAME} runtime Secret shape drifted"
	registration_example="${WORK_DIR}/${NETWORK}-registration-example.yaml"
	yq eval-all -N -r \
		'select(.kind == "Secret" and .metadata.namespace == "matrix" and .metadata.name == "'"${NAME}"'-registration") |
		 .stringData."registration.yaml"' "${EXAMPLE}" >"${registration_example}"
	assert_yq \
		'.id == "'"${NETWORK}"'" and
		 .url == "http://'"${NAME}"'.bridges.svc.cluster.local:'"${PORT}"'" and
		 .rate_limited == false and
		 .receive_ephemeral == true and
		 ."de.sorunome.msc2409.push_ephemeral" == true and
		 (.namespaces.users | length) == 2 and
		 .namespaces.users[0].regex == "^@'"${BOT}"':REPLACE_WITH_ESCAPED_SERVER_NAME$" and
		 .namespaces.users[1].regex == "^@'"${GHOST_PREFIX}"'.*:REPLACE_WITH_ESCAPED_SERVER_NAME$"' \
		"${registration_example}" "${DISPLAY_NAME} appservice registration template drifted"
}

for network in slack telegram; do
	set_bridge "${network}"
	render_and_validate_bridge
done

# The core Matrix-A2A bridge has no external origin by default. Each selected network profile must
# add its own anchored namespace through the canonical bridge Flux component list.
origin_manifest="${ROOT_DIR}/apps/matrix-a2a-bridge/deploy/helmrelease.yaml"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") |
	 ((.spec.values.bridgedOrigins | length) == 0 and
	 ([.spec.values.agents[].allowedSenders[]] | unique | join(",") == "@alice:${server_name}"))' \
	"${origin_manifest}" "Matrix-A2A base manifest must not classify an unselected external bridge"
if grep -Eq 'xox[bapcds]-' "${ROOT_DIR}/infra/secrets/mautrix-slack.sops.yaml.example"; then
	fail "Slack workspace credentials must remain runtime login state, not deployment Secrets"
fi

# Both shipping overlays remain off. Selecting both components must append independent Postgres
# and Matrix components without replacing canonical Flux paths or each other.
base_render="${WORK_DIR}/base.yaml"
kubectl kustomize "${ROOT_DIR}/clusters/base" >"${base_render}"
for network in slack telegram; do
	if yq -e 'select(.kind == "Kustomization" and .metadata.name == "mautrix-'"${network}"'")' \
		"${base_render}" >/dev/null 2>&1; then
		fail "${network} bridge is enabled in the base overlay"
	fi
done
for layer in postgres matrix bridge; do
	assert_yq \
		'select(.kind == "Kustomization" and .metadata.name == "'"${layer}"'") |
		 (.spec.components | length) == 0' \
		"${base_render}" "base ${layer} Flux layer must expose an empty opt-in component list"
done
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "bridge") |
	 (.spec.patches | length) == 0' \
	"${base_render}" "base bridge Flux layer must expose an empty target-cluster policy patch list"

mkdir -p "${COMPONENT_DIR}"
printf '%s\n' \
	'apiVersion: kustomize.config.k8s.io/v1beta1' \
	'kind: Kustomization' \
	'resources:' \
	'  - ../../../clusters/base' \
	'components:' \
	'  - ../../../infra/bridges/slack/cluster' \
	'  - ../../../infra/bridges/telegram/cluster' >"${COMPONENT_DIR}/kustomization.yaml"
component_render="${WORK_DIR}/components.yaml"
kubectl kustomize "${COMPONENT_DIR}" >"${component_render}"
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "postgres") |
	 (.spec.path == "./infra/postgres" and
	 (.spec.components | sort | join(",") == "../bridges/slack/postgres,../bridges/telegram/postgres"))' \
	"${component_render}" "bridge components did not compose in the Postgres layer"
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix") |
	 (.spec.path == "./infra/matrix" and
	 (.spec.components | sort | join(",") == "../bridges/slack/matrix,../bridges/telegram/matrix"))' \
	"${component_render}" "bridge components did not compose in the Matrix layer"
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "bridge") |
	 (.spec.path == "./apps/matrix-a2a-bridge/deploy" and
	 (.spec.components | sort | join(",") == "../../../infra/bridges/slack/a2a,../../../infra/bridges/telegram/a2a"))' \
	"${component_render}" "bridge components did not compose in the Matrix-A2A layer"
for network in slack telegram; do
	assert_yq \
		'select(.kind == "Kustomization" and .metadata.name == "mautrix-'"${network}"'") |
		 .spec.path == "./infra/bridges/'"${network}"'"' \
		"${component_render}" "${network} runtime Flux path drifted"
	assert_yq \
		'select(.kind == "Kustomization" and .metadata.name == "mautrix-'"${network}"'") |
		 (([.spec.dependsOn[].name] | sort | join(",")) == "bridge,matrix,platform-secrets,postgres")' \
		"${component_render}" "${network} runtime Flux dependencies drifted"
done

postgres_fixture="${WORK_DIR}/postgres-flux.yaml"
printf '%s\n' \
	'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
	'kind: Kustomization' \
	'metadata: {name: postgres-test, namespace: flux-system}' \
	'spec:' \
	'  interval: 30m' \
	'  path: ./infra/postgres' \
	'  components:' \
	'    - ../bridges/slack/postgres' \
	'    - ../bridges/telegram/postgres' \
	'  sourceRef: {kind: GitRepository, name: flux-system}' >"${postgres_fixture}"
postgres_render="${WORK_DIR}/postgres.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization postgres-test --path infra/postgres \
		--kustomization-file "${postgres_fixture}" --dry-run --in-memory-build
) >"${postgres_render}"
for role in slackbridge telegrambridge; do
	assert_yq \
		'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
		 ([.spec.managed.roles[] | select(.name == "'"${role}"'" and .login == true and .passwordSecret.name == "pg-'"${role}"'" and ((.disablePassword // false) == false))] | length) == 1' \
		"${postgres_render}" "${role} active CNPG login/password contract drifted"
	assert_yq \
		'select(.kind == "Database" and .metadata.name == "'"${role}"'") |
		 (.spec.cluster.name == "platform-pg" and
		 .spec.owner == "'"${role}"'" and
		 .spec.databaseReclaimPolicy == "retain")' \
		"${postgres_render}" "${role} CNPG Database contract drifted"
done
assert_yq \
	'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
	 ((.spec.postgresql.pg_hba | sort | join(",")) == "hostnossl all all all reject,hostssl all all all reject,hostssl bridge bridge all scram-sha-256,hostssl kagent kagent all scram-sha-256,hostssl keycloak keycloak all scram-sha-256,hostssl knowledge knowledge_owner all scram-sha-256,hostssl knowledge knowledge_retrieval all scram-sha-256,hostssl mas mas all scram-sha-256,hostssl slackbridge slackbridge all scram-sha-256,hostssl synapse synapse all scram-sha-256,hostssl telegrambridge telegrambridge all scram-sha-256" and
	 .spec.postgresql.pg_hba[-2] == "hostssl all all all reject" and
	 .spec.postgresql.pg_hba[-1] == "hostnossl all all all reject")' \
	"${postgres_render}" "Postgres HBA no longer enforces one TLS database per tenant role"

matrix_fixture="${WORK_DIR}/matrix-flux.yaml"
printf '%s\n' \
	'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
	'kind: Kustomization' \
	'metadata: {name: matrix-test, namespace: flux-system}' \
	'spec:' \
	'  interval: 30m' \
	'  path: ./infra/matrix' \
	'  components:' \
	'    - ../bridges/slack/matrix' \
	'    - ../bridges/telegram/matrix' \
	'  sourceRef: {kind: GitRepository, name: flux-system}' >"${matrix_fixture}"
matrix_render="${WORK_DIR}/matrix.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization matrix-test --path infra/matrix \
		--kustomization-file "${matrix_fixture}" --dry-run --in-memory-build
) >"${matrix_render}"
for network in slack telegram; do
	assert_yq \
		'select(.kind == "HelmRelease" and .metadata.name == "matrix-stack") |
		 (([.spec.values.synapse.appservices[].secret] | map(select(. == "mautrix-'"${network}"'-registration")) | length) == 1)' \
		"${matrix_render}" "ESS did not receive exactly one ${network} registration"
done

bridge_fixture="${WORK_DIR}/bridge-flux.yaml"
printf '%s\n' \
	'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
	'kind: Kustomization' \
	'metadata: {name: bridge-test, namespace: flux-system}' \
	'spec:' \
	'  interval: 30m' \
	'  path: ./apps/matrix-a2a-bridge/deploy' \
	'  components:' \
	'    - ../../../infra/bridges/slack/a2a' \
	'    - ../../../infra/bridges/telegram/a2a' \
	'  sourceRef: {kind: GitRepository, name: flux-system}' >"${bridge_fixture}"
bridge_render="${WORK_DIR}/bridge.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization bridge-test --path apps/matrix-a2a-bridge/deploy \
		--kustomization-file "${bridge_fixture}" --dry-run --in-memory-build
) >"${bridge_render}"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") |
	 ((.spec.values.bridgedOrigins | keys | sort | join(",")) == "slack,telegram" and
	 .spec.values.bridgedOrigins.slack[0] == "@slack_*:${server_name}" and
	 .spec.values.bridgedOrigins.telegram[0] == "@telegram_*:${server_name}" and
	 ([.spec.values.agents[].allowedSenders[]] | unique | join(",") == "@alice:${server_name}"))' \
	"${bridge_render}" "selected external namespaces did not compose in Matrix-A2A"

# Remote identities are live, per-cluster policy and must never enter the canonical HelmRelease.
# Prove the documented outer JSON append preserves existing Flux patches, composes with the
# selected origin, and leaves every other agent local.
printf '%s\n' \
	'apiVersion: kustomize.config.k8s.io/v1beta1' \
	'kind: Kustomization' \
	'resources:' \
	'  - ../../../clusters/base' \
	'components:' \
	'  - ../../../infra/bridges/slack/cluster' \
	'patches:' \
	'  - target:' \
	'      group: kustomize.toolkit.fluxcd.io' \
	'      version: v1' \
	'      kind: Kustomization' \
	'      name: bridge' \
	'      namespace: flux-system' \
	'    patch: |' \
	'      - op: add' \
	'        path: /spec/patches/-' \
	'        value:' \
	'          target:' \
	'            kind: HelmRelease' \
	'            name: matrix-a2a-bridge' \
	'          patch: |-' \
	'            - op: add' \
	'              path: /spec/values/agents/agent-docs-qa/allowedSenders/-' \
	'              value: "@slack_t0123456789-u0123456789:${server_name}"' >"${COMPONENT_DIR}/kustomization.yaml"
scoped_cluster_render="${WORK_DIR}/cluster-scoped-policy.yaml"
kubectl kustomize "${COMPONENT_DIR}" >"${scoped_cluster_render}"
scoped_bridge_fixture="${WORK_DIR}/bridge-scoped-policy-flux.yaml"
yq eval 'select(.kind == "Kustomization" and .metadata.name == "bridge")' \
	"${scoped_cluster_render}" >"${scoped_bridge_fixture}"
scoped_bridge_render="${WORK_DIR}/bridge-scoped-policy.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization bridge --path apps/matrix-a2a-bridge/deploy \
		--kustomization-file "${scoped_bridge_fixture}" --dry-run --in-memory-build
) >"${scoped_bridge_render}"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "matrix-a2a-bridge") |
	 ((.spec.values.bridgedOrigins | keys | join(",")) == "slack" and
	 (.spec.values.agents."agent-docs-qa".allowedSenders | join(",") == "@alice:${server_name},@slack_t0123456789-u0123456789:${server_name}") and
	 .spec.values.agents."agent-platform-helper".allowedSenders[0] == "@alice:${server_name}" and
	 .spec.values.agents."agent-scribe".allowedSenders[0] == "@alice:${server_name}")' \
	"${scoped_bridge_render}" "target-cluster sender policy did not remain scoped to the selected bridge"

# Offboarding is a two-phase GitOps transition: replace the runtime components with these
# temporary NOLOGIN components, verify CNPG applies the revocation, then remove the declarations.
printf '%s\n' \
	'apiVersion: kustomize.config.k8s.io/v1beta1' \
	'kind: Kustomization' \
	'resources:' \
	'  - ../../../clusters/base' \
	'components:' \
	'  - ../../../infra/bridges/slack/cluster-offboard' \
	'  - ../../../infra/bridges/telegram/cluster-offboard' >"${COMPONENT_DIR}/kustomization.yaml"
offboard_component_render="${WORK_DIR}/offboard-components.yaml"
kubectl kustomize "${COMPONENT_DIR}" >"${offboard_component_render}"
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "postgres") |
	 ((.spec.components | sort | join(",")) == "../bridges/slack/postgres-offboard,../bridges/telegram/postgres-offboard")' \
	"${offboard_component_render}" "bridge offboarding components did not compose in the Postgres layer"
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "matrix") |
	 (.spec.components | length) == 0' \
	"${offboard_component_render}" "bridge offboarding must not retain Matrix registrations"
assert_yq \
	'select(.kind == "Kustomization" and .metadata.name == "bridge") |
	 (.spec.components | length) == 0' \
	"${offboard_component_render}" "bridge offboarding must not retain external sender origins"
for network in slack telegram; do
	if yq -e 'select(.kind == "Kustomization" and .metadata.name == "mautrix-'"${network}"'")' \
		"${offboard_component_render}" >/dev/null 2>&1; then
		fail "${network} offboarding component retained the runtime workload"
	fi
done

offboard_postgres_fixture="${WORK_DIR}/postgres-offboard-flux.yaml"
printf '%s\n' \
	'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
	'kind: Kustomization' \
	'metadata: {name: postgres-offboard-test, namespace: flux-system}' \
	'spec:' \
	'  interval: 30m' \
	'  path: ./infra/postgres' \
	'  components:' \
	'    - ../bridges/slack/postgres-offboard' \
	'    - ../bridges/telegram/postgres-offboard' \
	'  sourceRef: {kind: GitRepository, name: flux-system}' >"${offboard_postgres_fixture}"
offboard_postgres_render="${WORK_DIR}/postgres-offboard.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization postgres-offboard-test --path infra/postgres \
		--kustomization-file "${offboard_postgres_fixture}" --dry-run --in-memory-build
) >"${offboard_postgres_render}"
for role in slackbridge telegrambridge; do
	assert_yq \
		'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
		 ([.spec.managed.roles[] | select(.name == "'"${role}"'" and .ensure == "present" and .login == false and .disablePassword == true and (has("passwordSecret") | not))] | length) == 1' \
		"${offboard_postgres_render}" "${role} offboarding did not render exactly one credential-free NOLOGIN role"
	if yq -e 'select(.kind == "Database" and .metadata.name == "'"${role}"'")' \
		"${offboard_postgres_render}" >/dev/null 2>&1; then
		fail "${role} offboarding component must leave the retained database unmanaged"
	fi
done
assert_yq \
	'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
	 ((.spec.postgresql.pg_hba | sort | join(",")) == "hostnossl all all all reject,hostssl all all all reject,hostssl bridge bridge all scram-sha-256,hostssl kagent kagent all scram-sha-256,hostssl keycloak keycloak all scram-sha-256,hostssl knowledge knowledge_owner all scram-sha-256,hostssl knowledge knowledge_retrieval all scram-sha-256,hostssl mas mas all scram-sha-256,hostssl synapse synapse all scram-sha-256" and
	 .spec.postgresql.pg_hba[-2] == "hostssl all all all reject" and
	 .spec.postgresql.pg_hba[-1] == "hostnossl all all all reject")' \
	"${offboard_postgres_render}" "offboarding retained an external-bridge HBA login path"

runtime_validate_bridge() {
	version_json="$(docker run --rm --entrypoint "${BINARY}" "${IMAGE}" --version-json)"
	jq -e \
		'.Tag == "'"${TAG}"'" and
		 .Commit == "'"${COMMIT}"'" and
		 .IsRelease == true and
		 .Mautrix.Version == "v0.28.1"' \
		<<<"${version_json}" >/dev/null || fail "${DISPLAY_NAME} image provenance differs from the audited release"

	generated_data="${WORK_DIR}/${NETWORK}-generated-data"
	generated_output="${WORK_DIR}/${NETWORK}-generated-output"
	mkdir -p "${generated_data}" "${generated_output}"
	chmod 0777 "${generated_data}" "${generated_output}"
	cp "${CONFIG}" "${generated_data}/config.yaml"
	chmod 0666 "${generated_data}/config.yaml"
	docker run --rm --user "$(id -u):$(id -g)" --entrypoint "${BINARY}" \
		-v "${generated_data}:/data" -v "${generated_output}:/output" \
		"${IMAGE}" --config /data/config.yaml --generate-registration \
		--registration /output/registration.yaml >/dev/null
	generated_registration="${generated_output}/registration.yaml"
	assert_yq \
		'.id == "'"${NETWORK}"'" and
		 .url == "http://'"${NAME}"'.bridges.svc.cluster.local:'"${PORT}"'" and
		 .rate_limited == false and
		 .receive_ephemeral == true and
		 ."de.sorunome.msc2409.push_ephemeral" == true and
		 (.sender_localpart | test("^[A-Za-z0-9]{32}$")) and
		 .namespaces.users[0].regex == "^@'"${BOT}"':ci\\.fgentic\\.example$" and
		 .namespaces.users[1].regex == "^@'"${GHOST_PREFIX}"'.*:ci\\.fgentic\\.example$"' \
		"${generated_registration}" "${DISPLAY_NAME} binary registration schema differs from the committed contract"

	runtime_secrets="${WORK_DIR}/${NETWORK}-runtime-secrets"
	mkdir -p "${runtime_secrets}"
	printf '%s' \
		"postgres://${ROLE}:fixture@127.0.0.1:1/${ROLE}?sslmode=require&connect_timeout=1" \
		>"${runtime_secrets}/database-uri"
	printf '%s' 'fixture-as-token-000000000000000000000000' >"${runtime_secrets}/as-token"
	printf '%s' 'fixture-hs-token-000000000000000000000000' >"${runtime_secrets}/hs-token"
	docker_env=(
		-e "${ENV_PREFIX}DATABASE__URI_FILE=/run/secrets/database-uri"
		-e "${ENV_PREFIX}APPSERVICE__AS_TOKEN_FILE=/run/secrets/as-token"
		-e "${ENV_PREFIX}APPSERVICE__HS_TOKEN_FILE=/run/secrets/hs-token"
	)
	if [ "${NETWORK}" = "telegram" ]; then
		printf '%s' '123456' >"${runtime_secrets}/api-id"
		printf '%s' '0123456789abcdef0123456789abcdef' >"${runtime_secrets}/api-hash"
		docker_env+=(
			-e MAUTRIX_TELEGRAM_NETWORK__API_ID_FILE=/run/secrets/api-id
			-e MAUTRIX_TELEGRAM_NETWORK__API_HASH_FILE=/run/secrets/api-hash
		)
	fi
	chmod -R a+rX "${runtime_secrets}" "${CONFIG}"
	set +e
	docker run --rm --user "$(id -u):$(id -g)" --read-only \
		--tmpfs /tmp:rw,noexec,nosuid,size=16m --entrypoint "${BINARY}" \
		"${docker_env[@]}" \
		-v "${CONFIG}:/data/config.yaml:ro" -v "${runtime_secrets}:/run/secrets:ro" \
		"${IMAGE}" --config /data/config.yaml --no-update >"${WORK_DIR}/${NETWORK}-runtime.log" 2>&1
	runtime_status=$?
	set -e
	[ "${runtime_status}" -ne 0 ] || fail "${DISPLAY_NAME} unreachable DB fixture unexpectedly started"
	grep -q 'Initializing bridge' "${WORK_DIR}/${NETWORK}-runtime.log" || fail "${DISPLAY_NAME} did not initialize"
	if grep -Eq 'Configuration error|Failed to parse config|Failed to parse environment variables' \
		"${WORK_DIR}/${NETWORK}-runtime.log"; then
		fail "${DISPLAY_NAME} rejected the read-only deployment config"
	fi
	grep -q '127.0.0.1:1' "${WORK_DIR}/${NETWORK}-runtime.log" || fail "${DISPLAY_NAME} DB secret override was not used"
}

if [ "${RUNTIME}" = "true" ]; then
	for network in slack telegram; do
		set_bridge "${network}"
		runtime_validate_bridge
	done
	echo "mautrix pinned image/config/registration contracts passed; no external-network request was made."
else
	echo "mautrix bridge renders, privacy defaults, and dual Flux composition passed (layers remain opt-in)."
fi
