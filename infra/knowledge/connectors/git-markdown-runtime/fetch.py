"""Acquire one exact Flux Git artifact and publish an immutable connector snapshot."""

from __future__ import annotations

import argparse
import fcntl
import hashlib
import http.client
import json
import os
import re
import shutil
import ssl
import stat
import tempfile
import unicodedata
from collections.abc import Mapping, Sequence
from pathlib import Path
from typing import cast
from urllib.parse import SplitResult, urlsplit

import git_markdown

API_HOST = "kubernetes.default.svc"
API_PATH = "/apis/source.toolkit.fluxcd.io/v1/namespaces/flux-system/gitrepositories/flux-system"
SOURCE_HOST = "source-controller.flux-system.svc.cluster.local"
SOURCE_PATH_PREFIX = "/gitrepository/flux-system/flux-system/"
CONNECTOR_ID = "git-markdown"

MAX_API_BYTES = 256 * 1024
MAX_CA_BYTES = 64 * 1024
MAX_TOKEN_BYTES = 16 * 1024
HTTP_TIMEOUT_SECONDS = 30.0
FILE_MODE = 0o440
DIRECTORY_MODE = 0o550
WRITABLE_DIRECTORY_MODE = 0o770
HTTP_TCHAR_RE = r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+"
HTTP_QUOTED_STRING_RE = r'"(?:[\t !#-\[\]-~\x80-\xff]|\\[\t !-~\x80-\xff])*"'
MEDIA_TYPE_RE = re.compile(
    rf"^[ \t]*(?P<type>{HTTP_TCHAR_RE})/(?P<subtype>{HTTP_TCHAR_RE})"
    rf"(?:[ \t]*;[ \t]*{HTTP_TCHAR_RE}=(?:{HTTP_TCHAR_RE}|{HTTP_QUOTED_STRING_RE}))*[ \t]*$"
)

type JSONObject = dict[str, object]


class AcquisitionError(RuntimeError):
    """The trusted Flux acquisition boundary failed closed."""


def _reject_constant(_value: str) -> object:
    raise AcquisitionError("JSON constant is forbidden")


def _strict_object(pairs: list[tuple[str, object]]) -> JSONObject:
    result: JSONObject = {}
    for key, value in pairs:
        if key in result:
            raise AcquisitionError("duplicate JSON object key")
        result[key] = value
    return result


def _object(value: object, *, name: str) -> JSONObject:
    if not isinstance(value, dict) or not all(isinstance(key, str) for key in value):
        raise AcquisitionError(f"{name} must be an object")
    return cast(JSONObject, value)


def _string(value: object, *, name: str, maximum: int) -> str:
    if not isinstance(value, str) or not value or value != value.strip():
        raise AcquisitionError(f"{name} must be a non-empty trimmed string")
    try:
        normalized = unicodedata.normalize("NFC", value)
        encoded = normalized.encode()
    except UnicodeError:
        raise AcquisitionError(f"{name} is not bounded clean text") from None
    if (
        normalized != value
        or len(encoded) > maximum
        or any(unicodedata.category(character).startswith("C") for character in normalized)
    ):
        raise AcquisitionError(f"{name} is not bounded clean text")
    return value


def _integer(value: object, *, name: str, minimum: int, maximum: int) -> int:
    if isinstance(value, bool) or not isinstance(value, int) or not minimum <= value <= maximum:
        raise AcquisitionError(f"{name} must be an integer in {minimum}..{maximum}")
    return value


def _read_file(path: Path, maximum: int) -> bytes:
    flags = os.O_RDONLY | os.O_CLOEXEC | os.O_NONBLOCK
    try:
        descriptor = os.open(path, flags)
        try:
            before = os.fstat(descriptor)
            if not stat.S_ISREG(before.st_mode):
                raise AcquisitionError(f"projected file {path.name} must be a regular file")
            chunks: list[bytes] = []
            remaining = maximum + 1
            while remaining > 0:
                chunk = os.read(descriptor, min(64 * 1024, remaining))
                if not chunk:
                    break
                chunks.append(chunk)
                remaining -= len(chunk)
            after = os.fstat(descriptor)
        finally:
            os.close(descriptor)
    except OSError as error:
        raise AcquisitionError(f"could not read projected file {path.name}: {error}") from error
    raw = b"".join(chunks)
    if not raw or len(raw) > maximum:
        raise AcquisitionError(f"projected file {path.name} is empty or oversized")
    if (
        before.st_dev != after.st_dev
        or before.st_ino != after.st_ino
        or before.st_size != after.st_size
        or before.st_mtime_ns != after.st_mtime_ns
        or after.st_size != len(raw)
    ):
        raise AcquisitionError(f"projected file {path.name} changed while it was being read")
    return raw


