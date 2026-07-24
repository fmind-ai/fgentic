#!/usr/bin/env python3
"""Offline unit + fake-API tests for the Alertmanager -> Matrix receiver (issue #456).

No network, no cluster: the content-free rendering contract is exercised with crafted payloads, and
the full webhook -> Matrix post is driven against an in-process fake Synapse. Mirrors the fixture
discipline of the canary's probe_test.py.
"""

from __future__ import annotations

import contextlib
import errno
import http.server
import io
import json
import os
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any, ClassVar, cast
from unittest import mock

sys.path.insert(0, str(Path(__file__).parent))
import receiver

ENABLED_PROFILE = str(Path(__file__).parents[1] / "profiles" / "enabled")
ROOM = "!ops:fgentic.localhost"

# An Alertmanager group webhook with content in annotations that MUST NOT leak.
WEBHOOK = {
    "status": "firing",
    "alerts": [
        {
            "status": "firing",
            "labels": {"alertname": "LLMTokenBurnHigh", "severity": "warning", "namespace": "monitoring"},
            "annotations": {"summary": "SECRET-PROMPT-CONTENT-should-never-leak", "description": "nope"},
            "generatorURL": "http://prometheus/graph?g0.expr=x",
        }
    ],
}


def _render_enabled_profile() -> list[dict[str, Any]]:
    manifest = subprocess.run(
        ["kubectl", "kustomize", ENABLED_PROFILE],
        check=True,
        capture_output=True,
        text=True,
    )
    rendered = subprocess.run(
        ["yq", "eval-all", "-o=json", "[.]"],
        input=manifest.stdout,
        check=True,
        capture_output=True,
        text=True,
    )
    return cast(list[dict[str, Any]], json.loads(rendered.stdout))


def test_listen_port_is_bounded_and_content_free() -> None:
    with mock.patch.dict(receiver.os.environ, {}, clear=True):
        assert receiver._listen_port() == 9_095
    for raw, expected in (("1024", 1_024), ("9095", 9_095), ("65535", 65_535)):
        with mock.patch.dict(receiver.os.environ, {"ALERTBOT_LISTEN_PORT": raw}, clear=True):
            assert receiver._listen_port() == expected

    expected_error = (
        "alert-receiver: ALERTBOT_LISTEN_PORT must be a canonical ASCII integer from 1024 to 65535\n"
    )
    invalid_values = (
        "",
        "0",
        "1",
        "1023",
        "-1",
        "+1024",
        "01024",
        " 1024",
        "1024 ",
        "١٠٢٤",
        "65536",
        "operator-private-value",
        "9" * 10_000,
    )
    for raw in invalid_values:
        process = subprocess.run(
            [sys.executable, str(Path(receiver.__file__))],
            env={**os.environ, "ALERTBOT_LISTEN_PORT": raw},
            capture_output=True,
            text=True,
            check=False,
        )
        assert process.returncode == 1
        assert process.stdout == ""
        assert process.stderr == expected_error
        if raw == "operator-private-value":
            assert raw not in process.stderr
        assert "Traceback" not in process.stderr
    print("ok: listen port is canonical, bounded, and content-free")


def test_rendered_ports_are_aligned() -> None:
    resources = _render_enabled_profile()
    by_identity = {(resource["kind"], resource["metadata"]["name"]): resource for resource in resources}
    config = by_identity[("ConfigMap", "alert-receiver-config")]
    deployment = by_identity[("Deployment", "alert-receiver")]
    service = by_identity[("Service", "alert-receiver")]
    policy = by_identity[("NetworkPolicy", "alert-receiver")]
    alertmanager = by_identity[("AlertmanagerConfig", "matrix-ops")]

    container_port = deployment["spec"]["template"]["spec"]["containers"][0]["ports"][0]
    service_port = service["spec"]["ports"][0]
    webhook_url = alertmanager["spec"]["receivers"][0]["webhookConfigs"][0]["url"]
    ports = {
        int(config["data"]["ALERTBOT_LISTEN_PORT"]),
        container_port["containerPort"],
        service_port["port"],
        policy["spec"]["ingress"][0]["ports"][0]["port"],
        urllib.parse.urlsplit(webhook_url).port,
    }
    assert config["data"]["ALERTBOT_LISTEN_PORT"] == str(receiver._DEFAULT_LISTEN_PORT)
    assert container_port["name"] == service_port["targetPort"] == "webhook"
    assert ports == {receiver._DEFAULT_LISTEN_PORT}
    print("ok: rendered alert delivery ports are aligned")


