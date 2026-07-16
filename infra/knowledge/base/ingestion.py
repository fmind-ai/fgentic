"""Fail-closed document ingestion for the sovereign knowledge store."""

from __future__ import annotations

import argparse
import hashlib
import http.client
import importlib
import ipaddress
import json
import logging
import math
import os
import re
import socket
import stat
import struct
import tempfile
import threading
import time
import unicodedata
import zipfile
from collections.abc import Iterable, Sequence
from contextlib import suppress
from dataclasses import dataclass
from pathlib import Path, PurePosixPath
from typing import Any, Protocol, cast, override
from urllib.parse import urlsplit

LOGGER = logging.getLogger("fgentic.knowledge_ingestion")

SCHEMA_VERSION = 1
EMBEDDING_DIMENSION = 1024
EMBEDDING_MODEL = "BAAI/bge-m3"
EMBEDDING_PROFILE = "bge-m3-1024-v1"
CHUNK_ID_DOMAIN = "fgentic.chunk.v1"
EMBEDDINGS_URL = "http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8082/v1/embeddings"
TOKENIZE_PATH = "/tokenize"
ISOLATED_SOURCE_BASENAME = "document"
RAW_RECORD_FILENAME = "chunks.jsonl"
CHECKPOINT_READY_FILENAME = "checkpoint.ready"
CHECKPOINT_ACKED_FILENAME = "checkpoint.acked"

MAX_MANIFEST_BYTES = 256 * 1024
MAX_SOURCES = 1
MAX_SOURCE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_SOURCE_BYTES = MAX_SOURCE_BYTES
MAX_PAGES = 256
MAX_ARCHIVE_ENTRIES = 4096
MAX_ARCHIVE_UNCOMPRESSED_BYTES = 64 * 1024 * 1024
MAX_ARCHIVE_COMPRESSION_RATIO = 100
MAX_CHUNKS_PER_SOURCE = 512
MAX_TOTAL_CHUNKS = MAX_CHUNKS_PER_SOURCE
MAX_CHUNK_BYTES = 64 * 1024
MAX_JSONL_BYTES = 64 * 1024 * 1024
MAX_JSONL_LINE_BYTES = 256 * 1024
MAX_FLOAT32_JSON_BYTES = 32
MAX_EMBEDDED_JSONL_BYTES = MAX_JSONL_BYTES + MAX_TOTAL_CHUNKS * (EMBEDDING_DIMENSION * MAX_FLOAT32_JSON_BYTES + 64)
MAX_EMBEDDING_REQUEST_BYTES = 128 * 1024
MAX_EMBEDDING_RESPONSE_BYTES = 2 * 1024 * 1024
MAX_EMBEDDING_BATCH = 8
MAX_MODEL_TOKENS = 8192
MAX_TOKENIZE_RESPONSE_BYTES = 256 * 1024
MAX_CHECKPOINT_BYTES = MAX_EMBEDDING_REQUEST_BYTES + MAX_EMBEDDING_BATCH * (
    EMBEDDING_DIMENSION * MAX_FLOAT32_JSON_BYTES + 256
)
EMBEDDING_TIMEOUT_SECONDS = 30.0
CHECKPOINT_TIMEOUT_SECONDS = 120.0
CHECKPOINT_POLL_SECONDS = 0.05
MAX_AUTHORIZATION_BYTES = 4096

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
ALLOWED_DOCUMENT_SUFFIXES = frozenset({".docx", ".html", ".md", ".pdf", ".pptx", ".txt"})
DNS_LABEL_RE = re.compile(r"^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")
GROUP_RE = re.compile(
    r"^partner/[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?/"
    r"[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$"
)
LOCALPART_RE = re.compile(r"^[a-z0-9._=/+-]+$")
HOST_RE = re.compile(r"^[A-Za-z0-9.-]+$")
IPV4_SHAPED_RE = re.compile(r"^[0-9]{1,3}(?:\.[0-9]{1,3}){3}$")
PORT_RE = re.compile(r"^[0-9]{1,5}$")
CHUNK_ID_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
SOURCE_DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")


class IngestionError(ValueError):
    """An input or boundary failed the ingestion contract."""


class ConversionResult(Protocol):
    """The Docling conversion result shape used by this script."""

    @property
    def document(self) -> object:
        """Return the converted Docling document."""
        ...

    @property
    def status(self) -> object:
        """Return the Docling conversion status."""
        ...


class DocumentConverter(Protocol):
    """The narrow Docling converter seam used by tests and production."""

    def convert(
        self,
        source: Path,
        *,
        raises_on_error: bool,
        max_file_size: int,
        max_num_pages: int,
    ) -> ConversionResult: ...


class DocumentChunker(Protocol):
    """The narrow Docling chunker seam used by tests and production."""

    def chunk(self, document: object) -> Iterable[object]: ...

    def contextualize(self, chunk: object) -> str: ...


@dataclass(frozen=True)
class Principal:
    """One normalized typed Matrix principal."""

    kind: str
    principal: str
    network: str | None = None

    def as_dict(self) -> dict[str, str]:
        result = {"kind": self.kind, "principal": self.principal}
        if self.network is not None:
            result["network"] = self.network
        return result


@dataclass(frozen=True)
class EmbeddingsEndpoint:
    """A validated HTTP embedding endpoint."""

    hostname: str
    port: int
    path: str


type _SocketAddress = tuple[str, int] | tuple[str, int, int, int]


@dataclass(frozen=True)
class _ResolvedAddress:
    """One numeric stream address resolved before the HTTP socket is opened."""

    family: int
    socktype: int
    proto: int
    sockaddr: _SocketAddress


@dataclass
class _RequestDeadline:
    """Abort the current HTTP socket when its post-resolution deadline expires."""

    expired: threading.Event
    sock: socket.socket | None = None

    def attach(self, sock: socket.socket) -> None:
        self.sock = sock
        if self.expired.is_set():
            self.abort()
            raise TimeoutError

    def expire(self) -> None:
        self.expired.set()
        self.abort()

    def abort(self) -> None:
        if self.sock is None:
            return
        with suppress(OSError):
            self.sock.shutdown(socket.SHUT_RDWR)
        with suppress(OSError):
            self.sock.close()


class _DeadlineHTTPConnection(http.client.HTTPConnection):
    """Retain the live socket so a watchdog can interrupt headers and body reads."""

    def __init__(
        self,
        host: str,
        port: int,
        *,
        timeout: float,
        deadline: _RequestDeadline,
        addresses: Sequence[_ResolvedAddress],
    ) -> None:
        super().__init__(host, port, timeout=timeout)
        self._deadline = deadline
        self._connect_timeout = timeout
        self._addresses = addresses

    @override
    def connect(self) -> None:
        last_error: OSError | None = None
        for address in self._addresses:
            candidate = socket.socket(address.family, address.socktype, address.proto)
            candidate.settimeout(self._connect_timeout)
            self._deadline.attach(candidate)
            try:
                candidate.connect(address.sockaddr)
                with suppress(OSError):
                    candidate.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)
            except OSError as error:
                candidate.close()
                if self._deadline.expired.is_set():
                    raise TimeoutError from error
                last_error = error
                continue
            self.sock = candidate
            return
        if last_error is not None:
            raise last_error
        raise OSError("HTTP endpoint resolved no usable stream address")


@dataclass(frozen=True)
class SourceMetadata:
    """Stable source identity and provenance copied to every chunk."""

    source_id: str
    locator: str
    revision: str
    title: str | None = None

    def as_dict(self, location: str) -> dict[str, str]:
        result = {
            "id": self.source_id,
            "locator": self.locator,
            "revision": self.revision,
            "location": location,
        }
        if self.title is not None:
            result["title"] = self.title
        return result


@dataclass(frozen=True)
class SourceSpec:
    """One fully validated source document and its authorization metadata."""

    path: Path
    relative_path: PurePosixPath
    content_digest: str
    metadata: SourceMetadata
    classification: str
    allowed_principals: tuple[Principal, ...]
    allowed_groups: tuple[str, ...]

    def chunk_metadata(self, location: str) -> dict[str, object]:
        return {
            "source": self.metadata.as_dict(location),
            "classification": self.classification,
            "allowed_principals": [principal.as_dict() for principal in self.allowed_principals],
            "allowed_groups": list(self.allowed_groups),
        }


@dataclass(frozen=True)
class SourceManifest:
    """The validated bounded source manifest."""

    corpus: str
    sources: tuple[SourceSpec, ...]


def _reject_json_constant(value: str) -> object:
    raise IngestionError(f"JSON constant is not permitted: {value}")


