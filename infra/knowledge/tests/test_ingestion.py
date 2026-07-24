"""Unit tests for the fail-closed knowledge ingestion boundary."""

from __future__ import annotations

import copy
import hashlib
import http.client
import json
import socket
import stat
import threading
import time
import zipfile
from collections.abc import Iterator
from contextlib import contextmanager
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path, PurePosixPath
from typing import Any, ClassVar, cast, override
from unittest import mock

import ingestion
import pytest

MATRIX_SOURCE = "# Matrix\n\nOnly approved readers."
PARTNER_SOURCE = "# Partner\n\nPublic joint brief."


def source_digest(content: str) -> str:
    return f"sha256:{hashlib.sha256(content.encode()).hexdigest()}"


def valid_manifest() -> dict[str, Any]:
    return {
        "schema_version": 1,
        "corpus": "reference-docs",
        "sources": [
            {
                "path": "matrix.md",
                "digest": source_digest(MATRIX_SOURCE),
                "source": {
                    "id": "reference-docs/matrix",
                    "title": "Matrix policy",
                    "locator": "git:docs/matrix.md",
                    "revision": "sha256:matrix-v1",
                },
                "classification": "approved_non_public",
                "allowed_principals": [
                    {"kind": "matrix", "principal": "@alice:org-a.example"},
                    {
                        "kind": "bridged_matrix",
                        "network": "slack",
                        "principal": "@slack_bob:org-a.example",
                    },
                ],
                "allowed_groups": [],
            },
        ],
    }


def write_bundle(tmp_path: Path, document: dict[str, object] | None = None) -> tuple[Path, Path]:
    source_root = tmp_path / "sources"
    source_root.mkdir()
    (source_root / "matrix.md").write_text(MATRIX_SOURCE, encoding="utf-8")
    (source_root / "partner.md").write_text(PARTNER_SOURCE, encoding="utf-8")
    manifest = tmp_path / "manifest.json"
    manifest.write_text(json.dumps(document or valid_manifest()), encoding="utf-8")
    return manifest, source_root


class FakeResult:
    def __init__(self, document: Path, status: str = "success") -> None:
        self._document = document
        self._status = status

    @property
    def document(self) -> object:
        return self._document

    @property
    def status(self) -> object:
        return type("FakeStatus", (), {"value": self._status})()


class FakeConverter:
    def __init__(self, status: str = "success") -> None:
        self.status = status
        self.calls: list[Path] = []

    def convert(
        self,
        source: Path,
        *,
        raises_on_error: bool,
        max_file_size: int,
        max_num_pages: int,
    ) -> FakeResult:
        assert raises_on_error
        assert max_file_size == ingestion.MAX_SOURCE_BYTES
        assert max_num_pages == ingestion.MAX_PAGES
        self.calls.append(source)
        return FakeResult(source, self.status)


class FakeChunker:
    def chunk(self, document: object) -> list[str]:
        path = document
        assert isinstance(path, Path)
        return [part for part in path.read_text(encoding="utf-8").split("\n\n") if part]

    def contextualize(self, chunk: object) -> str:
        assert isinstance(chunk, str)
        return chunk


def build_bound_records(
    tmp_path: Path,
    manifest: ingestion.SourceManifest,
    *,
    converter: ingestion.DocumentConverter | None = None,
    chunker: ingestion.DocumentChunker | None = None,
) -> list[dict[str, object]]:
    raw_groups = ingestion.build_raw_records(
        manifest,
        converter=converter or FakeConverter(),
        chunker=chunker or FakeChunker(),
    )
    raw_root = tmp_path / f"raw-{len(list(tmp_path.glob('raw-*'))):06d}"
    ingestion.write_raw_record_groups(raw_root, raw_groups)
    return ingestion.bind_raw_records(manifest, raw_root)


def test_manifest_normalizes_acl_and_validates_every_source(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)

    assert manifest.corpus == "reference-docs"
    assert [source.metadata.source_id for source in manifest.sources] == ["reference-docs/matrix"]
    principals = manifest.sources[0].allowed_principals
    assert [principal.kind for principal in principals] == ["bridged_matrix", "matrix"]


def test_manifest_accepts_one_exact_partner_group_source(tmp_path: Path) -> None:
    document = valid_manifest()
    document["sources"][0]["allowed_principals"] = []
    document["sources"][0]["allowed_groups"] = ["partner/org-b-a2a/product"]
    manifest_path, source_root = write_bundle(tmp_path, document)

    manifest = ingestion.load_manifest(manifest_path, source_root)

    assert manifest.sources[0].allowed_groups == ("partner/org-b-a2a/product",)


@pytest.mark.parametrize(
    "principal",
    [
        "@alice:matrix.org",
        "@alice:MATRIX.ORG",
        "@alice:matrix-host.example",
        f"@alice:{'a' * 63}.example.org",
        "@alice:matrix.org.",
        "@alice:matrix.org:8448",
        "@alice:1.2.3.4",
        "@alice:1.2.3.4:8448",
        "@alice:[2001:db8::1]",
        "@alice:[2001:db8::1]:8448",
    ],
)
def test_manifest_preserves_valid_matrix_server_name_forms(tmp_path: Path, principal: str) -> None:
    document = valid_manifest()
    document["sources"][0]["allowed_principals"] = [{"kind": "matrix", "principal": principal}]
    manifest_path, source_root = write_bundle(tmp_path, document)

    manifest = ingestion.load_manifest(manifest_path, source_root)

    assert manifest.sources[0].allowed_principals == (ingestion.Principal(kind="matrix", principal=principal),)


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
def test_manifest_rejects_malformed_matrix_dns_server_names(tmp_path: Path, server_name: str) -> None:
    document = valid_manifest()
    document["sources"][0]["allowed_principals"] = [{"kind": "matrix", "principal": f"@alice:{server_name}"}]
    manifest_path, source_root = write_bundle(tmp_path, document)

    with pytest.raises(ingestion.IngestionError, match="invalid server name") as caught:
        ingestion.load_manifest(manifest_path, source_root)

    assert server_name not in str(caught.value)


@pytest.mark.parametrize("source_path", ["matrix.md", "docs/matrix.md"])
def test_manifest_preserves_canonical_source_paths(tmp_path: Path, source_path: str) -> None:
    document = valid_manifest()
    document["sources"][0]["path"] = source_path
    manifest_path, source_root = write_bundle(tmp_path, document)
    source = source_root.joinpath(*PurePosixPath(source_path).parts)
    source.parent.mkdir(parents=True, exist_ok=True)
    source.write_text(MATRIX_SOURCE, encoding="utf-8")

    manifest = ingestion.load_manifest(manifest_path, source_root)

    assert manifest.sources[0].relative_path.as_posix() == source_path


@pytest.mark.parametrize(
    "source_path",
    [
        "./matrix.md",
        "docs//matrix.md",
        "docs/./matrix.md",
        "matrix.md/",
    ],
)
def test_manifest_rejects_noncanonical_source_paths(tmp_path: Path, source_path: str) -> None:
    document = valid_manifest()
    document["sources"][0]["path"] = source_path
    manifest_path, source_root = write_bundle(tmp_path, document)
    source = source_root.joinpath(*PurePosixPath(source_path).parts)
    source.parent.mkdir(parents=True, exist_ok=True)
    source.write_text(MATRIX_SOURCE, encoding="utf-8")

    with pytest.raises(ingestion.IngestionError, match="canonical contained relative path") as caught:
        ingestion.load_manifest(manifest_path, source_root)

    assert source_path not in str(caught.value)