def _declared_response_length(response: http.client.HTTPResponse, maximum: int) -> int | None:
    content_lengths = response.headers.get_all("Content-Length", [])
    transfer_encodings = response.headers.get_all("Transfer-Encoding", [])
    if content_lengths and transfer_encodings:
        raise AcquisitionError("HTTP response framing is ambiguous")
    if len(content_lengths) > 1 or len(transfer_encodings) > 1:
        raise AcquisitionError("HTTP response framing is ambiguous")
    if transfer_encodings:
        if transfer_encodings[0].strip(" \t").lower() != "chunked":
            raise AcquisitionError("HTTP response transfer coding is unsupported")
        return None
    if not content_lengths:
        return None

    value = content_lengths[0].strip(" \t")
    if not value.isascii() or not value.isdecimal():
        raise AcquisitionError("HTTP response Content-Length is invalid")
    normalized = value.lstrip("0") or "0"
    maximum_text = str(maximum)
    if len(normalized) > len(maximum_text) or (len(normalized) == len(maximum_text) and normalized > maximum_text):
        raise AcquisitionError("HTTP response declares an oversized body")
    return int(normalized)


def _read_response(response: http.client.HTTPResponse, maximum: int) -> bytes:
    declared_length = _declared_response_length(response, maximum)
    try:
        raw = response.read(maximum + 1)
    except http.client.HTTPException:
        raise AcquisitionError("HTTP response body is truncated or malformed") from None
    if not raw or len(raw) > maximum:
        raise AcquisitionError("HTTP response is empty or oversized")
    if declared_length is not None and len(raw) != declared_length:
        raise AcquisitionError("HTTP response body is truncated or malformed")
    return raw


def _strict_json_object(raw: bytes, *, name: str) -> JSONObject:
    try:
        document = json.loads(
            raw.decode("utf-8"),
            object_pairs_hook=_strict_object,
            parse_constant=_reject_constant,
        )
    except (AcquisitionError, RecursionError, UnicodeDecodeError, ValueError):
        raise AcquisitionError("HTTP response is not strict UTF-8 JSON") from None
    return _object(document, name=name)


def _has_json_content_type(response: http.client.HTTPResponse) -> bool:
    content_types = response.headers.get_all("Content-Type", [])
    if len(content_types) != 1 or not isinstance(content_types[0], str):
        return False
    match = MEDIA_TYPE_RE.fullmatch(content_types[0])
    return (
        match is not None and match.group("type").lower() == "application" and match.group("subtype").lower() == "json"
    )


def _read_api_response(response: http.client.HTTPResponse) -> bytes:
    if response.status != http.client.OK:
        raise AcquisitionError(f"Kubernetes API returned HTTP {response.status}")
    if not _has_json_content_type(response):
        raise AcquisitionError("Kubernetes API returned an unexpected media type")
    return _read_response(response, MAX_API_BYTES)


def _api_document(token_file: Path, ca_file: Path) -> JSONObject:
    try:
        token = _read_file(token_file, MAX_TOKEN_BYTES).decode("ascii")
    except UnicodeDecodeError as error:
        raise AcquisitionError("projected service-account token is not ASCII") from error
    if token != token.strip() or any(character.isspace() for character in token):
        raise AcquisitionError("projected service-account token is malformed")

    try:
        ca_data = _read_file(ca_file, MAX_CA_BYTES).decode("ascii")
    except UnicodeDecodeError as error:
        raise AcquisitionError("projected Kubernetes CA is not ASCII") from error
    try:
        context = ssl.create_default_context(cadata=ca_data)
    except (OSError, ssl.SSLError) as error:
        raise AcquisitionError(f"could not load projected Kubernetes CA: {error}") from error
    connection = http.client.HTTPSConnection(
        API_HOST,
        443,
        timeout=HTTP_TIMEOUT_SECONDS,
        context=context,
    )
    try:
        connection.request(
            "GET",
            API_PATH,
            headers={
                "Accept": "application/json",
                "Authorization": f"Bearer {token}",
            },
        )
        response = connection.getresponse()
        raw = _read_api_response(response)
    except (OSError, ssl.SSLError, http.client.HTTPException):
        raise AcquisitionError("Kubernetes API request failed") from None
    finally:
        connection.close()

    return _strict_json_object(raw, name="GitRepository")