def _strict_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise IngestionError(f"duplicate JSON object key: {key}")
        result[key] = value
    return result


def _read_bounded_bytes(path: Path, max_bytes: int) -> bytes:
    try:
        with path.open("rb") as handle:
            before = os.fstat(handle.fileno())
            raw = handle.read(max_bytes + 1)
            after = os.fstat(handle.fileno())
    except OSError as error:
        raise IngestionError(f"could not read {path}: {error}") from error
    if not raw or len(raw) > max_bytes:
        raise IngestionError(f"{path} must contain between 1 and {max_bytes} bytes")
    if (
        before.st_dev != after.st_dev
        or before.st_ino != after.st_ino
        or before.st_size != after.st_size
        or before.st_mtime_ns != after.st_mtime_ns
        or after.st_size != len(raw)
    ):
        raise IngestionError(f"{path} changed while it was being read")
    return raw


def _read_json(path: Path, max_bytes: int) -> object:
    raw = _read_bounded_bytes(path, max_bytes)
    try:
        return json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_strict_object,
            parse_constant=_reject_json_constant,
        )
    except (RecursionError, UnicodeDecodeError, ValueError, json.JSONDecodeError) as error:
        raise IngestionError(f"{path} is not strict UTF-8 JSON: {error}") from error


def _expect_object(
    value: object,
    *,
    name: str,
    required: frozenset[str],
    allowed: frozenset[str],
) -> dict[str, object]:
    if not isinstance(value, dict):
        raise IngestionError(f"{name} must be an object")
    keys = frozenset(value)
    missing = required - keys
    unknown = keys - allowed
    if missing:
        raise IngestionError(f"{name} is missing required fields: {', '.join(sorted(missing))}")
    if unknown:
        raise IngestionError(f"{name} has unknown fields: {', '.join(sorted(unknown))}")
    return cast(dict[str, object], value)


def _expect_list(value: object, *, name: str, maximum: int, non_empty: bool = False) -> list[object]:
    if not isinstance(value, list):
        raise IngestionError(f"{name} must be an array")
    if non_empty and not value:
        raise IngestionError(f"{name} must not be empty")
    if len(value) > maximum:
        raise IngestionError(f"{name} exceeds the maximum of {maximum} entries")
    return cast(list[object], value)


def _clean_text(value: object, *, name: str, max_bytes: int) -> str:
    if not isinstance(value, str):
        raise IngestionError(f"{name} must be a string")
    normalized = unicodedata.normalize("NFC", value)
    if normalized != value:
        raise IngestionError(f"{name} must already use NFC normalization")
    if normalized != normalized.strip() or not normalized:
        raise IngestionError(f"{name} must be non-empty with no surrounding whitespace")
    if len(normalized.encode("utf-8")) > max_bytes:
        raise IngestionError(f"{name} exceeds {max_bytes} UTF-8 bytes")
    if any(unicodedata.category(character).startswith("C") for character in normalized):
        raise IngestionError(f"{name} contains a control or format character")
    return normalized


def _dns_label(value: object, *, name: str) -> str:
    label = _clean_text(value, name=name, max_bytes=63)
    if DNS_LABEL_RE.fullmatch(label) is None:
        raise IngestionError(f"{name} must be a DNS-1123 label")
    return label


def _valid_port(raw: str | None) -> bool:
    if raw is None:
        return True
    if PORT_RE.fullmatch(raw) is None:
        return False
    port = int(raw)
    return 1 <= port <= 65535


def _full_mxid(value: object, *, name: str) -> str:
    mxid = _clean_text(value, name=name, max_bytes=255)
    if not mxid.startswith("@") or ":" not in mxid:
        raise IngestionError(f"{name} must be a full Matrix user ID")
    localpart, server_name = mxid[1:].split(":", 1)
    if not localpart or LOCALPART_RE.fullmatch(localpart) is None or not server_name:
        raise IngestionError(f"{name} must be a full Matrix user ID")

    host = server_name
    port: str | None = None
    if server_name.startswith("["):
        closing = server_name.find("]")
        if closing < 0:
            raise IngestionError(f"{name} has an invalid IPv6 server name")
        host = server_name[1:closing]
        suffix = server_name[closing + 1 :]
        if suffix:
            if not suffix.startswith(":"):
                raise IngestionError(f"{name} has an invalid IPv6 server name")
            port = suffix[1:]
        if "%" in host:
            raise IngestionError(f"{name} has an invalid IPv6 server name")
        try:
            if ipaddress.ip_address(host).version != 6:
                raise IngestionError(f"{name} has an invalid IPv6 server name")
        except ValueError as error:
            raise IngestionError(f"{name} has an invalid IPv6 server name") from error
    else:
        if server_name.count(":") == 1:
            host, port = server_name.rsplit(":", 1)
        if not host or len(host.encode("utf-8")) > 255:
            raise IngestionError(f"{name} has an invalid server name")
        try:
            address = ipaddress.ip_address(host)
        except ValueError:
            if IPV4_SHAPED_RE.fullmatch(host) is not None:
                raise IngestionError(f"{name} has an invalid IPv4 server name") from None
            if HOST_RE.fullmatch(host) is None:
                raise IngestionError(f"{name} has an invalid server name") from None
        else:
            if address.version != 4:
                raise IngestionError(f"{name} must bracket an IPv6 server name")
    if not _valid_port(port):
        raise IngestionError(f"{name} has an invalid server port")
    return mxid


def _relative_source_path(value: object, *, name: str) -> PurePosixPath:
    raw = _clean_text(value, name=name, max_bytes=512)
    if "\\" in raw:
        raise IngestionError(f"{name} must use POSIX separators")
    path = PurePosixPath(raw)
    if path.is_absolute() or any(part in {"", ".", ".."} for part in path.parts):
        raise IngestionError(f"{name} must be a contained relative path")
    if path.suffix.lower() not in ALLOWED_DOCUMENT_SUFFIXES:
        raise IngestionError(f"{name} has an unsupported document suffix")
    return path


def _principal(value: object, *, name: str) -> Principal:
    obj = _expect_object(
        value,
        name=name,
        required=frozenset({"kind", "principal"}),
        allowed=frozenset({"kind", "network", "principal"}),
    )
    kind = _clean_text(obj["kind"], name=f"{name}.kind", max_bytes=32)
    if kind not in {"matrix", "bridged_matrix"}:
        raise IngestionError(f"{name}.kind must be matrix or bridged_matrix")
    principal = _full_mxid(obj["principal"], name=f"{name}.principal")
    network_value = obj.get("network")
    if kind == "matrix":
        if network_value is not None:
            raise IngestionError(f"{name}.network is forbidden for native Matrix principals")
        return Principal(kind=kind, principal=principal)
    if kind == "bridged_matrix":
        if network_value is None:
            raise IngestionError(f"{name}.network is required for bridged Matrix principals")
        return Principal(
            kind=kind,
            principal=principal,
            network=_dns_label(network_value, name=f"{name}.network"),
        )
    raise AssertionError(f"unhandled principal kind: {kind}")


def _principals(value: object, *, name: str) -> tuple[Principal, ...]:
    raw = _expect_list(value, name=name, maximum=64)
    principals = tuple(_principal(item, name=f"{name}[{index}]") for index, item in enumerate(raw))
    keys = [json.dumps(principal.as_dict(), sort_keys=True, separators=(",", ":")) for principal in principals]
    if len(keys) != len(set(keys)):
        raise IngestionError(f"{name} contains a duplicate principal")
    return tuple(principal for _, principal in sorted(zip(keys, principals, strict=True)))


def _groups(value: object, *, name: str) -> tuple[str, ...]:
    raw = _expect_list(value, name=name, maximum=64)
    groups = tuple(_clean_text(item, name=f"{name}[{index}]", max_bytes=191) for index, item in enumerate(raw))
    if any(GROUP_RE.fullmatch(group) is None for group in groups):
        raise IngestionError(f"{name} values must be exact partner/<policy-id>/<group-id> names")
    if len(groups) != len(set(groups)):
        raise IngestionError(f"{name} contains a duplicate group")
    return tuple(sorted(groups))


def _source_digest(value: object, *, name: str) -> str:
    digest = _clean_text(value, name=name, max_bytes=71)
    if SOURCE_DIGEST_RE.fullmatch(digest) is None:
        raise IngestionError(f"{name} must be sha256 followed by 64 lowercase hexadecimal characters")
    return digest