@pytest.mark.parametrize(
    ("mutation", "message"),
    [
        (lambda doc: doc["sources"][0].pop("classification"), "missing required fields"),
        (lambda doc: doc["sources"][0].pop("digest"), "missing required fields"),
        (lambda doc: doc["sources"][0].update({"unexpected": True}), "unknown fields"),
        (lambda doc: doc["sources"][0]["source"].update({"unexpected": True}), "unknown fields"),
        (lambda doc: doc["sources"][0].update({"classification": "internal"}), "classification is unknown"),
        (
            lambda doc: doc["sources"][0].update({"allowed_principals": [{"kind": "matrix", "principal": "alice"}]}),
            "full Matrix user ID",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "matrix", "principal": "@alice:999.999.999.999"}]}
            ),
            "invalid IPv4 server name",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "matrix", "principal": "@alice:org.example:+80"}]}
            ),
            "invalid server port",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "matrix", "principal": "@alice:org.example:8_0"}]}
            ),
            "invalid server port",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "matrix", "principal": "@alice:org.example:٨٠"}]}
            ),
            "invalid server port",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "matrix", "principal": "@alice:[fe80::1%eth0]"}]}
            ),
            "invalid IPv6 server name",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {
                    "allowed_principals": [
                        {
                            "kind": "matrix",
                            "network": "slack",
                            "principal": "@alice:org-a.example",
                        }
                    ]
                }
            ),
            "network is forbidden",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "bridged_matrix", "principal": "@alice:org-a.example"}]}
            ),
            "network is required",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [{"kind": "azp", "principal": "partner-client"}]}
            ),
            "kind must be matrix or bridged_matrix",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {
                    "allowed_principals": [
                        {
                            "kind": "bridged_matrix",
                            "network": "Slack_Team",
                            "principal": "@alice:org-a.example",
                        }
                    ]
                }
            ),
            "DNS-1123 label",
        ),
        (
            lambda doc: doc["sources"][0]["allowed_principals"][0].update({"unexpected": True}),
            "unknown fields",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [doc["sources"][0]["allowed_principals"][0]] * 2}
            ),
            "duplicate principal",
        ),
        (
            lambda doc: doc["sources"][0].update({"allowed_principals": [], "allowed_groups": ["partner/org-b/*"]}),
            "exact partner",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {
                    "allowed_principals": [],
                    "allowed_groups": ["partner/org-b-a2a/product"] * 2,
                }
            ),
            "duplicate group",
        ),
        (
            lambda doc: doc["sources"][0].update({"allowed_principals": [], "allowed_groups": ["partner-client"]}),
            "exact partner",
        ),
        (
            lambda doc: doc["sources"][0].update({"allowed_principals": [], "allowed_groups": ["local/admins"]}),
            "exact partner",
        ),
        (
            lambda doc: doc["sources"][0].update({"allowed_principals": [], "allowed_groups": ["partner/org-b"]}),
            "exact partner",
        ),
        (
            lambda doc: doc["sources"][0].update(
                {"allowed_principals": [], "allowed_groups": ["partner/Org-B/product"]}
            ),
            "exact partner",
        ),
        (lambda doc: doc["sources"][0].pop("allowed_principals"), "missing required fields"),
        (lambda doc: doc["sources"][0].pop("allowed_groups"), "missing required fields"),
        (
            lambda doc: doc["sources"][0].update({"allowed_principals": [], "allowed_groups": []}),
            "authorization operand",
        ),
        (
            lambda doc: doc["sources"][0].update({"path": "../matrix.md"}),
            "contained relative path",
        ),
        (
            lambda doc: doc["sources"][0]["source"].update({"id": "other/matrix"}),
            "namespaced below corpus",
        ),
        (
            lambda doc: doc["sources"][0].update({"digest": f"sha256:{'A' * 64}"}),
            "64 lowercase hexadecimal",
        ),
    ],
)
def test_manifest_rejects_ambiguous_security_metadata(
    tmp_path: Path,
    mutation: Any,
    message: str,
) -> None:
    document = copy.deepcopy(valid_manifest())
    mutation(document)
    manifest_path, source_root = write_bundle(tmp_path, document)

    with pytest.raises(ingestion.IngestionError, match=message):
        ingestion.load_manifest(manifest_path, source_root)


