"""Focused tests for connector claim materialization into one-source bundles."""

from __future__ import annotations

import copy
import hashlib
import io
import json
import os
import tarfile
import traceback
from pathlib import Path
from typing import Any, cast

import connector_runtime
import git_markdown
import pytest

ARTIFACT_URL = (
    "http://source-controller.flux-system.svc.cluster.local/gitrepository/flux-system/flux-system/latest.tar.gz"
)


def acl_manifest(*, principal: str = "@alice:org-a.example") -> dict[str, Any]:
    return {
        "schema_version": 1,
        "corpus": "reference-docs",
        "classification": "approved_non_public",
        "allowed_principals": [{"kind": "matrix", "principal": principal}],
        "allowed_groups": ["partner/org-b-a2a/product"],
    }


def connector_for(
    path: str,
    content: bytes,
    *,
    principal: str = "@alice:org-a.example",
    snapshot_revision: str | None = None,
    unselected: bytes | None = None,
) -> git_markdown.GitMarkdownConnector:
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as archive:
        files = {
            git_markdown.ACL_MANIFEST_PATH: canonical_json(acl_manifest(principal=principal)),
            path: content,
        }
        if unselected is not None:
            files["README.md"] = unselected
        for name, raw in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(raw)
            info.mtime = 0
            archive.addfile(info, io.BytesIO(raw))
    artifact = stream.getvalue()
    return git_markdown.GitMarkdownConnector.from_artifact(
        connector_id="git-markdown",
        status=git_markdown.ArtifactStatus(
            revision=snapshot_revision or f"main@sha1:{hashlib.sha1(content, usedforsecurity=False).hexdigest()}",
            digest=digest(artifact),
            url=ARTIFACT_URL,
            size=len(artifact),
        ),
        artifact=artifact,
    )