def _source_metadata(
    value: object,
    *,
    name: str,
    corpus: str | None,
    require_location: bool,
) -> SourceMetadata | dict[str, str]:
    required = {"id", "locator", "revision"}
    allowed = {"id", "locator", "revision", "title"}
    if require_location:
        required.add("location")
        allowed.add("location")
    obj = _expect_object(
        value,
        name=name,
        required=frozenset(required),
        allowed=frozenset(allowed),
    )
    source_id = _clean_text(obj["id"], name=f"{name}.id", max_bytes=512)
    if corpus is not None and not source_id.startswith(f"{corpus}/"):
        raise IngestionError(f"{name}.id must be namespaced below corpus {corpus}/")
    locator = _clean_text(obj["locator"], name=f"{name}.locator", max_bytes=2048)
    revision = _clean_text(obj["revision"], name=f"{name}.revision", max_bytes=255)
    title_value = obj.get("title")
    title = None if title_value is None else _clean_text(title_value, name=f"{name}.title", max_bytes=512)
    if require_location:
        location = _clean_text(obj["location"], name=f"{name}.location", max_bytes=512)
        result = {
            "id": source_id,
            "locator": locator,
            "revision": revision,
            "location": location,
        }
        if title is not None:
            result["title"] = title
        return result
    return SourceMetadata(source_id=source_id, locator=locator, revision=revision, title=title)


def _resolve_source(root: Path, relative: PurePosixPath, max_source_bytes: int) -> Path:
    try:
        resolved_root = root.resolve(strict=True)
        candidate = (resolved_root / Path(*relative.parts)).resolve(strict=True)
        candidate.relative_to(resolved_root)
        stat = candidate.stat()
    except (OSError, ValueError) as error:
        raise IngestionError(f"source path is unavailable or escapes its bundle: {relative}") from error
    if not candidate.is_file() or not 1 <= stat.st_size <= max_source_bytes:
        raise IngestionError(f"source file {relative} must contain between 1 and {max_source_bytes} bytes")
    return candidate


def _validate_archive(path: Path) -> None:
    if path.suffix.lower() not in {".docx", ".pptx"}:
        return
    try:
        with zipfile.ZipFile(path) as archive:
            entries = archive.infolist()
            if not entries or len(entries) > MAX_ARCHIVE_ENTRIES:
                raise IngestionError(f"archive source must contain between 1 and {MAX_ARCHIVE_ENTRIES} entries")
            names: set[str] = set()
            total_uncompressed = 0
            for entry in entries:
                raw_name = entry.filename
                name = PurePosixPath(raw_name)
                normalized_name = name.as_posix()
                is_directory = entry.is_dir()
                canonical_name = f"{normalized_name}/" if is_directory else normalized_name
                unix_file_type = stat.S_IFMT(entry.external_attr >> 16)
                if (
                    not raw_name
                    or "\\" in raw_name
                    or name.is_absolute()
                    or any(part in {"", ".", ".."} for part in name.parts)
                    or raw_name != canonical_name
                    or normalized_name in names
                    or entry.flag_bits & 0x1
                    or (is_directory and unix_file_type not in {0, stat.S_IFDIR})
                    or (not is_directory and unix_file_type not in {0, stat.S_IFREG})
                ):
                    raise IngestionError("archive source contains an unsafe or duplicate entry")
                names.add(normalized_name)
                total_uncompressed += entry.file_size
                if total_uncompressed > MAX_ARCHIVE_UNCOMPRESSED_BYTES:
                    raise IngestionError(f"archive source expands beyond {MAX_ARCHIVE_UNCOMPRESSED_BYTES} bytes")
                if entry.file_size > MAX_ARCHIVE_COMPRESSION_RATIO * max(entry.compress_size, 1):
                    raise IngestionError("archive source exceeds the compression-ratio bound")
    except (OSError, zipfile.BadZipFile) as error:
        raise IngestionError("archive source is not a valid bounded office document") from error


def _read_verified_source(path: Path, max_bytes: int, expected_digest: str) -> bytes:
    content = _read_bounded_bytes(path, max_bytes)
    actual_digest = f"sha256:{hashlib.sha256(content).hexdigest()}"
    if actual_digest != expected_digest:
        raise IngestionError(f"source content digest does not match manifest: {path}")
    return content


def load_manifest(
    manifest_path: Path,
    source_root: Path,
    *,
    max_manifest_bytes: int = MAX_MANIFEST_BYTES,
    max_source_bytes: int = MAX_SOURCE_BYTES,
    max_total_source_bytes: int = MAX_TOTAL_SOURCE_BYTES,
) -> SourceManifest:
    """Validate the complete manifest and every source before downstream work."""
    document = _read_json(manifest_path, max_manifest_bytes)
    obj = _expect_object(
        document,
        name="manifest",
        required=frozenset({"schema_version", "corpus", "sources"}),
        allowed=frozenset({"schema_version", "corpus", "sources"}),
    )
    if type(obj["schema_version"]) is not int or obj["schema_version"] != SCHEMA_VERSION:
        raise IngestionError(f"manifest.schema_version must equal {SCHEMA_VERSION}")
    corpus = _dns_label(obj["corpus"], name="manifest.corpus")
    raw_sources = _expect_list(obj["sources"], name="manifest.sources", maximum=MAX_SOURCES, non_empty=True)

    sources: list[SourceSpec] = []
    source_ids: set[str] = set()
    source_paths: set[Path] = set()
    total_source_bytes = 0
    for index, raw_source in enumerate(raw_sources):
        name = f"manifest.sources[{index}]"
        source_obj = _expect_object(
            raw_source,
            name=name,
            required=frozenset(
                {
                    "path",
                    "digest",
                    "source",
                    "classification",
                    "allowed_principals",
                    "allowed_groups",
                }
            ),
            allowed=frozenset(
                {
                    "path",
                    "digest",
                    "source",
                    "classification",
                    "allowed_principals",
                    "allowed_groups",
                }
            ),
        )
        relative = _relative_source_path(source_obj["path"], name=f"{name}.path")
        content_digest = _source_digest(source_obj["digest"], name=f"{name}.digest")
        path = _resolve_source(source_root, relative, max_source_bytes)
        _validate_archive(path)
        metadata = _source_metadata(source_obj["source"], name=f"{name}.source", corpus=corpus, require_location=False)
        if not isinstance(metadata, SourceMetadata):
            raise AssertionError("source metadata parser returned the wrong type")
        classification = _clean_text(
            source_obj["classification"],
            name=f"{name}.classification",
            max_bytes=32,
        )
        if classification not in CLASSIFICATIONS:
            raise IngestionError(f"{name}.classification is unknown")
        principals = _principals(source_obj["allowed_principals"], name=f"{name}.allowed_principals")
        groups = _groups(source_obj["allowed_groups"], name=f"{name}.allowed_groups")
        if not principals and not groups:
            raise IngestionError(f"{name} requires at least one authorization operand")
        if metadata.source_id in source_ids:
            raise IngestionError(f"manifest has duplicate source id: {metadata.source_id}")
        if path in source_paths:
            raise IngestionError(f"manifest references the same source path more than once: {relative}")
        content = _read_verified_source(path, max_source_bytes, content_digest)
        source_ids.add(metadata.source_id)
        source_paths.add(path)
        total_source_bytes += len(content)
        if total_source_bytes > max_total_source_bytes:
            raise IngestionError(f"manifest sources exceed {max_total_source_bytes} total bytes")
        sources.append(
            SourceSpec(
                path=path,
                relative_path=relative,
                content_digest=content_digest,
                metadata=metadata,
                classification=classification,
                allowed_principals=principals,
                allowed_groups=groups,
            )
        )
    return SourceManifest(corpus=corpus, sources=tuple(sources))


def _projected_revision(root: Path) -> str | None:
    marker = root / "..data"
    try:
        return str(marker.readlink()) if marker.is_symlink() else None
    except OSError as error:
        raise IngestionError(f"could not inspect projected bundle revision: {error}") from error


def _atomic_write_bytes(path: Path, content: bytes, *, mode: int = 0o640) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary_path = Path(temporary_name)
    try:
        os.fchmod(descriptor, mode)
        with os.fdopen(descriptor, "wb") as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        temporary_path.replace(path)
    except Exception:
        temporary_path.unlink(missing_ok=True)
        raise