def test_manifest_rejects_duplicate_json_keys(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest_path.write_text(
        '{"schema_version":1,"schema_version":1,"corpus":"reference-docs","sources":[]}',
        encoding="utf-8",
    )

    with pytest.raises(ingestion.IngestionError, match="duplicate JSON object key"):
        ingestion.load_manifest(manifest_path, source_root)


def test_manifest_rejects_json_escaped_surrogate_as_ingestion_error(tmp_path: Path) -> None:
    document = valid_manifest()
    document["corpus"] = "\ud800"
    manifest_path, source_root = write_bundle(tmp_path, document)

    with pytest.raises(ingestion.IngestionError) as caught:
        ingestion.load_manifest(manifest_path, source_root)

    assert str(caught.value) == "manifest.corpus must be valid Unicode text"
    assert "\ud800" not in str(caught.value)


def test_manifest_validates_every_source_before_docling(tmp_path: Path) -> None:
    document = valid_manifest()
    document["sources"][0]["classification"] = "internal"
    manifest_path, source_root = write_bundle(tmp_path, document)
    converter = FakeConverter()

    with pytest.raises(ingestion.IngestionError, match="classification is unknown"):
        ingestion.load_manifest(manifest_path, source_root)

    assert converter.calls == []


def test_manifest_rejects_source_content_that_does_not_match_digest(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    (source_root / "matrix.md").write_text("# Matrix\n\nTampered policy.", encoding="utf-8")

    with pytest.raises(ingestion.IngestionError, match="content digest does not match manifest"):
        ingestion.load_manifest(manifest_path, source_root)


def test_manifest_requires_one_source_per_ingestion_run(tmp_path: Path) -> None:
    document = valid_manifest()
    partner = copy.deepcopy(document["sources"][0])
    partner["path"] = "partner.md"
    partner["source"] = {
        "id": "reference-docs/partner",
        "locator": "git:docs/partner.md",
        "revision": "sha256:partner-v1",
    }
    document["sources"].append(partner)
    manifest_path, source_root = write_bundle(tmp_path, document)

    with pytest.raises(ingestion.IngestionError, match="maximum of 1"):
        ingestion.load_manifest(manifest_path, source_root)


def test_snapshot_pins_manifest_and_document_bytes(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    snapshot_root = tmp_path / "snapshot"
    snapshot_root.mkdir()

    manifest = ingestion.snapshot_bundle(manifest_path, source_root, snapshot_root)

    assert len(manifest.sources) == 1
    assert manifest.sources[0].content_digest == source_digest(MATRIX_SOURCE)
    assert (snapshot_root / "manifest.json").read_bytes() == manifest_path.read_bytes()
    assert (snapshot_root / "sources/matrix.md").read_bytes() == (source_root / "matrix.md").read_bytes()


def test_snapshot_rejects_source_swap_between_validation_and_copy(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    snapshot_root = tmp_path / "snapshot"
    snapshot_root.mkdir()
    original_read = ingestion._read_verified_source
    reads = 0

    def swap_before_second_read(path: Path, max_bytes: int, expected_digest: str) -> bytes:
        nonlocal reads
        reads += 1
        if reads == 2:
            path.write_text("# Matrix\n\nSwapped after validation.", encoding="utf-8")
        return original_read(path, max_bytes, expected_digest)

    monkeypatch.setattr(ingestion, "_read_verified_source", swap_before_second_read)

    with pytest.raises(ingestion.IngestionError, match="content digest does not match manifest"):
        ingestion.snapshot_bundle(manifest_path, source_root, snapshot_root)


def test_snapshot_uses_copied_manifest_when_live_acl_changes(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    snapshot_root = tmp_path / "snapshot"
    snapshot_root.mkdir()
    original_write = ingestion._atomic_write_bytes

    def change_live_manifest_after_copy(path: Path, content: bytes, *, mode: int = 0o640) -> None:
        original_write(path, content, mode=mode)
        if path == snapshot_root / "manifest.json":
            changed = valid_manifest()
            changed["sources"][0]["classification"] = "regulated"
            changed["sources"][0]["allowed_principals"] = [{"kind": "matrix", "principal": "@mallory:org-a.example"}]
            manifest_path.write_text(json.dumps(changed), encoding="utf-8")

    monkeypatch.setattr(ingestion, "_atomic_write_bytes", change_live_manifest_after_copy)

    manifest = ingestion.snapshot_bundle(manifest_path, source_root, snapshot_root)

    assert manifest.sources[0].classification == "approved_non_public"
    assert [principal.principal for principal in manifest.sources[0].allowed_principals] == [
        "@slack_bob:org-a.example",
        "@alice:org-a.example",
    ]


def test_snapshot_prepares_one_parser_only_source_boundary(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    snapshot_root = tmp_path / "snapshot"
    raw_root = tmp_path / "work/raw"
    parser_tmp_root = tmp_path / "work/parser-tmp"
    snapshot_root.mkdir()
    manifest = ingestion.snapshot_bundle(manifest_path, source_root, snapshot_root)

    ingestion.prepare_parser_boundaries(manifest, snapshot_root, raw_root, parser_tmp_root)

    parser_root = snapshot_root / "parser"
    assert [entry.name for entry in parser_root.iterdir()] == ["document.md"]
    assert (parser_root / "document.md").stat().st_ino == (snapshot_root / "sources/matrix.md").stat().st_ino
    assert list(raw_root.iterdir()) == []
    assert list(parser_tmp_root.iterdir()) == []
    assert stat.S_IMODE(parser_root.stat().st_mode) == 0o2770
    assert stat.S_IMODE(raw_root.stat().st_mode) == 0o2770
    assert stat.S_IMODE(parser_tmp_root.stat().st_mode) == 0o2770


def test_isolated_parser_reads_exactly_one_source(tmp_path: Path) -> None:
    input_root = tmp_path / "input"
    output_root = tmp_path / "output"
    input_root.mkdir()
    output_root.mkdir()
    source = input_root / "document.md"
    source.write_text("# One\n\nOnly this source.", encoding="utf-8")
    converter = FakeConverter()

    record_count = ingestion.parse_isolated_source(
        input_root,
        output_root / ingestion.RAW_RECORD_FILENAME,
        converter=converter,
        chunker=FakeChunker(),
    )

    assert record_count == 2
    assert converter.calls == [source]
    assert [entry.name for entry in output_root.iterdir()] == [ingestion.RAW_RECORD_FILENAME]

    unused_input = tmp_path / "unused-input"
    unused_output = tmp_path / "unused-output"
    unused_input.mkdir()
    unused_output.mkdir()
    unused_converter = FakeConverter()
    with pytest.raises(ingestion.IngestionError, match="exactly one source"):
        ingestion.parse_isolated_source(
            unused_input,
            unused_output / ingestion.RAW_RECORD_FILENAME,
            converter=unused_converter,
            chunker=FakeChunker(),
        )
    assert unused_converter.calls == []
    assert list(unused_output.iterdir()) == []


def test_isolated_parser_rejects_multiple_visible_sources(tmp_path: Path) -> None:
    input_root = tmp_path / "input"
    output_root = tmp_path / "output"
    input_root.mkdir()
    output_root.mkdir()
    (input_root / "document.md").write_text("first", encoding="utf-8")
    (input_root / "other.md").write_text("second", encoding="utf-8")
    converter = FakeConverter()

    with pytest.raises(ingestion.IngestionError, match="exactly one source"):
        ingestion.parse_isolated_source(
            input_root,
            output_root / ingestion.RAW_RECORD_FILENAME,
            converter=converter,
            chunker=FakeChunker(),
        )

    assert converter.calls == []


def test_manifest_rejects_unsafe_office_archive(tmp_path: Path) -> None:
    source_root = tmp_path / "sources"
    source_root.mkdir()
    archive_path = source_root / "unsafe.docx"
    with zipfile.ZipFile(archive_path, "w") as archive:
        archive.writestr("../escape.xml", "<document/>")
    document = valid_manifest()
    document["sources"] = [document["sources"][0]]
    document["sources"][0]["path"] = "unsafe.docx"
    manifest_path = tmp_path / "manifest.json"
    manifest_path.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(ingestion.IngestionError, match="unsafe or duplicate entry"):
        ingestion.load_manifest(manifest_path, source_root)


@pytest.mark.parametrize("unsafe_name", ["word//document.xml", "word/./document.xml"])
def test_manifest_rejects_noncanonical_office_archive_paths(
    tmp_path: Path,
    unsafe_name: str,
) -> None:
    source_root = tmp_path / "sources"
    source_root.mkdir()
    archive_path = source_root / "unsafe.docx"
    with zipfile.ZipFile(archive_path, "w") as archive:
        archive.writestr("word/document.xml", "<document/>")
        archive.writestr(unsafe_name, "<duplicate/>")
    document = valid_manifest()
    document["sources"] = [document["sources"][0]]
    document["sources"][0]["path"] = "unsafe.docx"
    manifest_path = tmp_path / "manifest.json"
    manifest_path.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(ingestion.IngestionError, match="unsafe or duplicate entry"):
        ingestion.load_manifest(manifest_path, source_root)


def test_manifest_rejects_office_archive_symlink(tmp_path: Path) -> None:
    source_root = tmp_path / "sources"
    source_root.mkdir()
    archive_path = source_root / "unsafe.docx"
    symlink = zipfile.ZipInfo("word/document.xml")
    symlink.create_system = 3
    symlink.external_attr = (stat.S_IFLNK | 0o777) << 16
    with zipfile.ZipFile(archive_path, "w") as archive:
        archive.writestr(symlink, "../target.xml")
    document = valid_manifest()
    document["sources"] = [document["sources"][0]]
    document["sources"][0]["path"] = "unsafe.docx"
    manifest_path = tmp_path / "manifest.json"
    manifest_path.write_text(json.dumps(document), encoding="utf-8")

    with pytest.raises(ingestion.IngestionError, match="unsafe or duplicate entry"):
        ingestion.load_manifest(manifest_path, source_root)


def test_chunks_keep_exact_acl_and_stable_ids_across_acl_changes(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)
    records = build_bound_records(
        tmp_path,
        manifest,
        converter=FakeConverter(),
        chunker=FakeChunker(),
    )

    changed = valid_manifest()
    changed["sources"][0]["classification"] = "regulated"
    changed["sources"][0]["allowed_principals"] = [{"kind": "matrix", "principal": "@carol:org-a.example"}]
    changed_path = tmp_path / "changed.json"
    changed_path.write_text(json.dumps(changed), encoding="utf-8")
    changed_manifest = ingestion.load_manifest(changed_path, source_root)
    changed_records = build_bound_records(
        tmp_path,
        changed_manifest,
        converter=FakeConverter(),
        chunker=FakeChunker(),
    )

    original = {
        record["chunk_id"]: record
        for record in records
        if cast(dict[str, Any], record["metadata"])["source"]["id"] == "reference-docs/matrix"
    }
    replacement = {
        record["chunk_id"]: record
        for record in changed_records
        if cast(dict[str, Any], record["metadata"])["source"]["id"] == "reference-docs/matrix"
    }
    assert original.keys() == replacement.keys()
    assert all(
        cast(dict[str, Any], record["metadata"])["classification"] == "regulated" for record in replacement.values()
    )
    assert all(
        cast(dict[str, Any], record["metadata"])["allowed_principals"]
        == [{"kind": "matrix", "principal": "@carol:org-a.example"}]
        for record in replacement.values()
    )


def test_changed_content_replaces_only_changed_chunk_ids(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)
    before = build_bound_records(
        tmp_path,
        manifest,
        converter=FakeConverter(),
        chunker=FakeChunker(),
    )
    (source_root / "matrix.md").write_text("# Matrix\n\nUpdated policy.", encoding="utf-8")
    updated = valid_manifest()
    updated["sources"][0]["digest"] = source_digest("# Matrix\n\nUpdated policy.")
    manifest_path.write_text(json.dumps(updated), encoding="utf-8")
    after_manifest = ingestion.load_manifest(manifest_path, source_root)
    after = build_bound_records(
        tmp_path,
        after_manifest,
        converter=FakeConverter(),
        chunker=FakeChunker(),
    )

    before_matrix = {
        record["chunk_id"]
        for record in before
        if cast(dict[str, Any], record["metadata"])["source"]["id"] == "reference-docs/matrix"
    }
    after_matrix = {
        record["chunk_id"]
        for record in after
        if cast(dict[str, Any], record["metadata"])["source"]["id"] == "reference-docs/matrix"
    }
    assert len(before_matrix & after_matrix) == 1
    assert len(before_matrix - after_matrix) == 1
    assert len(after_matrix - before_matrix) == 1


def test_docling_partial_success_is_rejected_before_binding(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)

    with pytest.raises(ingestion.IngestionError, match="exact SUCCESS"):
        ingestion.build_raw_records(
            manifest,
            converter=FakeConverter(status="partial_success"),
            chunker=FakeChunker(),
        )


def test_binder_rejects_docling_security_forgery_and_missing_sources(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)
    forged_root = tmp_path / "forged"
    ingestion.write_raw_record_groups(
        forged_root,
        [[{"source_id": "reference-docs/partner", "ordinal": 1, "content": "forged"}]],
    )

    with pytest.raises(ingestion.IngestionError, match="unknown fields"):
        ingestion.bind_raw_records(manifest, forged_root)

    missing_root = tmp_path / "missing"
    missing_root.mkdir()
    with pytest.raises(ingestion.IngestionError, match="one exact parser result"):
        ingestion.bind_raw_records(manifest, missing_root)

    extra_root = tmp_path / "extra"
    ingestion.write_raw_record_groups(
        extra_root,
        [[{"ordinal": 1, "content": "valid"}]],
    )
    (extra_root / "laundered.jsonl").write_text('{"ordinal":1,"content":"other"}\n', encoding="utf-8")
    with pytest.raises(ingestion.IngestionError, match="one exact parser result"):
        ingestion.bind_raw_records(manifest, extra_root)


def test_stable_ids_distinguish_sources_and_duplicate_occurrences(tmp_path: Path) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)
    raw_root = tmp_path / "raw"
    ingestion.write_raw_record_groups(
        raw_root,
        [[{"ordinal": 1, "content": "same"}, {"ordinal": 2, "content": "same"}]],
    )

    records = ingestion.bind_raw_records(manifest, raw_root)
    assert len({record["chunk_id"] for record in records}) == 2

    partner_document = valid_manifest()
    partner_document["sources"][0]["path"] = "partner.md"
    partner_document["sources"][0]["digest"] = source_digest(PARTNER_SOURCE)
    partner_document["sources"][0]["source"] = {
        "id": "reference-docs/partner",
        "locator": "git:docs/partner.md",
        "revision": "sha256:partner-v1",
    }
    partner_path = tmp_path / "partner-manifest.json"
    partner_path.write_text(json.dumps(partner_document), encoding="utf-8")
    partner_manifest = ingestion.load_manifest(partner_path, source_root)
    partner_raw = tmp_path / "partner-raw"
    ingestion.write_raw_record_groups(partner_raw, [[{"ordinal": 1, "content": "same"}]])
    partner_records = ingestion.bind_raw_records(partner_manifest, partner_raw)

    assert partner_records[0]["chunk_id"] not in {record["chunk_id"] for record in records}


@pytest.mark.parametrize("contents", [("first", "second"), ("same", "same")])
def test_binder_rejects_forced_identifier_collision(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    contents: tuple[str, str],
) -> None:
    manifest_path, source_root = write_bundle(tmp_path)
    manifest = ingestion.load_manifest(manifest_path, source_root)
    raw_root = tmp_path / "raw"
    ingestion.write_raw_record_groups(
        raw_root,
        [[{"ordinal": 1, "content": contents[0]}, {"ordinal": 2, "content": contents[1]}]],
    )
    monkeypatch.setattr(ingestion, "_chunk_id", lambda *_args: f"sha256:{'a' * 64}")

    with pytest.raises(ingestion.IngestionError, match="identifier collision"):
        ingestion.bind_raw_records(manifest, raw_root)


def test_jsonl_reader_rejects_one_oversized_physical_line(tmp_path: Path) -> None:
    path = tmp_path / "oversized.jsonl"
    path.write_bytes(b"{" + b"x" * ingestion.MAX_JSONL_LINE_BYTES + b"}\n")

    with pytest.raises(ingestion.IngestionError, match="bounded line size"):
        ingestion.read_raw_records(path)


def test_jsonl_reader_rejects_json_escaped_surrogate_as_ingestion_error(tmp_path: Path) -> None:
    path = tmp_path / "invalid-unicode.jsonl"
    path.write_bytes(b'{"ordinal":1,"content":"\\ud800"}\n')

    with pytest.raises(ingestion.IngestionError) as caught:
        ingestion.read_raw_records(path)

    assert str(caught.value) == "Docling chunk must be valid Unicode text"


def test_vector_jsonl_budget_covers_every_bounded_float32_value() -> None:
    assert ingestion.MAX_EMBEDDED_JSONL_BYTES == ingestion.MAX_JSONL_BYTES + ingestion.MAX_TOTAL_CHUNKS * (
        ingestion.EMBEDDING_DIMENSION * ingestion.MAX_FLOAT32_JSON_BYTES + 64
    )
    assert len(json.dumps(-3.4028235e38)) <= ingestion.MAX_FLOAT32_JSON_BYTES
    assert len(json.dumps(1.401298464324817e-45)) <= ingestion.MAX_FLOAT32_JSON_BYTES


class EmbeddingHandler(BaseHTTPRequestHandler):
    calls: ClassVar[list[list[str]]] = []
    tokenize_calls: ClassVar[list[str]] = []
    tokenize_requests: ClassVar[list[dict[str, object]]] = []
    tokenize_authorizations: ClassVar[list[str | None]] = []
    events: ClassVar[list[str]] = []
    dimension = ingestion.EMBEDDING_DIMENSION
    model_override: ClassVar[str | None] = None
    reverse_response = False
    token_count = 1
    tokenize_override: ClassVar[dict[str, object] | bytes | None] = None
    tokenize_content_types: ClassVar[tuple[str, ...]] = ("application/json",)
    embedding_content_types: ClassVar[tuple[str, ...]] = ("application/json",)
    response_framing: ClassVar[str] = "content-length"
    response_byte_delay = 0.0

    def _write_body(self, body: bytes) -> None:
        if type(self).response_byte_delay == 0:
            self.wfile.write(body)
            return
        for byte in body:
            try:
                self.wfile.write(bytes((byte,)))
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                return
            time.sleep(type(self).response_byte_delay)

    def do_POST(self) -> None:
        length = int(self.headers["Content-Length"])
        request = json.loads(self.rfile.read(length))
        if self.path == ingestion.TOKENIZE_PATH:
            type(self).tokenize_calls.append(request["prompt"])
            type(self).tokenize_requests.append(request)
            type(self).tokenize_authorizations.append(self.headers.get("Authorization"))
            type(self).events.append(f"tokenize:{request['prompt']}")
            override = type(self).tokenize_override
            if isinstance(override, bytes):
                body = override
            else:
                tokens = list(range(type(self).token_count))
                body = json.dumps(
                    override
                    or {
                        "count": len(tokens),
                        "max_model_len": ingestion.MAX_MODEL_TOKENS,
                        "tokens": tokens,
                        "token_strs": None,
                    }
                ).encode()
            self.send_response(200)
            for content_type in type(self).tokenize_content_types:
                self.send_header("Content-Type", content_type)
            framing = type(self).response_framing
            if framing == "content-length":
                self.send_header("Content-Length", str(len(body)))
            elif framing == "duplicate-content-length":
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Content-Length", str(len(body)))
            elif framing == "content-length-and-transfer-encoding":
                self.send_header("Content-Length", str(len(body)))
                self.send_header("Transfer-Encoding", "chunked")
            elif framing == "invalid-content-length":
                self.send_header("Content-Length", "invalid")
            elif framing == "oversized-content-length":
                self.send_header("Content-Length", str(ingestion.MAX_TOKENIZE_RESPONSE_BYTES + 1))
            elif framing == "huge-content-length":
                self.send_header("Content-Length", "9" * 5000)
            elif framing == "incomplete-content-length":
                self.send_header("Content-Length", str(len(body) + 1))
            elif framing == "unsupported-transfer-encoding":
                self.send_header("Transfer-Encoding", "gzip")
            elif framing == "chunked":
                self.send_header("Transfer-Encoding", "chunked")
            else:
                raise AssertionError(f"unknown response framing: {framing}")
            self.end_headers()
            if framing == "chunked":
                self._write_body(f"{len(body):x}\r\n".encode() + body + b"\r\n0\r\n\r\n")
                return
            self._write_body(body)
            return
        assert self.path == "/v1/embeddings"
        type(self).calls.append(request["input"])
        type(self).events.append(f"embed:{','.join(request['input'])}")
        data = []
        for index, _ in enumerate(request["input"]):
            vector = [0.0] * type(self).dimension
            vector[index % type(self).dimension] = 1.0
            data.append({"object": "embedding", "index": index, "embedding": vector})
        if type(self).reverse_response:
            data.reverse()
        body = json.dumps(
            {
                "object": "list",
                "data": data,
                "model": type(self).model_override or request["model"],
            }
        ).encode()
        self.send_response(200)
        for content_type in type(self).embedding_content_types:
            self.send_header("Content-Type", content_type)
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self._write_body(body)

    @override
    def log_message(self, format: str, *args: Any) -> None:
        del format, args


@contextmanager
def embedding_server() -> Iterator[str]:
    EmbeddingHandler.calls = []
    EmbeddingHandler.tokenize_calls = []
    EmbeddingHandler.tokenize_requests = []
    EmbeddingHandler.tokenize_authorizations = []
    EmbeddingHandler.events = []
    EmbeddingHandler.model_override = None
    EmbeddingHandler.reverse_response = False
    EmbeddingHandler.token_count = 1
    EmbeddingHandler.tokenize_override = None
    EmbeddingHandler.tokenize_content_types = ("application/json",)
    EmbeddingHandler.embedding_content_types = ("application/json",)
    EmbeddingHandler.response_framing = "content-length"
    EmbeddingHandler.response_byte_delay = 0
    server = ThreadingHTTPServer(("127.0.0.1", 0), EmbeddingHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{server.server_port}/v1/embeddings"
    finally:
        server.shutdown()
        thread.join()
        server.server_close()


def planned_record(chunk_id: str, content: str, embedding: list[float] | None) -> dict[str, object]:
    return {
        "chunk_id": chunk_id,
        "content": content,
        "metadata": {
            "source": {
                "id": "reference-docs/matrix",
                "locator": "git:docs/matrix.md",
                "revision": "sha256:matrix-v1",
                "location": "chunk:000001",
            },
            "classification": "approved_non_public",
            "allowed_principals": [{"kind": "matrix", "principal": "@alice:org-a.example"}],
            "allowed_groups": [],
        },
        "embedding": embedding,
    }


def test_embedding_plan_reuses_unchanged_vectors(tmp_path: Path) -> None:
    existing = [0.0] * ingestion.EMBEDDING_DIMENSION
    existing[10] = 1.0
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [
            planned_record(f"sha256:{'1' * 64}", "unchanged", existing),
            planned_record(f"sha256:{'2' * 64}", "changed", None),
        ],
    )

    with embedding_server() as url:
        total, embedded = ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            allow_loopback=True,
        )

    assert (total, embedded) == (2, 1)
    assert EmbeddingHandler.calls == [["changed"]]
    assert EmbeddingHandler.tokenize_calls == ["changed"]
    records = ingestion._read_jsonl(output_path, with_embedding=True)
    assert records[0]["embedding"] == existing
    assert cast(list[float], records[1]["embedding"])[0] == 1.0


def test_embedding_plan_deduplicates_identical_missing_content(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [
            planned_record(f"sha256:{'4' * 64}", "same", None),
            planned_record(f"sha256:{'5' * 64}", "same", None),
        ],
    )

    with embedding_server() as url:
        total, embedded = ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            allow_loopback=True,
        )

    assert (total, embedded) == (2, 2)
    assert EmbeddingHandler.calls == [["same"]]
    assert EmbeddingHandler.tokenize_calls == ["same"]


def test_embedding_plan_uses_explicit_response_indexes(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [
            planned_record(f"sha256:{'7' * 64}", "first", None),
            planned_record(f"sha256:{'8' * 64}", "second", None),
        ],
    )

    with embedding_server() as url:
        EmbeddingHandler.reverse_response = True
        ingestion.embed_plan(plan_path, output_path, url=url, allow_loopback=True)

    records = ingestion._read_jsonl(output_path, with_embedding=True)
    first = cast(list[float], records[0]["embedding"])
    second = cast(list[float], records[1]["embedding"])
    assert first[0] == 1.0
    assert second[1] == 1.0


def test_embedding_batches_respect_serialized_byte_bound(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    content = "x" * (ingestion.MAX_CHUNK_BYTES - 1024)
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{index:064x}", f"{content}{index}", None) for index in range(3)],
    )

    with embedding_server() as url:
        ingestion.embed_plan(plan_path, output_path, url=url, allow_loopback=True)

    assert [len(batch) for batch in EmbeddingHandler.calls] == [2, 1]


def test_embedding_plan_rejects_wrong_dimension(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'3' * 64}", "changed", None)],
    )
    EmbeddingHandler.dimension = ingestion.EMBEDDING_DIMENSION - 1
    try:
        with (
            embedding_server() as url,
            pytest.raises(ingestion.IngestionError, match="exactly 1024"),
        ):
            ingestion.embed_plan(
                plan_path,
                output_path,
                url=url,
                allow_loopback=True,
            )
    finally:
        EmbeddingHandler.dimension = ingestion.EMBEDDING_DIMENSION


def test_embedding_plan_rejects_response_model_drift(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'6' * 64}", "changed", None)],
    )
    with embedding_server() as url:
        EmbeddingHandler.model_override = "other/model"
        with pytest.raises(ingestion.IngestionError, match="different model"):
            ingestion.embed_plan(plan_path, output_path, url=url, allow_loopback=True)


def test_embedding_plan_accepts_exact_8192_token_input(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'a' * 64}", "bounded", None)],
    )

    with embedding_server() as url:
        EmbeddingHandler.token_count = ingestion.MAX_MODEL_TOKENS
        ingestion.embed_plan(plan_path, output_path, url=url, allow_loopback=True)

    assert EmbeddingHandler.tokenize_calls == ["bounded"]
    assert EmbeddingHandler.calls == [["bounded"]]


