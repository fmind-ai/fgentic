#!/usr/bin/env bash
# Validate the disabled-by-default sovereign ingestion contract without starting Docker or a cluster.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
KNOWLEDGE_DIR="${ROOT_DIR}/infra/knowledge"
PYTHON_IMAGE="python:3.14-slim@sha256:b877e50bd90de10af8d82c57a022fc2e0dc731c5320d762a27986facfc3355c1"
DOCLING_IMAGE="quay.io/docling-project/docling-serve-cpu:v1.26.0@sha256:7e07522e0240c1db3ff5b837ffa969c2ecd5a71664c0e0369f5a69fc169e30ba"
POSTGRES_IMAGE="ghcr.io/cloudnative-pg/postgresql:17.10-202607130907-system-trixie@sha256:c141aec61cab8da3e215aebe0fa155e78442fbb41c982a86743915a967e12af9"

fail() {
	echo "error: $1" >&2
	exit 1
}

for command in helm jq kubeconform kubectl rg yq; do
	command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-knowledge-ingestion.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT

kubectl kustomize "${KNOWLEDGE_DIR}" >"${tmp_dir}/root.yaml"
kubectl kustomize "${KNOWLEDGE_DIR}/profiles/enabled" >"${tmp_dir}/enabled.yaml"
kubectl kustomize "${KNOWLEDGE_DIR}/profiles/disabled" >"${tmp_dir}/disabled.yaml"
kubectl kustomize "${ROOT_DIR}/scripts/testdata/knowledge-ingestion-cluster" \
	--load-restrictor LoadRestrictionsNone >"${tmp_dir}/cluster-enabled.yaml"
kubectl kustomize "${ROOT_DIR}/scripts/testdata/knowledge-ingestion-agentgateway" \
	--load-restrictor LoadRestrictionsNone >"${tmp_dir}/agentgateway-enabled.yaml"

gateway_version="$(
	yq eval-all -N -r '
    select(.kind == "OCIRepository" and .metadata.name == "agentgateway") |
    .spec.ref.tag
  ' "${ROOT_DIR}/infra/flux/sources.yaml"
)"
[[ -n "${gateway_version}" && "${gateway_version}" != "null" ]] \
	|| fail "agentgateway version pin is missing"
helm template agentgateway-crds oci://cr.agentgateway.dev/charts/agentgateway-crds \
	--version "${gateway_version}" >"${tmp_dir}/agentgateway-crds.yaml"
for kind in AgentgatewayBackend AgentgatewayPolicy; do
	case "${kind}" in
		AgentgatewayBackend) crd_name=agentgatewaybackends.agentgateway.dev ;;
		AgentgatewayPolicy) crd_name=agentgatewaypolicies.agentgateway.dev ;;
		*) fail "unsupported agentgateway CRD kind: ${kind}" ;;
	esac
	yq -o=json \
		"select(.kind == \"CustomResourceDefinition\" and .metadata.name == \"${crd_name}\") |
      .spec.versions[] | select(.name == \"v1alpha1\") | .schema.openAPIV3Schema" \
		"${tmp_dir}/agentgateway-crds.yaml" >"${tmp_dir}/${kind}-schema.json"
	jq -e '.type == "object" and (.properties.spec | type == "object")' \
		"${tmp_dir}/${kind}-schema.json" >/dev/null \
		|| fail "pinned ${kind} schema was not rendered"
	yq eval-all "select(.kind == \"${kind}\")" "${tmp_dir}/enabled.yaml" \
		| kubeconform -strict -summary -schema-location "${tmp_dir}/${kind}-schema.json"
done

cmp -s "${tmp_dir}/root.yaml" "${tmp_dir}/disabled.yaml" \
	|| fail "root and disabled knowledge-ingestion renders differ"
[ ! -s "${tmp_dir}/disabled.yaml" ] \
	|| fail "disabled knowledge-ingestion profile must render zero objects"

objects="$(
	yq eval-all -o=json '
    select(.metadata.labels."app.kubernetes.io/component" != "source-connector")
  ' "${tmp_dir}/enabled.yaml" | jq --slurp '.'
)"
cronjob="$(
	yq eval-all -o=json 'select(.kind == "CronJob" and .metadata.name == "knowledge-ingestion")' \
		"${tmp_dir}/enabled.yaml"
)"
cache_gc_cronjob="$(
	yq eval-all -o=json \
		'select(.kind == "CronJob" and .metadata.name == "knowledge-ingestion-cache-gc")' \
		"${tmp_dir}/enabled.yaml"
)"
route="$(
	yq eval-all -o=json 'select(.kind == "HTTPRoute" and .metadata.name == "knowledge-embeddings")' \
		"${tmp_dir}/enabled.yaml"
)"
backend="$(
	yq eval-all -o=json \
		'select(.kind == "AgentgatewayBackend" and .metadata.name == "knowledge-embeddings")' \
		"${tmp_dir}/enabled.yaml"
)"
tokenizer_backend="$(
	yq eval-all -o=json \
		'select(.kind == "AgentgatewayBackend" and .metadata.name == "knowledge-tokenizer")' \
		"${tmp_dir}/enabled.yaml"
)"
policy="$(
	yq eval-all -o=json \
		'select(.kind == "AgentgatewayPolicy" and .metadata.name == "knowledge-ingestion-authorization")' \
		"${tmp_dir}/enabled.yaml"
)"
runtime_config="$(
	yq eval-all -o=json \
		'select(.kind == "ConfigMap" and (.metadata.name | test("^knowledge-ingestion-runtime-")))' \
		"${tmp_dir}/enabled.yaml"
)"
cache_gc_runtime_config="$(
	yq eval-all -o=json \
		'select(.kind == "ConfigMap" and
      (.metadata.name | test("^knowledge-ingestion-cache-gc-runtime-")))' \
		"${tmp_dir}/enabled.yaml"
)"
cache_gc_runtime_name="$(jq -r '.metadata.name' <<<"${cache_gc_runtime_config}")"
cache_gc_network_policy="$(
	yq eval-all -o=json \
		'select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-ingestion-cache-gc")' \
		"${tmp_dir}/enabled.yaml"
)"

