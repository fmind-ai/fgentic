"""Immutable Git/Markdown source connector and deterministic sync planner."""

from __future__ import annotations

import hashlib
import ipaddress
import json
import re
import tarfile
import unicodedata
from collections.abc import Collection, Mapping
from dataclasses import dataclass
from io import BytesIO
from pathlib import PurePosixPath
from typing import Protocol, cast
from urllib.parse import SplitResult, urlsplit

ACL_MANIFEST_PATH = ".fgentic/knowledge-acl.json"
ACL_SCHEMA_VERSION = 1

MAX_ARTIFACT_BYTES = 32 * 1024 * 1024
MAX_ARCHIVE_ENTRIES = 4096
MAX_ARCHIVE_BYTES = 64 * 1024 * 1024
MAX_MANIFEST_BYTES = 256 * 1024
MAX_SOURCES = 512
MAX_SOURCE_BYTES = 16 * 1024 * 1024
MAX_TOTAL_SOURCE_BYTES = 64 * 1024 * 1024

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
DNS_LABEL_RE = re.compile(r"^[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$")
GROUP_RE = re.compile(
    r"^partner/[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?/"
    r"[a-z0-9](?:[-a-z0-9]{0,61}[a-z0-9])?$"
)
LOCALPART_RE = re.compile(r"^[a-z0-9._=/+-]+$")
DNS_HOST_LABEL_RE = re.compile(r"^[A-Za-z0-9](?:[-A-Za-z0-9]{0,61}[A-Za-z0-9])?$")
IPV4_SHAPED_RE = re.compile(r"^[0-9]{1,3}(?:\.[0-9]{1,3}){3}$")
PORT_RE = re.compile(r"^[0-9]{1,5}$")
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
LFS_POINTER_VERSION = "version https://git-lfs.github.com/spec/v1"


class ConnectorError(ValueError):
    """A connector artifact or checkpoint failed the fail-closed contract."""


class ArtifactReader(Protocol):
    """The bounded binary stream seam accepted by the reference connector."""

    def read(self, size: int = -1, /) -> bytes:
        """Read at most ``size`` bytes, returning an empty byte string at EOF."""
        ...


@dataclass(frozen=True)
class ArtifactStatus:
    """Immutable Flux source-controller artifact status projected into the job."""

    revision: str
    digest: str
    url: str
    size: int


@dataclass(frozen=True)
class SnapshotCursor:
    """The complete immutable source snapshot observed by a sync."""

    revision: str
    digest: str


@dataclass(frozen=True)
class Principal:
    """One normalized Matrix principal copied from the source-owned ACL."""

    kind: str
    principal: str
    network: str | None = None

    def as_dict(self) -> dict[str, str]:
        result = {"kind": self.kind, "principal": self.principal}
        if self.network is not None:
            result["network"] = self.network
        return result


@dataclass(frozen=True)
class SourceACL:
    """The complete source ACL mirrored into every derived chunk."""

    classification: str
    allowed_principals: tuple[Principal, ...]
    allowed_groups: tuple[str, ...]

    def as_dict(self) -> dict[str, object]:
        return {
            "classification": self.classification,
            "allowed_principals": [principal.as_dict() for principal in self.allowed_principals],
            "allowed_groups": list(self.allowed_groups),
        }


@dataclass(frozen=True)
class SourceReference:
    """Stable identity returned while enumerating one immutable snapshot."""

    source_id: str
    path: str


@dataclass(frozen=True)
class SourceDocument:
    """One verified Markdown source and its authoritative ACL."""

    source_id: str
    path: str
    locator: str
    revision: str
    content: bytes
    content_digest: str
    acl: SourceACL
    acl_digest: str

    def metadata(self) -> dict[str, object]:
        """Return the exact location-free metadata staged for the database."""
        return {
            "source": {
                "id": self.source_id,
                "locator": self.locator,
                "revision": self.revision,
            },
            **self.acl.as_dict(),
        }