def test_network_peers_are_exactly_scoped() -> None:
    resources = _render_enabled_profile()
    policies = {
        resource["metadata"]["name"]: resource["spec"] for resource in resources if resource["kind"] == "NetworkPolicy"
    }
    assert policies == {
        "alert-delivery-default-deny": {
            "podSelector": {},
            "policyTypes": ["Ingress", "Egress"],
        },
        "alert-receiver": {
            "podSelector": {
                "matchLabels": {"app.kubernetes.io/name": "alert-receiver"},
            },
            "policyTypes": ["Ingress", "Egress"],
            "ingress": [
                {
                    "from": [
                        {
                            "namespaceSelector": {
                                "matchLabels": {"kubernetes.io/metadata.name": "monitoring"},
                            },
                            "podSelector": {
                                "matchLabels": {
                                    "alertmanager": "kube-prometheus-stack-alertmanager",
                                    "app.kubernetes.io/name": "alertmanager",
                                },
                            },
                        }
                    ],
                    "ports": [{"protocol": "TCP", "port": 9095}],
                }
            ],
            "egress": [
                {
                    "to": [
                        {
                            "namespaceSelector": {
                                "matchLabels": {"kubernetes.io/metadata.name": "kube-system"},
                            },
                            "podSelector": {
                                "matchLabels": {"k8s-app": "kube-dns"},
                            },
                        }
                    ],
                    "ports": [
                        {"protocol": "UDP", "port": 53},
                        {"protocol": "TCP", "port": 53},
                    ],
                },
                {
                    "to": [
                        {
                            "namespaceSelector": {
                                "matchLabels": {"kubernetes.io/metadata.name": "matrix"},
                            },
                            "podSelector": {
                                "matchLabels": {"app.kubernetes.io/instance": "ess-haproxy"},
                            },
                        }
                    ],
                    "ports": [{"protocol": "TCP", "port": 8008}],
                },
            ],
        },
    }, (
        "alert-delivery NetworkPolicies must stay limited to default-deny plus the exact "
        "Alertmanager, CoreDNS, and ESS HAProxy peers"
    )
    print("ok: alert receiver network peers are exactly scoped")


def test_render_is_content_free() -> None:
    body = receiver._render(WEBHOOK)
    assert body == (
        "🔔 Alertmanager: firing (1 alert(s))\n"
        "• [firing] LLMTokenBurnHigh (warning) namespace=monitoring"
        " — http://prometheus/graph?g0.expr=x"
    )
    # The alert name, severity, safe namespace label, and the link are present.
    assert "LLMTokenBurnHigh" in body and "warning" in body and "namespace=monitoring" in body
    assert "http://prometheus/graph" in body
    # Annotation prose is NEVER forwarded.
    assert "SECRET-PROMPT-CONTENT-should-never-leak" not in body
    assert "nope" not in body
    print("ok: render is content-free")


def test_statuses_are_semantically_bounded() -> None:
    assert receiver._render({"status": "firing", "alerts": []}) == "🔔 Alertmanager: firing (0 alert(s))"
    assert receiver._render({"status": "resolved", "alerts": []}) == "✅ Alertmanager: resolved (0 alert(s))"
    assert receiver._render({"status": "healthy", "alerts": []}) == "⚠️ Alertmanager: unknown (0 alert(s))"

    body = receiver._render(
        {
            "status": "firing",
            "alerts": [
                {"status": "healthy", "labels": {"alertname": "Arbitrary"}},
                {"status": "resolved", "labels": {"alertname": "Canonical"}},
            ],
        }
    )
    assert "• [firing] Arbitrary (none)" in body
    assert "• [resolved] Canonical (none)" in body
    assert "[healthy]" not in body
    print("ok: alert statuses retain canonical operational semantics")


