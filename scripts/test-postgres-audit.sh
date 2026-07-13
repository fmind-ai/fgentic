#!/usr/bin/env bash
# Validate the privacy-safe pgAudit manifest and projection. --runtime creates its own disposable
# kind cluster, installs the repository-pinned CNPG chart, and never uses the active kube context.
set -euo pipefail

readonly ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly CLUSTER_MANIFEST="${ROOT_DIR}/infra/postgres/cluster.yaml"
readonly FILTER="${ROOT_DIR}/scripts/lib/postgres-audit.jq"
readonly FIXTURE="${ROOT_DIR}/scripts/testdata/postgres-audit.jsonl"
readonly KIND_CONFIG="${ROOT_DIR}/scripts/testdata/postgres-audit-kind.yaml"
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
RUNTIME_CLUSTER_NAME=""
RUNTIME_CLUSTER_OWNED=false
RUNTIME_WORKDIR=""
runtime=false

case "${1:-}" in
	"") ;;
	--runtime) runtime=true ;;
	*)
		echo "usage: ${0##*/} [--runtime]" >&2
		exit 2
		;;
esac

fail() {
	echo "error: $*" >&2
	exit 1
}

require_commands() {
	local command
	for command in "$@"; do
		command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
	done
}

static_contract() {
	require_commands jq yq

	yq -e '
    .spec.postgresql.parameters."pgaudit.log" == "ddl, role" and
    .spec.postgresql.parameters."pgaudit.log_catalog" == "off" and
    .spec.postgresql.parameters."pgaudit.log_parameter" == "off" and
    .spec.postgresql.parameters."pgaudit.log_statement" == "off" and
    ([.spec.postgresql.parameters | keys[] | select(test("^pgaudit\\."))] | sort | join(",")) ==
      "pgaudit.log,pgaudit.log_catalog,pgaudit.log_parameter,pgaudit.log_statement"
  ' "${CLUSTER_MANIFEST}" >/dev/null ||
		fail "Postgres must audit only DDL/ROLE and suppress catalog noise, SQL text, and parameters"
	yq -e '
    [.spec.postgresql.parameters | keys[] | select(test("^pg_stat_statements\\."))]
    | length == 0
  ' "${CLUSTER_MANIFEST}" >/dev/null ||
		fail "pg_stat_statements is deliberately outside the pgAudit change"
	yq -e '
    .kind == "Cluster" and
    .nodes[0].role == "control-plane" and
    (.nodes[0].kubeadmConfigPatches[0] | contains("KubeletInUserNamespace: true"))
  ' "${KIND_CONFIG}" >/dev/null || fail "kind fixture is not safe for constrained/rootless hosts"

	local projected
	projected="$(jq --compact-output --from-file "${FILTER}" "${FIXTURE}")"
	jq -e -s '
    length == 2 and
    ([.[].audit.class] | sort == ["DDL", "ROLE"]) and
    (.[0].audit.command == "CREATE TABLE") and
    (.[0].audit.object_name == "public.audit_fixture_table") and
    (.[1].audit.command == "ALTER ROLE") and
    (.[1].audit.object_name == "") and
    all(.[]; (has("statement") | not) and (has("parameter") | not)) and
    all(.[].audit; (has("statement") | not) and (has("parameter") | not))
  ' <<<"${projected}" >/dev/null || fail "minimal pgAudit projection contract drifted"
	[[ "${projected}" != *PGAUDIT_WRITE_SENTINEL* ]] ||
		fail "WRITE-class fixture leaked through the DDL/ROLE projection"

	echo "Postgres audit static contract passed"
}