normalized_objects="$(
	jq -c '
    map(if .kind == "ConfigMap" and (.metadata.name | startswith("knowledge-ingestion-runtime-"))
      then .metadata.name = "knowledge-ingestion-runtime-PLACEHOLDER"
      elif .kind == "ConfigMap" and
        (.metadata.name | startswith("knowledge-ingestion-cache-gc-runtime-"))
      then .metadata.name = "knowledge-ingestion-cache-gc-runtime-PLACEHOLDER"
      else .
    end)
  ' <<<"${objects}"
)"
jq -e '
  ([.[] | .kind + "/" + .metadata.namespace + "/" + .metadata.name] | sort) == [
    "AgentgatewayBackend/agentgateway-system/knowledge-embeddings",
    "AgentgatewayBackend/agentgateway-system/knowledge-tokenizer",
    "AgentgatewayPolicy/agentgateway-system/knowledge-ingestion-authorization",
    "ConfigMap/knowledge/knowledge-ingestion-cache-gc-runtime-PLACEHOLDER",
    "ConfigMap/knowledge/knowledge-ingestion-runtime-PLACEHOLDER",
    "CronJob/knowledge/knowledge-ingestion",
    "CronJob/knowledge/knowledge-ingestion-cache-gc",
    "HTTPRoute/agentgateway-system/knowledge-embeddings",
    "NetworkPolicy/agentgateway-system/agentgateway-allow-knowledge-ingestion",
    "NetworkPolicy/knowledge/knowledge-ingestion",
    "NetworkPolicy/knowledge/knowledge-ingestion-cache-gc",
    "ServiceAccount/knowledge/knowledge-ingestion"
  ] | if . then true else error("inventory") end
' <<<"${normalized_objects}" >/dev/null || fail "enabled knowledge-ingestion resource inventory drifted"
jq -e '
  ([.[] | select(.kind == "PersistentVolumeClaim")] | length) == 0 and
  ([.[] | select(
    .kind == "ConfigMap" and .metadata.name == "knowledge-source-bundle"
  )] | length) == 0
' <<<"${objects}" >/dev/null \
	|| fail "ingestion must consume operator-owned source storage without rendering corpus storage"

jq -e '
  (.data | keys | sort) == [
    "checkpoint.sql",
    "connector-claim.sql",
    "connector-publish.sql",
    "connector-tombstone.sql",
    "connector_runtime.py",
    "ingestion.py",
    "plan.sql",
    "write.sql"
  ]
' <<<"${runtime_config}" >/dev/null \
	|| fail "ingestion runtime ConfigMap inventory drifted"
jq -e --rawfile gc_sql "${KNOWLEDGE_DIR}/base/gc.sql" '
  (.data | keys) == ["gc.sql"] and
  .data["gc.sql"] == $gc_sql
