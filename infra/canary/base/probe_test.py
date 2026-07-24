#!/usr/bin/env python3
"""Offline unit + fake-API tests for the delegation-canary probe (issue #454).

No network, no cluster: the reply-detection contract is exercised with realistic mautrix-shaped
event timelines, and the full login→send→sync round-trip is driven against an in-process fake Matrix
homeserver. Mirrors the fake-transport fixture discipline of scripts/test-fed-check.sh.
"""

from __future__ import annotations

import contextlib
import http.server
import json
import os
import subprocess
import sys
import threading
import time
import urllib.parse
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
    assert probe._reply_succeeded([_reply(REPLY_EVENT, "  the answer  ")], GHOST, PROBE_EVENT)
    # Only a placeholder, no edit yet -> not successful.
    assert not probe._reply_succeeded([_reply(REPLY_EVENT, "⏳ working on it…")], GHOST, PROBE_EVENT)
    # Reply to a DIFFERENT event does not count.
    assert not probe._reply_succeeded([_reply(REPLY_EVENT, "answer", in_reply_to="$other")], GHOST, PROBE_EVENT)
    # A different sender does not count.
    assert not probe._reply_succeeded([_reply(REPLY_EVENT, "answer", sender="@x:y")], GHOST, PROBE_EVENT)
    # An edit whose new body is an error/blocked/empty placeholder is not successful.
    for bad in (
        "⚠️ blocked",
        "  ⚠️ blocked  ",
        "🛑 refused",
        "\t🛑 refused\n",
        "(the agent returned no content)",
        "  (the agent returned no content)\t",
        probe._PROVENANCE_BANNER,
        f"\n{probe._PROVENANCE_BANNER}\n",
        "",
        " \t\n",
    ):
        assert not probe._reply_succeeded([_reply(REPLY_EVENT, bad)], GHOST, PROBE_EVENT), (
            f"direct failure body {bad!r} must not count"
        )
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
    resources = json.loads(rendered.stdout)
    assert isinstance(resources, list)
    assert all(isinstance(resource, dict) for resource in resources)
    return resources


def test_deadline_configuration_contract() -> None:
    original = os.environ.pop("CANARY_DEADLINE_SECONDS", None)
    try:
        assert probe._deadline_seconds() == probe._DEFAULT_DEADLINE_SECONDS
        for raw, expected in (("1", 1), ("120", 120), ("150", 150)):
            os.environ["CANARY_DEADLINE_SECONDS"] = raw
            assert probe._deadline_seconds() == expected
    finally:
        if original is None:
            os.environ.pop("CANARY_DEADLINE_SECONDS", None)
        else:
            os.environ["CANARY_DEADLINE_SECONDS"] = original

    expected_error = (
        "canary: CANARY_DEADLINE_SECONDS must be a canonical ASCII integer "
        f"from 1 to {probe._MAX_DEADLINE_SECONDS}\n"
    )
    for raw in (
        "0",
        "-1",
        "151",
        " 1",
        "1 ",
        "01",
        "\N{ARABIC-INDIC DIGIT ONE}",
        "operator-private-value",
        "9" * 10_000,
    ):
        result = _run_probe_result([], raw)
        assert result.returncode != 0, f"invalid deadline {raw[:20]!r} must fail closed"
        assert result.stderr == expected_error, f"invalid deadline {raw[:20]!r} leaked unstable diagnostics"

    resources = _render_enabled_profile()
    cronjob = next(resource for resource in resources if resource.get("kind") == "CronJob")
    config = next(resource for resource in resources if resource.get("kind") == "ConfigMap")
    configured_default = int(config["data"]["CANARY_DEADLINE_SECONDS"])
    active_deadline = cronjob["spec"]["jobTemplate"]["spec"]["activeDeadlineSeconds"]
    assert configured_default == probe._DEFAULT_DEADLINE_SECONDS
    assert 1 <= configured_default <= probe._MAX_DEADLINE_SECONDS < active_deadline
    assert probe._sync_request_timeouts(120) == (5_000, 15.0)
    assert probe._sync_request_timeouts(1) == (1_000, 1)
    assert probe._sync_request_timeouts(0.0001) == (1, 0.0001)
    print("ok: deadline configuration is canonical, bounded, and content-free")


