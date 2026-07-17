#!/usr/bin/env bash
# Validate the Git/Markdown acquisition runtime without a cluster, database, or external source.
set -euo pipefail

ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
readonly CONNECTOR_DIR="${ROOT_DIR}/infra/knowledge/connectors"
readonly RUNTIME_DIR="${CONNECTOR_DIR}/git-markdown-runtime"
readonly ENABLED_DIR="${ROOT_DIR}/infra/knowledge/profiles/enabled"
readonly LOCAL_SETTINGS="${ROOT_DIR}/clusters/local/platform-settings.yaml"
readonly GCP_SETTINGS="${ROOT_DIR}/clusters/gcp/platform-settings.yaml"
readonly PYTHON_IMAGE="python:3.14-slim@sha256:b877e50bd90de10af8d82c57a022fc2e0dc731c5320d762a27986facfc3355c1"

fail() {
  echo "error: $*" >&2
  exit 1
}

for command in flux git jq kubeconform kubectl python yq; do
  command -v "${command}" >/dev/null 2>&1 || fail "required command not found: ${command}"
done

tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-knowledge-connectors.XXXXXX")"
trap 'rm -rf "${tmp_dir}"' EXIT

kubectl kustomize "${ENABLED_DIR}" >"${tmp_dir}/enabled.raw.yaml"
local_api_cidr="$(yq -er '.data.kubernetes_api_egress_cidr' "${LOCAL_SETTINGS}")"
gcp_api_cidr="$(yq -er '.data.kubernetes_api_egress_cidr' "${GCP_SETTINGS}")"
local_api_port="$(yq -er '.data.kubernetes_api_egress_port' "${LOCAL_SETTINGS}")"
gcp_api_port="$(yq -er '.data.kubernetes_api_egress_port' "${GCP_SETTINGS}")"
kubernetes_api_egress_cidr="${local_api_cidr}" kubernetes_api_egress_port="${local_api_port}" \
  flux envsubst --strict <"${tmp_dir}/enabled.raw.yaml" >"${tmp_dir}/enabled.yaml"
kubernetes_api_egress_cidr="${gcp_api_cidr}" kubernetes_api_egress_port="${gcp_api_port}" \
  flux envsubst --strict <"${tmp_dir}/enabled.raw.yaml" >"${tmp_dir}/enabled-gcp.yaml"

connector_objects="$({
  yq eval-all -o=json '
    select(.metadata.labels."app.kubernetes.io/component" == "source-connector")
  ' "${tmp_dir}/enabled.yaml"
} | jq --slurp '.')"

normalized_objects="$(
  jq -c '
    map(
      if .kind == "ConfigMap" and
        (.metadata.name | test("^knowledge-git-markdown-connector-runtime-"))
      then .metadata.name = "knowledge-git-markdown-connector-runtime-HASH"
      else .
      end
    )
  ' <<<"${connector_objects}"
)"

jq -e '
  ([.[] | .kind + "/" + .metadata.namespace + "/" + .metadata.name] | sort) == [
    "ConfigMap/knowledge/knowledge-git-markdown-connector-runtime-HASH",
    "CronJob/knowledge/knowledge-git-markdown-connector",
    "NetworkPolicy/knowledge/knowledge-git-markdown-connector",
    "Role/flux-system/knowledge-git-markdown-connector",
    "RoleBinding/flux-system/knowledge-git-markdown-connector",
    "ServiceAccount/knowledge/knowledge-git-markdown-connector"
  ]
' <<<"${normalized_objects}" >/dev/null || fail "connector resource inventory drifted"

jq -e '
  ([.[] | select(.kind == "CronJob")] | length) == 1 and
  ([.[] | select(.kind == "PersistentVolumeClaim")] | length) == 0 and
  ([.[] | select(.kind == "Secret")] | length) == 0 and
  ([.[] | .. | objects | select(has("secretKeyRef") or has("envFrom"))] | length) == 0
' <<<"${connector_objects}" >/dev/null || fail "connector count or secret-free boundary drifted"

runtime_config="$(
  yq eval-all -o=json '
    select(.kind == "ConfigMap" and
      (.metadata.name | test("^knowledge-git-markdown-connector-runtime-")))
  ' "${tmp_dir}/enabled.yaml"
)"
runtime_name="$(jq -r '.metadata.name' <<<"${runtime_config}")"
jq -e \
  --rawfile core "${CONNECTOR_DIR}/git_markdown.py" \
  --rawfile fetch "${RUNTIME_DIR}/fetch.py" '
  (.data | keys | sort) == ["fetch.py", "git_markdown.py"] and
  .data["git_markdown.py"] == $core and
  .data["fetch.py"] == $fetch
