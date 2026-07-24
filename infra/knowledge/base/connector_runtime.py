"""Materialize one claimed connector source into the isolated ingestion contract."""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import re
import shutil
import stat
import tempfile
import unicodedata
from collections.abc import Mapping, Sequence
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import cast

ACTION_FILENAME = "connector-action.json"
CURRENT_FILENAME = "current.json"
INVENTORY_FILENAME = "inventory.json"
MANIFEST_FILENAME = "manifest.json"
LOGGER = logging.getLogger("fgentic.knowledge_connector_runtime")

MAX_ACTION_BYTES = 64 * 1024
MAX_INVENTORY_BYTES = 32 * 1024 * 1024
MAX_SOURCE_BYTES = 16 * 1024 * 1024
MAX_SOURCES = 512
MAX_RETAINED_ARTIFACTS_PER_REVISION = 64
MAX_RETAINED_ENTRIES_PER_REVISION = 128

CLASSIFICATIONS = frozenset(
    {
        "public",
        "approved_non_public",
        "restricted",
        "regulated",
        "secret",
        "authentication",
    }
)
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
DNS_LABEL_RE = re.compile(r"^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")


class MaterializationError(ValueError):
    """A claimed action, retained source, or output boundary failed closed."""


@dataclass(frozen=True)
class ConnectorAction:
    """One exact present action claimed from the knowledge database."""

    connector_id: str
    source_id: str
    source_path: str
    source_revision: str
    content_digest: str
    acl_digest: str
    metadata: dict[str, object]
    snapshot_revision: str
    inventory_digest: str
    claim_expires_at: str


@dataclass(frozen=True)
class InventorySource:
    """One immutable source row in acquisition inventory evidence."""

    connector_id: str
    snapshot_revision: str
    inventory_digest: str
    source_id: str
    source_path: str
    source_revision: str
    content_digest: str
    acl_digest: str
    metadata: dict[str, object]

    def digest_item(self) -> dict[str, object]:
        return {
            "source_id": self.source_id,
            "source_path": self.source_path,
            "source_revision": self.source_revision,
            "content_digest": self.content_digest,
            "acl_digest": self.acl_digest,
            "metadata": self.metadata,
        }


@dataclass(frozen=True)
class Inventory:
    """One fully validated acquisition inventory."""

    connector_id: str
    snapshot_revision: str
    inventory_digest: str
    artifact_digest: str
    sources: tuple[InventorySource, ...]


@dataclass(frozen=True)
class MaterializedSource:
    """The published one-source ingestion bundle."""

    output_root: Path
    manifest_path: Path
    source_path: Path
    source_id: str
    inventory_digest: str


def parse_connector_action(path: Path) -> ConnectorAction:
    """Read and validate the exact database-claimed action document."""
    raw = _read_regular_path(path, max_bytes=MAX_ACTION_BYTES, allow_empty=True)
    if not raw.strip():
        raise MaterializationError("no connector action was claimed")
    document = _decode_json(raw, name="connector action")
    action = _expect_object(
        document,
        name="connector action",
        required=frozenset(
            {
                "connector_id",
                "source_id",
                "source_path",
                "action",
                "source_revision",
                "content_digest",
                "acl_digest",
                "metadata",
                "snapshot_revision",
                "inventory_digest",
                "claim_expires_at",
            }
        ),
        allowed=frozenset(
            {
                "connector_id",
                "source_id",
                "source_path",
                "action",
                "source_revision",
                "content_digest",
                "acl_digest",
                "metadata",
                "snapshot_revision",
                "inventory_digest",
                "claim_expires_at",
            }
        ),
    )
    action_kind = _clean_text(action["action"], name="connector action.action", max_bytes=16)
    if action_kind != "present":
        raise MaterializationError(f"connector action {action_kind!r} cannot be materialized as a source")

    connector_id = _dns_label(action["connector_id"], name="connector action.connector_id")
    source_path = _source_path(action["source_path"], name="connector action.source_path")
    source_id, corpus = _source_identity(
        action["source_id"],
        connector_id=connector_id,
        source_path=source_path,
        name="connector action.source_id",
    )
    source_revision = _clean_text(
        action["source_revision"],
        name="connector action.source_revision",
        max_bytes=255,
    )
    snapshot_revision = _clean_text(
        action["snapshot_revision"],
        name="connector action.snapshot_revision",
        max_bytes=255,
    )
    content_digest = _validated_digest(
        action["content_digest"],
        name="connector action.content_digest",
    )
    acl_digest = _validated_digest(action["acl_digest"], name="connector action.acl_digest")
    inventory_digest = _validated_digest(
        action["inventory_digest"],
        name="connector action.inventory_digest",
    )
    metadata = _metadata(
        action["metadata"],
        source_id=source_id,
        revision=source_revision,
        name="connector action.metadata",
    )
    if _acl_digest(metadata) != acl_digest:
        raise MaterializationError("connector action ACL digest does not match its metadata")
    claim_expires_at = _clean_text(
        action["claim_expires_at"],
        name="connector action.claim_expires_at",
        max_bytes=64,
    )
    if not corpus:
        raise AssertionError("validated source identity returned an empty corpus")
    return ConnectorAction(
        connector_id=connector_id,
        source_id=source_id,
        source_path=source_path,
        source_revision=source_revision,
        content_digest=content_digest,
        acl_digest=acl_digest,
        metadata=metadata,
        snapshot_revision=snapshot_revision,
        inventory_digest=inventory_digest,
        claim_expires_at=claim_expires_at,
    )


