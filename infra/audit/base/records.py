"""Content-bounded projection of pinned Synapse/MAS rows into closed audit records (issue #418).

This module is the compliance-critical heart of ADR 0018: it turns an exact-version source row
(Synapse 1.155.0 `events`, MAS 1.19.0 `user_session_authentications` + joins) into one of two
Fgentic-owned closed records, and it FAILS CLOSED rather than emit a partial or over-broad record.

The projectors read only explicitly named, allowlisted source columns — a payload, credential, IP,
User-Agent, token, or any other unlisted column present on the source row is never read, so it can
never reach the output. The output records are frozen dataclasses whose field set is closed; adding a
field requires a new schema version. A source row whose column set does not match the pinned
fingerprint (a version drift) is rejected before any field is read.

Nothing here connects to a database; the live collector (a separate #418 task) wires these pure
projectors to the read-only roles, the `on_new_event` wake-up, the durable cursor, and the #157 sink.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from typing import Final

MAS_AUTHENTICATION_SCHEMA: Final = "fgentic.mas_authentication.v1"
MATRIX_EVENT_SCHEMA: Final = "fgentic.matrix_event.v1"
ADMIN_ACTION_SCHEMA: Final = "fgentic.admin_action.v1"

# The exact source column set each pinned query selects. The projector rejects a row whose keys are
# not exactly this set: a missing column (schema regression) and an unexpected extra column (a source
# migration that might widen the surface) both fail closed, satisfying ADR 0018 gate 5 and keeping an
# unknown field from ever flowing through automatically (gate 2).
MATRIX_EVENT_SOURCE_COLUMNS: Final = frozenset(
    {
        "event_id",
        "type",
        "room_id",
        "sender",
        "origin_server_ts",
        "received_ts",
        "stream_ordering",
        "outlier",
        "rejection_reason",
    }
)
MAS_AUTHENTICATION_SOURCE_COLUMNS: Final = frozenset(
    {
        "authentication_id",
        "session_id",
        "occurred_at",
        "username",
        "method",
        "upstream_provider_id",
    }
)

# The only authentication methods ADR 0018 attributes to exactly one committed MAS authentication.
# Anything else (an unmapped or ambiguous method) fails closed rather than guess.
_ALLOWED_MAS_METHODS: Final = frozenset({"password", "upstream_oidc"})


class ProjectionError(ValueError):
    """A source row could not be projected into a closed record; the collection cycle must abort."""


@dataclass(frozen=True, slots=True)
class MatrixEventRecord:
    """Closed `fgentic.matrix_event.v1` record: a non-rejected event Synapse persisted locally."""

    origin_server_ts: int
    received_ts: int
    event_id: str
    room_id: str
    sender: str
    event_type: str
    stream_ordering: int

    @property
    def schema(self) -> str:
        return MATRIX_EVENT_SCHEMA

    @property
    def dedupe_key(self) -> tuple[str, str]:
        """`(schema, event_id)` — the sink deduplicates Matrix records on this."""
        return (MATRIX_EVENT_SCHEMA, self.event_id)

    def as_record(self) -> dict[str, object]:
        """The canonical wire form validated by `fgentic.matrix_event.v1`."""
        return {
            "origin_server_ts": self.origin_server_ts,
            "received_ts": self.received_ts,
            "event_id": self.event_id,
            "room_id": self.room_id,
            "sender": self.sender,
            "event_type": self.event_type,
            "stream_ordering": self.stream_ordering,
        }


@dataclass(frozen=True, slots=True)
class MasAuthenticationRecord:
    """Closed `fgentic.mas_authentication.v1` record: one MAS authentication committed successfully."""

    occurred_at: str
    authentication_id: str
    session_id: str
    matrix_user: str
    method: str
    upstream_provider_id: str | None

    @property
    def schema(self) -> str:
        return MAS_AUTHENTICATION_SCHEMA

    @property
    def dedupe_key(self) -> tuple[str, str]:
        """`(schema, authentication_id)` — the sink deduplicates MAS records on this."""
        return (MAS_AUTHENTICATION_SCHEMA, self.authentication_id)

    def as_record(self) -> dict[str, object]:
        """The canonical wire form validated by `fgentic.mas_authentication.v1`.

        `upstream_provider_id` is OMITTED (not emitted as null) when absent, so a password record
        matches the closed schema's optional-string field rather than sending a JSON ``null``.
        """
        record: dict[str, object] = {
            "occurred_at": self.occurred_at,
            "authentication_id": self.authentication_id,
            "session_id": self.session_id,
            "matrix_user": self.matrix_user,
            "method": self.method,
        }
        if self.upstream_provider_id is not None:
            record["upstream_provider_id"] = self.upstream_provider_id
        return record


def _require_columns(row: dict[str, object], expected: frozenset[str], source: str) -> None:
    actual = frozenset(row)
    if actual != expected:
        missing = sorted(expected - actual)
        unexpected = sorted(actual - expected)
        raise ProjectionError(
            f"{source} row columns do not match the pinned schema fingerprint: "
            f"missing={missing}, unexpected={unexpected}"
        )


def _require_str(value: object, field: str, source: str) -> str:
    if not isinstance(value, str) or value == "":
        raise ProjectionError(f"{source} field {field!r} must be a non-empty string")
    return value


def _require_int(value: object, field: str, source: str) -> int:
    # bool is an int subclass; reject it so a truthy flag can never masquerade as a timestamp.
    if isinstance(value, bool) or not isinstance(value, int):
        raise ProjectionError(f"{source} field {field!r} must be an integer")
    return value


def project_matrix_event(row: dict[str, object]) -> MatrixEventRecord | None:
    """Project one Synapse `events` row into a closed record, or ``None`` if it must not emit.

    Returns ``None`` for an outlier or rejected row (ADR 0018 gate 3: those never emit). Raises
    ``ProjectionError`` on any schema/version drift or malformed value so the cycle fails closed.
    """
    _require_columns(row, MATRIX_EVENT_SOURCE_COLUMNS, "matrix_event")

    outlier = row["outlier"]
    if not isinstance(outlier, bool):
        raise ProjectionError("matrix_event field 'outlier' must be a boolean")
    # A rejected event carries a non-null rejection reason; an accepted one is exactly SQL NULL.
    rejection_reason = row["rejection_reason"]
    if rejection_reason is not None and not isinstance(rejection_reason, str):
        raise ProjectionError("matrix_event field 'rejection_reason' must be a string or null")
    if outlier or rejection_reason is not None:
        return None

    return MatrixEventRecord(
        origin_server_ts=_require_int(row["origin_server_ts"], "origin_server_ts", "matrix_event"),
        received_ts=_require_int(row["received_ts"], "received_ts", "matrix_event"),
        event_id=_require_str(row["event_id"], "event_id", "matrix_event"),
        room_id=_require_str(row["room_id"], "room_id", "matrix_event"),
        sender=_require_str(row["sender"], "sender", "matrix_event"),
        # Synapse names the column `type`; the closed record exposes it as `event_type`.
        event_type=_require_str(row["type"], "type", "matrix_event"),
        stream_ordering=_require_int(row["stream_ordering"], "stream_ordering", "matrix_event"),
    )


def _reconstruct_matrix_user(username: str, server_name: str) -> str:
    """Rebuild `@localpart:server_name` from the MAS username, failing closed on a malformed value.

    The MAS `username` is a bare localpart. A Matrix localpart is a non-empty string of the Matrix
    grammar's allowed characters; anything with a `:`, `@`, whitespace, or an uppercase/other
    disallowed character is malformed and must fail closed rather than produce an unjoinable MXID.
    """
    # server_name is trusted operator config, not row data. Fgentic's convention is a portless
    # authority (fgentic.fmind.ai / fgentic.localhost), so a ':' is treated as malformed rather than
    # a host:port authority; a port-bearing homeserver would need the pattern widened deliberately.
    if server_name == "" or ":" in server_name:
        raise ProjectionError("mas_authentication server_name is not configured or malformed")
    localpart = username
    if localpart == "":
        raise ProjectionError("mas_authentication localpart is empty")
    allowed = set("abcdefghijklmnopqrstuvwxyz0123456789._=-/+")
    if any(character not in allowed for character in localpart):
        raise ProjectionError(f"mas_authentication localpart {username!r} is malformed")
    return f"@{localpart}:{server_name}"


def project_mas_authentication(
    row: dict[str, object],
    server_name: str,
) -> MasAuthenticationRecord:
    """Project one committed MAS authentication row into a closed record.

    Raises ``ProjectionError`` on schema drift, an ambiguous/unknown method, a malformed localpart, or
    any malformed value. The pinned collector query (a separate #418 task) selects only committed
    `user_session_authentications` rows, so a failed MAS attempt never reaches this projector
    (ADR 0018 gate 4); this function does not itself distinguish success from failure.
    """
    _require_columns(row, MAS_AUTHENTICATION_SOURCE_COLUMNS, "mas_authentication")

    method = _require_str(row["method"], "method", "mas_authentication")
    if method not in _ALLOWED_MAS_METHODS:
        raise ProjectionError(f"mas_authentication method {method!r} is not a single allowed method")

    upstream = row["upstream_provider_id"]
    if method == "upstream_oidc":
        upstream_provider_id: str | None = _require_str(upstream, "upstream_provider_id", "mas_authentication")
    else:
        # A password authentication must not carry an upstream provider; a present value is ambiguous.
        if upstream is not None:
            raise ProjectionError("mas_authentication password row must not carry an upstream_provider_id")
        upstream_provider_id = None

    username = _require_str(row["username"], "username", "mas_authentication")
    return MasAuthenticationRecord(
        occurred_at=_require_str(row["occurred_at"], "occurred_at", "mas_authentication"),
        authentication_id=_require_str(row["authentication_id"], "authentication_id", "mas_authentication"),
        session_id=_require_str(row["session_id"], "session_id", "mas_authentication"),
        matrix_user=_reconstruct_matrix_user(username, server_name),
        method=method,
        upstream_provider_id=upstream_provider_id,
    )


# --- Privileged admin-action stream (issue #455) --------------------------------------------------
# A THIRD content-bounded stream under the same ADR 0018 discipline. Unlike the two DB-row projectors
# above, its authoritative source is a LOG LINE, not a database table: the pinned Synapse 1.155.0
# `SynapseRequest` access log projects the authenticated requester, method, redacted admin route, and
# HTTP status. That is the only source that resolves an admin's opaque bearer token to their MXID (the
# gateway/Traefik access log never sees the identity — see the README/ADR non-claims). There is
# therefore NO read-only database grant for this stream. The structured row below is what the deferred
# log adapter (collector.parse_admin_log_message + the pinned log_config) hands this projector; no
# request body, IP, or User-Agent is ever present in it.

# The exact structured field set the deferred access-log adapter supplies per Synapse admin request.
# `position` is a monotonic ingest offset used ONLY as the reconcile cursor/dedup key; it is never
# emitted (as_record omits it). `occurred_at` comes from the pinned log_config formatter timestamp; the
# other four are parsed from the pinned v1.155.0 access-log message. A row whose keys are not exactly
# this set (a format/version drift) fails closed before any field is read.
ADMIN_ACTION_SOURCE_COLUMNS: Final = frozenset({"occurred_at", "acting_entity", "method", "path", "status", "position"})

# Synapse logs this authenticated-entity for an unauthenticated request; such a request is attributable
# to no admin, so it is skipped (returned as None), never guessed or failed closed as drift.
_UNAUTHENTICATED_ENTITIES: Final = frozenset({"None", "-", ""})

# A full MXID, matching the closed schema's acting_admin pattern (never a bare localpart — D6).
_MXID_PATTERN: Final = re.compile(r"^@[a-z0-9._=\-/+]+:[^:]+$")


@dataclass(frozen=True, slots=True)
class _AdminRoute:
    method: str
    path: re.Pattern[str]
    action_class: str


# Pinned Synapse v1.155.0 admin-mutation routes whose action class AND a content-free, non-secret
# target are FULLY determined by the request line alone. Routes deliberately absent here are documented
# non-claims (README / ADR 0018), never silent gaps:
#   - account suspension `PUT /_synapse/admin/v1/suspend/<user_id>`: the suspend/reactivate DIRECTION
#     lives only in the request body `{"suspend": bool}`, a request argument the content-free record
#     structurally excludes; emitting user_suspend vs user_reactivate from the request line would be a
#     guess, so it is not emitted.
#   - registration-token issue/revoke: the target is the secret token itself (in the revoke URL, in the
#     issue response), so no content-free non-secret target exists.
#   - every read (GET) and any other admin route: not a mutation this stream audits.
# Adding a route or action class is a schema/fingerprint change gated offline.
_ADMIN_ROUTES: Final = (
    _AdminRoute("DELETE", re.compile(r"^/_synapse/admin/v[12]/rooms/(?P<target>[^/?]+)$"), "room_purge"),
    _AdminRoute(
        "POST",
        re.compile(r"^/_synapse/admin/v1/media/quarantine/(?P<server>[^/?]+)/(?P<media>[^/?]+)$"),
        "media_quarantine",
    ),
    _AdminRoute(
        "POST", re.compile(r"^/_synapse/admin/v1/room/(?P<target>[^/?]+)/media/quarantine$"), "media_quarantine"
    ),
    _AdminRoute(
        "POST", re.compile(r"^/_synapse/admin/v1/user/(?P<target>[^/?]+)/media/quarantine$"), "media_quarantine"
    ),
    _AdminRoute("DELETE", re.compile(r"^/_synapse/admin/v1/event_reports/(?P<target>[^/?]+)$"), "report_dismiss"),
)


@dataclass(frozen=True, slots=True)
class AdminActionRecord:
    """Closed `fgentic.admin_action.v1` record: one attributed privileged Synapse admin-API mutation."""

    occurred_at: str
    acting_admin: str
    action_class: str
    target: str
    outcome: str
    # Monotonic ingest position of the source access-log line: the reconcile cursor/dedup key. It is NOT
    # part of the closed wire record (as_record omits it), so it never widens the audited field set.
    position: int

    @property
    def schema(self) -> str:
        return ADMIN_ACTION_SCHEMA

    @property
    def dedupe_key(self) -> tuple[str, int]:
        """`(schema, position)` — the sink deduplicates admin-action records on the ingest position."""
        return (ADMIN_ACTION_SCHEMA, self.position)

    def as_record(self) -> dict[str, object]:
        """The canonical wire form validated by `fgentic.admin_action.v1` (position deliberately omitted)."""
        return {
            "occurred_at": self.occurred_at,
            "acting_admin": self.acting_admin,
            "action_class": self.action_class,
            "target": self.target,
            "outcome": self.outcome,
        }


def admin_position(row: dict[str, object]) -> int:
    """Read the monotonic ingest `position` from a source row as the reconcile cursor/dedup key."""
    value = row.get("position")
    # bool is an int subclass; reject it so a flag can never masquerade as a cursor position.
    if isinstance(value, bool) or not isinstance(value, int):
        raise ProjectionError("admin_action field 'position' must be an integer")
    return value


def _outcome_from_status(status: int) -> str:
    """Map an HTTP status to the closed outcome enum: 2xx succeeded, 403 denied, everything else failed."""
    if 200 <= status < 300:
        return "succeeded"
    if status == 403:
        # A non-admin (authenticated but unauthorised) attempt is recorded as denied, never dropped.
        return "denied"
    return "failed"


def _match_admin_route(method: str, path: str) -> tuple[str, str] | None:
    """Map a pinned admin route to `(action_class, target)`, or ``None`` if this stream does not audit it."""
    for route in _ADMIN_ROUTES:
        if route.method != method:
            continue
        match = route.path.match(path)
        if match is None:
            continue
        groups = match.groupdict()
        # The single-media route yields a `<server_name>/<media_id>` locator; every other route names a
        # single `target` group. Both are bounded operational identifiers, never content or a secret.
        target = groups["target"] if "target" in groups else f"{groups['server']}/{groups['media']}"
        if target == "":
            raise ProjectionError(f"admin_action route {method} {path!r} matched with an empty target")
        return route.action_class, target
    return None


def project_admin_action(row: dict[str, object]) -> AdminActionRecord | None:
    """Project one pinned Synapse admin access-log row into a closed record, or ``None`` if none emits.

    Returns ``None`` for an unauthenticated/puppeted request (unattributable to a single admin) or an
    admin route this content-free stream does not audit — both documented non-claims, never a guess.
    Raises ``ProjectionError`` on any schema/version drift, malformed value, or non-sentinel entity that
    is not a full MXID, so the collection cycle fails closed.
    """
    _require_columns(row, ADMIN_ACTION_SOURCE_COLUMNS, "admin_action")

    entity = row["acting_entity"]
    if not isinstance(entity, str):
        raise ProjectionError("admin_action field 'acting_entity' must be a string")
    if entity in _UNAUTHENTICATED_ENTITIES:
        return None
    if "|" in entity:
        # Synapse renders a puppeted/appservice request as `authenticated_entity|requester`; it is not a
        # single human admin action, so it is not attributed here (documented non-claim), not failed closed.
        return None
    if _MXID_PATTERN.match(entity) is None:
        raise ProjectionError(f"admin_action acting_entity {entity!r} is not a full MXID or a known sentinel")

    status = _require_int(row["status"], "status", "admin_action")
    if not (100 <= status <= 599):
        raise ProjectionError(f"admin_action status {status} is not a valid HTTP status code")

    method = _require_str(row["method"], "method", "admin_action")
    path = _require_str(row["path"], "path", "admin_action")
    matched = _match_admin_route(method, path)
    if matched is None:
        return None
    action_class, target = matched

    return AdminActionRecord(
        occurred_at=_require_str(row["occurred_at"], "occurred_at", "admin_action"),
        acting_admin=entity,
        action_class=action_class,
        target=target,
        outcome=_outcome_from_status(status),
        position=admin_position(row),
    )
