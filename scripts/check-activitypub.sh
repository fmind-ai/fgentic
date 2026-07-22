#!/usr/bin/env bash
# Offline validation for the demo-reconciled ActivityPub agent gateway (issue #489). Renders the
# self-contained deploy unit, the Helm chart with the exact deploy values, and the cluster overlays
# WITHOUT contacting a cluster, then asserts the security-load-bearing invariants:
#   * a default-deny NetworkPolicy (ingress only from the Gateway + Prometheus; egress only DNS +
#     agentgateway) — the deployment-side defense-in-depth beside the in-code SSRF guard (#320);
#   * the public HTTPRoute stays gated off on the deploy unit;
#   * the demo overlay is the SOLE composition point — local, gcp, and base keep the gateway absent;
#   * the demo Flux Kustomization depends on platform-secrets/agentgateway/kagent, never matrix;
#   * the cluster-only demo secret path (not the SOPS/production path) provisions the keys.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"

for command in flux helm jq kubeconform kubectl yq; do
	if ! command -v "${command}" >/dev/null 2>&1; then
		echo "error: required command not found: ${command}" >&2
		exit 1
	fi
done

WORK_DIR="$(mktemp -d)"
cleanup() { rm -rf "${WORK_DIR}"; }
trap cleanup EXIT INT TERM

DEPLOY_DIR="${ROOT_DIR}/apps/activitypub-agent-gateway/deploy"
CHART_DIR="${ROOT_DIR}/apps/activitypub-agent-gateway/chart"
FIXTURE="${ROOT_DIR}/scripts/testdata/flux-build-kustomization.yaml"

fail() {
	echo "error: $1" >&2
	exit 1
}

assert_yq() { # assert_yq <jq-expr> <file> <message>
	local result
	if ! result="$(yq eval -o=json -I=0 '.' "$2" | jq -c "$1")"; then
		fail "$3"
	fi
	[ "${result}" = 'true' ] || fail "$3"
}

# --- 1. The Helm chart rendered with the EXACT deploy values -----------------------------------
chart_values="${WORK_DIR}/deploy-values.yaml"
yq -e '.spec.values' "${DEPLOY_DIR}/helmrelease.yaml" >"${chart_values}"
chart_render="${WORK_DIR}/chart.yaml"
helm template activitypub-agent-gateway "${CHART_DIR}" \
	--namespace activitypub --values "${chart_values}" >"${chart_render}"
kubeconform -strict -ignore-missing-schemas -summary "${chart_render}" >/dev/null

if yq -e 'select(.kind == "HTTPRoute")' "${chart_render}" >/dev/null 2>&1; then
	fail "deploy values must keep the public HTTPRoute gated off (no HTTPRoute may render)"
fi
# The deploy unit owns an authoritative NetworkPolicy, so the chart's ingress-only policy is disabled.
if yq -e 'select(.kind == "NetworkPolicy")' "${chart_render}" >/dev/null 2>&1; then
	fail "chart NetworkPolicy must be disabled in the deploy values (the deploy unit ships its own)"
fi
# The signed border and cross-transport identity must stay ON for the reconciled gateway, and — with
# the border on — the chart fails closed unless the durable activity store (DATABASE_URL) is wired
# (#321 duplicate-spend/dedup). A rendered Deployment therefore already proves database.enabled.
assert_yq \
	'select(.kind == "Deployment") |
	 ([.spec.template.spec.containers[0].env[] | select(.name == "POLICY_PATH")] | length) == 1 and
	 ([.spec.template.spec.containers[0].env[] | select(.name == "DATABASE_URL") |
	   select(.valueFrom.secretKeyRef.name == "activitypub-agent-gateway-db" and
	          .valueFrom.secretKeyRef.key == "url")] | length) == 1 and
	 ([.spec.template.spec.volumes[] | select(.name == "signing-key")] | length) == 1 and
	 ([.spec.template.spec.volumes[] | select(.name == "identity-key")] | length) == 1' \
	"${chart_render}" "reconciled gateway lost its border/integrity/identity/durable-store wiring"

