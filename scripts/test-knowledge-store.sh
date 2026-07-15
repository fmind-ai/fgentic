#!/usr/bin/env bash
# Validate the CNPG pgvector knowledge-store contract. --runtime creates its own disposable kind
# cluster, installs the repository-pinned CNPG chart, and never reads or mutates the active context.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly CLUSTER_MANIFEST="${ROOT_DIR}/infra/postgres/cluster.yaml"
readonly DATABASES_MANIFEST="${ROOT_DIR}/infra/postgres/databases.yaml"
readonly KUSTOMIZATION="${ROOT_DIR}/infra/postgres/kustomization.yaml"
readonly KIND_CONFIG="${ROOT_DIR}/scripts/testdata/postgres-audit-kind.yaml"
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
readonly POSTGRES_IMAGE="ghcr.io/cloudnative-pg/postgresql:17.10-202607130907-system-trixie@sha256:c141aec61cab8da3e215aebe0fa155e78442fbb41c982a86743915a967e12af9"
readonly OWNER_PASSWORD="KNOWLEDGE_OWNER_PASSWORD_SENTINEL"
readonly RETRIEVAL_PASSWORD="KNOWLEDGE_RETRIEVAL_PASSWORD_SENTINEL"
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
  require_commands jq kubectl rg yq

  local render render_objects roles database job policy schema_sql secret_template
  render="$(kubectl kustomize "${ROOT_DIR}/infra/postgres")"
  render_objects="$(yq eval-all -o=json '.' <<<"${render}" | jq --slurp '.')"
  roles="$(
    yq eval-all -o=json '
      select(.kind == "DatabaseRole" and
        (.metadata.name == "knowledge-owner" or .metadata.name == "knowledge-retrieval"))
    ' "${DATABASES_MANIFEST}" | jq --slurp '.'
  )"
  database="$(
    yq eval-all -o=json 'select(.kind == "Database" and .metadata.name == "knowledge")' \
      "${DATABASES_MANIFEST}"
  )"
  job="$(
    yq eval-all -o=json 'select(.kind == "Job" and .metadata.name == "knowledge-schema-v1")' \
      <<<"${render}"
  )"
  policy="$(
    yq eval-all -o=json '
      select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-schema-v1")
    ' <<<"${render}"
  )"
  schema_sql="$(
    yq eval-all -r 'select(.kind == "ConfigMap" and .metadata.name == "knowledge-schema-v1") |
      .data."schema.sql"' <<<"${render}"
  )"
  secret_template="$(
    yq eval-all -o=json 'select(.kind == "Secret")' \
      "${ROOT_DIR}/infra/secrets/knowledge-db.sops.yaml.example" | jq --slurp '.'
  )"

  jq -e '
    ([.[] | select(.kind == "Cluster") | .metadata.name] == ["platform-pg"]) and
    ([.[] | select(.kind == "Deployment" or .kind == "StatefulSet")] | length == 0) and
    ([.[] | select(.kind == "Job") | .metadata.name] == ["knowledge-schema-v1"]) and
    ([.[] | select(.kind == "ConfigMap" and .metadata.name == "knowledge-schema-v1") |
      .immutable] == [true])
  ' <<<"${render_objects}" >/dev/null ||
    fail "knowledge store must reuse platform-pg and add only its immutable schema artifact"

  EXPECTED_POSTGRES_IMAGE="${POSTGRES_IMAGE}" yq -e '
    .spec.imageName == strenv(EXPECTED_POSTGRES_IMAGE) and
    .spec.resources.requests.cpu == "100m" and
    .spec.resources.requests.memory == "256Mi" and
    .spec.resources.limits.cpu == "1" and
    .spec.resources.limits.memory == "512Mi" and
    (.spec.postgresql.pg_hba | join("|")) ==
      "hostssl synapse synapse all scram-sha-256|hostssl mas mas all scram-sha-256|hostssl bridge bridge all scram-sha-256|hostssl kagent kagent all scram-sha-256|hostssl keycloak keycloak all scram-sha-256|hostssl knowledge knowledge_owner all scram-sha-256|hostssl knowledge knowledge_retrieval all scram-sha-256|hostssl all all all reject|hostnossl all all all reject"
  ' "${CLUSTER_MANIFEST}" >/dev/null ||
    fail "Postgres image, laptop resources, or exact tenant HBA contract drifted"

  jq -e '
    length == 2 and
    ([.[].spec | keys | sort] | unique) == [[
      "bypassrls", "cluster", "connectionLimit", "createdb", "createrole",
      "databaseRoleReclaimPolicy", "ensure", "inherit", "login", "name",
      "passwordSecret", "replication", "superuser"
    ]] and
    all(.[].spec;
      .cluster.name == "platform-pg" and .ensure == "present" and .login == true and
      .inherit == false and .superuser == false and .createdb == false and
      .createrole == false and .replication == false and .bypassrls == false and
      .databaseRoleReclaimPolicy == "retain") and
    any(.[].spec;
      .name == "knowledge_owner" and .connectionLimit == 4 and
      .passwordSecret.name == "pg-knowledge-owner") and
    any(.[].spec;
      .name == "knowledge_retrieval" and .connectionLimit == 16 and
      .passwordSecret.name == "pg-knowledge-retrieval")
  ' <<<"${roles}" >/dev/null || fail "scoped first-class DatabaseRole contract drifted"

  jq -e '
    .metadata.namespace == "postgres" and
    .spec.cluster.name == "platform-pg" and
    .spec.name == "knowledge" and
    .spec.owner == "knowledge_owner" and
    .spec.encoding == "UTF8" and
    .spec.databaseReclaimPolicy == "retain" and
    .spec.extensions == [{"name": "vector", "ensure": "present", "version": "0.8.5"}]
  ' <<<"${database}" >/dev/null || fail "knowledge Database/vector extension contract drifted"

  yq -e '
    (.resources | contains(["knowledge-schema-v1.yaml"])) and
    (has("replacements") | not)
  ' "${KUSTOMIZATION}" >/dev/null ||
    fail "immutable v1 migration image must not follow future Cluster image replacements"

  jq -e --arg image "${POSTGRES_IMAGE}" '
    .spec.backoffLimit == 2 and (has("activeDeadlineSeconds") | not) and
    (has("ttlSecondsAfterFinished") | not) and
    .spec.template.spec.automountServiceAccountToken == false and
    .spec.template.spec.restartPolicy == "Never" and
    .spec.template.spec.securityContext == {
      "runAsNonRoot": true,
      "seccompProfile": {"type": "RuntimeDefault"}
    } and
    (.spec.template.spec.containers | length) == 1 and
    (.spec.template.spec.containers[0] |
      .name == "schema" and .image == $image and .imagePullPolicy == "IfNotPresent" and
      .securityContext == {
        "allowPrivilegeEscalation": false,
        "readOnlyRootFilesystem": true,
        "capabilities": {"drop": ["ALL"]}
      } and
      .resources.requests == {"cpu": "10m", "memory": "32Mi", "ephemeral-storage": "8Mi"} and
      .resources.limits == {"cpu": "100m", "memory": "64Mi", "ephemeral-storage": "32Mi"} and
      ([.env[] | select(.valueFrom.secretKeyRef != null) | .valueFrom.secretKeyRef.name] |
        unique) == ["pg-knowledge-owner"] and
      ([.env[] | select(.name == "PGSSLMODE") | .value] == ["require"]) and
      (.args | length) == 1 and
      (.args[0] | contains("while true; do")) and
      (.args[0] | contains("exec psql --no-psqlrc --set=ON_ERROR_STOP=1")) and
      (.args[0] | contains("/60") | not) and
      (.args[0] | contains("bounded deadline") | not) and
      (.volumeMounts | map(select(.name == "schema" and .readOnly == true)) | length) == 1) and
    (.spec.template.spec.volumes |
      map(select(.name == "schema" and .configMap.name == "knowledge-schema-v1")) | length) == 1
  ' <<<"${job}" >/dev/null || fail "bounded restricted one-shot schema Job contract drifted"

  jq -e '
    .spec.podSelector.matchLabels == {
      "app.kubernetes.io/name": "knowledge-schema",
      "app.kubernetes.io/instance": "v1"
    } and
    .spec.policyTypes == ["Egress"] and (.spec.egress | length) == 2 and
    any(.spec.egress[];
      .to == [{"namespaceSelector": {"matchLabels": {
        "kubernetes.io/metadata.name": "kube-system"
      }}}] and
      ([.ports[] | [.protocol, .port]] | sort) == [["TCP", 53], ["UDP", 53]]) and
    any(.spec.egress[];
      .to == [{"podSelector": {"matchLabels": {"cnpg.io/cluster": "platform-pg"}}}] and
      .ports == [{"protocol": "TCP", "port": 5432}])
  ' <<<"${policy}" >/dev/null || fail "schema Job egress is broader than DNS plus platform-pg"

  jq -e '
    length == 3 and
    all(.[];
      .type == "kubernetes.io/basic-auth" and
      (.stringData | keys | sort) == ["password", "username"]) and
    any(.[];
      .metadata.name == "pg-knowledge-owner" and .metadata.namespace == "postgres" and
      .stringData.username == "knowledge_owner" and
      .stringData.password == "REPLACE_WITH_PG_KNOWLEDGE_OWNER_PASSWORD") and
    ([.[] | select(.metadata.name == "pg-knowledge-owner")] | length == 1) and
    ([.[] | select(.metadata.name == "pg-knowledge-retrieval") | .metadata.namespace] | sort) ==
      ["knowledge", "postgres"] and
    all(.[] | select(.metadata.name == "pg-knowledge-retrieval");
      .stringData.username == "knowledge_retrieval" and
      .stringData.password == "REPLACE_WITH_PG_KNOWLEDGE_RETRIEVAL_PASSWORD")
  ' <<<"${secret_template}" >/dev/null ||
    fail "knowledge credential template widened owner scope or drifted retrieval copies"

  local environment encrypted_secrets
  for environment in local gcp; do
    rg --fixed-strings --quiet 'knowledge-db.sops.yaml' \
      "${ROOT_DIR}/clusters/${environment}/secrets/kustomization.yaml" ||
      fail "${environment} secret Kustomization omits knowledge-db.sops.yaml"
    encrypted_secrets="$(
      yq eval-all -o=json 'select(.kind == "Secret")' \
        "${ROOT_DIR}/clusters/${environment}/secrets/knowledge-db.sops.yaml" | jq --slurp '.'
    )"
    jq -e '
      length == 3 and
      all(.[];
        .type == "kubernetes.io/basic-auth" and
        (.stringData | keys | sort) == ["password", "username"] and
        (.stringData.username | startswith("ENC[AES256_GCM,")) and
        (.stringData.password | startswith("ENC[AES256_GCM,")) and
        .sops.encrypted_regex == "^(data|stringData)$" and
        (.sops.age | type) == "array" and (.sops.age | length) > 0 and
        (.sops.mac | startswith("ENC[AES256_GCM,"))) and
      ([.[] | select(.metadata.name == "pg-knowledge-owner" and
        .metadata.namespace == "postgres")] | length == 1) and
      ([.[] | select(.metadata.name == "pg-knowledge-owner" and
        .metadata.namespace != "postgres")] | length == 0) and
      ([.[] | select(.metadata.name == "pg-knowledge-retrieval") |
        .metadata.namespace] | sort) == ["knowledge", "postgres"]
    ' <<<"${encrypted_secrets}" >/dev/null ||
      fail "${environment} knowledge credentials are plaintext, incomplete, or mis-scoped"
  done

  local required_sql
  for required_sql in \
    'chunk_id text PRIMARY KEY' \
    'content text NOT NULL' \
    'embedding vector(1024) NOT NULL' \
    'metadata jsonb NOT NULL' \
    'CREATE OR REPLACE FUNCTION knowledge.is_bounded_clean_text(' \
    "value !~ '^[[:space:]]'" \
    "value !~ '[[:space:]]$'" \
    "value !~ '[[:cntrl:]]'" \
    'knowledge.is_bounded_clean_text(source_value, max_bytes)' \
    'knowledge.is_bounded_clean_text(chunk_id, 512)' \
    "content ~ '[^[:space:]]'" \
    "ARRAY['id', 'title', 'locator', 'revision', 'location']" \
    "'public'," \
    "'approved_non_public'," \
    "'restricted'," \
    "'regulated'," \
    "'secret'," \
    "'authentication'" \
    "principal - ARRAY['kind', 'principal']" \
    "principal - ARRAY['kind', 'network', 'principal']" \
    "'^partner/" \
    'chunks_classification_idx' \
    "ON knowledge.chunks ((metadata->>'classification'))" \
    'chunks_principals_gin_idx' \
    "((metadata->'allowed_principals') jsonb_path_ops)" \
    'chunks_groups_gin_idx' \
    "((metadata->'allowed_groups'))" \
    'chunks_embedding_hnsw_idx' \
    'USING hnsw (embedding vector_cosine_ops)' \
    'CREATE OR REPLACE FUNCTION knowledge.search_authorized_matrix(' \
    'CREATE OR REPLACE FUNCTION knowledge.search_authorized_groups(' \
    "(chunks.metadata->'allowed_principals') @> audience" \
    "(chunks.metadata->'allowed_groups') ?| groups" \
    'GRANT SELECT ON knowledge.chunks TO knowledge_retrieval' \
    'REVOKE ALL ON ALL TABLES IN SCHEMA knowledge FROM PUBLIC'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${schema_sql}" ||
      fail "knowledge schema SQL is missing: ${required_sql}"
  done
  local materialized_count
  materialized_count="$(
    rg --count --fixed-strings 'WITH authorized AS MATERIALIZED' <<<"${schema_sql}"
  )" || true
  [[ "${materialized_count:-0}" -eq 2 ]] ||
    fail "both secure search surfaces must materialize the authorized subset"
  if rg --quiet 'SECURITY[[:space:]]+DEFINER|maintenance_work_mem' <<<"${schema_sql}"; then
    fail "knowledge schema must use invoker rights and the laptop-safe default maintenance memory"
  fi

  yq -e '
    .kind == "Cluster" and .nodes[0].role == "control-plane" and
    (.nodes[0].kubeadmConfigPatches[0] | contains("KubeletInUserNamespace: true"))
  ' "${KIND_CONFIG}" >/dev/null || fail "kind fixture is not safe for constrained/rootless hosts"

  echo "Knowledge store static contract passed"
}