' <<<"${cache_gc_runtime_config}" >/dev/null \
	|| fail "cache-GC runtime ConfigMap must contain only the exact gc.sql program"

jq -e \
	--arg python "${PYTHON_IMAGE}" \
	--arg docling "${DOCLING_IMAGE}" \
	--arg postgres "${POSTGRES_IMAGE}" '
  .metadata.namespace == "knowledge" and
  .spec.suspend == true and
  .spec.concurrencyPolicy == "Forbid" and
  .spec.jobTemplate.spec.activeDeadlineSeconds == 1800 and
  .spec.jobTemplate.spec.backoffLimit == 0 and
  .spec.jobTemplate.spec.template.spec.serviceAccountName == "knowledge-ingestion" and
  .spec.jobTemplate.spec.template.spec.automountServiceAccountToken == false and
  .spec.jobTemplate.spec.template.spec.enableServiceLinks == false and
  .spec.jobTemplate.spec.template.spec.restartPolicy == "Never" and
  .spec.jobTemplate.spec.template.spec.securityContext == {
    "fsGroup": 2000,
    "fsGroupChangePolicy": "OnRootMismatch",
    "runAsNonRoot": true,
    "seccompProfile": {"type": "RuntimeDefault"}
  } and
  ([.spec.jobTemplate.spec.template.spec.initContainers[].name] ==
    [
      "connector-publish",
      "connector-claim",
      "connector-dispatch",
      "snapshot",
      "parse",
      "bind",
      "plan",
      "checkpoint",
      "embed"
    ]) and
  ([.spec.jobTemplate.spec.template.spec.containers[].name] == ["write"]) and
  ([.spec.jobTemplate.spec.template.spec.initContainers[],
    .spec.jobTemplate.spec.template.spec.containers[]] |
    length == 10 and
    all(.[];
      (.resources.requests | keys | sort) ==
        ["cpu", "ephemeral-storage", "memory"] and
      (.resources.limits | keys | sort) ==
        ["cpu", "ephemeral-storage", "memory"] and
      all(.resources.requests[];
        (type == "string" or type == "number") and
        ((tostring | length) > 0)) and
      all(.resources.limits[];
        (type == "string" or type == "number") and
        ((tostring | length) > 0)) and
      .securityContext.allowPrivilegeEscalation == false and
      .securityContext.readOnlyRootFilesystem == true and
      .securityContext.capabilities.drop == ["ALL"])) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "connector-publish") |
    .image == $postgres and
    (.args[0] | contains("inventory=/sources/.connector/git-markdown/current.json") and
      contains("[[ ! -e \"$inventory\" && ! -L \"$inventory\" ]]") and
      contains("[[ ! -f \"$inventory\" || -L \"$inventory\" || ! -s \"$inventory\" ]]") and
      contains("connector inventory exists but is not a non-empty regular file") and
      contains("--file=/runtime/connector-publish.sql") and
      contains("/dispatch/manual")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      .valueFrom.secretKeyRef.name] | unique) == ["pg-knowledge-connector"] and
    ([.volumeMounts[].name] | sort) == ["dispatch", "runtime", "sources", "tmp"] and
    [.volumeMounts[] | select(.name == "sources") | .readOnly] == [true]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "connector-claim") |
    .image == $postgres and
    (.args[0] | contains("--file=/runtime/connector-claim.sql") and
      contains("/dispatch/manual")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      .valueFrom.secretKeyRef.name] | unique) == ["pg-knowledge-ingestion"] and
    ([.volumeMounts[].name] | sort) == ["dispatch", "runtime", "tmp", "work"] and
    [.volumeMounts[] | select(.name == "dispatch") | .readOnly] == [true]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "connector-dispatch") |
    .image == $postgres and
    (.args[0] | contains("/work/connector-kind") and
      contains("--file=/runtime/connector-tombstone.sql") and
      contains("/dispatch/noop")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      .valueFrom.secretKeyRef.name] | unique) == ["pg-knowledge-ingestion"] and
    ([.volumeMounts[].name] | sort) == ["dispatch", "runtime", "tmp", "work"] and
    [.volumeMounts[] | select(.name == "work") | .readOnly] == [true]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "snapshot") |
    .image == $python and
    (.args[0] | contains("/runtime/connector_runtime.py materialize") and
      contains("--source-root /sources/.connector/git-markdown") and
      contains("--output-root /selected/bundle") and
      contains("manifest=/selected/bundle/manifest.json") and
      contains("source_root=/selected/bundle") and
      contains("/runtime/ingestion.py snapshot") and
      contains("--raw-root /work/raw") and
      contains("--tmp-root /parser-tmp")) and
    ([.volumeMounts[].name] | sort) ==
      [
        "dispatch",
        "parser-tmp",
        "runtime",
        "selected",
        "snapshot",
        "sources",
        "tmp",
        "work"
      ] and
    ([.volumeMounts[] | select(.name == "sources")] == [{
      "name": "sources",
      "mountPath": "/sources",
      "readOnly": true
    }])) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "parse") |
    .image == $docling and
    (.args[0] | contains("/dispatch/noop") and
      contains("/runtime/ingestion.py parse-isolated") and
      contains("--source-root /input") and
      contains("--output /output/chunks.jsonl")) and
    .volumeMounts == [
      {"name": "runtime", "mountPath": "/runtime", "readOnly": true},
      {
        "name": "snapshot",
        "mountPath": "/input",
        "readOnly": true,
        "subPath": "parser"
      },
      {"name": "work", "mountPath": "/output", "subPath": "raw"},
      {"name": "parser-tmp", "mountPath": "/tmp"},
      {"name": "dispatch", "mountPath": "/dispatch", "readOnly": true}
    ] and
    ([.env[] | select(.name == "HF_HUB_OFFLINE" or
      .name == "TRANSFORMERS_OFFLINE" or .name == "DOCLING_ARTIFACTS_PATH") |
      [.name, .value]] | sort) == [
        ["DOCLING_ARTIFACTS_PATH", "/opt/app-root/src/.cache/docling/models"],
        ["HF_HUB_OFFLINE", "1"],
        ["TRANSFORMERS_OFFLINE", "1"]
      ]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "bind") |
    .image == $python and
    (.args[0] | contains("/dispatch/noop") and
      contains("/runtime/ingestion.py bind") and
      contains("--raw-root /work/raw")) and
    ([.volumeMounts[].name] | sort) == ["dispatch", "runtime", "snapshot", "tmp", "work"]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "plan") |
    .image == $postgres and
    (.args[0] | contains("/dispatch/noop") and contains("--file=/runtime/plan.sql")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      .valueFrom.secretKeyRef.name] | unique) == ["pg-knowledge-ingestion"] and
    ([.volumeMounts[].name] | sort) == ["dispatch", "runtime", "tmp", "work"]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "checkpoint") |
    .image == $postgres and
    .restartPolicy == "Always" and
    (.args[0] | contains("--file=/runtime/checkpoint.sql") and
      contains("/checkpoint/checkpoint.ready") and
      contains("/checkpoint/checkpoint.acked") and
      contains("/dispatch/noop")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      .valueFrom.secretKeyRef.name] | unique) == ["pg-knowledge-ingestion"] and
    ([.volumeMounts[].name] | sort) == ["checkpoint", "dispatch", "runtime"]) and
  (.spec.jobTemplate.spec.template.spec.initContainers[] |
    select(.name == "embed") |
    .image == $python and
    (.args[0] | contains("/dispatch/noop") and
      contains("http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8082/v1/embeddings") and
      contains("--authorization-file /credentials/authorization") and
      contains("--checkpoint-root /checkpoint")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null)] | length) == 0 and
    ([.volumeMounts[].name] | sort) ==
      ["checkpoint", "credentials", "dispatch", "runtime", "tmp", "work"]) and
  (.spec.jobTemplate.spec.template.spec.containers[] |
    select(.name == "write") |
    .image == $postgres and
    (.args[0] | contains("/dispatch/noop") and
      contains("--set=connector_action=true") and
      contains("--file=/runtime/write.sql")) and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      .valueFrom.secretKeyRef.name] | unique) == ["pg-knowledge-ingestion"] and
    ([.volumeMounts[].name] | sort) == ["dispatch", "runtime", "tmp", "work"]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.secret != null) | [.name, .secret.secretName]] ==
    [["credentials", "knowledge-ingestion-credential"]]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "credentials") | .secret.defaultMode] == [288]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "sources")] == [{
      "name": "sources",
      "persistentVolumeClaim": {
        "claimName": "knowledge-source-bundle",
        "readOnly": true
      }
    }]) and
  ([.spec.jobTemplate.spec.template.spec.initContainers[],
    .spec.jobTemplate.spec.template.spec.containers[] |
    select(any(.volumeMounts[]?; .name == "sources")) |
    .name] == ["connector-publish", "snapshot"]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "parser-tmp")] == [{
      "name": "parser-tmp",
      "emptyDir": {"sizeLimit": "1Gi"}
    }]) and
  ([.spec.jobTemplate.spec.template.spec.initContainers[],
    .spec.jobTemplate.spec.template.spec.containers[] |
    select(any(.volumeMounts[]?; .name == "parser-tmp")) |
    .name] == ["snapshot", "parse"]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "work") | .emptyDir.sizeLimit] == ["320Mi"]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "checkpoint") | .emptyDir.sizeLimit] == ["2Mi"]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "selected") | .emptyDir.sizeLimit] == ["32Mi"]) and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.name == "dispatch") | .emptyDir.sizeLimit] == ["1Mi"])