def is_connector_source(work_root: Path) -> bool:
    """Return whether a non-empty present connector action exists below ``work_root``."""
    action_path = work_root / ACTION_FILENAME
    if not os.path.lexists(action_path):
        return False
    try:
        parse_connector_action(action_path)
    except MaterializationError as error:
        if str(error) == "no connector action was claimed":
            return False
        raise
    return True


def materialize_connector_source(
    *,
    source_root: Path,
    action_path: Path,
    output_root: Path,
) -> MaterializedSource:
    """Publish one #332-compatible bundle from an exact claim and immutable blob."""
    action = parse_connector_action(action_path)
    _validate_optional_inventory_evidence(source_root, action)

    blob_hex = _digest_hex(action.content_digest)
    content = _read_below_root(
        source_root,
        ("blobs", blob_hex),
        max_bytes=MAX_SOURCE_BYTES,
        optional=False,
    )
    if content is None:
        raise AssertionError("required blob reader returned no content")
    if _digest(content) != action.content_digest:
        raise MaterializationError("connector source blob digest does not match the claimed action")

    corpus = action.source_id.split("/", 1)[0]
    source_metadata = _expect_object(
        action.metadata["source"],
        name="connector action.metadata.source",
        required=frozenset({"id", "locator", "revision"}),
        allowed=frozenset({"id", "title", "locator", "revision"}),
    )
    manifest = {
        "schema_version": 1,
        "corpus": corpus,
        "sources": [
            {
                "path": action.source_path,
                "digest": action.content_digest,
                "source": source_metadata,
                "classification": action.metadata["classification"],
                "allowed_principals": action.metadata["allowed_principals"],
                "allowed_groups": action.metadata["allowed_groups"],
            }
        ],
    }
    manifest_raw = _canonical_json(manifest) + b"\n"
    return _publish_bundle(
        output_root=output_root,
        source_path=action.source_path,
        content=content,
        manifest_raw=manifest_raw,
        source_id=action.source_id,
        inventory_digest=action.inventory_digest,
    )


def parse_inventory(path: Path) -> Inventory:
    """Read and validate one complete acquisition inventory document."""
    return _parse_inventory_bytes(
        _read_regular_path(path, max_bytes=MAX_INVENTORY_BYTES),
        label=f"inventory {path}",
    )