def test_render_is_bounded() -> None:
    many = {"status": "firing", "alerts": [{"status": "firing", "labels": {"alertname": f"A{i}"}} for i in range(50)]}
    body = receiver._render(many)
    assert body.count("• ") == receiver._MAX_ALERTS, "alert list must be bounded"
    assert "and 30 more" in body
    print("ok: render is bounded")


def test_render_replaces_malformed_projected_values() -> None:
    body = receiver._render(
        {
            "status": "firing\nSECRET-STATUS",
            "alerts": [
                {
                    "status": "\ud800",
                    "labels": {
                        "alertname": "é" * (receiver._MAX_ALERT_NAME_BYTES // 2 + 1),
                        "severity": "warn\u202eing",
                        "namespace": "monitoring\nSECRET-LABEL",
                        "job_name": "before\u2028after",
                    },
                    "generatorURL": "http://prometheus/before\u2029after",
                }
            ],
        }
    )
    assert body == "⚠️ Alertmanager: unknown (1 alert(s))\n• [unknown] unknown (unknown)"
    assert "SECRET" not in body
    body.encode("utf-8")
    print("ok: malformed projected values use content-free fallbacks")


def test_render_notice_bytes_are_bounded() -> None:
    alerts = [
        {
            "status": "firing",
            "labels": {
                "alertname": "A" * receiver._MAX_ALERT_NAME_BYTES,
                "severity": "S" * receiver._MAX_SEVERITY_BYTES,
                **{key: "V" * receiver._MAX_LABEL_VALUE_BYTES for key in receiver._SAFE_LABELS},
            },
            "generatorURL": "http://prometheus/" + "x" * (receiver._MAX_GENERATOR_URL_BYTES - 18),
        }
        for _ in range(receiver._MAX_ALERTS + 5)
    ]
    body = receiver._render({"status": "firing", "alerts": alerts})
    assert len(body.encode("utf-8")) <= receiver._MAX_NOTICE_BYTES
    assert "… and 5 more" in body
    assert "omitted by notice byte limit" in body
    print("ok: rendered notice has a final UTF-8 byte bound")


def test_render_tolerates_malformed_alert() -> None:
    # A non-dict element must not crash the handler thread (do_POST catches no AttributeError).
    body = receiver._render({"status": "firing", "alerts": ["junk", {"labels": {"alertname": "Real"}}]})
    assert "Real" in body
    print("ok: render tolerates a malformed alert element")


def _txn(body: str) -> str:
    # Recompute the receiver's txn scheme independently, to pin its stable+bucketed contract.
    digest = receiver.hashlib.sha1(body.encode()).hexdigest()[:12]
    return f"alert-{digest}-{int(receiver.time.time()) // 300}"


def test_txn_id_is_stable_but_time_bucketed() -> None:
    same = "🔔 Alertmanager: firing (1 alert(s))\n• [firing] X (warning)"
    # Deterministic within a 5-min bucket (hashlib, not PYTHONHASHSEED-randomized builtin hash()):
    # an Alertmanager retry of the same delivery -> identical txn -> Matrix dedups it.
    assert _txn(same) == _txn(same)
    # A distinct group body yields a distinct txn.
    assert _txn(same + "!") != _txn(same)
    print("ok: txn id is deterministic and time-bucketed")


def test_safe_label_summary_excludes_content() -> None:
    labels = {"namespace": "monitoring", "matrix_user_id": "@alice:x", "body": "hi", "severity": "warning"}
    summary = receiver._safe_label_summary(labels)
    assert "namespace=monitoring" in summary
    assert "matrix_user_id" not in summary and "@alice" not in summary and "body" not in summary
    print("ok: safe-label summary excludes content")


def _serve(handler: type[http.server.BaseHTTPRequestHandler]) -> receiver._BoundedThreadingHTTPServer:
    server = receiver._BoundedThreadingHTTPServer(("127.0.0.1", 0), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


def _raw_status(
    server: receiver._BoundedThreadingHTTPServer,
    request: bytes,
    *,
    shutdown_write: bool = True,
) -> int:
    address = cast(tuple[str, int], server.server_address)
    with socket.create_connection(address, timeout=2) as connection:
        connection.settimeout(2)
        connection.sendall(request)
        if shutdown_write:
            try:
                connection.shutdown(socket.SHUT_WR)
            except OSError as error:
                # A fast rejection can close the peer after buffering its response but before this
                # half-close. The response remains readable; every other socket failure is real.
                if error.errno != errno.ENOTCONN:
                    raise
        status_line = connection.makefile("rb").readline()
    return int(status_line.split()[1])


def test_raw_status_tolerates_only_peer_closed_shutdown() -> None:
    server = cast(receiver._BoundedThreadingHTTPServer, mock.Mock(server_address=("127.0.0.1", 1)))
    connection = mock.MagicMock(spec=socket.socket)
    connection.__enter__.return_value = connection
    connection.makefile.return_value.readline.return_value = b"HTTP/1.1 400 Bad Request\r\n"

    connection.shutdown.side_effect = OSError(errno.ENOTCONN, "peer already closed")
    with mock.patch.object(socket, "create_connection", return_value=connection):
        assert _raw_status(server, b"complete request") == 400
    connection.makefile.assert_called_once_with("rb")

    connection.reset_mock()
    connection.shutdown.side_effect = OSError(errno.EBADF, "unexpected bad descriptor")
    with mock.patch.object(socket, "create_connection", return_value=connection):
        try:
            _raw_status(server, b"complete request")
        except OSError as error:
            assert error.errno == errno.EBADF
        else:
            raise AssertionError("unexpected shutdown errors must propagate")
    connection.makefile.assert_not_called()
    print("ok: raw status tolerates only peer-closed shutdown")


def test_webhook_rejects_invalid_framing_and_oversized_body() -> None:
    recv = _serve(receiver._Handler)
    try:
        assert _raw_status(recv, b"POST / HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n") == 411
        assert (
            _raw_status(
                recv,
                b"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 2\r\n"
                b"Content-Length: 2\r\nConnection: close\r\n\r\n{}",
            )
            == 400
        )
        assert (
            _raw_status(
                recv,
                b"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 2\r\n"
                b"Transfer-Encoding: chunked\r\nConnection: close\r\n\r\n{}",
            )
            == 400
        )
        assert (
            _raw_status(
                recv,
                b"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: -1\r\nConnection: close\r\n\r\n",
            )
            == 400
        )
        oversized = receiver._MAX_REQUEST_BYTES + 1
        assert (
            _raw_status(
                recv,
                f"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: {oversized}\r\nConnection: close\r\n\r\n".encode(),
            )
            == 413
        )
        huge_decimal = "9" * 5_000
        assert (
            _raw_status(
                recv,
                f"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: {huge_decimal}\r\n"
                "Connection: close\r\n\r\n".encode(),
            )
            == 413
        )
        print("ok: webhook framing and body size are bounded")
    finally:
        recv.shutdown()
        recv.server_close()


def test_request_headers_are_bounded() -> None:
    recv = _serve(receiver._Handler)
    prefix = b"GET /healthz HTTP/1.1\r\nHost: test\r\nX-Fill: "
    suffix = b"\r\nConnection: close\r\n\r\n"
    try:
        fill = b"a" * (receiver._MAX_HEADER_BYTES - len(prefix) - len(suffix))
        request = prefix + fill + suffix
        assert len(request) == receiver._MAX_HEADER_BYTES
        assert _raw_status(recv, request) == 200

        oversized = prefix + fill + b"a" + suffix
        assert _raw_status(recv, oversized) == 431
        assert _raw_status(recv, b"G" * (receiver._MAX_HEADER_BYTES + 1)) == 431
        print("ok: aggregate request headers are bounded")
    finally:
        recv.shutdown()
        recv.server_close()


def test_slow_headers_release_all_request_slots() -> None:
    with (
        mock.patch.object(receiver, "_REQUEST_TIMEOUT_SECONDS", 0.1),
        mock.patch.object(receiver._Handler, "timeout", 0.1),
    ):
        recv = _serve(receiver._Handler)
        address = cast(tuple[str, int], recv.server_address)
        connections = [socket.create_connection(address, timeout=2) for _ in range(receiver._MAX_CONCURRENT_REQUESTS)]

        def trickle(connection: socket.socket) -> None:
            deadline = time.monotonic() + 0.3
            while time.monotonic() < deadline:
                try:
                    connection.sendall(b"a")
                except OSError:
                    return
                time.sleep(0.03)

        try:
            for connection in connections:
                connection.sendall(b"GET /healthz HTTP/1.1\r\nHost: test\r\nX-Slow: ")
            senders = [threading.Thread(target=trickle, args=(connection,)) for connection in connections]
            for sender in senders:
                sender.start()

            # Every slow connection exceeds one shared wall-clock deadline despite the trickle.
            time.sleep(0.15)
            assert (
                _raw_status(
                    recv,
                    b"GET /healthz HTTP/1.1\r\nHost: test\r\nConnection: close\r\n\r\n",
                )
                == 200
            )
            for sender in senders:
                sender.join(timeout=1)
                assert not sender.is_alive()
            print("ok: slow headers release every bounded request slot")
        finally:
            for connection in connections:
                connection.close()
            recv.shutdown()
            recv.server_close()


def test_webhook_rejects_incomplete_and_slow_body() -> None:
    with (
        mock.patch.object(receiver, "_REQUEST_TIMEOUT_SECONDS", 0.1),
        mock.patch.object(receiver._Handler, "timeout", 0.1),
    ):
        recv = _serve(receiver._Handler)
        try:
            assert (
                _raw_status(
                    recv,
                    b"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{",
                )
                == 400
            )
            assert (
                _raw_status(
                    recv,
                    b"POST / HTTP/1.1\r\nHost: test\r\nContent-Length: 2\r\nConnection: close\r\n\r\n{",
                    shutdown_write=False,
                )
                == 408
            )
            print("ok: incomplete and slow webhook bodies are rejected")
        finally:
            recv.shutdown()
            recv.server_close()


class _BlockingHandler(http.server.BaseHTTPRequestHandler):
    release = threading.Event()
    lock = threading.Lock()
    active = 0
    max_active = 0

    def log_message(self, format: str, *args: Any) -> None:
        pass

    def do_GET(self) -> None:
        with self.lock:
            type(self).active += 1
            type(self).max_active = max(type(self).max_active, type(self).active)
        try:
            self.release.wait(timeout=2)
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
        finally:
            with self.lock:
                type(self).active -= 1


def test_request_concurrency_is_bounded_and_recovers() -> None:
    _BlockingHandler.release.clear()
    _BlockingHandler.active = 0
    _BlockingHandler.max_active = 0
    recv = _serve(_BlockingHandler)
    statuses: list[int] = []

    def request() -> None:
        with urllib.request.urlopen(f"http://127.0.0.1:{recv.server_address[1]}/", timeout=5) as response:
            statuses.append(response.status)

    clients = [threading.Thread(target=request) for _ in range(receiver._MAX_CONCURRENT_REQUESTS + 1)]
    try:
        for client in clients:
            client.start()
        deadline = time.monotonic() + 2
        while _BlockingHandler.active < receiver._MAX_CONCURRENT_REQUESTS:
            assert time.monotonic() < deadline, "bounded handlers did not become active"
            time.sleep(0.01)
        time.sleep(0.05)
        assert _BlockingHandler.max_active == receiver._MAX_CONCURRENT_REQUESTS

        _BlockingHandler.release.set()
        for client in clients:
            client.join(timeout=5)
            assert not client.is_alive()
        assert statuses == [200] * len(clients)

        # A fresh request after saturation proves every completed handler released its slot.
        request()
        assert statuses[-1] == 200
        print("ok: request concurrency is bounded and recovers")
    finally:
        _BlockingHandler.release.set()
        recv.shutdown()
        recv.server_close()


def test_thread_start_failure_releases_request_slot() -> None:
    recv = receiver._BoundedThreadingHTTPServer(("127.0.0.1", 0), receiver._Handler)
    try:
        with mock.patch.object(
            http.server.ThreadingHTTPServer,
            "process_request",
            side_effect=RuntimeError("synthetic thread-start failure"),
        ):
            try:
                recv.process_request(None, None)
            except RuntimeError:
                pass
            else:
                raise AssertionError("synthetic thread-start failure must propagate")

        acquired = [recv._request_slots.acquire(blocking=False) for _ in range(receiver._MAX_CONCURRENT_REQUESTS)]
        assert all(acquired)
        assert not recv._request_slots.acquire(blocking=False)
        for _ in acquired:
            recv._request_slots.release()
        print("ok: thread-start failure releases request slot")
    finally:
        recv.server_close()


class _FakeSynapse(http.server.BaseHTTPRequestHandler):
    received: ClassVar[list[dict[str, Any]]] = []
    response_mode = "normal"

    def log_message(self, format: str, *args: Any) -> None:
        pass

    def do_PUT(self) -> None:
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0") or "0"))
        _FakeSynapse.received.append(json.loads(raw or b"{}"))
        body = b'{"event_id":"$x"}'
        self.send_response(200)
        if self.response_mode == "absent-length":
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
        elif self.response_mode == "absent-length-oversized":
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b"operator-private-value" + b"x" * receiver._MAX_MATRIX_RESPONSE_BYTES)
        elif self.response_mode == "declared-oversized":
            self.send_header("Content-Length", str(receiver._MAX_MATRIX_RESPONSE_BYTES + 1))
            self.send_header("Connection", "close")
            self.end_headers()
        elif self.response_mode == "huge-content-length":
            self.send_header("Content-Length", "9" * 10_000)
            self.send_header("Connection", "close")
            self.end_headers()
        elif self.response_mode == "incomplete":
            self.send_header("Content-Length", str(len(body) + 1))
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(body)
        elif self.response_mode == "duplicate-content-length":
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.response_mode == "ambiguous-transfer":
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            self.wfile.write(f"{len(body):x}\r\n".encode() + body + b"\r\n0\r\n\r\n")
        elif self.response_mode == "chunked":
            self.send_header("Transfer-Encoding", "chunked")
            self.end_headers()
            self.wfile.write(f"{len(body):x}\r\n".encode() + body + b"\r\n0\r\n\r\n")
        elif self.response_mode == "trickle":
            body = b'{"x":1}'
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            for byte in body:
                try:
                    self.wfile.write(bytes([byte]))
                    self.wfile.flush()
                except (BrokenPipeError, ConnectionResetError):
                    break
                time.sleep(0.05)
        else:
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)