' <<<"${runtime_config}" >/dev/null || fail "connector runtime ConfigMap drifted"

cronjob="$(
  yq eval-all -o=json '
    select(.kind == "CronJob" and .metadata.name == "knowledge-git-markdown-connector")
  ' "${tmp_dir}/enabled.yaml"
)"
jq -e \
  --arg image "${PYTHON_IMAGE}" \
  --arg runtime_name "${runtime_name}" '
  .metadata.namespace == "knowledge" and
  .spec.schedule == "*/5 * * * *" and
  .spec.suspend == false and
  .spec.concurrencyPolicy == "Forbid" and
  .spec.startingDeadlineSeconds == 300 and
  .spec.successfulJobsHistoryLimit == 1 and
  .spec.failedJobsHistoryLimit == 2 and
  .spec.jobTemplate.spec.activeDeadlineSeconds == 300 and
  .spec.jobTemplate.spec.backoffLimit == 0 and
  .spec.jobTemplate.spec.ttlSecondsAfterFinished == 86400 and
  (.spec.jobTemplate.spec.template.spec |
    .serviceAccountName == "knowledge-git-markdown-connector" and
    .automountServiceAccountToken == false and
    .enableServiceLinks == false and
    .restartPolicy == "Never" and
    .securityContext == {
      "fsGroup": 2000,
      "fsGroupChangePolicy": "OnRootMismatch",
      "runAsNonRoot": true,
      "seccompProfile": {"type": "RuntimeDefault"}
    } and
    ([.containers[].name] == ["acquire"]) and
    (.containers[0] |
      .image == $image and
      .imagePullPolicy == "IfNotPresent" and
      .command == ["python"] and
      .args == [
        "/runtime/fetch.py",
        "--output-root",
        "/bundle/.connector/git-markdown",
        "--token-file",
        "/var/run/secrets/fgentic/token",
        "--ca-file",
        "/var/run/secrets/fgentic/ca.crt"
      ] and
      [.env[].name] == ["HOME", "TMPDIR", "PYTHONDONTWRITEBYTECODE", "PYTHONUNBUFFERED"] and
      .resources.requests == {
        "cpu": "25m",
        "memory": "64Mi",
        "ephemeral-storage": "16Mi"
      } and
      .resources.limits == {
        "cpu": "500m",
        "memory": "384Mi",
        "ephemeral-storage": "64Mi"
      } and
      .securityContext == {
        "allowPrivilegeEscalation": false,
        "capabilities": {"drop": ["ALL"]},
        "readOnlyRootFilesystem": true,
        "runAsGroup": 2000,
        "runAsNonRoot": true,
        "runAsUser": 65532
      } and
      ([.volumeMounts[].name] | sort) ==
        ["kube-api-access", "runtime", "source-bundle", "tmp"]) and
    ([.volumes[] | select(.name == "runtime")] == [{
      "name": "runtime",
      "configMap": {"name": $runtime_name, "defaultMode": 292}
    }]) and
    ([.volumes[] | select(.name == "source-bundle")] == [{
      "name": "source-bundle",
      "persistentVolumeClaim": {"claimName": "knowledge-source-bundle"}
    }]) and
    ([.volumes[] | select(.name == "kube-api-access") | .projected] == [{
      "defaultMode": 288,
      "sources": [
        {"serviceAccountToken": {"expirationSeconds": 600, "path": "token"}},
        {"configMap": {
          "name": "kube-root-ca.crt",
          "items": [{"key": "ca.crt", "path": "ca.crt"}]
        }}
      ]
    }]) and
    ([.volumes[] | select(.name == "tmp") | .emptyDir.sizeLimit] == ["16Mi"]))
' <<<"${cronjob}" >/dev/null || fail "connector CronJob security, token, or resource contract drifted"