# --- 2. The standalone default-deny NetworkPolicy ----------------------------------------------
np="${DEPLOY_DIR}/networkpolicy.yaml"
[ -f "${np}" ] || fail "deploy unit is missing networkpolicy.yaml"
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.namespace == "activitypub") |
	 ((.spec.policyTypes | sort | join(",")) == "Egress,Ingress" and
	  .spec.podSelector.matchLabels."app.kubernetes.io/name" == "activitypub-agent-gateway" and
	  ([.spec.ingress[].from[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] | sort | join(",")) == "gateway,monitoring" and
	  ([.spec.ingress[].ports[].port] | sort | join(",")) == "8480,9090" and
	  ([.spec.egress[].to[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] | sort | join(",")) == "agentgateway-system,kube-system,postgres" and
	  ([.spec.egress[].ports[].port] | sort | join(",")) == "53,53,5432,8080")' \
	"${np}" "AP default-deny NetworkPolicy lost its scoped ingress/egress boundary"
# Fail-closed by construction: no rule may open the public internet or an unscoped destination.
if yq -e 'select(.kind == "NetworkPolicy") | .spec.egress[].to[] | select(has("ipBlock"))' \
	"${np}" >/dev/null 2>&1; then
	fail "AP NetworkPolicy egress must not carry a public ipBlock (the public border stays gated)"
fi

# --- 3. The deploy unit rendered through Flux (substitution + kubeconform) ----------------------
# --strict-substitute fails the build if any platform-settings variable is left unresolved, so a
# clean render already proves substitution completeness.
deploy_render="${WORK_DIR}/deploy.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization cluster-overlay-validation \
		--path apps/activitypub-agent-gateway/deploy \
		--kustomization-file "${FIXTURE}" \
		--dry-run --in-memory-build --strict-substitute
) >"${deploy_render}"
kubeconform -strict -ignore-missing-schemas -summary "${deploy_render}" >/dev/null
for spec in "Namespace=activitypub" "ResourceQuota=compute-budget" "LimitRange=container-defaults" \
	"NetworkPolicy=activitypub-agent-gateway" "ConfigMap=fgentic-activitypub-policy" \
	"HelmRelease=activitypub-agent-gateway"; do
	kind="${spec%%=*}"
	name="${spec#*=}"
	yq -e 'select(.kind == "'"${kind}"'" and .metadata.name == "'"${name}"'")' \
		"${deploy_render}" >/dev/null 2>&1 \
		|| fail "deploy unit did not render ${kind}/${name}"
done

# --- 4. The demo overlay is the sole composition point -----------------------------------------
demo_render="${WORK_DIR}/demo.yaml"
kubectl kustomize "${ROOT_DIR}/clusters/demo" >"${demo_render}"
ap_flux="${WORK_DIR}/ap-flux.yaml"
yq eval 'select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and .metadata.name == "activitypub")' \
	"${demo_render}" >"${ap_flux}"
[ -s "${ap_flux}" ] || fail "demo overlay does not compose the activitypub Flux Kustomization"
assert_yq \
	'.spec.path == "./apps/activitypub-agent-gateway/deploy" and
	 .spec.prune == true and
	 ([.spec.dependsOn[].name] | sort | join(",")) == "agentgateway,kagent,platform-secrets" and
	 ([.spec.dependsOn[].name] | any(. == "matrix")) == false and
	 ([.spec.patches[].patch] | join(" ") | contains("podMonitor")) and
	 ([.spec.postBuild.substituteFrom[].name] | any(. == "platform-settings"))' \
	"${ap_flux}" "demo activitypub Flux Kustomization drifted (path/deps/matrix-independence/podMonitor)"

# The reconciled AP gateway is a second caller of the A2A chokepoint. Demo (and ONLY demo) must widen
# the agentgateway layer so its workload can reach kagent: the fail-closed a2a-bridge-authorization CEL
# must OR in the `activitypub-agent-gateway` workload on the SAME kagent path/method scope, and the
# load-bearing agentgateway-allow-agents ingress NetworkPolicy must admit the `activitypub` namespace
# on :8080. These live as embedded patches on the demo `agentgateway` Flux Kustomization; local/gcp
# keep the single-caller boundary (asserted absent below).
demo_agentgateway_flux="${WORK_DIR}/demo-agentgateway-flux.yaml"
yq eval 'select(.kind == "Kustomization" and .metadata.name == "agentgateway")' \
	"${demo_render}" >"${demo_agentgateway_flux}"
[ -s "${demo_agentgateway_flux}" ] || fail "demo overlay does not patch the agentgateway Flux Kustomization"
assert_yq \
	'([.spec.patches[] | select(.target.name == "a2a-bridge-authorization") | .patch] | join(" ") |
	   contains("activitypub-agent-gateway") and contains("/spec/traffic/authorization/policy/matchExpressions/0")) and
	 ([.spec.patches[] | select(.target.name == "agentgateway-allow-agents") | .patch] | join(" ") |
	   contains("/spec/ingress/0/from/-") and contains("kubernetes.io/metadata.name: activitypub"))' \
	"${demo_agentgateway_flux}" "demo agentgateway widening for the AP workload drifted (CEL/NetworkPolicy)"

