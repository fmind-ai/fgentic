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

from dataclasses import dataclass
from typing import Final

MAS_AUTHENTICATION_SCHEMA: Final = "fgentic.mas_authentication.v1"
MATRIX_EVENT_SCHEMA: Final = "fgentic.matrix_event.v1"

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