def _inventory_source(
    value: object,
    *,
    index: int,
    connector_id: str,
    snapshot_revision: str,
    inventory_digest: str,
) -> InventorySource:
    name = f"inventory.sources[{index}]"
    source = _expect_object(
        value,
        name=name,
        required=frozenset(
            {
                "connector_id",
                "snapshot_revision",
                "inventory_digest",
                "source_id",
                "source_path",
                "source_revision",
                "content_digest",
                "acl_digest",
                "metadata",
            }
        ),
        allowed=frozenset(
            {
                "connector_id",
                "snapshot_revision",
                "inventory_digest",
                "source_id",
                "source_path",
                "source_revision",
                "content_digest",
                "acl_digest",
                "metadata",
            }
        ),
    )
    if source["connector_id"] != connector_id:
        raise MaterializationError(f"{name}.connector_id does not match its inventory")
    if source["snapshot_revision"] != snapshot_revision:
        raise MaterializationError(f"{name}.snapshot_revision does not match its inventory")
    if source["inventory_digest"] != inventory_digest:
        raise MaterializationError(f"{name}.inventory_digest does not match its inventory")
    source_path = _source_path(source["source_path"], name=f"{name}.source_path")
    source_id, _ = _source_identity(
        source["source_id"],
        connector_id=connector_id,
        source_path=source_path,
        name=f"{name}.source_id",
    )
    source_revision = _clean_text(source["source_revision"], name=f"{name}.source_revision", max_bytes=255)
    content_digest = _validated_digest(source["content_digest"], name=f"{name}.content_digest")
    acl_digest = _validated_digest(source["acl_digest"], name=f"{name}.acl_digest")
    metadata = _metadata(
        source["metadata"],
        source_id=source_id,
        revision=source_revision,
        name=f"{name}.metadata",
    )
    if _acl_digest(metadata) != acl_digest:
        raise MaterializationError(f"{name}.acl_digest does not match its metadata")
    return InventorySource(
        connector_id=connector_id,
        snapshot_revision=snapshot_revision,
        inventory_digest=inventory_digest,
        source_id=source_id,
        source_path=source_path,
        source_revision=source_revision,
        content_digest=content_digest,
        acl_digest=acl_digest,
        metadata=metadata,
    )


def _validate_optional_inventory_evidence(source_root: Path, action: ConnectorAction) -> None:
    current_raw = _read_below_root(
        source_root,
        (CURRENT_FILENAME,),
        max_bytes=MAX_INVENTORY_BYTES,
        optional=True,
    )
    current: Inventory | None = None
    if current_raw is not None:
        current = _parse_inventory_bytes(current_raw, label="current inventory")
        if current.connector_id != action.connector_id:
            raise MaterializationError("current inventory belongs to a different connector")

    retained = _retained_inventory_evidence(source_root, action)

    if (
        current is not None
        and current.inventory_digest == action.inventory_digest
        and current.snapshot_revision == action.snapshot_revision
    ):
        _require_inventory_action(current, action)
        if retained is not None and (
            current.connector_id != retained.connector_id
            or current.snapshot_revision != retained.snapshot_revision
            or current.inventory_digest != retained.inventory_digest
            or current.sources != retained.sources
        ):
            raise MaterializationError("current and retained inventory evidence disagree")


def _retained_inventory_evidence(source_root: Path, action: ConnectorAction) -> Inventory | None:
    inventory_hex = _digest_hex(action.inventory_digest)
    revision_hex = hashlib.sha256(action.snapshot_revision.encode()).hexdigest()
    revision_root = _optional_directory_below(
        source_root,
        ("snapshots", inventory_hex, revision_hex),
    )
    if revision_root is None:
        return None

    try:
        entries: list[tuple[str, bool]] = []
        with os.scandir(revision_root) as iterator:
            for entry in iterator:
                if len(entries) >= MAX_RETAINED_ENTRIES_PER_REVISION:
                    raise MaterializationError("retained inventory directory entry count is invalid")
                entries.append((entry.name, entry.is_dir(follow_symlinks=False)))
    except OSError as error:
        raise MaterializationError(f"could not enumerate retained inventory evidence: {error}") from error
    artifact_entries = sorted(
        (entry for entry in entries if not entry[0].startswith(".pending-")),
        key=lambda entry: entry[0],
    )
    if not 1 <= len(artifact_entries) <= MAX_RETAINED_ARTIFACTS_PER_REVISION:
        raise MaterializationError("retained inventory artifact evidence count is invalid")

    retained: Inventory | None = None
    for entry_name, entry_is_directory in artifact_entries:
        if re.fullmatch(r"[0-9a-f]{64}", entry_name) is None or not entry_is_directory:
            raise MaterializationError("retained inventory artifact path is not one digest directory")
        retained_raw = _read_below_root(
            source_root,
            ("snapshots", inventory_hex, revision_hex, entry_name, INVENTORY_FILENAME),
            max_bytes=MAX_INVENTORY_BYTES,
            optional=False,
        )
        if retained_raw is None:  # pragma: no cover - optional=False is an explicit invariant.
            raise MaterializationError("retained inventory evidence disappeared")
        candidate = _parse_inventory_bytes(retained_raw, label="retained inventory")
        if (
            candidate.inventory_digest != action.inventory_digest
            or _digest_hex(candidate.artifact_digest) != entry_name
        ):
            raise MaterializationError("retained inventory path does not match its evidence digests")
        _require_inventory_action(candidate, action)
        if retained is not None and (
            candidate.connector_id != retained.connector_id
            or candidate.snapshot_revision != retained.snapshot_revision
            or candidate.inventory_digest != retained.inventory_digest
            or candidate.sources != retained.sources
        ):
            raise MaterializationError("retained inventory envelopes disagree on source state")
        retained = candidate
    return retained