def _artifact_status(document: Mapping[str, object]) -> git_markdown.ArtifactStatus:
    if document.get("apiVersion") != "source.toolkit.fluxcd.io/v1" or document.get("kind") != "GitRepository":
        raise AcquisitionError("Kubernetes API returned the wrong resource kind")
    metadata = _object(document.get("metadata"), name="GitRepository.metadata")
    if metadata.get("namespace") != "flux-system" or metadata.get("name") != "flux-system":
        raise AcquisitionError("Kubernetes API returned the wrong GitRepository")
    generation = _integer(metadata.get("generation"), name="metadata.generation", minimum=1, maximum=2**63 - 1)

    status = _object(document.get("status"), name="GitRepository.status")
    observed = _integer(
        status.get("observedGeneration"),
        name="status.observedGeneration",
        minimum=1,
        maximum=2**63 - 1,
    )
    if observed != generation:
        raise AcquisitionError("GitRepository status is stale")
    conditions = status.get("conditions")
    if not isinstance(conditions, list):
        raise AcquisitionError("GitRepository status.conditions must be an array")
    ready = [
        _object(condition, name="GitRepository Ready condition")
        for condition in conditions
        if isinstance(condition, dict) and condition.get("type") == "Ready"
    ]
    if len(ready) != 1 or ready[0].get("status") != "True" or ready[0].get("observedGeneration") != generation:
        raise AcquisitionError("GitRepository does not have one current Ready=True condition")

    artifact = _object(status.get("artifact"), name="GitRepository.status.artifact")
    result = git_markdown.ArtifactStatus(
        revision=_string(artifact.get("revision"), name="artifact.revision", maximum=255),
        digest=_string(artifact.get("digest"), name="artifact.digest", maximum=71),
        url=_string(artifact.get("url"), name="artifact.url", maximum=2048),
        size=_integer(
            artifact.get("size"),
            name="artifact.size",
            minimum=1,
            maximum=git_markdown.MAX_ARTIFACT_BYTES,
        ),
    )
    _digest_hex(result.digest, name="artifact.digest")
    return result


def _validated_source_url(value: str) -> SplitResult:
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError as error:
        raise AcquisitionError("artifact URL is malformed") from error
    if (
        parsed.scheme != "http"
        or parsed.hostname is None
        or parsed.hostname.rstrip(".") != SOURCE_HOST
        or port not in {None, 80}
        or parsed.username is not None
        or parsed.password is not None
        or not _canonical_source_path(parsed.path)
        or parsed.query
        or parsed.fragment
    ):
        raise AcquisitionError("artifact URL is not the exact in-cluster source-controller route")
    return parsed


def _canonical_source_path(value: str) -> bool:
    if not value.startswith(SOURCE_PATH_PREFIX):
        return False
    filename = value.removeprefix(SOURCE_PATH_PREFIX)
    if len(filename) > 255 or not filename.endswith(".tar.gz"):
        return False
    stem = filename.removesuffix(".tar.gz")
    if not stem or not stem.isascii() or not stem[0].isalnum():
        return False
    return all(character.isalnum() or character in "._-" for character in stem)


def _download_artifact(status: git_markdown.ArtifactStatus) -> bytes:
    parsed = _validated_source_url(status.url)
    host = cast(str, parsed.hostname)
    connection = http.client.HTTPConnection(host, parsed.port or 80, timeout=HTTP_TIMEOUT_SECONDS)
    try:
        connection.request("GET", parsed.path, headers={"Accept": "application/gzip, application/octet-stream"})
        response = connection.getresponse()
        if response.status != http.client.OK:
            raise AcquisitionError(f"source-controller returned HTTP {response.status}")
        raw = _read_response(response, status.size)
    except (OSError, http.client.HTTPException):
        raise AcquisitionError("source-controller request failed") from None
    finally:
        connection.close()
    if len(raw) != status.size:
        raise AcquisitionError("artifact byte length differs from Flux status")
    return raw