def test_embedding_plan_rejects_8193_tokens_before_any_embedding(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'b' * 64}", "too many", None)],
    )

    with embedding_server() as url:
        EmbeddingHandler.token_count = ingestion.MAX_MODEL_TOKENS + 1
        with pytest.raises(ingestion.IngestionError, match=r"maximum of 8192|1\.\.8192"):
            ingestion.embed_plan(plan_path, output_path, url=url, allow_loopback=True)

    assert EmbeddingHandler.tokenize_calls == ["too many"]
    assert EmbeddingHandler.calls == []
    assert not output_path.exists()


@pytest.mark.parametrize(
    ("response", "message"),
    [
        (
            {"count": 1, "max_model_len": 4096, "tokens": [1], "token_strs": None},
            "max_model_len must equal 8192",
        ),
        (
            {"count": 2, "max_model_len": 8192, "tokens": [1], "token_strs": None},
            "count must equal",
        ),
        (
            {
                "count": 1,
                "max_model_len": 8192,
                "tokens": [1],
                "token_strs": None,
                "unexpected": True,
            },
            "unknown fields",
        ),
        (b"{", "invalid JSON"),
    ],
)
def test_embedding_plan_rejects_tokenizer_contract_drift_before_embedding(
    tmp_path: Path,
    response: dict[str, object] | bytes,
    message: str,
) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'c' * 64}", "drift", None)],
    )

    with embedding_server() as url:
        EmbeddingHandler.tokenize_override = response
        with pytest.raises(ingestion.IngestionError, match=message):
            ingestion.embed_plan(plan_path, output_path, url=url, allow_loopback=True)

    assert EmbeddingHandler.calls == []
    assert not output_path.exists()