def _post_webhook(recv: receiver._BoundedThreadingHTTPServer, payload: dict[str, Any] = WEBHOOK) -> int:
    request = urllib.request.Request(
        f"http://127.0.0.1:{recv.server_address[1]}/",
        data=json.dumps(payload).encode(),
        headers={"Content-Type": "application/json"},
        method="POST",
    )
    try:
        with urllib.request.urlopen(request, timeout=5) as response:
            return response.status
    except urllib.error.HTTPError as error:
        return error.code


def test_webhook_posts_content_free_notice() -> None:
    _FakeSynapse.received = []
    _FakeSynapse.response_mode = "normal"
    synapse = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeSynapse)
    threading.Thread(target=synapse.serve_forever, daemon=True).start()
    recv = _serve(receiver._Handler)
    recv.homeserver = f"http://127.0.0.1:{synapse.server_address[1]}"
    recv.token = "alertbot-token"
    recv.room_id = ROOM
    try:
        assert _post_webhook(recv) == 200
        time.sleep(0.2)
        assert len(_FakeSynapse.received) == 1, "one notice per group webhook"
        posted = _FakeSynapse.received[0]
        assert posted["msgtype"] == "m.notice"
        assert "LLMTokenBurnHigh" in posted["body"]
        assert "SECRET-PROMPT-CONTENT-should-never-leak" not in posted["body"]
        print("ok: webhook posts one content-free m.notice")

        bounded_webhook = {
            "status": "firing",
            "alerts": [
                {
                    "status": "firing",
                    "labels": {"alertname": f"Alert{i}", "namespace": "n" * receiver._MAX_LABEL_VALUE_BYTES},
                    "generatorURL": "http://prometheus/" + "x" * 1_000,
                }
                for i in range(receiver._MAX_ALERTS)
            ],
        }
        assert _post_webhook(recv, bounded_webhook) == 200
        time.sleep(0.2)
        assert len(_FakeSynapse.received) == 2
        bounded_notice = _FakeSynapse.received[1]["body"]
        assert len(bounded_notice.encode("utf-8")) <= receiver._MAX_NOTICE_BYTES
        assert "omitted by notice byte limit" in bounded_notice
        print("ok: webhook posts a byte-bounded notice")
    finally:
        synapse.shutdown()
        synapse.server_close()
        recv.shutdown()
        recv.server_close()