class SourceConnector(Protocol):
    """Minimal immutable connector interface consumed by the sync planner."""

    @property
    def connector_id(self) -> str:
        """Return the stable connector instance identifier."""
        ...

    @property
    def corpus(self) -> str:
        """Return the stable destination corpus identifier."""
        ...

    @property
    def cursor(self) -> SnapshotCursor:
        """Return the immutable snapshot cursor."""
        ...

    @property
    def artifact_digest(self) -> str:
        """Return the verified immutable provider artifact digest."""
        ...

    def enumerate_sources(self) -> tuple[SourceReference, ...]:
        """Enumerate the complete desired source set in stable order."""
        ...

    def fetch_source(self, source_id: str) -> SourceDocument:
        """Fetch verified content and the source-owned ACL for one enumerated item."""
        ...

    def report_deletions(self, applied_source_ids: Collection[str]) -> tuple[str, ...]:
        """Report previously applied connector sources absent from this snapshot."""
        ...


@dataclass(frozen=True)
class GitMarkdownConnector:
    """Exactly one reference connector over a verified Flux Git artifact."""

    connector_id: str
    corpus: str
    cursor: SnapshotCursor
    artifact_digest: str
    _sources: tuple[SourceDocument, ...]

    @classmethod
    def from_artifact(
        cls,
        *,
        connector_id: str,
        status: ArtifactStatus,
        artifact: bytes | ArtifactReader,
    ) -> GitMarkdownConnector:
        """Validate a complete immutable artifact before exposing any source."""
        normalized_connector_id = _dns_label(connector_id, name="connector_id")
        normalized_status = _validate_status(status)
        repository_identity = _repository_identity(normalized_status.url)
        raw = _read_artifact(artifact)
        if len(raw) != normalized_status.size:
            raise ConnectorError("artifact byte length does not match Flux status size")
        if _digest(raw) != normalized_status.digest:
            raise ConnectorError("artifact digest does not match Flux status digest")

        files = _read_archive(raw)
        manifest_raw = files.get(ACL_MANIFEST_PATH)
        if manifest_raw is None:
            raise ConnectorError(f"artifact is missing the source-owned ACL manifest {ACL_MANIFEST_PATH}")
        manifest = _load_manifest(manifest_raw)
        corpus = _dns_label(manifest["corpus"], name="manifest.corpus")
        acl = _manifest_acl(manifest)
        acl_digest = _digest(_canonical_json(acl.as_dict()))
        source_paths = _reference_markdown_paths(files)

        sources: list[SourceDocument] = []
        total_source_bytes = 0
        for path in source_paths:
            content = files[path]
            if not 1 <= len(content) <= MAX_SOURCE_BYTES:
                raise ConnectorError(f"source must contain between 1 and {MAX_SOURCE_BYTES} bytes")
            total_source_bytes += len(content)
            if total_source_bytes > MAX_TOTAL_SOURCE_BYTES:
                raise ConnectorError(f"selected sources exceed {MAX_TOTAL_SOURCE_BYTES} bytes")
            _validate_markdown(content)

            source_id = _clean_text(
                f"{corpus}/{normalized_connector_id}/{path}",
                name="source ID",
                max_bytes=512,
            )
            locator = _clean_text(
                f"git:{repository_identity}#{path}",
                name="source locator",
                max_bytes=2048,
            )
            content_digest = _digest(content)
            sources.append(
                SourceDocument(
                    source_id=source_id,
                    path=path,
                    locator=locator,
                    revision=content_digest,
                    content=content,
                    content_digest=content_digest,
                    acl=acl,
                    acl_digest=acl_digest,
                )
            )

        ordered_sources = tuple(sorted(sources, key=lambda source: source.source_id))
        return cls(
            connector_id=normalized_connector_id,
            corpus=corpus,
            cursor=SnapshotCursor(
                revision=normalized_status.revision,
                digest=_inventory_digest(ordered_sources),
            ),
            artifact_digest=normalized_status.digest,
            _sources=ordered_sources,
        )

    def enumerate_sources(self) -> tuple[SourceReference, ...]:
        return tuple(SourceReference(source_id=source.source_id, path=source.path) for source in self._sources)

    def fetch_source(self, source_id: str) -> SourceDocument:
        for source in self._sources:
            if source.source_id == source_id:
                return source
        raise ConnectorError(f"source is not present in this immutable snapshot: {source_id}")

    def report_deletions(self, applied_source_ids: Collection[str]) -> tuple[str, ...]:
        prefix = f"{self.corpus}/{self.connector_id}/"
        desired = frozenset(source.source_id for source in self._sources)
        return tuple(
            sorted(
                {
                    source_id
                    for source_id in applied_source_ids
                    if source_id.startswith(prefix) and source_id not in desired
                }
            )
        )


