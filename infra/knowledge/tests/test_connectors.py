"""Unit tests for the immutable Git/Markdown connector and sync planner."""

from __future__ import annotations

import copy
import hashlib
import io
import json
import tarfile
import traceback
from collections.abc import Mapping
from typing import Any, cast

import git_markdown
import pytest

ARTIFACT_URL = (
    "http://source-controller.flux-system.svc.cluster.local/gitrepository/flux-system/flux-system/latest.tar.gz"
)


def manifest(*, principal: str = "@alice:org-a.example") -> dict[str, Any]:
    return {
        "schema_version": 1,
        "corpus": "reference-docs",
        "classification": "approved_non_public",
        "allowed_principals": [{"kind": "matrix", "principal": principal}],
        "allowed_groups": [],
    }


def archive_entries(entries: list[tuple[tarfile.TarInfo, bytes | None]]) -> bytes:
    stream = io.BytesIO()
    with tarfile.open(fileobj=stream, mode="w:gz") as archive:
        for info, content in entries:
            info.mtime = 0
            if content is not None:
                info.size = len(content)
                archive.addfile(info, io.BytesIO(content))
            else:
                archive.addfile(info)
    return stream.getvalue()


def regular_entry(path: str, content: bytes) -> tuple[tarfile.TarInfo, bytes]:
    return tarfile.TarInfo(path), content


def artifact_bytes(
    files: Mapping[str, bytes], *, extra: list[tuple[tarfile.TarInfo, bytes | None]] | None = None
) -> bytes:
    entries: list[tuple[tarfile.TarInfo, bytes | None]] = [
        regular_entry(path, content) for path, content in files.items()
    ]
    if extra is not None:
        entries.extend(extra)
    return archive_entries(entries)


def status_for(raw: bytes, *, revision: str = "main@sha1:0123456789abcdef") -> git_markdown.ArtifactStatus:
    return git_markdown.ArtifactStatus(
        revision=revision,
        digest=f"sha256:{hashlib.sha256(raw).hexdigest()}",
        url=ARTIFACT_URL,
        size=len(raw),
    )


def assert_content_free_rejection(raw: bytes, hostile: str) -> None:
    with pytest.raises(git_markdown.ConnectorError) as caught:
        git_markdown.GitMarkdownConnector.from_artifact(
            connector_id="git-markdown",
            status=status_for(raw),
            artifact=raw,
        )

    rendered = "".join(traceback.format_exception(caught.value))
    assert hostile not in str(caught.value)
    assert hostile not in rendered


def build_connector(
    documents: Mapping[str, bytes],
    *,
    document: dict[str, Any] | None = None,
    stream: bool = False,
    revision: str = "main@sha1:0123456789abcdef",
) -> git_markdown.GitMarkdownConnector:
    acl_manifest = manifest() if document is None else document
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(acl_manifest).encode(),
            **documents,
        }
    )
    artifact: bytes | git_markdown.ArtifactReader = io.BytesIO(raw) if stream else raw
    return git_markdown.GitMarkdownConnector.from_artifact(
        connector_id="git-markdown",
        status=status_for(raw, revision=revision),
        artifact=artifact,
    )


def applied_sources(connector: git_markdown.SourceConnector) -> dict[str, git_markdown.AppliedSource]:
    return {
        reference.source_id: git_markdown.AppliedSource(
            source_id=reference.source_id,
            content_digest=source.content_digest,
            acl_digest=source.acl_digest,
        )
        for reference in connector.enumerate_sources()
        for source in [connector.fetch_source(reference.source_id)]
    }


