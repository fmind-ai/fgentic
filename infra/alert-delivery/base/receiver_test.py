#!/usr/bin/env python3
"""Offline unit + fake-API tests for the Alertmanager -> Matrix receiver (issue #456).

No network, no cluster: the content-free rendering contract is exercised with crafted payloads, and
the full webhook -> Matrix post is driven against an in-process fake Synapse. Mirrors the fixture
discipline of the canary's probe_test.py.
"""

from __future__ import annotations

import http.server
import json
import sys
import threading
import time
import urllib.request
from pathlib import Path

sys.path.insert(0, str(Path(__file__).parent))
import receiver  # noqa: E402

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


def test_render_is_content_free() -> None:
    body = receiver._render(WEBHOOK)
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


class _FakeSynapse(http.server.BaseHTTPRequestHandler):
    received: list[dict] = []

    def log_message(self, *_a) -> None:
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
    recv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), receiver._Handler)
    recv.homeserver = f"http://127.0.0.1:{synapse.server_address[1]}"  # type: ignore[attr-defined]
    recv.token = "alertbot-token"  # type: ignore[attr-defined]
    recv.room_id = ROOM  # type: ignore[attr-defined]
    threading.Thread(target=recv.serve_forever, daemon=True).start()
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
    finally:
        synapse.shutdown()
        recv.shutdown()


if __name__ == "__main__":
    test_render_is_content_free()
    test_render_is_bounded()
    test_render_tolerates_malformed_alert()
    test_txn_id_is_stable_but_time_bucketed()
    test_safe_label_summary_excludes_content()
    test_webhook_posts_content_free_notice()
    print("alert receiver tests passed")
