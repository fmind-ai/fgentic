"""Deterministic contract for the audit collector's cursor + dedup reconciliation (ADR 0018 §5, #418).

Proves the collector's reconcile state machines without a database: cursor advances monotonically past
emitted and skipped rows, duplicate/recovered callbacks emit each record once, and a retrograde cursor
fails closed.
"""

from __future__ import annotations

from typing import cast

import pytest
from reconcile import (
    reconcile_mas_authentications,
    reconcile_matrix_events,
)
from records import MATRIX_EVENT_SCHEMA, ProjectionError

SERVER_NAME = "fgentic.localhost"


def _event_row(
    stream_ordering: int, event_id: str, *, outlier: bool = False, rejected: bool = False
) -> dict[str, object]:
    return {
        "event_id": event_id,
        "type": "m.room.message",
        "room_id": "!room:fgentic.localhost",
        "sender": "@alice:fgentic.localhost",
        "origin_server_ts": 1_700_000_000_000 + stream_ordering,
        "received_ts": 1_700_000_000_050 + stream_ordering,
        "stream_ordering": stream_ordering,
        "outlier": outlier,
        "rejection_reason": "auth" if rejected else None,
    }


def _mas_row(authentication_id: str) -> dict[str, object]:
    return {
        "authentication_id": authentication_id,
        "session_id": f"sess-{authentication_id}",
        "occurred_at": "2026-07-22T00:00:00Z",
        "username": "alice",
        "method": "password",
        "upstream_provider_id": None,
    }


# --- Matrix reconcile: cursor + dedup ------------------------------------------------------------


def test_accepted_rows_emit_and_advance_the_cursor() -> None:
    rows = [_event_row(10, "$a"), _event_row(11, "$b"), _event_row(12, "$c")]
    outcome = reconcile_matrix_events(rows, cursor=9, already_emitted=frozenset())
    assert [record.event_id for record in outcome.records] == ["$a", "$b", "$c"]
    assert outcome.cursor == 12
    assert all(record.schema == MATRIX_EVENT_SCHEMA for record in outcome.records)


def test_outlier_and_rejected_rows_advance_cursor_without_emitting() -> None:
    rows = [
        _event_row(10, "$a"),
        _event_row(11, "$skip", outlier=True),
        _event_row(12, "$b", rejected=True),
        _event_row(13, "$c"),
    ]
    outcome = reconcile_matrix_events(rows, cursor=9, already_emitted=frozenset())
    assert [record.event_id for record in outcome.records] == ["$a", "$c"]
    # The cursor advances past the suppressed rows so they are not re-fetched forever.
    assert outcome.cursor == 13


def test_duplicate_callback_emits_each_event_once() -> None:
    # A redelivered on_new_event: $a was already emitted; it advances the cursor but is not re-emitted.
    rows = [_event_row(10, "$a"), _event_row(11, "$b")]
    outcome = reconcile_matrix_events(rows, cursor=9, already_emitted=frozenset({"$a"}))
    assert [record.event_id for record in outcome.records] == ["$b"]
    assert outcome.cursor == 11


def test_missed_callback_is_recovered_from_the_cursor() -> None:
    # on_new_event was swallowed for $b/$c; the next cycle re-fetches everything above the cursor and
    # emits the events never yet emitted.
    rows = [_event_row(10, "$a"), _event_row(11, "$b"), _event_row(12, "$c")]
    outcome = reconcile_matrix_events(rows, cursor=9, already_emitted=frozenset({"$a"}))
    assert [record.event_id for record in outcome.records] == ["$b", "$c"]
    assert outcome.cursor == 12


def test_empty_batch_leaves_the_cursor_unchanged() -> None:
    outcome = reconcile_matrix_events([], cursor=42, already_emitted=frozenset())
    assert outcome.records == []
    assert outcome.cursor == 42


def test_retrograde_stream_ordering_fails_closed() -> None:
    rows = [_event_row(10, "$a"), _event_row(10, "$dup")]  # non-advancing cursor
    with pytest.raises(ProjectionError, match="did not advance"):
        reconcile_matrix_events(rows, cursor=9, already_emitted=frozenset())


def test_a_row_at_or_below_the_cursor_fails_closed() -> None:
    with pytest.raises(ProjectionError, match="did not advance"):
        reconcile_matrix_events([_event_row(9, "$old")], cursor=9, already_emitted=frozenset())


def test_non_integer_stream_ordering_fails_closed() -> None:
    row = _event_row(10, "$a") | {"stream_ordering": True}
    with pytest.raises(ProjectionError):
        reconcile_matrix_events([row], cursor=9, already_emitted=frozenset())


@pytest.mark.parametrize("bad_cursor", ["9", 9.0, True, None])
def test_non_integer_cursor_fails_closed(bad_cursor: object) -> None:
    # cast keeps the static call well-typed while exercising the runtime non-integer guard.
    with pytest.raises(ProjectionError, match="cursor must be an integer"):
        reconcile_matrix_events([_event_row(10, "$a")], cursor=cast(int, bad_cursor), already_emitted=frozenset())


def test_schema_drift_in_a_row_fails_the_cycle_closed() -> None:
    row = _event_row(10, "$a") | {"unexpected": "x"}
    with pytest.raises(ProjectionError, match="pinned schema fingerprint"):
        reconcile_matrix_events([row], cursor=9, already_emitted=frozenset())


# --- MAS reconcile: dedup by authentication_id ---------------------------------------------------


def test_mas_rows_emit_and_dedup_within_a_batch() -> None:
    rows = [_mas_row("auth-1"), _mas_row("auth-2"), _mas_row("auth-1")]
    records = reconcile_mas_authentications(rows, SERVER_NAME, already_emitted=frozenset())
    assert [record.authentication_id for record in records] == ["auth-1", "auth-2"]


def test_mas_already_emitted_authentication_is_skipped() -> None:
    rows = [_mas_row("auth-1"), _mas_row("auth-2")]
    records = reconcile_mas_authentications(rows, SERVER_NAME, already_emitted=frozenset({"auth-1"}))
    assert [record.authentication_id for record in records] == ["auth-2"]


def test_mas_ambiguous_method_fails_the_cycle_closed() -> None:
    rows = [_mas_row("auth-1") | {"method": "webauthn"}]
    with pytest.raises(ProjectionError):
        reconcile_mas_authentications(rows, SERVER_NAME, already_emitted=frozenset())