def test_full_snapshot_then_noop_advances_only_the_complete_cursor() -> None:
    connector: git_markdown.SourceConnector = build_connector(
        {
            "docs/zeta.md": b"# Zeta\n",
            "docs/alpha.md": b"# Alpha\n",
            "docs/ignored.txt": b"not Markdown",
            "README.md": b"# Repository root is outside the reference corpus\n",
            ".fgentic/ignored.md": b"# Connector policy directory is outside docs\n",
        },
        stream=True,
    )

    references = connector.enumerate_sources()
    assert [reference.source_id for reference in references] == [
        "reference-docs/git-markdown/docs/alpha.md",
        "reference-docs/git-markdown/docs/zeta.md",
    ]
    first = connector.fetch_source(references[0].source_id)
    assert first.content == b"# Alpha\n"
    assert first.content_digest.startswith("sha256:")
    assert first.revision == first.content_digest
    assert first.revision != connector.cursor.revision
    assert first.locator == "git:flux-system/flux-system#docs/alpha.md"
    assert first.acl_digest.startswith("sha256:")
    assert first.acl.as_dict() == {
        "classification": "approved_non_public",
        "allowed_principals": [{"kind": "matrix", "principal": "@alice:org-a.example"}],
        "allowed_groups": [],
    }
    assert connector.fetch_source(references[1].source_id).acl_digest == first.acl_digest
    inventory = git_markdown.inventory_payload(connector)
    assert set(inventory) == {
        "connector_id",
        "snapshot_revision",
        "inventory_digest",
        "artifact_digest",
        "source_count",
        "sources",
    }
    assert inventory["source_count"] == 2
    assert inventory["artifact_digest"] == connector.artifact_digest
    inventory_sources = cast(list[dict[str, object]], inventory["sources"])
    assert inventory_sources[0] == {
        "connector_id": "git-markdown",
        "snapshot_revision": connector.cursor.revision,
        "inventory_digest": connector.cursor.digest,
        "source_id": first.source_id,
        "source_path": first.path,
        "source_revision": first.revision,
        "content_digest": first.content_digest,
        "acl_digest": first.acl_digest,
        "metadata": first.metadata(),
    }
    canonical_inventory = [
        {
            key: source[key]
            for key in (
                "source_id",
                "source_path",
                "source_revision",
                "content_digest",
                "acl_digest",
                "metadata",
            )
        }
        for source in inventory_sources
    ]
    expected_inventory_digest = (
        "sha256:"
        + hashlib.sha256(
            json.dumps(canonical_inventory, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()
        ).hexdigest()
    )
    assert connector.cursor.digest == expected_inventory_digest
    assert connector.cursor.digest != connector.artifact_digest
    assert first.metadata()["source"] == {
        "id": first.source_id,
        "locator": first.locator,
        "revision": first.revision,
    }
    assert json.loads(git_markdown.inventory_json(connector)) == inventory

    initial = git_markdown.plan_next(connector, {})
    assert isinstance(initial.action, git_markdown.PresentAction)
    assert initial.action.source_id == references[0].source_id
    assert initial.complete_cursor is None
    assert json.loads(git_markdown.action_json(connector, initial)) == {
        "action": "present",
        "connector_id": "git-markdown",
        "source_id": first.source_id,
        "source_path": first.path,
        "source_revision": first.revision,
        "content_digest": first.content_digest,
        "acl_digest": first.acl_digest,
        "metadata": first.metadata(),
        "snapshot_revision": connector.cursor.revision,
        "inventory_digest": connector.cursor.digest,
    }

    complete = git_markdown.plan_next(connector, applied_sources(connector))
    assert complete.action is None
    assert complete.complete_cursor == connector.cursor
    assert json.loads(git_markdown.action_json(connector, complete)) == {
        "action": "complete",
        "connector_id": "git-markdown",
        "snapshot_revision": connector.cursor.revision,
        "inventory_digest": connector.cursor.digest,
    }


def test_shared_acl_digest_is_computed_once(monkeypatch: pytest.MonkeyPatch) -> None:
    acl_payload = git_markdown._canonical_json(
        {
            "classification": "approved_non_public",
            "allowed_principals": [{"kind": "matrix", "principal": "@alice:org-a.example"}],
            "allowed_groups": [],
        }
    )
    digest_calls = 0
    digest = git_markdown._digest

    def count_acl_digest(value: bytes) -> str:
        nonlocal digest_calls
        if value == acl_payload:
            digest_calls += 1
        return digest(value)

    monkeypatch.setattr(git_markdown, "_digest", count_acl_digest)

    connector = build_connector(
        {
            "docs/alpha.md": b"# Alpha\n",
            "docs/zeta.md": b"# Zeta\n",
        }
    )
    acl_digests = {
        connector.fetch_source(reference.source_id).acl_digest for reference in connector.enumerate_sources()
    }

    assert digest_calls == 1
    assert acl_digests == {digest(acl_payload)}


def test_content_change_selects_present_action() -> None:
    previous = build_connector({"docs/guide.md": b"# Guide\n\nOld.\n"})
    desired = build_connector({"docs/guide.md": b"# Guide\n\nNew.\n"})
    previous_state = applied_sources(previous)

    plan = git_markdown.plan_next(desired, previous_state)

    assert isinstance(plan.action, git_markdown.PresentAction)
    assert plan.action.source.content == b"# Guide\n\nNew.\n"
    assert plan.action.source.content_digest != next(iter(previous_state.values())).content_digest
    assert plan.action.source.revision == plan.action.source.content_digest
    assert plan.complete_cursor is None


def test_unrelated_repository_commit_advances_snapshot_without_reprocessing_source() -> None:
    documents = {"docs/guide.md": b"# Guide\n\nUnchanged.\n"}
    previous = build_connector(documents, revision="main@sha1:1111111111111111")
    desired = build_connector(documents, revision="main@sha1:2222222222222222")

    plan = git_markdown.plan_next(desired, applied_sources(previous))

    assert previous.cursor.revision != desired.cursor.revision
    assert previous.cursor.digest == desired.cursor.digest
    assert plan == git_markdown.SyncPlan(action=None, complete_cursor=desired.cursor)


def test_inventory_digest_is_canonical_utf8_for_non_ascii_source_identity() -> None:
    connector = build_connector({"docs/résumé.md": "# Résumé\n".encode()})
    payload = git_markdown.inventory_payload(connector)
    sources = cast(list[dict[str, object]], payload["sources"])
    digest_items = [
        {
            key: source[key]
            for key in (
                "source_id",
                "source_path",
                "source_revision",
                "content_digest",
                "acl_digest",
                "metadata",
            )
        }
        for source in sources
    ]
    canonical = json.dumps(digest_items, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()

    assert b"r\xc3\xa9sum\xc3\xa9.md" in canonical
    assert payload["inventory_digest"] == f"sha256:{hashlib.sha256(canonical).hexdigest()}"


def test_acl_only_change_selects_present_action_and_mirrors_new_acl() -> None:
    content = {"docs/policy.md": b"# Policy\n"}
    old_manifest = manifest()
    new_manifest = copy.deepcopy(old_manifest)
    new_manifest["allowed_principals"] = [{"kind": "matrix", "principal": "@bob:org-a.example"}]
    previous = build_connector(content, document=old_manifest)
    desired = build_connector(content, document=new_manifest)
    old_source = previous.fetch_source(previous.enumerate_sources()[0].source_id)

    plan = git_markdown.plan_next(desired, applied_sources(previous))

    assert isinstance(plan.action, git_markdown.PresentAction)
    assert plan.action.source.content_digest == old_source.content_digest
    assert plan.action.source.revision == old_source.revision
    assert plan.action.source.acl_digest != old_source.acl_digest
    assert plan.action.source.acl.allowed_principals == (
        git_markdown.Principal(kind="matrix", principal="@bob:org-a.example"),
    )


def test_resume_reselects_the_first_unapplied_source_until_full_cursor() -> None:
    connector = build_connector(
        {
            "docs/c.md": b"# C\n",
            "docs/a.md": b"# A\n",
            "docs/b.md": b"# B\n",
        }
    )
    applied: dict[str, git_markdown.AppliedSource] = {}

    for expected in ("docs/a.md", "docs/b.md", "docs/c.md"):
        plan = git_markdown.plan_next(connector, applied)
        assert isinstance(plan.action, git_markdown.PresentAction)
        assert plan.action.source.path == expected
        assert plan.complete_cursor is None
        source = plan.action.source
        applied[source.source_id] = git_markdown.AppliedSource(
            source_id=source.source_id,
            content_digest=source.content_digest,
            acl_digest=source.acl_digest,
        )

    complete = git_markdown.plan_next(connector, applied)
    assert complete == git_markdown.SyncPlan(action=None, complete_cursor=connector.cursor)


def test_delete_and_rename_converge_through_one_lexicographic_action_per_plan() -> None:
    previous = build_connector({"docs/a-old.md": b"# Renamed\n", "docs/keep.md": b"# Keep\n"})
    desired = build_connector({"docs/z-new.md": b"# Renamed\n", "docs/keep.md": b"# Keep\n"})
    applied = applied_sources(previous)
    out_of_scope = git_markdown.AppliedSource(
        source_id="other-corpus/git-markdown/docs/foreign.md",
        content_digest=f"sha256:{'0' * 64}",
        acl_digest=f"sha256:{'1' * 64}",
    )
    applied[out_of_scope.source_id] = out_of_scope

    assert desired.report_deletions(applied) == ("reference-docs/git-markdown/docs/a-old.md",)
    first = git_markdown.plan_next(desired, applied)
    assert first.action == git_markdown.TombstoneAction(
        source_id="reference-docs/git-markdown/docs/a-old.md",
        cursor=desired.cursor,
    )
    assert isinstance(first.action, git_markdown.TombstoneAction)
    assert json.loads(git_markdown.action_json(desired, first)) == {
        "action": "tombstone",
        "connector_id": "git-markdown",
        "source_id": "reference-docs/git-markdown/docs/a-old.md",
        "snapshot_revision": desired.cursor.revision,
        "inventory_digest": desired.cursor.digest,
    }
    del applied[first.action.source_id]

    second = git_markdown.plan_next(desired, applied)
    assert isinstance(second.action, git_markdown.PresentAction)
    assert second.action.source.path == "docs/z-new.md"


def test_empty_complete_inventory_tombstones_the_final_applied_source() -> None:
    previous = build_connector({"docs/final.md": b"# Final\n"})
    desired = build_connector({})

    plan = git_markdown.plan_next(desired, applied_sources(previous))

    assert desired.enumerate_sources() == ()
    assert git_markdown.inventory_payload(desired)["source_count"] == 0
    assert plan.action == git_markdown.TombstoneAction(
        source_id="reference-docs/git-markdown/docs/final.md",
        cursor=desired.cursor,
    )


@pytest.mark.parametrize(
    ("documents", "document", "message"),
    [
        ({"docs/kept.md": b"# Kept\n", "docs/bad.md": b"\xff"}, None, "valid UTF-8"),
        ({"docs/kept.md": b"# Kept\n", "docs/bad.md": b""}, None, "between 1 and"),
    ],
)
def test_failed_or_partial_snapshot_is_never_exposed_for_tombstone_planning(
    documents: Mapping[str, bytes],
    document: dict[str, Any] | None,
    message: str,
) -> None:
    with pytest.raises(git_markdown.ConnectorError, match=message):
        build_connector(documents, document=document)


@pytest.mark.parametrize("unsafe_path", ["../escape.md", "/absolute.md", "docs/../escape.md"])
def test_artifact_rejects_traversal_and_absolute_paths(unsafe_path: str) -> None:
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
            unsafe_path: b"unsafe",
        }
    )

    with pytest.raises(git_markdown.ConnectorError, match="canonical contained relative path"):
        git_markdown.GitMarkdownConnector.from_artifact(
            connector_id="git-markdown",
            status=status_for(raw),
            artifact=raw,
        )


