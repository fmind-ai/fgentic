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

echo "activitypub gateway: deploy unit, default-deny NetworkPolicy, gated route, demo-only composition, and cluster-only secret path validated offline."