def canonical_json(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def digest(content: bytes) -> str:
    return f"sha256:{hashlib.sha256(content).hexdigest()}"


def action_for(connector: git_markdown.SourceConnector) -> dict[str, Any]:
    inventory = git_markdown.inventory_payload(connector)
    sources = cast(list[dict[str, object]], inventory["sources"])
    source = sources[0]
    return {
        "connector_id": source["connector_id"],
        "source_id": source["source_id"],
        "source_path": source["source_path"],
        "action": "present",
        "source_revision": source["source_revision"],
        "content_digest": source["content_digest"],
        "acl_digest": source["acl_digest"],
        "metadata": source["metadata"],
        "snapshot_revision": source["snapshot_revision"],
        "inventory_digest": source["inventory_digest"],
        "claim_expires_at": "2026-07-17T04:35:00+00:00",
    }


def write_json(path: Path, value: object) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_bytes(canonical_json(value) + b"\n")


def install_acquisition(
    root: Path,
    connector: git_markdown.GitMarkdownConnector,
    *,
    current: bool = True,
    retained: bool = True,
    blob: bool = True,
) -> dict[str, Any]:
    inventory = git_markdown.inventory_payload(connector)
    inventory_digest = inventory["inventory_digest"]
    assert isinstance(inventory_digest, str)
    source = connector.fetch_source(connector.enumerate_sources()[0].source_id)
    if current:
        write_json(root / connector_runtime.CURRENT_FILENAME, inventory)
    if retained:
        revision_digest = hashlib.sha256(connector.cursor.revision.encode()).hexdigest()
        artifact_digest = connector.artifact_digest.removeprefix("sha256:")
        write_json(
            root
            / "snapshots"
            / inventory_digest.removeprefix("sha256:")
            / revision_digest
            / artifact_digest
            / connector_runtime.INVENTORY_FILENAME,
            inventory,
        )
    if blob:
        blob_path = root / "blobs" / source.content_digest.removeprefix("sha256:")
        blob_path.parent.mkdir(parents=True, exist_ok=True)
        blob_path.write_bytes(source.content)
    return inventory


def write_action(work_root: Path, action: object) -> Path:
    path = work_root / connector_runtime.ACTION_FILENAME
    write_json(path, action)
    return path


def materialize(tmp_path: Path, connector: git_markdown.GitMarkdownConnector) -> connector_runtime.MaterializedSource:
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action_path = write_action(tmp_path / "work", action_for(connector))
    return connector_runtime.materialize_connector_source(
        source_root=source_root,
        action_path=action_path,
        output_root=tmp_path / "selected",
    )


def test_present_action_materializes_exact_acl_and_source_bundle(tmp_path: Path) -> None:
    connector = connector_for("docs/grounding.md", b"# Grounding\n\nSovereign context.\n")
    source = connector.fetch_source(connector.enumerate_sources()[0].source_id)

    result = materialize(tmp_path, connector)

    assert result.source_id == source.source_id
    assert result.inventory_digest == connector.cursor.digest
    assert result.source_path.read_bytes() == source.content
    manifest = json.loads(result.manifest_path.read_bytes())
    assert manifest == {
        "schema_version": 1,
        "corpus": "reference-docs",
        "sources": [
            {
                "path": "docs/grounding.md",
                "digest": source.content_digest,
                "source": source.metadata()["source"],
                "classification": "approved_non_public",
                "allowed_principals": [{"kind": "matrix", "principal": "@alice:org-a.example"}],
                "allowed_groups": ["partner/org-b-a2a/product"],
            }
        ],
    }
    assert "location" not in manifest["sources"][0]["source"]


def test_present_action_materializes_below_existing_volume_mount(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action_path = write_action(tmp_path / "work", action_for(connector))
    mount_root = tmp_path / "selected"
    mount_root.mkdir()

    result = connector_runtime.materialize_connector_source(
        source_root=source_root,
        action_path=action_path,
        output_root=mount_root / "bundle",
    )

    assert result.manifest_path == mount_root / "bundle" / "manifest.json"
    assert result.source_path.read_bytes() == b"# Trusted\n"


def test_pending_older_action_uses_retained_inventory_and_blob_after_current_moves(tmp_path: Path) -> None:
    old = connector_for("docs/grounding.md", b"# Grounding\n\nOld reviewed revision.\n")
    new = connector_for("docs/grounding.md", b"# Grounding\n\nNew reviewed revision.\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, old, current=False)
    install_acquisition(source_root, new, retained=True)
    action_path = write_action(tmp_path / "work", action_for(old))

    result = connector_runtime.materialize_connector_source(
        source_root=source_root,
        action_path=action_path,
        output_root=tmp_path / "selected",
    )

    assert result.source_path.read_bytes() == b"# Grounding\n\nOld reviewed revision.\n"
    assert result.inventory_digest == old.cursor.digest


def test_pending_action_uses_its_retained_revision_when_current_has_same_inventory(tmp_path: Path) -> None:
    content = b"# Grounding\n\nSelected content is unchanged.\n"
    old = connector_for(
        "docs/grounding.md",
        content,
        snapshot_revision=f"main@sha1:{'a' * 40}",
    )
    new = connector_for(
        "docs/grounding.md",
        content,
        snapshot_revision=f"main@sha1:{'b' * 40}",
        unselected=b"# Unselected repository change\n",
    )
    assert old.cursor.digest == new.cursor.digest
    assert old.cursor.revision != new.cursor.revision
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, old, current=False)
    install_acquisition(source_root, new)
    action_path = write_action(tmp_path / "work", action_for(old))

    result = connector_runtime.materialize_connector_source(
        source_root=source_root,
        action_path=action_path,
        output_root=tmp_path / "selected",
    )

    assert result.source_path.read_bytes() == content
    assert result.inventory_digest == old.cursor.digest


def test_retained_inventory_discovery_bounds_stale_pending_entries(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action = action_for(connector)
    revision_root = (
        source_root
        / "snapshots"
        / str(action["inventory_digest"]).removeprefix("sha256:")
        / hashlib.sha256(str(action["snapshot_revision"]).encode()).hexdigest()
    )
    for index in range(connector_runtime.MAX_RETAINED_ENTRIES_PER_REVISION + 1):
        (revision_root / f".pending-{index:03d}").mkdir()
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match="entry count"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_blob_is_sufficient_when_optional_inventory_evidence_is_absent(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Source\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector, current=False, retained=False)
    action_path = write_action(tmp_path / "work", action_for(connector))

    result = connector_runtime.materialize_connector_source(
        source_root=source_root,
        action_path=action_path,
        output_root=tmp_path / "selected",
    )

    assert result.source_path.read_bytes() == b"# Source\n"


def test_rejects_blob_digest_mismatch(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action = action_for(connector)
    blob = source_root / "blobs" / str(action["content_digest"]).removeprefix("sha256:")
    blob.write_bytes(b"# Tampered\n")
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match="blob digest"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_rejects_action_acl_mismatch_before_materialization(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action = action_for(connector)
    action["acl_digest"] = f"sha256:{'0' * 64}"
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match="ACL digest"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_rejects_json_escaped_surrogate_as_materialization_error(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    action = action_for(connector)
    action["source_revision"] = "\ud800"
    action_path = tmp_path / "work" / connector_runtime.ACTION_FILENAME
    action_path.parent.mkdir(parents=True)
    action_path.write_text(json.dumps(action), encoding="utf-8")

    with pytest.raises(connector_runtime.MaterializationError) as caught:
        connector_runtime.parse_connector_action(action_path)

    assert str(caught.value) == "connector action.source_revision must be valid Unicode text"
    assert "\ud800" not in str(caught.value)


def test_rejects_duplicate_action_keys_without_reflection(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    action_path = tmp_path / "work" / connector_runtime.ACTION_FILENAME
    action_path.parent.mkdir(parents=True)
    output_root = tmp_path / "selected"
    hostile_key = "attacker\nforged-log"
    encoded_key = json.dumps(hostile_key)
    action_path.write_text(f"{{{encoded_key}:1,{encoded_key}:2}}", encoding="utf-8")

    with pytest.raises(connector_runtime.MaterializationError) as caught:
        connector_runtime.parse_connector_action(action_path)

    assert "JSON object contains duplicate key" in str(caught.value)
    assert hostile_key not in str(caught.value)
    assert "forged-log" not in "".join(traceback.format_exception(caught.value))

    assert (
        connector_runtime.main(
            [
                "materialize",
                "--action",
                os.fspath(action_path),
                "--source-root",
                os.fspath(tmp_path / "missing-acquisition"),
                "--output-root",
                os.fspath(output_root),
            ]
        )
        == 2
    )
    assert "JSON object contains duplicate key" in caplog.text
    assert hostile_key not in caplog.text
    assert "forged-log" not in caplog.text
    assert not output_root.exists()


def test_rejects_unknown_action_keys_without_reflection(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    action = action_for(connector)
    hostile_key = "attacker\nforged-log"
    action[hostile_key] = True
    action_path = write_action(tmp_path / "work", action)
    output_root = tmp_path / "selected"

    with pytest.raises(connector_runtime.MaterializationError) as caught:
        connector_runtime.parse_connector_action(action_path)

    assert str(caught.value) == "connector action has unknown fields"
    assert hostile_key not in str(caught.value)
    assert "forged-log" not in "".join(traceback.format_exception(caught.value))

    assert (
        connector_runtime.main(
            [
                "materialize",
                "--action",
                os.fspath(action_path),
                "--source-root",
                os.fspath(tmp_path / "missing-acquisition"),
                "--output-root",
                os.fspath(output_root),
            ]
        )
        == 2
    )
    assert "connector action has unknown fields" in caplog.text
    assert hostile_key not in caplog.text
    assert "forged-log" not in caplog.text
    assert not output_root.exists()


def test_rejects_retained_inventory_that_disagrees_with_action(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    inventory = install_acquisition(source_root, connector)
    action = action_for(connector)
    changed = copy.deepcopy(inventory)
    changed["sources"][0]["metadata"]["allowed_principals"][0]["principal"] = "@mallory:org-a.example"
    retained = (
        source_root
        / "snapshots"
        / str(action["inventory_digest"]).removeprefix("sha256:")
        / hashlib.sha256(str(action["snapshot_revision"]).encode()).hexdigest()
        / connector.artifact_digest.removeprefix("sha256:")
        / connector_runtime.INVENTORY_FILENAME
    )
    write_json(retained, changed)
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match=r"acl_digest|canonical source digest"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_rejects_traversal_in_claimed_source_path(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action = action_for(connector)
    action["source_path"] = "../escape.md"
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match=r"docs/\*\*/\*\.md"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_rejects_symlinked_blob(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector, blob=False)
    action = action_for(connector)
    target = tmp_path / "outside.md"
    target.write_bytes(b"# Trusted\n")
    blob = source_root / "blobs" / str(action["content_digest"]).removeprefix("sha256:")
    blob.parent.mkdir(parents=True)
    blob.symlink_to(target)
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match="regular file"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


@pytest.mark.parametrize("action_kind", ["tombstone", "noop"])
def test_rejects_tombstone_and_noop_actions(tmp_path: Path, action_kind: str) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    action = action_for(connector)
    action["action"] = action_kind
    if action_kind == "tombstone":
        action["acl_digest"] = None
        action["metadata"] = None
    action_path = write_action(tmp_path / "work", action)

    with pytest.raises(connector_runtime.MaterializationError, match="cannot be materialized"):
        connector_runtime.materialize_connector_source(
            source_root=tmp_path / "missing-acquisition",
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_empty_action_is_noop_and_not_a_connector_source(tmp_path: Path) -> None:
    work_root = tmp_path / "work"
    work_root.mkdir()
    action_path = work_root / connector_runtime.ACTION_FILENAME
    action_path.write_bytes(b"")

    assert not connector_runtime.is_connector_source(work_root)
    with pytest.raises(connector_runtime.MaterializationError, match="no connector action"):
        connector_runtime.materialize_connector_source(
            source_root=tmp_path / "missing-acquisition",
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_refuses_to_overwrite_output_bundle(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action_path = write_action(tmp_path / "work", action_for(connector))
    output_root = tmp_path / "selected"
    output_root.mkdir()

    with pytest.raises(connector_runtime.MaterializationError, match="overwrite"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=output_root,
        )


def test_rejects_symlinked_current_inventory_evidence(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector, current=False)
    outside = tmp_path / "current.json"
    write_json(outside, git_markdown.inventory_payload(connector))
    (source_root / connector_runtime.CURRENT_FILENAME).symlink_to(outside)
    action_path = write_action(tmp_path / "work", action_for(connector))

    with pytest.raises(connector_runtime.MaterializationError, match="regular file"):
        connector_runtime.materialize_connector_source(
            source_root=source_root,
            action_path=action_path,
            output_root=tmp_path / "selected",
        )


def test_cli_materialize_subcommand_uses_exact_runtime_shape(tmp_path: Path) -> None:
    connector = connector_for("docs/source.md", b"# Trusted\n")
    source_root = tmp_path / "acquisition"
    install_acquisition(source_root, connector)
    action_path = write_action(tmp_path / "work", action_for(connector))
    output_root = tmp_path / "selected"

    assert (
        connector_runtime.main(
            [
                "materialize",
                "--action",
                os.fspath(action_path),
                "--source-root",
                os.fspath(source_root),
                "--output-root",
                os.fspath(output_root),
            ]
        )
        == 0
    )
    assert (output_root / "docs/source.md").read_bytes() == b"# Trusted\n"
