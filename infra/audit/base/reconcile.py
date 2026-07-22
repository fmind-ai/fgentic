"""Cursor + dedup reconciliation for the content-bounded audit collector (ADR 0018, issue #418).

These are the pure reconciliation state machines the live collector wraps around the read-only source
queries: they turn an ordered batch of already-fetched source rows into records to emit, advancing a
durable high-water cursor and deduplicating so a duplicate `on_new_event` wake-up or a missed callback
recovered from the cursor emits each record exactly once. They FAIL CLOSED (raise ``ProjectionError``)
on a retrograde/duplicate cursor rather than emit a partial or double record.

No database access lives here — the collector supplies the fetched rows (from a dedicated read-only
role with column-level grants) and the already-emitted keys (from the #157 sink); this layer is fully
unit-testable offline, matching ADR 0018's "prove the queries without a Kubernetes runtime".
"""

from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass

from records import (
    AdminActionRecord,
    MasAuthenticationRecord,
    MatrixEventRecord,
    ProjectionError,
    admin_position,
    project_admin_action,
    project_mas_authentication,
    project_matrix_event,
)


@dataclass(frozen=True, slots=True)
class MatrixReconcileOutcome:
    """Records to emit this cycle plus the advanced Synapse `stream_ordering` high-water cursor."""

    records: list[MatrixEventRecord]
    cursor: int


def _stream_ordering(row: dict[str, object]) -> int:
    value = row.get("stream_ordering")
    # bool is an int subclass; reject it so a flag can never masquerade as a cursor position.
    if isinstance(value, bool) or not isinstance(value, int):
        raise ProjectionError("matrix_event field 'stream_ordering' must be an integer")
    return value


def reconcile_matrix_events(
    rows: Iterable[dict[str, object]],
    cursor: int,
    already_emitted: frozenset[str],
) -> MatrixReconcileOutcome:
    """Reconcile a batch of Synapse `events` rows ordered by `stream_ordering` ascending.

    `cursor` is the durable high-water (last processed `stream_ordering`); the pinned query returns only
    rows beyond it. `already_emitted` is the set of `event_id`s the sink already holds — the dedup key
    is `(schema, event_id)` (ADR 0018 §5). `on_new_event` is only an at-least-once wake-up: a redelivered
    or cursor-recovered row whose `event_id` was already emitted advances the cursor but is not emitted
    again. A row whose `stream_ordering` does not strictly advance the cursor fails the cycle closed.
    """
    if isinstance(cursor, bool) or not isinstance(cursor, int):
        # ADR 0018 §5 lists a non-integer cursor as a fail-closed case; surface the typed audit error
        # rather than a bare TypeError at the comparison below.
        raise ProjectionError("matrix reconcile cursor must be an integer")
    records: list[MatrixEventRecord] = []
    next_cursor = cursor
    # No within-batch dedup set is needed here (unlike the MAS reconcile): Synapse's `event_id` is
    # unique and `stream_ordering` is unique + strictly ascending, so a same-position duplicate is
    # caught by the strict-advance guard and a same-`event_id` row cannot recur in one ordered batch.
    for row in rows:
        # project_matrix_event validates the full pinned column set first, so a schema drift fails here.
        record = project_matrix_event(row)
        position = _stream_ordering(row)
        if position <= next_cursor:
            raise ProjectionError(
                f"matrix cursor did not advance (retrograde or duplicate stream_ordering {position} "
                f"at or below cursor {next_cursor})"
            )
        next_cursor = position
        if record is None:
            # Outlier or rejected: never emitted, but the cursor still advances past it (ADR gate 3).
            continue
        if record.event_id in already_emitted:
            continue
        records.append(record)
    return MatrixReconcileOutcome(records=records, cursor=next_cursor)


def reconcile_mas_authentications(
    rows: Iterable[dict[str, object]],
    server_name: str,
    already_emitted: frozenset[str],
) -> list[MasAuthenticationRecord]:
    """Reconcile a batch of committed MAS `user_session_authentications` rows.

    Deduplicates by `(schema, authentication_id)` (ADR 0018 §5): an `authentication_id` the sink already
    holds is skipped so a redelivered row emits once. Any schema drift, ambiguous method, or malformed
    localpart fails the cycle closed via the projector.
    """
    records: list[MasAuthenticationRecord] = []
    seen_in_batch: set[str] = set()
    for row in rows:
        record = project_mas_authentication(row, server_name)
        if record.authentication_id in already_emitted or record.authentication_id in seen_in_batch:
            continue
        seen_in_batch.add(record.authentication_id)
        records.append(record)
    return records


@dataclass(frozen=True, slots=True)
class AdminReconcileOutcome:
    """Admin-action records to emit this cycle plus the advanced monotonic log-position high-water cursor."""

    records: list[AdminActionRecord]
    cursor: int


def reconcile_admin_actions(
    rows: Iterable[dict[str, object]],
    cursor: int,
    already_emitted: frozenset[int],
) -> AdminReconcileOutcome:
    """Reconcile a batch of pinned Synapse admin access-log rows ordered by ingest `position` ascending.

    `cursor` is the durable high-water ingest position; the fetch returns only rows beyond it. The log
    line has no natural stable id, so the monotonic append-only ingest position is BOTH the cursor and the
    `(schema, position)` dedup key: a crash between the sink write and the cursor save re-fetches from the
    old cursor and `already_emitted` (positions already in the sink) drops the re-emit exactly once. A row
    whose `position` does not strictly advance the cursor fails the cycle closed (retrograde/duplicate), and
    any schema/format drift fails closed in the projector before a partial record is emitted.
    """
    if isinstance(cursor, bool) or not isinstance(cursor, int):
        raise ProjectionError("admin reconcile cursor must be an integer")
    records: list[AdminActionRecord] = []
    next_cursor = cursor
    for row in rows:
        # project_admin_action validates the full pinned column set first, so a format drift fails here.
        record = project_admin_action(row)
        position = admin_position(row)
        if position <= next_cursor:
            raise ProjectionError(
                f"admin cursor did not advance (retrograde or duplicate position {position} "
                f"at or below cursor {next_cursor})"
            )
        next_cursor = position
        if record is None:
            # Unauthenticated/puppeted request or a non-audited route: never emitted, but the cursor still
            # advances past it so the stream cannot stall on ordinary admin traffic.
            continue
        if record.position in already_emitted:
            continue
        records.append(record)
    return AdminReconcileOutcome(records=records, cursor=next_cursor)
