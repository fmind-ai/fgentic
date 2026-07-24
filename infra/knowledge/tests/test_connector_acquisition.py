"""Unit tests for the fail-closed Git Markdown acquisition boundary."""

from __future__ import annotations

import errno
import fcntl
import http.client
import importlib.util
import os
import socketserver
import ssl
import stat
import sys
import threading
import time
from collections.abc import Callable, Iterator
from contextlib import contextmanager
from pathlib import Path
from types import ModuleType
from typing import ClassVar, cast, override

import pytest


def _load_acquisition() -> ModuleType:
    path = Path(__file__).parents[1] / "connectors" / "git-markdown-runtime" / "fetch.py"
    spec = importlib.util.spec_from_file_location("git_markdown_acquisition", path)
    if spec is None or spec.loader is None:
        raise RuntimeError("could not load connector acquisition runtime")
    module = importlib.util.module_from_spec(spec)
    sys.modules[spec.name] = module
    spec.loader.exec_module(module)
    return module


acquisition = _load_acquisition()


class _RawResponseHandler(socketserver.BaseRequestHandler):
    response: ClassVar[bytes]

    @override
    def handle(self) -> None:
        self.request.recv(64 * 1024)
        self.request.sendall(self.response)


class _LoopbackServer(socketserver.ThreadingTCPServer):
    allow_reuse_address = True
    daemon_threads = True


class _SlowResponseHandler(socketserver.BaseRequestHandler):
    @override
    def handle(self) -> None:
        self.request.recv(64 * 1024)
        self.request.sendall(b"HTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\n")
        for byte in b"slow":
            time.sleep(0.04)
            try:
                self.request.sendall(bytes([byte]))
            except OSError:
                return


@contextmanager
def _response(raw: bytes) -> Iterator[http.client.HTTPResponse]:
    handler = type("RawResponseHandler", (_RawResponseHandler,), {"response": raw})
    server = _LoopbackServer(("127.0.0.1", 0), handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=2)
    try:
        connection.request("GET", "/")
        yield connection.getresponse()
    finally:
        connection.close()
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)


def _http_response(headers: bytes, body: bytes) -> bytes:
    return b"HTTP/1.1 200 OK\r\n" + headers + b"\r\n" + body


def _fifo_reader_error(fifo: Path, operation: Callable[[], object]) -> Exception:
    os.mkfifo(fifo)
    finished = threading.Event()
    errors: list[Exception] = []

    def read_fifo() -> None:
        try:
            operation()
        except Exception as error:
            errors.append(error)
        finally:
            finished.set()

    reader_thread = threading.Thread(target=read_fifo, daemon=True)
    reader_thread.start()
    if not finished.wait(timeout=1):
        try:
            descriptor = os.open(fifo, os.O_WRONLY | os.O_NONBLOCK)
        except OSError as error:
            if error.errno == errno.ENXIO:
                pytest.fail("projected-file reader did not establish a FIFO reader")
            raise
        try:
            os.write(descriptor, b"blocked")
        finally:
            os.close(descriptor)
        reader_thread.join(timeout=1)
        if reader_thread.is_alive():
            pytest.fail("projected-file reader remained blocked after FIFO wake-up")
        pytest.fail("projected-file reader blocked waiting for a FIFO writer")

    assert len(errors) == 1
    return errors[0]


@pytest.mark.parametrize("fifo_name", ["token", "ca.crt"])
def test_api_document_rejects_non_regular_projected_files_without_blocking(
    tmp_path: Path,
    fifo_name: str,
) -> None:
    token_file = tmp_path / "token"
    ca_file = tmp_path / "ca.crt"
    regular_file = ca_file if fifo_name == "token" else token_file
    regular_file.write_text("regular", encoding="ascii")
    fifo = tmp_path / fifo_name

    error = _fifo_reader_error(fifo, lambda: acquisition._api_document(token_file, ca_file))

    assert isinstance(error, acquisition.AcquisitionError)
    assert str(error) == f"projected file {fifo_name} must be a regular file"


def test_projected_file_reader_accepts_symlink_to_regular_file(tmp_path: Path) -> None:
    target = tmp_path / "..data" / "token"
    target.parent.mkdir()
    target.write_bytes(b"projected")
    projected = tmp_path / "token"
    projected.symlink_to(Path("..data/token"))

    assert acquisition._read_file(projected, 16) == b"projected"