# Prove the widening actually APPLIES cleanly through Flux (a malformed JSON patch or a bad path would
# fail the build), then assert the EFFECTIVE rendered CEL + NetworkPolicy — not just the patch strings.
# Reuse the CI fixture's canonical substitute set (infra/agentgateway needs vars beyond demo settings).
demo_agentgateway_flux_built="${WORK_DIR}/demo-agentgateway-flux-built.yaml"
FIXTURE_PATH="${FIXTURE}" yq eval-all -o=yaml \
	'select(.kind == "Kustomization" and .metadata.name == "agentgateway") |
	 .spec.postBuild = load(strenv(FIXTURE_PATH)).spec.postBuild' \
	"${demo_render}" >"${WORK_DIR}/demo-agentgateway-kustomization.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization agentgateway \
		--path infra/agentgateway \
		--kustomization-file "${WORK_DIR}/demo-agentgateway-kustomization.yaml" \
		--dry-run --in-memory-build --strict-substitute
) >"${demo_agentgateway_flux_built}"
assert_yq \
	'select(.kind == "AgentgatewayPolicy" and .metadata.name == "a2a-bridge-authorization") |
	 (.spec.traffic.authorization.policy.matchExpressions[0] |
	   contains("activitypub-agent-gateway") and contains("matrix-a2a-bridge") and
	   contains("/api/a2a/kagent/"))' \
	"${demo_agentgateway_flux_built}" "demo CEL does not admit the activitypub-agent-gateway workload after build"
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.name == "agentgateway-allow-agents") |
	 ((.spec.ingress[0].from | length) == 3 and
	  ([.spec.ingress[0].from[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] |
	    sort | join(",")) == "activitypub,bridge,kagent")' \
	"${demo_agentgateway_flux_built}" "demo agentgateway ingress does not admit the activitypub namespace after build"

# The demo overlay must also compose the opt-in Postgres tenant into the shared CNPG cluster, so the
# durable activity store the border requires has a scoped role + database. local/gcp must not.
demo_postgres_flux="${WORK_DIR}/demo-postgres-flux.yaml"
yq eval 'select(.kind == "Kustomization" and .metadata.name == "postgres")' \
	"${demo_render}" >"${demo_postgres_flux}"
assert_yq \
	'([.spec.components[]] | any(. == "components/activitypub"))' \
	"${demo_postgres_flux}" "demo postgres layer must compose the activitypub Postgres tenant component"

# The component itself must add exactly one scoped login role, its per-tenant TLS HBA row, and the
# retained Database — mirroring the bridge/kagent pattern (own database + scoped role). Build it inside
# the canonical Postgres layer through Flux, exactly as an overlay composes it.
pg_fixture="${WORK_DIR}/pg-flux.yaml"
printf '%s\n' \
	'apiVersion: kustomize.toolkit.fluxcd.io/v1' \
	'kind: Kustomization' \
	'metadata: {name: postgres-activitypub-test, namespace: flux-system}' \
	'spec:' \
	'  interval: 30m' \
	'  path: ./infra/postgres' \
	'  components:' \
	'    - components/activitypub' \
	'  sourceRef: {kind: GitRepository, name: flux-system}' >"${pg_fixture}"
pg_render="${WORK_DIR}/pg.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization postgres-activitypub-test \
		--path infra/postgres \
		--kustomization-file "${pg_fixture}" \
		--dry-run --in-memory-build
) >"${pg_render}"
assert_yq \
	'select(.kind == "Cluster" and .metadata.name == "platform-pg") |
	 ([.spec.managed.roles[] | select(.name == "activitypub" and .login == true and .passwordSecret.name == "pg-activitypub")] | length) == 1 and
	 ([.spec.postgresql.pg_hba[] | select(. == "hostssl activitypub activitypub all scram-sha-256")] | length) == 1' \
	"${pg_render}" "activitypub Postgres role/HBA contract drifted"