' <<<"${cronjob}" >/dev/null \
	|| fail "CronJob trust phases, images, credentials, or bounds drifted"

jq -e \
	--arg postgres "${POSTGRES_IMAGE}" \
	--arg runtime_name "${cache_gc_runtime_name}" '
  .metadata.namespace == "knowledge" and
  .metadata.labels == {
    "app.kubernetes.io/name": "knowledge-ingestion-cache-gc",
    "app.kubernetes.io/component": "cache-gc",
    "app.kubernetes.io/part-of": "fgentic"
  } and
  .spec.schedule == "17 * * * *" and
  .spec.suspend == false and
  .spec.concurrencyPolicy == "Forbid" and
  .spec.startingDeadlineSeconds == 1800 and
  .spec.successfulJobsHistoryLimit == 1 and
  .spec.failedJobsHistoryLimit == 1 and
  .spec.jobTemplate.spec.activeDeadlineSeconds == 300 and
  .spec.jobTemplate.spec.backoffLimit == 2 and
  .spec.jobTemplate.spec.ttlSecondsAfterFinished == 86400 and
  .spec.jobTemplate.spec.template.metadata.labels == {
    "app.kubernetes.io/name": "knowledge-ingestion-cache-gc",
    "app.kubernetes.io/component": "cache-gc",
    "app.kubernetes.io/part-of": "fgentic"
  } and
  .spec.jobTemplate.spec.template.spec.serviceAccountName == "knowledge-ingestion" and
  .spec.jobTemplate.spec.template.spec.automountServiceAccountToken == false and
  .spec.jobTemplate.spec.template.spec.enableServiceLinks == false and
  .spec.jobTemplate.spec.template.spec.restartPolicy == "Never" and
  .spec.jobTemplate.spec.template.spec.securityContext == {
    "runAsNonRoot": true,
    "seccompProfile": {"type": "RuntimeDefault"}
  } and
  ([.spec.jobTemplate.spec.template.spec.initContainers[]?] | length) == 0 and
  ([.spec.jobTemplate.spec.template.spec.containers[]] | length) == 1 and
  (.spec.jobTemplate.spec.template.spec.containers[0] |
    .name == "cache-gc" and
    .image == $postgres and
    .imagePullPolicy == "IfNotPresent" and
    .command == ["psql"] and
    .args == ["--quiet", "--no-psqlrc", "--file=/runtime/gc.sql"] and
    [.env[].name] == [
      "PGHOST",
      "PGPORT",
      "PGDATABASE",
      "PGUSER",
      "PGPASSWORD",
      "PGSSLMODE",
      "PGCONNECT_TIMEOUT",
      "HOME"
    ] and
    ([.env[] | select(.valueFrom.secretKeyRef != null) |
      {
        name: .name,
        secret: .valueFrom.secretKeyRef.name,
        key: .valueFrom.secretKeyRef.key
      }] | sort_by(.name)) == [
        {
          "name": "PGPASSWORD",
          "secret": "pg-knowledge-ingestion",
          "key": "password"
        },
        {
          "name": "PGUSER",
          "secret": "pg-knowledge-ingestion",
          "key": "username"
        }
      ] and
    ([.env[] | select(.valueFrom != null and .valueFrom.secretKeyRef == null)] |
      length) == 0 and
    ([.env[] | select(.value != null) | {name: .name, value: .value}] |
      sort_by(.name)) == [
        {"name": "HOME", "value": "/tmp"},
        {"name": "PGCONNECT_TIMEOUT", "value": "3"},
        {"name": "PGDATABASE", "value": "knowledge"},
        {
          "name": "PGHOST",
          "value": "platform-pg-rw.postgres.svc.cluster.local"
        },
        {"name": "PGPORT", "value": "5432"},
        {"name": "PGSSLMODE", "value": "require"}
      ] and
    .resources.requests == {
      "cpu": "10m",
      "memory": "32Mi",
      "ephemeral-storage": "8Mi"
    } and
    .resources.limits == {
      "cpu": "100m",
      "memory": "64Mi",
      "ephemeral-storage": "32Mi"
    } and
    .securityContext == {
      "allowPrivilegeEscalation": false,
      "capabilities": {"drop": ["ALL"]},
      "readOnlyRootFilesystem": true
    } and
    .volumeMounts == [
      {"name": "runtime", "mountPath": "/runtime", "readOnly": true},
      {"name": "tmp", "mountPath": "/tmp"}
    ]) and
  .spec.jobTemplate.spec.template.spec.volumes == [
    {
      "name": "runtime",
      "configMap": {
        "name": $runtime_name,
        "defaultMode": 292
      }
    },
    {
      "name": "tmp",
      "emptyDir": {"sizeLimit": "8Mi"}
    }
  ] and
  ([.spec.jobTemplate.spec.template.spec.volumes[] |
    select(.projected != null or .secret != null)] | length) == 0
