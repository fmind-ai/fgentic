#!/usr/bin/env python3
"""Synthetic delegation canary (issue #454).

Post one nonce-bearing @mention into the dedicated canary room and assert the target ghost's
`m.notice` reply arrives within a deadline. Exit 0 on a successful round-trip, non-zero otherwise —
the CronJob's Job success/failure is what the PrometheusRule alerts on (via kube-state-metrics),
so a broken delegation plane on an idle cluster surfaces as a failing/stale canary Job rather than
silence.

Content-free by construction: the message body is only a fixed marker plus a random nonce and a
timestamp — never real prompts or data. Standard library only, so it runs from the already-pinned
`python:3.14-slim` image with no added dependency. The canary is an ordinary allowlisted sender on
the normal admission/rate path (no privileged bypass); its invocation capacity is bounded by the
CronJob schedule.
"""

from __future__ import annotations

import http.client
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request
from typing import Any, Never

# Terminal ghost replies that must NOT count as a successful delegation (mirrors the reply contract
# in scripts/lib/demo-reply.jq): warning/blocked/working placeholders and the empty-content notice.
_UNSUCCESSFUL_PREFIXES = ("⚠️", "\U0001f6d1")  # ⚠️, 🛑
_UNSUCCESSFUL_BODIES = ("⏳ working on it…", "(the agent returned no content)")
_PROVENANCE_BANNER = "--- BEGIN FGENTIC BRIDGE PROVENANCE ---"
_MAX_RESPONSE_BYTES = 262_144
_MAX_REPLY_STATE = 32
_DEFAULT_DEADLINE_SECONDS = 120
_MAX_DEADLINE_SECONDS = 150
_DEFAULT_REQUEST_TIMEOUT_SECONDS = 15.0
_MAX_SYNC_TIMEOUT_MILLISECONDS = 5_000


def _env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        _fail(f"required environment variable {name} is missing")
    return value


def _fail(message: str) -> Never:
    print(f"canary: {message}", file=sys.stderr)
    raise SystemExit(1)


def _deadline_seconds() -> int:
    raw = os.environ.get("CANARY_DEADLINE_SECONDS", str(_DEFAULT_DEADLINE_SECONDS))
    error = (
        "CANARY_DEADLINE_SECONDS must be a canonical ASCII integer "
        f"from 1 to {_MAX_DEADLINE_SECONDS}"
    )
    if not raw.isascii() or not raw.isdecimal():
        _fail(error)

    # Normalize before int() so an oversized decimal cannot hit Python's digit limit. Requiring the
    # canonical representation also rejects padding and leading zeroes instead of silently fixing it.
    normalized = raw.lstrip("0") or "0"
    if len(normalized) > len(str(_MAX_DEADLINE_SECONDS)):
        _fail(error)
    value = int(normalized)
    if raw != str(value) or not 1 <= value <= _MAX_DEADLINE_SECONDS:
        _fail(error)
    return value


def _sync_request_timeouts(remaining_seconds: float) -> tuple[int, float]:
    long_poll_milliseconds = max(
        1,
        min(_MAX_SYNC_TIMEOUT_MILLISECONDS, int(remaining_seconds * 1_000)),
    )
    transport_seconds = min(_DEFAULT_REQUEST_TIMEOUT_SECONDS, remaining_seconds)
    return long_poll_milliseconds, transport_seconds


