"""Governance: the read-only collector roles grant SELECT on exactly the allowlisted content-free
columns and never a forbidden one (issue #418 Task 4, ADR 0018).

Every granted table is pinned to an exact per-table column allowlist: `events` to the projector's
`MATRIX_EVENT_SOURCE_COLUMNS` (base/records.py), the MAS tables to explicit physical-column sets. A
widened, drifted, or whole-table grant — or any payload/credential/PII column — fails
check:identity-audit. Validated live: the role reads the allowlisted columns but is denied `content`.
"""

from __future__ import annotations

import re
from pathlib import Path

from records import MATRIX_EVENT_SOURCE_COLUMNS

SQL = (Path(__file__).resolve().parent.parent / "sql" / "read-only-roles.sql").read_text(encoding="utf-8")

# The exact physical columns each table may expose to the collector. `events` is the projector's own
# source allowlist; the MAS tables are the join columns the post-join projector row needs. Any drift
# between this map and the SQL fails the gate in both directions.
EXPECTED_GRANTS: dict[str, frozenset[str]] = {
    "events": frozenset(MATRIX_EVENT_SOURCE_COLUMNS),
    "user_session_authentications": frozenset(
        {
            "user_session_authentication_id",
            "user_session_id",
            "created_at",
            "user_password_id",
            "upstream_oauth_authorization_session_id",
        }
    ),
    "user_sessions": frozenset({"user_session_id", "user_id"}),
    "users": frozenset({"user_id", "username"}),
    "upstream_oauth_authorization_sessions": frozenset(
        {"upstream_oauth_authorization_session_id", "upstream_oauth_provider_id"}
    ),
}

# Payload, credential, contact, network-identity, and request columns from ADR 0018 gate 1 that must
# never be granted, whatever table they live on.
FORBIDDEN_COLUMNS = frozenset(
    {
        "content",
        "unrecognized_keys",
        "unsigned",
        "password",
        "hashed_password",
        "password_hash",
        "email",
        "display_name",
        "displayname",
        "ip",
        "ip_address",
        "last_active_ip",
        "user_agent",
        "last_active_user_agent",
        "cookie",
        "access_token",
        "refresh_token",
        "authorization_code",
        "code",
        "redirect_uri",
        "client_secret",
        "query",
        "request_query",
    }
)

_GRANT_SELECT_COLUMN_SCOPED = re.compile(
    r"GRANT\s+SELECT\s*\(([^)]*)\)\s*ON\s+public\.([A-Za-z_][A-Za-z0-9_]*)",
    re.IGNORECASE | re.DOTALL,
)


def _column_scoped_grants() -> dict[str, set[str]]:
    grants: dict[str, set[str]] = {}
    for match in _GRANT_SELECT_COLUMN_SCOPED.finditer(SQL):
        columns = {column.strip() for column in match.group(1).split(",") if column.strip()}
        grants.setdefault(match.group(2), set()).update(columns)
    return grants


def test_every_granted_table_matches_its_exact_allowlist() -> None:
    grants = _column_scoped_grants()
    assert set(grants) == set(EXPECTED_GRANTS), "a table is granted that is not in the allowlist (or vice versa)"
    for table, expected in EXPECTED_GRANTS.items():
        assert grants[table] == set(expected), f"{table} grant drifted from its allowlist"


def test_events_grant_is_tied_to_the_projector_source_allowlist() -> None:
    assert _column_scoped_grants()["events"] == set(MATRIX_EVENT_SOURCE_COLUMNS)


def test_no_forbidden_column_is_ever_granted() -> None:
    all_granted: set[str] = set()
    for columns in _column_scoped_grants().values():
        all_granted |= columns
    assert FORBIDDEN_COLUMNS.isdisjoint(all_granted)


def test_no_grant_select_is_whole_table_or_schema_wide() -> None:
    # Every `GRANT SELECT` must be immediately followed by a column list `(...)`. A whole-table grant
    # (`GRANT SELECT ON [TABLE] public.x`, `GRANT SELECT ON ALL TABLES IN SCHEMA ...`) reads every
    # column including content, so its next non-whitespace token is `O` (of ON), not `(` — and fails.
    for match in re.finditer(r"GRANT\s+SELECT\s*(\S)", SQL, re.IGNORECASE):
        assert match.group(1) == "(", f"non-column-scoped GRANT SELECT: ...{match.group(0)!r}"


def test_whole_table_check_would_catch_an_evasion() -> None:
    # Prove the column-scoped guard is not a no-op: every whole-table / schema-wide shape is caught.
    for evasion in (
        "GRANT SELECT ON public.users TO r;",
        "GRANT SELECT ON TABLE public.users TO r;",
        "GRANT SELECT ON ALL TABLES IN SCHEMA public TO r;",
    ):
        match = re.search(r"GRANT\s+SELECT\s*(\S)", evasion, re.IGNORECASE)
        assert match is not None
        assert match.group(1) != "("
