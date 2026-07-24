#!/usr/bin/env python3
"""Sovereign Alertmanager -> Matrix webhook receiver (issue #456).

A self-hosted stdlib HTTP receiver — no external image, no license question, no SaaS. Alertmanager
POSTs its grouped webhook here; the receiver posts ONE bounded, content-free `m.notice` per group to
a dedicated ops room, as a plain `@alertbot` user that is in no agent's `allowedSenders` — so an
alert can never invoke an agent (D7/D8, no alert-storm -> LLM amplification).

Content-free by construction: only the alert name, severity, firing/resolved status, the namespace
and a bounded set of low-cardinality labels, a count, and the generator link are forwarded — never
alert annotation prose, Matrix event content, prompts, or secrets. The Matrix access token comes
from the per-cluster SOPS Secret; the receiver holds no other credential.

Standard library only, so it runs from the already-pinned python:3.14-slim image.
"""

from __future__ import annotations

import hashlib
import http.client
import http.server
import ipaddress
import json
import os
import queue
import re
import socket
import sys
import threading
import time
import unicodedata
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, NoReturn, cast

# Low-cardinality, non-content labels safe to surface (never message text / user identifiers).
_SAFE_LABELS = ("namespace", "job_name", "cronjob", "gen_ai_system", "resource_kind")
_MAX_ALERTS = 20  # bound the fan-out so an alert storm becomes a bounded stream, never a flood.
# Leave ample room for the Matrix event envelope below the homeserver's request/event limits.
_MAX_NOTICE_BYTES = 16_384
_MAX_MATRIX_RESPONSE_BYTES = 16_384
_MAX_STATUS_BYTES = 16
_MAX_ALERT_NAME_BYTES = 128
_MAX_SEVERITY_BYTES = 32
_MAX_LABEL_VALUE_BYTES = 128
_MAX_GENERATOR_URL_BYTES = 2_048
_ALERT_STATUSES = frozenset({"firing", "resolved"})
_STATUS_ICONS = {"firing": "🔔", "resolved": "✅", "unknown": "⚠️"}
_UNSAFE_TEXT_CATEGORIES = frozenset({"Cc", "Cf", "Cs", "Co", "Cn", "Zl", "Zp"})
# Four full header sets stay small beside the interpreter baseline in the 64 MiB container.
_MAX_HEADER_BYTES = 16_384
_MAX_REQUEST_BYTES = 65_536
_MAX_CONCURRENT_REQUESTS = 4
_MAX_CONCURRENT_MATRIX_REQUESTS = 4
_REQUEST_TIMEOUT_SECONDS = 5.0
_MATRIX_REQUEST_TIMEOUT_SECONDS = 15.0
_MATRIX_REQUEST_SLOTS = threading.BoundedSemaphore(_MAX_CONCURRENT_MATRIX_REQUESTS)
_DEFAULT_LISTEN_PORT = 9_095
_MIN_LISTEN_PORT = 1_024
_MAX_LISTEN_PORT = 65_535
_HTTP_TOKEN = r"[!#$%&'*+\-.^_`|~0-9A-Za-z]+"
_HTTP_QUOTED_STRING = r'"(?:[\t !#-\[\]-~\x80-\xff]|\\[\t !-~\x80-\xff])*"'
_MEDIA_TYPE = re.compile(
    rf"^[ \t]*(?P<type>{_HTTP_TOKEN})/(?P<subtype>{_HTTP_TOKEN})"
    rf"(?:[ \t]*;[ \t]*(?:{_HTTP_TOKEN}=(?:{_HTTP_TOKEN}|{_HTTP_QUOTED_STRING}))?)*[ \t]*$"
)
_HOST_LABEL = re.compile(r"^[A-Za-z0-9](?:[-A-Za-z0-9]{0,61}[A-Za-z0-9])?$")
_LISTEN_PORT_ERROR = (
    f"ALERTBOT_LISTEN_PORT must be a canonical ASCII integer from {_MIN_LISTEN_PORT} to {_MAX_LISTEN_PORT}"
)


class MatrixResponseError(Exception):
    """The Matrix send response violated the receiver's bounded framing contract."""