def test_probe_egress_is_exactly_scoped() -> None:
    resources = _render_enabled_profile()
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
    sync_timeouts: ClassVar[list[int]] = []
    sync_index = 0
    response_mode: str | None = None

    def log_message(self, format: str, *args: Any) -> None:  # silence
        pass

    def _send_content_type_headers(self) -> None:
        content_types = {
            "absent-content-type": (),
            "duplicate-content-type": ("application/json", "application/json"),
            "non-json-content-type": ("text/plain",),
            "missing-content-type-parameter-value": ("application/json; charset",),
            "unterminated-content-type-parameter": ('application/json; profile="ops',),
            "space-before-content-type-parameter-equals": ("application/json; charset =UTF-8",),
            "space-after-content-type-parameter-equals": ("application/json; charset= UTF-8",),
            "parameterized-content-type": ('Application/JSON; ; profile="ops;canary";;charset=UTF-8;',),
        }.get(self.response_mode, ("application/json",))
        for content_type in content_types:
            self.send_header("Content-Type", content_type)

    def _send(self, payload: dict | None, *, status: int = 200) -> None:
        body = json.dumps(payload).encode() if payload is not None else b""
        self.send_response(status)
        self._send_content_type_headers()
        transfer_encodings = {
            "chunked": ("ChUnKeD",),
            "duplicate-transfer-encoding": ("chunked", "chunked"),
            "unsupported-transfer-encoding": ("gzip",),
            "combined-transfer-encoding": ("gzip, chunked",),
            "empty-transfer-encoding": ("",),
            "whitespace-transfer-encoding": ("chunked ",),
        }.get(self.response_mode)
        if transfer_encodings is not None:
            for transfer_encoding in transfer_encodings:
                self.send_header("Transfer-Encoding", transfer_encoding)
            if self.response_mode != "chunked":
                self.send_header("Connection", "close")
        else:
            self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        # The hard-deadline test intentionally closes before the slow response.
        with contextlib.suppress(BrokenPipeError):
            if body and self.response_mode in {"chunked", "duplicate-transfer-encoding"}:
                self.wfile.write(f"{len(body):x}\r\n".encode() + body + b"\r\n0\r\n\r\n")
            elif body:
                self.wfile.write(body)
        if transfer_encodings is not None and self.response_mode != "chunked":
            self.close_connection = True

    def _send_trickle(self) -> None:
        body = b'{"x":1}'
        self.send_response(200)
        self._send_content_type_headers()
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        for byte in body:
            with contextlib.suppress(BrokenPipeError):
                self.wfile.write(bytes([byte]))
                self.wfile.flush()
            time.sleep(0.15)

    def _send_hostile_login(self) -> bool:
        if self.response_mode == "absent-length-oversized":
            self.send_response(200)
            self._send_content_type_headers()
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b"{" + b"x" * probe._MAX_RESPONSE_BYTES)
        elif self.response_mode == "declared-oversized":
            self.send_response(200)
            self._send_content_type_headers()
            self.send_header("Content-Length", str(probe._MAX_RESPONSE_BYTES + 1))
            self.send_header("Connection", "close")
            self.end_headers()
        elif self.response_mode == "incomplete":
            self.send_response(200)
            self._send_content_type_headers()
            self.send_header("Content-Length", "10")
            self.send_header("Connection", "close")
            self.end_headers()
            self.wfile.write(b"{}")
        elif self.response_mode == "malformed-json":
            self.send_response(200)
            self._send_content_type_headers()
            self.send_header("Content-Length", "1")
            self.end_headers()
            self.wfile.write(b"{")
        elif self.response_mode == "non-object-json":
            self.send_response(200)
            self._send_content_type_headers()
            self.send_header("Content-Length", "2")
            self.end_headers()
            self.wfile.write(b"[]")
        elif self.response_mode == "recursive-json":
            body = b'{"x":' + b"[" * 2_000 + b"0" + b"]" * 2_000 + b"}"
            self.send_response(200)
            self._send_content_type_headers()
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        elif self.response_mode == "duplicate-content-length":
            self.send_response(200)
            self._send_content_type_headers()
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
        status = 202 if self.response_mode == "accepted-post" else 200
        self._send(
            {"access_token": "canary-token"} if self.path.endswith("/login") else {},
            status=status,
        )

    def do_PUT(self) -> None:
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        status = 202 if self.response_mode == "accepted-put" else 200
        self._send({"event_id": PROBE_EVENT}, status=status)

    def do_GET(self) -> None:
        if self.response_mode == "trickle-response":
            self._send_trickle()
            return
        if "since=" not in self.path:
            if self.response_mode == "no-content-get":
                self._send(None, status=204)
                return
            cursor = "attacker-secret" if self.response_mode == "hostile-cursor" else "s0"
            self._send({"next_batch": cursor})  # initial cursor, no events yet
            return
        query = urllib.parse.parse_qs(urllib.parse.urlsplit(self.path).query)
        _FakeMatrix.sync_timeouts.append(int(query["timeout"][0]))
        if self.response_mode == "slow-sync":
            time.sleep(2.5)
        if self.response_mode == "hostile-cursor":
            self.send_response(200)
            self._send_content_type_headers()
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
    _FakeMatrix.sync_timeouts = []
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