runtime_contract() {
  require_commands docker flux helm jq kind kubectl rg yq
  docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

  local chart chart_version repository source namespace primary runtime_render
  chart="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "cloudnative-pg") |
    .spec.chart.spec.chart' "${ROOT_DIR}/infra/flux/releases.yaml")"
  chart_version="$(yq -er '
    select(.kind == "HelmRelease" and .metadata.name == "cloudnative-pg") |
    .spec.chart.spec.version
  ' "${ROOT_DIR}/infra/flux/releases.yaml")"
  source="$(yq -er 'select(.kind == "HelmRelease" and .metadata.name == "cloudnative-pg") |
    .spec.chart.spec.sourceRef.name' "${ROOT_DIR}/infra/flux/releases.yaml")"
  repository="$(SOURCE="${source}" yq -er '
    select(.kind == "HelmRepository" and .metadata.name == strenv(SOURCE)) | .spec.url
  ' "${ROOT_DIR}/infra/flux/sources.yaml")"

  RUNTIME_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-knowledge-store-${RANDOM}-$$}"
  namespace="postgres"
  RUNTIME_WORKDIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-knowledge-store.XXXXXX")"
  KUBECONFIG="${RUNTIME_WORKDIR}/kubeconfig"
  export KUBECONFIG

  cleanup() {
    local result=$?
    trap - EXIT INT TERM
    if [[ "${KEEP_KIND_CLUSTER:-0}" == "1" && "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
      echo "==> Keeping kind cluster ${RUNTIME_CLUSTER_NAME}; use KUBECONFIG=${KUBECONFIG}"
    else
      if [[ "${RUNTIME_CLUSTER_OWNED}" == true ]]; then
        if kind delete cluster --name "${RUNTIME_CLUSTER_NAME}" >/dev/null 2>&1 &&
          ! kind get clusters 2>/dev/null |
          rg --fixed-strings --line-regexp --quiet "${RUNTIME_CLUSTER_NAME}"; then
          echo "==> Deleted isolated knowledge-store cluster ${RUNTIME_CLUSTER_NAME}"
        else
          echo "warning: failed to delete owned kind cluster ${RUNTIME_CLUSTER_NAME}" >&2
          result=1
        fi
      fi
      rm -rf "${RUNTIME_WORKDIR}"
    fi
    exit "${result}"
  }
  trap cleanup EXIT
  trap 'exit 130' INT TERM

  if kind get clusters | rg --fixed-strings --line-regexp --quiet "${RUNTIME_CLUSTER_NAME}"; then
    fail "kind cluster already exists; refusing to mutate it: ${RUNTIME_CLUSTER_NAME}"
  fi

  echo "==> Creating isolated kind cluster ${RUNTIME_CLUSTER_NAME}"
  kind create cluster --name "${RUNTIME_CLUSTER_NAME}" --image "${KIND_NODE_IMAGE}" \
    --config "${KIND_CONFIG}" --kubeconfig "${KUBECONFIG}" --wait 180s
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
  kubectl --namespace "${namespace}" create secret generic pg-knowledge-owner \
    --type kubernetes.io/basic-auth \
    --from-literal username=knowledge_owner \
    --from-literal password="${OWNER_PASSWORD}" >/dev/null
  kubectl --namespace "${namespace}" create secret generic pg-knowledge-retrieval \
    --type kubernetes.io/basic-auth \
    --from-literal username=knowledge_retrieval \
    --from-literal password="${RETRIEVAL_PASSWORD}" >/dev/null

  NAMESPACE="${namespace}" yq '
    .metadata.namespace = strenv(NAMESPACE) |
    .spec.storage.size = "1Gi" |
    .spec.monitoring.enablePodMonitor = false |
    .spec.postgresql.pg_hba = [
      "hostssl knowledge knowledge_owner all scram-sha-256",
      "hostssl knowledge knowledge_retrieval all scram-sha-256",
      "hostssl all all all reject",
      "hostnossl all all all reject"
    ] |
    del(.spec.backup, .spec.serviceAccountTemplate, .spec.managed)
  ' "${CLUSTER_MANIFEST}" >"${RUNTIME_WORKDIR}/cluster.yaml"

  echo "==> Reconciling the one-instance pgvector operand"
  kubectl apply --filename "${RUNTIME_WORKDIR}/cluster.yaml" >/dev/null
  kubectl --namespace "${namespace}" wait cluster/platform-pg \
    --for=condition=Ready --timeout=8m >/dev/null

  runtime_render="${RUNTIME_WORKDIR}/postgres-render.yaml"
  kubectl kustomize "${ROOT_DIR}/infra/postgres" >"${runtime_render}"
  yq eval-all '
    select(
      (.kind == "DatabaseRole" and
        (.metadata.name == "knowledge-owner" or .metadata.name == "knowledge-retrieval")) or
      (.kind == "Database" and .metadata.name == "knowledge") or
      (.kind == "ConfigMap" and .metadata.name == "knowledge-schema-v1") or
      (.kind == "Job" and .metadata.name == "knowledge-schema-v1") or
      (.kind == "NetworkPolicy" and .metadata.name == "knowledge-schema-v1")
    )
  ' "${runtime_render}" >"${RUNTIME_WORKDIR}/knowledge.raw.yaml"
  flux envsubst --strict <"${RUNTIME_WORKDIR}/knowledge.raw.yaml" \
    >"${RUNTIME_WORKDIR}/knowledge.yaml"

  echo "==> Reconciling declarative roles, database, vector extension, and schema v1"
  kubectl apply --filename "${RUNTIME_WORKDIR}/knowledge.yaml" >/dev/null
  if ! kubectl --namespace "${namespace}" wait job/knowledge-schema-v1 \
    --for=condition=Complete --timeout=8m >/dev/null; then
    kubectl --namespace "${namespace}" get cluster,databaserole,database,job,pod >&2 || true
    kubectl --namespace "${namespace}" logs job/knowledge-schema-v1 >&2 || true
    fail "knowledge schema Job did not complete"
  fi

  primary="$(kubectl --namespace "${namespace}" get cluster platform-pg \
    --output=jsonpath='{.status.currentPrimary}')"
  [[ -n "${primary}" ]] || fail "CNPG did not report a primary instance"

  admin_sql() {
    local database="$1"
    shift
    kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
      psql --no-psqlrc --set=ON_ERROR_STOP=1 --username postgres --dbname "${database}" "$@"
  }
  owner_sql() {
    kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
      env PGPASSWORD="${OWNER_PASSWORD}" \
      psql --no-psqlrc --set=ON_ERROR_STOP=1 \
      --dbname='host=127.0.0.1 dbname=knowledge user=knowledge_owner sslmode=require' "$@"
  }
  retrieval_sql() {
    kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
      env PGPASSWORD="${RETRIEVAL_PASSWORD}" \
      psql --no-psqlrc --set=ON_ERROR_STOP=1 \
      --dbname='host=127.0.0.1 dbname=knowledge user=knowledge_retrieval sslmode=require' "$@"
  }
  retrieval_plan() {
    local query="$1"
    kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
      env PGPASSWORD="${RETRIEVAL_PASSWORD}" PGOPTIONS='-c enable_seqscan=off' \
      psql --no-psqlrc --quiet --tuples-only --no-align --set=ON_ERROR_STOP=1 \
      --dbname='host=127.0.0.1 dbname=knowledge user=knowledge_retrieval sslmode=require' \
      --command="${query}"
  }
  assert_prefilter_plan() {
    local plan="$1" index="$2"
    jq -e --arg index "${index}" '
      any(.. | objects; .["Index Name"]? == $index) and
      any(.. | objects; .["Node Type"]? == "CTE Scan") and
      all(.. | objects; .["Index Name"]? != "chunks_embedding_hnsw_idx")
    ' <<<"${plan}" >/dev/null || fail "materialized prefilter did not use ${index}"
  }

  local actual
  actual="$(admin_sql postgres --tuples-only --no-align --command="
    SELECT count(*) = 2
    FROM pg_roles
    WHERE rolname IN ('knowledge_owner', 'knowledge_retrieval')
      AND rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb
      AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
      AND ((rolname = 'knowledge_owner' AND rolconnlimit = 4)
        OR (rolname = 'knowledge_retrieval' AND rolconnlimit = 16))
  ")"
  [[ "${actual}" == "t" ]] || fail "runtime role attributes are not least-privilege"
  actual="$(admin_sql knowledge --tuples-only --no-align --command="
    SELECT current_setting('server_version_num') = '170010'
      AND (SELECT extversion = '0.8.5' FROM pg_extension WHERE extname = 'vector')
      AND (SELECT pg_get_userbyid(datdba) = 'knowledge_owner'
        FROM pg_database WHERE datname = 'knowledge')
      AND to_regclass('knowledge.chunks') IS NOT NULL
      AND to_regclass('knowledge.schema_migrations') IS NOT NULL
      AND to_regprocedure(
        'knowledge.search_authorized_matrix(vector,text[],jsonb,integer)'
      ) IS NOT NULL
      AND to_regprocedure(
        'knowledge.search_authorized_groups(vector,text[],text[],integer)'
      ) IS NOT NULL
      AND (SELECT count(*) = 5 FROM pg_indexes
        WHERE schemaname = 'knowledge' AND indexname IN (
		  'chunks_classification_idx', 'chunks_principals_gin_idx',
		  'chunks_groups_gin_idx', 'chunks_source_id_idx', 'chunks_embedding_hnsw_idx'
        ))
      AND (SELECT count(*) = 1 FROM knowledge.schema_migrations WHERE version = 1)
      AND NOT has_table_privilege('knowledge_retrieval', 'knowledge.chunks', 'INSERT')
      AND has_table_privilege('knowledge_retrieval', 'knowledge.chunks', 'SELECT')
  ")"
  [[ "${actual}" == "t" ]] || fail "runtime database, extension, schema, index, or grant drifted"

  echo "==> Exercising valid typed metadata and secure search surfaces"
  owner_sql >/dev/null <<'SQL'
INSERT INTO knowledge.chunks (chunk_id, content, embedding, metadata)
VALUES
  (
    'matrix-shared',
    'Shared Matrix chunk',
    (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
    '{
      "source": {
        "id": "source-matrix-shared",
        "title": "Shared Matrix source",
        "locator": "https://docs.example/shared",
        "revision": "sha256:shared",
        "location": "section-1"
      },
      "classification": "public",
      "allowed_principals": [
        {"kind": "matrix", "principal": "@alice:org-a.example"},
        {"kind": "matrix", "principal": "@bob:org-a.example"}
      ],
      "allowed_groups": []
    }'::jsonb
  ),
  (
    'matrix-partial',
    'Partial Matrix chunk',
    (ARRAY[0.9::real, 0.1::real] || array_fill(0::real, ARRAY[1022]))::vector(1024),
    '{
      "source": {"id": "source-matrix-partial"},
      "classification": "public",
      "allowed_principals": [
        {"kind": "matrix", "principal": "@alice:org-a.example"}
      ],
      "allowed_groups": []
    }'::jsonb
  ),
  (
    'bridged',
    'Bridged Matrix chunk',
    (ARRAY[0.8::real, 0.2::real] || array_fill(0::real, ARRAY[1022]))::vector(1024),
    '{
      "source": {"id": "source-bridged"},
      "classification": "approved_non_public",
      "allowed_principals": [
        {
          "kind": "bridged_matrix",
          "network": "slack",
          "principal": "@slack_bob:org-a.example"
        }
      ],
      "allowed_groups": []
    }'::jsonb
  ),
  (
    'partner-group',
    E'Partner group chunk\nSecond line',
    (ARRAY[0.7::real, 0.3::real] || array_fill(0::real, ARRAY[1022]))::vector(1024),
    '{
      "source": {"id": "source-partner", "location": "page-2"},
      "classification": "public",
      "allowed_principals": [],
      "allowed_groups": ["partner/org-b/docs"]
    }'::jsonb
  ),
  (
    'restricted',
    'Restricted chunk',
    (ARRAY[0.6::real, 0.4::real] || array_fill(0::real, ARRAY[1022]))::vector(1024),
    '{
      "source": {"id": "source-restricted"},
      "classification": "restricted",
      "allowed_principals": [
        {"kind": "matrix", "principal": "@alice:org-a.example"}
      ],
      "allowed_groups": []
    }'::jsonb
  );