@pytest.mark.parametrize(
    ("framing", "message"),
    [
        ("duplicate-content-length", "ambiguous response framing"),
        ("content-length-and-transfer-encoding", "ambiguous response framing"),
        ("invalid-content-length", "invalid response framing"),
        ("oversized-content-length", "response exceeds 262144 bytes"),
        ("huge-content-length", "response exceeds 262144 bytes"),
        ("incomplete-content-length", "incomplete response body"),
        ("unsupported-transfer-encoding", "unsupported response framing"),
    ],
)
def test_tokenizer_response_rejects_ambiguous_or_invalid_framing(
    framing: str,
    message: str,
) -> None:
    with embedding_server() as url:
        EmbeddingHandler.response_framing = framing
        with pytest.raises(ingestion.IngestionError, match=message):
            ingestion._preflight_tokenize(
                url,
                model=ingestion.EMBEDDING_MODEL,
                content="framing",
                timeout_seconds=1,
                allow_loopback=True,
                authorization=None,
            )

    assert EmbeddingHandler.calls == []


def test_tokenizer_response_accepts_exact_chunked_framing() -> None:
    with embedding_server() as url:
        EmbeddingHandler.response_framing = "chunked"
        ingestion._preflight_tokenize(
            url,
            model=ingestion.EMBEDDING_MODEL,
            content="chunked",
            timeout_seconds=1,
            allow_loopback=True,
            authorization=None,
        )

    assert EmbeddingHandler.tokenize_calls == ["chunked"]
    assert EmbeddingHandler.calls == []