def _request(
    method: str,
    url: str,
    token: str | None,
    body: dict | None,
    *,
    timeout_seconds: float = _DEFAULT_REQUEST_TIMEOUT_SECONDS,
) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=data, method=method)
    request.add_header("Content-Type", "application/json")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=timeout_seconds) as response:
            content_lengths = response.headers.get_all("Content-Length", [])
            if len(content_lengths) > 1 or (
                content_lengths
                and (
                    not content_lengths[0].isascii()
                    or not content_lengths[0].isdecimal()
                    or bool(response.headers.get_all("Transfer-Encoding", []))
                )
            ):
                _fail(f"{method} Matrix response has invalid framing")

            declared_length: int | None = None
            if content_lengths:
                normalized_length = content_lengths[0].lstrip("0") or "0"
                if len(normalized_length) > len(str(_MAX_RESPONSE_BYTES)):
                    _fail(f"{method} Matrix response exceeds {_MAX_RESPONSE_BYTES} bytes")
                declared_length = int(normalized_length)
                if declared_length > _MAX_RESPONSE_BYTES:
                    _fail(f"{method} Matrix response exceeds {_MAX_RESPONSE_BYTES} bytes")

            raw = response.read(_MAX_RESPONSE_BYTES + 1)
            if len(raw) > _MAX_RESPONSE_BYTES:
                _fail(f"{method} Matrix response exceeds {_MAX_RESPONSE_BYTES} bytes")
            if declared_length is not None and len(raw) != declared_length:
                _fail(f"{method} Matrix response is incomplete")
            try:
                payload = json.loads(raw or b"{}")
            except (RecursionError, ValueError):
                _fail(f"{method} Matrix response contains invalid JSON")
            if not isinstance(payload, dict):
                _fail(f"{method} Matrix response is not an object")
            return payload
    except urllib.error.HTTPError as error:  # 4xx/5xx from Synapse
        _fail(f"{method} Matrix request failed: HTTP {error.code}")
    except (http.client.HTTPException, urllib.error.URLError, TimeoutError, ValueError) as error:
        _fail(f"{method} Matrix request failed: {type(error).__name__}")
    return {}  # unreachable; _fail raises


def _successful_body(body: object) -> bool:
    if not isinstance(body, str):
        return False
    normalized = body.strip()
    if not normalized:
        return False
    if normalized.startswith(_UNSUCCESSFUL_PREFIXES) or normalized.startswith(_PROVENANCE_BANNER):
        return False
    return normalized not in _UNSUCCESSFUL_BODIES


class _ReplyTracker:
    def __init__(self, ghost: str, probe_event: str) -> None:
        self._ghost = ghost
        self._probe_event = probe_event
        self._replies: dict[str, bool] = {}

    def _reserve(self, event_id: str) -> None:
        if event_id in self._replies:
            return
        if len(self._replies) >= _MAX_REPLY_STATE:
            _fail(f"reply correlation state exceeds {_MAX_REPLY_STATE} events")

    def observe(self, events: list[Any]) -> bool:
        for event in events:
            if not isinstance(event, dict) or event.get("sender") != self._ghost:
                continue
            content = event.get("content", {})
            if not isinstance(content, dict) or content.get("msgtype") != "m.notice":
                continue
            relates = content.get("m.relates_to", {})
            if not isinstance(relates, dict):
                continue

            if relates.get("rel_type") == "m.replace":
                target = relates.get("event_id")
                new_content = content.get("m.new_content", {})
                new_body = new_content.get("body") if isinstance(new_content, dict) else None
                if isinstance(target, str) and target in self._replies:
                    self._replies[target] = _successful_body(new_body)
                continue

            in_reply_to = relates.get("m.in_reply_to", {})
            if not isinstance(in_reply_to, dict) or in_reply_to.get("event_id") != self._probe_event:
                continue
            event_id = event.get("event_id")
            if not isinstance(event_id, str) or not event_id:
                continue
            self._reserve(event_id)
            self._replies[event_id] = _successful_body(content.get("body"))
        return any(self._replies.values())


def _reply_succeeded(events: list[dict], ghost: str, probe_event: str) -> bool:
    """True when the ghost's answer to our probe event is a real reply (mirrors demo-reply.jq).

    `events` is the accumulated ghost `m.room.message` history across sync batches — the placeholder
    reply and its later `m.replace` edit routinely arrive in separate syncs. The bridge's terminal
    answer on the async (GetTask-polled) path is an m.replace edit whose `m.relates_to` carries only
    `{rel_type: m.replace, event_id: <reply id>}` and NO `m.in_reply_to`, so the reply is found by
    `m.in_reply_to == probe_event` and the authoritative body is the latest edit targeting that
    reply's own event id, falling back to the reply body for the fast synchronous case.
    """
    return _ReplyTracker(ghost, probe_event).observe(events)