ANALYZE knowledge.chunks;
SQL

  actual="$(retrieval_sql --tuples-only --no-align --command='SELECT count(*) FROM knowledge.chunks')"
  [[ "${actual}" == "5" ]] || fail "valid knowledge fixtures were not inserted"

  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT string_agg(chunk_id, ',' ORDER BY chunk_id)
    FROM knowledge.search_authorized_matrix(
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      ARRAY['public']::text[],
      '[
        {\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"},
        {\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}
      ]'::jsonb,
      10
    )
  ")"
  if [[ "${actual}" != "matrix-shared" ]]; then
    local matrix_diagnostic
    matrix_diagnostic="$(retrieval_sql --tuples-only --no-align --field-separator='|' --command="
      WITH input AS (
        SELECT
          ARRAY['public']::text[] AS classes,
          '[
            {\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"},
            {\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}
          ]'::jsonb AS audience,
          (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024) AS query
      )
      SELECT
        knowledge.is_valid_principal_array(input.audience, 16),
        input.classes = ARRAY['public']::text[],
        vector_norm(input.query) > 0,
        chunks.metadata->'allowed_principals' @> input.audience,
        (chunks.metadata->>'classification') = ANY (input.classes),
        (SELECT count(*) FROM knowledge.search_authorized_matrix(
          input.query, input.classes, input.audience, 10
        ))
      FROM knowledge.chunks AS chunks CROSS JOIN input
      WHERE chunks.chunk_id = 'matrix-shared'
    ")"
    fail "Matrix search returned $(printf '%q' "${actual}"); predicates/function=${matrix_diagnostic}"
  fi
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT string_agg(chunk_id, ',' ORDER BY chunk_id)
    FROM knowledge.search_authorized_matrix(
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      ARRAY['approved_non_public', 'public']::text[],
      '[{
        \"kind\":\"bridged_matrix\",
        \"network\":\"slack\",
        \"principal\":\"@slack_bob:org-a.example\"
      }]'::jsonb,
      10
    )
  ")"
  [[ "${actual}" == "bridged" ]] || fail "bridged Matrix kind/network identity was erased"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT string_agg(chunk_id, ',' ORDER BY chunk_id)
    FROM knowledge.search_authorized_groups(
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      ARRAY['public']::text[], ARRAY['partner/org-b/docs']::text[], 10
    )
  ")"
  [[ "${actual}" == "partner-group" ]] || fail "exact partner-group intersection failed"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT count(*)
    FROM knowledge.search_authorized_groups(
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      ARRAY['public']::text[], ARRAY['partner/org-b']::text[], 10
    )
  ")"
  [[ "${actual}" == "0" ]] || fail "partner-group prefix widened authorization"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT count(*)
    FROM knowledge.search_authorized_matrix(
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      ARRAY['restricted']::text[],
      '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
      10
    )
  ")"
  [[ "${actual}" == "0" ]] || fail "v1 search admitted a forbidden classification"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT
      (SELECT metadata = '{
        \"source\": {
          \"id\": \"source-matrix-shared\",
          \"title\": \"Shared Matrix source\",
          \"locator\": \"https://docs.example/shared\",
          \"revision\": \"sha256:shared\",
          \"location\": \"section-1\"
        },
        \"classification\": \"public\",
        \"allowed_principals\": [
          {\"kind\": \"matrix\", \"principal\": \"@alice:org-a.example\"},
          {\"kind\": \"matrix\", \"principal\": \"@bob:org-a.example\"}
        ],
        \"allowed_groups\": []
      }'::jsonb FROM knowledge.chunks WHERE chunk_id = 'matrix-shared')
      AND
      (SELECT metadata = '{
        \"source\": {\"id\": \"source-bridged\"},
        \"classification\": \"approved_non_public\",
        \"allowed_principals\": [{
          \"kind\": \"bridged_matrix\",
          \"network\": \"slack\",
          \"principal\": \"@slack_bob:org-a.example\"
        }],
        \"allowed_groups\": []
      }'::jsonb FROM knowledge.chunks WHERE chunk_id = 'bridged')
      AND
      (SELECT metadata = '{
        \"source\": {\"id\": \"source-partner\", \"location\": \"page-2\"},
        \"classification\": \"public\",
        \"allowed_principals\": [],
        \"allowed_groups\": [\"partner/org-b/docs\"]
      }'::jsonb FROM knowledge.chunks WHERE chunk_id = 'partner-group')
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "native, bridged, or partner-group metadata did not round-trip without drift"

  echo "==> Proving malformed metadata fails at the database boundary"
  owner_sql >/dev/null <<'SQL'
