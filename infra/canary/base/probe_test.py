#!/usr/bin/env python3
"""Offline unit + fake-API tests for the delegation-canary probe (issue #454).

No network, no cluster: the reply-detection contract is exercised with realistic mautrix-shaped
event timelines, and the full login→send→sync round-trip is driven against an in-process fake Matrix
homeserver. Mirrors the fake-transport fixture discipline of scripts/test-fed-check.sh.
"""

from __future__ import annotations

import http.server
import json
import os
import subprocess
import sys
import threading
from pathlib import Path
from typing import Any, ClassVar

PROBE = str(Path(__file__).with_name("probe.py"))
ENABLED_PROFILE = str(Path(__file__).parents[1] / "profiles" / "enabled")

sys.path.insert(0, str(Path(__file__).parent))
import probe  # noqa: E402  (import after sys.path shim)

GHOST = "@agent-docs-qa:fgentic.localhost"
ROOM = "!canary:fgentic.localhost"
PROBE_EVENT = "$probe"
REPLY_EVENT = "$reply"


def _reply(event_id: str, body: str, in_reply_to: str = PROBE_EVENT, sender: str = GHOST) -> dict:
    """An m.notice that replies to our probe event (may be a placeholder body)."""
    return {
        "type": "m.room.message",
        "event_id": event_id,
        "sender": sender,
        "content": {
            "msgtype": "m.notice",
            "body": body,
            "m.relates_to": {"m.in_reply_to": {"event_id": in_reply_to}},
        },
    }


def _edit(target_event: str, new_body: str, sender: str = GHOST) -> dict:
    """A mautrix-shaped m.replace edit: m.relates_to has rel_type + event_id, NO m.in_reply_to."""
    return {
        "type": "m.room.message",
        "event_id": "$edit",
        "sender": sender,
        "content": {
            "msgtype": "m.notice",
            "body": " * final answer",
            "m.new_content": {"msgtype": "m.notice", "body": new_body},
            "m.relates_to": {"rel_type": "m.replace", "event_id": target_event},
        },
    }


def test_reply_detection_contract() -> None:
    # Async path: a placeholder reply followed by a SEPARATE m.replace edit (the real bridge shape).
    async_timeline = [_reply(REPLY_EVENT, "⏳ working on it…"), _edit(REPLY_EVENT, "final answer")]
    assert probe._reply_succeeded(async_timeline, GHOST, PROBE_EVENT), "edited async reply must count"
    # Fast synchronous path: a single m.notice reply with a real body, no edit.
    assert probe._reply_succeeded([_reply(REPLY_EVENT, "the answer")], GHOST, PROBE_EVENT)
    # Only a placeholder, no edit yet -> not successful.
    assert not probe._reply_succeeded([_reply(REPLY_EVENT, "⏳ working on it…")], GHOST, PROBE_EVENT)
    # Reply to a DIFFERENT event does not count.
    assert not probe._reply_succeeded([_reply(REPLY_EVENT, "answer", in_reply_to="$other")], GHOST, PROBE_EVENT)
    # A different sender does not count.
    assert not probe._reply_succeeded([_reply(REPLY_EVENT, "answer", sender="@x:y")], GHOST, PROBE_EVENT)
    # An edit whose new body is an error/blocked/empty placeholder is not successful.
    for bad in ("⚠️ blocked", "🛑 refused", "(the agent returned no content)", ""):
        assert not probe._reply_succeeded(
            [_reply(REPLY_EVENT, "⏳ working on it…"), _edit(REPLY_EVENT, bad)], GHOST, PROBE_EVENT
        ), f"error edit body {bad!r} must not count"
    assert not probe._reply_succeeded(
        [
            _reply(REPLY_EVENT, "⏳ working on it…"),
            _edit(REPLY_EVENT, "premature answer"),
            _edit(REPLY_EVENT, "⚠️ final failure"),
        ],
        GHOST,
        PROBE_EVENT,
    ), "the latest edit in a sync batch must remain authoritative"
    tracker = probe._ReplyTracker(GHOST, PROBE_EVENT)
    assert not tracker.observe([_reply(REPLY_EVENT, "⏳ working on it…")])
    assert tracker.observe([_edit(REPLY_EVENT, "final answer")])
    print("ok: reply-detection contract")


def test_reply_correlation_state_is_bounded() -> None:
    tracker = probe._ReplyTracker(GHOST, PROBE_EVENT)
    for index in range(probe._MAX_REPLY_STATE):
        assert not tracker.observe([_reply(f"$reply-{index}", "⏳ working on it…")])
    try:
        tracker.observe([_reply("$overflow", "⏳ working on it…")])
    except SystemExit:
        pass
    else:
        raise AssertionError("reply state must fail closed at its fixed limit")
    print("ok: reply correlation state is bounded")


def test_probe_egress_is_exactly_scoped() -> None:
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
        resource["metadata"]["name"]: resource["spec"]
        for resource in resources
        if resource["kind"] == "NetworkPolicy"
    }
    assert policies == {
        "canary-default-deny": {
            "podSelector": {},
            "policyTypes": ["Ingress", "Egress"],
        },
        "canary-probe": {
            "podSelector": {
                "matchLabels": {"app.kubernetes.io/name": "delegation-canary"},
            },
            "policyTypes": ["Ingress", "Egress"],
            "ingress": [],
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
                                "matchLabels": {
                                    "app.kubernetes.io/name": "matrix-authentication-service",
                                },
                            },
                        }
                    ],
                    "ports": [{"protocol": "TCP", "port": 8080}],
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
        "canary NetworkPolicies must stay limited to default-deny plus the exact CoreDNS, MAS, "
        "and ESS HAProxy peers"
    )
    print("ok: canary probe egress is exactly scoped")