service_account="$(
  yq eval-all -o=json '
    select(.kind == "ServiceAccount" and .metadata.name == "knowledge-git-markdown-connector")
  ' "${tmp_dir}/enabled.yaml"
)"
jq -e '
  .metadata.namespace == "knowledge" and .automountServiceAccountToken == false and
  ((.secrets // []) | length) == 0 and ((.imagePullSecrets // []) | length) == 0
' <<<"${service_account}" >/dev/null || fail "connector ServiceAccount gained ambient credentials"

role="$(
  yq eval-all -o=json '
    select(.kind == "Role" and .metadata.name == "knowledge-git-markdown-connector")
  ' "${tmp_dir}/enabled.yaml"
)"
role_binding="$(
  yq eval-all -o=json '
    select(.kind == "RoleBinding" and .metadata.name == "knowledge-git-markdown-connector")
  ' "${tmp_dir}/enabled.yaml"
)"
jq -e '
  .metadata.namespace == "flux-system" and
  .rules == [{
    "apiGroups": ["source.toolkit.fluxcd.io"],
    "resources": ["gitrepositories"],
    "resourceNames": ["flux-system"],
    "verbs": ["get"]
  }]
' <<<"${role}" >/dev/null || fail "connector RBAC widened beyond exact GitRepository get"
jq -e '
  .metadata.namespace == "flux-system" and
  .roleRef == {
    "apiGroup": "rbac.authorization.k8s.io",
    "kind": "Role",
    "name": "knowledge-git-markdown-connector"
  } and
  .subjects == [{
    "kind": "ServiceAccount",
    "name": "knowledge-git-markdown-connector",
    "namespace": "knowledge"
  }]
' <<<"${role_binding}" >/dev/null || fail "connector RoleBinding subject drifted"

