"""Deterministic contract for the privileged admin-action projector + log parse (issue #455, ADR 0018).

These fixtures prove, without a Kubernetes runtime, that one pinned Synapse admin access-log row projects
to exactly one closed `fgentic.admin_action.v1` record (or fails closed / is a documented non-claim):
the closed field set, structural exclusion of every payload/credential/network field, the action-class
and outcome enums, denied-attempt recording, and fail-closed behaviour on version/format drift. The
capture point is the pinned Synapse 1.155.0 request access log (the only source that resolves an admin's
bearer token to their MXID); there is no database grant for this log-line stream.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from collector import ADMIN_ACCESS_LOG_PATTERN, parse_admin_log_message
from records import (
    ADMIN_ACTION_SCHEMA,
    ADMIN_ACTION_SOURCE_COLUMNS,
    AdminActionRecord,
    ProjectionError,
    project_admin_action,
)
from schemacheck import load_schema, schema_property_names, validate_instance

SCHEMA = load_schema(Path(__file__).resolve().parent.parent / "schemas" / "admin_action.v1.schema.json")

# Payload, credential, network-identity, and request values that must never be a source column, an output
# field, or a value extracted from the raw log line (the ADR's rejected-content list for this stream).
FORBIDDEN_FIELDS = frozenset(
    {
        "content",
        "body",
        "media",
        "media_bytes",
        "password",
        "access_token",
        "refresh_token",
        "token",
        "registration_token",
        "suspend",
        "email",
        "display_name",
        "ip",
        "ip_address",
        "user_agent",
        "request_args",
        "arguments",
        "query",
        "reason_text",
    }
)


def _row(**overrides: object) -> dict[str, object]:
    row: dict[str, object] = {
        "occurred_at": "2026-07-22T00:00:00Z",
        "acting_entity": "@alice:fgentic.localhost",
        "method": "DELETE",
        "path": "/_synapse/admin/v1/rooms/!room:fgentic.localhost",
        "status": 200,
        "position": 100,
    }
    row.update(overrides)
    return row


def _record(**overrides: object) -> AdminActionRecord:
    record = project_admin_action(_row(**overrides))
    assert record is not None
    return record


# --- Source-column fingerprint --------------------------------------------------------------------


def test_source_columns_carry_no_forbidden_field() -> None:
    assert FORBIDDEN_FIELDS.isdisjoint(ADMIN_ACTION_SOURCE_COLUMNS)


def test_missing_or_extra_source_column_fails_closed() -> None:
    for drift in ({"leaked": "x"}, {}):
        row = _row(**drift)
        if not drift:
            del row["status"]
        with pytest.raises(ProjectionError):
            project_admin_action(row)


# --- The closed record set and structural exclusion -----------------------------------------------


def test_projected_record_validates_against_the_closed_schema() -> None:
    validate_instance(_record().as_record(), SCHEMA)


def test_record_wire_form_is_exactly_the_closed_field_set_without_position() -> None:
    wire = _record().as_record()
    assert set(wire) == schema_property_names(SCHEMA)
    # `position` is the reconcile cursor/dedup key on the dataclass, never part of the wire record.
    assert "position" not in wire
    assert FORBIDDEN_FIELDS.isdisjoint(wire)


def test_no_forbidden_value_leaks_into_the_record() -> None:
    # Even if an adapter bug appended forbidden keys to the row, the projector reads only allowlisted
    # columns and fails closed on the extra key rather than passing it through.
    with pytest.raises(ProjectionError):
        project_admin_action(_row(user_agent="Ketesa/1.0", ip="10.0.0.5"))


# --- Action-class mapping (URL-determined, non-secret target) -------------------------------------


@pytest.mark.parametrize(
    ("method", "path", "action_class", "target"),
    [
        ("DELETE", "/_synapse/admin/v1/rooms/!r:fgentic.localhost", "room_purge", "!r:fgentic.localhost"),
        ("DELETE", "/_synapse/admin/v2/rooms/!r:fgentic.localhost", "room_purge", "!r:fgentic.localhost"),
        (
            "POST",
            "/_synapse/admin/v1/media/quarantine/fgentic.localhost/abcMEDIA",
            "media_quarantine",
            "fgentic.localhost/abcMEDIA",
        ),
        (
            "POST",
            "/_synapse/admin/v1/room/!r:fgentic.localhost/media/quarantine",
            "media_quarantine",
            "!r:fgentic.localhost",
        ),
        # Legacy alias on the same v1.155.0 QuarantineMediaInRoom servlet: emits one media_quarantine
        # record (bounded <room_id> target), never a silent capture gap.
        (
            "POST",
            "/_synapse/admin/v1/quarantine_media/!r:fgentic.localhost",
            "media_quarantine",
            "!r:fgentic.localhost",
        ),
        (
            "POST",
            "/_synapse/admin/v1/user/@bob:fgentic.localhost/media/quarantine",
            "media_quarantine",
            "@bob:fgentic.localhost",
        ),
        ("DELETE", "/_synapse/admin/v1/event_reports/42", "report_dismiss", "42"),
    ],
)
def test_each_audited_route_maps_to_its_action_class_and_target(
    method: str, path: str, action_class: str, target: str
) -> None:
    record = _record(method=method, path=path)
    assert record.action_class == action_class
    assert record.target == target
    validate_instance(record.as_record(), SCHEMA)


# --- Outcome mapping, including recorded denials ---------------------------------------------------


@pytest.mark.parametrize(
    ("status", "outcome"),
    [(200, "succeeded"), (201, "succeeded"), (204, "succeeded"), (403, "denied"), (400, "failed"), (500, "failed")],
)
def test_status_maps_to_outcome(status: int, outcome: str) -> None:
    assert _record(status=status).outcome == outcome


def test_denied_non_admin_attempt_is_recorded_with_the_attacker_mxid() -> None:
    # A non-admin who is authenticated but unauthorised gets 403 with their own MXID: recorded as denied,
    # never a silent gap.
    record = _record(acting_entity="@mallory:fgentic.localhost", status=403)
    assert record.acting_admin == "@mallory:fgentic.localhost"
    assert record.outcome == "denied"


# --- Documented non-claims: routes/entities that must NOT be emitted (never guessed) --------------


@pytest.mark.parametrize(
    "path",
    [
        # account suspension: suspend/reactivate direction is body-only (a forbidden request argument).
        "/_synapse/admin/v1/suspend/@bob:fgentic.localhost",
        # registration-token issue/revoke: the target would be the secret token itself.
        "/_synapse/admin/v1/registration_tokens/new",
        "/_synapse/admin/v1/registration_tokens/s3cr3t-token",
        # reads and unlisted admin routes are not mutations this stream audits.
        "/_synapse/admin/v2/users/@bob:fgentic.localhost",
        "/_synapse/admin/v1/rooms/!r:fgentic.localhost/members",
    ],
)
def test_non_audited_admin_route_emits_nothing(path: str) -> None:
    method = "POST" if path.endswith("/new") else "DELETE" if "registration_tokens/" in path else "PUT"
    assert project_admin_action(_row(method=method, path=path)) is None


@pytest.mark.parametrize("entity", ["None", "-", ""])
def test_unauthenticated_request_is_not_attributed(entity: str) -> None:
    assert project_admin_action(_row(acting_entity=entity)) is None


def test_puppeted_or_appservice_request_is_not_attributed() -> None:
    # Synapse renders `authenticated_entity|requester` for a puppeted request: not a single human admin.
    assert project_admin_action(_row(acting_entity="@as:fgentic.localhost|@bob:fgentic.localhost")) is None


# --- Fail-closed on malformed values --------------------------------------------------------------


@pytest.mark.parametrize("entity", ["alice", "@alice", "@Alice:x", "alice:fgentic.localhost"])
def test_non_sentinel_non_mxid_entity_fails_closed(entity: str) -> None:
    with pytest.raises(ProjectionError):
        project_admin_action(_row(acting_entity=entity))


@pytest.mark.parametrize("status", [True, "200", 99, 600, 700])
def test_invalid_status_fails_closed(status: object) -> None:
    with pytest.raises(ProjectionError):
        project_admin_action(_row(status=status))


@pytest.mark.parametrize("position", [True, "100", 3.5])
def test_invalid_position_fails_closed(position: object) -> None:
    with pytest.raises(ProjectionError):
        project_admin_action(_row(position=position))


def test_dedupe_key_is_schema_and_position() -> None:
    assert _record(position=100).dedupe_key == (ADMIN_ACTION_SCHEMA, 100)


# --- The pinned access-log parse (the source fingerprint) -----------------------------------------

# One exact Synapse 1.155.0 access-log completion message. It carries the client IP (leading) and the
# User-Agent (trailing quoted) inline; the parse must extract neither.
_LOG_MESSAGE = (
    "10.0.0.5 - client - {@alice:fgentic.localhost} Processed request: "
    "0.012sec/0.001sec ru=(0.008sec, 0.002sec) db=(0.001sec/0.003sec/2) 128B 200 "
    '"DELETE /_synapse/admin/v1/rooms/!room:fgentic.localhost HTTP/1.1" "Ketesa/1.0" [2 dbevts]'
)


def test_parse_extracts_only_the_content_free_request_fields() -> None:
    parsed = parse_admin_log_message(_LOG_MESSAGE)
    assert parsed == {
        "acting_entity": "@alice:fgentic.localhost",
        "method": "DELETE",
        "path": "/_synapse/admin/v1/rooms/!room:fgentic.localhost",
        "status": 200,
    }


def test_parse_never_captures_the_ip_or_user_agent() -> None:
    parsed = parse_admin_log_message(_LOG_MESSAGE)
    flat = " ".join(str(value) for value in parsed.values())
    assert "10.0.0.5" not in flat  # client IP present in the raw line, never captured
    assert "Ketesa" not in flat  # User-Agent present in the raw line, never captured


def test_parse_reads_an_incomplete_response_code() -> None:
    # Synapse appends `!` to the code when the response is incomplete; the numeric status is still read.
    incomplete = _LOG_MESSAGE.replace(" 200 ", " 200! ")
    assert parse_admin_log_message(incomplete)["status"] == 200


def test_parse_end_to_end_feeds_the_projector() -> None:
    row = parse_admin_log_message(_LOG_MESSAGE) | {"occurred_at": "2026-07-22T00:00:00Z", "position": 100}
    record = project_admin_action(row)
    assert record is not None
    assert record.action_class == "room_purge"
    validate_instance(record.as_record(), SCHEMA)


@pytest.mark.parametrize(
    "message",
    [
        "not an access log line",
        # a drifted format (extra field / renamed segment) must fail closed, not silently mis-parse.
        '10.0.0.5 - client - {@alice:x} Handled request: 200 "GET /x HTTP/1.1"',
        "",
    ],
)
def test_parse_fails_closed_on_format_drift(message: str) -> None:
    assert ADMIN_ACCESS_LOG_PATTERN.match(message) is None
    with pytest.raises(ProjectionError):
        parse_admin_log_message(message)