@pytest.mark.parametrize(
    "content_types",
    [
        (),
        ("application/json", "application/json"),
        ("text/plain",),
        ("application/json; charset",),
        ("application/json; charset =utf-8",),
    ],
    ids=["missing", "duplicate", "non-json", "missing-parameter-value", "space-before-parameter-equals"],
)
def test_tokenizer_response_rejects_invalid_media_types_before_body_read(
    content_types: tuple[str, ...],
) -> None:
    with embedding_server() as url:
        EmbeddingHandler.tokenize_content_types = content_types
        with (
            mock.patch.object(
                http.client.HTTPResponse,
                "read1",
                side_effect=AssertionError("invalid media type reached body read"),
            ),
            pytest.raises(ingestion.IngestionError, match="tokenization backend returned a non-JSON content type"),
        ):
            ingestion._preflight_tokenize(
                url,
                model=ingestion.EMBEDDING_MODEL,
                content="media-type",
                timeout_seconds=1,
                allow_loopback=True,
                authorization=None,
            )

    assert EmbeddingHandler.calls == []


def test_embedding_response_rejects_invalid_media_type_before_body_read() -> None:
    with embedding_server() as url:
        EmbeddingHandler.embedding_content_types = ("text/plain",)
        with (
            mock.patch.object(
                http.client.HTTPResponse,
                "read1",
                side_effect=AssertionError("invalid media type reached body read"),
            ),
            pytest.raises(ingestion.IngestionError, match="embedding backend returned a non-JSON content type"),
        ):
            ingestion._post_embeddings(
                url,
                model=ingestion.EMBEDDING_MODEL,
                inputs=["media-type"],
                timeout_seconds=1,
                allow_loopback=True,
                authorization=None,
            )

    assert EmbeddingHandler.calls == [["media-type"]]