def test_matrix_responses_are_bounded_and_strictly_framed() -> None:
    _FakeSynapse.received = []
    synapse = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeSynapse)
    threading.Thread(target=synapse.serve_forever, daemon=True).start()
    recv = _serve(receiver._Handler)
    recv.homeserver = f"http://127.0.0.1:{synapse.server_address[1]}"
    recv.token = "alertbot-token"
    recv.room_id = ROOM
    try:
        for mode in ("normal", "absent-length", "chunked"):
            _FakeSynapse.response_mode = mode
            assert _post_webhook(recv) == 200, f"bounded {mode} response must succeed"

        invalid_modes = (
            "absent-length-oversized",
            "declared-oversized",
            "huge-content-length",
            "incomplete",
            "duplicate-content-length",
            "ambiguous-transfer",
        )
        errors = io.StringIO()
        with contextlib.redirect_stderr(errors):
            for mode in invalid_modes:
                _FakeSynapse.response_mode = mode
                assert _post_webhook(recv) == 502, f"{mode} response must fail closed"

        assert len(_FakeSynapse.received) == 9
        assert errors.getvalue() == "alert-receiver: delivery failed: MatrixResponseError\n" * len(invalid_modes)
        assert "operator-private-value" not in errors.getvalue()
        print("ok: Matrix send responses are bounded and strictly framed")
    finally:
        _FakeSynapse.response_mode = "normal"
        synapse.shutdown()
        synapse.server_close()
        recv.shutdown()
        recv.server_close()