@pytest.mark.parametrize("entry_type", [tarfile.SYMTYPE, tarfile.LNKTYPE, tarfile.FIFOTYPE])
def test_artifact_rejects_links_and_special_entries(entry_type: bytes) -> None:
    special = tarfile.TarInfo("docs/special.md")
    special.type = entry_type
    special.linkname = "docs/source.md"
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
        },
        extra=[(special, None)],
    )

    with pytest.raises(git_markdown.ConnectorError, match="link or special"):
        git_markdown.GitMarkdownConnector.from_artifact(
            connector_id="git-markdown",
            status=status_for(raw),
            artifact=raw,
        )


def test_artifact_ignores_unrelated_repository_symlink_without_extracting_it() -> None:
    compatibility_link = tarfile.TarInfo("AGENTS.md")
    compatibility_link.type = tarfile.SYMTYPE
    compatibility_link.linkname = ".agents/AGENTS.md"
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
        },
        extra=[(compatibility_link, None)],
    )

    connector = git_markdown.GitMarkdownConnector.from_artifact(
        connector_id="git-markdown",
        status=status_for(raw),
        artifact=raw,
    )

    assert connector.enumerate_sources() == (
        git_markdown.SourceReference(
            source_id="reference-docs/git-markdown/docs/source.md",
            path="docs/source.md",
        ),
    )