def test_embedding_and_tokenizer_accept_parameterized_json_media_types() -> None:
    content_type = 'Application/JSON; profile="knowledge"; charset=UTF-8'
    with embedding_server() as url:
        EmbeddingHandler.tokenize_content_types = (content_type,)
        EmbeddingHandler.embedding_content_types = (content_type,)
        ingestion._preflight_tokenize(
            url,
            model=ingestion.EMBEDDING_MODEL,
            content="parameterized",
            timeout_seconds=1,
            allow_loopback=True,
            authorization=None,
        )
        vectors = ingestion._post_embeddings(
            url,
            model=ingestion.EMBEDDING_MODEL,
            inputs=["parameterized"],
            timeout_seconds=1,
            allow_loopback=True,
            authorization=None,
        )

    assert EmbeddingHandler.tokenize_calls == ["parameterized"]
    assert EmbeddingHandler.calls == [["parameterized"]]
    assert len(vectors) == 1


@pytest.mark.parametrize(
    ("response", "message"),
    [
        pytest.param(
            b'{"nested":' + b"[" * 10_000 + b"]" * 10_000 + b"}",
            "invalid JSON",
            id="recursive",
        ),
        pytest.param(b"[]", "non-object JSON document", id="non-object"),
        pytest.param(
            b'{"hostile-body-key":1,"hostile-body-key":2}',
            "invalid JSON",
            id="duplicate-key",
        ),
    ],
)
def test_tokenizer_response_rejects_invalid_json_without_echoing_content(
    response: bytes,
    message: str,
) -> None:
    with embedding_server() as url:
        EmbeddingHandler.tokenize_override = response
        with pytest.raises(ingestion.IngestionError, match=message) as error:
            ingestion._preflight_tokenize(
                url,
                model=ingestion.EMBEDDING_MODEL,
                content="json",
                timeout_seconds=1,
                allow_loopback=True,
                authorization=None,
            )

    assert "hostile-body-key" not in str(error.value)
    assert EmbeddingHandler.calls == []


def test_tokenizer_response_obeys_one_absolute_deadline() -> None:
    with embedding_server() as url:
        EmbeddingHandler.response_byte_delay = 0.04
        started = time.monotonic()
        with pytest.raises(ingestion.IngestionError, match="bounded deadline"):
            ingestion._preflight_tokenize(
                url,
                model=ingestion.EMBEDDING_MODEL,
                content="slow",
                timeout_seconds=0.1,
                allow_loopback=True,
                authorization=None,
            )
        elapsed = time.monotonic() - started

    assert elapsed < 0.5
    assert EmbeddingHandler.calls == []


def test_dns_resolution_obeys_the_same_absolute_deadline(monkeypatch: pytest.MonkeyPatch) -> None:
    resolution_started = threading.Event()
    release_resolution = threading.Event()
    resolution_finished = threading.Event()

    def delayed_resolution(*_args: object, **_kwargs: object) -> list[tuple[int, int, int, str, tuple[str, int]]]:
        resolution_started.set()
        release_resolution.wait(timeout=1)
        resolution_finished.set()
        return [
            (
                int(socket.AF_INET),
                int(socket.SOCK_STREAM),
                socket.IPPROTO_TCP,
                "",
                ("127.0.0.1", 9),
            )
        ]

    monkeypatch.setattr(ingestion.socket, "getaddrinfo", delayed_resolution)
    endpoint = ingestion.EmbeddingsEndpoint(
        hostname="localhost",
        port=9,
        path="/v1/embeddings",
    )
    started = time.monotonic()
    try:
        with pytest.raises(ingestion.IngestionError, match="bounded deadline"):
            ingestion._post_bounded_json(
                endpoint,
                path="/tokenize",
                body=b"{}",
                max_request_bytes=16,
                max_response_bytes=16,
                timeout_seconds=0.05,
                authorization=None,
                operation="tokenizer",
            )
    finally:
        release_resolution.set()
    elapsed = time.monotonic() - started

    assert resolution_started.is_set()
    assert resolution_finished.wait(timeout=1)
    assert elapsed < 0.25


def test_all_unique_inputs_are_tokenized_before_the_first_embedding(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [
            planned_record(f"sha256:{'d' * 64}", "first", None),
            planned_record(f"sha256:{'e' * 64}", "second", None),
        ],
    )

    with embedding_server() as url:
        ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            batch_size=1,
            allow_loopback=True,
        )

    assert EmbeddingHandler.events == [
        "tokenize:first",
        "tokenize:second",
        "embed:first",
        "embed:second",
    ]


def test_tokenize_request_is_exact_and_authenticated(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'0' * 64}", "exact request", None)],
    )

    with embedding_server() as url:
        ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            allow_loopback=True,
            authorization="Bearer workload-key",
        )

    assert EmbeddingHandler.tokenize_requests == [
        {
            "model": ingestion.EMBEDDING_MODEL,
            "prompt": "exact request",
            "add_special_tokens": True,
            "return_token_strs": False,
        }
    ]
    assert EmbeddingHandler.tokenize_authorizations == ["Bearer workload-key"]


def _start_checkpoint_ack(
    checkpoint_root: Path,
    *,
    delete_ready: bool = False,
) -> tuple[threading.Thread, threading.Event]:
    started = threading.Event()

    def acknowledge() -> None:
        started.set()
        ready = checkpoint_root / ingestion.CHECKPOINT_READY_FILENAME
        acked = checkpoint_root / ingestion.CHECKPOINT_ACKED_FILENAME
        for _ in range(2000):
            if ready.exists():
                if delete_ready:
                    ready.unlink()
                else:
                    ready.replace(acked)
                return
            threading.Event().wait(0.005)
        raise AssertionError("checkpoint was never published")

    thread = threading.Thread(target=acknowledge, daemon=True)
    thread.start()
    return thread, started


def test_embedding_batch_waits_for_durable_checkpoint_ack(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    checkpoint_root = tmp_path / "checkpoint"
    checkpoint_root.mkdir()
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'f' * 64}", "checkpointed", None)],
    )

    with embedding_server() as url:
        thread, started = _start_checkpoint_ack(checkpoint_root)
        assert started.wait(timeout=5)
        ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            allow_loopback=True,
            checkpoint_root=checkpoint_root,
            # Exercise the real handshake without making scheduler latency the assertion.
            checkpoint_timeout_seconds=5,
        )
        thread.join(timeout=5)
        assert not thread.is_alive()

    assert output_path.exists()
    assert list(checkpoint_root.iterdir()) == []