def _is_json_content_type(value: str) -> bool:
    match = _MEDIA_TYPE.fullmatch(value)
    return (
        match is not None
        and match.group("type").lower() == "application"
        and match.group("subtype").lower() == "json"
    )


def _has_supported_transfer_coding(values: list[Any]) -> bool:
    return not values or (
        len(values) == 1
        and isinstance(values[0], str)
        and values[0].lower() == "chunked"
    )


def _env(name: str, default: str | None = None) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        if default is not None:
            return default
        print(f"alert-receiver: required environment variable {name} is missing", file=sys.stderr)
        raise SystemExit(1)
    return value


def _invalid_listen_port() -> NoReturn:
    print(f"alert-receiver: {_LISTEN_PORT_ERROR}", file=sys.stderr)
    raise SystemExit(1)


def _listen_port() -> int:
    raw = os.environ.get("ALERTBOT_LISTEN_PORT")
    if raw is None:
        return _DEFAULT_LISTEN_PORT
    if not raw.isascii() or not raw.isdecimal():
        _invalid_listen_port()

    # Normalize before int() so an oversized decimal cannot hit Python's digit limit.
    normalized = raw.lstrip("0") or "0"
    if len(normalized) > len(str(_MAX_LISTEN_PORT)):
        _invalid_listen_port()
    port = int(normalized)
    if raw != str(port) or not _MIN_LISTEN_PORT <= port <= _MAX_LISTEN_PORT:
        _invalid_listen_port()
    return port


def _clean_scalar(value: object, *, fallback: str, maximum: int) -> str:
    if not isinstance(value, str):
        return fallback
    try:
        normalized = unicodedata.normalize("NFC", value)
        encoded = normalized.encode("utf-8")
    except UnicodeError:
        return fallback
    if (
        normalized != value
        or not normalized
        or normalized != normalized.strip()
        or len(encoded) > maximum
        or any(unicodedata.category(character) in _UNSAFE_TEXT_CATEGORIES for character in normalized)
    ):
        return fallback
    return normalized


def _clean_status(value: object, *, fallback: str) -> str:
    status = _clean_scalar(value, fallback=fallback, maximum=_MAX_STATUS_BYTES)
    return status if status in _ALERT_STATUSES else fallback


def _safe_label_summary(labels: dict) -> str:
    parts = []
    for key in _SAFE_LABELS:
        value = _clean_scalar(labels.get(key), fallback="", maximum=_MAX_LABEL_VALUE_BYTES)
        if value:
            parts.append(f"{key}={value}")
    return " ".join(parts)


def _valid_hostname(value: str) -> bool:
    try:
        ipaddress.ip_address(value)
        return True
    except ValueError:
        pass
    hostname = value.removesuffix(".")
    labels = hostname.split(".")
    return (
        bool(hostname)
        and len(hostname.encode("ascii")) <= 253
        and all(_HOST_LABEL.fullmatch(label) is not None for label in labels)
    )


def _generator_link(value: object) -> str:
    link = _clean_scalar(value, fallback="", maximum=_MAX_GENERATOR_URL_BYTES)
    if not link or not link.isascii() or any(character.isspace() for character in link) or "\\" in link:
        return ""
    try:
        parsed = urllib.parse.urlsplit(link)
        hostname = parsed.hostname
        port = parsed.port
        username = parsed.username
        password = parsed.password
    except ValueError:
        return ""
    if (
        parsed.scheme not in {"http", "https"}
        or not parsed.netloc
        or hostname is None
        or not _valid_hostname(hostname)
        or username is not None
        or password is not None
        or port == 0
        or parsed.geturl() != link
    ):
        return ""
    return link