def test_artifact_rejects_duplicate_case_colliding_paths() -> None:
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
            "DOCS/SOURCE.MD": b"# Collision\n",
        }
    )

    with pytest.raises(git_markdown.ConnectorError, match="duplicate or case-colliding"):
        git_markdown.GitMarkdownConnector.from_artifact(
            connector_id="git-markdown",
            status=status_for(raw),
            artifact=raw,
        )


def test_artifact_rejects_status_digest_mismatch() -> None:
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
        }
    )
    status = status_for(raw)
    mismatched = git_markdown.ArtifactStatus(
        revision=status.revision,
        digest=f"sha256:{'0' * 64}",
        url=status.url,
        size=status.size,
    )

    with pytest.raises(git_markdown.ConnectorError, match="digest does not match"):
        git_markdown.GitMarkdownConnector.from_artifact(
            connector_id="git-markdown",
            status=mismatched,
            artifact=raw,
        )


def test_connector_rejects_git_lfs_pointer_instead_of_indexing_it() -> None:
    lfs_pointer = (
        b"version https://git-lfs.github.com/spec/v1\n"
        b"oid sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\n"
        b"size 123\n"
    )

    with pytest.raises(git_markdown.ConnectorError, match="Git LFS pointer"):
        build_connector({"docs/source.md": lfs_pointer})