def snapshot_bundle(manifest_path: Path, source_root: Path, snapshot_root: Path) -> SourceManifest:
    """Pin one bounded source revision before Docling sees any document."""
    if snapshot_root.exists() and any(snapshot_root.iterdir()):
        raise IngestionError("snapshot output must be empty")
    projected_revision = _projected_revision(source_root)
    manifest_bytes = _read_bounded_bytes(manifest_path, MAX_MANIFEST_BYTES)
    _atomic_write_bytes(snapshot_root / "manifest.json", manifest_bytes)
    manifest = load_manifest(snapshot_root / "manifest.json", source_root)
    for source in manifest.sources:
        content = _read_verified_source(source.path, MAX_SOURCE_BYTES, source.content_digest)
        _atomic_write_bytes(snapshot_root / "sources" / Path(*source.relative_path.parts), content)
    if _projected_revision(source_root) != projected_revision:
        raise IngestionError("projected source bundle changed while it was being snapshotted")
    return load_manifest(snapshot_root / "manifest.json", snapshot_root / "sources")


def _empty_directory(path: Path, *, mode: int = 0o2770) -> None:
    try:
        if path.exists():
            if not path.is_dir() or any(path.iterdir()):
                raise IngestionError(f"isolated parser directory must be empty: {path}")
        else:
            path.mkdir(parents=True, mode=mode)
        path.chmod(mode)
    except OSError as error:
        raise IngestionError(f"could not prepare isolated parser directory {path}: {error}") from error


def prepare_parser_boundaries(
    manifest: SourceManifest,
    snapshot_root: Path,
    raw_root: Path,
    tmp_root: Path,
) -> None:
    """Prepare subPath mounts so Docling can see exactly one source and one output."""
    if len(manifest.sources) != 1:
        raise IngestionError("one ingestion run requires exactly one source")
    input_root = snapshot_root / "parser"
    _empty_directory(input_root)
    _empty_directory(raw_root)
    _empty_directory(tmp_root)
    source = manifest.sources[0]
    isolated_source = input_root / f"{ISOLATED_SOURCE_BASENAME}{source.relative_path.suffix.lower()}"
    try:
        os.link(source.path, isolated_source)
    except OSError as error:
        raise IngestionError("could not isolate the parser source") from error


def _normalized_chunk_text(value: str) -> str:
    normalized = unicodedata.normalize("NFC", value.replace("\r\n", "\n").replace("\r", "\n")).strip()
    if not normalized:
        raise IngestionError("Docling produced an empty chunk")
    if len(normalized.encode("utf-8")) > MAX_CHUNK_BYTES:
        raise IngestionError(f"Docling chunk exceeds {MAX_CHUNK_BYTES} UTF-8 bytes")
    for character in normalized:
        if unicodedata.category(character).startswith("C") and character not in {"\n", "\t"}:
            raise IngestionError("Docling chunk contains a forbidden control or format character")
    return normalized


def _chunk_id(source_id: str, content: str, occurrence: int) -> str:
    content_digest = hashlib.sha256(content.encode("utf-8")).hexdigest()
    material = b"\x00".join(
        (
            CHUNK_ID_DOMAIN.encode("ascii"),
            EMBEDDING_PROFILE.encode("ascii"),
            source_id.encode("utf-8"),
            content_digest.encode("ascii"),
            str(occurrence).encode("ascii"),
        )
    )
    return f"sha256:{hashlib.sha256(material).hexdigest()}"


def _load_docling() -> tuple[DocumentConverter, DocumentChunker]:
    try:
        converter_module = importlib.import_module("docling.document_converter")
        chunker_module = importlib.import_module("docling.chunking")
        converter_class: Any = converter_module.DocumentConverter
        chunker_class: Any = chunker_module.HierarchicalChunker
        return cast(DocumentConverter, converter_class()), cast(
            DocumentChunker,
            chunker_class(always_emit_headings=False),
        )
    except (AttributeError, ModuleNotFoundError, TypeError) as error:
        raise IngestionError("the pinned Docling runtime is unavailable or incompatible") from error


def build_source_raw_records(
    source: Path,
    *,
    converter: DocumentConverter | None = None,
    chunker: DocumentChunker | None = None,
    max_pages: int = MAX_PAGES,
    max_source_bytes: int = MAX_SOURCE_BYTES,
) -> list[dict[str, object]]:
    """Convert one already-isolated source into text-only untrusted records."""
    if converter is None or chunker is None:
        converter, chunker = _load_docling()

    try:
        result = converter.convert(
            source,
            raises_on_error=True,
            max_file_size=max_source_bytes,
            max_num_pages=max_pages,
        )
    except Exception as error:
        raise IngestionError("Docling conversion failed for one validated source") from error
    status = getattr(result.status, "value", result.status)
    if status != "success":
        raise IngestionError("Docling conversion did not finish with exact SUCCESS")

    source_records: list[dict[str, object]] = []
    try:
        chunks = chunker.chunk(result.document)
        for index, chunk in enumerate(chunks, start=1):
            if index > MAX_CHUNKS_PER_SOURCE:
                raise IngestionError(f"source exceeds {MAX_CHUNKS_PER_SOURCE} chunks")
            content = _normalized_chunk_text(chunker.contextualize(chunk))
            source_records.append(
                {
                    "ordinal": index,
                    "content": content,
                }
            )
    except IngestionError:
        raise
    except Exception as error:
        raise IngestionError("Docling chunking failed for one validated source") from error
    if not source_records:
        raise IngestionError("Docling produced no chunks for one validated source")
    return source_records


def build_raw_records(
    manifest: SourceManifest,
    *,
    converter: DocumentConverter | None = None,
    chunker: DocumentChunker | None = None,
    max_pages: int = MAX_PAGES,
    max_source_bytes: int = MAX_SOURCE_BYTES,
) -> list[list[dict[str, object]]]:
    """Build text-only records in trusted manifest order for offline validation."""
    if converter is None or chunker is None:
        converter, chunker = _load_docling()

    groups: list[list[dict[str, object]]] = []
    total_records = 0
    total_bytes = 0
    for source in manifest.sources:
        source_records = build_source_raw_records(
            source.path,
            converter=converter,
            chunker=chunker,
            max_pages=max_pages,
            max_source_bytes=max_source_bytes,
        )
        groups.append(source_records)
        total_records += len(source_records)
        if total_records > MAX_TOTAL_CHUNKS:
            raise IngestionError(f"manifest exceeds {MAX_TOTAL_CHUNKS} chunks")
        total_bytes += sum(len(json.dumps(record, ensure_ascii=False).encode("utf-8")) + 1 for record in source_records)
        if total_bytes > MAX_JSONL_BYTES:
            raise IngestionError(f"raw chunk output exceeds {MAX_JSONL_BYTES} bytes")
    return groups


def isolated_source(source_root: Path) -> Path:
    """Return the sole source visible inside the parser container."""
    try:
        if source_root.is_symlink() or not source_root.is_dir():
            raise IngestionError("isolated parser input must be one real directory")
        entries = list(source_root.iterdir())
    except OSError as error:
        raise IngestionError("could not inspect isolated parser input") from error
    if len(entries) != 1:
        raise IngestionError("isolated parser input must contain exactly one source")
    source = entries[0]
    if (
        source.is_symlink()
        or not source.is_file()
        or source.stem != ISOLATED_SOURCE_BASENAME
        or source.suffix.lower() not in ALLOWED_DOCUMENT_SUFFIXES
    ):
        raise IngestionError("isolated parser input contains an invalid source")
    try:
        source_stat = source.stat()
    except OSError as error:
        raise IngestionError("could not inspect isolated parser source") from error
    if not 1 <= source_stat.st_size <= MAX_SOURCE_BYTES:
        raise IngestionError(f"isolated source must contain between 1 and {MAX_SOURCE_BYTES} bytes")
    _validate_archive(source)
    return source


def parse_isolated_source(
    source_root: Path,
    output_path: Path,
    *,
    converter: DocumentConverter | None = None,
    chunker: DocumentChunker | None = None,
) -> int:
    """Parse one container-isolated source without seeing the manifest or another source."""
    source = isolated_source(source_root)
    try:
        if output_path.parent.is_symlink() or not output_path.parent.is_dir() or any(output_path.parent.iterdir()):
            raise IngestionError("isolated parser output directory must be empty")
    except OSError as error:
        raise IngestionError("could not inspect isolated parser output") from error
    records = build_source_raw_records(source, converter=converter, chunker=chunker)
    write_jsonl(output_path, records)
    return len(records)