def _render(payload: dict) -> str:
    """Build one bounded, content-free notice for an Alertmanager group webhook."""
    status = _clean_status(payload.get("status"), fallback="unknown")
    alerts = payload.get("alerts", [])
    if not isinstance(alerts, list):
        alerts = []
    icon = _STATUS_ICONS[status]
    header = f"{icon} Alertmanager: {status} ({len(alerts)} alert(s))"
    alert_lines = []
    for alert in alerts[:_MAX_ALERTS]:
        if not isinstance(alert, dict):
            continue  # tolerate a malformed element without dropping the whole delivery
        labels = alert.get("labels", {}) if isinstance(alert.get("labels"), dict) else {}
        name = _clean_scalar(labels.get("alertname", "unknown"), fallback="unknown", maximum=_MAX_ALERT_NAME_BYTES)
        severity = _clean_scalar(labels.get("severity", "none"), fallback="unknown", maximum=_MAX_SEVERITY_BYTES)
        summary = _safe_label_summary(labels)
        link = _generator_link(alert.get("generatorURL", ""))
        alert_status = _clean_status(alert.get("status"), fallback=status)
        piece = f"• [{alert_status}] {name} ({severity})"
        if summary:
            piece += f" {summary}"
        if link:
            piece += f" — {link}"
        alert_lines.append(piece)

    count_omitted = max(0, len(alerts) - _MAX_ALERTS)
    for included in range(len(alert_lines), -1, -1):
        byte_omitted = len(alert_lines) - included
        lines = [header, *alert_lines[:included]]
        if count_omitted:
            lines.append(f"… and {count_omitted} more")
        if byte_omitted:
            lines.append(f"… {byte_omitted} alert(s) omitted by notice byte limit")
        notice = "\n".join(lines)
        if len(notice.encode("utf-8")) <= _MAX_NOTICE_BYTES:
            return notice
    raise AssertionError("alert notice header exceeds its fixed byte bound")


def _validate_matrix_response(response: Any) -> None:
    if response.status != 200:
        raise MatrixResponseError

    content_types = response.headers.get_all("Content-Type", [])
    if (
        len(content_types) != 1
        or not isinstance(content_types[0], str)
        or not _is_json_content_type(content_types[0])
    ):
        raise MatrixResponseError

    content_lengths = response.headers.get_all("Content-Length", [])
    transfer_encodings = response.headers.get_all("Transfer-Encoding", [])
    if (
        len(content_lengths) > 1
        or not _has_supported_transfer_coding(transfer_encodings)
        or (content_lengths and transfer_encodings)
    ):
        raise MatrixResponseError

    declared_length: int | None = None
    if content_lengths:
        raw_length = content_lengths[0]
        if not raw_length.isascii() or not raw_length.isdecimal():
            raise MatrixResponseError
        normalized_length = raw_length.lstrip("0") or "0"
        if len(normalized_length) > len(str(_MAX_MATRIX_RESPONSE_BYTES)):
            raise MatrixResponseError
        declared_length = int(normalized_length)
        if declared_length > _MAX_MATRIX_RESPONSE_BYTES:
            raise MatrixResponseError

    body = response.read(_MAX_MATRIX_RESPONSE_BYTES + 1)
    if len(body) > _MAX_MATRIX_RESPONSE_BYTES:
        raise MatrixResponseError
    if declared_length is not None and len(body) != declared_length:
        raise MatrixResponseError
    try:
        payload = json.loads(body)
    except (RecursionError, ValueError):
        raise MatrixResponseError from None
    if not isinstance(payload, dict):
        raise MatrixResponseError
    event_id = payload.get("event_id")
    if not isinstance(event_id, str) or not event_id:
        raise MatrixResponseError


def _post_notice_io(homeserver: str, token: str, room_id: str, body: str) -> None:
    encoded_room = urllib.parse.quote(room_id, safe="")
    # Transaction id = deterministic body digest + a coarse (5-min) time bucket. Alertmanager retries
    # a failed webhook within seconds, so a retry of the SAME delivery lands in the same bucket ->
    # identical txn -> Matrix dedups it (idempotent). A genuine repeat reminder (repeatInterval, hours
    # later) falls in a different bucket -> new txn -> delivered. hashlib (not builtin hash()) keeps
    # the digest stable across pod restarts, where str hashing is PYTHONHASHSEED-randomized.
    # SHA-1 is an idempotency digest here, never a security or integrity primitive.
    digest = hashlib.sha1(body.encode()).hexdigest()[:12]
    bucket = int(time.time()) // 300
    txn = f"alert-{digest}-{bucket}"
    url = f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/send/m.room.message/{txn}"
    data = json.dumps({"msgtype": "m.notice", "body": body}).encode()
    request = urllib.request.Request(url, data=data, method="PUT")
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {token}")
    # The homeserver URL is an operator-owned ConfigMap value and NetworkPolicy permits only Synapse.
    with urllib.request.urlopen(request, timeout=_MATRIX_REQUEST_TIMEOUT_SECONDS) as response:
        _validate_matrix_response(response)


