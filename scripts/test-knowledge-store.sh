#!/usr/bin/env bash
# Validate the CNPG pgvector knowledge-store contract. --runtime creates its own disposable kind
# cluster, installs the repository-pinned CNPG chart, and never reads or mutates the active context.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly CLUSTER_MANIFEST="${ROOT_DIR}/infra/postgres/cluster.yaml"
readonly KUSTOMIZATION="${ROOT_DIR}/infra/postgres/kustomization.yaml"
readonly INGESTION_COMPONENT="${ROOT_DIR}/infra/postgres/components/knowledge-ingestion/kustomization.yaml"
readonly INGESTION_FIXTURE="${ROOT_DIR}/scripts/testdata/knowledge-ingestion-postgres"
readonly INGESTION_CHECKPOINT_SQL="${ROOT_DIR}/infra/knowledge/base/checkpoint.sql"
readonly INGESTION_GC_SQL="${ROOT_DIR}/infra/knowledge/base/gc.sql"
readonly INGESTION_PLAN_SQL="${ROOT_DIR}/infra/knowledge/base/plan.sql"
readonly INGESTION_WRITE_SQL="${ROOT_DIR}/infra/knowledge/base/write.sql"
readonly CONNECTOR_PUBLISH_SQL="${ROOT_DIR}/infra/knowledge/base/connector-publish.sql"
readonly CONNECTOR_CLAIM_SQL="${ROOT_DIR}/infra/knowledge/base/connector-claim.sql"
readonly CONNECTOR_TOMBSTONE_SQL="${ROOT_DIR}/infra/knowledge/base/connector-tombstone.sql"
readonly INGESTION_CRONJOB="${ROOT_DIR}/infra/knowledge/base/cronjob.yaml"
readonly INGESTION_SCRIPT="${ROOT_DIR}/infra/knowledge/base/ingestion.py"
readonly MATRIX_SOURCE_EXAMPLE="${ROOT_DIR}/infra/knowledge/source-bundle.example.yaml"
readonly PARTNER_SOURCE_EXAMPLE="${ROOT_DIR}/infra/knowledge/source-bundle-partner.example.yaml"
readonly KIND_CONFIG="${ROOT_DIR}/scripts/testdata/postgres-audit-kind.yaml"
readonly KIND_NODE_IMAGE="kindest/node:v1.34.0@sha256:7416a61b42b1662ca6ca89f02028ac133a309a2a30ba309614e8ec94d976dc5a"
readonly POSTGRES_IMAGE="ghcr.io/cloudnative-pg/postgresql:17.10-202607130907-system-trixie@sha256:c141aec61cab8da3e215aebe0fa155e78442fbb41c982a86743915a967e12af9"
readonly OWNER_PASSWORD="KNOWLEDGE_OWNER_PASSWORD_SENTINEL"
readonly CONNECTOR_PASSWORD="KNOWLEDGE_CONNECTOR_PASSWORD_SENTINEL"
readonly INGESTION_PASSWORD="KNOWLEDGE_INGESTION_PASSWORD_SENTINEL"
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

  local base_render base_render_objects render render_objects roles database job policy schema_sql
  local job_v2 policy_v2 schema_v2_sql job_v3 policy_v3 schema_v3_sql
  local secret_template ingestion_secret_template
  local checkpoint_sql gc_sql plan_sql write_sql connector_publish_sql connector_claim_sql
  local connector_tombstone_sql
  local lease_reclaim_line lease_claim_line pending_cleanup_line final_cleanup_line current_reset
  local connector_complete_line
  local publish_digest_line publish_begin_line publish_insert_line publish_complete_line
  local publish_commit_line tombstone_insert_line tombstone_apply_line tombstone_release_line
  local tombstone_commit_line write_lease_release_line write_commit_line
  local connector_claim_limit_count publish_begin_count publish_commit_count
  local claim_begin_count claim_commit_count tombstone_begin_count tombstone_commit_count
  local lease_clock_count pending_delete_count final_delete_count
  base_render="$(kubectl kustomize "${ROOT_DIR}/infra/postgres")"
  base_render_objects="$(yq eval-all -o=json '.' <<<"${base_render}" | jq --slurp '.')"
  render="$(kubectl kustomize "${INGESTION_FIXTURE}" --load-restrictor LoadRestrictionsNone)"
  render_objects="$(yq eval-all -o=json '.' <<<"${render}" | jq --slurp '.')"
  roles="$(
    yq eval-all -o=json '
      select(.kind == "DatabaseRole" and
        (.metadata.name == "knowledge-owner" or
          .metadata.name == "knowledge-ingestion" or
          .metadata.name == "knowledge-connector" or
          .metadata.name == "knowledge-retrieval"))
    ' <<<"${render}" | jq --slurp '.'
  )"
  database="$(
    yq eval-all -o=json 'select(.kind == "Database" and .metadata.name == "knowledge")' \
      <<<"${render}"
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
  job_v2="$(
    yq eval-all -o=json 'select(.kind == "Job" and .metadata.name == "knowledge-schema-v2")' \
      <<<"${render}"
  )"
  policy_v2="$(
    yq eval-all -o=json '
      select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-schema-v2")
    ' <<<"${render}"
  )"
  schema_v2_sql="$(
    yq eval-all -r 'select(.kind == "ConfigMap" and .metadata.name == "knowledge-schema-v2") |
      .data."schema.sql"' <<<"${render}"
  )"
  job_v3="$(
    yq eval-all -o=json 'select(.kind == "Job" and .metadata.name == "knowledge-schema-v3")' \
      <<<"${render}"
  )"
  policy_v3="$(
    yq eval-all -o=json '
      select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-schema-v3")
    ' <<<"${render}"
  )"
  schema_v3_sql="$(
    yq eval-all -r 'select(.kind == "ConfigMap" and .metadata.name == "knowledge-schema-v3") |
      .data."schema.sql"' <<<"${render}"
  )"
  checkpoint_sql="$(<"${INGESTION_CHECKPOINT_SQL}")"
  gc_sql="$(<"${INGESTION_GC_SQL}")"
  plan_sql="$(<"${INGESTION_PLAN_SQL}")"
  write_sql="$(<"${INGESTION_WRITE_SQL}")"
  connector_publish_sql="$(<"${CONNECTOR_PUBLISH_SQL}")"
  connector_claim_sql="$(<"${CONNECTOR_CLAIM_SQL}")"
  connector_tombstone_sql="$(<"${CONNECTOR_TOMBSTONE_SQL}")"
  secret_template="$(
    yq eval-all -o=json 'select(.kind == "Secret")' \
      "${ROOT_DIR}/infra/secrets/knowledge-db.sops.yaml.example" | jq --slurp '.'
  )"
  ingestion_secret_template="$(
    yq eval-all -o=json 'select(.kind == "Secret")' \
      "${ROOT_DIR}/infra/secrets/knowledge-ingestion.sops.yaml.example" | jq --slurp '.'
  )"

  jq -e '
    ([.[] | select(.kind == "Cluster") | .metadata.name] == ["platform-pg"]) and
    ([.[] | select(.kind == "Deployment" or .kind == "StatefulSet")] | length == 0) and
    ([.[] | select(.kind == "DatabaseRole" and
      (.metadata.name == "knowledge-ingestion" or .metadata.name == "knowledge-connector"))] |
      length == 0) and
    ([.[] | select(.kind == "Job") | .metadata.name] == ["knowledge-schema-v1"]) and
    ([.[] | select(.kind == "ConfigMap" and
      (.metadata.name == "knowledge-schema-v2" or .metadata.name == "knowledge-schema-v3"))] |
      length == 0)
  ' <<<"${base_render_objects}" >/dev/null ||
    fail "base Postgres render unexpectedly enables the optional ingestion boundary"

  jq -e '
    ([.[] | select(.kind == "Cluster") | .metadata.name] == ["platform-pg"]) and
    ([.[] | select(.kind == "Deployment" or .kind == "StatefulSet")] | length == 0) and
    ([.[] | select(.kind == "Job") | .metadata.name] | sort) ==
      ["knowledge-schema-v1", "knowledge-schema-v2", "knowledge-schema-v3"] and
    ([.[] | select(.kind == "ConfigMap" and
      (.metadata.name == "knowledge-schema-v1" or
        .metadata.name == "knowledge-schema-v2" or
        .metadata.name == "knowledge-schema-v3")) |
      .immutable] | length == 3) and
    all(.[] | select(.kind == "ConfigMap" and
      (.metadata.name == "knowledge-schema-v1" or
        .metadata.name == "knowledge-schema-v2" or
        .metadata.name == "knowledge-schema-v3")); .immutable == true)
  ' <<<"${render_objects}" >/dev/null ||
    fail "knowledge store must reuse platform-pg and retain all immutable schema artifacts"

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

  yq eval-all -e '
    select(.kind == "Cluster" and .metadata.name == "platform-pg") |
    (.spec.postgresql.pg_hba | join("|")) ==
      "hostssl synapse synapse all scram-sha-256|hostssl mas mas all scram-sha-256|hostssl bridge bridge all scram-sha-256|hostssl kagent kagent all scram-sha-256|hostssl keycloak keycloak all scram-sha-256|hostssl knowledge knowledge_owner all scram-sha-256|hostssl knowledge knowledge_ingestion all scram-sha-256|hostssl knowledge knowledge_connector all scram-sha-256|hostssl knowledge knowledge_retrieval all scram-sha-256|hostssl all all all reject|hostnossl all all all reject"
  ' <<<"${render}" >/dev/null ||
    fail "opt-in ingestion Component did not insert the exact knowledge HBA rows"

  jq -e '
    length == 4 and
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
      .name == "knowledge_ingestion" and .connectionLimit == 4 and
      .passwordSecret.name == "pg-knowledge-ingestion") and
    any(.[].spec;
      .name == "knowledge_connector" and .connectionLimit == 2 and
      .passwordSecret.name == "pg-knowledge-connector") and
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
    (.resources | contains(["knowledge-schema-v2.yaml"]) | not) and
    (.resources | contains(["knowledge-schema-v3.yaml"]) | not) and
    (has("replacements") | not)
  ' "${KUSTOMIZATION}" >/dev/null ||
    fail "base Postgres Kustomization unexpectedly includes an opt-in migration"

  yq -e '
    .apiVersion == "kustomize.config.k8s.io/v1alpha1" and
    .kind == "Component" and
    (.resources | sort | join("|")) ==
      "connector-role.yaml|knowledge-schema-v2.yaml|knowledge-schema-v3.yaml|role.yaml" and
    (.patches | length) == 1 and
    .patches[0].target.group == "postgresql.cnpg.io" and
    .patches[0].target.version == "v1" and
    .patches[0].target.kind == "Cluster" and
    .patches[0].target.name == "platform-pg" and
    .patches[0].target.namespace == "postgres" and
    (.patches[0].patch | contains("path: /spec/postgresql/pg_hba/6")) and
    (.patches[0].patch |
      contains("value: hostssl knowledge knowledge_ingestion all scram-sha-256")) and
    (.patches[0].patch | contains("path: /spec/postgresql/pg_hba/7")) and
    (.patches[0].patch |
      contains("value: hostssl knowledge knowledge_connector all scram-sha-256"))
  ' "${INGESTION_COMPONENT}" >/dev/null ||
    fail "opt-in ingestion Component resources or exact HBA patch drifted"

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
      (.args[0] | contains("knowledge_ingestion")) and
      (.args[0] | contains("schema_migrations WHERE version = 1")) and
      (.args[0] | contains("exec psql --no-psqlrc --set=ON_ERROR_STOP=1")) and
      (.volumeMounts | map(select(.name == "schema" and .readOnly == true)) | length) == 1) and
    (.spec.template.spec.volumes |
      map(select(.name == "schema" and .configMap.name == "knowledge-schema-v2")) | length) == 1
  ' <<<"${job_v2}" >/dev/null ||
    fail "bounded restricted v2 schema Job contract drifted"

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
      (.args[0] | contains("knowledge_connector")) and
      (.args[0] | contains("schema_migrations WHERE version = 2")) and
      (.args[0] | contains("exec psql --no-psqlrc --set=ON_ERROR_STOP=1")) and
      (.volumeMounts | map(select(.name == "schema" and .readOnly == true)) | length) == 1) and
    (.spec.template.spec.volumes |
      map(select(.name == "schema" and .configMap.name == "knowledge-schema-v3")) | length) == 1
  ' <<<"${job_v3}" >/dev/null ||
    fail "bounded restricted v3 schema Job contract drifted"

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
    .spec.podSelector.matchLabels == {
      "app.kubernetes.io/name": "knowledge-schema",
      "app.kubernetes.io/instance": "v2"
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
  ' <<<"${policy_v2}" >/dev/null ||
    fail "v2 schema Job egress is broader than DNS plus platform-pg"

  jq -e '
    .spec.podSelector.matchLabels == {
      "app.kubernetes.io/name": "knowledge-schema",
      "app.kubernetes.io/instance": "v3"
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
  ' <<<"${policy_v3}" >/dev/null ||
    fail "v3 schema Job egress is broader than DNS plus platform-pg"

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

  jq -e '
    length == 6 and
    ([.[] | select(.metadata.name == "pg-knowledge-ingestion") |
      .metadata.namespace] | sort) == ["knowledge", "postgres"] and
    all(.[] | select(.metadata.name == "pg-knowledge-ingestion");
      .type == "kubernetes.io/basic-auth" and
      .stringData.username == "knowledge_ingestion" and
      .stringData.password == "REPLACE_WITH_PG_KNOWLEDGE_INGESTION_PASSWORD") and
    ([.[] | select(.metadata.name == "pg-knowledge-connector") |
      .metadata.namespace] | sort) == ["knowledge", "postgres"] and
    all(.[] | select(.metadata.name == "pg-knowledge-connector");
      .type == "kubernetes.io/basic-auth" and
      .stringData.username == "knowledge_connector" and
      .stringData.password == "REPLACE_WITH_PG_KNOWLEDGE_CONNECTOR_PASSWORD") and
    any(.[];
      .metadata.name == "knowledge-ingestion-callers" and
      .metadata.namespace == "agentgateway-system" and
      .type == "Opaque" and
      (.stringData."knowledge-ingestion" | fromjson |
        .key == "REPLACE_WITH_KNOWLEDGE_INGESTION_WORKLOAD_CREDENTIAL" and
        .metadata == {"workload": "knowledge-ingestion"})) and
    any(.[];
      .metadata.name == "knowledge-ingestion-credential" and
      .metadata.namespace == "knowledge" and
      .type == "Opaque" and
      .stringData.authorization ==
        "Bearer REPLACE_WITH_KNOWLEDGE_INGESTION_WORKLOAD_CREDENTIAL")
  ' <<<"${ingestion_secret_template}" >/dev/null ||
    fail "knowledge ingestion credential template copies or scopes drifted"

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

    if rg --fixed-strings --quiet 'knowledge-ingestion.sops.yaml' \
      "${ROOT_DIR}/clusters/${environment}/secrets/kustomization.yaml"; then
      fail "${environment} enables optional ingestion secrets in the default profile"
    fi
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

  for required_sql in \
    "pg_advisory_xact_lock(hashtextextended('fgentic:knowledge-schema:v2', 0))" \
    'REVOKE ALL ON DATABASE knowledge FROM knowledge_ingestion' \
    'GRANT CONNECT ON DATABASE knowledge TO knowledge_ingestion' \
    'REVOKE ALL ON SCHEMA public FROM knowledge_ingestion' \
    'GRANT USAGE ON SCHEMA public TO knowledge_ingestion' \
    'GRANT USAGE ON TYPE public.vector TO knowledge_ingestion' \
    'GRANT EXECUTE ON FUNCTION public.vector_norm(public.vector)' \
    'REVOKE ALL ON SCHEMA knowledge FROM knowledge_ingestion' \
    'GRANT USAGE ON SCHEMA knowledge TO knowledge_ingestion' \
    'REVOKE ALL ON ALL TABLES IN SCHEMA knowledge FROM knowledge_ingestion' \
    'REVOKE ALL ON ALL SEQUENCES IN SCHEMA knowledge FROM knowledge_ingestion' \
    'REVOKE ALL ON ALL FUNCTIONS IN SCHEMA knowledge FROM knowledge_ingestion' \
    'CREATE TABLE IF NOT EXISTS knowledge.ingestion_leases' \
    "CONSTRAINT ingestion_leases_single_writer CHECK (name = 'chunks-v1')" \
    'CREATE TABLE IF NOT EXISTS knowledge.ingestion_pending' \
    'CREATE TABLE IF NOT EXISTS knowledge.ingestion_final' \
    'CREATE TABLE IF NOT EXISTS knowledge.ingestion_embedding_cache' \
    'source_id text NOT NULL' \
    'cached_at timestamptz NOT NULL' \
    'expires_at timestamptz NOT NULL' \
    "CHECK (profile = 'bge-m3-1024-v1')" \
    'knowledge.is_bounded_clean_text(source_id, 512)' \
    "content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')" \
    "expires_at <= cached_at + interval '24 hours'" \
    'PRIMARY KEY (profile, source_id, content_sha256)' \
    'GRANT SELECT, INSERT, UPDATE, DELETE ON knowledge.chunks TO knowledge_ingestion' \
    'GRANT SELECT, INSERT, UPDATE, DELETE ON knowledge.ingestion_leases' \
    'GRANT SELECT, INSERT, DELETE ON knowledge.ingestion_pending' \
    'GRANT SELECT, INSERT, DELETE ON knowledge.ingestion_final' \
    'GRANT SELECT, INSERT, DELETE ON knowledge.ingestion_embedding_cache' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_dns1123_label(text)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_bounded_clean_text(text, integer)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_full_mxid(text)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_valid_principal_array(jsonb, integer)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_valid_group_array(jsonb, integer)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_valid_metadata(jsonb)' \
    'INSERT INTO knowledge.schema_migrations (version)' \
    'VALUES (2)'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${schema_v2_sql}" ||
      fail "knowledge schema v2 SQL is missing: ${required_sql}"
  done
  if rg --quiet \
    'GRANT .*knowledge\.search_authorized_|GRANT .*knowledge\.schema_migrations' \
    <<<"${schema_v2_sql}"; then
    fail "knowledge ingestion role gained retrieval or migration-table authority"
  fi
  if rg --quiet \
    'GRANT .*UPDATE.*knowledge\.ingestion_embedding_cache|GRANT .*TRUNCATE.*knowledge\.ingestion_embedding_cache' \
    <<<"${schema_v2_sql}"; then
    fail "knowledge ingestion cache grants are broader than select/insert/delete"
  fi

  for required_sql in \
    "pg_advisory_xact_lock(hashtextextended('fgentic:knowledge-schema:v3', 0))" \
    'REVOKE ALL ON DATABASE knowledge FROM knowledge_connector' \
    'GRANT CONNECT, TEMPORARY ON DATABASE knowledge TO knowledge_connector' \
    'REVOKE ALL ON ALL TABLES IN SCHEMA knowledge FROM knowledge_connector' \
    'REVOKE ALL ON ALL SEQUENCES IN SCHEMA knowledge FROM knowledge_connector' \
    'REVOKE ALL ON ALL FUNCTIONS IN SCHEMA knowledge FROM knowledge_connector' \
    'CREATE TABLE IF NOT EXISTS knowledge.connector_snapshots' \
    "connector_id = 'git-markdown'" \
    'CREATE TABLE IF NOT EXISTS knowledge.connector_inventory' \
    'CREATE TABLE IF NOT EXISTS knowledge.connector_sources' \
    'claim_expires_at IS NOT NULL' \
    "claim_expires_at <= claimed_at + interval '35 minutes'" \
    'CREATE OR REPLACE FUNCTION knowledge.canonical_jsonb_text(value jsonb)' \
    'ORDER BY entry.key COLLATE "C"' \
    'ORDER BY entry.ordinality' \
    'CREATE OR REPLACE FUNCTION knowledge.connector_inventory_json_digest(value jsonb)' \
    'CREATE OR REPLACE FUNCTION knowledge.connector_inventory_digest(' \
    "'source_id', inventory.source_id" \
    "'source_path', inventory.source_path" \
    "'source_revision', inventory.source_revision" \
    "'content_digest', inventory.content_digest" \
    "'acl_digest', inventory.acl_digest" \
    "'metadata', inventory.metadata" \
    'ORDER BY inventory.source_id COLLATE "C"' \
    'CREATE OR REPLACE FUNCTION knowledge.guard_connector_inventory()' \
    'BEFORE INSERT OR UPDATE OR DELETE ON knowledge.connector_inventory' \
    'CREATE OR REPLACE FUNCTION knowledge.begin_connector_snapshot(' \
    "requested_connector_id <> 'git-markdown'" \
    'CREATE OR REPLACE FUNCTION knowledge.block_connector_snapshot(' \
    'blocked_artifact_digest = requested_artifact_digest' \
    'CREATE OR REPLACE FUNCTION knowledge.try_advance_connector_snapshot(' \
    'CREATE OR REPLACE FUNCTION knowledge.complete_connector_snapshot(' \
    'actual_inventory_digest := knowledge.connector_inventory_digest(' \
    'actual_source_count <> current_snapshot.expected_source_count' \
    'actual_inventory_digest <> requested_inventory_digest' \
    'inventory.acl_digest IS DISTINCT FROM' \
    "'classification', inventory.metadata->'classification'" \
    "'allowed_principals', inventory.metadata->'allowed_principals'" \
    "'allowed_groups', inventory.metadata->'allowed_groups'" \
    'connector inventory source count, canonical digest, or ACL digest is invalid' \
    'SET applied_snapshot_revision = sources.desired_snapshot_revision' \
    'sources.applied_metadata IS NOT DISTINCT FROM sources.desired_metadata' \
    "chunks.metadata->>'classification' = 'authentication'" \
    'sources.applied_digest IS DISTINCT FROM inventory.content_digest' \
    'sources.applied_acl_digest IS DISTINCT FROM inventory.acl_digest' \
    'sources.applied_metadata IS DISTINCT FROM inventory.metadata' \
    "sources.desired_action IS DISTINCT FROM 'tombstone'" \
    "sources.applied_action IS DISTINCT FROM 'tombstone'" \
    "split_part(source_id, '/', 2) = connector_id" \
    "split_part(source_id, '/', 1) || '/' || connector_id || '/' || source_path" \
    'position(chr(92) IN source_path) = 0' \
    "metadata->'source' ?& ARRAY['id', 'locator', 'revision']" \
    'UPDATE knowledge.chunks AS chunks' \
    "'{classification}'" \
    "to_jsonb('authentication'::text)" \
    "chunks.metadata #>> '{source,id}' = sources.source_id" \
    "chunks.metadata->>'classification' <> 'authentication'" \
    'CREATE OR REPLACE FUNCTION knowledge.complete_connector_present(' \
    'CREATE OR REPLACE FUNCTION knowledge.apply_connector_tombstone(' \
    'GRANT SELECT ON knowledge.connector_snapshots TO knowledge_connector' \
    'GRANT INSERT ON knowledge.connector_inventory TO knowledge_connector' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_full_mxid(text)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_valid_principal_array(jsonb, integer)' \
    'GRANT EXECUTE ON FUNCTION knowledge.is_valid_group_array(jsonb, integer)' \
    'GRANT EXECUTE ON FUNCTION knowledge.connector_inventory_json_digest(jsonb)' \
    'GRANT EXECUTE ON FUNCTION knowledge.begin_connector_snapshot(text, text, text, integer)' \
    'GRANT EXECUTE ON FUNCTION knowledge.block_connector_snapshot(text, text, text)' \
    'GRANT EXECUTE ON FUNCTION knowledge.complete_connector_snapshot(text, text, text)' \
    'GRANT SELECT ON knowledge.connector_snapshots, knowledge.connector_inventory' \
    'GRANT SELECT ON knowledge.connector_sources TO knowledge_ingestion' \
    'GRANT UPDATE (claim_holder, claimed_at, claim_expires_at)' \
    'GRANT EXECUTE ON FUNCTION knowledge.complete_connector_present(uuid)' \
    'GRANT EXECUTE ON FUNCTION knowledge.apply_connector_tombstone(uuid)' \
    'INSERT INTO knowledge.schema_migrations (version)' \
    'VALUES (3)'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${schema_v3_sql}" ||
      fail "knowledge schema v3 SQL is missing: ${required_sql}"
  done
  if rg --multiline --multiline-dotall --quiet \
    'GRANT[^;]*ON (FUNCTION )?knowledge\.(chunks|ingestion_[a-z_]+|search_authorized_[a-z_]+)[^;]*TO knowledge_connector;' \
    <<<"${schema_v3_sql}"; then
    fail "knowledge connector gained chunk, embedding, ingestion, or retrieval authority"
  fi
  if rg --multiline --multiline-dotall --quiet \
    'GRANT[^;]*(INSERT|UPDATE|DELETE|TRUNCATE|REFERENCES|TRIGGER|ALL)[^;]*ON knowledge\.(connector_snapshots|connector_sources)[^;]*TO knowledge_connector;|GRANT[^;]*(SELECT|UPDATE|DELETE|TRUNCATE|REFERENCES|TRIGGER|ALL)[^;]*ON knowledge\.connector_inventory[^;]*TO knowledge_connector;' \
    <<<"${schema_v3_sql}"; then
    fail "knowledge connector gained connector-state read or mutation authority"
  fi
  if rg --multiline --multiline-dotall --quiet \
    'GRANT[^;]*(INSERT|DELETE|TRUNCATE)[^;]*ON knowledge\.connector_[a-z_]+[^;]*TO knowledge_ingestion;' \
    <<<"${schema_v3_sql}"; then
    fail "knowledge ingestion gained connector publication or deletion authority"
  fi
  if rg --multiline --multiline-dotall --quiet \
    'GRANT UPDATE[[:space:]]+ON knowledge\.connector_[a-z_]+[^;]*TO knowledge_ingestion;|GRANT[^;]*(INSERT|DELETE|TRUNCATE|REFERENCES|TRIGGER|ALL)[^;]*ON knowledge\.connector_[a-z_]+[^;]*TO knowledge_ingestion;' \
    <<<"${schema_v3_sql}"; then
    fail "knowledge ingestion gained broad connector-state mutation authority"
  fi
  if rg --multiline --multiline-dotall --quiet \
    'GRANT EXECUTE ON FUNCTION knowledge\.(canonical_jsonb_text|connector_inventory_[a-z_]+|begin_connector_snapshot|complete_connector_snapshot)[^;]*TO knowledge_ingestion;' \
    <<<"${schema_v3_sql}"; then
    fail "knowledge ingestion can publish or complete connector inventories"
  fi

  for required_sql in \
    '\set ON_ERROR_STOP on' \
    'BEGIN;' \
    'CREATE TEMPORARY TABLE connector_snapshot_input' \
    "\\copy connector_snapshot_input (payload) FROM '/sources/.connector/git-markdown/current.json'" \
    'jsonb_array_length(snapshot->'"'"'sources'"'"')' \
    "snapshot ? 'blocked'" \
    'knowledge.block_connector_snapshot(' \
    'jsonb_agg(' \
    'jsonb_build_object(' \
    "'source_id'" \
    "'source_path'" \
    "'source_revision'" \
    "'content_digest'" \
    "'acl_digest'" \
    "'metadata'" \
    'knowledge.connector_inventory_json_digest(canonical_inventory)' \
    'knowledge.begin_connector_snapshot(' \
    'INSERT INTO knowledge.connector_inventory (' \
    'knowledge.complete_connector_snapshot(' \
    'COMMIT;'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${connector_publish_sql}" ||
      fail "connector publish SQL is missing: ${required_sql}"
  done
  if rg --quiet 'knowledge\.(chunks|ingestion_embedding_cache)' <<<"${connector_publish_sql}"; then
    fail "connector publisher touches chunks or embedding state"
  fi
  if rg --fixed-strings --quiet 'connector snapshot has unapplied actions' <<<"${schema_v3_sql}"; then
    fail "newer complete connector inventories cannot preempt stale desired state"
  fi

  for required_sql in \
    '\set ON_ERROR_STOP on' \
    'BEGIN;' \
    'WHERE claim_expires_at <= transaction_timestamp()' \
    'snapshots.enumeration_complete' \
    'snapshots.blocked_at IS NULL' \
    'FOR UPDATE OF sources SKIP LOCKED' \
    'LIMIT 1' \
    "transaction_timestamp() + interval '35 minutes'" \
    '\o /work/connector-action.json' \
    '\o /work/connector-kind' \
    "'action', sources.desired_action" \
    "'source_id', sources.source_id" \
    "'content_digest', sources.desired_digest" \
    "'acl_digest', sources.desired_acl_digest" \
    "'claim_expires_at', selected.claim_expires_at" \
    'sources.applied_snapshot_revision IS DISTINCT FROM sources.desired_snapshot_revision' \
    'sources.applied_inventory_digest IS DISTINCT FROM sources.desired_inventory_digest' \
    'COMMIT;'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${connector_claim_sql}" ||
      fail "connector claim SQL is missing: ${required_sql}"
  done
  connector_claim_limit_count="$(rg --fixed-strings --count 'LIMIT 1' <<<"${connector_claim_sql}")"
  [[ "${connector_claim_limit_count}" -eq 1 ]] ||
    fail "connector claim must select exactly one bounded action"

  for required_sql in \
    '\set ON_ERROR_STOP on' \
    'BEGIN;' \
    'DELETE FROM knowledge.ingestion_leases' \
    'INSERT INTO knowledge.ingestion_leases (name, holder, expires_at)' \
    "'chunks-v1'" \
    'SELECT knowledge.apply_connector_tombstone(' \
    'DELETE FROM knowledge.ingestion_leases' \
    'COMMIT;'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${connector_tombstone_sql}" ||
      fail "connector tombstone SQL is missing: ${required_sql}"
  done
  publish_begin_count="$(rg --fixed-strings --count 'BEGIN;' <<<"${connector_publish_sql}")"
  publish_commit_count="$(rg --fixed-strings --count 'COMMIT;' <<<"${connector_publish_sql}")"
  claim_begin_count="$(rg --fixed-strings --count 'BEGIN;' <<<"${connector_claim_sql}")"
  claim_commit_count="$(rg --fixed-strings --count 'COMMIT;' <<<"${connector_claim_sql}")"
  tombstone_begin_count="$(rg --fixed-strings --count 'BEGIN;' <<<"${connector_tombstone_sql}")"
  tombstone_commit_count="$(rg --fixed-strings --count 'COMMIT;' <<<"${connector_tombstone_sql}")"
  [[ "${publish_begin_count}" -eq 1 ]] &&
    [[ "${publish_commit_count}" -eq 1 ]] &&
    [[ "${claim_begin_count}" -eq 1 ]] &&
    [[ "${claim_commit_count}" -eq 1 ]] &&
    [[ "${tombstone_begin_count}" -eq 1 ]] &&
    [[ "${tombstone_commit_count}" -eq 1 ]] ||
    fail "connector SQL programs must each expose one atomic transaction"

  publish_digest_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'inventory_digest := knowledge.connector_inventory_json_digest' \
      <<<"${connector_publish_sql}" | cut -d: -f1
  )"
  publish_begin_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'PERFORM knowledge.begin_connector_snapshot' <<<"${connector_publish_sql}" | cut -d: -f1
  )"
  publish_insert_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'INSERT INTO knowledge.connector_inventory' <<<"${connector_publish_sql}" | cut -d: -f1
  )"
  publish_complete_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'PERFORM knowledge.complete_connector_snapshot' <<<"${connector_publish_sql}" | cut -d: -f1
  )"
  publish_commit_line="$(
    rg --line-number --fixed-strings --max-count 1 'COMMIT;' \
      <<<"${connector_publish_sql}" | cut -d: -f1
  )"
  if ! ((publish_digest_line < publish_begin_line && \
    publish_begin_line < publish_insert_line && \
    publish_insert_line < publish_complete_line && \
    publish_complete_line < publish_commit_line)); then
    fail "connector publisher does not validate, stage, complete, and commit in order"
  fi

  tombstone_insert_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'INSERT INTO knowledge.ingestion_leases' <<<"${connector_tombstone_sql}" | cut -d: -f1
  )"
  tombstone_apply_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'SELECT knowledge.apply_connector_tombstone' <<<"${connector_tombstone_sql}" | cut -d: -f1
  )"
  tombstone_release_line="$(
    rg --line-number --fixed-strings 'DELETE FROM knowledge.ingestion_leases' \
      <<<"${connector_tombstone_sql}" | tail -n 1 | cut -d: -f1
  )"
  tombstone_commit_line="$(
    rg --line-number --fixed-strings --max-count 1 'COMMIT;' \
      <<<"${connector_tombstone_sql}" | cut -d: -f1
  )"
  if ! ((tombstone_insert_line < tombstone_apply_line && \
    tombstone_apply_line < tombstone_release_line && \
    tombstone_release_line < tombstone_commit_line)); then
    fail "connector tombstone does not delete and advance within the shared lease transaction"
  fi

  for required_sql in \
    'pending chunk set must contain between 1 and 512 rows' \
    'pending chunk set must contain exactly one source' \
    'WHERE expires_at <= transaction_timestamp()' \
    "transaction_timestamp() + interval '35 minutes'" \
    'AND leases.expires_at > transaction_timestamp()' \
    "cache.profile = 'bge-m3-1024-v1'" \
    "cache.source_id = pending.payload #>> '{metadata,source,id}'" \
    "cache.content_sha256 = encode(" \
    "sha256(convert_to(pending.payload->>'content', 'UTF8'))" \
    'cache.expires_at > clock_timestamp()' \
    'embedding cache SHA256 collides with different exact content' \
    'canonical embeddings conflict for exact content' \
    'canonical and checkpoint embeddings conflict for exact content' \
    'LEFT JOIN LATERAL (' \
    "WHERE content_chunk.content = pending.payload->>'content'" \
    "WHEN cache.content = pending.payload->>'content'" \
    'DELETE FROM knowledge.ingestion_embedding_cache' \
    'FROM active_cache' \
    'ranked_cache.retention_rank > 1024'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${plan_sql}" ||
      fail "knowledge ingestion plan SQL is missing: ${required_sql}"
  done

  lease_reclaim_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'DELETE FROM knowledge.ingestion_leases' <<<"${plan_sql}" | cut -d: -f1
  )"
  lease_claim_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'INSERT INTO knowledge.ingestion_leases' <<<"${plan_sql}" | cut -d: -f1
  )"
  pending_cleanup_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'DELETE FROM knowledge.ingestion_pending AS pending' <<<"${plan_sql}" | cut -d: -f1
  )"
  final_cleanup_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'DELETE FROM knowledge.ingestion_final AS final' <<<"${plan_sql}" | cut -d: -f1
  )"
  if ! ((lease_reclaim_line < lease_claim_line && \
    lease_claim_line < pending_cleanup_line && \
    pending_cleanup_line < final_cleanup_line)); then
    fail "knowledge ingestion plan must claim the lease before locking staging receipts"
  fi
  lease_clock_count="$(rg --only-matching 'transaction_timestamp\(\)' <<<"${plan_sql}" | wc -l)"
  [[ "${lease_clock_count}" -eq 4 ]] ||
    fail "knowledge ingestion lease decisions must share exactly one transaction-stable clock"
  pending_delete_count="$(
    rg --fixed-strings --count 'DELETE FROM knowledge.ingestion_pending' <<<"${plan_sql}"
  )"
  [[ "${pending_delete_count}" -eq 2 ]] ||
    fail "knowledge ingestion plan must clean orphan and current-run pending receipts"
  final_delete_count="$(
    rg --fixed-strings --count 'DELETE FROM knowledge.ingestion_final' <<<"${plan_sql}"
  )"
  [[ "${final_delete_count}" -eq 2 ]] ||
    fail "knowledge ingestion plan must clean orphan and current-run final receipts"
  for current_reset in ingestion_pending ingestion_final; do
    rg --multiline --quiet \
      "DELETE FROM knowledge\\.${current_reset}\\nWHERE run_id = :'run_id'::uuid;" \
      <<<"${plan_sql}" ||
      fail "knowledge ingestion plan does not reset current-run ${current_reset}"
  done
  if rg --quiet 'leases\.expires_at > clock_timestamp\(\)' <<<"${plan_sql}"; then
    fail "knowledge ingestion lease liveness must not use a moving wall-clock cutoff"
  fi
  if rg --multiline --quiet \
    'DELETE FROM knowledge\.ingestion_leases\nWHERE expires_at <= clock_timestamp\(\)' \
    <<<"${plan_sql}"; then
    fail "knowledge ingestion lease reclamation must not use a moving wall-clock cutoff"
  fi

  for required_sql in \
    "WHERE name = 'chunks-v1'" \
    "AND holder = current_setting('fgentic.ingestion_run_id')::uuid" \
    "AND expires_at > clock_timestamp()" \
    '\copy knowledge.ingestion_final (payload) FROM PSTDIN' \
    'embedding checkpoint must contain between 1 and 8 rows' \
    "payload - ARRAY['profile', 'content', 'embedding']" \
    "payload->>'profile' <> 'bge-m3-1024-v1'" \
    'embedding checkpoint content is absent from authoritative pending input' \
    'embedding checkpoint SHA256 collides within the final chunk set' \
    'exact content produced conflicting embeddings in one checkpoint' \
    'checkpoint embedding conflicts with canonical exact content' \
    "cache.source_id = pending.payload #>> '{metadata,source,id}'" \
    "INSERT INTO knowledge.ingestion_embedding_cache (" \
    "'bge-m3-1024-v1'" \
    "sha256(convert_to(final.payload->>'content', 'UTF8'))" \
    "stamped.cached_at + interval '24 hours'" \
    'ON CONFLICT (profile, source_id, content_sha256) DO NOTHING' \
    'embedding checkpoint conflicts with cached exact content or vector' \
    'FROM active_cache' \
    'ranked_cache.retention_rank > 1024' \
    'DELETE FROM knowledge.ingestion_final'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${checkpoint_sql}" ||
      fail "knowledge ingestion checkpoint SQL is missing: ${required_sql}"
  done

  for required_sql in \
    "SET LOCAL lock_timeout = '5s'" \
    "SET LOCAL statement_timeout = '30s'" \
    'WHERE expires_at <= clock_timestamp()' \
    'ORDER BY' \
    'expires_at,' \
    'source_id,' \
    'profile,' \
    'content_sha256' \
    'LIMIT 1024' \
    'DELETE FROM knowledge.ingestion_embedding_cache AS cache'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${gc_sql}" ||
      fail "knowledge ingestion cache GC SQL is missing: ${required_sql}"
  done

  for required_sql in \
    "\\copy knowledge.ingestion_final (payload) FROM '/work/chunks.jsonl'" \
    'final chunk set must contain between 1 and 512 rows' \
    'final chunk set must contain exactly one source' \
    'embedding phase changed the authoritative bound chunk set' \
    'embedding cache SHA256 collides with different exact content' \
    'canonical embeddings conflict for exact content' \
    'canonical and checkpoint embeddings conflict for exact content' \
    'final embedding lacks canonical or checkpoint provenance' \
    'DELETE FROM knowledge.ingestion_embedding_cache AS cache' \
    'WITH committed_sources AS (' \
    "cache.profile = 'bge-m3-1024-v1'" \
    "cache.source_id = final.payload #>> '{metadata,source,id}'" \
    'cache.expires_at > clock_timestamp()' \
    "sha256(convert_to(final.payload->>'content', 'UTF8'))" \
    'WHERE cache.source_id = committed_sources.source_id' \
    'DELETE FROM knowledge.ingestion_pending' \
    'DELETE FROM knowledge.ingestion_final' \
    'DELETE FROM knowledge.ingestion_leases'; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${write_sql}" ||
      fail "knowledge ingestion write SQL is missing: ${required_sql}"
  done
  for required_sql in \
    '\if :{?connector_action}' \
    "SELECT knowledge.complete_connector_present(:'run_id'::uuid);"; do
    rg --fixed-strings --quiet "${required_sql}" <<<"${write_sql}" ||
      fail "knowledge ingestion write SQL omits the connector completion hook: ${required_sql}"
  done
  connector_complete_line="$(
    rg --line-number --fixed-strings --max-count 1 \
      'SELECT knowledge.complete_connector_present' <<<"${write_sql}" | cut -d: -f1
  )"
  pending_cleanup_line="$(
    rg --line-number --fixed-strings \
      'DELETE FROM knowledge.ingestion_pending' <<<"${write_sql}" | tail -n 1 | cut -d: -f1
  )"
  final_cleanup_line="$(
    rg --line-number --fixed-strings \
      'DELETE FROM knowledge.ingestion_final' <<<"${write_sql}" | tail -n 1 | cut -d: -f1
  )"
  write_lease_release_line="$(
    rg --line-number --fixed-strings 'DELETE FROM knowledge.ingestion_leases' \
      <<<"${write_sql}" | tail -n 1 | cut -d: -f1
  )"
  write_commit_line="$(
    rg --line-number --fixed-strings --max-count 1 'COMMIT;' \
      <<<"${write_sql}" | cut -d: -f1
  )"
  if ! ((connector_complete_line < pending_cleanup_line && \
    pending_cleanup_line < final_cleanup_line && \
    final_cleanup_line < write_lease_release_line && \
    write_lease_release_line < write_commit_line)); then
    fail "connector source completion must share the final chunk transaction"
  fi

  yq -e '
    .kind == "CronJob" and
    .metadata.name == "knowledge-ingestion" and
    .spec.schedule == "2-59/5 * * * *" and
    .spec.suspend == true and
    .spec.concurrencyPolicy == "Forbid" and
    .spec.startingDeadlineSeconds == 300
  ' "${INGESTION_CRONJOB}" >/dev/null ||
    fail "knowledge ingestion schedule no longer offsets acquisition or drains one action safely"

  yq -e '
    .kind == "Cluster" and .nodes[0].role == "control-plane" and
    (.nodes[0].kubeadmConfigPatches[0] | contains("KubeletInUserNamespace: true"))
  ' "${KIND_CONFIG}" >/dev/null || fail "kind fixture is not safe for constrained/rootless hosts"

  echo "Knowledge store static contract passed"
}

