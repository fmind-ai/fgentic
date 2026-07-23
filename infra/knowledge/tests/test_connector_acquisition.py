"""Unit tests for the fail-closed Git Markdown acquisition boundary."""

from __future__ import annotations

import http.client
import importlib.util
import socketserver
import sys
import threading
from collections.abc import Iterator
from contextlib import contextmanager
from pathlib import Path
from types import ModuleType
from typing import ClassVar, override

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
    ],
    ids=[
        "duplicate-content-length",
        "content-length-and-transfer-encoding",
        "invalid-content-length",
        "oversized-content-length",
        "huge-content-length",
        "unsupported-transfer-encoding",
        "multiple-transfer-codings",
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


def test_response_reader_accepts_canonical_chunked_body() -> None:
    raw = _http_response(b"Transfer-Encoding: chunked\r\n", b"2\r\nok\r\n0\r\n\r\n")
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