def _post_notice(homeserver: str, token: str, room_id: str, body: str) -> None:
    if not _MATRIX_REQUEST_SLOTS.acquire(blocking=False):
        raise TimeoutError

    outcomes: queue.Queue[BaseException | None] = queue.Queue(maxsize=1)

    def deliver() -> None:
        outcome: BaseException | None = None
        try:
            _post_notice_io(homeserver, token, room_id, body)
        except BaseException as error:
            # Preserve the original exception type across the worker boundary without a traceback.
            outcome = error
        finally:
            _MATRIX_REQUEST_SLOTS.release()
        # Publish only after capacity is visible again so the next webhook cannot observe a stale
        # saturation failure after this delivery has already completed.
        outcomes.put(outcome)

    # urllib's timeout is per blocking socket operation. A bounded daemon worker gives the caller a
    # wall-clock deadline without letting repeated timed-out I/O accumulate unbounded threads.
    worker = threading.Thread(target=deliver, daemon=True)
    try:
        worker.start()
    except RuntimeError as error:
        _MATRIX_REQUEST_SLOTS.release()
        raise MatrixResponseError from error

    try:
        outcome = outcomes.get(timeout=_MATRIX_REQUEST_TIMEOUT_SECONDS)
    except queue.Empty:
        raise TimeoutError from None
    if isinstance(outcome, BaseException):
        raise outcome


class _BoundedThreadingHTTPServer(http.server.ThreadingHTTPServer):
    homeserver = ""
    token = ""
    room_id = ""

    def __init__(
        self,
        server_address: tuple[str, int],
        handler_class: type[http.server.BaseHTTPRequestHandler],
    ) -> None:
        super().__init__(server_address, handler_class)
        self._request_slots = threading.BoundedSemaphore(_MAX_CONCURRENT_REQUESTS)

    def process_request(self, request: Any, client_address: Any) -> None:
        # Acquire before ThreadingMixIn allocates a thread so excess connections stay bounded by
        # this semaphore and the kernel listen backlog rather than process memory.
        self._request_slots.acquire()
        try:
            super().process_request(request, client_address)
        except BaseException:
            self._request_slots.release()
            raise

    def process_request_thread(self, request: Any, client_address: Any) -> None:
        try:
            super().process_request_thread(request, client_address)
        finally:
            self._request_slots.release()


class _HeadersTooLargeError(Exception):
    pass