assert_yq \
	'select(.kind == "Database" and .metadata.name == "activitypub") |
	 (.spec.cluster.name == "platform-pg" and .spec.owner == "activitypub" and .spec.databaseReclaimPolicy == "retain")' \
	"${pg_render}" "activitypub CNPG Database contract drifted"

# local, gcp, and base must keep the gateway absent (renders unchanged by this issue).
for entrypoint in base local gcp; do
	entry_render="${WORK_DIR}/${entrypoint}.yaml"
	if [ "${entrypoint}" = base ]; then
		kubectl kustomize "${ROOT_DIR}/clusters/base" >"${entry_render}"
	else
		(
			cd "${ROOT_DIR}"
			flux build kustomization cluster-overlay-validation \
				--path "clusters/${entrypoint}" \
				--kustomization-file "${FIXTURE}" \
				--dry-run --in-memory-build --strict-substitute
		) >"${entry_render}"
	fi
	if yq -e 'select(.kind == "Kustomization" and .metadata.name == "activitypub")' \
		"${entry_render}" >/dev/null 2>&1; then
		fail "clusters/${entrypoint} must NOT reconcile the activitypub gateway (demo-only)"
	fi
	if grep -qi 'activitypub' "${entry_render}"; then
		fail "clusters/${entrypoint} render references activitypub (must stay absent)"
	fi
done

# --- 5. The demo secret path is cluster-only, not the SOPS/production path -----------------------
grep -q 'create_activitypub_secrets' "${ROOT_DIR}/scripts/lib/demo-secrets.sh" \
	|| fail "demo secret path does not provision the ActivityPub gateway keys"
for secret in activitypub-agent-gateway-identity-key activitypub-agent-gateway-signing-key \
	activitypub-agent-gateway-credential activitypub-agent-gateway-db pg-activitypub; do
	grep -q "${secret}" "${ROOT_DIR}/scripts/lib/demo-secrets.sh" \
		|| fail "demo secret path is missing ${secret}"
done
demo_secrets="${ROOT_DIR}/scripts/lib/demo-secrets.sh"
# The deploy enables groups + status feed, so the signing-key Secret must carry BOTH the Ed25519
# object-proof key and the RSA hop-signature key (rsa.pem) or the pod fails to start (#476). The SOPS
# example template documents the same two-key shape for a production enablement.
grep -qF 'rsa.pem=' "${demo_secrets}" \
	|| fail "demo signing-key Secret must provide rsa.pem (groups/status-feed HTTP signatures)"
grep -qF 'rsa.pem' "${ROOT_DIR}/infra/secrets/activitypub-agent-gateway-signing-key.sops.yaml.example" \
	|| fail "signing-key SOPS example must document rsa.pem for a production enablement"