@dataclass(frozen=True)
class AppliedSource:
    """The content and ACL digests last committed for one source."""

    source_id: str
    content_digest: str
    acl_digest: str


@dataclass(frozen=True)
class PresentAction:
    """Apply or replace one desired source."""

    source: SourceDocument
    cursor: SnapshotCursor

    @property
    def source_id(self) -> str:
        return self.source.source_id


@dataclass(frozen=True)
class TombstoneAction:
    """Delete all chunks and connector state for one absent source."""

    source_id: str
    cursor: SnapshotCursor


type SyncAction = PresentAction | TombstoneAction


@dataclass(frozen=True)
class SyncPlan:
    """One bounded action, or a fully converged cursor checkpoint."""

    action: SyncAction | None
    complete_cursor: SnapshotCursor | None


def plan_next(connector: SourceConnector, applied: Mapping[str, AppliedSource]) -> SyncPlan:
    """Select one lexicographic change without advancing a partial snapshot cursor."""
    for source_id, state in applied.items():
        if source_id != state.source_id:
            raise ConnectorError("applied source map key does not match its source ID")
        _validated_digest(state.content_digest, name=f"applied source {source_id} content digest")
        _validated_digest(state.acl_digest, name=f"applied source {source_id} ACL digest")

    candidates: list[SyncAction] = []
    for reference in connector.enumerate_sources():
        source = connector.fetch_source(reference.source_id)
        current = applied.get(reference.source_id)
        if (
            current is None
            or current.content_digest != source.content_digest
            or current.acl_digest != source.acl_digest
        ):
            candidates.append(PresentAction(source=source, cursor=connector.cursor))

    candidates.extend(
        TombstoneAction(source_id=source_id, cursor=connector.cursor)
        for source_id in connector.report_deletions(applied.keys())
    )
    if candidates:
        action = min(candidates, key=lambda candidate: (candidate.source_id, type(candidate).__name__))
        return SyncPlan(action=action, complete_cursor=None)
    return SyncPlan(action=None, complete_cursor=connector.cursor)


def inventory_payload(connector: SourceConnector) -> dict[str, object]:
    """Serialize the complete inventory using the v3 database staging field names."""
    sources: list[dict[str, object]] = []
    for reference in connector.enumerate_sources():
        source = connector.fetch_source(reference.source_id)
        sources.append(
            {
                "connector_id": connector.connector_id,
                "snapshot_revision": connector.cursor.revision,
                "inventory_digest": connector.cursor.digest,
                "source_id": source.source_id,
                "source_path": source.path,
                "source_revision": source.revision,
                "content_digest": source.content_digest,
                "acl_digest": source.acl_digest,
                "metadata": source.metadata(),
            }
        )
    return {
        "connector_id": connector.connector_id,
        "snapshot_revision": connector.cursor.revision,
        "inventory_digest": connector.cursor.digest,
        "artifact_digest": connector.artifact_digest,
        "source_count": len(sources),
        "sources": sources,
    }


def inventory_json(connector: SourceConnector) -> bytes:
    """Return the canonical UTF-8 JSON form of a complete inventory."""
    return _canonical_json(inventory_payload(connector))


def action_payload(connector: SourceConnector, plan: SyncPlan) -> dict[str, object]:
    """Serialize one planner result using the database desired-action field names."""
    action = plan.action
    if action is None:
        if plan.complete_cursor != connector.cursor:
            raise ConnectorError("complete plan cursor does not match the connector snapshot")
        return {
            "action": "complete",
            "connector_id": connector.connector_id,
            "snapshot_revision": connector.cursor.revision,
            "inventory_digest": connector.cursor.digest,
        }
    if action.cursor != connector.cursor:
        raise ConnectorError("action cursor does not match the connector snapshot")
    if isinstance(action, TombstoneAction):
        return {
            "action": "tombstone",
            "connector_id": connector.connector_id,
            "source_id": action.source_id,
            "snapshot_revision": connector.cursor.revision,
            "inventory_digest": connector.cursor.digest,
        }
    source = action.source
    return {
        "action": "present",
        "connector_id": connector.connector_id,
        "source_id": source.source_id,
        "source_path": source.path,
        "source_revision": source.revision,
        "content_digest": source.content_digest,
        "acl_digest": source.acl_digest,
        "metadata": source.metadata(),
        "snapshot_revision": connector.cursor.revision,
        "inventory_digest": connector.cursor.digest,
    }