runtime_contract() {
  require_commands docker flux helm id jq kind kubectl od python rg tr yq
  docker info >/dev/null 2>&1 || fail "Docker daemon is not available"

  local chart chart_version repository source namespace primary runtime_render client_pod
  local docling_image host_gid
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
  docling_image="$(
    yq -er '
      select(.kind == "CronJob" and .metadata.name == "knowledge-ingestion") |
      .spec.jobTemplate.spec.template.spec.initContainers[] |
      select(.name == "parse") | .image
    ' "${INGESTION_CRONJOB}"
  )"
  [[ "${docling_image}" == *@sha256:* ]] ||
    fail "knowledge ingestion Docling image is not digest-pinned"
  host_gid="$(id -g)"

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
  kubectl --namespace "${namespace}" create secret generic pg-knowledge-ingestion \
    --type kubernetes.io/basic-auth \
    --from-literal username=knowledge_ingestion \
    --from-literal password="${INGESTION_PASSWORD}" >/dev/null
  kubectl --namespace "${namespace}" create secret generic pg-knowledge-connector \
    --type kubernetes.io/basic-auth \
    --from-literal username=knowledge_connector \
    --from-literal password="${CONNECTOR_PASSWORD}" >/dev/null
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
      "hostssl knowledge knowledge_ingestion all scram-sha-256",
      "hostssl knowledge knowledge_connector all scram-sha-256",
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
  kubectl kustomize "${INGESTION_FIXTURE}" \
    --load-restrictor LoadRestrictionsNone >"${runtime_render}"
  yq eval-all '
    select(
      (.kind == "DatabaseRole" and
        (.metadata.name == "knowledge-owner" or
          .metadata.name == "knowledge-ingestion" or
          .metadata.name == "knowledge-connector" or
          .metadata.name == "knowledge-retrieval")) or
      (.kind == "Database" and .metadata.name == "knowledge") or
      (.kind == "ConfigMap" and
        (.metadata.name == "knowledge-schema-v1" or
          .metadata.name == "knowledge-schema-v2" or
          .metadata.name == "knowledge-schema-v3")) or
      (.kind == "Job" and
        (.metadata.name == "knowledge-schema-v1" or
          .metadata.name == "knowledge-schema-v2" or
          .metadata.name == "knowledge-schema-v3")) or
      (.kind == "NetworkPolicy" and
        (.metadata.name == "knowledge-schema-v1" or
          .metadata.name == "knowledge-schema-v2" or
          .metadata.name == "knowledge-schema-v3"))
    )
  ' "${runtime_render}" >"${RUNTIME_WORKDIR}/knowledge.raw.yaml"
  flux envsubst --strict <"${RUNTIME_WORKDIR}/knowledge.raw.yaml" \
    >"${RUNTIME_WORKDIR}/knowledge.yaml"

  echo "==> Reconciling declarative roles, database, vector extension, and schema migrations"
  kubectl apply --filename "${RUNTIME_WORKDIR}/knowledge.yaml" >/dev/null
  if ! kubectl --namespace "${namespace}" wait job/knowledge-schema-v1 \
    --for=condition=Complete --timeout=8m >/dev/null; then
    kubectl --namespace "${namespace}" get cluster,databaserole,database,job,pod >&2 || true
    kubectl --namespace "${namespace}" logs job/knowledge-schema-v1 >&2 || true
    fail "knowledge schema Job did not complete"
  fi
  if ! kubectl --namespace "${namespace}" wait job/knowledge-schema-v2 \
    --for=condition=Complete --timeout=8m >/dev/null; then
    kubectl --namespace "${namespace}" get cluster,databaserole,database,job,pod >&2 || true
    kubectl --namespace "${namespace}" logs job/knowledge-schema-v2 >&2 || true
    fail "knowledge schema v2 Job did not complete"
  fi
  if ! kubectl --namespace "${namespace}" wait job/knowledge-schema-v3 \
    --for=condition=Complete --timeout=8m >/dev/null; then
    kubectl --namespace "${namespace}" get cluster,databaserole,database,job,pod >&2 || true
    kubectl --namespace "${namespace}" logs job/knowledge-schema-v3 >&2 || true
    fail "knowledge schema v3 Job did not complete"
  fi

  primary="$(kubectl --namespace "${namespace}" get cluster platform-pg \
    --output=jsonpath='{.status.currentPrimary}')"
  [[ -n "${primary}" ]] || fail "CNPG did not report a primary instance"
  client_pod="knowledge-ingestion-sql-client"

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
  ingestion_sql() {
    kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
      env PGPASSWORD="${INGESTION_PASSWORD}" \
      psql --no-psqlrc --set=ON_ERROR_STOP=1 \
      --dbname='host=127.0.0.1 dbname=knowledge user=knowledge_ingestion sslmode=require' "$@"
  }
  connector_sql() {
    kubectl --namespace "${namespace}" exec --stdin "pod/${primary}" --container postgres -- \
      env PGPASSWORD="${CONNECTOR_PASSWORD}" \
      psql --no-psqlrc --set=ON_ERROR_STOP=1 \
      --dbname='host=127.0.0.1 dbname=knowledge user=knowledge_connector sslmode=require' "$@"
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

  kubectl --namespace "${namespace}" create configmap knowledge-ingestion-sql-runtime \
    --from-file="checkpoint.sql=${INGESTION_CHECKPOINT_SQL}" \
    --from-file="gc.sql=${INGESTION_GC_SQL}" \
    --from-file="plan.sql=${INGESTION_PLAN_SQL}" \
    --from-file="write.sql=${INGESTION_WRITE_SQL}" \
    --from-file="connector-publish.sql=${CONNECTOR_PUBLISH_SQL}" \
    --from-file="connector-claim.sql=${CONNECTOR_CLAIM_SQL}" \
    --from-file="connector-tombstone.sql=${CONNECTOR_TOMBSTONE_SQL}" \
    --dry-run=client \
    --output=yaml |
    kubectl apply --filename=- >/dev/null
  kubectl apply --filename=- >/dev/null <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: ${client_pod}
  namespace: ${namespace}
  labels:
    app.kubernetes.io/name: knowledge-ingestion-sql-client
    app.kubernetes.io/component: runtime-test
    app.kubernetes.io/part-of: fgentic
spec:
  automountServiceAccountToken: false
  enableServiceLinks: false
  restartPolicy: Never
  securityContext:
    fsGroup: 2000
    fsGroupChangePolicy: OnRootMismatch
    runAsNonRoot: true
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: psql
      image: ${POSTGRES_IMAGE}
      imagePullPolicy: IfNotPresent
      command:
        - /bin/sh
        - -ceu
        - --
      args:
        - exec sleep 86400
      env:
        - name: PGHOST
          value: platform-pg-rw.postgres.svc.cluster.local
        - name: PGPORT
          value: "5432"
        - name: PGDATABASE
          value: knowledge
        - name: PGUSER
          valueFrom:
            secretKeyRef:
              name: pg-knowledge-ingestion
              key: username
        - name: PGPASSWORD
          valueFrom:
            secretKeyRef:
              name: pg-knowledge-ingestion
              key: password
        - name: PGSSLMODE
          value: require
        - name: PGCONNECT_TIMEOUT
          value: "3"
        - name: HOME
          value: /tmp
      resources:
        requests:
          cpu: 10m
          memory: 32Mi
          ephemeral-storage: 8Mi
        limits:
          cpu: 100m
          memory: 64Mi
          ephemeral-storage: 32Mi
      securityContext:
        allowPrivilegeEscalation: false
        capabilities:
          drop:
            - ALL
        readOnlyRootFilesystem: true
      volumeMounts:
        - name: runtime
          mountPath: /runtime
          readOnly: true
        - name: work
          mountPath: /work
        - name: work
          mountPath: /sources
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: runtime
      configMap:
        name: knowledge-ingestion-sql-runtime
        defaultMode: 0444
    - name: work
      emptyDir:
        sizeLimit: 32Mi
    - name: tmp
      emptyDir:
        sizeLimit: 8Mi
YAML
  if ! kubectl --namespace "${namespace}" wait "pod/${client_pod}" \
    --for=condition=Ready --timeout=2m >/dev/null; then
    kubectl --namespace "${namespace}" describe "pod/${client_pod}" >&2 || true
    kubectl --namespace "${namespace}" logs "pod/${client_pod}" --container psql >&2 || true
    fail "restricted knowledge-ingestion SQL client did not become ready"
  fi

  ingestion_client_exec() {
    kubectl --namespace "${namespace}" exec "pod/${client_pod}" --container psql -- "$@"
  }
  ingestion_client_write() {
    local destination="$1"
    kubectl --namespace "${namespace}" exec --stdin "pod/${client_pod}" --container psql -- \
      tee "${destination}" >/dev/null
  }
  ingestion_client_plan() {
    local run_id="$1"
    ingestion_client_exec psql --quiet --no-psqlrc \
      --set="run_id=${run_id}" --file=/runtime/plan.sql
  }
  ingestion_client_checkpoint() {
    local run_id="$1"
    kubectl --namespace "${namespace}" exec --stdin "pod/${client_pod}" --container psql -- \
      psql --quiet --no-psqlrc \
      --set="run_id=${run_id}" --file=/runtime/checkpoint.sql
  }
  ingestion_client_checkpoint_file() {
    local run_id="$1"
    local source="${2:-/work/checkpoint.ready}"
    ingestion_client_exec /bin/sh -ceu \
      "exec psql --quiet --no-psqlrc --set=\"run_id=\$1\" \\
        --file=/runtime/checkpoint.sql <\"\$2\"" \
      -- "${run_id}" "${source}"
  }
  ingestion_client_gc() {
    ingestion_client_exec psql --quiet --no-psqlrc --file=/runtime/gc.sql
  }
  ingestion_client_commit() {
    local run_id="$1"
    ingestion_client_exec psql --quiet --no-psqlrc \
      --set="run_id=${run_id}" --file=/runtime/write.sql
  }
  connector_client_publish() {
    ingestion_client_exec env \
      PGUSER=knowledge_connector \
      "PGPASSWORD=${CONNECTOR_PASSWORD}" \
      psql --quiet --no-psqlrc --file=/runtime/connector-publish.sql
  }
  connector_client_claim() {
    local run_id="$1"
    ingestion_client_exec psql --quiet --no-psqlrc \
      --set="run_id=${run_id}" --file=/runtime/connector-claim.sql
  }
  connector_client_commit() {
    local run_id="$1"
    ingestion_client_exec psql --quiet --no-psqlrc \
      --set="run_id=${run_id}" --set=connector_action=true \
      --file=/runtime/write.sql
  }
  connector_client_tombstone() {
    local run_id="$1"
    ingestion_client_exec psql --quiet --no-psqlrc \
      --set="run_id=${run_id}" --file=/runtime/connector-tombstone.sql
  }
  ingestion_client_query() {
    local query="$1"
    ingestion_client_exec psql --quiet --no-psqlrc --tuples-only --no-align \
      --set=ON_ERROR_STOP=1 --command="${query}"
  }
  ingestion_client_copy_plan() {
    ingestion_client_exec /bin/sh -ceu \
      'cp /work/plan.jsonl /work/chunks.jsonl; chmod 0640 /work/chunks.jsonl'
  }
  knowledge_pending_record() {
    local chunk_id="$1" content="$2" classification="$3" principal="$4"
    local source_id="${5:-runtime-sql/source}"
    jq -cn \
      --arg chunk_id "${chunk_id}" \
      --arg content "${content}" \
      --arg classification "${classification}" \
      --arg principal "${principal}" \
      --arg source_id "${source_id}" \
      '{
        chunk_id: $chunk_id,
        content: $content,
        metadata: {
          source: {
            id: $source_id,
            title: "Exact SQL lifecycle fixture",
            locator: "test:knowledge-ingestion/runtime",
            revision: "runtime-v1",
            location: "section-1"
          },
          classification: $classification,
          allowed_principals: [{kind: "matrix", principal: $principal}],
          allowed_groups: []
        }
      }'
  }
  knowledge_final_record() {
    local record axis="$5"
    local source_id="${6:-runtime-sql/source}"
    record="$(knowledge_pending_record "$1" "$2" "$3" "$4" "${source_id}")"
    jq -cn \
      --argjson record "${record}" \
      --argjson axis "${axis}" \
      '$record + {
        embedding: [range(0; 1024) | if . == $axis then 1 else 0 end]
      }'
  }
  knowledge_checkpoint_record() {
    local content="$1" axis="$2"
    jq -cn \
      --arg content "${content}" \
      --argjson axis "${axis}" \
      '{
        profile: "bge-m3-1024-v1",
        content: $content,
        embedding: [range(0; 1024) | if . == $axis then 1 else 0 end]
      }'
  }
  connector_acl_digest() {
    local metadata="$1"
    connector_sql --quiet --tuples-only --no-align \
      --set="metadata=${metadata}" <<'SQL'
WITH input AS (SELECT :'metadata'::jsonb AS metadata)
SELECT knowledge.connector_inventory_json_digest(
  jsonb_build_object(
    'classification', metadata->'classification',
    'allowed_principals', metadata->'allowed_principals',
    'allowed_groups', metadata->'allowed_groups'
  )
)
FROM input;
SQL
  }
  connector_content_digest() {
    CONNECTOR_CONTENT="$1" python -c '
import hashlib
import os

print("sha256:" + hashlib.sha256(os.environ["CONNECTOR_CONTENT"].encode()).hexdigest())
'
  }
  connector_inventory_digest() {
    local inventory="$1"
    connector_sql --quiet --tuples-only --no-align \
      --set="inventory=${inventory}" <<'SQL'
SELECT knowledge.connector_inventory_json_digest(:'inventory'::jsonb);
SQL
  }
  connector_inventory_digest_python() {
    CONNECTOR_INVENTORY="$1" python -c '
import hashlib
import json
import os

value = json.loads(os.environ["CONNECTOR_INVENTORY"])
canonical = json.dumps(
    value,
    ensure_ascii=False,
    sort_keys=True,
    separators=(",", ":"),
).encode()
print("sha256:" + hashlib.sha256(canonical).hexdigest())
'
  }
  connector_snapshot() {
    local revision="$1" artifact_digest="$2" inventory="$3" inventory_digest
    inventory_digest="$(connector_inventory_digest "${inventory}")"
    jq -cn \
      --arg revision "${revision}" \
      --arg artifact_digest "${artifact_digest}" \
      --arg inventory_digest "${inventory_digest}" \
      --argjson inventory "${inventory}" '
        {
          connector_id: "git-markdown",
          snapshot_revision: $revision,
          artifact_digest: $artifact_digest,
          inventory_digest: $inventory_digest,
          source_count: ($inventory | length),
          sources: ($inventory | map(. + {
            connector_id: "git-markdown",
            snapshot_revision: $revision,
            inventory_digest: $inventory_digest
          }))
        }
      '
  }
  connector_publish_snapshot() {
    local snapshot="$1"
    ingestion_client_exec mkdir -p /sources/.connector/git-markdown
    ingestion_client_write /sources/.connector/git-markdown/current.json <<<"${snapshot}"
    connector_client_publish
  }
  connector_pending_record() {
    local action="$1" chunk_id="$2" content="$3"
    jq -cn \
      --argjson action "${action}" \
      --arg chunk_id "${chunk_id}" \
      --arg content "${content}" '
        {
          chunk_id: $chunk_id,
          content: $content,
          metadata: ($action.metadata | .source.location = "chunk:000001")
        }
      '
  }
  connector_final_record() {
    local record axis="$4"
    record="$(connector_pending_record "$1" "$2" "$3")"
    jq -cn \
      --argjson record "${record}" \
      --argjson axis "${axis}" '
        $record + {
          embedding: [range(0; 1024) | if . == $axis then 1 else 0 end]
        }
      '
  }
  assert_ingestion_staging_empty() {
    local counts
    counts="$(ingestion_client_query "
      SELECT
        (SELECT count(*) FROM knowledge.ingestion_leases) || '|' ||
        (SELECT count(*) FROM knowledge.ingestion_pending) || '|' ||
        (SELECT count(*) FROM knowledge.ingestion_final)
    ")"
    [[ "${counts}" == "0|0|0" ]] ||
      fail "knowledge ingestion staging was not empty: ${counts}"
  }
  assert_embedding_cache_count() {
    local expected="$1"
    local actual_count
    actual_count="$(ingestion_client_query "
      SELECT count(*) FROM knowledge.ingestion_embedding_cache
    ")"
    [[ "${actual_count}" == "${expected}" ]] ||
      fail "knowledge ingestion embedding cache count was ${actual_count}, expected ${expected}"
  }
  assert_prefilter_plan() {
    local plan="$1" index="$2"
    jq -e --arg index "${index}" '
      any(.. | objects; .["Index Name"]? == $index) and
      any(.. | objects; .["Node Type"]? == "CTE Scan") and
      all(.. | objects; .["Index Name"]? != "chunks_embedding_hnsw_idx")
    ' <<<"${plan}" >/dev/null || fail "materialized prefilter did not use ${index}"
  }
  exercise_docling_sample() {
    local label="$1"
    local example="$2"
    local document_key="$3"
    local first_run="$4"
    local rerun="$5"
    local axis="$6"
    local sample_root input_root raw_root source_root manifest_path pending_path
    local plan_path final_path checkpoint_path source_id expected_count persisted_rows
    local first_xmins rerun_xmins remaining_count

    sample_root="${RUNTIME_WORKDIR}/docling-${label}"
    input_root="${sample_root}/input"
    raw_root="${sample_root}/raw"
    source_root="${sample_root}/sources"
    manifest_path="${sample_root}/manifest.json"
    pending_path="${sample_root}/pending.jsonl"
    plan_path="${sample_root}/plan.jsonl"
    final_path="${sample_root}/chunks.jsonl"
    checkpoint_path="${sample_root}/checkpoint.jsonl"
    mkdir -p "${input_root}" "${raw_root}" "${source_root}"
    chmod 2770 "${raw_root}"

    yq -o=json '.data."manifest.json"' "${example}" |
      jq -j '.' >"${manifest_path}"
    DOCUMENT_KEY="${document_key}" yq -o=json '.data[strenv(DOCUMENT_KEY)]' "${example}" |
      jq -j '.' >"${source_root}/${document_key}"
    cp "${source_root}/${document_key}" "${input_root}/document.${document_key##*.}"

    echo "==> Parsing the ${label} sample with the pinned offline Docling image"
    docker run --rm \
      --network none \
      --read-only \
      --cap-drop ALL \
      --security-opt no-new-privileges \
      --user 1001:0 \
      --group-add "${host_gid}" \
      --pids-limit 512 \
      --cpus 2 \
      --memory 3g \
      --env HOME=/tmp \
      --env TMPDIR=/tmp \
      --env PYTHONDONTWRITEBYTECODE=1 \
      --env PYTHONUNBUFFERED=1 \
      --env HF_HUB_OFFLINE=1 \
      --env TRANSFORMERS_OFFLINE=1 \
      --env DOCLING_ARTIFACTS_PATH=/opt/app-root/src/.cache/docling/models \
      --env OMP_NUM_THREADS=2 \
      --env DO_NOT_TRACK=1 \
      --mount "type=bind,src=${ROOT_DIR}/infra/knowledge/base,dst=/runtime,readonly" \
      --mount "type=bind,src=${input_root},dst=/input,readonly" \
      --mount "type=bind,src=${raw_root},dst=/output" \
      --tmpfs /tmp:rw,noexec,nosuid,nodev,size=268435456,uid=1001,gid=0,mode=1770 \
      "${docling_image}" \
      python /runtime/ingestion.py parse-isolated \
      --source-root /input \
      --output /output/chunks.jsonl

    PYTHONDONTWRITEBYTECODE=1 python "${INGESTION_SCRIPT}" bind \
      --manifest "${manifest_path}" \
      --source-root "${source_root}" \
      --raw-root "${raw_root}" \
      --output "${pending_path}"

    case "${label}" in
    matrix)
      source_id="reference-docs/matrix-principal"
      jq -se '
        length > 0 and
        all(.[];
          (keys | sort) == ["chunk_id", "content", "metadata"] and
          .metadata.source.id == "reference-docs/matrix-principal" and
          .metadata.source.title == "Matrix access example" and
          .metadata.source.locator == "git:docs/examples/matrix-principal.md" and
          .metadata.source.revision == "sha256:matrix-principal-v1" and
          (.metadata.source.location | test("^chunk:[0-9]{6}$")) and
          .metadata.classification == "approved_non_public" and
          .metadata.allowed_principals == [{
            "kind": "matrix",
            "principal": "@alice:org-a.example"
          }] and
          .metadata.allowed_groups == []) and
        any(.[];
          .content |
          contains("complete typed Matrix audience declared in the manifest"))
      ' "${pending_path}" >/dev/null ||
        fail "real Docling Matrix sample lost content, source provenance, or typed ACL"
      ;;
    partner)
      source_id="reference-docs/partner-group"
      jq -se '
        length > 0 and
        all(.[];
          (keys | sort) == ["chunk_id", "content", "metadata"] and
          .metadata.source.id == "reference-docs/partner-group" and
          .metadata.source.title == "Partner access example" and
          .metadata.source.locator == "git:docs/examples/partner-group.md" and
          .metadata.source.revision == "sha256:partner-group-v1" and
          (.metadata.source.location | test("^chunk:[0-9]{6}$")) and
          .metadata.classification == "public" and
          .metadata.allowed_principals == [] and
          .metadata.allowed_groups == ["partner/org-b-a2a/product"]) and
        any(.[];
          .content |
          contains("exact namespaced partner group declared in the manifest"))
      ' "${pending_path}" >/dev/null ||
        fail "real Docling partner sample lost content, source provenance, or exact group ACL"
      ;;
    *)
      fail "unsupported real Docling sample: ${label}"
      ;;
    esac

    expected_count="$(jq -s 'length' "${pending_path}")"
    ingestion_client_write /work/pending.jsonl <"${pending_path}"
    ingestion_client_plan "${first_run}" >/dev/null
    ingestion_client_exec cat /work/plan.jsonl >"${plan_path}"
    jq -se \
      --argjson expected_count "${expected_count}" \
      --slurpfile pending "${pending_path}" '
        length == $expected_count and
        map(del(.embedding)) == $pending and
        all(.[]; .embedding == null)
      ' "${plan_path}" >/dev/null ||
      fail "first ${label} sample plan changed the bound set or reused an embedding"

    jq -c --argjson axis "${axis}" '
      . + {
        embedding: [range(0; 1024) | if . == $axis then 1 else 0 end]
      }
    ' "${plan_path}" >"${final_path}"
    jq -cs '
      unique_by(.content)[] |
      {
        profile: "bge-m3-1024-v1",
        content,
        embedding
      }
    ' "${final_path}" >"${checkpoint_path}"
    while IFS= read -r checkpoint; do
      ingestion_client_checkpoint "${first_run}" <<<"${checkpoint}"
    done <"${checkpoint_path}"
    ingestion_client_write /work/chunks.jsonl <"${final_path}"
    ingestion_client_commit "${first_run}" >/dev/null
    assert_ingestion_staging_empty
    assert_embedding_cache_count 0

    persisted_rows="$(ingestion_client_query "
      SELECT jsonb_build_object(
        'chunk_id', chunk_id,
        'content', content,
        'metadata', metadata,
        'embedding', (embedding::text)::jsonb
      )::text
      FROM knowledge.chunks
      WHERE metadata #>> '{source,id}' = '${source_id}'
      ORDER BY chunk_id
    ")"
    jq -se \
      --argjson expected_count "${expected_count}" \
      --slurpfile pending "${pending_path}" '
        length == $expected_count and
        map({
          chunk_id,
          content,
          metadata
        }) == $pending and
        all(.[];
          (.embedding | type) == "array" and
          (.embedding | length) == 1024 and
          any(.embedding[]; . != 0))
      ' <<<"${persisted_rows}" >/dev/null ||
      fail "real ${label} sample did not land with exact metadata and a valid embedding"
    first_xmins="$(ingestion_client_query "
      SELECT string_agg(chunk_id || ':' || xmin::text, ',' ORDER BY chunk_id)
      FROM knowledge.chunks
      WHERE metadata #>> '{source,id}' = '${source_id}'
    ")"
    [[ -n "${first_xmins}" ]] || fail "real ${label} sample wrote no knowledge rows"

    ingestion_client_write /work/pending.jsonl <"${pending_path}"
    ingestion_client_plan "${rerun}" >/dev/null
    ingestion_client_exec cat /work/plan.jsonl >"${plan_path}"
    jq -se \
      --argjson expected_count "${expected_count}" \
      --slurpfile pending "${pending_path}" '
        length == $expected_count and
        map(del(.embedding)) == $pending and
        all(.[];
          (.embedding | type) == "array" and
          (.embedding | length) == 1024 and
          any(.embedding[]; . != 0))
      ' "${plan_path}" >/dev/null ||
      fail "real ${label} rerun did not produce a fully populated zero-call plan"
    ingestion_client_copy_plan
    ingestion_client_commit "${rerun}" >/dev/null
    assert_ingestion_staging_empty
    assert_embedding_cache_count 0
    rerun_xmins="$(ingestion_client_query "
      SELECT string_agg(chunk_id || ':' || xmin::text, ',' ORDER BY chunk_id)
      FROM knowledge.chunks
      WHERE metadata #>> '{source,id}' = '${source_id}'
    ")"
    [[ "${rerun_xmins}" == "${first_xmins}" ]] ||
      fail "real ${label} sample rerun rewrote unchanged chunks"

    ingestion_sql --quiet --command="
      DELETE FROM knowledge.chunks
      WHERE metadata #>> '{source,id}' = '${source_id}'
    " >/dev/null
    remaining_count="$(ingestion_client_query "
      SELECT count(*) FROM knowledge.chunks
      WHERE metadata #>> '{source,id}' = '${source_id}'
    ")"
    [[ "${remaining_count}" == "0" ]] || fail "real ${label} sample cleanup left knowledge rows"
    assert_ingestion_staging_empty
    assert_embedding_cache_count 0
  }

  local actual
  actual="$(admin_sql postgres --tuples-only --no-align --command="
    SELECT count(*) = 4
    FROM pg_roles
    WHERE rolname IN (
      'knowledge_owner', 'knowledge_ingestion', 'knowledge_connector', 'knowledge_retrieval'
    )
      AND rolcanlogin AND NOT rolinherit AND NOT rolsuper AND NOT rolcreatedb
      AND NOT rolcreaterole AND NOT rolreplication AND NOT rolbypassrls
      AND ((rolname = 'knowledge_owner' AND rolconnlimit = 4)
        OR (rolname = 'knowledge_ingestion' AND rolconnlimit = 4)
        OR (rolname = 'knowledge_connector' AND rolconnlimit = 2)
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
      AND to_regclass('knowledge.ingestion_embedding_cache') IS NOT NULL
      AND to_regclass('knowledge.connector_snapshots') IS NOT NULL
      AND to_regclass('knowledge.connector_inventory') IS NOT NULL
      AND to_regclass('knowledge.connector_sources') IS NOT NULL
      AND (
        SELECT count(*) = 7
        FROM pg_constraint
        WHERE conrelid = 'knowledge.ingestion_embedding_cache'::regclass
          AND conname IN (
            'ingestion_embedding_cache_pkey',
            'ingestion_embedding_cache_profile_fixed',
            'ingestion_embedding_cache_source_bounded',
            'ingestion_embedding_cache_content_bounded',
            'ingestion_embedding_cache_sha256_exact',
            'ingestion_embedding_cache_ttl_bounded',
            'ingestion_embedding_cache_nonzero'
          )
      )
      AND to_regprocedure(
        'knowledge.search_authorized_matrix(vector,text[],jsonb,integer)'
      ) IS NOT NULL
      AND to_regprocedure(
        'knowledge.search_authorized_groups(vector,text[],text[],integer)'
      ) IS NOT NULL
      AND to_regprocedure('knowledge.connector_inventory_json_digest(jsonb)') IS NOT NULL
      AND to_regprocedure(
        'knowledge.begin_connector_snapshot(text,text,text,integer)'
      ) IS NOT NULL
      AND to_regprocedure(
        'knowledge.complete_connector_snapshot(text,text,text)'
      ) IS NOT NULL
      AND to_regprocedure('knowledge.complete_connector_present(uuid)') IS NOT NULL
      AND to_regprocedure('knowledge.apply_connector_tombstone(uuid)') IS NOT NULL
      AND (SELECT count(*) = 5 FROM pg_indexes
        WHERE schemaname = 'knowledge' AND indexname IN (
		  'chunks_classification_idx', 'chunks_principals_gin_idx',
		  'chunks_groups_gin_idx', 'chunks_source_id_idx', 'chunks_embedding_hnsw_idx'
        ))
      AND (SELECT array_agg(version ORDER BY version) = ARRAY[1, 2, 3]
        FROM knowledge.schema_migrations)
      AND has_database_privilege('knowledge_connector', 'knowledge', 'CONNECT')
      AND has_database_privilege('knowledge_connector', 'knowledge', 'TEMPORARY')
      AND has_schema_privilege('knowledge_connector', 'knowledge', 'USAGE')
      AND NOT has_schema_privilege('knowledge_connector', 'knowledge', 'CREATE')
      AND has_table_privilege(
        'knowledge_connector', 'knowledge.connector_snapshots', 'SELECT'
      )
      AND NOT has_table_privilege(
        'knowledge_connector', 'knowledge.connector_snapshots', 'INSERT'
      )
      AND has_table_privilege(
        'knowledge_connector', 'knowledge.connector_inventory', 'INSERT'
      )
      AND NOT has_table_privilege(
        'knowledge_connector', 'knowledge.connector_inventory', 'SELECT'
      )
      AND NOT has_table_privilege(
        'knowledge_connector', 'knowledge.connector_inventory', 'UPDATE'
      )
      AND NOT has_table_privilege(
        'knowledge_connector', 'knowledge.connector_inventory', 'DELETE'
      )
      AND NOT has_table_privilege(
        'knowledge_connector', 'knowledge.connector_sources', 'SELECT'
      )
      AND NOT has_table_privilege('knowledge_connector', 'knowledge.chunks', 'SELECT')
      AND NOT has_table_privilege(
        'knowledge_connector', 'knowledge.ingestion_embedding_cache', 'SELECT'
      )
      AND has_function_privilege(
        'knowledge_connector', 'knowledge.is_full_mxid(text)', 'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector',
        'knowledge.is_valid_principal_array(jsonb,integer)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector',
        'knowledge.is_valid_group_array(jsonb,integer)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector', 'knowledge.is_valid_metadata(jsonb)', 'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector', 'knowledge.canonical_jsonb_text(jsonb)', 'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector',
        'knowledge.connector_inventory_json_digest(jsonb)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector',
        'knowledge.begin_connector_snapshot(text,text,text,integer)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_connector',
        'knowledge.complete_connector_snapshot(text,text,text)',
        'EXECUTE'
      )
      AND NOT has_function_privilege(
        'knowledge_connector',
        'knowledge.connector_inventory_digest(text,text,text)',
        'EXECUTE'
      )
      AND NOT has_function_privilege(
        'knowledge_connector', 'knowledge.complete_connector_present(uuid)', 'EXECUTE'
      )
      AND NOT has_function_privilege(
        'knowledge_connector', 'knowledge.apply_connector_tombstone(uuid)', 'EXECUTE'
      )
      AND NOT has_table_privilege('knowledge_retrieval', 'knowledge.chunks', 'INSERT')
      AND has_table_privilege('knowledge_retrieval', 'knowledge.chunks', 'SELECT')
      AND NOT has_table_privilege(
        'knowledge_retrieval', 'knowledge.connector_snapshots', 'SELECT'
      )
      AND has_database_privilege('knowledge_ingestion', 'knowledge', 'CONNECT')
      AND NOT has_database_privilege('knowledge_ingestion', 'knowledge', 'TEMPORARY')
      AND has_schema_privilege('knowledge_ingestion', 'public', 'USAGE')
      AND NOT has_schema_privilege('knowledge_ingestion', 'public', 'CREATE')
      AND has_schema_privilege('knowledge_ingestion', 'knowledge', 'USAGE')
      AND NOT has_schema_privilege('knowledge_ingestion', 'knowledge', 'CREATE')
      AND has_type_privilege('knowledge_ingestion', 'public.vector', 'USAGE')
      AND has_function_privilege(
        'knowledge_ingestion',
        'public.vector_norm(vector)',
        'EXECUTE'
      )
      AND has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'SELECT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'INSERT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'UPDATE')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'DELETE')
      AND NOT has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'TRUNCATE')
      AND NOT has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'REFERENCES')
      AND NOT has_table_privilege('knowledge_ingestion', 'knowledge.chunks', 'TRIGGER')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_leases', 'SELECT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_leases', 'INSERT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_leases', 'UPDATE')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_leases', 'DELETE')
      AND NOT has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_leases', 'TRUNCATE')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_pending', 'SELECT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_pending', 'INSERT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_pending', 'DELETE')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_final', 'SELECT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_final', 'INSERT')
      AND has_table_privilege('knowledge_ingestion', 'knowledge.ingestion_final', 'DELETE')
      AND has_table_privilege(
        'knowledge_ingestion',
        'knowledge.ingestion_embedding_cache',
        'SELECT'
      )
      AND has_table_privilege(
        'knowledge_ingestion',
        'knowledge.ingestion_embedding_cache',
        'INSERT'
      )
      AND has_table_privilege(
        'knowledge_ingestion',
        'knowledge.ingestion_embedding_cache',
        'DELETE'
      )
      AND NOT has_table_privilege(
        'knowledge_ingestion',
        'knowledge.ingestion_embedding_cache',
        'UPDATE'
      )
      AND NOT has_table_privilege(
        'knowledge_ingestion',
        'knowledge.ingestion_embedding_cache',
        'TRUNCATE'
      )
      AND NOT has_table_privilege(
        'knowledge_ingestion',
        'knowledge.schema_migrations',
        'SELECT'
      )
      AND has_function_privilege(
        'knowledge_ingestion',
        'knowledge.is_dns1123_label(text)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_ingestion',
        'knowledge.is_bounded_clean_text(text,integer)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_ingestion',
        'knowledge.is_full_mxid(text)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_ingestion',
        'knowledge.is_valid_principal_array(jsonb,integer)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_ingestion',
        'knowledge.is_valid_group_array(jsonb,integer)',
        'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_ingestion',
        'knowledge.is_valid_metadata(jsonb)',
        'EXECUTE'
      )
      AND NOT has_function_privilege(
        'knowledge_ingestion',
        'knowledge.search_authorized_matrix(vector,text[],jsonb,integer)',
        'EXECUTE'
      )
      AND NOT has_function_privilege(
        'knowledge_ingestion',
        'knowledge.search_authorized_groups(vector,text[],text[],integer)',
        'EXECUTE'
      )
      AND has_table_privilege(
        'knowledge_ingestion', 'knowledge.connector_snapshots', 'SELECT'
      )
      AND has_table_privilege(
        'knowledge_ingestion', 'knowledge.connector_inventory', 'SELECT'
      )
      AND NOT has_table_privilege(
        'knowledge_ingestion', 'knowledge.connector_inventory', 'INSERT'
      )
      AND has_table_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'SELECT'
      )
      AND NOT has_table_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'UPDATE'
      )
      AND has_column_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'claim_holder', 'UPDATE'
      )
      AND has_column_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'claimed_at', 'UPDATE'
      )
      AND has_column_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'claim_expires_at', 'UPDATE'
      )
      AND NOT has_column_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'desired_action', 'UPDATE'
      )
      AND NOT has_column_privilege(
        'knowledge_ingestion', 'knowledge.connector_sources', 'applied_action', 'UPDATE'
      )
      AND has_function_privilege(
        'knowledge_ingestion', 'knowledge.complete_connector_present(uuid)', 'EXECUTE'
      )
      AND has_function_privilege(
        'knowledge_ingestion', 'knowledge.apply_connector_tombstone(uuid)', 'EXECUTE'
      )
      AND NOT has_function_privilege(
        'knowledge_ingestion',
        'knowledge.begin_connector_snapshot(text,text,text,integer)',
        'EXECUTE'
      )
  ")"
  [[ "${actual}" == "t" ]] || fail "runtime database, extension, schema, index, or grant drifted"

  echo "==> Exercising the exact knowledge-ingestion SQL lifecycle"
  local run_first run_rerun run_acl run_changed run_mismatch run_competing run_reclaim
  local run_failed run_conflict run_unrelated run_resume run_multi_source run_cross run_batch
  local run_cache_scope run_cache_expiry run_cache_bound run_lock_old run_lock_new run_same
  local chunk_first chunk_changed chunk_candidate chunk_mismatch chunk_failed chunk_unrelated
  local chunk_deferred chunk_multi_a chunk_multi_b chunk_cross chunk_batch_a chunk_batch_b
  local chunk_cache_scope chunk_cache_expiry chunk_cache_bound chunk_lock_old chunk_same_old
  local framed_content changed_content checkpoint_content unrelated_content deferred_content
  local batch_content_a batch_content_b cache_scope_content
  local expected_content_hex plan_json
  local first_xmin rerun_xmin changed_xmin reclaimed_xmin
  local lock_writer_pid lock_plan_pid lock_writer_log lock_plan_log
  run_first="00000000-0000-4000-8000-000000000001"
  run_rerun="00000000-0000-4000-8000-000000000002"
  run_acl="00000000-0000-4000-8000-000000000003"
  run_changed="00000000-0000-4000-8000-000000000004"
  run_mismatch="00000000-0000-4000-8000-000000000005"
  run_competing="00000000-0000-4000-8000-000000000006"
  run_reclaim="00000000-0000-4000-8000-000000000007"
  run_failed="00000000-0000-4000-8000-000000000008"
  run_conflict="00000000-0000-4000-8000-000000000009"
  run_unrelated="00000000-0000-4000-8000-000000000010"
  run_resume="00000000-0000-4000-8000-000000000011"
  run_multi_source="00000000-0000-4000-8000-000000000012"
  run_cross="00000000-0000-4000-8000-000000000013"
  run_batch="00000000-0000-4000-8000-000000000014"
  run_cache_scope="00000000-0000-4000-8000-000000000015"
  run_cache_expiry="00000000-0000-4000-8000-000000000016"
  run_cache_bound="00000000-0000-4000-8000-000000000017"
  run_lock_old="00000000-0000-4000-8000-000000000018"
  run_lock_new="00000000-0000-4000-8000-000000000019"
  run_same="00000000-0000-4000-8000-000000000020"
  printf -v chunk_first 'sha256:%064x' 1
  printf -v chunk_changed 'sha256:%064x' 2
  printf -v chunk_candidate 'sha256:%064x' 3
  printf -v chunk_mismatch 'sha256:%064x' 4
  printf -v chunk_failed 'sha256:%064x' 5
  printf -v chunk_unrelated 'sha256:%064x' 6
  printf -v chunk_deferred 'sha256:%064x' 7
  printf -v chunk_multi_a 'sha256:%064x' 8
  printf -v chunk_multi_b 'sha256:%064x' 9
  printf -v chunk_cross 'sha256:%064x' 10
  printf -v chunk_batch_a 'sha256:%064x' 11
  printf -v chunk_batch_b 'sha256:%064x' 12
  printf -v chunk_cache_scope 'sha256:%064x' 13
  printf -v chunk_cache_expiry 'sha256:%064x' 14
  printf -v chunk_cache_bound 'sha256:%064x' 15
  printf -v chunk_lock_old 'sha256:%064x' 16
  printf -v chunk_same_old 'sha256:%064x' 17
  framed_content=$'Souveraineté — "quoted" \\ path\nsecond\tcolumn'
  changed_content="Changed semantic content"
  checkpoint_content="Durable failed-run content"
  unrelated_content="Unrelated one-source content"
  deferred_content="Deferred second batch content"
  batch_content_a="Successful checkpoint batch A"
  batch_content_b="Successful checkpoint batch B"
  cache_scope_content="Source-scoped cache content"

  {
    knowledge_pending_record \
      "${chunk_multi_a}" "First source" \
      "public" "@alice:org-a.example" "runtime-sql/multi-a"
    knowledge_pending_record \
      "${chunk_multi_b}" "Second source" \
      "public" "@alice:org-a.example" "runtime-sql/multi-b"
  } |
    ingestion_client_write /work/pending.jsonl
  if ingestion_client_plan "${run_multi_source}" >/dev/null 2>&1; then
    fail "knowledge ingestion plan accepted more than one source"
  fi
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  knowledge_pending_record \
    "${chunk_first}" "${framed_content}" "public" "@alice:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_first}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e \
    --arg chunk_id "${chunk_first}" \
    --arg content "${framed_content}" '
      .chunk_id == $chunk_id and
      .content == $content and
      .metadata.classification == "public" and
      .metadata.allowed_principals == [{
        "kind": "matrix",
        "principal": "@alice:org-a.example"
      }] and
      .embedding == null
    ' <<<"${plan_json}" >/dev/null ||
    fail "first exact SQL plan changed JSON framing, ACL metadata, or embedding state"
  knowledge_final_record \
    "${chunk_first}" "${framed_content}" "public" "@alice:org-a.example" 0 |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${framed_content}" 0 |
    ingestion_client_checkpoint "${run_first}"
  ingestion_client_commit "${run_first}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  expected_content_hex="$(
    printf '%s' "${framed_content}" | od -An -tx1 | tr -d ' \n'
  )"
  actual="$(ingestion_client_query "
    SELECT encode(convert_to(content, 'UTF8'), 'hex')
    FROM knowledge.chunks
    WHERE chunk_id = '${chunk_first}'
  ")"
  [[ "${actual}" == "${expected_content_hex}" ]] ||
    fail "real COPY framing changed Unicode or escaped content bytes"
  first_xmin="$(ingestion_client_query "
    SELECT xmin::text FROM knowledge.chunks WHERE chunk_id = '${chunk_first}'
  ")"
  [[ -n "${first_xmin}" ]] || fail "first exact SQL write did not persist its chunk"

  knowledge_pending_record \
    "${chunk_cross}" "${framed_content}" \
    "public" "@alice:org-a.example" "runtime-sql/cross-source" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_cross}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e --arg chunk_id "${chunk_cross}" '
    .chunk_id == $chunk_id and
    (.embedding | type) == "array" and
    (.embedding | length) == 1024 and
    .embedding[0] == 1
  ' <<<"${plan_json}" >/dev/null ||
    fail "new stable chunk ID did not reuse the canonical exact-content embedding"
  if knowledge_checkpoint_record "${framed_content}" 1 |
    ingestion_client_checkpoint "${run_cross}" >/dev/null 2>&1; then
    fail "checkpoint accepted a vector conflicting with canonical exact content"
  fi
  assert_embedding_cache_count 0
  ingestion_client_copy_plan
  ingestion_client_commit "${run_cross}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  knowledge_pending_record \
    "${chunk_first}" "${framed_content}" "public" "@alice:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_rerun}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e --arg chunk_id "${chunk_first}" '
    .chunk_id == $chunk_id and
    (.embedding | type) == "array" and
    (.embedding | length) == 1024 and
    .embedding[0] == 1
  ' <<<"${plan_json}" >/dev/null ||
    fail "exact rerun did not reuse the stored embedding"
  ingestion_client_copy_plan
  ingestion_client_commit "${run_rerun}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0
  rerun_xmin="$(ingestion_client_query "
    SELECT xmin::text FROM knowledge.chunks WHERE chunk_id = '${chunk_first}'
  ")"
  [[ "${rerun_xmin}" == "${first_xmin}" ]] ||
    fail "exact rerun rewrote an unchanged knowledge row"

  knowledge_pending_record \
    "${chunk_first}" "${framed_content}" "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_acl}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '
    .metadata.classification == "approved_non_public" and
    .metadata.allowed_principals == [{
      "kind": "matrix",
      "principal": "@bob:org-a.example"
    }] and
    (.embedding | type) == "array" and
    (.embedding | length) == 1024
  ' <<<"${plan_json}" >/dev/null ||
    fail "ACL-only re-ingestion failed to bind new metadata while reusing the vector"
  ingestion_client_copy_plan
  ingestion_client_commit "${run_acl}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0
  actual="$(ingestion_client_query "
    SELECT metadata = '{
      \"source\": {
        \"id\": \"runtime-sql/source\",
        \"title\": \"Exact SQL lifecycle fixture\",
        \"locator\": \"test:knowledge-ingestion/runtime\",
        \"revision\": \"runtime-v1\",
        \"location\": \"section-1\"
      },
      \"classification\": \"approved_non_public\",
      \"allowed_principals\": [{
        \"kind\": \"matrix\",
        \"principal\": \"@bob:org-a.example\"
      }],
      \"allowed_groups\": []
    }'::jsonb
    FROM knowledge.chunks
    WHERE chunk_id = '${chunk_first}'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "ACL/classification replacement retained stale authorization metadata"

  knowledge_pending_record \
    "${chunk_changed}" "${changed_content}" "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_changed}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e --arg chunk_id "${chunk_changed}" '
    .chunk_id == $chunk_id and .embedding == null
  ' <<<"${plan_json}" >/dev/null ||
    fail "changed content unexpectedly reused an old embedding"
  knowledge_final_record \
    "${chunk_changed}" "${changed_content}" \
    "approved_non_public" "@bob:org-a.example" 1 |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${changed_content}" 1 |
    ingestion_client_checkpoint "${run_changed}"
  ingestion_client_commit "${run_changed}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0
  actual="$(ingestion_client_query "
    SELECT count(*) || '|' ||
      count(*) FILTER (WHERE chunk_id = '${chunk_changed}')
    FROM knowledge.chunks
    WHERE metadata #>> '{source,id}' = 'runtime-sql/source'
  ")"
  [[ "${actual}" == "1|1" ]] ||
    fail "changed-content ingestion did not delete the obsolete same-source chunk"
  changed_xmin="$(ingestion_client_query "
    SELECT xmin::text FROM knowledge.chunks WHERE chunk_id = '${chunk_changed}'
  ")"

  knowledge_pending_record \
    "${chunk_candidate}" "Candidate replacement" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_mismatch}" >/dev/null
  if knowledge_checkpoint_record "Mismatched final record" 2 |
    ingestion_client_checkpoint "${run_mismatch}" >/dev/null 2>&1; then
    fail "checkpoint accepted content absent from authoritative pending input"
  fi
  knowledge_final_record \
    "${chunk_mismatch}" "Mismatched final record" \
    "approved_non_public" "@bob:org-a.example" 2 |
    ingestion_client_write /work/chunks.jsonl
  if ingestion_client_commit "${run_mismatch}" >/dev/null 2>&1; then
    fail "writer accepted a final set that differed from authoritative pending input"
  fi
  actual="$(ingestion_client_query "
    SELECT count(*) || '|' || min(chunk_id) || '|' || min(xmin::text)
    FROM knowledge.chunks
    WHERE metadata #>> '{source,id}' = 'runtime-sql/source'
  ")"
  [[ "${actual}" == "1|${chunk_changed}|${changed_xmin}" ]] ||
    fail "failed final-set validation changed the prior corpus"
  actual="$(ingestion_client_query "
    SELECT holder::text || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_mismatch}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_final
        WHERE run_id = '${run_mismatch}'::uuid)
    FROM knowledge.ingestion_leases
    WHERE name = 'chunks-v1'
  ")"
  [[ "${actual}" == "${run_mismatch}|1|0" ]] ||
    fail "failed writer did not preserve the bounded plan receipt"

  knowledge_pending_record \
    "${chunk_candidate}" "Competing replacement" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  if ingestion_client_plan "${run_competing}" >/dev/null 2>&1; then
    fail "a competing plan acquired an active knowledge-ingestion lease"
  fi
  actual="$(ingestion_client_query "
    SELECT
      (SELECT count(*) FROM knowledge.ingestion_leases
        WHERE holder = '${run_mismatch}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_mismatch}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_competing}'::uuid)
  ")"
  [[ "${actual}" == "1|1|0" ]] ||
    fail "competing-plan rejection changed the active lease receipt"

  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_mismatch}'::uuid
  " >/dev/null
  knowledge_pending_record \
    "${chunk_changed}" "${changed_content}" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_reclaim}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT holder::text || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_mismatch}'::uuid)
    FROM knowledge.ingestion_leases
    WHERE name = 'chunks-v1'
  ")"
  [[ "${actual}" == "${run_reclaim}|0" ]] ||
    fail "expired knowledge-ingestion lease or receipt was not reclaimed"
  ingestion_client_copy_plan
  ingestion_client_commit "${run_reclaim}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0
  reclaimed_xmin="$(ingestion_client_query "
    SELECT xmin::text FROM knowledge.chunks WHERE chunk_id = '${chunk_changed}'
  ")"
  [[ "${reclaimed_xmin}" == "${changed_xmin}" ]] ||
    fail "lease reclamation rewrote unchanged corpus content"

  echo "==> Proving lease-first reclamation cannot deadlock an expiring writer"
  knowledge_pending_record \
    "${chunk_lock_old}" "Lock-order old receipt" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_lock_old}" >/dev/null
  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_lock_old}'::uuid
  " >/dev/null
  ingestion_client_exec rm -f /work/lease-lock-held /work/lease-lock-release
  ingestion_client_write /work/lease-lock-writer.sql <<SQL
