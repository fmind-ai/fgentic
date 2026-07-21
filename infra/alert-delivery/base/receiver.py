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
import http.server
import json
import os
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# Low-cardinality, non-content labels safe to surface (never message text / user identifiers).
_SAFE_LABELS = ("namespace", "job_name", "cronjob", "gen_ai_system", "resource_kind")
_MAX_ALERTS = 20  # bound the fan-out so an alert storm becomes a bounded stream, never a flood.


def _env(name: str, default: str | None = None) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        if default is not None:
            return default
        print(f"alert-receiver: required environment variable {name} is missing", file=sys.stderr)
        raise SystemExit(1)
    return value


def _safe_label_summary(labels: dict) -> str:
    parts = [f"{key}={labels[key]}" for key in _SAFE_LABELS if isinstance(labels.get(key), str)]
    return " ".join(parts)


def _render(payload: dict) -> str:
    """Build one bounded, content-free notice for an Alertmanager group webhook."""
    status = payload.get("status", "firing")
    alerts = payload.get("alerts", [])
    if not isinstance(alerts, list):
        alerts = []
    icon = "🔔" if status == "firing" else "✅"
    lines = [f"{icon} Alertmanager: {status} ({len(alerts)} alert(s))"]
    for alert in alerts[:_MAX_ALERTS]:
        if not isinstance(alert, dict):
            continue  # tolerate a malformed element without dropping the whole delivery
        labels = alert.get("labels", {}) if isinstance(alert.get("labels"), dict) else {}
        name = labels.get("alertname", "unknown")
        severity = labels.get("severity", "none")
        summary = _safe_label_summary(labels)
        link = alert.get("generatorURL", "")
        piece = f"• [{alert.get('status', status)}] {name} ({severity})"
        if summary:
            piece += f" {summary}"
        if isinstance(link, str) and link.startswith("http"):
            piece += f" — {link}"
        lines.append(piece)
    if len(alerts) > _MAX_ALERTS:
        lines.append(f"… and {len(alerts) - _MAX_ALERTS} more")
    return "\n".join(lines)


def _post_notice(homeserver: str, token: str, room_id: str, body: str) -> None:
    encoded_room = urllib.parse.quote(room_id, safe="")
    # Transaction id = deterministic body digest + a coarse (5-min) time bucket. Alertmanager retries
    # a failed webhook within seconds, so a retry of the SAME delivery lands in the same bucket ->
    # identical txn -> Matrix dedups it (idempotent). A genuine repeat reminder (repeatInterval, hours
    # later) falls in a different bucket -> new txn -> delivered. hashlib (not builtin hash()) keeps
    # the digest stable across pod restarts, where str hashing is PYTHONHASHSEED-randomized.
    digest = hashlib.sha1(body.encode()).hexdigest()[:12]  # noqa: S324 (idempotency key, not security)
    bucket = int(time.time()) // 300
    txn = f"alert-{digest}-{bucket}"
    url = f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/send/m.room.message/{txn}"
    data = json.dumps({"msgtype": "m.notice", "body": body}).encode()
    request = urllib.request.Request(url, data=data, method="PUT")
    request.add_header("Content-Type", "application/json")
    request.add_header("Authorization", f"Bearer {token}")
    with urllib.request.urlopen(request, timeout=15) as response:  # noqa: S310 (in-cluster HTTP)
        response.read()


class _Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_args) -> None:  # content-free: never log request bodies
        pass

    def _reply(self, code: int) -> None:
        self.send_response(code)
        self.send_header("Content-Length", "0")
        self.end_headers()

    def do_GET(self) -> None:
        # Liveness/readiness only.
        self._reply(200 if self.path == "/healthz" else 404)

    def do_POST(self) -> None:
        raw = self.rfile.read(int(self.headers.get("Content-Length", "0") or "0"))
        try:
            payload = json.loads(raw or b"{}")
        except ValueError:
            self._reply(400)
            return
        if not isinstance(payload, dict):
            self._reply(400)
            return
        try:
            _post_notice(
                self.server.homeserver, self.server.token, self.server.room_id, _render(payload)
            )
        except (urllib.error.URLError, TimeoutError, ValueError) as error:
            # Fail visibly to Alertmanager (it retries) but keep the log content-free.
            print(f"alert-receiver: delivery failed: {type(error).__name__}", file=sys.stderr)
            self._reply(502)
            return
        self._reply(200)


def main() -> int:
    homeserver = _env("ALERTBOT_HOMESERVER_URL").rstrip("/")
    token = _env("ALERTBOT_ACCESS_TOKEN")
    room_id = _env("ALERTBOT_OPS_ROOM_ID")
    port = int(_env("ALERTBOT_LISTEN_PORT", "9095"))
    server = http.server.ThreadingHTTPServer(("0.0.0.0", port), _Handler)  # noqa: S104 (in-cluster)
    server.homeserver = homeserver  # type: ignore[attr-defined]
    server.token = token  # type: ignore[attr-defined]
    server.room_id = room_id  # type: ignore[attr-defined]
    print(f"alert-receiver: listening on :{port}, delivering to the ops room")
    server.serve_forever()
    return 0


if __name__ == "__main__":
    sys.exit(main())