def _digest_hex(value: str, *, name: str) -> str:
    prefix = "sha256:"
    if not value.startswith(prefix):
        raise AcquisitionError(f"{name} must use sha256")
    digest = value.removeprefix(prefix)
    if (
        len(digest) != 64
        or digest.lower() != digest
        or any(character not in "0123456789abcdef" for character in digest)
    ):
        raise AcquisitionError(f"{name} must contain 64 lowercase hexadecimal characters")
    return digest


def _snapshot_revision_key(revision: str) -> str:
    return hashlib.sha256(revision.encode()).hexdigest()


def _fsync_directory(path: Path) -> None:
    descriptor = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def _ensure_directory(path: Path, mode: int) -> None:
    path.mkdir(parents=True, exist_ok=True, mode=mode)
    info = path.lstat()
    if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
        raise AcquisitionError(f"snapshot path is not a directory: {path.name}")
    path.chmod(mode)


def _write_new(path: Path, content: bytes, mode: int = FILE_MODE) -> None:
    flags = os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    descriptor = os.open(path, flags, mode)
    try:
        with os.fdopen(descriptor, "wb", closefd=False) as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.fchmod(descriptor, mode)
    finally:
        os.close(descriptor)


def _write_temporary(directory: Path, prefix: str, content: bytes, mode: int = FILE_MODE) -> Path:
    descriptor, temporary_name = tempfile.mkstemp(prefix=prefix, dir=directory)
    temporary = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "wb", closefd=False) as handle:
            handle.write(content)
            handle.flush()
            os.fsync(handle.fileno())
        os.fchmod(descriptor, mode)
    except BaseException:
        temporary.unlink(missing_ok=True)
        raise
    finally:
        os.close(descriptor)
    return temporary


def _read_exact(path: Path, maximum: int) -> bytes:
    flags = os.O_RDONLY | os.O_CLOEXEC
    if hasattr(os, "O_NOFOLLOW"):
        flags |= os.O_NOFOLLOW
    try:
        descriptor = os.open(path, flags)
    except OSError as error:
        raise AcquisitionError(f"could not open published path {path.name}: {error}") from error
    try:
        info = os.fstat(descriptor)
        if not stat.S_ISREG(info.st_mode) or not 1 <= info.st_size <= maximum:
            raise AcquisitionError(f"published path is not one bounded regular file: {path.name}")
        with os.fdopen(descriptor, "rb", closefd=False) as handle:
            content = handle.read(maximum + 1)
        if len(content) != info.st_size:
            raise AcquisitionError(f"published path changed while read: {path.name}")
        return content
    finally:
        os.close(descriptor)


def _publish_blob(blobs_root: Path, digest: str, content: bytes) -> None:
    digest_hex = _digest_hex(digest, name="source content digest")
    if hashlib.sha256(content).hexdigest() != digest_hex:
        raise AcquisitionError("source content differs from its connector digest")
    destination = blobs_root / digest_hex
    if destination.exists():
        if _read_exact(destination, git_markdown.MAX_SOURCE_BYTES) != content:
            raise AcquisitionError("existing content-addressed blob differs")
        return
    temporary = _write_temporary(
        blobs_root,
        f".pending-{digest_hex}-",
        content,
    )
    try:
        os.link(temporary, destination, follow_symlinks=False)
    except FileExistsError:
        if _read_exact(destination, git_markdown.MAX_SOURCE_BYTES) != content:
            raise AcquisitionError("racing content-addressed blob differs") from None
    finally:
        temporary.unlink(missing_ok=True)
    _fsync_directory(blobs_root)


def _replace_current(output_root: Path, content: bytes) -> None:
    temporary_current = _write_temporary(output_root, ".current-", content)
    try:
        temporary_current.replace(output_root / "current.json")
    finally:
        temporary_current.unlink(missing_ok=True)
    _fsync_directory(output_root)


def _publish_blocked(output_root: Path, status: git_markdown.ArtifactStatus) -> None:
    blocked = (
        json.dumps(
            {
                "connector_id": CONNECTOR_ID,
                "blocked": True,
                "snapshot_revision": status.revision,
                "artifact_digest": status.digest,
                "reason": "artifact-rejected",
            },
            ensure_ascii=False,
            sort_keys=True,
            separators=(",", ":"),
        ).encode()
        + b"\n"
    )
    _replace_current(output_root, blocked)