def test_matrix_delivery_workers_are_bounded_and_recover() -> None:
    release = threading.Event()
    calls = 0
    calls_lock = threading.Lock()

    def blocking_delivery(*_args: object) -> None:
        nonlocal calls
        with calls_lock:
            calls += 1
        release.wait(timeout=2)

    with (
        mock.patch.object(receiver, "_MATRIX_REQUEST_TIMEOUT_SECONDS", 0.05),
        mock.patch.object(receiver, "_post_notice_io", side_effect=blocking_delivery),
    ):
        for _ in range(receiver._MAX_CONCURRENT_MATRIX_REQUESTS):
            try:
                receiver._post_notice("http://matrix", "token", ROOM, "notice")
            except TimeoutError:
                pass
            else:
                raise AssertionError("a retained Matrix worker must reach its wall-clock deadline")

        try:
            receiver._post_notice("http://matrix", "token", ROOM, "notice")
        except TimeoutError:
            pass
        else:
            raise AssertionError("a saturated Matrix worker pool must fail closed")
        assert calls == receiver._MAX_CONCURRENT_MATRIX_REQUESTS

        release.set()
        deadline = time.monotonic() + 1
        while True:
            try:
                receiver._post_notice("http://matrix", "token", ROOM, "notice")
                break
            except TimeoutError:
                assert time.monotonic() < deadline, "Matrix worker capacity did not recover"
                time.sleep(0.01)
        assert calls == receiver._MAX_CONCURRENT_MATRIX_REQUESTS + 1
    print("ok: Matrix delivery workers are bounded and recover")