def test_connector_rejects_ambiguous_uppercase_markdown_suffix() -> None:
    with pytest.raises(git_markdown.ConnectorError, match=r"lowercase \.md suffix"):
        build_connector({"docs/source.MD": b"# Source\n"})


@pytest.mark.parametrize(
    "principal",
    [
        "@alice:matrix.org",
        "@alice:MATRIX.ORG",
        "@alice:matrix-host.example",
        "@alice:matrix.org.",
        "@alice:matrix.org:8448",
        "@alice:1.2.3.4",
        "@alice:1.2.3.4:8448",
        "@alice:[2001:db8::1]",
        "@alice:[2001:db8::1]:8448",
    ],
)
def test_connector_preserves_valid_matrix_server_name_forms(principal: str) -> None:
    connector = build_connector({"docs/source.md": b"# Source\n"}, document=manifest(principal=principal))
    source = connector.fetch_source(connector.enumerate_sources()[0].source_id)

    assert source.acl.allowed_principals == (git_markdown.Principal(kind="matrix", principal=principal),)


@pytest.mark.parametrize(
    "server_name",
    [
        ".example.org",
        "example..org",
        "-example.org",
        "example-.org",
        "example.org..",
        f"{'a' * 64}.example.org",
    ],
)
def test_connector_rejects_malformed_matrix_dns_server_names(server_name: str) -> None:
    with pytest.raises(git_markdown.ConnectorError, match="invalid server name"):
        build_connector(
            {"docs/source.md": b"# Source\n"},
            document=manifest(principal=f"@alice:{server_name}"),
        )