def _publish_snapshot(output_root: Path, connector: git_markdown.GitMarkdownConnector) -> None:
    inventory = git_markdown.inventory_json(connector)
    inventory_document = _object(
        json.loads(inventory.decode("utf-8"), object_pairs_hook=_strict_object, parse_constant=_reject_constant),
        name="connector inventory",
    )
    artifact_digest = _string(
        inventory_document.get("artifact_digest"),
        name="inventory.artifact_digest",
        maximum=71,
    )
    if artifact_digest != connector.artifact_digest:
        raise AcquisitionError("inventory artifact digest differs from connector")
    inventory_digest = _string(
        inventory_document.get("inventory_digest"),
        name="inventory.inventory_digest",
        maximum=71,
    )
    if inventory_digest != connector.cursor.digest:
        raise AcquisitionError("inventory digest differs from connector cursor")
    inventory_name = _digest_hex(inventory_digest, name="inventory digest")
    revision_name = _snapshot_revision_key(connector.cursor.revision)
    artifact_name = _digest_hex(artifact_digest, name="artifact digest")

    snapshots_root = output_root / "snapshots"
    blobs_root = output_root / "blobs"
    _ensure_directory(snapshots_root, WRITABLE_DIRECTORY_MODE)
    _ensure_directory(blobs_root, WRITABLE_DIRECTORY_MODE)
    inventory_root = snapshots_root / inventory_name
    _ensure_directory(inventory_root, WRITABLE_DIRECTORY_MODE)
    revision_root = inventory_root / revision_name
    _ensure_directory(revision_root, WRITABLE_DIRECTORY_MODE)
    _fsync_directory(snapshots_root)
    _fsync_directory(inventory_root)

    for reference in connector.enumerate_sources():
        source = connector.fetch_source(reference.source_id)
        _publish_blob(blobs_root, source.content_digest, source.content)

    # Git commits that do not alter selected documents keep the same canonical inventory digest.
    # Key each envelope by revision and artifact digest: source-controller may rebuild identical
    # selected bytes for the same Git revision with a different verified artifact digest.
    destination = revision_root / artifact_name
    if destination.exists():
        info = destination.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode):
            raise AcquisitionError("existing content-addressed snapshot is not a directory")
        if _read_exact(destination / "inventory.json", len(inventory)) != inventory:
            raise AcquisitionError("existing content-addressed snapshot inventory differs")
    else:
        pending = Path(tempfile.mkdtemp(prefix=".pending-", dir=revision_root))
        try:
            _write_new(pending / "inventory.json", inventory)
            pending.chmod(DIRECTORY_MODE)
            _fsync_directory(pending)
            pending.rename(destination)
            _fsync_directory(revision_root)
        except BaseException:
            shutil.rmtree(pending, ignore_errors=True)
            raise

    _replace_current(output_root, inventory)


def acquire(output_root: Path, token_file: Path, ca_file: Path) -> None:
    _ensure_directory(output_root, WRITABLE_DIRECTORY_MODE)
    lock_path = output_root / ".lock"
    with lock_path.open("a+b") as lock:
        lock_path.chmod(0o660)
        fcntl.flock(lock.fileno(), fcntl.LOCK_EX)
        document = _api_document(token_file, ca_file)
        status = _artifact_status(document)
        artifact = _download_artifact(status)
        if hashlib.sha256(artifact).hexdigest() != _digest_hex(
            status.digest,
            name="artifact.digest",
        ):
            raise AcquisitionError("downloaded artifact digest differs from Flux status")
        try:
            connector = git_markdown.GitMarkdownConnector.from_artifact(
                connector_id=CONNECTOR_ID,
                status=status,
                artifact=artifact,
            )
        except git_markdown.ConnectorError:
            _publish_blocked(output_root, status)
            raise
        _publish_snapshot(output_root, connector)


def _parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-root", type=Path, required=True)
    parser.add_argument("--token-file", type=Path, required=True)
    parser.add_argument("--ca-file", type=Path, required=True)
    return parser


def main(argv: Sequence[str] | None = None) -> int:
    args = _parser().parse_args(argv)
    acquire(args.output_root, args.token_file, args.ca_file)
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