runtime_contract() {
	require_commands docker helm jq kind kubectl yq
	docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

	local chart chart_version repository source
	chart="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "cloudnative-pg") | .spec.chart.spec.chart' \
		"${ROOT_DIR}/infra/flux/releases.yaml")"
	chart_version="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "cloudnative-pg") | .spec.chart.spec.version' \
		"${ROOT_DIR}/infra/flux/releases.yaml")"
	source="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "cloudnative-pg") | .spec.chart.spec.sourceRef.name' \
		"${ROOT_DIR}/infra/flux/releases.yaml")"
	repository="$(SOURCE="${source}" yq -er '
    select(.kind == "HelmRepository" and .metadata.name == strenv(SOURCE)) | .spec.url
  ' "${ROOT_DIR}/infra/flux/sources.yaml")"

	local namespace
	RUNTIME_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-postgres-audit-${RANDOM}-$$}"
	namespace="fgentic-postgres-audit"
	RUNTIME_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-postgres-audit.XXXXXX")"
	KUBECONFIG="${RUNTIME_WORKDIR}/kubeconfig"
	export KUBECONFIG

	cleanup() {
		local result=$?
		trap - EXIT INT TERM
		if [[ "${KEEP_KIND_CLUSTER:-0}" == "1" && "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
			echo "==> Keeping kind cluster ${RUNTIME_CLUSTER_NAME}; use KUBECONFIG=${KUBECONFIG}"
		else
			if [[ "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
				kind delete cluster --name "${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1 || true
			fi
			rm -rf "${RUNTIME_WORKDIR}"
		fi
		exit "${result}"
	}
	trap cleanup EXIT
	trap 'exit 130' INT TERM

	if kind get clusters | grep -Fxq "${RUNTIME_CLUSTER_NAME}"; then
		fail "kind cluster already exists; refusing to mutate it: ${RUNTIME_CLUSTER_NAME}"
	fi

	echo "==> Creating isolated kind cluster ${RUNTIME_CLUSTER_NAME}"
	kind create cluster --name "${RUNTIME_CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" \
		--config "${KIND_CONFIG}" --kubeconfig "${KUBECONFIG}"
	RUNTIME_CLUSTER_OWNED=true

	echo "==> Installing repository-pinned CloudNativePG chart ${chart_version}"
	helm upgrade --install cloudnative-pg "${chart}" \
		--repo "${repository}" \
		--version "${chart_version}" \
		--namespace cnpg-system \
		--create-namespace \
		--wait \
		--timeout 8m >/dev/null

	kubectl create namespace "${namespace}" >/dev/null
	NAMESPACE="${namespace}" yq '
    .metadata.name = "audit-pg" |
    .metadata.namespace = strenv(NAMESPACE) |
    .spec.storage.size = "1Gi" |
    .spec.monitoring.enablePodMonitor = false |
    .spec.postgresql.pg_hba = [
      "hostssl audit_tenant audit_fixture_role all scram-sha-256",
      "hostssl all all all reject",
      "hostnossl all all all reject"
    ] |
    del(.spec.backup, .spec.serviceAccountTemplate, .spec.managed)
  ' "${CLUSTER_MANIFEST}" >"${RUNTIME_WORKDIR}/cluster.yaml"

	echo "==> Reconciling disposable CNPG cluster with the production pgAudit parameters"
	kubectl apply --filename "${RUNTIME_WORKDIR}/cluster.yaml" >/dev/null
	kubectl --namespace "${namespace}" wait cluster/audit-pg \
		--for=condition=Ready --timeout=8m >/dev/null

	local primary
	primary="$(kubectl --namespace "${namespace}" get cluster audit-pg \
		--output=jsonpath='{.status.currentPrimary}')"
	[[ -n "${primary}" ]] || fail "CNPG did not report a primary instance"

	local pgaudit_extension pg_stat_statements
	pgaudit_extension="$(kubectl --namespace "${namespace}" exec "pod/${primary}" \
		--container postgres -- psql --tuples-only --no-align --username postgres --dbname postgres \
		--command="SELECT EXISTS (SELECT FROM pg_extension WHERE extname = 'pgaudit')")"
	[[ "${pgaudit_extension}" == "t" ]] || fail "CNPG did not manage the pgAudit extension"
	pg_stat_statements="$(kubectl --namespace "${namespace}" exec "pod/${primary}" \
		--container postgres -- psql --tuples-only --no-align --username postgres --dbname postgres \
		--command="SELECT EXISTS (SELECT FROM pg_extension WHERE extname = 'pg_stat_statements')")"
	[[ "${pg_stat_statements}" == "f" ]] || fail "pg_stat_statements was enabled outside issue scope"

	echo "==> Creating a scoped tenant database and role"
	kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
		env PGAPPNAME=fgentic-pgaudit-admin-fixture \
		psql --set=ON_ERROR_STOP=1 --username postgres --dbname postgres >/dev/null <<'SQL'
CREATE ROLE audit_fixture_role LOGIN PASSWORD 'PGAUDIT_ROLE_PASSWORD_SENTINEL';
ALTER ROLE audit_fixture_role SET statement_timeout = '1s';
CREATE DATABASE audit_tenant OWNER audit_fixture_role;
SQL
	local tenant_pgaudit_extension
	tenant_pgaudit_extension="$(kubectl --namespace "${namespace}" exec "pod/${primary}" \
		--container postgres -- psql --tuples-only --no-align --username postgres --dbname audit_tenant \
		--command="SELECT EXISTS (SELECT FROM pg_extension WHERE extname = 'pgaudit')")"
	[[ "${tenant_pgaudit_extension}" == "t" ]] ||
		fail "CNPG did not make pgAudit available in a newly created tenant database"

	echo "==> Exercising DDL, WRITE, and READ as the tenant role"
	kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
		env PGAPPNAME=fgentic-pgaudit-tenant-fixture \
		PGPASSWORD=PGAUDIT_ROLE_PASSWORD_SENTINEL \
		psql --set=ON_ERROR_STOP=1 \
		--dbname='host=127.0.0.1 dbname=audit_tenant user=audit_fixture_role sslmode=require' \
		>/dev/null <<'SQL'
CREATE TABLE audit_fixture_table (id integer, value text);
COMMENT ON TABLE audit_fixture_table IS 'PGAUDIT_STATEMENT_SENTINEL';
INSERT INTO audit_fixture_table VALUES (42, 'PGAUDIT_WRITE_SENTINEL');
SELECT * FROM audit_fixture_table;
DROP TABLE audit_fixture_table;
SQL
	kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
		env PGAPPNAME=fgentic-pgaudit-admin-cleanup \
		psql --set=ON_ERROR_STOP=1 --username postgres --dbname postgres >/dev/null <<'SQL'
DROP DATABASE audit_tenant;
DROP ROLE audit_fixture_role;
SQL

	runtime_records_ready() {
		jq -e '
      [.[] | select(.logger == "pgaudit" and .msg == "record")] as $records |
      [$records[] | select(
        .record.application_name == "fgentic-pgaudit-admin-fixture" and
        .record.audit.class == "ROLE"
      )] as $admin |
      [$records[] | select(
        .record.application_name == "fgentic-pgaudit-tenant-fixture" and
        .record.user_name == "audit_fixture_role" and
        .record.database_name == "audit_tenant" and
        .record.audit.class == "DDL"
      )] as $tenant |
      (["CREATE ROLE", "ALTER ROLE"] - [$admin[].record.audit.command] | length == 0) and
      ([$admin[].record.session_id] | unique | length == 1) and
      (["CREATE TABLE", "COMMENT", "DROP TABLE"] - [$tenant[].record.audit.command] |
        length == 0) and
      ([$tenant[].record.session_id] | unique | length == 1) and
      any($records[];
        .record.application_name == "fgentic-pgaudit-admin-cleanup" and
        .record.audit.class == "ROLE" and
        .record.audit.command == "DROP ROLE")
    ' --slurp "$1" >/dev/null
	}

	local logs_ready=false
	for _ in {1..30}; do
		kubectl --namespace "${namespace}" logs "pod/${primary}" --container postgres \
			>"${RUNTIME_WORKDIR}/postgres.jsonl"
		if runtime_records_ready "${RUNTIME_WORKDIR}/postgres.jsonl"; then
			logs_ready=true
			break
		fi
		sleep 1
	done
	[[ "${logs_ready}" == true ]] || fail "complete correlated pgAudit fixture did not reach CNPG stdout"

	runtime_records_ready "${RUNTIME_WORKDIR}/postgres.jsonl" ||
		fail "runtime pgAudit records did not preserve fixture session correlation"
	jq -e '
    [.[] | select(.logger == "pgaudit" and .msg == "record") | .record.audit] as $audit |
    ($audit | length > 0) and
    all($audit[]; .class == "DDL" or .class == "ROLE") and
    all($audit[]; .statement == "<not logged>" and .parameter == "<not logged>") and
    any(.[]; .logger == "pgaudit" and .msg == "record" and
      .record.application_name == "fgentic-pgaudit-tenant-fixture" and
      .record.user_name == "audit_fixture_role" and
      .record.database_name == "audit_tenant" and
      .record.audit.class == "DDL" and .record.audit.command == "CREATE TABLE" and
      .record.audit.object_name == "public.audit_fixture_table") and
    any($audit[]; .class == "DDL" and .command == "COMMENT") and
    any($audit[]; .class == "DDL" and .command == "DROP TABLE" and
      .object_name == "public.audit_fixture_table")
  ' --slurp "${RUNTIME_WORKDIR}/postgres.jsonl" >/dev/null ||
		fail "runtime pgAudit records did not preserve the DDL/ROLE-only redacted contract"
	if grep -Eq 'PGAUDIT_(ROLE_PASSWORD|STATEMENT|WRITE)_SENTINEL' \
		"${RUNTIME_WORKDIR}/postgres.jsonl"; then
		fail "CNPG stdout leaked SQL statement or row content"
	fi

	jq --compact-output --from-file "${FILTER}" "${RUNTIME_WORKDIR}/postgres.jsonl" \
		>"${RUNTIME_WORKDIR}/audit.jsonl"
	jq -e -s '
    length > 0 and
    all(.[]; (.audit.class == "DDL" or .audit.class == "ROLE")) and
    all(.[]; (has("statement") | not) and (has("parameter") | not)) and
    all(.[].audit; (has("statement") | not) and (has("parameter") | not)) and
    any(.[]; .database_role == "audit_fixture_role" and .database == "audit_tenant" and
      .audit.command == "CREATE TABLE" and
      .audit.object_name == "public.audit_fixture_table")
  ' "${RUNTIME_WORKDIR}/audit.jsonl" >/dev/null || fail "runtime minimal projection failed"

	echo "Postgres audit runtime contract passed (${chart} ${chart_version}, ${primary})"
}

static_contract
if ${runtime}; then
	runtime_contract
fi