def test_round_trip_deadline_is_hard() -> None:
    started = time.monotonic()
    result = _run_probe_result(
        [[_reply(REPLY_EVENT, "late answer")]],
        "1",
        response_mode="slow-sync",
    )
    elapsed = time.monotonic() - started
    assert result.returncode != 0, "a reply observed after the deadline must fail"
    assert elapsed < 2, "the sync request must not retain its fixed five/15-second bounds"
    assert len(_FakeMatrix.sync_timeouts) == 1
    assert 1 <= _FakeMatrix.sync_timeouts[0] <= 1_000
    assert "delegation round-trip ok" not in result.stdout
    print("ok: round-trip deadline is a hard success and transport bound")


def test_request_timeout_is_end_to_end() -> None:
    _FakeMatrix.response_mode = "trickle-response"
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _FakeMatrix)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    started = time.monotonic()
    try:
        try:
            probe._request(
                "GET",
                f"http://127.0.0.1:{server.server_address[1]}",
                None,
                None,
                timeout_seconds=0.2,
            )
        except SystemExit:
            pass
        else:
            raise AssertionError("a trickled response must not extend the request deadline")
        assert time.monotonic() - started < 0.5
    finally:
        server.shutdown()
        server.server_close()
    print("ok: request timeout is an end-to-end wall-clock bound")


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


def test_matrix_transfer_coding_is_strict_and_chunked_is_accepted() -> None:
    for mode in (
        "duplicate-transfer-encoding",
        "unsupported-transfer-encoding",
        "combined-transfer-encoding",
        "empty-transfer-encoding",
        "whitespace-transfer-encoding",
    ):
        result = _run_probe_result([], "1", response_mode=mode)
        assert result.returncode != 0, f"{mode} must fail closed"
        assert result.stdout == ""
        assert result.stderr == "canary: POST Matrix response has invalid framing\n"

    timelines = [
        [_reply(REPLY_EVENT, "⏳ working on it…")],
        [_edit(REPLY_EVENT, "final answer")],
    ]
    assert _run_probe(timelines, "10", response_mode="chunked") == 0
    print("ok: Matrix transfer coding is strict and chunked-compatible")


def test_non_200_success_responses_fail_closed() -> None:
    for mode, method in (
        ("accepted-post", "POST"),
        ("no-content-get", "GET"),
        ("accepted-put", "PUT"),
    ):
        result = _run_probe_result([], "1", response_mode=mode)
        assert result.returncode != 0, f"{mode} must fail closed"
        assert result.stdout == ""
        assert result.stderr == f"canary: {method} Matrix response has unexpected HTTP status\n"
    print("ok: non-200 Matrix success responses fail closed")


def test_matrix_content_type_is_strict_and_parameterized_json_is_accepted() -> None:
    for mode in (
        "absent-content-type",
        "duplicate-content-type",
        "non-json-content-type",
        "missing-content-type-parameter-value",
        "unterminated-content-type-parameter",
        "space-before-content-type-parameter-equals",
        "space-after-content-type-parameter-equals",
    ):
        result = _run_probe_result([], "1", response_mode=mode)
        assert result.returncode != 0, f"{mode} must fail closed"
        assert result.stdout == ""
        assert result.stderr == "canary: POST Matrix response has invalid Content-Type\n"

    timelines = [
        [_reply(REPLY_EVENT, "⏳ working on it…")],
        [_edit(REPLY_EVENT, "final answer")],
    ]
    assert _run_probe(timelines, "10", response_mode="parameterized-content-type") == 0
    print("ok: Matrix JSON response media type is strict and parameter-compatible")


def test_hostile_cursor_never_reaches_failure_log() -> None:
    result = _run_probe_result([], "1", response_mode="hostile-cursor")
    assert result.returncode != 0
    assert "attacker-secret" not in result.stderr
    assert "GET Matrix response contains invalid JSON" in result.stderr
    print("ok: hostile sync cursor is excluded from failure logs")


if __name__ == "__main__":
    test_reply_detection_contract()
    test_reply_correlation_state_is_bounded()
    test_deadline_configuration_contract()
    test_probe_egress_is_exactly_scoped()
    test_round_trip_success()
    test_round_trip_timeout()
    test_round_trip_deadline_is_hard()
    test_request_timeout_is_end_to_end()
    test_hostile_responses_fail_closed()
    test_matrix_transfer_coding_is_strict_and_chunked_is_accepted()
    test_non_200_success_responses_fail_closed()
    test_matrix_content_type_is_strict_and_parameterized_json_is_accepted()
    test_hostile_cursor_never_reaches_failure_log()
    print("canary probe tests passed")