def _raw_record(value: object, *, name: str) -> dict[str, object]:
    obj = _expect_object(
        value,
        name=name,
        required=frozenset({"ordinal", "content"}),
        allowed=frozenset({"ordinal", "content"}),
    )
    ordinal = obj["ordinal"]
    if type(ordinal) is not int or not 1 <= ordinal <= MAX_CHUNKS_PER_SOURCE:
        raise IngestionError(f"{name}.ordinal must be in 1..{MAX_CHUNKS_PER_SOURCE}")
    content = _normalized_chunk_text(_clean_text_or_content(obj["content"], name=f"{name}.content"))
    return {"ordinal": ordinal, "content": content}


def _read_jsonl_values(
    path: Path,
    *,
    max_bytes: int = MAX_JSONL_BYTES,
) -> Iterable[tuple[int, object]]:
    try:
        handle = path.open("rb")
    except OSError as error:
        raise IngestionError(f"could not open JSONL input {path}: {error}") from error
    total_bytes = 0
    line_number = 0
    with handle:
        while True:
            raw_line = handle.readline(MAX_JSONL_LINE_BYTES + 1)
            if not raw_line:
                break
            line_number += 1
            total_bytes += len(raw_line)
            if len(raw_line) > MAX_JSONL_LINE_BYTES or not raw_line.endswith(b"\n"):
                raise IngestionError(f"JSONL line {line_number} exceeds the bounded line size")
            if total_bytes > max_bytes:
                raise IngestionError(f"JSONL input exceeds {max_bytes} bytes")
            if not raw_line.strip():
                raise IngestionError(f"JSONL line {line_number} is empty")
            try:
                value = json.loads(
                    raw_line.decode("utf-8"),
                    object_pairs_hook=_strict_object,
                    parse_constant=_reject_json_constant,
                )
            except (RecursionError, UnicodeDecodeError, ValueError, json.JSONDecodeError) as error:
                raise IngestionError(f"JSONL line {line_number} is invalid") from error
            yield line_number, value


def read_raw_records(path: Path) -> list[dict[str, object]]:
    """Read the untrusted Docling handoff with strict complete bounds."""
    records: list[dict[str, object]] = []
    seen_ordinals: set[int] = set()
    for line_number, value in _read_jsonl_values(path):
        record = _raw_record(value, name=f"raw_records[{line_number - 1}]")
        ordinal = cast(int, record["ordinal"])
        if ordinal in seen_ordinals:
            raise IngestionError("raw chunk input contains a duplicate ordinal")
        seen_ordinals.add(ordinal)
        records.append(record)
        if len(records) > MAX_CHUNKS_PER_SOURCE:
            raise IngestionError(f"raw source input exceeds {MAX_CHUNKS_PER_SOURCE} records")
    if not records:
        raise IngestionError("raw chunk input must not be empty")
    return records


def write_raw_record_groups(
    root: Path,
    groups: Sequence[Sequence[dict[str, object]]],
) -> None:
    """Write the sole parser payload for one source-scoped ingestion run."""
    if len(groups) != 1:
        raise IngestionError("one ingestion run requires exactly one raw source group")
    if root.exists() and any(root.iterdir()):
        raise IngestionError("raw output directory must be empty")
    root.mkdir(parents=True, exist_ok=True)
    write_jsonl(root / RAW_RECORD_FILENAME, groups[0])


def bind_raw_records(manifest: SourceManifest, raw_root: Path) -> list[dict[str, object]]:
    """Bind the sole parser payload to its trusted source and security metadata."""
    if len(manifest.sources) != 1:
        raise IngestionError("one ingestion run requires exactly one source")
    try:
        entries = list(raw_root.iterdir())
    except OSError as error:
        raise IngestionError("could not inspect the raw Docling output directory") from error
    if (
        len(entries) != 1
        or entries[0].name != RAW_RECORD_FILENAME
        or entries[0].is_symlink()
        or not entries[0].is_file()
    ):
        raise IngestionError("Docling output must contain one exact parser result")
    try:
        total_bytes = entries[0].stat().st_size
    except OSError as error:
        raise IngestionError("could not inspect the raw Docling result") from error
    if total_bytes > MAX_JSONL_BYTES:
        raise IngestionError(f"raw chunk input exceeds {MAX_JSONL_BYTES} bytes")

    bound: list[dict[str, object]] = []
    ids: dict[str, str] = {}
    source = manifest.sources[0]
    source_records = sorted(
        read_raw_records(raw_root / RAW_RECORD_FILENAME),
        key=lambda item: cast(int, item["ordinal"]),
    )
    expected_ordinals = list(range(1, len(source_records) + 1))
    actual_ordinals = [cast(int, record["ordinal"]) for record in source_records]
    if actual_ordinals != expected_ordinals:
        raise IngestionError("Docling output ordinals must be complete and contiguous")
    occurrences: dict[str, int] = {}
    for record in source_records:
        content = cast(str, record["content"])
        digest = hashlib.sha256(content.encode("utf-8")).hexdigest()
        occurrence = occurrences.get(digest, 0)
        occurrences[digest] = occurrence + 1
        chunk_id = _chunk_id(source.metadata.source_id, content, occurrence)
        previous = ids.get(chunk_id)
        if previous is not None:
            raise IngestionError("stable chunk identifier collision")
        ids[chunk_id] = content
        ordinal = cast(int, record["ordinal"])
        bound.append(
            {
                "chunk_id": chunk_id,
                "content": content,
                "metadata": source.chunk_metadata(f"chunk:{ordinal:06d}"),
            }
        )
    bound.sort(key=lambda record: cast(str, record["chunk_id"]))
    return bound


def _normalize_metadata(value: object, *, name: str) -> dict[str, object]:
    obj = _expect_object(
        value,
        name=name,
        required=frozenset({"source", "classification", "allowed_principals", "allowed_groups"}),
        allowed=frozenset({"source", "classification", "allowed_principals", "allowed_groups"}),
    )
    source = _source_metadata(obj["source"], name=f"{name}.source", corpus=None, require_location=True)
    if not isinstance(source, dict):
        raise AssertionError("chunk source metadata parser returned the wrong type")
    classification = _clean_text(obj["classification"], name=f"{name}.classification", max_bytes=32)
    if classification not in CLASSIFICATIONS:
        raise IngestionError(f"{name}.classification is unknown")
    principals = _principals(obj["allowed_principals"], name=f"{name}.allowed_principals")
    groups = _groups(obj["allowed_groups"], name=f"{name}.allowed_groups")
    if not principals and not groups:
        raise IngestionError(f"{name} requires at least one authorization operand")
    return {
        "source": source,
        "classification": classification,
        "allowed_principals": [principal.as_dict() for principal in principals],
        "allowed_groups": list(groups),
    }


def _record(value: object, *, name: str, with_embedding: bool) -> dict[str, object]:
    required = {"chunk_id", "content", "metadata"}
    if with_embedding:
        required.add("embedding")
    obj = _expect_object(
        value,
        name=name,
        required=frozenset(required),
        allowed=frozenset(required),
    )
    chunk_id = _clean_text(obj["chunk_id"], name=f"{name}.chunk_id", max_bytes=512)
    if CHUNK_ID_RE.fullmatch(chunk_id) is None:
        raise IngestionError(f"{name}.chunk_id must be a stable sha256 identifier")
    content = _normalized_chunk_text(_clean_text_or_content(obj["content"], name=f"{name}.content"))
    metadata = _normalize_metadata(obj["metadata"], name=f"{name}.metadata")
    record: dict[str, object] = {"chunk_id": chunk_id, "content": content, "metadata": metadata}
    if with_embedding:
        embedding = obj["embedding"]
        record["embedding"] = None if embedding is None else _embedding_vector(embedding, name=f"{name}.embedding")
    return record


def _clean_text_or_content(value: object, *, name: str) -> str:
    if not isinstance(value, str):
        raise IngestionError(f"{name} must be a string")
    return value


def _read_jsonl(path: Path, *, with_embedding: bool) -> list[dict[str, object]]:
    records: list[dict[str, object]] = []
    seen_ids: set[str] = set()
    max_bytes = MAX_EMBEDDED_JSONL_BYTES if with_embedding else MAX_JSONL_BYTES
    for line_number, value in _read_jsonl_values(path, max_bytes=max_bytes):
        record = _record(value, name=f"records[{line_number - 1}]", with_embedding=with_embedding)
        chunk_id = cast(str, record["chunk_id"])
        if chunk_id in seen_ids:
            raise IngestionError("JSONL input contains a duplicate chunk_id")
        seen_ids.add(chunk_id)
        records.append(record)
        if len(records) > MAX_TOTAL_CHUNKS:
            raise IngestionError(f"JSONL input exceeds {MAX_TOTAL_CHUNKS} records")
    if not records:
        raise IngestionError("JSONL input must not be empty")
    records.sort(key=lambda record: cast(str, record["chunk_id"]))
    return records