' <<<"${cache_gc_cronjob}" >/dev/null \
	|| fail "cache-GC CronJob schedule, isolation, credentials, or bounds drifted"

jq -e '
  .metadata.namespace == "knowledge" and
  .spec.podSelector == {
    "matchLabels": {
      "app.kubernetes.io/name": "knowledge-ingestion-cache-gc"
    }
  } and
  .spec.policyTypes == ["Ingress", "Egress"] and
  .spec.ingress == [] and
  .spec.egress == [
    {
      "to": [{
        "namespaceSelector": {
          "matchLabels": {
            "kubernetes.io/metadata.name": "kube-system"
          }
        },
        "podSelector": {
          "matchLabels": {"k8s-app": "kube-dns"}
        }
      }],
      "ports": [
        {"protocol": "UDP", "port": 53},
        {"protocol": "TCP", "port": 53}
      ]
    },
    {
      "to": [{
        "namespaceSelector": {
          "matchLabels": {
            "kubernetes.io/metadata.name": "postgres"
          }
        },
        "podSelector": {
          "matchLabels": {"cnpg.io/cluster": "platform-pg"}
        }
      }],
      "ports": [{"protocol": "TCP", "port": 5432}]
    }
  ]
' <<<"${cache_gc_network_policy}" >/dev/null \
	|| fail "cache-GC egress must be only DNS and exact platform-pg, with no ingress"