def action_json(connector: SourceConnector, plan: SyncPlan) -> bytes:
    """Return the canonical UTF-8 JSON form of one action or completion."""
    return _canonical_json(action_payload(connector, plan))


def _read_artifact(artifact: bytes | ArtifactReader) -> bytes:
    if isinstance(artifact, bytes):
        raw = artifact
    else:
        chunks: list[bytes] = []
        remaining = MAX_ARTIFACT_BYTES + 1
        while remaining > 0:
            chunk = artifact.read(min(64 * 1024, remaining))
            if not isinstance(chunk, bytes):
                raise ConnectorError("artifact stream must return bytes")
            if not chunk:
                break
            chunks.append(chunk)
            remaining -= len(chunk)
        raw = b"".join(chunks)
    if not 1 <= len(raw) <= MAX_ARTIFACT_BYTES:
        raise ConnectorError(f"artifact must contain between 1 and {MAX_ARTIFACT_BYTES} bytes")
    return raw


def _validate_status(status: ArtifactStatus) -> ArtifactStatus:
    revision = _clean_text(status.revision, name="artifact revision", max_bytes=255)
    digest = _validated_digest(status.digest, name="artifact digest")
    url = _clean_text(status.url, name="artifact URL", max_bytes=2048)
    parsed = _split_url(url)
    if (
        parsed.scheme != "http"
        or parsed.hostname is None
        or parsed.username is not None
        or parsed.password is not None
        or not parsed.path.startswith("/")
        or parsed.query
        or parsed.fragment
    ):
        raise ConnectorError("artifact URL must be a plain in-cluster HTTP URL without credentials, query, or fragment")
    _repository_identity(url)
    if isinstance(status.size, bool) or not isinstance(status.size, int) or not 1 <= status.size <= MAX_ARTIFACT_BYTES:
        raise ConnectorError(f"artifact size must be an integer between 1 and {MAX_ARTIFACT_BYTES}")
    return ArtifactStatus(revision=revision, digest=digest, url=url, size=status.size)


def _repository_identity(url: str) -> str:
    parsed = _split_url(url)
    hostname = parsed.hostname
    if hostname is None or hostname.rstrip(".") != "source-controller.flux-system.svc.cluster.local":
        raise ConnectorError("artifact URL must belong to the in-cluster Flux source-controller")
    try:
        port = parsed.port
    except ValueError:
        raise ConnectorError("artifact URL contains an invalid source-controller port") from None
    if port not in {None, 80} or "%" in parsed.path:
        raise ConnectorError("artifact URL must use the canonical Flux source-controller address")
    parts = parsed.path.split("/")
    if len(parts) != 5 or parts[0] or parts[1] != "gitrepository" or not parts[4].endswith(".tar.gz"):
        raise ConnectorError("artifact URL must identify one immutable Flux GitRepository artifact")
    namespace = _dns_label(parts[2], name="artifact GitRepository namespace")
    name = _dns_label(parts[3], name="artifact GitRepository name")
    _clean_text(parts[4], name="artifact filename", max_bytes=255)
    return f"{namespace}/{name}"


def _split_url(url: str) -> SplitResult:
    try:
        return urlsplit(url)
    except ValueError:
        raise ConnectorError("artifact URL is invalid") from None