def _timeline(sync: dict, room_id: str) -> list[Any]:
    rooms = sync.get("rooms", {})
    if not isinstance(rooms, dict):
        _fail("sync response rooms is not an object")
    joined = rooms.get("join", {})
    if not isinstance(joined, dict):
        _fail("sync response joined rooms is not an object")
    room = joined.get(room_id, {})
    if not isinstance(room, dict):
        _fail("sync response canary room is not an object")
    timeline = room.get("timeline", {})
    if not isinstance(timeline, dict):
        _fail("sync response timeline is not an object")
    events = timeline.get("events", [])
    if not isinstance(events, list):
        _fail("sync response events is not a list")
    return events


def main() -> int:
    homeserver = _env("CANARY_HOMESERVER_URL").rstrip("/")
    # Under the reference ESS/MSC3861 stack Synapse does not serve password login; the compat
    # /login endpoint is MAS. Sync and send still go to Synapse with the token MAS returns.
    login_url = _env("CANARY_LOGIN_URL").rstrip("/")
    user = _env("CANARY_USER")
    password = _env("CANARY_PASSWORD")
    room_id = _env("CANARY_ROOM_ID")
    ghost = _env("CANARY_TARGET_MXID")
    deadline_seconds = _deadline_seconds()

    login = _request(
        "POST",
        f"{login_url}/_matrix/client/v3/login",
        None,
        {
            "type": "m.login.password",
            "identifier": {"type": "m.id.user", "user": user},
            "password": password,
            "initial_device_display_name": "fgentic-canary",
        },
    )
    token = login.get("access_token")
    if not isinstance(token, str) or not token:
        _fail("login did not return an access token")

    # Establish a sync cursor BEFORE sending, so we only match replies to this probe.
    since = _request("GET", f"{homeserver}/_matrix/client/v3/sync?timeout=0", token, None).get("next_batch")
    if not isinstance(since, str) or not since:
        _fail("initial sync did not return a batch cursor")

    nonce = secrets.token_hex(8)
    encoded_room = urllib.parse.quote(room_id, safe="")
    probe = _request(
        "PUT",
        f"{homeserver}/_matrix/client/v3/rooms/{encoded_room}/send/m.room.message/canary-{nonce}",
        token,
        {
            "msgtype": "m.text",
            # Content-free: fixed marker + nonce + timestamp only.
            "body": f"fgentic delegation canary {nonce} at {int(time.time())}",
            "m.mentions": {"user_ids": [ghost]},
        },
    )
    probe_event = probe.get("event_id")
    if not isinstance(probe_event, str) or not probe_event:
        _fail("sending the canary mention did not return an event id")

    # Retain only bounded reply ids and success bits across batches; never accumulate event bodies.
    replies = _ReplyTracker(ghost, probe_event)
    deadline = time.monotonic() + deadline_seconds
    while True:
        remaining_seconds = deadline - time.monotonic()
        if remaining_seconds <= 0:
            break
        sync_timeout_milliseconds, transport_timeout_seconds = _sync_request_timeouts(
            remaining_seconds
        )
        encoded_since = urllib.parse.quote(since, safe="")
        sync = _request(
            "GET",
            (
                f"{homeserver}/_matrix/client/v3/sync"
                f"?timeout={sync_timeout_milliseconds}&since={encoded_since}"
            ),
            token,
            None,
            timeout_seconds=transport_timeout_seconds,
        )
        if time.monotonic() >= deadline:
            break
        next_batch = sync.get("next_batch")
        if not isinstance(next_batch, str) or not next_batch:
            _fail("sync response did not return a batch cursor")
        since = next_batch
        events = [
            event
            for event in _timeline(sync, room_id)
            if isinstance(event, dict) and event.get("type") == "m.room.message"
        ]
        if replies.observe(events):
            print(f"canary: delegation round-trip ok (nonce {nonce})")
            return 0

    _fail(f"no successful ghost reply within {deadline_seconds}s (nonce {nonce})")
    return 1  # unreachable


if __name__ == "__main__":
    sys.exit(main())