jq -e '
  .spec.parentRefs == [{"name": "agentgateway-proxy", "sectionName": "embeddings"}] and
  .spec.rules == [
    {
      "matches": [{"path": {"type": "Exact", "value": "/v1/embeddings"}}],
      "backendRefs": [{
        "name": "knowledge-embeddings",
        "group": "agentgateway.dev",
        "kind": "AgentgatewayBackend"
      }]
    },
    {
      "matches": [{"path": {"type": "Exact", "value": "/tokenize"}}],
      "backendRefs": [{
        "name": "knowledge-tokenizer",
        "group": "agentgateway.dev",
        "kind": "AgentgatewayBackend"
      }]
    }
  ]
' <<<"${route}" >/dev/null || fail "dedicated embeddings route drifted"

jq -e '
  .spec.ai.provider.custom.model == "BAAI/bge-m3" and
  .spec.ai.provider.custom.formats == [{"type": "Embeddings", "path": "/v1/embeddings"}] and
  .spec.ai.provider.host == "knowledge-embeddings.models.svc.cluster.local" and
  .spec.ai.provider.port == 8000 and
  .spec.policies.ai.routes == {"/v1/embeddings": "Embeddings"} and
  .spec.policies.http.requestTimeout == "30s" and
  (.spec | has("static") | not)
' <<<"${backend}" >/dev/null || fail "governed custom embeddings backend drifted"

jq -e '
  .spec.static == {
    "host": "knowledge-embeddings.models.svc.cluster.local",
    "port": 8000
  } and
  (.spec | keys | sort) == ["static"]
' <<<"${tokenizer_backend}" >/dev/null || fail "model-local tokenizer backend drifted"