DO $test$
DECLARE
  v vector(1024) := (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024);
  z vector(1024) := array_fill(0::real, ARRAY[1024])::vector(1024);
BEGIN
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-missing-class', 'x', v,
      '{"source":{"id":"s"},"allowed_principals":[{"kind":"matrix","principal":"@alice:org-a.example"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted missing classification';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-class', 'x', v,
      '{"source":{"id":"s"},"classification":"internal","allowed_principals":[{"kind":"matrix","principal":"@alice:org-a.example"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted unknown classification';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-localpart', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[{"kind":"matrix","principal":"alice"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted bare localpart';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-mxid', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[{"kind":"matrix","principal":"@Alice:org-a.example"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted malformed MXID';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-matrix-network', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[{"kind":"matrix","network":"slack","principal":"@alice:org-a.example"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted network on native Matrix principal';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-principal-field', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[{"kind":"matrix","principal":"@alice:org-a.example","role":"admin"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted unknown principal field';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-bridge-network', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[{"kind":"bridged_matrix","principal":"@slack_bob:org-a.example"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted bridged Matrix principal without network';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-group-wildcard', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/*"]}'
    );
    RAISE EXCEPTION 'accepted wildcard partner group';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-group-bare', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["org-b"]}'
    );
    RAISE EXCEPTION 'accepted bare partner identity';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-duplicate-principal', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[{"kind":"matrix","principal":"@alice:org-a.example"},{"kind":"matrix","principal":"@alice:org-a.example"}],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted duplicate principal';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-duplicate-group', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/docs","partner/org-b/docs"]}'
    );
    RAISE EXCEPTION 'accepted duplicate group';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-empty-acl', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":[]}'
    );
    RAISE EXCEPTION 'accepted empty authorization operands';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-extra-field', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/docs"],"extra":true}'
    );
    RAISE EXCEPTION 'accepted unknown metadata field';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-source', 'x', v,
      '{"source":{"id":"s","uri":"https://example.invalid"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/docs"]}'
    );
    RAISE EXCEPTION 'accepted unknown source field';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      E'\n', 'x', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/docs"]}'
    );
    RAISE EXCEPTION 'accepted newline-only chunk ID';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-source-whitespace', 'x', v,
      jsonb_build_object(
        'source', jsonb_build_object('id', E'\t'),
        'classification', 'public',
        'allowed_principals', '[]'::jsonb,
        'allowed_groups', '["partner/org-b/docs"]'::jsonb
      )
    );
    RAISE EXCEPTION 'accepted tab-only source ID';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-content-whitespace', E'\n\t', v,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/docs"]}'
    );
    RAISE EXCEPTION 'accepted POSIX-whitespace-only content';
  EXCEPTION WHEN check_violation THEN NULL; END;
  BEGIN
    INSERT INTO knowledge.chunks VALUES (
      'invalid-zero-vector', 'x', z,
      '{"source":{"id":"s"},"classification":"public","allowed_principals":[],"allowed_groups":["partner/org-b/docs"]}'
    );
    RAISE EXCEPTION 'accepted zero cosine vector';
  EXCEPTION WHEN check_violation THEN NULL; END;