def _parse_inventory_bytes(raw: bytes, *, label: str) -> Inventory:
    document = _decode_json(raw, name=label)
    # Reuse the strict path parser without weakening regular-file checks through a temporary file.
    root = _expect_object(
        document,
        name=label,
        required=frozenset(
            {
                "connector_id",
                "snapshot_revision",
                "inventory_digest",
                "artifact_digest",
                "source_count",
                "sources",
            }
        ),
        allowed=frozenset(
            {
                "connector_id",
                "snapshot_revision",
                "inventory_digest",
                "artifact_digest",
                "source_count",
                "sources",
            }
        ),
    )
    return _inventory_from_object(root, label=label)


def _inventory_from_object(root: Mapping[str, object], *, label: str) -> Inventory:
    connector_id = _dns_label(root["connector_id"], name=f"{label}.connector_id")
    snapshot_revision = _clean_text(root["snapshot_revision"], name=f"{label}.snapshot_revision", max_bytes=255)
    inventory_digest = _validated_digest(root["inventory_digest"], name=f"{label}.inventory_digest")
    artifact_digest = _validated_digest(root["artifact_digest"], name=f"{label}.artifact_digest")
    raw_sources = _expect_list(root["sources"], name=f"{label}.sources", maximum=MAX_SOURCES)
    source_count = root["source_count"]
    if isinstance(source_count, bool) or not isinstance(source_count, int) or source_count != len(raw_sources):
        raise MaterializationError(f"{label}.source_count must equal the exact sources array length")
    sources = tuple(
        _inventory_source(
            value,
            index=index,
            connector_id=connector_id,
            snapshot_revision=snapshot_revision,
            inventory_digest=inventory_digest,
        )
        for index, value in enumerate(raw_sources)
    )
    source_ids = [source.source_id for source in sources]
    source_paths = [source.source_path for source in sources]
    if source_ids != sorted(source_ids) or len(source_ids) != len(set(source_ids)):
        raise MaterializationError(f"{label}.sources must have unique source IDs in lexicographic order")
    if len(source_paths) != len(set(source_paths)) or len(source_paths) != len(
        {path.casefold() for path in source_paths}
    ):
        raise MaterializationError(f"{label}.sources contains duplicate or case-colliding paths")
    if _digest(_canonical_json([source.digest_item() for source in sources])) != inventory_digest:
        raise MaterializationError(f"{label} canonical source digest does not match inventory_digest")
    return Inventory(
        connector_id=connector_id,
        snapshot_revision=snapshot_revision,
        inventory_digest=inventory_digest,
        artifact_digest=artifact_digest,
        sources=sources,
    )


def _require_inventory_action(inventory: Inventory, action: ConnectorAction) -> None:
    if inventory.connector_id != action.connector_id:
        raise MaterializationError("inventory evidence belongs to a different connector")
    if inventory.snapshot_revision != action.snapshot_revision:
        raise MaterializationError("inventory evidence revision does not match the claimed action")
    matching = [source for source in inventory.sources if source.source_id == action.source_id]
    if len(matching) != 1:
        raise MaterializationError("inventory evidence does not contain the exact claimed source")
    source = matching[0]
    if (
        source.source_path != action.source_path
        or source.source_revision != action.source_revision
        or source.content_digest != action.content_digest
        or source.acl_digest != action.acl_digest
        or source.metadata != action.metadata
    ):
        raise MaterializationError("inventory evidence does not match the exact claimed source fields")


