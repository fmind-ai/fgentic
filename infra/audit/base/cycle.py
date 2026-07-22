"""One durable reconciliation cycle for the content-bounded audit collector (ADR 0018, issue #418).

Ties the injected query executor, the projectors/reconcile, the durable sink, and the high-water cursor
store into a single crash-safe cycle. Durability ordering is load-bearing: records are written to the
sink BEFORE the cursor advances, so a crash between the two leaves the cursor behind an already-emitted
record — the next cycle re-fetches it and the sink's dedup drops the re-emit (the missed-callback
recovery in reconcile.py). A cycle that fails anywhere (query error, schema drift, ambiguous method,
retrograde cursor, sink write error) aborts WITHOUT advancing the cursor, so nothing is lost or skipped.

The sink and cursor store are injected Protocols, so this orchestration is fully unit-testable offline;
the concrete Postgres executor, the #157 sink, and the durable cursor store are the deferred runtime
adapters that satisfy these interfaces.
"""

from __future__ import annotations

from typing import Protocol

from collector import (
    AdminFetch,
    Execute,
    collect_admin_actions,
    collect_mas_authentications,
    collect_matrix_events,
)
from records import AdminActionRecord, MasAuthenticationRecord, MatrixEventRecord


class MatrixSink(Protocol):
    """The durable operator/auditor-only store for matrix-event records (#157)."""

    def emitted_event_ids(self) -> frozenset[str]:
        """The `event_id`s already persisted, for `(schema, event_id)` dedup.

        Must reflect durably-committed rows; because dedup is by the stable `event_id`, `write` need
        not be atomic — a partial write is reconciled on the next cycle (the committed ids are skipped,
        the rest re-emitted).
        """
        ...

    def write(self, records: list[MatrixEventRecord]) -> None:
        """Durably persist the batch before the cursor advances."""
        ...


class MatrixCursorStore(Protocol):
    """The durable Synapse `stream_ordering` high-water cursor."""

    def load(self) -> int: ...

    def save(self, cursor: int) -> None: ...


class MasSink(Protocol):
    """The durable operator/auditor-only store for MAS-authentication records (#157)."""

    def emitted_authentication_ids(self) -> frozenset[str]:
        """The `authentication_id`s already persisted, for `(schema, authentication_id)` dedup."""
        ...

    def write(self, records: list[MasAuthenticationRecord]) -> None: ...


class MasCursorStore(Protocol):
    """The durable MAS `created_at` high-water cursor (an ISO-8601 timestamp string)."""

    def load(self) -> str: ...

    def save(self, cursor: str) -> None: ...


def run_matrix_collection_cycle(
    execute: Execute,
    sink: MatrixSink,
    cursor_store: MatrixCursorStore,
    limit: int = 500,
) -> int:
    """Run one matrix-event collection cycle; return the number of records written.

    Order: load the cursor, collect + reconcile beyond it, WRITE the records, THEN save the advanced
    cursor. Any exception aborts before the cursor is saved, so the cycle is crash-safe and idempotent
    under the sink's `(schema, event_id)` dedup.
    """
    cursor = cursor_store.load()
    outcome = collect_matrix_events(execute, cursor, sink.emitted_event_ids(), limit)
    sink.write(outcome.records)
    cursor_store.save(outcome.cursor)
    return len(outcome.records)


def run_mas_collection_cycle(
    execute: Execute,
    sink: MasSink,
    cursor_store: MasCursorStore,
    server_name: str,
    limit: int = 500,
) -> int:
    """Run one MAS-authentication collection cycle; return the number of records written.

    MAS has no unique monotonic stream, so the cursor is the `created_at` (occurred_at) high-water and
    the query is inclusive (`>=`): records are written first, then the cursor advances to the newest
    written `occurred_at`, and dedup by `authentication_id` makes the re-fetched boundary rows safe.
    Same crash-safe ordering as the matrix cycle. The cursor advances only when at least one record was
    written; an empty batch keeps the cursor (assumes sub-second `created_at` precision, so a single
    timestamp never holds more than `limit` distinct authentications).
    """
    cursor = cursor_store.load()
    records = collect_mas_authentications(execute, server_name, sink.emitted_authentication_ids(), cursor, limit)
    sink.write(records)
    if records:
        cursor_store.save(max(record.occurred_at for record in records))
    return len(records)


class AdminSink(Protocol):
    """The durable operator/auditor-only store for admin-action records (#157)."""

    def emitted_positions(self) -> frozenset[int]:
        """The log-ingest `position`s already persisted, for `(schema, position)` dedup."""
        ...

    def write(self, records: list[AdminActionRecord]) -> None:
        """Durably persist the batch before the cursor advances."""
        ...


class AdminCursorStore(Protocol):
    """The durable monotonic admin access-log ingest-position high-water cursor."""

    def load(self) -> int: ...

    def save(self, cursor: int) -> None: ...


def run_admin_collection_cycle(
    fetch: AdminFetch,
    sink: AdminSink,
    cursor_store: AdminCursorStore,
    limit: int = 500,
) -> int:
    """Run one admin-action collection cycle; return the number of records written.

    Same crash-safe ordering as the matrix cycle: load the cursor, collect + reconcile the access-log batch
    beyond it, WRITE the records, THEN save the advanced ingest-position cursor. Any exception aborts before
    the cursor is saved, so the cycle is crash-safe and idempotent under the sink's `(schema, position)` dedup.
    """
    cursor = cursor_store.load()
    outcome = collect_admin_actions(fetch, cursor, sink.emitted_positions(), limit)
    sink.write(outcome.records)
    cursor_store.save(outcome.cursor)
    return len(outcome.records)
