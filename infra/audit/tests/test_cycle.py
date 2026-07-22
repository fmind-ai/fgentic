"""Deterministic contract for the audit collector's durable cycle orchestration (issue #418, ADR 0018).

Uses fake sink + cursor store to prove the crash-safe ordering (records written before the cursor
advances) and that any failure aborts before the cursor is saved — no lost or skipped record.
"""

from __future__ import annotations

from collections.abc import Mapping, Sequence

import pytest
from cycle import run_mas_collection_cycle, run_matrix_collection_cycle
from records import MasAuthenticationRecord, MatrixEventRecord, ProjectionError

SERVER_NAME = "fgentic.localhost"


class FakeExecutor:
    def __init__(self, rows: Sequence[dict[str, object]]) -> None:
        self._rows = list(rows)

    def __call__(self, _sql: str, _params: Mapping[str, object]) -> Sequence[dict[str, object]]:
        return self._rows


class FakeMatrixSink:
    def __init__(self, already: frozenset[str] = frozenset()) -> None:
        self._already = already
        self.written: list[MatrixEventRecord] = []
        self.log: list[str] = []

    def emitted_event_ids(self) -> frozenset[str]:
        return self._already

    def write(self, records: list[MatrixEventRecord]) -> None:
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


class FakeMasSink:
    def __init__(self, already: frozenset[str] = frozenset()) -> None:
        self._already = already
        self.written: list[MasAuthenticationRecord] = []

    def emitted_authentication_ids(self) -> frozenset[str]:
        return self._already

    def write(self, records: list[MasAuthenticationRecord]) -> None:
        self.written.extend(records)


class FakeMasCursorStore:
    def __init__(self, cursor: str = "") -> None:
        self._cursor = cursor
        self.saved: str | None = None

    def load(self) -> str:
        return self._cursor

    def save(self, cursor: str) -> None:
        self.saved = cursor


def _event_row(stream_ordering: int, event_id: str) -> dict[str, object]:
    return {
        "event_id": event_id,
        "type": "m.room.message",
        "room_id": "!room:fgentic.localhost",
        "sender": "@alice:fgentic.localhost",
        "origin_server_ts": 1_700_000_000_000 + stream_ordering,
        "received_ts": 1_700_000_000_050 + stream_ordering,
        "stream_ordering": stream_ordering,
        "outlier": False,
        "rejection_reason": None,
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


def test_matrix_cycle_writes_records_then_advances_the_cursor() -> None:
    sink = FakeMatrixSink()
    store = FakeCursorStore(9, sink.log)
    written = run_matrix_collection_cycle(FakeExecutor([_event_row(10, "$a"), _event_row(11, "$b")]), sink, store)
    assert written == 2
    assert [record.event_id for record in sink.written] == ["$a", "$b"]
    assert store.saved == 11
    # Durability: the batch is persisted BEFORE the cursor advances.
    assert sink.log == ["write", "save"]


def test_matrix_cycle_uses_the_sink_dedup_set() -> None:
    sink = FakeMatrixSink(already=frozenset({"$a"}))
    store = FakeCursorStore(9, sink.log)
    written = run_matrix_collection_cycle(FakeExecutor([_event_row(10, "$a"), _event_row(11, "$b")]), sink, store)
    assert written == 1
    assert [record.event_id for record in sink.written] == ["$b"]
    assert store.saved == 11  # cursor still advances past the deduped row


def test_matrix_cycle_fails_closed_without_advancing_the_cursor() -> None:
    sink = FakeMatrixSink()
    store = FakeCursorStore(9, sink.log)
    with pytest.raises(ProjectionError):
        run_matrix_collection_cycle(FakeExecutor([_event_row(10, "$a") | {"leaked": "x"}]), sink, store)
    assert sink.written == []
    assert store.saved is None  # neither write nor cursor advance happened


class RaisingMatrixCursorStore:
    def __init__(self, cursor: int) -> None:
        self._cursor = cursor

    def load(self) -> int:
        return self._cursor

    def save(self, cursor: int) -> None:
        raise RuntimeError("cursor store unavailable")


def test_matrix_records_are_durable_even_if_the_cursor_save_fails() -> None:
    # The crash window the design hinges on: write committed, save throws. Records stay durable; the
    # stale cursor re-fetches them next cycle and dedup drops the re-emit.
    sink = FakeMatrixSink()
    store = RaisingMatrixCursorStore(9)
    with pytest.raises(RuntimeError):
        run_matrix_collection_cycle(FakeExecutor([_event_row(10, "$a")]), sink, store)
    assert [record.event_id for record in sink.written] == ["$a"]  # written before the save failed


def test_matrix_empty_cycle_advances_nothing_but_re_saves_the_cursor() -> None:
    sink = FakeMatrixSink()
    store = FakeCursorStore(42, sink.log)
    written = run_matrix_collection_cycle(FakeExecutor([]), sink, store)
    assert written == 0
    assert store.saved == 42


def test_mas_cycle_writes_deduped_records_and_advances_cursor() -> None:
    sink = FakeMasSink(already=frozenset({"auth-1"}))
    store = FakeMasCursorStore()
    written = run_mas_collection_cycle(FakeExecutor([_mas_row("auth-1"), _mas_row("auth-2")]), sink, store, SERVER_NAME)
    assert written == 1
    assert [record.authentication_id for record in sink.written] == ["auth-2"]
    assert store.saved == "2026-07-22T00:00:00Z"  # advanced to the newest written occurred_at


def test_mas_cycle_with_no_new_records_keeps_the_cursor() -> None:
    sink = FakeMasSink(already=frozenset({"auth-1"}))
    store = FakeMasCursorStore(cursor="2026-07-22T00:00:00Z")
    written = run_mas_collection_cycle(FakeExecutor([_mas_row("auth-1")]), sink, store, SERVER_NAME)
    assert written == 0
    assert store.saved is None  # all deduped: cursor unchanged, re-fetched next cycle


def test_mas_cycle_fails_closed_on_ambiguous_method() -> None:
    sink = FakeMasSink()
    store = FakeMasCursorStore()
    with pytest.raises(ProjectionError):
        run_mas_collection_cycle(FakeExecutor([_mas_row("auth-1") | {"method": "ambiguous"}]), sink, store, SERVER_NAME)
    assert sink.written == []
    assert store.saved is None