# The AP gateway must be registered as an agentgateway A2A caller (its own workload identity) so the
# demo-scoped CEL admits it; the exact workload string must match the CEL widening above.
grep -qF "activitypub-agent-gateway=\"\${ap_caller}\"" "${demo_secrets}" \
	|| fail "demo secret path must register the activitypub-agent-gateway A2A caller"
grep -qF "a2a_caller_document \"\${ap_token}\" activitypub-agent-gateway" "${demo_secrets}" \
	|| fail "AP caller must bind the activitypub-agent-gateway workload identity"
# The namespace-local DATABASE_URL must reuse the CNPG role password (same-password contract). Prove
# it statically: the URL password field is the ${ap_db_password} variable, and that variable is
# assigned from bootstrap_secret_value pg-activitypub — the exact source the CNPG pg-activitypub role
# Secret uses (PG_ACTIVITYPUB), so both credentials are provably equal offline without a cluster.
demo_secrets="${ROOT_DIR}/scripts/lib/demo-secrets.sh"
grep -qF "ap_db_password=\"\$(bootstrap_secret_value pg-activitypub)\"" "${demo_secrets}" \
	|| fail "demo DB password must derive from bootstrap_secret_value pg-activitypub"
grep -qF "url=postgres://activitypub:\${ap_db_password}@platform-pg-rw.postgres" "${demo_secrets}" \
	|| fail "demo DATABASE_URL must use the pg-activitypub-derived password"
# The SOPS templates remain examples only; no real encrypted AP key may enter a cluster secret dir.
for template in identity signing; do
	[ -f "${ROOT_DIR}/infra/secrets/activitypub-agent-gateway-${template}-key.sops.yaml.example" ] \
		|| fail "missing infra/secrets AP ${template}-key SOPS template"
done
if compgen -G "${ROOT_DIR}/clusters/*/secrets/activitypub-agent-gateway-*" >/dev/null 2>&1; then
	fail "an ActivityPub key leaked into a production per-cluster secret dir (demo path is cluster-only)"
fi

# --- 6. Pinned-key resolver + the interop-acceptance posture (issue #489 Task 7, ADR 0021) --------
# The pinned-key resolver lets an in-cluster peer that the #320 SSRF guard cannot fetch be verified
# from an out-of-band pin, WITHOUT weakening the guard (unpinned actors stay on the guarded HTTP
# resolver). The border, allowlist, budget, dedup, and FEP-8b32 OUTBOUND signing stay ON; only the
# inbound object-proof gate relaxes for the Mastodon/GtS-wire peer. All demo-scoped.
grep -q 'PINNED_KEYS_PATH' "${ROOT_DIR}/apps/activitypub-agent-gateway/internal/config/config.go" \
	|| fail "the gateway config is missing the PINNED_KEYS_PATH knob"

# The chart mounts the pinned-keys Secret and sets PINNED_KEYS_PATH only when pinnedKeys.enabled.
pinned_render="${WORK_DIR}/chart-pinned.yaml"
helm template activitypub-agent-gateway "${CHART_DIR}" --namespace activitypub \
	--values "${chart_values}" --set pinnedKeys.enabled=true >"${pinned_render}"
assert_yq \
	'select(.kind == "Deployment") |
	 ([.spec.template.spec.containers[0].env[] | select(.name == "PINNED_KEYS_PATH")] | length) == 1 and
	 ([.spec.template.spec.volumes[] | select(.name == "pinned-keys")] | length) == 1' \
	"${pinned_render}" "chart does not mount the pinned-keys Secret when pinnedKeys.enabled"
# With pinnedKeys DISABLED (the deploy default) no pin is mounted — pinning is strictly opt-in.
if yq -e 'select(.kind == "Deployment") | .spec.template.spec.containers[0].env[] |
	select(.name == "PINNED_KEYS_PATH")' "${chart_render}" >/dev/null 2>&1; then
	fail "the deploy default must NOT mount pinned keys (pinning is opt-in, demo-only)"
fi