def _embedding_vector(value: object, *, name: str) -> list[float]:
    values = _expect_list(value, name=name, maximum=EMBEDDING_DIMENSION)
    if len(values) != EMBEDDING_DIMENSION:
        raise IngestionError(f"{name} must contain exactly {EMBEDDING_DIMENSION} values")
    vector: list[float] = []
    nonzero = False
    for index, item in enumerate(values):
        if isinstance(item, bool) or not isinstance(item, int | float):
            raise IngestionError(f"{name}[{index}] must be a finite number")
        number = float(item)
        if not math.isfinite(number):
            raise IngestionError(f"{name}[{index}] must be finite")
        try:
            quantized = struct.unpack("!f", struct.pack("!f", number))[0]
        except OverflowError as error:
            raise IngestionError(f"{name}[{index}] is outside the float32 range") from error
        if not math.isfinite(quantized):
            raise IngestionError(f"{name}[{index}] is outside the finite float32 range")
        nonzero = nonzero or quantized != 0
        vector.append(quantized)
    if not nonzero:
        raise IngestionError(f"{name} must be non-zero")
    return vector


def _validate_embeddings_url(url: str, *, allow_loopback: bool) -> EmbeddingsEndpoint:
    parts = urlsplit(url)
    if (
        parts.scheme != "http"
        or parts.username is not None
        or parts.password is not None
        or parts.query
        or parts.fragment
        or parts.path != "/v1/embeddings"
    ):
        raise IngestionError("embeddings URL must be plain in-cluster HTTP on the exact /v1/embeddings path")
    hostname = parts.hostname
    if hostname is None:
        raise IngestionError("embeddings URL must include a hostname")
    port = parts.port or 80
    if allow_loopback:
        if hostname not in {"127.0.0.1", "localhost"}:
            raise IngestionError("test embeddings URL must use loopback")
    elif hostname != "agentgateway-proxy.agentgateway-system.svc.cluster.local" or port != 8082:
        raise IngestionError("production embeddings URL must target the exact agentgateway service")
    return EmbeddingsEndpoint(hostname=hostname, port=port, path=parts.path)


def _embedding_request_body(model: str, inputs: Sequence[str]) -> bytes:
    return json.dumps(
        {"model": model, "input": list(inputs), "encoding_format": "float"},
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
    ).encode("utf-8")


def _tokenize_request_body(model: str, content: str) -> bytes:
    return json.dumps(
        {
            "model": model,
            "prompt": content,
            "add_special_tokens": True,
            "return_token_strs": False,
        },
        ensure_ascii=False,
        allow_nan=False,
        separators=(",", ":"),
    ).encode("utf-8")


def _resolve_endpoint_addresses(
    hostname: str,
    port: int,
    *,
    timeout_seconds: float,
) -> tuple[_ResolvedAddress, ...]:
    """Resolve one endpoint without letting the blocking system resolver exceed the request budget."""
    addresses: list[_ResolvedAddress] = []
    errors: list[OSError] = []

    def resolve() -> None:
        try:
            resolved = socket.getaddrinfo(
                hostname,
                port,
                family=socket.AF_UNSPEC,
                type=socket.SOCK_STREAM,
            )
            for family, socktype, proto, _, sockaddr in resolved:
                if family not in {socket.AF_INET, socket.AF_INET6} or socktype != socket.SOCK_STREAM:
                    continue
                addresses.append(
                    _ResolvedAddress(
                        family=int(family),
                        socktype=int(socktype),
                        proto=proto,
                        sockaddr=cast(_SocketAddress, sockaddr),
                    )
                )
        except OSError as error:
            errors.append(error)

    worker = threading.Thread(target=resolve, name="knowledge-http-resolver", daemon=True)
    worker.start()
    worker.join(timeout_seconds)
    if worker.is_alive():
        raise TimeoutError
    if errors:
        raise errors[0]
    if not addresses:
        raise OSError("HTTP endpoint resolved no usable stream address")
    return tuple(addresses)


def _post_bounded_json(
    endpoint: EmbeddingsEndpoint,
    *,
    path: str,
    body: bytes,
    max_request_bytes: int,
    max_response_bytes: int,
    timeout_seconds: float,
    authorization: str | None,
    operation: str,
) -> object:
    if len(body) > max_request_bytes:
        raise IngestionError(f"{operation} request exceeds {max_request_bytes} bytes")
    started_at = time.monotonic()
    try:
        addresses = _resolve_endpoint_addresses(
            endpoint.hostname,
            endpoint.port,
            timeout_seconds=timeout_seconds,
        )
    except TimeoutError as error:
        raise IngestionError(f"{operation} request exceeded its bounded deadline") from error
    except OSError as error:
        raise IngestionError(f"{operation} request failed before a bounded response") from error
    remaining_seconds = timeout_seconds - (time.monotonic() - started_at)
    if remaining_seconds <= 0:
        raise IngestionError(f"{operation} request exceeded its bounded deadline")
    deadline = _RequestDeadline(expired=threading.Event())
    connection = _DeadlineHTTPConnection(
        endpoint.hostname,
        endpoint.port,
        timeout=remaining_seconds,
        deadline=deadline,
        addresses=addresses,
    )
    watchdog = threading.Timer(remaining_seconds, deadline.expire)
    watchdog.daemon = True
    headers = {
        "Content-Type": "application/json",
        "Accept": "application/json",
        "Connection": "close",
    }
    if authorization is not None:
        headers["Authorization"] = authorization
    watchdog.start()
    try:
        connection.request("POST", path, body=body, headers=headers)
        response = connection.getresponse()
        content_type = response.getheader("Content-Type", "").split(";", 1)[0].strip().lower()
        raw_buffer = bytearray()
        while len(raw_buffer) <= max_response_bytes:
            chunk = response.read1(min(64 * 1024, max_response_bytes + 1 - len(raw_buffer)))
            if not chunk:
                break
            raw_buffer.extend(chunk)
        raw = bytes(raw_buffer)
    except (OSError, TimeoutError, http.client.HTTPException) as error:
        if deadline.expired.is_set():
            raise IngestionError(f"{operation} request exceeded its bounded deadline") from error
        raise IngestionError(f"{operation} request failed before a bounded response") from error
    finally:
        watchdog.cancel()
        connection.close()
        watchdog.join()
    if deadline.expired.is_set():
        raise IngestionError(f"{operation} request exceeded its bounded deadline")
    if response.status != 200:
        raise IngestionError(f"{operation} backend returned HTTP {response.status}")
    if content_type != "application/json":
        raise IngestionError(f"{operation} backend returned a non-JSON content type")
    if len(raw) > max_response_bytes:
        raise IngestionError(f"{operation} response exceeds {max_response_bytes} bytes")
    try:
        return json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_strict_object,
            parse_constant=_reject_json_constant,
        )
    except (UnicodeDecodeError, json.JSONDecodeError) as error:
        raise IngestionError(f"{operation} backend returned invalid JSON") from error


def _preflight_tokenize(
    url: str,
    *,
    model: str,
    content: str,
    timeout_seconds: float,
    allow_loopback: bool,
    authorization: str | None,
) -> None:
    endpoint = _validate_embeddings_url(url, allow_loopback=allow_loopback)
    if not allow_loopback and authorization is None:
        raise IngestionError("production tokenization calls require the ingestion workload credential")
    document = _post_bounded_json(
        endpoint,
        path=TOKENIZE_PATH,
        body=_tokenize_request_body(model, content),
        max_request_bytes=MAX_EMBEDDING_REQUEST_BYTES,
        max_response_bytes=MAX_TOKENIZE_RESPONSE_BYTES,
        timeout_seconds=timeout_seconds,
        authorization=authorization,
        operation="tokenization",
    )
    obj = _expect_object(
        document,
        name="tokenize_response",
        required=frozenset({"count", "max_model_len", "tokens", "token_strs"}),
        allowed=frozenset({"count", "max_model_len", "tokens", "token_strs"}),
    )
    count = obj["count"]
    max_model_len = obj["max_model_len"]
    if type(count) is not int or not 1 <= count <= MAX_MODEL_TOKENS:
        raise IngestionError(f"tokenize_response.count must be in 1..{MAX_MODEL_TOKENS}")
    if type(max_model_len) is not int or max_model_len != MAX_MODEL_TOKENS:
        raise IngestionError(f"tokenize_response.max_model_len must equal {MAX_MODEL_TOKENS}")
    tokens = _expect_list(obj["tokens"], name="tokenize_response.tokens", maximum=MAX_MODEL_TOKENS)
    if count != len(tokens):
        raise IngestionError("tokenize_response.count must equal the exact tokens length")
    for index, token in enumerate(tokens):
        if type(token) is not int or not 0 <= token <= 2**63 - 1:
            raise IngestionError(f"tokenize_response.tokens[{index}] must be a non-negative int64")
    if obj["token_strs"] is not None:
        raise IngestionError("tokenize_response.token_strs must remain null")