\set ON_ERROR_STOP on
BEGIN;
SELECT holder
FROM knowledge.ingestion_leases
WHERE holder = '${run_lock_old}'::uuid
FOR UPDATE;
\! touch /work/lease-lock-held
\! until test -e /work/lease-lock-release; do sleep 0.05; done
DELETE FROM knowledge.ingestion_pending
WHERE run_id = '${run_lock_old}'::uuid;
DELETE FROM knowledge.ingestion_final
WHERE run_id = '${run_lock_old}'::uuid;
DELETE FROM knowledge.ingestion_leases
WHERE holder = '${run_lock_old}'::uuid;
COMMIT;
SQL
  lock_writer_log="${RUNTIME_WORKDIR}/lease-lock-writer.log"
  lock_plan_log="${RUNTIME_WORKDIR}/lease-lock-plan.log"
  ingestion_client_exec psql --quiet --no-psqlrc \
    --file=/work/lease-lock-writer.sql >"${lock_writer_log}" 2>&1 &
  lock_writer_pid=$!
  for _ in {1..200}; do
    if ingestion_client_exec test -e /work/lease-lock-held >/dev/null 2>&1; then
      break
    fi
    sleep 0.05
  done
  if ! ingestion_client_exec test -e /work/lease-lock-held >/dev/null 2>&1; then
    ingestion_client_exec touch /work/lease-lock-release || true
    wait "${lock_writer_pid}" || true
    sed -n '1,240p' "${lock_writer_log}" >&2
    fail "old writer did not acquire the lease row for the lock-order proof"
  fi

  knowledge_pending_record \
    "${chunk_changed}" "${changed_content}" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_exec env PGAPPNAME=knowledge-ingestion-lock-order-plan \
    psql --quiet --no-psqlrc \
    --set="run_id=${run_lock_new}" --file=/runtime/plan.sql \
    >"${lock_plan_log}" 2>&1 &
  lock_plan_pid=$!
  actual="f"
  for _ in {1..200}; do
    actual="$(
      admin_sql knowledge --quiet --tuples-only --no-align --command="
        SELECT EXISTS (
          SELECT 1
          FROM pg_stat_activity
          WHERE application_name = 'knowledge-ingestion-lock-order-plan'
            AND wait_event_type = 'Lock'
        )
      "
    )"
    [[ "${actual}" == "t" ]] && break
    sleep 0.05
  done
  if [[ "${actual}" != "t" ]]; then
    ingestion_client_exec touch /work/lease-lock-release || true
    wait "${lock_writer_pid}" || true
    wait "${lock_plan_pid}" || true
    sed -n '1,240p' "${lock_writer_log}" >&2
    sed -n '1,240p' "${lock_plan_log}" >&2
    fail "new plan did not wait on the expired lease before staging cleanup"
  fi
  ingestion_client_exec touch /work/lease-lock-release
  if ! wait "${lock_writer_pid}"; then
    wait "${lock_plan_pid}" || true
    sed -n '1,240p' "${lock_writer_log}" >&2
    sed -n '1,240p' "${lock_plan_log}" >&2
    fail "old writer deadlocked while releasing the expired lease"
  fi
  if ! wait "${lock_plan_pid}"; then
    sed -n '1,240p' "${lock_plan_log}" >&2
    fail "new plan failed after the old writer released the expired lease"
  fi
  actual="$(ingestion_client_query "
    SELECT holder::text || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_lock_old}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_lock_new}'::uuid)
    FROM knowledge.ingestion_leases
    WHERE name = 'chunks-v1'
  ")"
  [[ "${actual}" == "${run_lock_new}|0|1" ]] ||
    fail "lease-first reclamation did not leave only the new plan receipt"
  ingestion_client_copy_plan
  ingestion_client_commit "${run_lock_new}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  echo "==> Proving same-run crash receipts are reset after lease reacquisition"
  knowledge_pending_record \
    "${chunk_same_old}" "Same-run old pending receipt" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_same}" >/dev/null
  knowledge_final_record \
    "${chunk_same_old}" "Same-run old final receipt" \
    "approved_non_public" "@bob:org-a.example" 17 |
    ingestion_client_write /work/same-run-final.jsonl
  ingestion_client_write /work/same-run-final.sql <<'SQL'