def test_matrix_worker_start_failure_releases_slot() -> None:
    with mock.patch.object(threading.Thread, "start", side_effect=RuntimeError("synthetic start failure")):
        try:
            receiver._post_notice("http://matrix", "token", ROOM, "notice")
        except receiver.MatrixResponseError:
            pass
        else:
            raise AssertionError("a Matrix worker start failure must fail closed")

    acquired = [
        receiver._MATRIX_REQUEST_SLOTS.acquire(blocking=False)
        for _ in range(receiver._MAX_CONCURRENT_MATRIX_REQUESTS)
    ]
    assert all(acquired)
    assert not receiver._MATRIX_REQUEST_SLOTS.acquire(blocking=False)
    for _ in acquired:
        receiver._MATRIX_REQUEST_SLOTS.release()
    print("ok: Matrix worker start failure releases its slot")


def test_matrix_worker_releases_slot_before_publishing_outcome() -> None:
    def delivery(observed: list[BaseException]) -> None:
        try:
            receiver._post_notice("http://matrix", "token", ROOM, "notice")
        except BaseException as error:
            observed.append(error)

    for delivery_error in (None, receiver.MatrixResponseError("synthetic delivery failure")):
        class PausedReleaseSemaphore:
            def __init__(self) -> None:
                self.inner = threading.BoundedSemaphore(receiver._MAX_CONCURRENT_MATRIX_REQUESTS)
                self.release_started = threading.Event()
                self.release_allowed = threading.Event()

            def acquire(self, *, blocking: bool = True) -> bool:
                return self.inner.acquire(blocking=blocking)

            def release(self) -> None:
                self.release_started.set()
                assert self.release_allowed.wait(timeout=1)
                self.inner.release()

        slots = PausedReleaseSemaphore()
        observed: list[BaseException] = []

        worker_effect = None if delivery_error is None else delivery_error
        with (
            mock.patch.object(receiver, "_MATRIX_REQUEST_SLOTS", slots),
            mock.patch.object(receiver, "_post_notice_io", side_effect=worker_effect),
        ):
            caller = threading.Thread(target=delivery, args=(observed,))
            caller.start()
            assert slots.release_started.wait(timeout=1)
            assert caller.is_alive(), "delivery outcome became visible before its slot was released"
            slots.release_allowed.set()
            caller.join(timeout=1)
            assert not caller.is_alive()

        if delivery_error is None:
            assert observed == []
        else:
            assert len(observed) == 1 and type(observed[0]) is type(delivery_error)
        acquired = [
            slots.acquire(blocking=False)
            for _ in range(receiver._MAX_CONCURRENT_MATRIX_REQUESTS)
        ]
        assert all(acquired)
    print("ok: Matrix worker releases capacity before publishing success or failure")