def _post_embeddings(
    url: str,
    *,
    model: str,
    inputs: Sequence[str],
    timeout_seconds: float,
    allow_loopback: bool,
    authorization: str | None,
) -> list[list[float]]:
    endpoint = _validate_embeddings_url(url, allow_loopback=allow_loopback)
    if not allow_loopback and authorization is None:
        raise IngestionError("production embedding calls require the ingestion workload credential")
    body = _embedding_request_body(model, inputs)
    document = _post_bounded_json(
        endpoint,
        path=endpoint.path,
        body=body,
        max_request_bytes=MAX_EMBEDDING_REQUEST_BYTES,
        max_response_bytes=MAX_EMBEDDING_RESPONSE_BYTES,
        timeout_seconds=timeout_seconds,
        authorization=authorization,
        operation="embedding",
    )
    obj = _expect_object(
        document,
        name="embedding_response",
        required=frozenset({"data", "model"}),
        allowed=frozenset({"data", "model", "object", "usage"}),
    )
    response_model = _clean_text(obj["model"], name="embedding_response.model", max_bytes=255)
    if response_model != model:
        raise IngestionError("embedding backend returned a different model")
    data = _expect_list(obj["data"], name="embedding_response.data", maximum=len(inputs))
    if len(data) != len(inputs):
        raise IngestionError("embedding backend returned the wrong number of vectors")
    by_index: dict[int, list[float]] = {}
    for position, item in enumerate(data):
        item_obj = _expect_object(
            item,
            name=f"embedding_response.data[{position}]",
            required=frozenset({"embedding", "index"}),
            allowed=frozenset({"embedding", "index", "object"}),
        )
        index = item_obj["index"]
        if type(index) is not int or not 0 <= index < len(inputs) or index in by_index:
            raise IngestionError("embedding backend returned an invalid or duplicate index")
        by_index[index] = _embedding_vector(
            item_obj["embedding"],
            name=f"embedding_response.data[{position}].embedding",
        )
    return [by_index[index] for index in range(len(inputs))]


def _embedding_batches(
    records: Sequence[dict[str, object]],
    *,
    model: str,
    batch_size: int,
) -> Iterable[list[dict[str, object]]]:
    batch: list[dict[str, object]] = []
    for record in records:
        candidate = [*batch, record]
        candidate_inputs = [cast(str, item["content"]) for item in candidate]
        if (
            len(candidate) > batch_size
            or len(_embedding_request_body(model, candidate_inputs)) > MAX_EMBEDDING_REQUEST_BYTES
        ):
            if not batch:
                raise IngestionError("one chunk cannot fit in a bounded embedding request")
            yield batch
            batch = [record]
            if len(_embedding_request_body(model, [cast(str, record["content"])])) > MAX_EMBEDDING_REQUEST_BYTES:
                raise IngestionError("one chunk cannot fit in a bounded embedding request")
        else:
            batch = candidate
    if batch:
        yield batch


def _checkpoint_records(
    batch: Sequence[dict[str, object]],
    vectors: Sequence[list[float]],
) -> list[dict[str, object]]:
    return [
        {
            "profile": EMBEDDING_PROFILE,
            "content": cast(str, record["content"]),
            "embedding": vector,
        }
        for record, vector in zip(batch, vectors, strict=True)
    ]


def _read_checkpoint(path: Path) -> list[dict[str, object]]:
    records: list[dict[str, object]] = []
    seen_content: set[str] = set()
    for line_number, value in _read_jsonl_values(path, max_bytes=MAX_CHECKPOINT_BYTES):
        name = f"checkpoint[{line_number - 1}]"
        obj = _expect_object(
            value,
            name=name,
            required=frozenset({"profile", "content", "embedding"}),
            allowed=frozenset({"profile", "content", "embedding"}),
        )
        profile = _clean_text(obj["profile"], name=f"{name}.profile", max_bytes=64)
        if profile != EMBEDDING_PROFILE:
            raise IngestionError(f"{name}.profile must equal {EMBEDDING_PROFILE}")
        content = _normalized_chunk_text(_clean_text_or_content(obj["content"], name=f"{name}.content"))
        if content in seen_content:
            raise IngestionError("checkpoint contains duplicate content")
        seen_content.add(content)
        records.append(
            {
                "profile": profile,
                "content": content,
                "embedding": _embedding_vector(obj["embedding"], name=f"{name}.embedding"),
            }
        )
        if len(records) > MAX_EMBEDDING_BATCH:
            raise IngestionError(f"checkpoint exceeds {MAX_EMBEDDING_BATCH} records")
    if not records:
        raise IngestionError("checkpoint must contain at least one record")
    return records


def _await_checkpoint_ack(
    checkpoint_root: Path,
    *,
    expected: Sequence[dict[str, object]] | None,
    timeout_seconds: float,
) -> tuple[Path, list[dict[str, object]]]:
    ready = checkpoint_root / CHECKPOINT_READY_FILENAME
    acked = checkpoint_root / CHECKPOINT_ACKED_FILENAME
    deadline = time.monotonic() + timeout_seconds
    while True:
        if acked.exists():
            if ready.exists():
                raise IngestionError("checkpoint ready and ack files cannot coexist")
            records = _read_checkpoint(acked)
            if expected is not None and records != list(expected):
                raise IngestionError("checkpoint acknowledgement differs from the published batch")
            return acked, records
        if not ready.exists():
            if acked.exists():
                continue
            raise IngestionError("checkpoint disappeared without a database acknowledgement")
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise IngestionError("checkpoint database acknowledgement timed out")
        time.sleep(min(CHECKPOINT_POLL_SECONDS, remaining))


def _apply_checkpoint(
    checkpoint_records: Sequence[dict[str, object]],
    by_content: dict[str, list[dict[str, object]]],
) -> None:
    for checkpoint in checkpoint_records:
        content = cast(str, checkpoint["content"])
        duplicates = by_content.pop(content, None)
        if duplicates is None:
            raise IngestionError("checkpoint content is absent from the current missing plan")
        vector = cast(list[float], checkpoint["embedding"])
        for duplicate in duplicates:
            duplicate["embedding"] = vector


def _resume_checkpoint(
    checkpoint_root: Path,
    by_content: dict[str, list[dict[str, object]]],
    *,
    timeout_seconds: float,
) -> None:
    ready = checkpoint_root / CHECKPOINT_READY_FILENAME
    acked = checkpoint_root / CHECKPOINT_ACKED_FILENAME
    if not ready.exists() and not acked.exists():
        return
    ack_path, records = _await_checkpoint_ack(
        checkpoint_root,
        expected=None,
        timeout_seconds=timeout_seconds,
    )
    _apply_checkpoint(records, by_content)
    ack_path.unlink()