def test_checkpoint_disappearance_fails_without_final_output(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    checkpoint_root = tmp_path / "checkpoint"
    checkpoint_root.mkdir()
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'1' * 64}", "lost", None)],
    )

    with embedding_server() as url:
        thread, started = _start_checkpoint_ack(checkpoint_root, delete_ready=True)
        assert started.wait(timeout=5)
        with pytest.raises(ingestion.IngestionError, match="disappeared"):
            ingestion.embed_plan(
                plan_path,
                output_path,
                url=url,
                allow_loopback=True,
                checkpoint_root=checkpoint_root,
                checkpoint_timeout_seconds=5,
            )
        thread.join(timeout=5)
        assert not thread.is_alive()

    assert not output_path.exists()


def test_checkpoint_ack_timeout_fails_without_final_output(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    checkpoint_root = tmp_path / "checkpoint"
    checkpoint_root.mkdir()
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'2' * 64}", "timeout", None)],
    )

    with embedding_server() as url, pytest.raises(ingestion.IngestionError, match="timed out"):
        ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            allow_loopback=True,
            checkpoint_root=checkpoint_root,
            checkpoint_timeout_seconds=0.01,
        )

    assert not output_path.exists()


def test_acknowledged_checkpoint_resumes_without_model_calls(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    checkpoint_root = tmp_path / "checkpoint"
    checkpoint_root.mkdir()
    content = "resume"
    vector = [0.0] * ingestion.EMBEDDING_DIMENSION
    vector[4] = 1.0
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'3' * 64}", content, None)],
    )
    ingestion.write_jsonl(
        checkpoint_root / ingestion.CHECKPOINT_ACKED_FILENAME,
        [{"profile": ingestion.EMBEDDING_PROFILE, "content": content, "embedding": vector}],
        max_bytes=ingestion.MAX_CHECKPOINT_BYTES,
    )

    with embedding_server() as url:
        ingestion.embed_plan(
            plan_path,
            output_path,
            url=url,
            allow_loopback=True,
            checkpoint_root=checkpoint_root,
            checkpoint_timeout_seconds=1,
        )

    assert EmbeddingHandler.tokenize_calls == []
    assert EmbeddingHandler.calls == []
    records = ingestion._read_jsonl(output_path, with_embedding=True)
    assert records[0]["embedding"] == vector
    assert list(checkpoint_root.iterdir()) == []


def test_embedding_vector_rejects_float32_underflow() -> None:
    values = [0.0] * ingestion.EMBEDDING_DIMENSION
    values[0] = 1e-46

    with pytest.raises(ingestion.IngestionError, match="non-zero"):
        ingestion._embedding_vector(values, name="embedding")


@pytest.mark.parametrize("value", [True, float("nan"), float("inf"), 1e100])
def test_embedding_vector_rejects_non_float32_values(value: object) -> None:
    values: list[object] = [0.0] * ingestion.EMBEDDING_DIMENSION
    values[0] = value

    with pytest.raises(ingestion.IngestionError, match=r"finite number|finite|float32"):
        ingestion._embedding_vector(values, name="embedding")


@pytest.mark.parametrize(
    "value",
    [
        "Bearer workload-key",
        "Bearer abc.DEF_123~+/",
        "Bearer abc+/_~.-0123===",
    ],
    ids=["opaque", "complete-alphabet", "terminal-padding"],
)
def test_workload_authorization_accepts_rfc6750_b64token(tmp_path: Path, value: str) -> None:
    credential = tmp_path / "authorization"
    credential.write_text(value, encoding="ascii")

    assert ingestion.read_authorization(credential) == value


@pytest.mark.parametrize(
    "value",
    [
        b"Bearer ",
        b"bearer token",
        b"Bearer token\n",
        b"Bearer tok en",
        b"Bearer token\x00",
        b"Bearer token\x7f",
        b'Bearer token"',
        b"Bearer token,",
        b"Bearer =token",
        b"Bearer token=tail",
    ],
    ids=[
        "empty",
        "wrong-scheme",
        "newline",
        "embedded-space",
        "null",
        "delete-control",
        "quote",
        "comma",
        "leading-padding",
        "non-terminal-padding",
    ],
)
def test_workload_authorization_rejects_non_b64token_without_reflection(tmp_path: Path, value: bytes) -> None:
    credential = tmp_path / "authorization"
    credential.write_bytes(value)

    with pytest.raises(ingestion.IngestionError) as caught:
        ingestion.read_authorization(credential)

    assert str(caught.value) == "workload authorization must be one exact Bearer credential"


@pytest.mark.parametrize(
    ("value", "message"),
    [
        (b"Bearer caf\xc3\xa9", "must be ASCII"),
        (b"Bearer " + b"a" * ingestion.MAX_AUTHORIZATION_BYTES, "must contain between"),
    ],
    ids=["non-ascii", "oversized"],
)
def test_workload_authorization_preserves_ascii_and_size_bounds(
    tmp_path: Path,
    value: bytes,
    message: str,
) -> None:
    credential = tmp_path / "authorization"
    credential.write_bytes(value)

    with pytest.raises(ingestion.IngestionError, match=message) as caught:
        ingestion.read_authorization(credential)

    assert "caf" not in str(caught.value)
    assert "a" * 64 not in str(caught.value)


def test_embed_cli_rejects_invalid_authorization_before_request_path(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
) -> None:
    credential = tmp_path / "authorization"
    credential.write_bytes(b"Bearer private,credential")
    called = False

    def unexpected_embed(*_args: object, **_kwargs: object) -> tuple[int, int]:
        nonlocal called
        called = True
        raise AssertionError("embed path reached for an invalid workload credential")

    monkeypatch.setattr(ingestion, "embed_plan", unexpected_embed)

    assert (
        ingestion.main(
            [
                "embed",
                "--plan",
                str(tmp_path / "plan.jsonl"),
                "--output",
                str(tmp_path / "output.jsonl"),
                "--authorization-file",
                str(credential),
                "--checkpoint-root",
                str(tmp_path / "checkpoint"),
            ]
        )
        == 2
    )
    assert not called
    assert "private,credential" not in caplog.text


def test_production_embedding_call_requires_workload_credential(tmp_path: Path) -> None:
    plan_path = tmp_path / "plan.jsonl"
    output_path = tmp_path / "chunks.jsonl"
    ingestion.write_jsonl(
        plan_path,
        [planned_record(f"sha256:{'9' * 64}", "changed", None)],
    )

    with pytest.raises(ingestion.IngestionError, match="workload credential"):
        ingestion.embed_plan(plan_path, output_path)


def test_production_embedding_url_is_exact() -> None:
    ingestion._validate_embeddings_url(ingestion.EMBEDDINGS_URL, allow_loopback=False)
    with pytest.raises(ingestion.IngestionError, match="exact agentgateway"):
        ingestion._validate_embeddings_url(
            "http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8080/v1/embeddings",
            allow_loopback=False,
        )