jq -e '
  .spec.targetRefs == [{
    "group": "gateway.networking.k8s.io",
    "kind": "HTTPRoute",
    "name": "knowledge-embeddings"
  }] and
  .spec.traffic.buffer.request.maxBytes == "128Ki" and
  .spec.traffic.apiKeyAuthentication == {
    "mode": "Strict",
    "secretRef": {"name": "knowledge-ingestion-callers"}
  } and
  .spec.traffic.authorization.action == "Require" and
  (.spec.traffic.authorization.policy.matchExpressions[0] |
    contains("apiKey.workload == \"knowledge-ingestion\"") and
    contains("request.method == \"POST\"") and
    contains("request.path == \"/tokenize\"") and
    contains("request.path == \"/v1/embeddings\"")) and
  .spec.traffic.rateLimit.local == [{"requests": 1024, "unit": "Hours"}]
' <<<"${policy}" >/dev/null || fail "embedding authentication or rate bound drifted"

gateway="$(
	yq eval-all -o=json \
		'select(.kind == "Gateway" and .metadata.name == "agentgateway-proxy")' \
		"${tmp_dir}/agentgateway-enabled.yaml"
)"
jq -e '
  (.spec.listeners | length) == 2 and
  .spec.listeners[0].name == "default" and
  .spec.listeners[0].protocol == "HTTP" and
  .spec.listeners[0].port == 8080 and
  .spec.listeners[0].allowedRoutes.namespaces.from == "All" and
  .spec.listeners[1].name == "embeddings" and
  .spec.listeners[1].protocol == "HTTP" and
  .spec.listeners[1].port == 8082 and
  .spec.listeners[1].allowedRoutes.namespaces.from == "Same"
' <<<"${gateway}" >/dev/null \
	|| fail "agentgateway dedicated listener drifted"

network_policy="$(
	yq eval-all -o=json \
		'select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-ingestion")' \
		"${KNOWLEDGE_DIR}/base/networkpolicy.yaml"
)"
jq -e '
  .metadata.namespace == "knowledge" and
  .spec.ingress == [] and
  .spec.policyTypes == ["Ingress", "Egress"] and
  ([.spec.egress[].ports[].port] | sort) == [53, 53, 5432, 8082]
' <<<"${network_policy}" >/dev/null \
	|| fail "ingestion egress is broader than DNS, scoped PostgreSQL, and :8082"

for example in source-bundle.example.yaml source-bundle-partner.example.yaml; do
	case "${example}" in
		source-bundle.example.yaml)
			fixture_name="knowledge-source-bundle-public-test-fixture"
			;;
		source-bundle-partner.example.yaml)
			fixture_name="knowledge-source-bundle-partner-public-test-fixture"
			;;
		*) fail "unsupported knowledge source bundle fixture: ${example}" ;;
	esac
	yq -o=json '.' "${KNOWLEDGE_DIR}/${example}" \
		| jq -e --arg fixture_name "${fixture_name}" '
    .kind == "ConfigMap" and
    .immutable == true and
    .metadata.name == $fixture_name and
    .metadata.namespace == "knowledge" and
    .metadata.annotations == {
      "fgentic.fmind.ai/corpus-content": "synthetic-public",
      "fgentic.fmind.ai/test-fixture-only": "true"
    } and
    (.data["manifest.json"] | fromjson |
      .schema_version == 1 and
      .corpus == "reference-docs" and
      (.sources | length) == 1 and
      (.sources[0].digest | test("^sha256:[0-9a-f]{64}$")))
  ' >/dev/null \
		|| fail "synthetic public one-source test fixture drifted: ${example}"
	if rg --fixed-strings --glob kustomization.yaml --quiet "${example}" "${KNOWLEDGE_DIR}"; then
		fail "offline source fixture must not be referenced by a production kustomization: ${example}"
	fi
done

mkdir -p "${tmp_dir}/sources"
yq -o=json '.' "${KNOWLEDGE_DIR}/source-bundle.example.yaml" \
	| jq -j '.data["manifest.json"]' >"${tmp_dir}/manifest.json"
yq -o=json '.' "${KNOWLEDGE_DIR}/source-bundle.example.yaml" \
	| jq -j '.data["matrix-principal.md"]' >"${tmp_dir}/sources/matrix-principal.md"
PYTHONDONTWRITEBYTECODE=1 python \
	"${KNOWLEDGE_DIR}/base/ingestion.py" validate \
	--manifest "${tmp_dir}/manifest.json" \
	--source-root "${tmp_dir}/sources" >/dev/null

mkdir -p "${tmp_dir}/partner-sources"
yq -o=json '.' "${KNOWLEDGE_DIR}/source-bundle-partner.example.yaml" \
	| jq -j '.data["manifest.json"]' >"${tmp_dir}/partner-manifest.json"
