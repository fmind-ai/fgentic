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

import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.parse
import urllib.request

# Terminal ghost replies that must NOT count as a successful delegation (mirrors the reply contract
# in scripts/lib/demo-reply.jq): warning/blocked/working placeholders and the empty-content notice.
_UNSUCCESSFUL_PREFIXES = ("⚠️", "\U0001f6d1")  # ⚠️, 🛑
_UNSUCCESSFUL_BODIES = ("⏳ working on it…", "(the agent returned no content)")
_PROVENANCE_BANNER = "--- BEGIN FGENTIC BRIDGE PROVENANCE ---"


def _env(name: str) -> str:
    value = os.environ.get(name, "").strip()
    if not value:
        _fail(f"required environment variable {name} is missing")
    return value


def _fail(message: str) -> "None":
    print(f"canary: {message}", file=sys.stderr)
    raise SystemExit(1)


def _request(method: str, url: str, token: str | None, body: dict | None) -> dict:
    data = json.dumps(body).encode() if body is not None else None
    request = urllib.request.Request(url, data=data, method=method)
    request.add_header("Content-Type", "application/json")
    if token:
        request.add_header("Authorization", f"Bearer {token}")
    try:
        with urllib.request.urlopen(request, timeout=15) as response:  # noqa: S310 (in-cluster HTTP)
            return json.loads(response.read() or b"{}")
    except urllib.error.HTTPError as error:  # 4xx/5xx from Synapse
        _fail(f"{method} {url} failed: HTTP {error.code}")
    except (urllib.error.URLError, TimeoutError, ValueError) as error:
        _fail(f"{method} {url} failed: {error}")
    return {}  # unreachable; _fail raises


def _successful_body(body: object) -> bool:
    if not isinstance(body, str) or not body.strip():
        return False
    if body.startswith(_UNSUCCESSFUL_PREFIXES) or body.startswith(_PROVENANCE_BANNER):
        return False
    return body not in _UNSUCCESSFUL_BODIES


def _reply_succeeded(events: list[dict], ghost: str, probe_event: str) -> bool:
    """True when the ghost's answer to our probe event is a real reply (mirrors demo-reply.jq).

    `events` is the accumulated ghost `m.room.message` history across sync batches — the placeholder
    reply and its later `m.replace` edit routinely arrive in separate syncs. The bridge's terminal
    answer on the async (GetTask-polled) path is an m.replace edit whose `m.relates_to` carries only
    `{rel_type: m.replace, event_id: <reply id>}` and NO `m.in_reply_to`, so the reply is found by
    `m.in_reply_to == probe_event` and the authoritative body is the latest edit targeting that
    reply's own event id, falling back to the reply body for the fast synchronous case.
    """
    replies: dict[str, object] = {}  # reply event id -> reply body (may be a placeholder)
    edits: dict[str, object] = {}  # edited event id -> latest m.new_content body (last wins)
    for event in events:
        if event.get("sender") != ghost:
            continue
        content = event.get("content", {})
        if content.get("msgtype") != "m.notice":
            continue
        relates = content.get("m.relates_to", {})
        if relates.get("rel_type") == "m.replace":
            target = relates.get("event_id")
            new_body = content.get("m.new_content", {}).get("body")
            if isinstance(target, str):
                edits[target] = new_body
        if relates.get("m.in_reply_to", {}).get("event_id") == probe_event:
            event_id = event.get("event_id")
            if isinstance(event_id, str) and event_id:
                replies[event_id] = content.get("body")
    for reply_id, reply_body in replies.items():
        effective = edits.get(reply_id, reply_body)  # latest edit wins, else the reply itself
        if _successful_body(effective):
            return True
    return False


def main() -> int:
    homeserver = _env("CANARY_HOMESERVER_URL").rstrip("/")
    # Under the reference ESS/MSC3861 stack Synapse does not serve password login; the compat
    # /login endpoint is MAS. Sync and send still go to Synapse with the token MAS returns.
    login_url = _env("CANARY_LOGIN_URL").rstrip("/")
    user = _env("CANARY_USER")
    password = _env("CANARY_PASSWORD")
    room_id = _env("CANARY_ROOM_ID")
    ghost = _env("CANARY_TARGET_MXID")
    deadline_seconds = int(os.environ.get("CANARY_DEADLINE_SECONDS", "120"))

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
    since = _request(
        "GET", f"{homeserver}/_matrix/client/v3/sync?timeout=0", token, None
    ).get("next_batch")
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

    # Accumulate the ghost's messages across sync batches: the placeholder reply and its later
    # m.replace edit often land in different syncs.
    ghost_events: list[dict] = []
    deadline = time.monotonic() + deadline_seconds
    while time.monotonic() < deadline:
        encoded_since = urllib.parse.quote(since, safe="")
        sync = _request(
            "GET",
            f"{homeserver}/_matrix/client/v3/sync?timeout=5000&since={encoded_since}",
            token,
            None,
        )
        since = sync.get("next_batch", since)
        timeline = (
            sync.get("rooms", {}).get("join", {}).get(room_id, {}).get("timeline", {}).get("events", [])
        )
        ghost_events.extend(
            event
            for event in timeline
            if event.get("type") == "m.room.message" and event.get("sender") == ghost
        )
        if _reply_succeeded(ghost_events, ghost, probe_event):
            print(f"canary: delegation round-trip ok (nonce {nonce})")
            return 0

    _fail(f"no successful ghost reply within {deadline_seconds}s (nonce {nonce})")
    return 1  # unreachable


if __name__ == "__main__":
    sys.exit(main())
