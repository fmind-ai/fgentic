"""Deterministic contract for the audit collector's query orchestration (issue #418, ADR 0018).

Uses an injected fake executor (no database) to prove the collector runs the pinned parameterised
queries, feeds the rows through the projectors + reconcile, and fails closed on a bad row. A governance
check ties the Synapse query's selected columns to the projector allowlist and forbids any content or
credential column in either query. The queries themselves are validated against the live exact-version
Synapse 1.155.0 / MAS 1.19.0 schema.
"""

from __future__ import annotations

import re
from collections.abc import Mapping, Sequence

import pytest
from collector import (
    MAS_AUTHENTICATION_QUERY,
    MATRIX_EVENT_QUERY,
    collect_mas_authentications,
    collect_matrix_events,
)
from records import (
    MAS_AUTHENTICATION_SOURCE_COLUMNS,
    MATRIX_EVENT_SOURCE_COLUMNS,
    ProjectionError,
)

SERVER_NAME = "fgentic.localhost"

FORBIDDEN_IN_QUERY = (
    "content",
    "unrecognized_keys",
    "password_hash",
    "hashed_password",
    "email",
    "user_agent",
    "access_token",
    "refresh_token",
    "authorization_code",
    "client_secret",
    "redirect_uri",
)


class FakeExecutor:
    """Records the SQL/params it was called with and returns pre-seeded rows."""

    def __init__(self, rows: Sequence[dict[str, object]]) -> None:
        self._rows = list(rows)
        self.calls: list[tuple[str, Mapping[str, object]]] = []

    def __call__(self, sql: str, params: Mapping[str, object]) -> Sequence[dict[str, object]]:
        self.calls.append((sql, dict(params)))
        return self._rows


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


def test_collect_matrix_events_runs_the_query_and_reconciles() -> None:
    executor = FakeExecutor([_event_row(10, "$a"), _event_row(11, "$b")])
    outcome = collect_matrix_events(executor, cursor=9, already_emitted=frozenset(), limit=200)
    assert [record.event_id for record in outcome.records] == ["$a", "$b"]
    assert outcome.cursor == 11
    sql, params = executor.calls[0]
    assert sql == MATRIX_EVENT_QUERY
    assert params == {"cursor": 9, "limit": 200}


def test_collect_matrix_events_fails_closed_on_a_bad_row() -> None:
    executor = FakeExecutor([_event_row(10, "$a") | {"leaked": "x"}])
    with pytest.raises(ProjectionError):
        collect_matrix_events(executor, cursor=9, already_emitted=frozenset())


def test_collect_matrix_events_dedups_already_emitted() -> None:
    executor = FakeExecutor([_event_row(10, "$a"), _event_row(11, "$b")])
    outcome = collect_matrix_events(executor, cursor=9, already_emitted=frozenset({"$a"}))
    assert [record.event_id for record in outcome.records] == ["$b"]
    assert outcome.cursor == 11


def test_collect_mas_authentications_runs_query_and_dedups() -> None:
    executor = FakeExecutor([_mas_row("auth-1"), _mas_row("auth-2"), _mas_row("auth-1")])
    records = collect_mas_authentications(executor, SERVER_NAME, already_emitted=frozenset())
    assert [record.authentication_id for record in records] == ["auth-1", "auth-2"]
    sql, params = executor.calls[0]
    assert sql == MAS_AUTHENTICATION_QUERY
    assert "cursor" in params
    assert "limit" in params


def test_collect_mas_authentications_fails_closed_on_ambiguous_method() -> None:
    executor = FakeExecutor([_mas_row("auth-1") | {"method": "ambiguous"}])
    with pytest.raises(ProjectionError):
        collect_mas_authentications(executor, SERVER_NAME, already_emitted=frozenset())


def test_matrix_query_selects_exactly_the_projector_allowlist() -> None:
    select_clause = re.search(r"SELECT\s+(.*?)\s+FROM", MATRIX_EVENT_QUERY, re.IGNORECASE | re.DOTALL)
    assert select_clause is not None
    selected = {column.strip() for column in select_clause.group(1).split(",") if column.strip()}
    assert selected == set(MATRIX_EVENT_SOURCE_COLUMNS)


def test_mas_query_aliases_match_the_projector_allowlist() -> None:
    # The `AS <alias>` output names must equal MAS_AUTHENTICATION_SOURCE_COLUMNS exactly, or the
    # projector's exact-column check fails closed on the post-join row.
    aliases = {match.group(1) for match in re.finditer(r"\bAS\s+(\w+)", MAS_AUTHENTICATION_QUERY)}
    assert aliases == set(MAS_AUTHENTICATION_SOURCE_COLUMNS)


def test_neither_query_references_a_forbidden_column() -> None:
    for query in (MATRIX_EVENT_QUERY, MAS_AUTHENTICATION_QUERY):
        for forbidden in FORBIDDEN_IN_QUERY:
            assert not re.search(rf"\b{re.escape(forbidden)}\b", query), forbidden
