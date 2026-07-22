"""Deterministic contract for the admin-action durable cycle orchestration (issue #455, ADR 0018).

Uses a fake sink + cursor store to prove the crash-safe ordering (records written before the ingest-
position cursor advances) and that any failure aborts before the cursor is saved — no lost or skipped
admin record.
"""

from __future__ import annotations

from collections.abc import Sequence

import pytest
from cycle import run_admin_collection_cycle
from records import AdminActionRecord, ProjectionError


class FakeFetch:
    def __init__(self, rows: Sequence[dict[str, object]]) -> None:
        self._rows = list(rows)

    def __call__(self, _cursor: int, _limit: int) -> Sequence[dict[str, object]]:
        return self._rows


class FakeAdminSink:
    def __init__(self, already: frozenset[int] = frozenset()) -> None:
        self._already = already
        self.written: list[AdminActionRecord] = []
        self.log: list[str] = []

    def emitted_positions(self) -> frozenset[int]:
        return self._already

    def write(self, records: list[AdminActionRecord]) -> None:
        self.log.append("write")
        self.written.extend(records)


class FakeCursorStore:
    def __init__(self, cursor: int, log: list[str]) -> None:
        self._cursor = cursor
        self.log = log
        self.saved: int | None = None

    def load(self) -> int:
        return self._cursor

    def save(self, cursor: int) -> None:
        self.log.append("save")
        self.saved = cursor


class RaisingCursorStore:
    def __init__(self, cursor: int) -> None:
        self._cursor = cursor

    def load(self) -> int:
        return self._cursor

    def save(self, cursor: int) -> None:
        raise RuntimeError("cursor store unavailable")


def _row(position: int, *, status: int = 200) -> dict[str, object]:
    return {
        "occurred_at": "2026-07-22T00:00:00Z",
        "acting_entity": "@alice:fgentic.localhost",
        "method": "DELETE",
        "path": "/_synapse/admin/v1/rooms/!r:fgentic.localhost",
        "status": status,
        "position": position,
    }


def test_cycle_writes_records_then_advances_the_cursor() -> None:
    sink = FakeAdminSink()
    store = FakeCursorStore(9, sink.log)
    written = run_admin_collection_cycle(FakeFetch([_row(10), _row(11)]), sink, store)
    assert written == 2
    assert [record.position for record in sink.written] == [10, 11]
    assert store.saved == 11
    assert sink.log == ["write", "save"]  # durability: persisted BEFORE the cursor advances


def test_cycle_uses_the_sink_dedup_set() -> None:
    sink = FakeAdminSink(already=frozenset({10}))
    store = FakeCursorStore(9, sink.log)
    written = run_admin_collection_cycle(FakeFetch([_row(10), _row(11)]), sink, store)
    assert written == 1
    assert [record.position for record in sink.written] == [11]
    assert store.saved == 11  # cursor still advances past the deduped row


def test_cycle_fails_closed_without_advancing_the_cursor() -> None:
    sink = FakeAdminSink()
    store = FakeCursorStore(9, sink.log)
    with pytest.raises(ProjectionError):
        run_admin_collection_cycle(FakeFetch([_row(10) | {"leaked": "x"}]), sink, store)
    assert sink.written == []
    assert store.saved is None


def test_records_are_durable_even_if_the_cursor_save_fails() -> None:
    sink = FakeAdminSink()
    store = RaisingCursorStore(9)
    with pytest.raises(RuntimeError):
        run_admin_collection_cycle(FakeFetch([_row(10)]), sink, store)
    assert [record.position for record in sink.written] == [10]  # written before the save failed


def test_empty_cycle_advances_nothing_but_re_saves_the_cursor() -> None:
    sink = FakeAdminSink()
    store = FakeCursorStore(42, sink.log)
    written = run_admin_collection_cycle(FakeFetch([]), sink, store)
    assert written == 0
    assert store.saved == 42