# The demo overlay's activitypub Flux Kustomization must enable the pins, relax requireInbound, and
# open the gateway inbox to the interop-peer namespace — and nothing else may.
assert_yq \
	'([.spec.patches[] | select(.target.kind == "HelmRelease") | .patch] | join(" ") |
	   contains("pinnedKeys") and contains("activitypub-agent-gateway-pinned-keys") and
	   contains("/spec/values/integrity/requireInbound")) and
	 ([.spec.patches[] | select(.target.kind == "NetworkPolicy") | .patch] | join(" ") |
	   contains("/spec/ingress/-") and contains("kubernetes.io/metadata.name: activitypub-interop"))' \
	"${ap_flux}" "demo activitypub overlay lost the pinned-key / requireInbound / peer-ingress posture"

# Prove those demo patches actually APPLY to the deploy unit (a bad JSON-patch path fails the build),
# then assert the EFFECTIVE HelmRelease values + NetworkPolicy — not just that the patch strings exist.
demo_deploy_flux="${WORK_DIR}/demo-activitypub-deploy-flux.yaml"
FIXTURE_PATH="${FIXTURE}" yq eval-all -o=yaml \
	'select(.apiVersion == "kustomize.toolkit.fluxcd.io/v1" and .kind == "Kustomization" and .metadata.name == "activitypub") |
	 .spec.postBuild = load(strenv(FIXTURE_PATH)).spec.postBuild' \
	"${demo_render}" >"${demo_deploy_flux}"
demo_deploy_built="${WORK_DIR}/demo-activitypub-deploy.yaml"
(
	cd "${ROOT_DIR}"
	flux build kustomization activitypub \
		--path apps/activitypub-agent-gateway/deploy \
		--kustomization-file "${demo_deploy_flux}" \
		--dry-run --in-memory-build --strict-substitute
) >"${demo_deploy_built}"
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "activitypub-agent-gateway") |
	 (.spec.values.pinnedKeys.enabled == true and
	  .spec.values.pinnedKeys.secretName == "activitypub-agent-gateway-pinned-keys" and
	  .spec.values.integrity.requireInbound == false and
	  .spec.values.policy.enabled == true and .spec.values.budget.enabled == true and
	  .spec.values.integrity.enabled == true and .spec.values.httpRoute.enabled == false and
	  .spec.values.image.repository == "activitypub-agent-gateway" and
	  .spec.values.image.pullPolicy == "Never" and
	  (.spec.values.image.repository | contains("ghcr.io") | not))' \
	"${demo_deploy_built}" "demo HelmRelease posture drifted (pins on, requireInbound off, border/budget on, route off, local image)"
# The GHCR-unpublished AP image is built + k3d-imported locally for demo; the demo HelmRelease must
# reference the locally imported tag (the CI-substituted demo_bridge_tag), never GHCR.
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "activitypub-agent-gateway") |
	 .spec.values.image.tag == "local"' \
	"${demo_deploy_built}" "demo AP image tag must be the local demo_bridge_tag, not a GHCR digest"
# The deploy unit (the reference for local/gcp, which do NOT override it) must still name the GHCR
# repository, so only demo diverges to a local image.
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "activitypub-agent-gateway") |
	 .spec.values.image.repository == "ghcr.io/fmind-ai/activitypub-agent-gateway"' \
	"${DEPLOY_DIR}/helmrelease.yaml" "deploy unit must keep the GHCR image repository for local/gcp"
# The AP gateway is the last demo workload, so its first cold rollout can exceed helm's default 5m
# wait. The DEMO release must extend the timeout + add remediation retries so the slow reconcile
# completes; the deploy unit must NOT carry them, so local/gcp keep the Flux defaults.
assert_yq \
	'select(.kind == "HelmRelease" and .metadata.name == "activitypub-agent-gateway") |
	 (.spec.timeout == "10m" and
	  .spec.install.remediation.retries >= 1 and .spec.install.remediation.remediateLastFailure == true and
	  .spec.upgrade.remediation.retries >= 1 and .spec.upgrade.remediation.remediateLastFailure == true)' \
	"${demo_deploy_built}" "demo HelmRelease must extend the helm wait + remediation for the cold rollout"