def _metadata(value: object, *, source_id: str, revision: str, name: str) -> dict[str, object]:
    metadata = _expect_object(
        value,
        name=name,
        required=frozenset({"source", "classification", "allowed_principals", "allowed_groups"}),
        allowed=frozenset({"source", "classification", "allowed_principals", "allowed_groups"}),
    )
    source = _expect_object(
        metadata["source"],
        name=f"{name}.source",
        required=frozenset({"id", "locator", "revision"}),
        allowed=frozenset({"id", "title", "locator", "revision"}),
    )
    if source.get("id") != source_id or source.get("revision") != revision:
        raise MaterializationError(f"{name}.source identity or revision does not match its row")
    _clean_text(source["locator"], name=f"{name}.source.locator", max_bytes=2048)
    if "title" in source:
        _clean_text(source["title"], name=f"{name}.source.title", max_bytes=512)
    classification = _clean_text(metadata["classification"], name=f"{name}.classification", max_bytes=64)
    if classification not in CLASSIFICATIONS:
        raise MaterializationError(f"{name}.classification is unknown")
    principals = _expect_list(metadata["allowed_principals"], name=f"{name}.allowed_principals", maximum=64)
    groups = _expect_list(metadata["allowed_groups"], name=f"{name}.allowed_groups", maximum=64)
    if not principals and not groups:
        raise MaterializationError(f"{name} must admit at least one principal or group")
    for index, principal_value in enumerate(principals):
        principal_name = f"{name}.allowed_principals[{index}]"
        principal = _expect_object(
            principal_value,
            name=principal_name,
            required=frozenset({"kind", "principal"}),
            allowed=frozenset({"kind", "network", "principal"}),
        )
        kind = _clean_text(principal["kind"], name=f"{principal_name}.kind", max_bytes=32)
        _clean_text(principal["principal"], name=f"{principal_name}.principal", max_bytes=255)
        if kind == "matrix" and "network" in principal:
            raise MaterializationError(f"{principal_name}.network is forbidden for Matrix principals")
        if kind == "bridged_matrix":
            if "network" not in principal:
                raise MaterializationError(f"{principal_name}.network is required for bridged principals")
            _dns_label(principal["network"], name=f"{principal_name}.network")
        elif kind != "matrix":
            raise MaterializationError(f"{principal_name}.kind is unknown")
    for index, group in enumerate(groups):
        _clean_text(group, name=f"{name}.allowed_groups[{index}]", max_bytes=191)
    return metadata


def _acl_digest(metadata: Mapping[str, object]) -> str:
    return _digest(
        _canonical_json(
            {
                "classification": metadata["classification"],
                "allowed_principals": metadata["allowed_principals"],
                "allowed_groups": metadata["allowed_groups"],
            }
        )
    )


def _publish_bundle(
    *,
    output_root: Path,
    source_path: str,
    content: bytes,
    manifest_raw: bytes,
    source_id: str,
    inventory_digest: str,
) -> MaterializedSource:
    if os.path.lexists(output_root):
        raise MaterializationError(f"refusing to overwrite materialized output root: {output_root}")
    parent = output_root.parent
    _require_plain_directory(parent, name="materialized output parent")
    staging = Path(tempfile.mkdtemp(prefix=f".{output_root.name}.staging-", dir=parent))
    try:
        staged_source = staging.joinpath(*PurePosixPath(source_path).parts)
        staged_source.parent.mkdir(parents=True, mode=0o700)
        _write_new_file(staged_source, content)
        _write_new_file(staging / MANIFEST_FILENAME, manifest_raw)
        _sync_directory(staging)
        staging.rename(output_root)
        _sync_directory(parent)
    except Exception:
        shutil.rmtree(staging, ignore_errors=True)
        raise
    return MaterializedSource(
        output_root=output_root,
        manifest_path=output_root / MANIFEST_FILENAME,
        source_path=output_root.joinpath(*PurePosixPath(source_path).parts),
        source_id=source_id,
        inventory_digest=inventory_digest,
    )