def test_artifact_rejection_diagnostics_do_not_reflect_source_controlled_values() -> None:
    hostile = "private-partner-redacted"
    base = {
        git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
        "docs/source.md": b"# Source\n",
    }

    special = tarfile.TarInfo(f"docs/{hostile}.md")
    special.type = tarfile.SYMTYPE
    special.linkname = "docs/source.md"
    duplicate_manifest = (
        json.dumps(manifest())
        .encode()
        .replace(
            b'"corpus":',
            f'"{hostile}":1,"{hostile}":2,"corpus":'.encode(),
        )
    )
    unknown_manifest = manifest()
    unknown_manifest[hostile] = "secret"

    hostile_artifacts = (
        hostile.encode(),
        artifact_bytes({**base, f"docs/{hostile}/../escape.md": b"unsafe"}),
        artifact_bytes({**base, f"docs/{hostile}\udcff.md": b"unsafe"}),
        artifact_bytes({**base, f"docs/{hostile}.md": b"# First\n", f"DOCS/{hostile.upper()}.MD": b"# Second\n"}),
        artifact_bytes(base, extra=[(special, None)]),
        artifact_bytes({**base, f"docs/{hostile}.md": b"\xff"}),
        artifact_bytes(
            {
                **base,
                f"docs/{hostile}.md": (
                    b"version https://git-lfs.github.com/spec/v1\n"
                    b"oid sha256:0123456789abcdef0123456789abcdef"
                    b"0123456789abcdef0123456789abcdef\n"
                    b"size 123\n"
                ),
            }
        ),
        artifact_bytes({**base, f"docs/{hostile}.MD": b"# Source\n"}),
        artifact_bytes(
            {
                git_markdown.ACL_MANIFEST_PATH: json.dumps(unknown_manifest).encode(),
                "docs/source.md": b"# Source\n",
            }
        ),
        artifact_bytes(
            {
                git_markdown.ACL_MANIFEST_PATH: duplicate_manifest,
                "docs/source.md": b"# Source\n",
            }
        ),
        artifact_bytes(
            {
                git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest(principal=f"@alice:[{hostile}]")).encode(),
                "docs/source.md": b"# Source\n",
            }
        ),
    )

    for raw in hostile_artifacts:
        assert_content_free_rejection(raw, hostile)


def test_artifact_status_rejection_does_not_reflect_invalid_authority() -> None:
    hostile = "private-partner-redacted"
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
        }
    )
    status = status_for(raw)
    unsafe_urls = (
        status.url.replace("/gitrepository/", f":{hostile}/gitrepository/"),
        f"http://{hostile}\uff0fexample/gitrepository/flux-system/flux-system/latest.tar.gz",
    )

    for unsafe_url in unsafe_urls:
        unsafe_status = git_markdown.ArtifactStatus(
            revision=status.revision,
            digest=status.digest,
            url=unsafe_url,
            size=status.size,
        )
        with pytest.raises(git_markdown.ConnectorError) as caught:
            git_markdown.GitMarkdownConnector.from_artifact(
                connector_id="git-markdown",
                status=unsafe_status,
                artifact=raw,
            )

        rendered = "".join(traceback.format_exception(caught.value))
        assert hostile not in str(caught.value)
        assert hostile not in rendered


@pytest.mark.parametrize(
    ("mutation", "message"),
    [
        (lambda item: item.pop("allowed_groups"), "missing required fields"),
        (lambda item: item.update({"role": "reader"}), "unknown fields"),
        (lambda item: item["allowed_principals"][0].update({"role": "owner"}), "unknown fields"),
        (lambda item: item.update({"allowed_principals": [], "allowed_groups": []}), "at least one"),
        (lambda item: item.update({"sources": []}), "unknown fields"),
    ],
)
def test_source_owned_acl_manifest_rejects_missing_unknown_or_unsafe_fields(
    mutation: Any,
    message: str,
) -> None:
    document = manifest()
    mutation(document)

    with pytest.raises(git_markdown.ConnectorError, match=message):
        build_connector({"docs/source.md": b"# Source\n"}, document=document)


def test_artifact_size_bound_is_checked_before_reading_content() -> None:
    raw = artifact_bytes(
        {
            git_markdown.ACL_MANIFEST_PATH: json.dumps(manifest()).encode(),
            "docs/source.md": b"# Source\n",
        }
    )
    status = status_for(raw)
    oversized = git_markdown.ArtifactStatus(
        revision=status.revision,
        digest=status.digest,
        url=status.url,
        size=git_markdown.MAX_ARTIFACT_BYTES + 1,
    )

    with pytest.raises(git_markdown.ConnectorError, match="artifact size"):
        git_markdown.GitMarkdownConnector.from_artifact(
            connector_id="git-markdown",
            status=oversized,
            artifact=raw,
        )