def _read_archive(raw: bytes) -> dict[str, bytes]:
    try:
        with tarfile.open(fileobj=BytesIO(raw), mode="r:gz") as archive:
            names: set[str] = set()
            folded_names: set[str] = set()
            total_bytes = 0
            files: dict[str, bytes] = {}
            entry_count = 0
            for member in archive:
                entry_count += 1
                if entry_count > MAX_ARCHIVE_ENTRIES:
                    raise ConnectorError(f"artifact contains more than {MAX_ARCHIVE_ENTRIES} entries")
                name = _archive_path(member.name, is_directory=member.isdir())
                folded = name.casefold()
                if name in names or folded in folded_names:
                    raise ConnectorError("artifact contains a duplicate or case-colliding path")
                names.add(name)
                folded_names.add(folded)

                if member.isdir():
                    continue
                if not member.isfile() or member.type not in {tarfile.REGTYPE, tarfile.AREGTYPE}:
                    # Never extract links or special entries. The repository itself legitimately
                    # carries unrelated compatibility symlinks, but ambiguity under the selected
                    # docs/ and ACL-manifest boundaries remains a hard failure.
                    if _is_connector_owned_path(name):
                        raise ConnectorError("artifact contains a link or special entry under a connector-owned path")
                    continue
                total_bytes += member.size
                if member.size < 0 or total_bytes > MAX_ARCHIVE_BYTES:
                    raise ConnectorError(f"artifact expands beyond {MAX_ARCHIVE_BYTES} bytes")
                handle = archive.extractfile(member)
                if handle is None:
                    raise ConnectorError("artifact entry cannot be read")
                content = handle.read(member.size + 1)
                if len(content) != member.size:
                    raise ConnectorError("artifact entry is truncated or changed while read")
                files[name] = content
            if entry_count == 0:
                raise ConnectorError("artifact must contain at least one entry")
            return files
    except (OSError, EOFError, tarfile.TarError):
        raise ConnectorError("artifact is not a valid bounded tar.gz archive") from None


def _archive_path(value: str, *, is_directory: bool) -> str:
    raw = _clean_text(value, name="artifact path", max_bytes=512)
    if "\\" in raw:
        raise ConnectorError("artifact paths must use POSIX separators")
    path = PurePosixPath(raw)
    normalized = path.as_posix()
    canonical = {normalized}
    if is_directory:
        canonical.add(f"{normalized}/")
    if path.is_absolute() or raw not in canonical or any(part in {"", ".", ".."} for part in path.parts):
        raise ConnectorError("artifact path must be a canonical contained relative path")
    return normalized


def _is_connector_owned_path(value: str) -> bool:
    path = PurePosixPath(value)
    return path.parts[0] in {"docs", ".fgentic"}


def _load_manifest(raw: bytes) -> dict[str, object]:
    if not 1 <= len(raw) <= MAX_MANIFEST_BYTES:
        raise ConnectorError(f"ACL manifest must contain between 1 and {MAX_MANIFEST_BYTES} bytes")
    try:
        document = json.loads(raw.decode("utf-8"), object_pairs_hook=_strict_object, parse_constant=_reject_constant)
    except (RecursionError, UnicodeDecodeError, ValueError, json.JSONDecodeError):
        raise ConnectorError("ACL manifest must be strict UTF-8 JSON") from None
    manifest = _expect_object(
        document,
        name="manifest",
        required=frozenset(
            {
                "schema_version",
                "corpus",
                "classification",
                "allowed_principals",
                "allowed_groups",
            }
        ),
        allowed=frozenset(
            {
                "schema_version",
                "corpus",
                "classification",
                "allowed_principals",
                "allowed_groups",
            }
        ),
    )
    schema_version = manifest["schema_version"]
    if isinstance(schema_version, bool) or not isinstance(schema_version, int) or schema_version != ACL_SCHEMA_VERSION:
        raise ConnectorError(f"manifest.schema_version must equal {ACL_SCHEMA_VERSION}")
    return manifest


def _manifest_acl(manifest: Mapping[str, object]) -> SourceACL:
    classification = _clean_text(manifest["classification"], name="manifest.classification", max_bytes=64)
    if classification not in CLASSIFICATIONS:
        raise ConnectorError("manifest.classification is unknown")
    principals = _principals(manifest["allowed_principals"], name="manifest.allowed_principals")
    groups = _groups(manifest["allowed_groups"], name="manifest.allowed_groups")
    if not principals and not groups:
        raise ConnectorError("manifest must admit at least one principal or group")
    return SourceACL(
        classification=classification,
        allowed_principals=principals,
        allowed_groups=groups,
    )