class _FakeMatrix(http.server.BaseHTTPRequestHandler):
    # Successive ghost timelines; empty -> the probe times out.
    timelines: ClassVar[list[list[dict]]] = []
    sync_index = 0
    response_mode: str | None = None

    def log_message(self, format: str, *args: Any) -> None:  # silence
        pass

    def _send(self, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def _send_hostile_login(self) -> bool:
        if self.response_mode == "absent-length-oversized":
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b"{" + b"x" * probe._MAX_RESPONSE_BYTES)
        elif self.response_mode == "declared-oversized":
            self.send_response(200)
            self.send_header("Content-Length", str(probe._MAX_RESPONSE_BYTES + 1))
            self.send_header("Connection", "close")
            self.end_headers()
        elif self.response_mode == "incomplete":
            self.send_response(200)
            self.send_header("Content-Length", "10")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b"{}")
        elif self.response_mode == "malformed-json":
            self.send_response(200)
            self.send_header("Content-Length", "1")
            self.end_headers()
            self.wfile.write(b"{")
        elif self.response_mode == "non-object-json":
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"[]")
        elif self.response_mode == "recursive-json":
            body = b'{"x":' + b"[" * 2_000 + b"0" + b"]" * 2_000 + b"}"
            self.send_response(200)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.response_mode == "duplicate-content-length":
            self.send_response(200)
            self.send_header("Content-Length", "2")
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"{}")
        else:
            return False
        self.close_connection = True
        return True

    def do_POST(self) -> None:
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        if self.path.endswith("/login") and self._send_hostile_login():
            return
        self._send({"access_token": "canary-token"} if self.path.endswith("/login") else {})

    def do_PUT(self) -> None:
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self._send({"event_id": PROBE_EVENT})

    def do_GET(self) -> None:
        if "since=" not in self.path:
            cursor = "attacker-secret" if self.response_mode == "hostile-cursor" else "s0"
            self._send({"next_batch": cursor})  # initial cursor, no events yet
            return
        if self.response_mode == "hostile-cursor":
            self.send_response(200)
            self.send_header("Content-Length", "1")
            self.end_headers()
            self.wfile.write(b"{")
            return
        index = _FakeMatrix.sync_index
        _FakeMatrix.sync_index += 1
        events = _FakeMatrix.timelines[index] if index < len(_FakeMatrix.timelines) else []
        rooms = {"join": {ROOM: {"timeline": {"events": events}}}} if events else {}
        self._send({"next_batch": "s1", "rooms": rooms})


def _run_probe_result(
    timelines: list[list[dict]],
    deadline: str,
    *,
    response_mode: str | None = None,
) -> subprocess.CompletedProcess[str]:
    _FakeMatrix.timelines = timelines
    _FakeMatrix.sync_index = 0
    _FakeMatrix.response_mode = response_mode
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeMatrix)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        base = f"http://127.0.0.1:{server.server_address[1]}"
        env = {
            **os.environ,
            "CANARY_HOMESERVER_URL": base,
            "CANARY_LOGIN_URL": base,  # the fake serves both; the URL split itself is config
            "CANARY_USER": "canary",
            "CANARY_PASSWORD": "secret",
            "CANARY_ROOM_ID": ROOM,
            "CANARY_TARGET_MXID": GHOST,
            "CANARY_DEADLINE_SECONDS": deadline,
        }
        return subprocess.run(
            [sys.executable, PROBE],
            env=env,
            check=False,
            capture_output=True,
            text=True,
        )
    finally:
        server.shutdown()
        server.server_close()


def _run_probe(
    timelines: list[list[dict]],
    deadline: str,
    *,
    response_mode: str | None = None,
) -> int:
    return _run_probe_result(timelines, deadline, response_mode=response_mode).returncode


def test_round_trip_success() -> None:
    timelines = [
        [_reply(REPLY_EVENT, "⏳ working on it…")],
        [_edit(REPLY_EVENT, "final answer")],
    ]
    assert _run_probe(timelines, "10") == 0, "probe should succeed across sync batches"
    print("ok: round-trip success")


def test_round_trip_timeout() -> None:
    assert _run_probe([], "1") != 0, "probe must fail closed when no reply arrives"
    print("ok: round-trip timeout fails closed")


def test_hostile_responses_fail_closed() -> None:
    for mode in (
        "absent-length-oversized",
        "declared-oversized",
        "incomplete",
        "malformed-json",
        "non-object-json",
        "recursive-json",
        "duplicate-content-length",
    ):
        assert _run_probe([], "1", response_mode=mode) != 0, f"{mode} must fail closed"
    print("ok: hostile Matrix responses fail closed")


def test_hostile_cursor_never_reaches_failure_log() -> None:
    result = _run_probe_result([], "1", response_mode="hostile-cursor")
    assert result.returncode != 0
    assert "attacker-secret" not in result.stderr
    assert "GET Matrix response contains invalid JSON" in result.stderr
    print("ok: hostile sync cursor is excluded from failure logs")


if __name__ == "__main__":
    test_reply_detection_contract()
    test_reply_correlation_state_is_bounded()
    test_probe_egress_is_exactly_scoped()
    test_round_trip_success()
    test_round_trip_timeout()
    test_hostile_responses_fail_closed()
    test_hostile_cursor_never_reaches_failure_log()
    print("canary probe tests passed")