\set ON_ERROR_STOP on
SELECT set_config('fgentic.ingestion_run_id', :'run_id', false);
\copy knowledge.ingestion_final (payload) FROM '/work/same-run-final.jsonl' WITH (FORMAT csv, DELIMITER E'\x1f', QUOTE E'\x1e', ESCAPE E'\x1e')
SQL
  ingestion_client_exec psql --quiet --no-psqlrc \
    --set="run_id=${run_same}" --file=/work/same-run-final.sql >/dev/null
  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_same}'::uuid
  " >/dev/null
  knowledge_pending_record \
    "${chunk_changed}" "${changed_content}" \
    "approved_non_public" "@bob:org-a.example" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_same}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT holder::text || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_same}'::uuid) || '|' ||
      (SELECT min(payload->>'content') FROM knowledge.ingestion_pending
        WHERE run_id = '${run_same}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_final
        WHERE run_id = '${run_same}'::uuid)
    FROM knowledge.ingestion_leases
    WHERE name = 'chunks-v1'
  ")"
  [[ "${actual}" == "${run_same}|1|${changed_content}|0" ]] ||
    fail "same-run lease reacquisition retained an old pending or final receipt"
  ingestion_client_copy_plan
  ingestion_client_commit "${run_same}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  echo "==> Proving failed-run embedding checkpoints survive an interleaved source"
  {
    knowledge_pending_record \
      "${chunk_failed}" "${checkpoint_content}" \
      "approved_non_public" "@carol:org-a.example" "runtime-sql/resume"
    knowledge_pending_record \
      "${chunk_deferred}" "${deferred_content}" \
      "approved_non_public" "@carol:org-a.example" "runtime-sql/resume"
  } |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_failed}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -se \
    --arg chunk_failed "${chunk_failed}" \
    --arg chunk_deferred "${chunk_deferred}" '
      length == 2 and
      ([.[].chunk_id] | sort) == ([$chunk_failed, $chunk_deferred] | sort) and
      all(.[]; .embedding == null)
  ' <<<"${plan_json}" >/dev/null ||
    fail "new two-input failed-run fixture unexpectedly reused an embedding"
  knowledge_checkpoint_record "${checkpoint_content}" 2 |
    ingestion_client_write /work/checkpoint.ready
  ingestion_client_checkpoint_file "${run_failed}"
  assert_embedding_cache_count 1
  actual="$(ingestion_client_query "
    SELECT holder::text || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_failed}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_final
        WHERE run_id = '${run_failed}'::uuid)
    FROM knowledge.ingestion_leases
    WHERE name = 'chunks-v1'
  ")"
  [[ "${actual}" == "${run_failed}|2|0" ]] ||
    fail "subset checkpoint changed pending input or retained transient batch staging"
  actual="$(ingestion_client_query "
    SELECT count(*) = 1
      AND bool_and(profile = 'bge-m3-1024-v1')
      AND bool_and(source_id = 'runtime-sql/resume')
      AND bool_and(
        content_sha256 = encode(sha256(convert_to(content, 'UTF8')), 'hex')
      )
      AND bool_and(content = '${checkpoint_content}')
      AND bool_and(expires_at > cached_at)
      AND bool_and(expires_at <= cached_at + interval '24 hours')
      AND bool_and(expires_at > clock_timestamp())
      AND bool_and(
        embedding = (
          ARRAY[0::real, 0::real, 1::real] ||
          array_fill(0::real, ARRAY[1021])
        )::vector(1024)
      )
    FROM knowledge.ingestion_embedding_cache
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "failed-run checkpoint did not persist exact source, TTL, content, and vector"

  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_failed}'::uuid
  " >/dev/null
  knowledge_pending_record \
    "${chunk_failed}" "${checkpoint_content}" \
    "approved_non_public" "@carol:org-a.example" "runtime-sql/resume" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_conflict}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '
    (.embedding | type) == "array" and
    (.embedding | length) == 1024 and
    .embedding[2] == 1
  ' <<<"${plan_json}" >/dev/null ||
    fail "expired failed run did not resume from its durable exact-content checkpoint"
  if knowledge_checkpoint_record "${checkpoint_content}" 3 |
    ingestion_client_checkpoint "${run_conflict}" >/dev/null 2>&1; then
    fail "checkpoint accepted a conflicting vector for cached exact content"
  fi
  assert_embedding_cache_count 1

  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_conflict}'::uuid
  " >/dev/null
  knowledge_pending_record \
    "${chunk_unrelated}" "${unrelated_content}" \
    "public" "@dave:org-a.example" "runtime-sql/unrelated" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_unrelated}" >/dev/null
  knowledge_final_record \
    "${chunk_unrelated}" "${unrelated_content}" \
    "public" "@dave:org-a.example" 4 "runtime-sql/unrelated" |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${unrelated_content}" 4 |
    ingestion_client_checkpoint "${run_unrelated}"
  assert_embedding_cache_count 2
  ingestion_client_commit "${run_unrelated}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 1
  actual="$(ingestion_client_query "
    SELECT content FROM knowledge.ingestion_embedding_cache
  ")"
  [[ "${actual}" == "${checkpoint_content}" ]] ||
    fail "unrelated successful source pruned another source's failed-run checkpoint"

  owner_sql --quiet --command="
    WITH stamped AS (
      SELECT clock_timestamp() AS cached_at
    )
    INSERT INTO knowledge.ingestion_embedding_cache (
      profile,
      source_id,
      content_sha256,
      content,
      embedding,
      cached_at,
      expires_at
    )
    SELECT
      'bge-m3-1024-v1',
      'runtime-sql/resume',
      encode(sha256(convert_to('Stale same-source checkpoint', 'UTF8')), 'hex'),
      'Stale same-source checkpoint',
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      stamped.cached_at,
      stamped.cached_at + interval '24 hours'
    FROM stamped
  " >/dev/null
  assert_embedding_cache_count 2
  knowledge_pending_record \
    "${chunk_failed}" "${checkpoint_content}" \
    "approved_non_public" "@carol:org-a.example" "runtime-sql/resume" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_resume}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e --arg chunk_id "${chunk_failed}" '
    .chunk_id == $chunk_id and
    (.embedding | type) == "array" and
    (.embedding | length) == 1024 and
    .embedding[2] == 1
  ' <<<"${plan_json}" >/dev/null ||
    fail "interleaved source run erased the failed-run embedding checkpoint"
  ingestion_client_copy_plan
  ingestion_client_commit "${run_resume}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  echo "==> Proving sequential checkpoint batches accumulate before one full write"
  {
    knowledge_pending_record \
      "${chunk_batch_a}" "${batch_content_a}" \
      "public" "@erin:org-a.example" "runtime-sql/batch"
    knowledge_pending_record \
      "${chunk_batch_b}" "${batch_content_b}" \
      "public" "@erin:org-a.example" "runtime-sql/batch"
  } |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_batch}" >/dev/null
  {
    knowledge_final_record \
      "${chunk_batch_a}" "${batch_content_a}" \
      "public" "@erin:org-a.example" 5 "runtime-sql/batch"
    knowledge_final_record \
      "${chunk_batch_b}" "${batch_content_b}" \
      "public" "@erin:org-a.example" 6 "runtime-sql/batch"
  } |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${batch_content_a}" 5 |
    ingestion_client_checkpoint "${run_batch}"
  knowledge_checkpoint_record "${batch_content_b}" 6 |
    ingestion_client_checkpoint "${run_batch}"
  actual="$(ingestion_client_query "
    SELECT holder::text || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_pending
        WHERE run_id = '${run_batch}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_final
        WHERE run_id = '${run_batch}'::uuid) || '|' ||
      (SELECT count(*) FROM knowledge.ingestion_embedding_cache)
    FROM knowledge.ingestion_leases
    WHERE name = 'chunks-v1'
  ")"
  [[ "${actual}" == "${run_batch}|2|0|2" ]] ||
    fail "sequential checkpoint batches did not accumulate with cleared transient staging"
  ingestion_client_commit "${run_batch}" >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0
  actual="$(ingestion_client_query "
    SELECT count(*) FROM knowledge.chunks
    WHERE metadata #>> '{source,id}' = 'runtime-sql/batch'
  ")"
  [[ "${actual}" == "2" ]] ||
    fail "full writer did not commit both sequentially checkpointed embedding batches"

  echo "==> Proving cache source isolation, expiry, and deterministic hard bound"
  owner_sql --quiet --command="
    WITH stamped AS (
      SELECT clock_timestamp() AS cached_at
    )
    INSERT INTO knowledge.ingestion_embedding_cache (
      profile,
      source_id,
      content_sha256,
      content,
      embedding,
      cached_at,
      expires_at
    )
    SELECT
      'bge-m3-1024-v1',
      'runtime-sql/cache-other',
      encode(sha256(convert_to('${cache_scope_content}', 'UTF8')), 'hex'),
      '${cache_scope_content}',
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      stamped.cached_at,
      stamped.cached_at + interval '24 hours'
    FROM stamped
  " >/dev/null
  knowledge_pending_record \
    "${chunk_cache_scope}" "${cache_scope_content}" \
    "public" "@frank:org-a.example" "runtime-sql/cache-current" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_cache_scope}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '.embedding == null' <<<"${plan_json}" >/dev/null ||
    fail "a source reused another source's durable embedding checkpoint"
  assert_embedding_cache_count 1

  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_cache_scope}'::uuid;
    WITH stamped AS (
      SELECT clock_timestamp() - interval '25 hours' AS cached_at
    )
    INSERT INTO knowledge.ingestion_embedding_cache (
      profile,
      source_id,
      content_sha256,
      content,
      embedding,
      cached_at,
      expires_at
    )
    SELECT
      'bge-m3-1024-v1',
      'runtime-sql/cache-current',
      encode(sha256(convert_to('${cache_scope_content}', 'UTF8')), 'hex'),
      '${cache_scope_content}',
      (ARRAY[0::real, 1::real] || array_fill(0::real, ARRAY[1022]))::vector(1024),
      stamped.cached_at,
      stamped.cached_at + interval '24 hours'
    FROM stamped
  " >/dev/null
  ingestion_client_gc >/dev/null
  actual="$(ingestion_client_query "
    SELECT count(*) || '|' ||
      count(*) FILTER (WHERE expires_at <= clock_timestamp()) || '|' ||
      count(*) FILTER (WHERE source_id = 'runtime-sql/cache-other')
    FROM knowledge.ingestion_embedding_cache
  ")"
  [[ "${actual}" == "1|0|1" ]] ||
    fail "independent cache GC did not prune only the expired checkpoint"
  knowledge_pending_record \
    "${chunk_cache_expiry}" "${cache_scope_content}" \
    "public" "@frank:org-a.example" "runtime-sql/cache-current" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_cache_expiry}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '.embedding == null' <<<"${plan_json}" >/dev/null ||
    fail "an expired same-source embedding checkpoint was reused"

  owner_sql --quiet --command="
    UPDATE knowledge.ingestion_leases
    SET expires_at = clock_timestamp() - interval '1 minute'
    WHERE holder = '${run_cache_expiry}'::uuid;
    DELETE FROM knowledge.ingestion_embedding_cache;
    WITH stamped AS (
      SELECT clock_timestamp() AS cached_at
    ),
    cache_rows AS (
      SELECT
        CASE
          WHEN series = 1 THEN 'runtime-sql/cache-bound-current'
          ELSE 'runtime-sql/cache-bound-' || lpad(series::text, 4, '0')
        END AS source_id,
        CASE
          WHEN series = 1 THEN 'Hard-bound plan input'
          ELSE 'Bounded cache row ' || series::text
        END AS content,
        stamped.cached_at - ((1025 - series) * interval '1 second') AS cached_at
      FROM generate_series(1, 1025) AS generated(series)
      CROSS JOIN stamped
    )
    INSERT INTO knowledge.ingestion_embedding_cache (
      profile,
      source_id,
      content_sha256,
      content,
      embedding,
      cached_at,
      expires_at
    )
    SELECT
      'bge-m3-1024-v1',
      cache_rows.source_id,
      encode(sha256(convert_to(cache_rows.content, 'UTF8')), 'hex'),
      cache_rows.content,
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      cache_rows.cached_at,
      cache_rows.cached_at + interval '24 hours'
    FROM cache_rows
  " >/dev/null
  knowledge_pending_record \
    "${chunk_cache_bound}" "Hard-bound plan input" \
    "public" "@frank:org-a.example" "runtime-sql/cache-bound-current" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${run_cache_bound}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '
    (.embedding | type) == "array" and
    (.embedding | length) == 1024 and
    .embedding[0] == 1
  ' <<<"${plan_json}" >/dev/null ||
    fail "cache hard-bound pruning evicted the active input's exact receipt"
  actual="$(ingestion_client_query "
    SELECT count(*) || '|' ||
      count(*) FILTER (WHERE source_id = 'runtime-sql/cache-bound-current') || '|' ||
      count(*) FILTER (WHERE source_id = 'runtime-sql/cache-bound-0002') || '|' ||
      count(*) FILTER (WHERE source_id = 'runtime-sql/cache-bound-1025')
    FROM knowledge.ingestion_embedding_cache
  ")"
  [[ "${actual}" == "1024|1|0|1" ]] ||
    fail "cache hard-bound pruning did not retain active input and evict oldest unrelated row"
  owner_sql --quiet --command="
    DELETE FROM knowledge.ingestion_embedding_cache;
    DELETE FROM knowledge.ingestion_pending
    WHERE run_id = '${run_cache_bound}'::uuid;
    DELETE FROM knowledge.ingestion_final
    WHERE run_id = '${run_cache_bound}'::uuid;
    DELETE FROM knowledge.ingestion_leases
    WHERE holder = '${run_cache_bound}'::uuid
  " >/dev/null
  assert_ingestion_staging_empty
  assert_embedding_cache_count 0

  owner_sql --quiet --command="
    DELETE FROM knowledge.chunks
    WHERE metadata #>> '{source,id}' LIKE 'runtime-sql/%'
  " >/dev/null
  actual="$(ingestion_client_query "
    SELECT count(*) FROM knowledge.chunks
    WHERE metadata #>> '{source,id}' LIKE 'runtime-sql/%'
  ")"
  [[ "${actual}" == "0" ]] || fail "exact SQL lifecycle fixture cleanup failed"

  echo "==> Exercising real Docling samples through the exact SQL lifecycle"
  exercise_docling_sample \
    matrix \
    "${MATRIX_SOURCE_EXAMPLE}" \
    matrix-principal.md \
    00000000-0000-4000-8000-000000000101 \
    00000000-0000-4000-8000-000000000102 \
    7
  exercise_docling_sample \
    partner \
    "${PARTNER_SOURCE_EXAMPLE}" \
    partner-group.md \
    00000000-0000-4000-8000-000000000201 \
    00000000-0000-4000-8000-000000000202 \
    11

  echo "==> Exercising the DML-only knowledge ingestion role"
  ingestion_sql >/dev/null <<'SQL'