yq -o=json '.' "${KNOWLEDGE_DIR}/source-bundle-partner.example.yaml" \
	| jq -j '.data["partner-group.md"]' >"${tmp_dir}/partner-sources/partner-group.md"
PYTHONDONTWRITEBYTECODE=1 python \
	"${KNOWLEDGE_DIR}/base/ingestion.py" validate \
	--manifest "${tmp_dir}/partner-manifest.json" \
	--source-root "${tmp_dir}/partner-sources" >/dev/null

for required in \
	"INSERT INTO knowledge.ingestion_leases" \
	"pending chunk set must contain between 1 and 512 rows" \
	"stable chunk identifier collides with different stored content or source" \
	"WHERE expires_at <= transaction_timestamp()" \
	"transaction_timestamp() + interval '35 minutes'" \
	"AND leases.expires_at > transaction_timestamp()" \
	"DELETE FROM knowledge.ingestion_final"; do
	rg --fixed-strings --quiet "${required}" "${KNOWLEDGE_DIR}/base/plan.sql" \
		|| fail "plan SQL is missing: ${required}"
done
for required in \
	"embedding checkpoint must contain between 1 and 8 rows" \
	"embedding checkpoint content is absent from authoritative pending input" \
	"INSERT INTO knowledge.ingestion_embedding_cache" \
	"DELETE FROM knowledge.ingestion_final"; do
	rg --fixed-strings --quiet "${required}" "${KNOWLEDGE_DIR}/base/checkpoint.sql" \
		|| fail "checkpoint SQL is missing: ${required}"
done
for required in \
	"embedding phase changed the authoritative bound chunk set" \
	"vector_norm((payload->'embedding')::text::vector(1024)) <= 0" \
	"ON CONFLICT (chunk_id) DO UPDATE" \
	"DELETE FROM knowledge.ingestion_leases"; do
	rg --fixed-strings --quiet "${required}" "${KNOWLEDGE_DIR}/base/write.sql" \
		|| fail "write SQL is missing: ${required}"
done
if rg --quiet 'pg-knowledge-owner|knowledge_owner' "${KNOWLEDGE_DIR}/base"; then
	fail "ingestion workload references the schema-owner identity"
fi

flux_kustomization="$(
	yq eval-all -o=json \
		'select(.kind == "Kustomization" and .metadata.name == "knowledge-ingestion")' \
		"${ROOT_DIR}/clusters/base/infrastructure.yaml"
)"
jq -e '
  .spec.path == "./infra/knowledge/profiles/disabled" and
  .spec.prune == true and .spec.wait == true and
  ([.spec.dependsOn[].name] | sort) == ["agentgateway", "platform-secrets", "postgres"]
' <<<"${flux_kustomization}" >/dev/null \
	|| fail "disabled-by-default Flux knowledge-ingestion DAG contract drifted"

cluster_postgres="$(
	yq eval-all -o=json \
		'select(.kind == "Kustomization" and .metadata.name == "postgres")' \
		"${tmp_dir}/cluster-enabled.yaml"
)"
cluster_agentgateway="$(
	yq eval-all -o=json \
		'select(.kind == "Kustomization" and .metadata.name == "agentgateway")' \
		"${tmp_dir}/cluster-enabled.yaml"
)"
cluster_ingestion="$(
	yq eval-all -o=json \
		'select(.kind == "Kustomization" and .metadata.name == "knowledge-ingestion")' \
		"${tmp_dir}/cluster-enabled.yaml"
)"
jq -e '
  .spec.path == "./infra/agentgateway" and
  .spec.components == ["components/knowledge-ingestion"]
' <<<"${cluster_agentgateway}" >/dev/null \
	|| fail "cluster opt-in omitted the dedicated agentgateway listener Component"
jq -e '
  .spec.path == "./infra/postgres" and
  .spec.components == ["components/knowledge-ingestion"]
' <<<"${cluster_postgres}" >/dev/null \
	|| fail "cluster opt-in omitted the ingestion Postgres Component"
jq -e '
  .spec.path == "./infra/knowledge/profiles/enabled" and
  ([.spec.dependsOn[].name] | sort) == ["agentgateway", "platform-secrets", "postgres"]
' <<<"${cluster_ingestion}" >/dev/null \
	|| fail "cluster opt-in did not enable the ingestion workload after its dependencies"

echo "Knowledge ingestion static contract passed."