def embed_plan(
    plan_path: Path,
    output_path: Path,
    *,
    url: str = EMBEDDINGS_URL,
    model: str = EMBEDDING_MODEL,
    batch_size: int = MAX_EMBEDDING_BATCH,
    timeout_seconds: float = EMBEDDING_TIMEOUT_SECONDS,
    allow_loopback: bool = False,
    authorization: str | None = None,
    checkpoint_root: Path | None = None,
    checkpoint_timeout_seconds: float = CHECKPOINT_TIMEOUT_SECONDS,
) -> tuple[int, int]:
    """Reuse unchanged vectors and call agentgateway only for missing embeddings."""
    if model != EMBEDDING_MODEL:
        raise IngestionError(f"embedding model must remain fixed at {EMBEDDING_MODEL}")
    if not 1 <= batch_size <= MAX_EMBEDDING_BATCH:
        raise IngestionError(f"embedding batch size must be in 1..{MAX_EMBEDDING_BATCH}")
    if not 1 <= timeout_seconds <= 120:
        raise IngestionError("embedding timeout must be in 1..120 seconds")
    if not 0 < checkpoint_timeout_seconds <= 300:
        raise IngestionError("checkpoint timeout must be greater than 0 and at most 300 seconds")
    if not allow_loopback and authorization is None:
        raise IngestionError("production embedding calls require the ingestion workload credential")
    if not allow_loopback and checkpoint_root is None:
        raise IngestionError("production embedding calls require the durable checkpoint root")
    if checkpoint_root is not None and (not checkpoint_root.exists() or not checkpoint_root.is_dir()):
        raise IngestionError("checkpoint root must be an existing directory")
    records = _read_jsonl(plan_path, with_embedding=True)
    missing = [record for record in records if record["embedding"] is None]
    by_content: dict[str, list[dict[str, object]]] = {}
    unique_missing: list[dict[str, object]] = []
    for record in missing:
        content = cast(str, record["content"])
        if content not in by_content:
            by_content[content] = []
            unique_missing.append(record)
        by_content[content].append(record)
    if checkpoint_root is not None:
        _resume_checkpoint(
            checkpoint_root,
            by_content,
            timeout_seconds=checkpoint_timeout_seconds,
        )
    unique_missing = [record for record in unique_missing if cast(str, record["content"]) in by_content]
    for record in unique_missing:
        _preflight_tokenize(
            url,
            model=model,
            content=cast(str, record["content"]),
            timeout_seconds=timeout_seconds,
            allow_loopback=allow_loopback,
            authorization=authorization,
        )
    for batch in _embedding_batches(unique_missing, model=model, batch_size=batch_size):
        vectors = _post_embeddings(
            url,
            model=model,
            inputs=[cast(str, record["content"]) for record in batch],
            timeout_seconds=timeout_seconds,
            allow_loopback=allow_loopback,
            authorization=authorization,
        )
        checkpoint_records = _checkpoint_records(batch, vectors)
        if checkpoint_root is not None:
            ready = checkpoint_root / CHECKPOINT_READY_FILENAME
            acked = checkpoint_root / CHECKPOINT_ACKED_FILENAME
            if ready.exists() or acked.exists():
                raise IngestionError("checkpoint single-flight files were not cleared")
            write_jsonl(ready, checkpoint_records, max_bytes=MAX_CHECKPOINT_BYTES)
            ack_path, acknowledged = _await_checkpoint_ack(
                checkpoint_root,
                expected=checkpoint_records,
                timeout_seconds=checkpoint_timeout_seconds,
            )
            _apply_checkpoint(acknowledged, by_content)
            ack_path.unlink()
        else:
            _apply_checkpoint(checkpoint_records, by_content)
    write_jsonl(output_path, records, max_bytes=MAX_EMBEDDED_JSONL_BYTES)
    return len(records), len(missing)


def read_authorization(path: Path) -> str:
    """Read one bounded Bearer credential without exposing it through arguments or logs."""
    raw = _read_bounded_bytes(path, MAX_AUTHORIZATION_BYTES)
    try:
        value = raw.decode("ascii")
    except UnicodeDecodeError as error:
        raise IngestionError("workload authorization must be ASCII") from error
    if (
        not value.startswith("Bearer ")
        or len(value) <= len("Bearer ")
        or value != value.strip()
        or any(character.isspace() for character in value[len("Bearer ") :])
    ):
        raise IngestionError("workload authorization must be one exact Bearer credential")
    return value


def write_jsonl(
    path: Path,
    records: Sequence[dict[str, object]],
    *,
    max_bytes: int = MAX_JSONL_BYTES,
) -> None:
    """Atomically write bounded canonical JSONL for the Pod's shared ingestion group."""
    path.parent.mkdir(parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=f".{path.name}.", dir=path.parent)
    temporary_path = Path(temporary_name)
    total_bytes = 0
    try:
        os.fchmod(descriptor, 0o640)
        with os.fdopen(descriptor, "wb") as handle:
            for record in records:
                line = (
                    json.dumps(
                        record,
                        ensure_ascii=False,
                        allow_nan=False,
                        sort_keys=True,
                        separators=(",", ":"),
                    ).encode("utf-8")
                    + b"\n"
                )
                if len(line) > MAX_JSONL_LINE_BYTES:
                    raise IngestionError(f"JSONL output line exceeds {MAX_JSONL_LINE_BYTES} bytes")
                total_bytes += len(line)
                if total_bytes > max_bytes:
                    raise IngestionError(f"JSONL output exceeds {max_bytes} bytes")
                handle.write(line)
            handle.flush()
            os.fsync(handle.fileno())
        temporary_path.replace(path)
    except Exception:
        temporary_path.unlink(missing_ok=True)
        raise


def _build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    subparsers = parser.add_subparsers(dest="command", required=True)

    validate = subparsers.add_parser("validate", help="validate a source manifest and bundle")
    validate.add_argument("--manifest", type=Path, required=True)
    validate.add_argument("--source-root", type=Path, required=True)

    snapshot = subparsers.add_parser("snapshot", help="pin and validate one immutable source revision")
    snapshot.add_argument("--manifest", type=Path, required=True)
    snapshot.add_argument("--source-root", type=Path, required=True)
    snapshot.add_argument("--output-root", type=Path, required=True)
    snapshot.add_argument("--raw-root", type=Path, required=True)
    snapshot.add_argument("--tmp-root", type=Path, required=True)

    parse = subparsers.add_parser("parse-isolated", help="parse the sole source mounted into this container")
    parse.add_argument("--source-root", type=Path, required=True)
    parse.add_argument("--output", type=Path, required=True)

    bind = subparsers.add_parser("bind", help="bind trusted security metadata after Docling")
    bind.add_argument("--manifest", type=Path, required=True)
    bind.add_argument("--source-root", type=Path, required=True)
    bind.add_argument("--raw-root", type=Path, required=True)
    bind.add_argument("--output", type=Path, required=True)

    embed = subparsers.add_parser("embed", help="reuse existing vectors and embed only changed chunks")
    embed.add_argument("--plan", type=Path, required=True)
    embed.add_argument("--output", type=Path, required=True)
    embed.add_argument("--url", default=EMBEDDINGS_URL)
    embed.add_argument("--model", default=EMBEDDING_MODEL)
    embed.add_argument("--batch-size", type=int, default=MAX_EMBEDDING_BATCH)
    embed.add_argument("--timeout-seconds", type=float, default=EMBEDDING_TIMEOUT_SECONDS)
    embed.add_argument("--authorization-file", type=Path, required=True)
    embed.add_argument("--checkpoint-root", type=Path, required=True)
    embed.add_argument("--checkpoint-timeout-seconds", type=float, default=CHECKPOINT_TIMEOUT_SECONDS)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    """Run one bounded ingestion phase without logging corpus content."""
    logging.basicConfig(level=logging.INFO, format="%(levelname)s %(name)s %(message)s")
    args = _build_parser().parse_args(argv)
    try:
        if args.command == "validate":
            manifest = load_manifest(args.manifest, args.source_root)
            LOGGER.info("validated manifest corpus=%s sources=%d", manifest.corpus, len(manifest.sources))
            return 0
        if args.command == "snapshot":
            manifest = snapshot_bundle(args.manifest, args.source_root, args.output_root)
            prepare_parser_boundaries(manifest, args.output_root, args.raw_root, args.tmp_root)
            LOGGER.info("snapshotted manifest corpus=%s sources=%d", manifest.corpus, len(manifest.sources))
            return 0
        if args.command == "parse-isolated":
            record_count = parse_isolated_source(args.source_root, args.output)
            LOGGER.info("parsed isolated source chunks=%d", record_count)
            return 0
        if args.command == "bind":
            manifest = load_manifest(args.manifest, args.source_root)
            records = bind_raw_records(manifest, args.raw_root)
            write_jsonl(args.output, records)
            LOGGER.info(
                "bound manifest corpus=%s sources=%d chunks=%d",
                manifest.corpus,
                len(manifest.sources),
                len(records),
            )
            return 0
        if args.command == "embed":
            record_count, embedded_count = embed_plan(
                args.plan,
                args.output,
                url=args.url,
                model=args.model,
                batch_size=args.batch_size,
                timeout_seconds=args.timeout_seconds,
                authorization=read_authorization(args.authorization_file),
                checkpoint_root=args.checkpoint_root,
                checkpoint_timeout_seconds=args.checkpoint_timeout_seconds,
            )
            LOGGER.info("prepared chunks=%d newly_embedded=%d", record_count, embedded_count)
            return 0
    except (IngestionError, OSError) as error:
        LOGGER.error("ingestion denied: %s", error)
        return 2
    raise AssertionError(f"unhandled command: {args.command}")


if __name__ == "__main__":
    raise SystemExit(main())