class _BoundedHeaderReader:
    def __init__(
        self,
        stream: Any,
        connection: socket.socket,
        deadline: float,
    ) -> None:
        self._stream = stream
        self._connection = connection
        self._deadline = deadline
        self._received = 0

    def readline(self, limit: int = -1) -> bytes:
        line = bytearray()
        remaining_budget = _MAX_HEADER_BYTES - self._received
        read_limit = remaining_budget + 1
        if limit >= 0:
            read_limit = min(read_limit, limit)

        while len(line) < read_limit:
            remaining_time = self._deadline - time.monotonic()
            if remaining_time <= 0:
                raise TimeoutError
            self._connection.settimeout(remaining_time)
            chunk = self._stream.read1(1)
            if not chunk:
                break
            line.extend(chunk)
            self._received += len(chunk)
            if self._received > _MAX_HEADER_BYTES:
                raise _HeadersTooLargeError
            if chunk == b"\n":
                break
        return bytes(line)


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    timeout = _REQUEST_TIMEOUT_SECONDS

    def log_message(self, format: str, *args: Any) -> None:
        # Content-free: never log request bodies.
        pass

    def _reply(self, code: int) -> None:
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self) -> None:
        # Liveness/readiness only.
        self._reply(200 if self.path == "/healthz" else 404)

    def handle_one_request(self) -> None:
        self._request_deadline = time.monotonic() + _REQUEST_TIMEOUT_SECONDS
        self.requestline = ""
        self.request_version = ""
        self.command = ""
        stream = self.rfile
        self.rfile = cast(Any, _BoundedHeaderReader(stream, self.connection, self._request_deadline))
        try:
            self.raw_requestline = self.rfile.readline(65_537)
            if len(self.raw_requestline) > 65_536:
                self.requestline = ""
                self.request_version = ""
                self.command = ""
                self.send_error(414)
                return
            if not self.raw_requestline:
                self.close_connection = True
                return
            if not self.parse_request():
                return
        except _HeadersTooLargeError:
            self.close_connection = True
            self.send_error(431)
            return
        except TimeoutError:
            self.close_connection = True
            return
        finally:
            self.rfile = stream
            self.connection.settimeout(self.timeout)

        method = getattr(self, f"do_{self.command}", None)
        if method is None:
            self.send_error(501, f"Unsupported method ({self.command!r})")
            return
        try:
            method()
            self.wfile.flush()
        except TimeoutError:
            self.close_connection = True

    def _request_size(self) -> int | None:
        content_lengths = self.headers.get_all("Content-Length", [])
        if not content_lengths:
            self.close_connection = True
            self._reply(411)
            return None
        if (
            len(content_lengths) != 1
            or not content_lengths[0].isascii()
            or not content_lengths[0].isdecimal()
            or bool(self.headers.get_all("Transfer-Encoding", []))
        ):
            self.close_connection = True
            self._reply(400)
            return None

        # Normalize before int() so a huge decimal header cannot hit Python's digit limit.
        normalized_length = content_lengths[0].lstrip("0") or "0"
        if len(normalized_length) > len(str(_MAX_REQUEST_BYTES)):
            self.close_connection = True
            self._reply(413)
            return None
        size = int(normalized_length)
        if size > _MAX_REQUEST_BYTES:
            self.close_connection = True
            self._reply(413)
            return None
        return size

    def _read_body(self, size: int) -> bytes | None:
        body = bytearray(size)
        view = memoryview(body)
        received = 0
        try:
            while received < size:
                remaining = self._request_deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError
                self.connection.settimeout(remaining)
                read = self.rfile.readinto1(view[received:])
                if read == 0:
                    self.close_connection = True
                    self._reply(400)
                    return None
                received += read
        except TimeoutError:
            self.close_connection = True
            self._reply(408)
            return None
        finally:
            self.connection.settimeout(self.timeout)
        return bytes(body)

    def _has_json_content_type(self) -> bool:
        content_types = self.headers.get_all("Content-Type", [])
        if (
            len(content_types) == 1
            and isinstance(content_types[0], str)
            and _is_json_content_type(content_types[0])
        ):
            return True
        self.close_connection = True
        self._reply(415)
        return False

    def do_POST(self) -> None:
        size = self._request_size()
        if size is None or not self._has_json_content_type():
            return
        raw = self._read_body(size)
        if raw is None:
            return
        try:
            payload = json.loads(raw or b"{}")
        except (RecursionError, ValueError):
            self._reply(400)
            return
        if not isinstance(payload, dict):
            self._reply(400)
            return
        server = cast(_BoundedThreadingHTTPServer, self.server)
        try:
            _post_notice(server.homeserver, server.token, server.room_id, _render(payload))
        except (
            MatrixResponseError,
            http.client.HTTPException,
            urllib.error.URLError,
            TimeoutError,
            ValueError,
        ) as error:
            # Fail visibly to Alertmanager (it retries) but keep the log content-free.
            print(f"alert-receiver: delivery failed: {type(error).__name__}", file=sys.stderr)
            self._reply(502)
            return
        self._reply(200)


def main() -> int:
    port = _listen_port()
    homeserver = _env("ALERTBOT_HOMESERVER_URL").rstrip("/")
    token = _env("ALERTBOT_ACCESS_TOKEN")
    room_id = _env("ALERTBOT_OPS_ROOM_ID")
    # NetworkPolicy restricts this in-cluster listener to the Alertmanager namespace.
    server = _BoundedThreadingHTTPServer(("0.0.0.0", port), _Handler)
    server.homeserver = homeserver
    server.token = token
    server.room_id = room_id
    print(f"alert-receiver: listening on :{port}, delivering to the ops room")
    server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
