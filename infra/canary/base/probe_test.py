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

PROBE = str(Path(__file__).with_name("probe.py"))

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
    print("ok: reply-detection contract")


class _FakeMatrix(http.server.BaseHTTPRequestHandler):
    # timeline the ghost eventually posts (list of events); empty -> the probe times out.
    timeline: list[dict] = []

    def log_message(self, *_args) -> None:  # silence
        pass

    def _send(self, payload: dict) -> None:
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self._send({"access_token": "canary-token"} if self.path.endswith("/login") else {})

    def do_PUT(self) -> None:
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self._send({"event_id": PROBE_EVENT})

    def do_GET(self) -> None:
        if "since=" not in self.path:
            self._send({"next_batch": "s0"})  # initial cursor, no events yet
            return
        events = _FakeMatrix.timeline
        rooms = {"join": {ROOM: {"timeline": {"events": events}}}} if events else {}
        self._send({"next_batch": "s1", "rooms": rooms})


def _run_probe(timeline: list[dict], deadline: str) -> int:
    _FakeMatrix.timeline = timeline
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
        return subprocess.run([sys.executable, PROBE], env=env, check=False).returncode
    finally:
        server.shutdown()


def test_round_trip_success() -> None:
    timeline = [_reply(REPLY_EVENT, "⏳ working on it…"), _edit(REPLY_EVENT, "final answer")]
    assert _run_probe(timeline, "10") == 0, "probe should succeed on an edited async reply"
    print("ok: round-trip success")


def test_round_trip_timeout() -> None:
    assert _run_probe([], "1") != 0, "probe must fail closed when no reply arrives"
    print("ok: round-trip timeout fails closed")


if __name__ == "__main__":
    test_reply_detection_contract()
    test_round_trip_success()
    test_round_trip_timeout()
    print("canary probe tests passed")
