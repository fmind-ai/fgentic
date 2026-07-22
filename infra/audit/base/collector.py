"""Pinned source queries + orchestration for the content-bounded audit collector (ADR 0018, #418).

This layer holds the exact SQL the read-only roles execute (selecting ONLY the allowlisted content-free
columns — never event content, credentials, IPs, or User-Agent) and wires each query batch through the
projectors and the cursor/dedup reconcile. The database connection is injected as an `execute` callable,
so the collector is unit-testable offline; the queries themselves are validated against the live
exact-version Synapse 1.155.0 / MAS 1.19.0 schema. The DB-driver plumbing, the `on_new_event` wake-up,
and the #157 sink write wrap this layer as deferred runtime tasks.
"""

from __future__ import annotations

from collections.abc import Callable, Mapping, Sequence

from reconcile import (
    MatrixReconcileOutcome,
    reconcile_mas_authentications,
    reconcile_matrix_events,
)
from records import MasAuthenticationRecord

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
