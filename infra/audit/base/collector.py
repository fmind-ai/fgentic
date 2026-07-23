"""Pinned source queries + orchestration for the content-bounded audit collector (ADR 0018, #418).

This layer holds the exact SQL the read-only roles execute (selecting ONLY the allowlisted content-free
columns — never event content, credentials, IPs, or User-Agent) and wires each query batch through the
projectors and the cursor/dedup reconcile. The database connection is injected as an `execute` callable,
so the collector is unit-testable offline; the queries themselves are validated against the live
exact-version Synapse 1.155.0 / MAS 1.19.0 schema. The DB-driver plumbing, the `on_new_event` wake-up,
and the #157 sink write wrap this layer as deferred runtime tasks.
"""

from __future__ import annotations

import re
from collections.abc import Callable, Mapping, Sequence

from reconcile import (
    AdminReconcileOutcome,
    MatrixReconcileOutcome,
    reconcile_admin_actions,
    reconcile_mas_authentications,
    reconcile_matrix_events,
)
from records import MasAuthenticationRecord, ProjectionError

# An injected query executor: run `sql` bound with `params` and return each result row as a dict of
# column name -> value. The collector never builds SQL by string interpolation; parameters are bound.
Execute = Callable[[str, Mapping[str, object]], Sequence[dict[str, object]]]

# Synapse events: exactly MATRIX_EVENT_SOURCE_COLUMNS, rows beyond the cursor that have a stream
# position, ordered so the reconcile advances monotonically. Outlier/rejected suppression happens in
# the projector (so the cursor still advances past them), not by filtering the query.
MATRIX_EVENT_QUERY = """
SELECT event_id, type, room_id, sender, origin_server_ts, received_ts,
       stream_ordering, outlier, rejection_reason
FROM public.events
WHERE stream_ordering > %(cursor)s AND stream_ordering IS NOT NULL
ORDER BY stream_ordering ASC
LIMIT %(limit)s
"""

# MAS: one committed authentication per row, joined to its session, user, and (for upstream_oidc) the
# upstream provider. `method` is derived from which mutually-exclusive foreign key is set; the projector
# rejects the 'ambiguous' sentinel and a password row that also carries an upstream provider. Only the
# granted columns are read — never the password hash, email, IP, User-Agent, OAuth code, or token.
MAS_AUTHENTICATION_QUERY = """
SELECT usa.user_session_authentication_id AS authentication_id,
       usa.user_session_id AS session_id,
       usa.created_at AS occurred_at,
       u.username AS username,
       CASE WHEN usa.user_password_id IS NOT NULL THEN 'password'
            WHEN usa.upstream_oauth_authorization_session_id IS NOT NULL THEN 'upstream_oidc'
            ELSE 'ambiguous' END AS method,
       uoas.upstream_oauth_provider_id AS upstream_provider_id
FROM user_session_authentications usa
JOIN user_sessions us ON us.user_session_id = usa.user_session_id
JOIN users u ON u.user_id = us.user_id
LEFT JOIN upstream_oauth_authorization_sessions uoas
       ON uoas.upstream_oauth_authorization_session_id = usa.upstream_oauth_authorization_session_id
WHERE usa.created_at >= %(cursor)s
ORDER BY usa.created_at ASC
LIMIT %(limit)s
"""
# `created_at` is NOT unique (unlike Synapse stream_ordering), so the cursor is inclusive (>=) and the
# reconcile deduplicates by authentication_id: re-fetching the boundary rows at the same timestamp is
# safe (dedup drops the re-emit), whereas a strict > could permanently skip a same-timestamp sibling
# split across a LIMIT boundary. The durable high-water advance is the deferred #157-sink task.


def collect_matrix_events(
    execute: Execute,
    cursor: int,
    already_emitted: frozenset[str],
    limit: int = 500,
) -> MatrixReconcileOutcome:
    """Fetch the next Synapse events batch beyond `cursor` and reconcile it into records + new cursor."""
    rows = execute(MATRIX_EVENT_QUERY, {"cursor": cursor, "limit": limit})
    return reconcile_matrix_events(list(rows), cursor, already_emitted)


def collect_mas_authentications(
    execute: Execute,
    server_name: str,
    already_emitted: frozenset[str],
    cursor: str = "",
    limit: int = 500,
) -> list[MasAuthenticationRecord]:
    """Fetch committed MAS authentications after the `created_at` cursor and reconcile (dedup) them."""
    rows = execute(MAS_AUTHENTICATION_QUERY, {"cursor": cursor, "limit": limit})
    return reconcile_mas_authentications(list(rows), server_name, already_emitted)


# --- Admin actions (#455): a LOG-LINE source, not SQL ---------------------------------------------
# The pinned Synapse 1.155.0 `SynapseRequest` completion access-log message format (synapse/http/site.py
# `_finished_processing`). This regex IS the fingerprint, exactly as the SQL column set is for the DB
# streams: a Synapse whose format string drifts no longer matches and the parse fails closed. It reads
# ONLY the authenticated entity, HTTP status, method, and redacted path. The leading client IP and the
# trailing quoted User-Agent are matched positionally but NEVER captured, so they cannot reach a record.
ADMIN_ACCESS_LOG_PATTERN = re.compile(
    r"^\S+ - \S+ - \{(?P<entity>[^}]*)\} Processed request: "
    r"\S+ ru=\([^)]*\) db=\([^)]*\) \d+B (?P<status>\d+)!? "
    r'"(?P<method>[A-Z]+) (?P<path>\S+) HTTP/[0-9.]+"'
)

# The exact-version source and the pinned access-log format the parse above is bound to. An upgrade that
# changes either fails the offline contract until the fixtures are re-proved (ADR 0018).
ADMIN_SOURCE_SYNAPSE_VERSION = "v1.155.0"

# The deferred log adapter tails the pinned log_config JSON handler, calls parse_admin_log_message on each
# access-log message, adds `occurred_at` (the formatter timestamp) and a monotonic `position` (ingest
# offset), and returns rows beyond `cursor` ordered by position ascending.
AdminFetch = Callable[[int, int], Sequence[dict[str, object]]]


def parse_admin_log_message(message: str) -> dict[str, object]:
    """Parse one pinned Synapse v1.155.0 access-log message into the four content-free request fields.

    Returns `{acting_entity, method, path, status}` — the only fields a record needs. A message that does
    not match the pinned format is treated as format/version drift and fails closed via ``ProjectionError``,
    so the client IP and User-Agent that live in the raw line can never be captured or emitted.
    """
    match = ADMIN_ACCESS_LOG_PATTERN.match(message)
    if match is None:
        raise ProjectionError(
            f"admin access-log message does not match the pinned Synapse {ADMIN_SOURCE_SYNAPSE_VERSION} format"
        )
    return {
        "acting_entity": match.group("entity"),
        "method": match.group("method"),
        "path": match.group("path"),
        "status": int(match.group("status")),
    }


def collect_admin_actions(
    fetch: AdminFetch,
    cursor: int,
    already_emitted: frozenset[int],
    limit: int = 500,
) -> AdminReconcileOutcome:
    """Fetch the next admin access-log batch beyond `cursor` and reconcile it into records + new cursor."""
    rows = fetch(cursor, limit)
    return reconcile_admin_actions(list(rows), cursor, already_emitted)
