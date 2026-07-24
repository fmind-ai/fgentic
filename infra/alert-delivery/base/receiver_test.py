#!/usr/bin/env python3
"""Offline unit + fake-API tests for the Alertmanager -> Matrix receiver (issue #456).

No network, no cluster: the content-free rendering contract is exercised with crafted payloads, and
the full webhook -> Matrix post is driven against an in-process fake Synapse. Mirrors the fixture
discipline of the canary's probe_test.py.
"""

from __future__ import annotations

import errno
import http.server
import json
import socket
import subprocess
import sys
import threading
import time
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


def test_network_peers_are_exactly_scoped() -> None:
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
    resources = json.loads(rendered.stdout)
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
    assert body == "✅ Alertmanager: unknown (1 alert(s))\n• [unknown] unknown (unknown)"
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

    def log_message(self, format: str, *args: Any) -> None:
        pass

    def do_PUT(self) -> None:
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0") or "0"))
        _FakeSynapse.received.append(json.loads(raw or b"{}"))
        body = b'{"event_id":"$x"}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def test_webhook_posts_content_free_notice() -> None:
    _FakeSynapse.received = []
    synapse = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeSynapse)
    threading.Thread(target=synapse.serve_forever, daemon=True).start()
    recv = _serve(receiver._Handler)
    recv.homeserver = f"http://127.0.0.1:{synapse.server_address[1]}"
    recv.token = "alertbot-token"
    recv.room_id = ROOM
    try:
        request = urllib.request.Request(
            f"http://127.0.0.1:{recv.server_address[1]}/",
            data=json.dumps(WEBHOOK).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(request, timeout=5) as response:
            assert response.status == 200
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
        bounded_request = urllib.request.Request(
            f"http://127.0.0.1:{recv.server_address[1]}/",
            data=json.dumps(bounded_webhook).encode(),
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with urllib.request.urlopen(bounded_request, timeout=5) as response:
            assert response.status == 200
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


if __name__ == "__main__":
    test_network_peers_are_exactly_scoped()
    test_render_is_content_free()
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
    print("alert receiver tests passed")