def test_published_path_reader_rejects_fifo_without_blocking(tmp_path: Path) -> None:
    fifo = tmp_path / "published"

    error = _fifo_reader_error(fifo, lambda: acquisition._read_exact(fifo, 16))

    assert isinstance(error, acquisition.AcquisitionError)
    assert str(error) == "published path is not one bounded regular file: published"


def test_acquire_rejects_symlinked_lock_without_touching_target(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    output_root = tmp_path / "output"
    output_root.mkdir()
    target = tmp_path / "outside"
    target.write_bytes(b"preserve")
    target.chmod(0o600)
    (output_root / ".lock").symlink_to(target)
    requested = False

    def unexpected_request(_token: Path, _ca: Path) -> dict[str, object]:
        nonlocal requested
        requested = True
        raise AssertionError("Kubernetes API reached through a symlinked lock")

    monkeypatch.setattr(acquisition, "_api_document", unexpected_request)

    with pytest.raises(acquisition.AcquisitionError, match="lock must be a regular file"):
        acquisition.acquire(output_root, tmp_path / "token", tmp_path / "ca.crt")

    assert not requested
    assert target.read_bytes() == b"preserve"
    assert stat.S_IMODE(target.stat().st_mode) == 0o600


def test_acquire_rejects_fifo_lock_without_blocking(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    output_root = tmp_path / "output"
    output_root.mkdir()
    os.mkfifo(output_root / ".lock")
    requested = False

    def unexpected_request(_token: Path, _ca: Path) -> dict[str, object]:
        nonlocal requested
        requested = True
        raise AssertionError("Kubernetes API reached through a FIFO lock")

    monkeypatch.setattr(acquisition, "_api_document", unexpected_request)

    with pytest.raises(acquisition.AcquisitionError, match="lock must be a regular file"):
        acquisition.acquire(output_root, tmp_path / "token", tmp_path / "ca.crt")

    assert not requested


def test_acquisition_lock_remains_persistent_and_exclusive(tmp_path: Path) -> None:
    lock_path = tmp_path / ".lock"
    first = acquisition._open_lock(lock_path)
    try:
        fcntl.flock(first, fcntl.LOCK_EX)
        second = acquisition._open_lock(lock_path)
        try:
            with pytest.raises(BlockingIOError):
                fcntl.flock(second, fcntl.LOCK_EX | fcntl.LOCK_NB)
        finally:
            os.close(second)
    finally:
        os.close(first)

    assert lock_path.is_file()
    assert stat.S_IMODE(lock_path.stat().st_mode) == 0o660


def test_api_document_reads_bounded_ca_before_constructing_client(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
) -> None:
    token_file = tmp_path / "token"
    ca_file = tmp_path / "ca.crt"
    reads: list[tuple[Path, int]] = []

    def read_projected(path: Path, maximum: int) -> bytes:
        reads.append((path, maximum))
        return b"token" if path == token_file else b"PEM"

    def reject_context(*, cadata: str) -> None:
        assert cadata == "PEM"
        raise ssl.SSLError("invalid fixture CA")

    monkeypatch.setattr(acquisition, "_read_file", read_projected)
    monkeypatch.setattr(acquisition.ssl, "create_default_context", reject_context)

    with pytest.raises(acquisition.AcquisitionError, match="could not load projected Kubernetes CA"):
        acquisition._api_document(token_file, ca_file)

    assert reads == [
        (token_file, acquisition.MAX_TOKEN_BYTES),
        (ca_file, acquisition.MAX_CA_BYTES),
    ]


@pytest.mark.parametrize(
    ("headers", "body", "expected"),
    [
        (b"Content-Length: 2\r\nContent-Length: 2\r\n", b"ok", "HTTP response framing is ambiguous"),
        (
            b"Content-Length: 2\r\nTransfer-Encoding: chunked\r\n",
            b"2\r\nok\r\n0\r\n\r\n",
            "HTTP response framing is ambiguous",
        ),
        (b"Content-Length: invalid\r\n", b"ok", "HTTP response Content-Length is invalid"),
        (b"Content-Length: 5\r\n", b"overs", "HTTP response declares an oversized body"),
        (
            b"Content-Length: " + b"9" * 5000 + b"\r\n",
            b"",
            "HTTP response declares an oversized body",
        ),
        (b"Transfer-Encoding: gzip\r\n", b"secret", "HTTP response transfer coding is unsupported"),
        (
            b"Transfer-Encoding: chunked, gzip\r\n",
            b"secret",
            "HTTP response transfer coding is unsupported",
        ),
        (b"Transfer-Encoding: \vchunked\v\r\n", b"secret", "HTTP response transfer coding is unsupported"),
        (b"Transfer-Encoding: \fchunked\f\r\n", b"secret", "HTTP response transfer coding is unsupported"),
    ],
    ids=[
        "duplicate-content-length",
        "content-length-and-transfer-encoding",
        "invalid-content-length",
        "oversized-content-length",
        "huge-content-length",
        "unsupported-transfer-encoding",
        "multiple-transfer-codings",
        "vertical-tab-transfer-encoding",
        "form-feed-transfer-encoding",
    ],
)
def test_response_reader_rejects_ambiguous_or_unsupported_framing(
    headers: bytes,
    body: bytes,
    expected: str,
) -> None:
    with (
        _response(_http_response(headers, body)) as response,
        pytest.raises(acquisition.AcquisitionError) as caught,
    ):
        acquisition._read_response(response, 4)

    assert str(caught.value) == expected


def test_response_reader_accepts_exact_content_length() -> None:
    with _response(_http_response(b"Content-Length: 2\r\n", b"ok")) as response:
        assert acquisition._read_response(response, 2) == b"ok"


def test_request_deadline_aborts_a_slow_drip_response() -> None:
    server = _LoopbackServer(("127.0.0.1", 0), _SlowResponseHandler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    connection = http.client.HTTPConnection("127.0.0.1", server.server_address[1], timeout=1)
    started = time.monotonic()
    try:
        with pytest.raises(acquisition.AcquisitionError) as caught:
            acquisition._request_bytes(
                connection,
                "/",
                {"Accept": "application/octet-stream"},
                lambda response: acquisition._read_response(response, 4),
                operation="fixture",
                timeout_seconds=0.08,
            )
        elapsed = time.monotonic() - started
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=2)

    assert str(caught.value) == "fixture request exceeded its total deadline"
    assert elapsed < 0.5


def test_response_reader_accepts_content_length_with_optional_whitespace() -> None:
    with _response(_http_response(b"Content-Length: 2 \t\r\n", b"ok")) as response:
        assert acquisition._read_response(response, 2) == b"ok"


@pytest.mark.parametrize(
    "header",
    [b"Transfer-Encoding: chunked\r\n", b"Transfer-Encoding: \tchunked \t\r\n"],
    ids=["canonical", "optional-whitespace"],
)
def test_response_reader_decodes_chunked_body(header: bytes) -> None:
    raw = _http_response(header, b"2\r\nok\r\n0\r\n\r\n")
    with _response(raw) as response:
        assert acquisition._read_response(response, 2) == b"ok"


def test_response_reader_rejects_declared_truncation_without_reflecting_body() -> None:
    with (
        _response(_http_response(b"Content-Length: 12\r\n", b"secret")) as response,
        pytest.raises(acquisition.AcquisitionError) as caught,
    ):
        acquisition._read_response(response, 12)

    assert "secret" not in str(caught.value)


@pytest.mark.parametrize(
    "content_type_headers",
    [
        b"",
        b"Content-Type: application/json\r\nContent-Type: application/json\r\n",
        b"Content-Type: text/plain\r\n",
        b"Content-Type: application/json; charset\r\n",
        b"Content-Type: application/json; charset=\r\n",
        b"Content-Type: application/json; charset =utf-8\r\n",
        b'Content-Type: application/json; charset="utf-8\r\n',
        b"Content-Type: application/json;\r\n",
        b"Content-Type: application/json;;charset=utf-8\r\n",
        b"Content-Type: application/json; ; charset=utf-8\r\n",
    ],
    ids=[
        "missing",
        "duplicate",
        "non-json",
        "missing-parameter-value",
        "empty-parameter-value",
        "whitespace-before-equals",
        "unterminated-quoted-string",
        "trailing-semicolon",
        "consecutive-semicolons",
        "whitespace-only-parameter",
    ],
)
def test_api_response_rejects_invalid_media_type_before_body_read(
    monkeypatch: pytest.MonkeyPatch,
    content_type_headers: bytes,
) -> None:
    read = False

    def unexpected_read(_response: http.client.HTTPResponse, _maximum: int) -> bytes:
        nonlocal read
        read = True
        raise AssertionError("response body read for an invalid media type")

    monkeypatch.setattr(acquisition, "_read_response", unexpected_read)
    headers = content_type_headers + b"Content-Length: 6\r\n"
    with (
        _response(_http_response(headers, b"secret")) as response,
        pytest.raises(acquisition.AcquisitionError, match="unexpected media type"),
    ):
        acquisition._read_api_response(response)

    assert not read


def test_api_response_accepts_parameterized_json_media_type() -> None:
    headers = b'Content-Type: Application/JSON; charset="utf-8"; profile=flux\r\nContent-Length: 2\r\n'
    with _response(_http_response(headers, b"{}")) as response:
        assert acquisition._read_api_response(response) == b"{}"


@pytest.mark.parametrize(
    ("raw", "hostile"),
    [
        (b'{"safe":1,"hostile-duplicate-key":2,"hostile-duplicate-key":3}', "hostile-duplicate-key"),
        (b'{"value":hostile-constant}', "hostile-constant"),
        (b"[" * 10_000 + b"0" + b"]" * 10_000, "0"),
    ],
    ids=["duplicate-key", "invalid-constant", "recursive"],
)
def test_strict_json_errors_do_not_reflect_upstream_content(raw: bytes, hostile: str) -> None:
    with pytest.raises(acquisition.AcquisitionError) as caught:
        acquisition._strict_json_object(raw, name="GitRepository")

    assert str(caught.value) == "HTTP response is not strict UTF-8 JSON"
    assert hostile not in str(caught.value)


def test_strict_json_requires_an_object_root() -> None:
    with pytest.raises(acquisition.AcquisitionError, match="must be an object"):
        acquisition._strict_json_object(b"[]", name="GitRepository")


def _git_repository_document(revision: str) -> dict[str, object]:
    return {
        "apiVersion": "source.toolkit.fluxcd.io/v1",
        "kind": "GitRepository",
        "metadata": {
            "namespace": "flux-system",
            "name": "flux-system",
            "generation": 1,
        },
        "status": {
            "observedGeneration": 1,
            "conditions": [
                {
                    "type": "Ready",
                    "status": "True",
                    "observedGeneration": 1,
                }
            ],
            "artifact": {
                "revision": revision,
                "digest": f"sha256:{'0' * 64}",
                "url": (
                    "http://source-controller.flux-system.svc.cluster.local/"
                    "gitrepository/flux-system/flux-system/latest.tar.gz"
                ),
                "size": 1,
            },
        },
    }


def test_artifact_status_preserves_canonical_revision() -> None:
    revision = f"main@sha1:{'a' * 40}"

    assert acquisition._artifact_status(_git_repository_document(revision)).revision == revision


@pytest.mark.parametrize(
    "observed_generation",
    [True, False, 1.0, None],
    ids=["true", "false", "float", "missing"],
)
def test_acquire_rejects_non_integer_ready_generation_before_artifact_download(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    observed_generation: object,
) -> None:
    document = _git_repository_document(f"main@sha1:{'a' * 40}")
    status = cast(dict[str, object], document["status"])
    conditions = cast(list[dict[str, object]], status["conditions"])
    condition = conditions[0]
    condition["observedGeneration"] = observed_generation
    downloaded = False

    def unexpected_download(_status: object) -> bytes:
        nonlocal downloaded
        downloaded = True
        raise AssertionError("artifact download reached for a non-integer Ready generation")

    monkeypatch.setattr(acquisition, "_api_document", lambda _token, _ca: document)
    monkeypatch.setattr(acquisition, "_download_artifact", unexpected_download)

    with pytest.raises(acquisition.AcquisitionError) as caught:
        acquisition.acquire(tmp_path / "output", tmp_path / "token", tmp_path / "ca")

    assert str(caught.value) == "Ready condition.observedGeneration must be an integer in 1..9223372036854775807"
    assert str(observed_generation) not in str(caught.value)
    assert not downloaded


@pytest.mark.parametrize(
    "conditions",
    [
        [],
        [{"type": "Ready", "status": "True", "observedGeneration": 2}],
    ],
    ids=["missing", "stale"],
)
def test_artifact_status_rejects_missing_or_stale_ready_condition(conditions: list[object]) -> None:
    document = _git_repository_document(f"main@sha1:{'a' * 40}")
    status = cast(dict[str, object], document["status"])
    status["conditions"] = conditions

    with pytest.raises(acquisition.AcquisitionError) as caught:
        acquisition._artifact_status(document)

    assert str(caught.value) == "GitRepository does not have one current Ready=True condition"


@pytest.mark.parametrize(
    "revision",
    [
        f"main@sha1:{'a' * 39}\ud800",
        f"main@sha1:{'a' * 39}\x7f",
        f"main@sha1:{'a' * 39}\u202e",
        f"ma\u0301in@sha1:{'a' * 40}",
    ],
    ids=["lone-surrogate", "delete-control", "bidi-format", "non-nfc"],
)
def test_acquire_rejects_unclean_revision_before_artifact_download(
    monkeypatch: pytest.MonkeyPatch,
    tmp_path: Path,
    revision: str,
) -> None:
    downloaded = False

    def unexpected_download(_status: object) -> bytes:
        nonlocal downloaded
        downloaded = True
        raise AssertionError("artifact download reached for an invalid revision")

    monkeypatch.setattr(acquisition, "_api_document", lambda _token, _ca: _git_repository_document(revision))
    monkeypatch.setattr(acquisition, "_download_artifact", unexpected_download)

    with pytest.raises(acquisition.AcquisitionError) as caught:
        acquisition.acquire(tmp_path / "output", tmp_path / "token", tmp_path / "ca")

    assert str(caught.value) == "artifact.revision is not bounded clean text"
    assert revision not in str(caught.value)
    assert not downloaded


@pytest.mark.parametrize(
    "url",
    [
        "http://source-controller.flux-system.svc.cluster.local/gitrepository/flux-system/flux-system/latest.tar.gz",
        "http://source-controller.flux-system.svc.cluster.local.:80"
        "/gitrepository/flux-system/flux-system/0123456789abcdef0123456789abcdef01234567.tar.gz",
        "http://source-controller.flux-system.svc.cluster.local/gitrepository/flux-system/flux-system/"
        f"{'a' * 248}.tar.gz",
    ],
)
def test_source_url_accepts_canonical_artifact_paths(url: str) -> None:
    assert acquisition._validated_source_url(url).geturl() == url


@pytest.mark.parametrize(
    "suffix",
    [
        "../private-partner-redacted.tar.gz",
        "./private-partner-redacted.tar.gz",
        "%2e%2e/private-partner-redacted.tar.gz",
        "nested/private-partner-redacted.tar.gz",
        "/private-partner-redacted.tar.gz",
        "private%2fpartner-redacted.tar.gz",
        r"private\partner-redacted.tar.gz",
        "private partner-redacted.tar.gz",
        "privaté-partner-redacted.tar.gz",
        "private:partner-redacted.tar.gz",
        "private-partner-redacted-\x7f.tar.gz",
        f"{'a' * 249}.tar.gz",
        ".tar.gz",
    ],
)
def test_download_rejects_noncanonical_artifact_paths_before_http_construction(
    monkeypatch: pytest.MonkeyPatch,
    suffix: str,
) -> None:
    constructed = False

    def unexpected_connection(*_args: object, **_kwargs: object) -> None:
        nonlocal constructed
        constructed = True
        raise AssertionError("HTTP connection constructed for an invalid artifact path")

    monkeypatch.setattr(acquisition.http.client, "HTTPConnection", unexpected_connection)
    status = acquisition.git_markdown.ArtifactStatus(
        revision="main@sha1:0123456789abcdef",
        digest=f"sha256:{'0' * 64}",
        url=(f"http://source-controller.flux-system.svc.cluster.local/gitrepository/flux-system/flux-system/{suffix}"),
        size=1,
    )

    with pytest.raises(acquisition.AcquisitionError) as caught:
        acquisition._download_artifact(status)

    assert str(caught.value) == "artifact URL is not the exact in-cluster source-controller route"
    assert "private-partner-redacted" not in str(caught.value)
    assert not constructed