def _write_new_file(path: Path, content: bytes) -> None:
    try:
        with path.open("xb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
    except OSError as error:
        raise MaterializationError(f"could not write materialized file {path}: {error}") from error


def _sync_directory(path: Path) -> None:
    try:
        descriptor = os.open(path, os.O_RDONLY | getattr(os, "O_DIRECTORY", 0))
    except OSError as error:
        raise MaterializationError(f"could not open materialization directory {path}: {error}") from error
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _read_below_root(
    root: Path,
    parts: Sequence[str],
    *,
    max_bytes: int,
    optional: bool,
) -> bytes | None:
    _require_plain_directory(root, name="connector source root")
    current = root
    for index, part in enumerate(parts):
        if not part or part in {".", ".."} or "/" in part or "\\" in part:
            raise MaterializationError("connector source path contains an unsafe component")
        current /= part
        try:
            status = os.lstat(current)
        except FileNotFoundError:
            if optional:
                return None
            raise MaterializationError(f"required connector source path is missing: {current}") from None
        except OSError as error:
            raise MaterializationError(f"could not inspect connector source path {current}: {error}") from error
        is_last = index == len(parts) - 1
        if is_last:
            if not stat.S_ISREG(status.st_mode):
                raise MaterializationError(f"connector source path must be a regular file: {current}")
        elif not stat.S_ISDIR(status.st_mode):
            raise MaterializationError(f"connector source path component must be a directory: {current}")
    return _read_regular_path(current, max_bytes=max_bytes)


def _optional_directory_below(root: Path, parts: Sequence[str]) -> Path | None:
    _require_plain_directory(root, name="connector source root")
    current = root
    for part in parts:
        if not part or part in {".", ".."} or "/" in part or "\\" in part:
            raise MaterializationError("connector source directory contains an unsafe component")
        current /= part
        try:
            status = os.lstat(current)
        except FileNotFoundError:
            return None
        except OSError as error:
            raise MaterializationError(f"could not inspect connector source directory {current}: {error}") from error
        if not stat.S_ISDIR(status.st_mode):
            raise MaterializationError(
                f"connector source directory must be a real directory, never a symlink: {current}"
            )
    return current


def _read_regular_path(path: Path, *, max_bytes: int, allow_empty: bool = False) -> bytes:
    try:
        before_path = os.lstat(path)
    except OSError as error:
        raise MaterializationError(f"could not inspect required regular file {path}: {error}") from error
    if not stat.S_ISREG(before_path.st_mode):
        raise MaterializationError(f"required path must be a regular file, never a symlink or special file: {path}")
    flags = os.O_RDONLY | getattr(os, "O_CLOEXEC", 0) | getattr(os, "O_NOFOLLOW", 0) | getattr(os, "O_NONBLOCK", 0)
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise MaterializationError(f"could not open required regular file {path}: {error}") from error
    try:
        before = os.fstat(descriptor)
        if not stat.S_ISREG(before.st_mode):
            raise MaterializationError(f"required path must be a regular file, never a symlink or special file: {path}")
        chunks: list[bytes] = []
        remaining = max_bytes + 1
        while remaining > 0:
            chunk = os.read(descriptor, min(64 * 1024, remaining))
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        after = os.fstat(descriptor)
    finally:
        os.close(descriptor)
    try:
        after_path = os.lstat(path)
    except OSError as error:
        raise MaterializationError(f"regular file disappeared while it was read: {path}: {error}") from error
    raw = b"".join(chunks)
    if (not allow_empty and not raw) or len(raw) > max_bytes:
        lower = 0 if allow_empty else 1
        raise MaterializationError(f"{path} must contain between {lower} and {max_bytes} bytes")
    if (
        before_path.st_dev != before.st_dev
        or before_path.st_ino != before.st_ino
        or before_path.st_dev != after_path.st_dev
        or before_path.st_ino != after_path.st_ino
        or before_path.st_size != after_path.st_size
        or before_path.st_mtime_ns != after_path.st_mtime_ns
        or before.st_dev != after.st_dev
        or before.st_ino != after.st_ino
        or before.st_size != after.st_size
        or before.st_mtime_ns != after.st_mtime_ns
        or after.st_size != len(raw)
    ):
        raise MaterializationError(f"regular file changed while it was read: {path}")
    return raw


def _require_plain_directory(path: Path, *, name: str) -> None:
    try:
        status = os.lstat(path)
    except OSError as error:
        raise MaterializationError(f"could not inspect {name} {path}: {error}") from error
    if not stat.S_ISDIR(status.st_mode):
        raise MaterializationError(f"{name} must be a real directory, never a symlink: {path}")


def _source_path(value: object, *, name: str) -> str:
    raw = _clean_text(value, name=name, max_bytes=512)
    if "\\" in raw:
        raise MaterializationError(f"{name} must use POSIX separators")
    path = PurePosixPath(raw)
    if (
        path.is_absolute()
        or raw != path.as_posix()
        or any(part in {"", ".", ".."} for part in path.parts)
        or len(path.parts) < 2
        or path.parts[0] != "docs"
        or path.suffix != ".md"
    ):
        raise MaterializationError(f"{name} must be a canonical contained docs/**/*.md path")
    return raw


def _source_identity(
    value: object,
    *,
    connector_id: str,
    source_path: str,
    name: str,
) -> tuple[str, str]:
    source_id = _clean_text(value, name=name, max_bytes=512)
    corpus, separator, remainder = source_id.partition("/")
    _dns_label(corpus, name=f"{name} corpus")
    expected = f"{corpus}/{connector_id}/{source_path}"
    if not separator or not remainder or source_id != expected:
        raise MaterializationError(f"{name} must equal corpus/connector/source_path")
    return source_id, corpus


def _validated_digest(value: object, *, name: str) -> str:
    digest = _clean_text(value, name=name, max_bytes=71)
    if DIGEST_RE.fullmatch(digest) is None:
        raise MaterializationError(f"{name} must be sha256 followed by 64 lowercase hexadecimal characters")
    return digest


def _digest_hex(value: str) -> str:
    return _validated_digest(value, name="digest").removeprefix("sha256:")


def _dns_label(value: object, *, name: str) -> str:
    label = _clean_text(value, name=name, max_bytes=63)
    if DNS_LABEL_RE.fullmatch(label) is None:
        raise MaterializationError(f"{name} must be a DNS-1123 label")
    return label


def _clean_text(value: object, *, name: str, max_bytes: int) -> str:
    if not isinstance(value, str):
        raise MaterializationError(f"{name} must be a string")
    try:
        normalized = unicodedata.normalize("NFC", value)
        encoded = normalized.encode()
    except UnicodeError:
        raise MaterializationError(f"{name} must be valid Unicode text") from None
    if normalized != value:
        raise MaterializationError(f"{name} must already use NFC normalization")
    if not normalized or normalized != normalized.strip():
        raise MaterializationError(f"{name} must be non-empty with no surrounding whitespace")
    if len(encoded) > max_bytes:
        raise MaterializationError(f"{name} exceeds {max_bytes} UTF-8 bytes")
    if any(unicodedata.category(character).startswith("C") for character in normalized):
        raise MaterializationError(f"{name} contains a control or format character")
    return normalized


def _expect_object(
    value: object,
    *,
    name: str,
    required: frozenset[str],
    allowed: frozenset[str],
) -> dict[str, object]:
    if not isinstance(value, dict):
        raise MaterializationError(f"{name} must be an object")
    keys = frozenset(str(key) for key in value)
    missing = required - keys
    unknown = keys - allowed
    if missing:
        raise MaterializationError(f"{name} is missing required fields: {', '.join(sorted(missing))}")
    if unknown:
        raise MaterializationError(f"{name} has unknown fields")
    return cast(dict[str, object], value)


def _expect_list(value: object, *, name: str, maximum: int) -> list[object]:
    if not isinstance(value, list):
        raise MaterializationError(f"{name} must be an array")
    if len(value) > maximum:
        raise MaterializationError(f"{name} exceeds the maximum of {maximum} entries")
    return cast(list[object], value)


def _decode_json(raw: bytes, *, name: str) -> object:
    try:
        return json.loads(raw.decode("utf-8"), object_pairs_hook=_strict_object, parse_constant=_reject_constant)
    except (RecursionError, UnicodeDecodeError, ValueError, json.JSONDecodeError) as error:
        raise MaterializationError(f"{name} must be strict UTF-8 JSON: {error}") from error


def _strict_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise MaterializationError("JSON object contains duplicate key")
        result[key] = value
    return result


def _reject_constant(value: str) -> object:
    raise MaterializationError(f"JSON constant is not permitted: {value}")


def _canonical_json(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def _digest(content: bytes) -> str:
    return f"sha256:{hashlib.sha256(content).hexdigest()}"


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description="Run the connector-to-ingestion handoff")
    subparsers = parser.add_subparsers(dest="command", required=True)
    materialize = subparsers.add_parser("materialize", help="materialize one claimed present action")
    materialize.add_argument("--source-root", type=Path, required=True)
    materialize.add_argument("--action", type=Path, required=True)
    materialize.add_argument("--output-root", type=Path, required=True)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    """Run the bounded materialization helper."""
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s %(message)s")
    arguments = _parser().parse_args(argv)
    if arguments.command != "materialize":
        raise AssertionError(f"unhandled connector runtime command: {arguments.command}")
    try:
        materialize_connector_source(
            source_root=arguments.source_root,
            action_path=arguments.action,
            output_root=arguments.output_root,
        )
    except MaterializationError as error:
        LOGGER.error("materialization denied: %s", error)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