network_policy="$(
  yq eval-all -o=json '
    select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-git-markdown-connector")
  ' "${tmp_dir}/enabled.yaml"
)"
jq -e --arg api_cidr "${local_api_cidr}" --argjson api_port "${local_api_port}" '
  .metadata.namespace == "knowledge" and
  .spec.podSelector.matchLabels == {
    "app.kubernetes.io/name": "knowledge-git-markdown-connector"
  } and
  .spec.policyTypes == ["Ingress", "Egress"] and .spec.ingress == [] and
  (.spec.egress | length) == 3 and
  .spec.egress[0].ports == [
    {"protocol": "UDP", "port": 53},
    {"protocol": "TCP", "port": 53}
  ] and
  .spec.egress[1].to == [{
    "namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "flux-system"}},
    "podSelector": {"matchLabels": {"app": "source-controller"}}
  }] and
  .spec.egress[1].ports == [{"protocol": "TCP", "port": 9090}] and
  [.spec.egress[2].to[].ipBlock.cidr] == [$api_cidr] and
  .spec.egress[2].ports == [{"protocol": "TCP", "port": $api_port}] and
  ([.spec.egress[].ports[].port] | sort) == [53, 53, $api_port, 9090] and
  ([.spec.egress[].to[]?.ipBlock.cidr // empty] | index("0.0.0.0/0")) == null
' <<<"${network_policy}" >/dev/null || fail "connector egress widened beyond DNS, source-controller, and API VIPs"

gcp_network_policy="$(
  yq eval-all -o=json '
    select(.kind == "NetworkPolicy" and .metadata.name == "knowledge-git-markdown-connector")
  ' "${tmp_dir}/enabled-gcp.yaml"
)"
jq -e --arg api_cidr "${gcp_api_cidr}" --argjson api_port "${gcp_api_port}" '
  [.spec.egress[2].to[].ipBlock.cidr] == [$api_cidr] and
  .spec.egress[2].ports == [{"protocol": "TCP", "port": $api_port}] and
  ([.spec.egress[].to[]?.ipBlock.cidr // empty] | length) == 1
' <<<"${gcp_network_policy}" >/dev/null ||
  fail "GCP connector egress did not select its exact API endpoint CIDR"

rg --fixed-strings --quiet 'GitMarkdownConnector.from_artifact' "${RUNTIME_DIR}/fetch.py" ||
  fail "fetch wrapper bypasses the typed connector validator"
rg --fixed-strings --quiet 'git_markdown.inventory_json(connector)' "${RUNTIME_DIR}/fetch.py" ||
  fail "fetch wrapper does not publish the canonical connector inventory"
rg --fixed-strings --quiet 'temporary_current.replace(output_root / "current.json")' \
  "${RUNTIME_DIR}/fetch.py" || fail "current inventory is not atomically replaced last"
if rg --quiet 'PGHOST|PGPASSWORD|secretKeyRef|port: (5432|8080|8082)' "${RUNTIME_DIR}"; then
  fail "connector runtime references a database, model gateway, or static credential"
fi

PYTHONDONTWRITEBYTECODE=1 python -c '
from pathlib import Path
for path in (Path("'"${CONNECTOR_DIR}/git_markdown.py"'"), Path("'"${RUNTIME_DIR}/fetch.py"'")):
    compile(path.read_bytes(), str(path), "exec")
'

TEST_CONNECTOR_DIR="${CONNECTOR_DIR}" \
  TEST_RUNTIME_DIR="${RUNTIME_DIR}" \
  TEST_REPO_ROOT="${ROOT_DIR}" \
  PYTHONDONTWRITEBYTECODE=1 \
  python <<'PY'
from __future__ import annotations

import copy
import hashlib
import importlib.util
import io
import json
import os
import subprocess
import sys
import tarfile
import tempfile
from pathlib import Path

connector_dir = Path(os.environ["TEST_CONNECTOR_DIR"])
runtime_dir = Path(os.environ["TEST_RUNTIME_DIR"])
repo_root = Path(os.environ["TEST_REPO_ROOT"])
sys.path.insert(0, str(connector_dir))
import git_markdown  # noqa: E402

spec = importlib.util.spec_from_file_location("git_markdown_fetch", runtime_dir / "fetch.py")
if spec is None or spec.loader is None:
    raise AssertionError("could not load connector acquisition wrapper")
fetch = importlib.util.module_from_spec(spec)
spec.loader.exec_module(fetch)


def artifact(documents: dict[str, bytes], *, include_acl: bool = True) -> bytes:
    manifest = json.dumps(
        {
            "schema_version": 1,
            "corpus": "reference-docs",
            "classification": "approved_non_public",
            "allowed_principals": [
                {"kind": "matrix", "principal": "@alice:org-a.example"}
            ],
            "allowed_groups": [],
        },
        sort_keys=True,
        separators=(",", ":"),
    ).encode()
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as archive:
        entries = dict(documents)
        if include_acl:
            entries[git_markdown.ACL_MANIFEST_PATH] = manifest
        for path, content in entries.items():
            info = tarfile.TarInfo(path)
            info.mtime = 0
            info.size = len(content)
            archive.addfile(info, io.BytesIO(content))
    return stream.getvalue()


def api_document(
    raw: bytes,
    *,
    generation: int = 7,
    revision: str = f"main@sha1:{'a' * 40}",
) -> dict[str, object]:
    digest = hashlib.sha256(raw).hexdigest()
    return {
        "apiVersion": "source.toolkit.fluxcd.io/v1",
        "kind": "GitRepository",
        "metadata": {
            "generation": generation,
            "name": "flux-system",
            "namespace": "flux-system",
        },
        "status": {
            "observedGeneration": generation,
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "observedGeneration": generation,
                }
            ],
            "artifact": {
                "revision": revision,
                "digest": f"sha256:{digest}",
                "size": len(raw),
                "url": (
                    "http://source-controller.flux-system.svc.cluster.local./"
                    f"gitrepository/flux-system/flux-system/{digest}.tar.gz"
                ),
            },
        },
    }


def acquire(
    root: Path,
    raw: bytes,
    *,
    revision: str = f"main@sha1:{'a' * 40}",
) -> None:
    document = api_document(raw, revision=revision)
    fetch._api_document = lambda _token, _ca: document
    fetch._download_artifact = lambda status: raw
    fetch.acquire(root, root / "unused-token", root / "unused-ca")


first = artifact({"docs/guide.md": b"# Guide\n", "docs/policy.md": b"# Policy\n"})
first_document = api_document(first)
first_status = fetch._artifact_status(first_document)
assert fetch._validated_source_url(first_status.url).hostname is not None

stale = copy.deepcopy(first_document)
stale["status"]["observedGeneration"] = 6
try:
    fetch._artifact_status(stale)
except fetch.AcquisitionError:
    pass
else:
    raise AssertionError("stale Flux status was accepted")

try:
    fetch._validated_source_url("https://example.com/git.tar.gz")
except fetch.AcquisitionError:
    pass
else:
    raise AssertionError("external artifact URL was accepted")

with tempfile.TemporaryDirectory() as temporary:
    root = Path(temporary) / "bundle" / ".connector" / "git-markdown"
    acquire(root, first)

    first_inventory = (root / "current.json").read_bytes()
    first_payload = json.loads(first_inventory)
    artifact_hex = first_payload["artifact_digest"].removeprefix("sha256:")
    inventory_hex = first_payload["inventory_digest"].removeprefix("sha256:")
    first_revision_hex = hashlib.sha256(first_payload["snapshot_revision"].encode()).hexdigest()
    assert (
        root / "snapshots" / inventory_hex / first_revision_hex / artifact_hex / "inventory.json"
    ).read_bytes() == first_inventory
    assert first_payload["inventory_digest"] != first_payload["artifact_digest"]
    assert artifact_hex != inventory_hex
    assert first_payload["source_count"] == 2
    for source in first_payload["sources"]:
        blob = root / "blobs" / source["content_digest"].removeprefix("sha256:")
        assert hashlib.sha256(blob.read_bytes()).hexdigest() == blob.name

    first_blob = root / "blobs" / first_payload["sources"][0]["content_digest"].removeprefix("sha256:")
    expected_blob = first_blob.read_bytes()
    first_blob.unlink()
    stale_pending = first_blob.parent / f".pending-{first_blob.name}-1"
    stale_pending.write_bytes(expected_blob)
    acquire(root, first)
    assert first_blob.read_bytes() == expected_blob
    assert stale_pending.read_bytes() == expected_blob
    stale_pending.unlink()
    assert (root / "current.json").read_bytes() == first_inventory

    first_blob.chmod(0o640)
    first_blob.write_bytes(b"tampered")
    try:
        acquire(root, first)
    except fetch.AcquisitionError:
        pass
    else:
        raise AssertionError("a differing existing content-addressed blob was overwritten")
    assert first_blob.read_bytes() == b"tampered"
    assert (root / "current.json").read_bytes() == first_inventory
    first_blob.write_bytes(expected_blob)
    first_blob.chmod(0o440)

    unchanged = artifact(
        {
            "docs/guide.md": b"# Guide\n",
            "docs/policy.md": b"# Policy\n",
            "README.md": b"# Unselected change\n",
        }
    )
    acquire(root, unchanged, revision=f"main@sha1:{'b' * 40}")
    unchanged_payload = json.loads((root / "current.json").read_bytes())
    unchanged_revision_hex = hashlib.sha256(unchanged_payload["snapshot_revision"].encode()).hexdigest()
    assert unchanged_payload["inventory_digest"] == first_payload["inventory_digest"]
    assert unchanged_payload["artifact_digest"] != first_payload["artifact_digest"]
    assert unchanged_revision_hex != first_revision_hex
    unchanged_artifact_hex = unchanged_payload["artifact_digest"].removeprefix("sha256:")
    assert (
        root
        / "snapshots"
        / inventory_hex
        / unchanged_revision_hex
        / unchanged_artifact_hex
        / "inventory.json"
    ).exists()
    assert (
        root / "snapshots" / inventory_hex / first_revision_hex / artifact_hex / "inventory.json"
    ).exists()

    rebuilt = artifact(
        {
            "docs/guide.md": b"# Guide\n",
            "docs/policy.md": b"# Policy\n",
            "README.md": b"# Rebuilt unselected bytes\n",
        }
    )
    acquire(root, rebuilt, revision=f"main@sha1:{'b' * 40}")
    rebuilt_payload = json.loads((root / "current.json").read_bytes())
    rebuilt_artifact_hex = rebuilt_payload["artifact_digest"].removeprefix("sha256:")
    assert rebuilt_payload["snapshot_revision"] == unchanged_payload["snapshot_revision"]
    assert rebuilt_payload["inventory_digest"] == unchanged_payload["inventory_digest"]
    assert rebuilt_artifact_hex != unchanged_artifact_hex
    assert (
        root
        / "snapshots"
        / inventory_hex
        / unchanged_revision_hex
        / unchanged_artifact_hex
        / "inventory.json"
    ).exists()
    assert (
        root
        / "snapshots"
        / inventory_hex
        / unchanged_revision_hex
        / rebuilt_artifact_hex
        / "inventory.json"
    ).exists()

    second = artifact({"docs/guide.md": b"# Guide v2\n"})
    acquire(root, second, revision=f"main@sha1:{'c' * 40}")
    second_payload = json.loads((root / "current.json").read_bytes())
    second_artifact_hex = second_payload["artifact_digest"].removeprefix("sha256:")
    second_inventory_hex = second_payload["inventory_digest"].removeprefix("sha256:")
    second_revision_hex = hashlib.sha256(second_payload["snapshot_revision"].encode()).hexdigest()
    assert second_artifact_hex != artifact_hex
    assert second_inventory_hex != inventory_hex
    assert (
        root / "snapshots" / inventory_hex / first_revision_hex / artifact_hex / "inventory.json"
    ).exists()
    assert (
        root
        / "snapshots"
        / second_inventory_hex
        / second_revision_hex
        / second_artifact_hex
        / "inventory.json"
    ).exists()
    assert first_blob.exists()

    stable_current = (root / "current.json").read_bytes()
    fetch._api_document = lambda _token, _ca: api_document(second)
    fetch._download_artifact = lambda _status: second + b"corrupt"
    try:
        fetch.acquire(root, root / "unused-token", root / "unused-ca")
    except fetch.AcquisitionError as error:
        assert "digest differs" in str(error)
    else:
        raise AssertionError("a corrupt artifact was published")
    assert (root / "current.json").read_bytes() == stable_current

    rejected = artifact({"docs/guide.md": b"# Missing ACL\n"}, include_acl=False)
    rejected_document = api_document(rejected, revision=f"main@sha1:{'d' * 40}")
    fetch._api_document = lambda _token, _ca: rejected_document
    fetch._download_artifact = lambda _status: rejected
    try:
        fetch.acquire(root, root / "unused-token", root / "unused-ca")
    except git_markdown.ConnectorError:
        pass
    else:
        raise AssertionError("a digest-valid artifact without an ACL published")
    blocked_payload = json.loads((root / "current.json").read_bytes())
    assert blocked_payload == {
        "artifact_digest": rejected_document["status"]["artifact"]["digest"],
        "blocked": True,
        "connector_id": "git-markdown",
        "reason": "artifact-rejected",
        "snapshot_revision": f"main@sha1:{'d' * 40}",
    }
    acquire(root, second, revision=f"main@sha1:{'c' * 40}")
    assert (root / "current.json").read_bytes() == stable_current

tracked = {
    Path(path)
    for path in subprocess.check_output(
        ["git", "-C", str(repo_root), "ls-files", "-z"],
    ).decode().rstrip("\0").split("\0")
    if path
}
tracked.add(Path(git_markdown.ACL_MANIFEST_PATH))
repository_stream = io.BytesIO()
with tarfile.open(fileobj=repository_stream, mode="w:gz") as repository_archive:
    for relative in sorted(tracked):
        source = repo_root / relative
        info = repository_archive.gettarinfo(str(source), arcname=relative.as_posix())
        if info.isreg():
            with source.open("rb") as handle:
                repository_archive.addfile(info, handle)
        else:
            repository_archive.addfile(info)
repository_artifact = repository_stream.getvalue()
repository_connector = git_markdown.GitMarkdownConnector.from_artifact(
    connector_id="git-markdown",
    status=git_markdown.ArtifactStatus(
        revision=f"main@sha1:{'d' * 40}",
        digest=f"sha256:{hashlib.sha256(repository_artifact).hexdigest()}",
        url=(
            "http://source-controller.flux-system.svc.cluster.local./"
            "gitrepository/flux-system/flux-system/repository.tar.gz"
        ),
        size=len(repository_artifact),
    ),
    artifact=repository_artifact,
)
expected_markdown = sorted(
    path.as_posix()
    for path in tracked
    if len(path.parts) >= 2 and path.parts[0] == "docs" and path.suffix == ".md"
)
actual_markdown = [reference.path for reference in repository_connector.enumerate_sources()]
assert actual_markdown == expected_markdown
assert len(actual_markdown) > 0
repository_source = repository_connector.fetch_source(
    repository_connector.enumerate_sources()[0].source_id
)
assert repository_source.acl.classification == "public"
assert repository_source.acl.allowed_groups == ("partner/org-b/docs",)
PY

yq eval-all '
  select(.metadata.labels."app.kubernetes.io/component" == "source-connector")
' "${tmp_dir}/enabled.yaml" | kubeconform -strict -summary

echo "Knowledge Git/Markdown connector acquisition contract passed."