def _reference_markdown_paths(files: Mapping[str, bytes]) -> tuple[str, ...]:
    paths: list[str] = []
    for raw in files:
        path = PurePosixPath(raw)
        if len(path.parts) < 2 or path.parts[0] != "docs" or path.suffix.casefold() != ".md":
            continue
        if path.suffix != ".md":
            raise ConnectorError("repository Markdown path must use the lowercase .md suffix")
        paths.append(raw)
    if len(paths) > MAX_SOURCES:
        raise ConnectorError(f"artifact contains more than {MAX_SOURCES} docs/**/*.md sources")
    return tuple(sorted(paths))


def _validate_markdown(content: bytes) -> None:
    try:
        text = content.decode("utf-8")
    except UnicodeDecodeError:
        raise ConnectorError("source must be valid UTF-8 Markdown") from None
    first_line = text.splitlines()[0] if text else ""
    if first_line == LFS_POINTER_VERSION:
        raise ConnectorError("source is a Git LFS pointer, not document content")


def _principal(value: object, *, name: str) -> Principal:
    item = _expect_object(
        value,
        name=name,
        required=frozenset({"kind", "principal"}),
        allowed=frozenset({"kind", "network", "principal"}),
    )
    kind = _clean_text(item["kind"], name=f"{name}.kind", max_bytes=32)
    if kind not in {"matrix", "bridged_matrix"}:
        raise ConnectorError(f"{name}.kind must be matrix or bridged_matrix")
    principal = _full_mxid(item["principal"], name=f"{name}.principal")
    network_value = item.get("network")
    if kind == "matrix":
        if network_value is not None:
            raise ConnectorError(f"{name}.network is forbidden for native Matrix principals")
        return Principal(kind=kind, principal=principal)
    if network_value is None:
        raise ConnectorError(f"{name}.network is required for bridged Matrix principals")
    return Principal(
        kind=kind,
        principal=principal,
        network=_dns_label(network_value, name=f"{name}.network"),
    )


def _principals(value: object, *, name: str) -> tuple[Principal, ...]:
    raw = _expect_list(value, name=name, maximum=64)
    principals = tuple(_principal(item, name=f"{name}[{index}]") for index, item in enumerate(raw))
    keys = [_canonical_json(principal.as_dict()) for principal in principals]
    if len(keys) != len(set(keys)):
        raise ConnectorError(f"{name} contains a duplicate principal")
    return tuple(principal for _, principal in sorted(zip(keys, principals, strict=True)))


def _groups(value: object, *, name: str) -> tuple[str, ...]:
    raw = _expect_list(value, name=name, maximum=64)
    groups = tuple(_clean_text(item, name=f"{name}[{index}]", max_bytes=191) for index, item in enumerate(raw))
    if any(GROUP_RE.fullmatch(group) is None for group in groups):
        raise ConnectorError(f"{name} values must be exact partner/<policy-id>/<group-id> names")
    if len(groups) != len(set(groups)):
        raise ConnectorError(f"{name} contains a duplicate group")
    return tuple(sorted(groups))


def _full_mxid(value: object, *, name: str) -> str:
    mxid = _clean_text(value, name=name, max_bytes=255)
    if not mxid.startswith("@") or ":" not in mxid:
        raise ConnectorError(f"{name} must be a full Matrix user ID")
    localpart, server_name = mxid[1:].split(":", 1)
    if not localpart or LOCALPART_RE.fullmatch(localpart) is None or not server_name:
        raise ConnectorError(f"{name} must be a full Matrix user ID")

    host = server_name
    port: str | None = None
    if server_name.startswith("["):
        closing = server_name.find("]")
        if closing < 0:
            raise ConnectorError(f"{name} has an invalid IPv6 server name")
        host = server_name[1:closing]
        suffix = server_name[closing + 1 :]
        if suffix:
            if not suffix.startswith(":"):
                raise ConnectorError(f"{name} has an invalid IPv6 server name")
            port = suffix[1:]
        if "%" in host:
            raise ConnectorError(f"{name} has an invalid IPv6 server name")
        try:
            if ipaddress.ip_address(host).version != 6:
                raise ConnectorError(f"{name} has an invalid IPv6 server name")
        except ValueError:
            raise ConnectorError(f"{name} has an invalid IPv6 server name") from None
    else:
        if server_name.count(":") == 1:
            host, port = server_name.rsplit(":", 1)
        if not host or len(host.encode()) > 255:
            raise ConnectorError(f"{name} has an invalid server name")
        try:
            address = ipaddress.ip_address(host)
        except ValueError:
            if IPV4_SHAPED_RE.fullmatch(host) is not None:
                raise ConnectorError(f"{name} has an invalid IPv4 server name") from None
            if not _valid_dns_host(host):
                raise ConnectorError(f"{name} has an invalid server name") from None
        else:
            if address.version != 4:
                raise ConnectorError(f"{name} must bracket an IPv6 server name")
    if not _valid_port(port):
        raise ConnectorError(f"{name} has an invalid server port")
    return mxid