INSERT INTO knowledge.chunks (chunk_id, content, embedding, metadata)
VALUES (
  'ingestion-boundary',
  'Initial ingestion content',
  (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
  '{
    "source": {"id": "source-ingestion-boundary"},
    "classification": "public",
    "allowed_principals": [
      {"kind": "matrix", "principal": "@alice:org-a.example"}
    ],
    "allowed_groups": []
  }'::jsonb
);
UPDATE knowledge.chunks
SET content = 'Updated ingestion content'
WHERE chunk_id = 'ingestion-boundary';
SQL
  actual="$(ingestion_sql --tuples-only --no-align --command="
    SELECT content FROM knowledge.chunks WHERE chunk_id = 'ingestion-boundary'
  ")"
  [[ "${actual}" == "Updated ingestion content" ]] ||
    fail "knowledge ingestion role could not select, insert, or update its chunk"
  actual="$(ingestion_sql --quiet --tuples-only --no-align --command="
    DELETE FROM knowledge.chunks
    WHERE chunk_id = 'ingestion-boundary'
    RETURNING chunk_id
  ")"
  [[ "${actual}" == "ingestion-boundary" ]] ||
    fail "knowledge ingestion role could not delete its chunk"
  if ingestion_sql --command='CREATE TABLE knowledge.ingestion_forbidden (id integer)' \
    >/dev/null 2>&1; then
    fail "knowledge ingestion role unexpectedly holds schema CREATE"
  fi
  if ingestion_sql --command='ALTER TABLE knowledge.chunks ADD COLUMN forbidden integer' \
    >/dev/null 2>&1; then
    fail "knowledge ingestion role unexpectedly owns or can alter chunks"
  fi
  if ingestion_sql --command="
    SELECT * FROM knowledge.search_authorized_matrix(
      (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
      ARRAY['public']::text[],
      '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
      1
    )
  " >/dev/null 2>&1; then
    fail "knowledge ingestion role unexpectedly executes the retrieval surface"
  fi

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
  if ingestion_sql --command='CREATE DATABASE knowledge_ingestion_forbidden' \
    >/dev/null 2>&1; then
    fail "knowledge ingestion role unexpectedly holds CREATEDB"
  fi
  if ingestion_sql --command='CREATE ROLE knowledge_ingestion_forbidden' \
    >/dev/null 2>&1; then
    fail "knowledge ingestion role unexpectedly holds CREATEROLE"
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
  if kubectl --namespace "${namespace}" exec "pod/${primary}" --container postgres -- \
    env PGPASSWORD="${INGESTION_PASSWORD}" \
    psql --no-psqlrc --set=ON_ERROR_STOP=1 \
    --dbname='host=127.0.0.1 dbname=unrelated_service user=knowledge_ingestion sslmode=require' \
    --command='SELECT 1' >/dev/null 2>&1; then
    fail "knowledge ingestion role crossed the exact knowledge HBA boundary"
  fi
  if kubectl --namespace "${namespace}" exec "pod/${primary}" --container postgres -- \
    env PGPASSWORD="${CONNECTOR_PASSWORD}" \
    psql --no-psqlrc --set=ON_ERROR_STOP=1 \
    --dbname='host=127.0.0.1 dbname=unrelated_service user=knowledge_connector sslmode=require' \
    --command='SELECT 1' >/dev/null 2>&1; then
    fail "knowledge connector role crossed the exact knowledge HBA boundary"
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

  echo "==> Exercising resumable connector snapshot, present, ACL, and tombstone SQL"
  local connector_source_a connector_source_b connector_content_a connector_content_b
  local connector_digest_a connector_digest_b connector_acl_alice connector_acl_alice_b
  local connector_acl_bob
  local connector_metadata_a_alice connector_metadata_b_alice connector_metadata_a_bob
  local connector_metadata_invalid
  local connector_inventory_bad connector_inventory_bad_metadata connector_inventory_preempt
  local connector_inventory_revoke
  local connector_inventory_v1 connector_inventory_v2
  local connector_inventory_v1_db_digest connector_inventory_v1_python_digest
  local connector_snapshot_bad connector_snapshot_bad_metadata connector_snapshot_blocked
  local connector_snapshot_noop connector_snapshot_preempt connector_snapshot_revert
  local connector_snapshot_revoke connector_snapshot_v1 connector_snapshot_v2
  local connector_action_a connector_action_replay connector_action_b
  local connector_action_acl connector_action_tombstone connector_action_reclaimed
  local connector_action_tombstone_normalized connector_action_reclaimed_normalized
  local connector_chunk_a connector_chunk_b connector_bad_acl
  local connector_run_a connector_run_b connector_run_acl connector_run_old connector_run_new
  connector_source_a="reference-docs/git-markdown/docs/a.md"
  connector_source_b="reference-docs/git-markdown/docs/é.md"
  connector_content_a="Stable connector source A content"
  connector_content_b="Connector source B content"
  connector_digest_a="$(connector_content_digest "${connector_content_a}")"
  connector_digest_b="$(connector_content_digest "${connector_content_b}")"
  printf -v connector_chunk_a 'sha256:%064x' 21
  printf -v connector_chunk_b 'sha256:%064x' 22
  printf -v connector_bad_acl 'sha256:%064x' 0
  connector_run_a="00000000-0000-4000-8000-000000000021"
  connector_run_b="00000000-0000-4000-8000-000000000022"
  connector_run_acl="00000000-0000-4000-8000-000000000023"
  connector_run_old="00000000-0000-4000-8000-000000000024"
  connector_run_new="00000000-0000-4000-8000-000000000025"

  connector_metadata_a_alice="$(jq -cn \
    --arg source_id "${connector_source_a}" \
    --arg revision "${connector_digest_a}" '
      {
        source: {
          id: $source_id,
          title: "Connector source A é",
          locator: "git:flux-system/flux-system#docs/a.md",
          revision: $revision
        },
        classification: "approved_non_public",
        allowed_principals: [{kind: "matrix", principal: "@alice:org-a.example"}],
        allowed_groups: []
      }
    ')"
  connector_metadata_b_alice="$(jq -cn \
    --arg source_id "${connector_source_b}" \
    --arg revision "${connector_digest_b}" '
      {
        source: {
          id: $source_id,
          title: "Connector source B",
          locator: "git:flux-system/flux-system#docs/é.md",
          revision: $revision
        },
        classification: "approved_non_public",
        allowed_principals: [{kind: "matrix", principal: "@alice:org-a.example"}],
        allowed_groups: []
      }
    ')"
  connector_metadata_a_bob="$(jq -cn \
    --arg source_id "${connector_source_a}" \
    --arg revision "${connector_digest_a}" '
      {
        source: {
          id: $source_id,
          title: "Connector source A é",
          locator: "git:flux-system/flux-system#docs/a.md",
          revision: $revision
        },
        classification: "approved_non_public",
        allowed_principals: [{kind: "matrix", principal: "@bob:org-a.example"}],
        allowed_groups: []
      }
    ')"
  connector_acl_alice="$(connector_acl_digest "${connector_metadata_a_alice}")"
  connector_acl_alice_b="$(connector_acl_digest "${connector_metadata_b_alice}")"
  [[ "${connector_acl_alice}" == "${connector_acl_alice_b}" ]] ||
    fail "equal connector ACL operands produced different canonical digests"
  connector_acl_bob="$(connector_acl_digest "${connector_metadata_a_bob}")"
  [[ "${connector_acl_alice}" != "${connector_acl_bob}" ]] ||
    fail "changed connector ACL operands retained the old canonical digest"
  connector_metadata_invalid="$(jq -c 'del(.source.locator)' <<<"${connector_metadata_a_alice}")"

  connector_inventory_bad="$(jq -cn \
    --arg source_id "${connector_source_a}" \
    --arg revision "${connector_digest_a}" \
    --arg content_digest "${connector_digest_a}" \
    --arg acl_digest "${connector_bad_acl}" \
    --argjson metadata "${connector_metadata_a_alice}" '
      [{
        source_id: $source_id,
        source_path: "docs/a.md",
        source_revision: $revision,
        content_digest: $content_digest,
        acl_digest: $acl_digest,
        metadata: $metadata
      }]
    ')"
  connector_snapshot_bad="$(connector_snapshot \
    "git-invalid-acl" \
    "sha256:9999999999999999999999999999999999999999999999999999999999999999" \
    "${connector_inventory_bad}")"
  if connector_publish_snapshot "${connector_snapshot_bad}" >/dev/null 2>&1; then
    fail "connector publisher accepted an ACL digest unrelated to the validated metadata"
  fi
  actual="$(ingestion_client_query "
    SELECT
      (SELECT count(*) FROM knowledge.connector_snapshots) || '|' ||
      (SELECT count(*) FROM knowledge.connector_inventory) || '|' ||
      (SELECT count(*) FROM knowledge.connector_sources)
  ")"
  [[ "${actual}" == "0|0|0" ]] ||
    fail "rejected connector ACL publication left partial database state"

  connector_inventory_bad_metadata="$(jq -cn \
    --arg source_id "${connector_source_a}" \
    --arg revision "${connector_digest_a}" \
    --arg content_digest "${connector_digest_a}" \
    --arg acl_digest "${connector_acl_alice}" \
    --argjson metadata "${connector_metadata_invalid}" '
      [{
        source_id: $source_id,
        source_path: "docs/a.md",
        source_revision: $revision,
        content_digest: $content_digest,
        acl_digest: $acl_digest,
        metadata: $metadata
      }]
    ')"
  connector_snapshot_bad_metadata="$(connector_snapshot \
    "git-invalid-metadata" \
    "sha256:8888888888888888888888888888888888888888888888888888888888888888" \
    "${connector_inventory_bad_metadata}")"
  if connector_publish_snapshot "${connector_snapshot_bad_metadata}" >/dev/null 2>&1; then
    fail "connector publisher accepted metadata the materializer cannot consume"
  fi
  actual="$(ingestion_client_query "
    SELECT
      (SELECT count(*) FROM knowledge.connector_snapshots) || '|' ||
      (SELECT count(*) FROM knowledge.connector_inventory) || '|' ||
      (SELECT count(*) FROM knowledge.connector_sources)
  ")"
  [[ "${actual}" == "0|0|0" ]] ||
    fail "rejected connector metadata publication left partial database state"

  connector_inventory_v1="$(jq -cn \
    --arg source_a "${connector_source_a}" \
    --arg source_b "${connector_source_b}" \
    --arg digest_a "${connector_digest_a}" \
    --arg digest_b "${connector_digest_b}" \
    --arg acl_digest "${connector_acl_alice}" \
    --argjson metadata_a "${connector_metadata_a_alice}" \
    --argjson metadata_b "${connector_metadata_b_alice}" '
      [
        {
          source_id: $source_a,
          source_path: "docs/a.md",
          source_revision: $digest_a,
          content_digest: $digest_a,
          acl_digest: $acl_digest,
          metadata: $metadata_a
        },
        {
          source_id: $source_b,
          source_path: "docs/é.md",
          source_revision: $digest_b,
          content_digest: $digest_b,
          acl_digest: $acl_digest,
          metadata: $metadata_b
        }
      ] | sort_by(.source_id)
    ')"
  connector_inventory_v1_db_digest="$(connector_inventory_digest "${connector_inventory_v1}")"
  connector_inventory_v1_python_digest="$(
    connector_inventory_digest_python "${connector_inventory_v1}"
  )"
  [[ "${connector_inventory_v1_db_digest}" == "${connector_inventory_v1_python_digest}" ]] ||
    fail "PostgreSQL and the UTF-8 connector serializer disagreed on canonical inventory digest"
  connector_snapshot_v1="$(connector_snapshot \
    "git-snapshot-v1" \
    "sha256:1111111111111111111111111111111111111111111111111111111111111111" \
    "${connector_inventory_v1}")"
  connector_publish_snapshot "${connector_snapshot_v1}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT enumeration_complete
      AND applied_revision IS NULL
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE desired_action = 'present') = 2
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE applied_action IS NOT NULL) = 0
    FROM knowledge.connector_snapshots
    WHERE connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "complete connector inventory advanced before both sources were applied"

  if ingestion_sql --quiet --command="
    UPDATE knowledge.connector_sources
    SET claim_holder = '00000000-0000-4000-8000-000000000099'::uuid,
        claimed_at = clock_timestamp(),
        claim_expires_at = NULL
    WHERE source_id = '${connector_source_a}'
  " >/dev/null 2>&1; then
    fail "connector source accepted a claim holder without a bounded expiry"
  fi

  connector_client_claim "${connector_run_a}" >/dev/null
  connector_action_a="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e \
    --arg source_id "${connector_source_a}" \
    --arg digest "${connector_digest_a}" \
    --arg acl_digest "${connector_acl_alice}" \
    --argjson metadata "${connector_metadata_a_alice}" '
      .connector_id == "git-markdown" and
      .source_id == $source_id and
      .source_path == "docs/a.md" and
      .action == "present" and
      .source_revision == $digest and
      .content_digest == $digest and
      .acl_digest == $acl_digest and
      .metadata == $metadata and
      .snapshot_revision == "git-snapshot-v1" and
      (.claim_expires_at | type) == "string"
    ' <<<"${connector_action_a}" >/dev/null || {
    echo "connector action: ${connector_action_a}" >&2
    fail "connector claim lost source A identity, digest, ACL, or snapshot binding"
  }

  connector_publish_snapshot "${connector_snapshot_v1}" >/dev/null
  connector_client_claim "${connector_run_a}" >/dev/null
  connector_action_replay="$(ingestion_client_exec cat /work/connector-action.json)"
  [[ "${connector_action_replay}" == "${connector_action_a}" ]] ||
    fail "identical snapshot replay replaced or extended the active connector claim"

  connector_pending_record \
    "${connector_action_a}" "${connector_chunk_a}" "${connector_content_a}" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${connector_run_a}" >/dev/null
  connector_final_record \
    "${connector_action_a}" "${connector_chunk_a}" "${connector_content_a}" 21 |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${connector_content_a}" 21 |
    ingestion_client_checkpoint "${connector_run_a}"
  connector_client_commit "${connector_run_a}" >/dev/null
  assert_ingestion_staging_empty
  connector_client_claim "${connector_run_b}" >/dev/null
  connector_action_b="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e --arg source_id "${connector_source_b}" '
    .source_id == $source_id and .action == "present"
  ' <<<"${connector_action_b}" >/dev/null ||
    fail "preemption fixture did not hold the older source B claim"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT count(*) = 1
    FROM knowledge.search_authorized_matrix(
      ${query_vector},
      ARRAY['approved_non_public', 'public']::text[],
      '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
      50
    )
    WHERE metadata #>> '{source,id}' = '${connector_source_a}'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "preemption fixture source A was not initially retrievable to Alice"

  connector_inventory_preempt="$(jq -cn \
    --arg source_id "${connector_source_a}" \
    --arg digest "${connector_digest_a}" \
    --arg acl_digest "${connector_acl_bob}" \
    --argjson metadata "${connector_metadata_a_bob}" '
      [{
        source_id: $source_id,
        source_path: "docs/a.md",
        source_revision: $digest,
        content_digest: $digest,
        acl_digest: $acl_digest,
        metadata: $metadata
      }]
    ')"
  connector_snapshot_preempt="$(connector_snapshot \
    "git-snapshot-preempt" \
    "sha256:7777777777777777777777777777777777777777777777777777777777777777" \
    "${connector_inventory_preempt}")"
  connector_publish_snapshot "${connector_snapshot_preempt}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT snapshots.desired_revision = 'git-snapshot-preempt'
      AND snapshots.applied_revision IS NULL
      AND snapshots.blocked_at IS NULL
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE source_id = '${connector_source_a}'
          AND desired_action = 'present'
          AND desired_acl_digest = '${connector_acl_bob}'
          AND applied_action = 'present'
          AND claim_holder IS NULL) = 1
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE source_id = '${connector_source_b}'
          AND desired_action = 'tombstone'
          AND applied_action IS NULL
          AND claim_holder IS NULL) = 1
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = '${connector_source_a}'
          AND metadata->>'classification' = 'authentication') = 1
    FROM knowledge.connector_snapshots AS snapshots
    WHERE snapshots.connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "newer connector inventory did not preempt and invalidate older pending work"
  if ingestion_sql --quiet --command="
    SELECT knowledge.complete_connector_present('${connector_run_b}'::uuid)
  " >/dev/null 2>&1; then
    fail "preempted connector action still completed against newer desired state"
  fi
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT
      (SELECT count(*) = 0
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
      AND
      (SELECT count(*) = 0
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "preempted ACL remained retrievable to its old or new audience"
  connector_client_claim "${connector_run_new}" >/dev/null
  connector_action_replay="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e \
    --arg source_id "${connector_source_a}" \
    --arg acl_digest "${connector_acl_bob}" '
      .source_id == $source_id and
      .snapshot_revision == "git-snapshot-preempt" and
      .acl_digest == $acl_digest
    ' <<<"${connector_action_replay}" >/dev/null ||
    fail "post-preemption claim did not select only the latest source contract"

  owner_sql --quiet >/dev/null <<'SQL'
BEGIN;
DELETE FROM knowledge.ingestion_embedding_cache
WHERE source_id LIKE 'reference-docs/git-markdown/%';
DELETE FROM knowledge.chunks
WHERE metadata #>> '{source,id}' LIKE 'reference-docs/git-markdown/%';
UPDATE knowledge.connector_snapshots
SET enumeration_complete = false,
    enumeration_completed_at = NULL;
DELETE FROM knowledge.connector_inventory;
DELETE FROM knowledge.connector_sources;
DELETE FROM knowledge.connector_snapshots;
COMMIT;
SQL
  connector_publish_snapshot "${connector_snapshot_v1}" >/dev/null
  connector_client_claim "${connector_run_a}" >/dev/null
  connector_action_a="$(ingestion_client_exec cat /work/connector-action.json)"

  connector_pending_record \
    "${connector_action_a}" "${connector_chunk_a}" "${connector_content_a}" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${connector_run_a}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e \
    --arg source_id "${connector_source_a}" \
    --argjson metadata "${connector_metadata_a_alice}" '
      .embedding == null and
      .metadata.source.id == $source_id and
      (.metadata.source | del(.location)) == $metadata.source and
      .metadata.classification == $metadata.classification and
      .metadata.allowed_principals == $metadata.allowed_principals and
      .metadata.allowed_groups == $metadata.allowed_groups
    ' <<<"${plan_json}" >/dev/null ||
    fail "connector source A plan changed its exact inventory metadata"
  connector_final_record \
    "${connector_action_a}" "${connector_chunk_a}" "${connector_content_a}" 21 |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${connector_content_a}" 21 |
    ingestion_client_checkpoint "${connector_run_a}"
  connector_client_commit "${connector_run_a}" >/dev/null
  assert_ingestion_staging_empty
  actual="$(ingestion_client_query "
    SELECT applied_action = 'present'
      AND applied_digest = '${connector_digest_a}'
      AND applied_acl_digest = '${connector_acl_alice}'
      AND claim_holder IS NULL
      AND (SELECT applied_revision IS NULL
        FROM knowledge.connector_snapshots WHERE connector_id = 'git-markdown')
    FROM knowledge.connector_sources
    WHERE source_id = '${connector_source_a}'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "source A completion advanced the repository before source B"

  connector_client_claim "${connector_run_b}" >/dev/null
  connector_action_b="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e --arg source_id "${connector_source_b}" '
    .source_id == $source_id and .source_path == "docs/é.md" and .action == "present"
  ' <<<"${connector_action_b}" >/dev/null || fail "connector did not claim source B second"
  connector_pending_record \
    "${connector_action_b}" "${connector_chunk_b}" "${connector_content_b}" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${connector_run_b}" >/dev/null
  connector_final_record \
    "${connector_action_b}" "${connector_chunk_b}" "${connector_content_b}" 22 |
    ingestion_client_write /work/chunks.jsonl
  knowledge_checkpoint_record "${connector_content_b}" 22 |
    ingestion_client_checkpoint "${connector_run_b}"
  connector_client_commit "${connector_run_b}" >/dev/null
  assert_ingestion_staging_empty
  actual="$(ingestion_client_query "
    SELECT applied_revision = desired_revision
      AND applied_inventory_digest = desired_inventory_digest
      AND applied_revision = 'git-snapshot-v1'
      AND NOT EXISTS (
        SELECT 1 FROM knowledge.connector_sources
        WHERE applied_action IS DISTINCT FROM desired_action
          OR applied_revision IS DISTINCT FROM desired_revision
          OR applied_digest IS DISTINCT FROM desired_digest
          OR applied_acl_digest IS DISTINCT FROM desired_acl_digest
          OR applied_metadata IS DISTINCT FROM desired_metadata
      )
    FROM knowledge.connector_snapshots
    WHERE connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "connector v1 cursor did not advance after both exact source actions"

  connector_snapshot_noop="$(connector_snapshot \
    "git-snapshot-noop" \
    "sha256:5555555555555555555555555555555555555555555555555555555555555555" \
    "${connector_inventory_v1}")"
  connector_publish_snapshot "${connector_snapshot_noop}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT snapshots.desired_revision = 'git-snapshot-noop'
      AND snapshots.applied_revision = snapshots.desired_revision
      AND snapshots.applied_inventory_digest = snapshots.desired_inventory_digest
      AND NOT EXISTS (
        SELECT 1 FROM knowledge.connector_sources
        WHERE desired_snapshot_revision <> 'git-snapshot-noop'
          OR applied_snapshot_revision IS DISTINCT FROM desired_snapshot_revision
          OR applied_inventory_digest IS DISTINCT FROM desired_inventory_digest
          OR applied_action IS DISTINCT FROM desired_action
          OR applied_revision IS DISTINCT FROM desired_revision
          OR applied_digest IS DISTINCT FROM desired_digest
          OR applied_acl_digest IS DISTINCT FROM desired_acl_digest
          OR applied_metadata IS DISTINCT FROM desired_metadata
          OR claim_holder IS NOT NULL
      )
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' IN ('${connector_source_a}', '${connector_source_b}')
          AND metadata->>'classification' = 'approved_non_public') = 2
    FROM knowledge.connector_snapshots AS snapshots
    WHERE snapshots.connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "unchanged sources did not fast-forward their newer repository snapshot without model work"
  connector_client_claim "${connector_run_acl}" >/dev/null
  actual="$(ingestion_client_exec cat /work/connector-kind)"
  [[ -z "${actual}" ]] || fail "repository-only connector revision produced a source action"

  connector_inventory_revoke="$(jq -cn \
    --arg source_a "${connector_source_a}" \
    --arg source_b "${connector_source_b}" \
    --arg digest_a "${connector_digest_a}" \
    --arg digest_b "${connector_digest_b}" \
    --arg acl_alice "${connector_acl_alice}" \
    --arg acl_bob "${connector_acl_bob}" \
    --argjson metadata_a "${connector_metadata_a_bob}" \
    --argjson metadata_b "${connector_metadata_b_alice}" '
      [
        {
          source_id: $source_a,
          source_path: "docs/a.md",
          source_revision: $digest_a,
          content_digest: $digest_a,
          acl_digest: $acl_bob,
          metadata: $metadata_a
        },
        {
          source_id: $source_b,
          source_path: "docs/é.md",
          source_revision: $digest_b,
          content_digest: $digest_b,
          acl_digest: $acl_alice,
          metadata: $metadata_b
        }
      ] | sort_by(.source_id)
    ')"
  connector_snapshot_revoke="$(connector_snapshot \
    "git-snapshot-revoke" \
    "sha256:4444444444444444444444444444444444444444444444444444444444444444" \
    "${connector_inventory_revoke}")"
  connector_publish_snapshot "${connector_snapshot_revoke}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT snapshots.applied_revision = 'git-snapshot-noop'
      AND snapshots.desired_revision = 'git-snapshot-revoke'
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = '${connector_source_a}'
          AND metadata->>'classification' = 'authentication') = 1
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = '${connector_source_b}'
          AND metadata->>'classification' = 'approved_non_public') = 1
    FROM knowledge.connector_snapshots AS snapshots
    WHERE snapshots.connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "pending ACL revocation did not quarantine only its changed source"

  connector_snapshot_revert="$(connector_snapshot \
    "git-snapshot-revert" \
    "sha256:3333333333333333333333333333333333333333333333333333333333333333" \
    "${connector_inventory_v1}")"
  connector_publish_snapshot "${connector_snapshot_revert}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT snapshots.applied_revision = 'git-snapshot-noop'
      AND snapshots.desired_revision = 'git-snapshot-revert'
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE source_id = '${connector_source_a}'
          AND applied_acl_digest = desired_acl_digest
          AND applied_snapshot_revision = 'git-snapshot-noop'
          AND desired_snapshot_revision = 'git-snapshot-revert') = 1
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE source_id = '${connector_source_b}'
          AND applied_snapshot_revision = desired_snapshot_revision
          AND desired_snapshot_revision = 'git-snapshot-revert') = 1
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = '${connector_source_a}'
          AND metadata->>'classification' = 'authentication') = 1
    FROM knowledge.connector_snapshots AS snapshots
    WHERE snapshots.connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "reverted ACL incorrectly fast-forwarded a quarantined source"
  connector_client_claim "${connector_run_acl}" >/dev/null
  connector_action_acl="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e \
    --arg source_id "${connector_source_a}" \
    --arg acl_digest "${connector_acl_alice}" '
      .source_id == $source_id and
      .snapshot_revision == "git-snapshot-revert" and
      .acl_digest == $acl_digest
    ' <<<"${connector_action_acl}" >/dev/null ||
    fail "quarantined ACL revert did not produce an exact present action"
  connector_pending_record \
    "${connector_action_acl}" "${connector_chunk_a}" "${connector_content_a}" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${connector_run_acl}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '
    (.embedding | type) == "array" and (.embedding | length) == 1024 and
    .metadata.allowed_principals == [{kind: "matrix", principal: "@alice:org-a.example"}]
  ' <<<"${plan_json}" >/dev/null ||
    fail "quarantined ACL revert did not reuse its prior vector under the restored ACL"
  ingestion_client_copy_plan
  connector_client_commit "${connector_run_acl}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT applied_revision = desired_revision
      AND applied_revision = 'git-snapshot-revert'
      AND NOT EXISTS (
        SELECT 1 FROM knowledge.connector_sources
        WHERE applied_snapshot_revision IS DISTINCT FROM desired_snapshot_revision
          OR applied_inventory_digest IS DISTINCT FROM desired_inventory_digest
      )
    FROM knowledge.connector_snapshots
    WHERE connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "restored ACL action did not advance the exact reverted snapshot"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT
      (SELECT count(*) = 1
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
      AND
      (SELECT count(*) = 0
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "restored connector ACL did not reauthorize only Alice"

  connector_snapshot_blocked="$(jq -cn \
    '{
      connector_id: "git-markdown",
      blocked: true,
      snapshot_revision: "git-snapshot-invalid-acl",
      artifact_digest:
        "sha256:6666666666666666666666666666666666666666666666666666666666666666",
      reason: "artifact-rejected"
    }')"
  connector_publish_snapshot "${connector_snapshot_blocked}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT snapshots.blocked_revision = 'git-snapshot-invalid-acl'
      AND snapshots.blocked_artifact_digest =
        'sha256:6666666666666666666666666666666666666666666666666666666666666666'
      AND snapshots.blocked_at IS NOT NULL
      AND snapshots.applied_revision = 'git-snapshot-revert'
      AND NOT EXISTS (
        SELECT 1 FROM knowledge.connector_sources
        WHERE applied_action IS NOT NULL
          OR applied_revision IS NOT NULL
          OR applied_digest IS NOT NULL
          OR applied_acl_digest IS NOT NULL
          OR applied_metadata IS NOT NULL
          OR applied_snapshot_revision IS NOT NULL
          OR applied_inventory_digest IS NOT NULL
          OR applied_at IS NOT NULL
          OR claim_holder IS NOT NULL
      )
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' IN ('${connector_source_a}', '${connector_source_b}')
          AND metadata->>'classification' = 'authentication') = 2
    FROM knowledge.connector_snapshots AS snapshots
    WHERE snapshots.connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "rejected Ready artifact did not quarantine every prior connector authorization"
  connector_client_claim "${connector_run_acl}" >/dev/null
  actual="$(ingestion_client_exec cat /work/connector-kind)"
  [[ -z "${actual}" ]] || fail "blocked connector snapshot still exposed a claim"

  connector_inventory_v2="$(jq -cn \
    --arg source_id "${connector_source_a}" \
    --arg digest "${connector_digest_a}" \
    --arg acl_digest "${connector_acl_bob}" \
    --argjson metadata "${connector_metadata_a_bob}" '
      [{
        source_id: $source_id,
        source_path: "docs/a.md",
        source_revision: $digest,
        content_digest: $digest,
        acl_digest: $acl_digest,
        metadata: $metadata
      }]
    ')"
  connector_snapshot_v2="$(connector_snapshot \
    "git-snapshot-v2" \
    "sha256:2222222222222222222222222222222222222222222222222222222222222222" \
    "${connector_inventory_v2}")"
  connector_publish_snapshot "${connector_snapshot_v2}" >/dev/null
  actual="$(ingestion_client_query "
    SELECT snapshots.applied_revision = 'git-snapshot-revert'
      AND snapshots.desired_revision = 'git-snapshot-v2'
      AND snapshots.blocked_at IS NULL
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE source_id = '${connector_source_a}'
          AND desired_action = 'present'
          AND applied_digest IS NULL
          AND applied_acl_digest IS NULL) = 1
      AND (SELECT count(*) FROM knowledge.connector_sources
        WHERE source_id = '${connector_source_b}'
          AND desired_action = 'tombstone'
          AND applied_action IS NULL) = 1
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' IN ('${connector_source_a}', '${connector_source_b}')
          AND metadata->>'classification' = 'authentication') = 2
    FROM knowledge.connector_snapshots AS snapshots
    WHERE snapshots.connector_id = 'git-markdown'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "connector v2 did not isolate ACL-only work from the omitted-source tombstone"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT
      (SELECT count(*) = 0
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
      AND
      (SELECT count(*) = 0
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "pending connector ACL change remained retrievable to Alice or Bob"

  connector_client_claim "${connector_run_acl}" >/dev/null
  connector_action_acl="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e \
    --arg source_id "${connector_source_a}" \
    --arg acl_digest "${connector_acl_bob}" '
      .source_id == $source_id and .action == "present" and .acl_digest == $acl_digest and
      .metadata.allowed_principals == [{kind: "matrix", principal: "@bob:org-a.example"}]
    ' <<<"${connector_action_acl}" >/dev/null ||
    fail "ACL-only connector claim did not bind source A to Bob"
  connector_pending_record \
    "${connector_action_acl}" "${connector_chunk_a}" "${connector_content_a}" |
    ingestion_client_write /work/pending.jsonl
  ingestion_client_plan "${connector_run_acl}" >/dev/null
  plan_json="$(ingestion_client_exec cat /work/plan.jsonl)"
  jq -e '
    (.embedding | type) == "array" and (.embedding | length) == 1024 and
    .embedding[21] == 1 and
    .metadata.allowed_principals == [{kind: "matrix", principal: "@bob:org-a.example"}]
  ' <<<"${plan_json}" >/dev/null ||
    fail "ACL-only connector update did not reuse the exact prior embedding"
  ingestion_client_copy_plan
  connector_client_commit "${connector_run_acl}" >/dev/null
  assert_ingestion_staging_empty
  actual="$(ingestion_client_query "
    SELECT metadata->'allowed_principals' =
        '[{\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}]'::jsonb
      AND (embedding::real[])[22] = 1
      AND (SELECT applied_revision = 'git-snapshot-revert'
        FROM knowledge.connector_snapshots WHERE connector_id = 'git-markdown')
    FROM knowledge.chunks
    WHERE chunk_id = '${connector_chunk_a}'
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "ACL-only connector commit changed the vector or advanced past the tombstone"
  actual="$(retrieval_sql --tuples-only --no-align --command="
    SELECT
      (SELECT count(*) = 1
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@bob:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
      AND
      (SELECT count(*) = 0
        FROM knowledge.search_authorized_matrix(
          ${query_vector},
          ARRAY['approved_non_public', 'public']::text[],
          '[{\"kind\":\"matrix\",\"principal\":\"@alice:org-a.example\"}]'::jsonb,
          50
        )
        WHERE metadata #>> '{source,id}' = '${connector_source_a}')
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "applied connector ACL did not admit only Bob to source A"

  connector_client_claim "${connector_run_old}" >/dev/null
  connector_action_tombstone="$(ingestion_client_exec cat /work/connector-action.json)"
  jq -e --arg source_id "${connector_source_b}" '
    .source_id == $source_id and .action == "tombstone" and
    .acl_digest == null and .metadata == null
  ' <<<"${connector_action_tombstone}" >/dev/null ||
    fail "omitted connector source did not produce an exact tombstone action"
  ingestion_sql --quiet --command="
    UPDATE knowledge.connector_sources
    SET claimed_at = clock_timestamp() - interval '30 minutes',
        claim_expires_at = clock_timestamp() - interval '1 second'
    WHERE claim_holder = '${connector_run_old}'::uuid
  " >/dev/null
  connector_client_claim "${connector_run_new}" >/dev/null
  connector_action_reclaimed="$(ingestion_client_exec cat /work/connector-action.json)"
  connector_action_reclaimed_normalized="$(
    jq -cS 'del(.claim_expires_at)' <<<"${connector_action_reclaimed}"
  )"
  connector_action_tombstone_normalized="$(
    jq -cS 'del(.claim_expires_at)' <<<"${connector_action_tombstone}"
  )"
  [[ "${connector_action_reclaimed_normalized}" == "${connector_action_tombstone_normalized}" ]] ||
    fail "expired connector tombstone reclaim changed the bound action"
  actual="$(ingestion_client_query "
    SELECT claim_holder = '${connector_run_new}'::uuid
      AND claimed_at IS NOT NULL
      AND claim_expires_at > clock_timestamp()
    FROM knowledge.connector_sources
    WHERE source_id = '${connector_source_b}'
  ")"
  [[ "${actual}" == "t" ]] || fail "expired connector claim was not safely reclaimed"

  ingestion_sql --quiet --command="
    INSERT INTO knowledge.ingestion_embedding_cache (
      profile, source_id, content_sha256, content, embedding, cached_at, expires_at
    ) VALUES
      (
        'bge-m3-1024-v1', '${connector_source_a}',
        encode(sha256(convert_to('connector cache A', 'UTF8')), 'hex'),
        'connector cache A',
        (ARRAY[1::real] || array_fill(0::real, ARRAY[1023]))::vector(1024),
        clock_timestamp(), clock_timestamp() + interval '1 hour'
      ),
      (
        'bge-m3-1024-v1', '${connector_source_b}',
        encode(sha256(convert_to('connector cache B', 'UTF8')), 'hex'),
        'connector cache B',
        (ARRAY[0::real, 1::real] || array_fill(0::real, ARRAY[1022]))::vector(1024),
        clock_timestamp(), clock_timestamp() + interval '1 hour'
      )
  " >/dev/null
  connector_client_tombstone "${connector_run_new}" >/dev/null
  assert_ingestion_staging_empty
  actual="$(ingestion_client_query "
    SELECT
      (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = '${connector_source_a}') = 1
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = '${connector_source_b}') = 0
      AND (SELECT count(*) FROM knowledge.chunks
        WHERE metadata #>> '{source,id}' = 'source-matrix-shared') = 1
      AND (SELECT count(*) FROM knowledge.ingestion_embedding_cache
        WHERE source_id = '${connector_source_a}') = 1
      AND (SELECT count(*) FROM knowledge.ingestion_embedding_cache
        WHERE source_id = '${connector_source_b}') = 0
      AND (SELECT applied_action = 'tombstone' AND claim_holder IS NULL
        FROM knowledge.connector_sources WHERE source_id = '${connector_source_b}')
      AND (SELECT applied_action = 'present' AND claim_holder IS NULL
        FROM knowledge.connector_sources WHERE source_id = '${connector_source_a}')
      AND (SELECT applied_revision = desired_revision
        AND applied_inventory_digest = desired_inventory_digest
        AND applied_revision = 'git-snapshot-v2'
        FROM knowledge.connector_snapshots WHERE connector_id = 'git-markdown')
      AND NOT EXISTS (
        SELECT 1 FROM knowledge.connector_sources
        WHERE applied_action IS DISTINCT FROM desired_action
          OR applied_revision IS DISTINCT FROM desired_revision
          OR applied_digest IS DISTINCT FROM desired_digest
          OR applied_acl_digest IS DISTINCT FROM desired_acl_digest
          OR applied_metadata IS DISTINCT FROM desired_metadata
          OR claim_holder IS NOT NULL
      )
  ")"
  [[ "${actual}" == "t" ]] ||
    fail "connector tombstone did not isolate deletion, cache cleanup, and cursor advancement"

  if connector_sql --command='SELECT count(*) FROM knowledge.chunks' >/dev/null 2>&1; then
    fail "knowledge connector unexpectedly read chunk content"
  fi
  if connector_sql --command="
    SELECT knowledge.apply_connector_tombstone('${connector_run_new}'::uuid)
  " >/dev/null 2>&1; then
    fail "knowledge connector unexpectedly executed the ingestion tombstone function"
  fi
  if ingestion_sql --command="
    SELECT knowledge.begin_connector_snapshot(
      'git-markdown', 'forbidden',
      'sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', 0
    )
  " >/dev/null 2>&1; then
    fail "knowledge ingestion unexpectedly opened a connector snapshot"
  fi
  if ingestion_sql --command="
    UPDATE knowledge.connector_sources SET desired_action = 'present'
    WHERE false
  " >/dev/null 2>&1; then
    fail "knowledge ingestion unexpectedly changed desired connector state"
  fi
  if ingestion_sql --command="
    INSERT INTO knowledge.connector_inventory
    SELECT * FROM knowledge.connector_inventory WHERE false
  " >/dev/null 2>&1; then
    fail "knowledge ingestion unexpectedly published connector inventory"
  fi
  if retrieval_sql --command='SELECT count(*) FROM knowledge.connector_snapshots' \
    >/dev/null 2>&1; then
    fail "knowledge retrieval unexpectedly read connector control state"
  fi
  if connector_sql --command="
    SELECT knowledge.begin_connector_snapshot(
      'other-connector', 'forbidden',
      'sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb', 0
    )
  " >/dev/null 2>&1; then
    fail "knowledge connector escaped its exact database-bound identity"
  fi

  echo "Knowledge store runtime contract passed (${chart} ${chart_version}, ${primary})"
}

static_contract
if ${runtime}; then
  runtime_contract
fi