if yq -e 'select(.kind == "HelmRelease" and .metadata.name == "activitypub-agent-gateway") |
	(has("spec")) and (.spec | has("timeout") or has("install") or has("upgrade"))' \
	"${DEPLOY_DIR}/helmrelease.yaml" >/dev/null 2>&1; then
	fail "deploy unit must not set timeout/install/upgrade (local/gcp keep Flux defaults; demo overrides)"
fi
# The demo build + side-load lifecycle must mirror the bridge: build the AP image and k3d-import it.
grep -q 'AP_GATEWAY_IMAGE' "${ROOT_DIR}/scripts/demo.sh" \
	|| fail "demo lifecycle does not define the AP gateway image tag"
grep -qF "build_image \"\${AP_GATEWAY_IMAGE}\"" "${ROOT_DIR}/scripts/lib/demo-cluster.sh" \
	|| fail "demo lifecycle does not build the AP gateway image like the bridge"
grep -qF "k3d image import --mode auto --cluster \"\${CLUSTER_NAME}\" \"\${AP_GATEWAY_IMAGE}\"" \
	"${ROOT_DIR}/scripts/lib/demo-cluster.sh" \
	|| fail "demo lifecycle does not side-load the AP gateway image into the cluster"
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.name == "activitypub-agent-gateway") |
	 ([.spec.ingress[].from[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] | sort | join(",")) == "activitypub-interop,gateway,monitoring"' \
	"${demo_deploy_built}" "demo gateway ingress must add ONLY the interop-peer namespace (gateway + monitoring retained)"

# The demo secret path pins the peer PUBLIC keys and keeps the private keys cluster-only.
grep -q 'create_activitypub_interop_pins' "${demo_secrets}" \
	|| fail "demo secret path does not pin the interop peer keys"
for token in ap-peer-allowed-key ap-peer-denied-key activitypub-agent-gateway-pinned-keys; do
	grep -q "${token}" "${ROOT_DIR}/scripts/lib/activitypub-interop.sh" \
		|| fail "interop constants missing ${token}"
done

# The scripted peer manifest renders, is digest-pinned (never :latest), default-deny, and — being in
# no Flux entrypoint — is applied ONLY by the acceptance harness (local/gcp never see it).
peer_render="${WORK_DIR}/interop-peer.yaml"
kubectl kustomize "${ROOT_DIR}/clusters/demo/interop-peer" >"${peer_render}"
kubeconform -strict -ignore-missing-schemas -summary "${peer_render}" >/dev/null
assert_yq \
	'select(.kind == "Deployment") |
	 (.spec.template.spec.containers[0].image | test("@sha256:[0-9a-f]{64}$")) and
	 (.spec.template.spec.containers[0].image | test(":latest$") | not) and
	 (.spec.template.spec.securityContext.runAsNonRoot == true)' \
	"${peer_render}" "interop peer image must be digest-pinned non-root (never :latest)"
assert_yq \
	'select(.kind == "NetworkPolicy" and .metadata.name == "activitypub-interop-peer") |
	 ((.spec.policyTypes | sort | join(",")) == "Egress,Ingress" and
	  ((.spec | has("ingress")) | not) and
	  ([.spec.egress[].to[].namespaceSelector.matchLabels."kubernetes.io/metadata.name"] | sort | join(",")) == "activitypub,kube-system")' \
	"${peer_render}" "interop peer NetworkPolicy must be default-deny, egress only to DNS + the gateway"
for entrypoint in base local gcp; do
	if grep -qi 'activitypub-interop' "${WORK_DIR}/${entrypoint}.yaml"; then
		fail "clusters/${entrypoint} must not reference the interop peer (acceptance-only, demo profile)"
	fi
done

echo "activitypub gateway: deploy unit, default-deny NetworkPolicy, gated route, demo-only composition, pinned-key resolver + interop peer, and cluster-only secret path validated offline."