def _valid_dns_host(raw: str) -> bool:
    host = raw.removesuffix(".")
    return bool(host) and all(DNS_HOST_LABEL_RE.fullmatch(label) is not None for label in host.split("."))


def _valid_port(raw: str | None) -> bool:
    if raw is None:
        return True
    if PORT_RE.fullmatch(raw) is None:
        return False
    return 1 <= int(raw) <= 65535


def _dns_label(value: object, *, name: str) -> str:
    label = _clean_text(value, name=name, max_bytes=63)
    if DNS_LABEL_RE.fullmatch(label) is None:
        raise ConnectorError(f"{name} must be a DNS-1123 label")
    return label


def _validated_digest(value: object, *, name: str) -> str:
    digest = _clean_text(value, name=name, max_bytes=71)
    if DIGEST_RE.fullmatch(digest) is None:
        raise ConnectorError(f"{name} must be sha256 followed by 64 lowercase hexadecimal characters")
    return digest


def _clean_text(value: object, *, name: str, max_bytes: int) -> str:
    if not isinstance(value, str):
        raise ConnectorError(f"{name} must be a string")
    try:
        normalized = unicodedata.normalize("NFC", value)
        encoded = normalized.encode()
    except UnicodeError:
        raise ConnectorError(f"{name} must be valid Unicode text") from None
    if normalized != value:
        raise ConnectorError(f"{name} must already use NFC normalization")
    if not normalized or normalized != normalized.strip():
        raise ConnectorError(f"{name} must be non-empty with no surrounding whitespace")
    if len(encoded) > max_bytes:
        raise ConnectorError(f"{name} exceeds {max_bytes} UTF-8 bytes")
    if any(unicodedata.category(character).startswith("C") for character in normalized):
        raise ConnectorError(f"{name} contains a control or format character")
    return normalized


def _expect_object(
    value: object,
    *,
    name: str,
    required: frozenset[str],
    allowed: frozenset[str],
) -> dict[str, object]:
    if not isinstance(value, dict):
        raise ConnectorError(f"{name} must be an object")
    keys = frozenset(str(key) for key in value)
    missing = required - keys
    unknown = keys - allowed
    if missing:
        raise ConnectorError(f"{name} is missing required fields: {', '.join(sorted(missing))}")
    if unknown:
        raise ConnectorError(f"{name} has unknown fields")
    return cast(dict[str, object], value)


def _expect_list(value: object, *, name: str, maximum: int, non_empty: bool = False) -> list[object]:
    if not isinstance(value, list):
        raise ConnectorError(f"{name} must be an array")
    if non_empty and not value:
        raise ConnectorError(f"{name} must not be empty")
    if len(value) > maximum:
        raise ConnectorError(f"{name} exceeds the maximum of {maximum} entries")
    return cast(list[object], value)


def _strict_object(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise ConnectorError("JSON object contains a duplicate key")
        result[key] = value
    return result


def _reject_constant(_value: str) -> object:
    raise ConnectorError("JSON constant is not permitted")


def _canonical_json(value: object) -> bytes:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":")).encode()


def _digest(content: bytes) -> str:
    return f"sha256:{hashlib.sha256(content).hexdigest()}"


def _inventory_item(source: SourceDocument) -> dict[str, object]:
    return {
        "source_id": source.source_id,
        "source_path": source.path,
        "source_revision": source.revision,
        "content_digest": source.content_digest,
        "acl_digest": source.acl_digest,
        "metadata": source.metadata(),
    }


def _inventory_digest(sources: tuple[SourceDocument, ...]) -> str:
    return _digest(_canonical_json([_inventory_item(source) for source in sources]))