END;
$test$;
SQL

  if owner_sql --command="
    INSERT INTO knowledge.chunks VALUES (
      'invalid-vector-dimensions', 'x', ARRAY[1, 0, 0]::vector(3),
      '{\"source\":{\"id\":\"s\"},\"classification\":\"public\",\"allowed_principals\":[],\"allowed_groups\":[\"partner/org-b/docs\"]}'::jsonb
    )
  " >/dev/null 2>&1; then
    fail "accepted a vector with the wrong dimensions"
  fi

  actual="$(retrieval_sql --tuples-only --no-align --command='SELECT count(*) FROM knowledge.chunks')"
  [[ "${actual}" == "5" ]] || fail "retrieval role cannot read the knowledge chunks"
  if retrieval_sql --command="
    INSERT INTO knowledge.chunks SELECT * FROM knowledge.chunks WHERE false
  " >/dev/null 2>&1; then
    fail "retrieval role unexpectedly holds write privileges"
  fi
  if owner_sql --command='CREATE DATABASE knowledge_forbidden' >/dev/null 2>&1; then
    fail "knowledge owner unexpectedly holds CREATEDB"
  fi
  if owner_sql --command='CREATE ROLE knowledge_forbidden' >/dev/null 2>&1; then
    fail "knowledge owner unexpectedly holds CREATEROLE"
  fi
  admin_sql postgres --command='CREATE DATABASE unrelated_service' >/dev/null
  if kubectl --namespace "${namespace}" exec "pod/${primary}" --container postgres -- \
    env PGPASSWORD="${RETRIEVAL_PASSWORD}" \
    psql --no-psqlrc --set=ON_ERROR_STOP=1 \
    --dbname='host=127.0.0.1 dbname=unrelated_service user=knowledge_retrieval sslmode=require' \
    --command='SELECT 1' >/dev/null 2>&1; then
    fail "retrieval role crossed the exact knowledge HBA boundary"
  fi
  if kubectl --namespace "${namespace}" exec "pod/${primary}" --container postgres -- \
    env PGPASSWORD="${OWNER_PASSWORD}" \
    psql --no-psqlrc --set=ON_ERROR_STOP=1 \
    --dbname='host=127.0.0.1 dbname=unrelated_service user=knowledge_owner sslmode=require' \
    --command='SELECT 1' >/dev/null 2>&1; then
    fail "knowledge owner crossed the exact knowledge HBA boundary"
  fi

  echo "==> Verifying ACL prefilter indexes, materialization, exact sort, and separate HNSW"
  local query_vector matrix_plan classification_plan principal_plan group_plan hnsw_plan
  query_vector="(ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024)"
  matrix_plan="$(retrieval_plan "
    EXPLAIN (FORMAT JSON, COSTS OFF)
    WITH authorized AS MATERIALIZED (
      SELECT chunks.chunk_id, chunks.content, chunks.embedding, chunks.metadata
      FROM knowledge.chunks
      WHERE (chunks.metadata->>'classification') = 'public'
        AND (chunks.metadata->'allowed_principals') @>
          '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb
    )
    SELECT chunk_id, content, metadata, embedding <=> ${query_vector} AS cosine_distance
    FROM authorized ORDER BY cosine_distance, chunk_id LIMIT 10
  ")"
  jq -e '
    any(.. | objects; .["Node Type"]? == "CTE Scan") and
    any(.. | objects; .["Node Type"]? == "Sort") and
    all(.. | objects; .["Index Name"]? != "chunks_embedding_hnsw_idx")
  ' <<<"${matrix_plan}" >/dev/null ||
    fail "secure plan did not materialize ACL rows before exact sorting"

  classification_plan="$(retrieval_plan "
    EXPLAIN (FORMAT JSON, COSTS OFF)
    WITH authorized AS MATERIALIZED (
      SELECT * FROM knowledge.chunks WHERE (metadata->>'classification') = 'public'
    )
    SELECT chunk_id FROM authorized ORDER BY embedding <=> ${query_vector} LIMIT 5
  ")"
  principal_plan="$(retrieval_plan "
    EXPLAIN (FORMAT JSON, COSTS OFF)
    WITH authorized AS MATERIALIZED (
      SELECT * FROM knowledge.chunks
      WHERE (metadata->'allowed_principals') @>
        '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb
    )
    SELECT chunk_id FROM authorized ORDER BY embedding <=> ${query_vector} LIMIT 5
  ")"
  group_plan="$(retrieval_plan "
    EXPLAIN (FORMAT JSON, COSTS OFF)
    WITH authorized AS MATERIALIZED (
      SELECT * FROM knowledge.chunks
      WHERE (metadata->'allowed_groups') ?| ARRAY['partner/org-b/docs']::text[]
    )
    SELECT chunk_id FROM authorized ORDER BY embedding <=> ${query_vector} LIMIT 5
  ")"
  assert_prefilter_plan "${classification_plan}" "chunks_classification_idx"
  assert_prefilter_plan "${principal_plan}" "chunks_principals_gin_idx"
  assert_prefilter_plan "${group_plan}" "chunks_groups_gin_idx"

  hnsw_plan="$(retrieval_plan "
    EXPLAIN (FORMAT JSON, COSTS OFF)
    SELECT chunk_id FROM knowledge.chunks
    ORDER BY embedding <=> ${query_vector} LIMIT 5
  ")"
  jq -e '
    any(.. | objects; .["Index Name"]? == "chunks_embedding_hnsw_idx") and
    all(.. | objects; .["Index Name"]? != "chunks_classification_idx") and
		all(.. | objects; .["Index Name"]? != "chunks_principals_gin_idx") and
		all(.. | objects; .["Index Name"]? != "chunks_groups_gin_idx")
  ' <<<"${hnsw_plan}" >/dev/null || fail "vector-only plan was not independently HNSW-eligible"

  echo "Knowledge store runtime contract passed (${chart} ${chart_version}, ${primary})"
}

static_contract
if ${runtime}; then
  runtime_contract
fi
