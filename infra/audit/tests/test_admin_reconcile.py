"""Deterministic contract for the admin-action cursor + dedup reconciliation (issue #455, ADR 0018).

Proves the log-line reconcile without a runtime: the monotonic ingest-position cursor advances past
emitted and non-audited rows, a position already in the sink deduplicates to a single emit, and a
retrograde/duplicate position or any format drift fails closed.
"""

from __future__ import annotations

from typing import cast

import pytest
from reconcile import reconcile_admin_actions
from records import ProjectionError


def _row(
    position: int,
    *,
    path: str = "/_synapse/admin/v1/rooms/!r:fgentic.localhost",
    entity: str = "@alice:fgentic.localhost",
) -> dict[str, object]:
    return {
        "occurred_at": "2026-07-22T00:00:00Z",
        "acting_entity": entity,
        "method": "DELETE",
        "path": path,
        "status": 200,
        "position": position,
    }


def test_ascending_batch_emits_and_advances_the_cursor() -> None:
    outcome = reconcile_admin_actions([_row(10), _row(11)], cursor=9, already_emitted=frozenset())
    assert [record.position for record in outcome.records] == [10, 11]
    assert outcome.cursor == 11


def test_position_already_in_the_sink_deduplicates_but_advances_the_cursor() -> None:
    # Crash-recovery re-fetch: a position already written is dropped exactly once, cursor still advances.
    outcome = reconcile_admin_actions([_row(10), _row(11)], cursor=9, already_emitted=frozenset({10}))
    assert [record.position for record in outcome.records] == [11]
    assert outcome.cursor == 11


def test_non_audited_rows_advance_the_cursor_without_emitting() -> None:
    # A read and an unauthenticated request must not stall the stream: cursor advances, nothing emitted.
    rows = [
        _row(10, path="/_synapse/admin/v1/suspend/@bob:fgentic.localhost", entity="@alice:fgentic.localhost"),
        _row(11, entity="None"),
        _row(12),
    ]
    outcome = reconcile_admin_actions(rows, cursor=9, already_emitted=frozenset())
    assert [record.position for record in outcome.records] == [12]
    assert outcome.cursor == 12


def test_retrograde_or_duplicate_position_fails_closed() -> None:
    for positions in ([11, 11], [11, 10], [9]):
        with pytest.raises(ProjectionError):
            reconcile_admin_actions([_row(position) for position in positions], cursor=9, already_emitted=frozenset())


@pytest.mark.parametrize("cursor", [True, "9", 3.5])
def test_non_integer_cursor_fails_closed(cursor: object) -> None:
    with pytest.raises(ProjectionError):
        reconcile_admin_actions([_row(10)], cursor=cast(int, cursor), already_emitted=frozenset())


def test_format_drift_in_a_row_fails_the_batch_closed() -> None:
    with pytest.raises(ProjectionError):
        reconcile_admin_actions([_row(10) | {"leaked": "x"}], cursor=9, already_emitted=frozenset())