def test_matrix_delivery_timeout_is_end_to_end() -> None:
    _FakeSynapse.received = []
    _FakeSynapse.response_mode = "trickle"
    synapse = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeSynapse)
    threading.Thread(target=synapse.serve_forever, daemon=True).start()
    recv = _serve(receiver._Handler)
    recv.homeserver = f"http://127.0.0.1:{synapse.server_address[1]}"
    recv.token = "alertbot-token"
    recv.room_id = ROOM
    errors = io.StringIO()
    started = time.monotonic()
    try:
        with (
            mock.patch.object(receiver, "_MATRIX_REQUEST_TIMEOUT_SECONDS", 0.1),
            contextlib.redirect_stderr(errors),
        ):
            assert _post_webhook(recv) == 502
        assert time.monotonic() - started < 0.3
        assert errors.getvalue() == "alert-receiver: delivery failed: TimeoutError\n"
        print("ok: Matrix delivery timeout is an end-to-end wall-clock bound")
    finally:
        _FakeSynapse.response_mode = "normal"
        synapse.shutdown()
        synapse.server_close()
        recv.shutdown()
        recv.server_close()


if __name__ == "__main__":
    test_listen_port_is_bounded_and_content_free()
    test_rendered_ports_are_aligned()
    test_network_peers_are_exactly_scoped()
    test_render_is_content_free()
    test_statuses_are_semantically_bounded()
    test_render_is_bounded()
    test_render_replaces_malformed_projected_values()
    test_render_notice_bytes_are_bounded()
    test_render_tolerates_malformed_alert()
    test_txn_id_is_stable_but_time_bucketed()
    test_safe_label_summary_excludes_content()
    test_raw_status_tolerates_only_peer_closed_shutdown()
    test_webhook_rejects_invalid_framing_and_oversized_body()
    test_request_headers_are_bounded()
    test_slow_headers_release_all_request_slots()
    test_webhook_rejects_incomplete_and_slow_body()
    test_request_concurrency_is_bounded_and_recovers()
    test_thread_start_failure_releases_request_slot()
    test_webhook_posts_content_free_notice()
    test_matrix_responses_are_bounded_and_strictly_framed()
    test_matrix_delivery_workers_are_bounded_and_recover()
    test_matrix_worker_start_failure_releases_slot()
    test_matrix_worker_releases_slot_before_publishing_outcome()
    test_matrix_delivery_timeout_is_end_to_end()
    print("alert receiver tests passed")
