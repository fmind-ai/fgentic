"""Deterministic contract for the content-bounded audit projectors (ADR 0018 negative gates, #418).

These fixtures prove the ADR's required negative gates at the projection layer without a Kubernetes
runtime: closed field sets, structural exclusion of payload/credential/network fields, rejected/outlier
suppression, single-method selection, and fail-closed behaviour on every enumerated drift.
"""

from __future__ import annotations

import dataclasses
from pathlib import Path

import pytest
from records import (
    MAS_AUTHENTICATION_SCHEMA,
    MAS_AUTHENTICATION_SOURCE_COLUMNS,
    MATRIX_EVENT_SCHEMA,
    MATRIX_EVENT_SOURCE_COLUMNS,
    MasAuthenticationRecord,
    MatrixEventRecord,
    ProjectionError,
    project_mas_authentication,
    project_matrix_event,
)
from schemacheck import load_schema, schema_property_names, schema_required, validate_instance

SCHEMA_DIR = Path(__file__).resolve().parent.parent / "schemas"
SERVER_NAME = "fgentic.localhost"

# Fields that must never appear as a selected source column or an output field. Payload, credential,
# network-identity, and request values from the ADR's rejected-content list.
FORBIDDEN_FIELDS = frozenset(
    {
        "content",
        "formatted_body",
        "body",
        "unsigned",
        "unrecognized_keys",
        "state_key",
        "txn_id",
        "transaction_id",
        "password",
        "password_hash",
        "hashed_password",
        "email",
        "display_name",
        "displayname",
        "ip",
        "ip_address",
        "user_agent",
        "cookie",
        "authorization_code",
        "access_token",
        "refresh_token",
        "token",
        "query",
        "request_query",
        "redirect_uri",
        "client_secret",
    }
)


def _valid_event_row() -> dict[str, object]:
    return {
        "event_id": "$abc123:fgentic.localhost",
        "type": "m.room.message",
        "room_id": "!room:fgentic.localhost",
        "sender": "@alice:fgentic.localhost",
        "origin_server_ts": 1_700_000_000_000,
        "received_ts": 1_700_000_000_050,
        "stream_ordering": 42,
        "outlier": False,
        "rejection_reason": None,
    }


def _valid_password_row() -> dict[str, object]:
    return {
        "authentication_id": "auth-1",
        "session_id": "sess-1",
        "occurred_at": "2026-07-22T00:00:00Z",
        "username": "alice",
        "method": "password",
        "upstream_provider_id": None,
    }


def _valid_upstream_row() -> dict[str, object]:
    return {
        "authentication_id": "auth-2",
        "session_id": "sess-2",
        "occurred_at": "2026-07-22T00:01:00Z",
        "username": "bob",
        "method": "upstream_oidc",
        "upstream_provider_id": "01H8PKNWKKRPCBW4YGH1RWV279",
    }


def _load_schema(name: str) -> dict[str, object]:
    return load_schema(SCHEMA_DIR / name)


# --- Gate 1 & 2: closed field sets, no forbidden field, no automatic pass-through ------------------


def test_source_allowlists_exclude_every_forbidden_field() -> None:
    assert MATRIX_EVENT_SOURCE_COLUMNS.isdisjoint(FORBIDDEN_FIELDS)
    assert MAS_AUTHENTICATION_SOURCE_COLUMNS.isdisjoint(FORBIDDEN_FIELDS)


def test_output_records_expose_only_allowlisted_fields() -> None:
    projected = project_matrix_event(_valid_event_row())
    assert projected is not None
    event = dataclasses.asdict(projected)
    assert event.keys() == {
        "origin_server_ts",
        "received_ts",
        "event_id",
        "room_id",
        "sender",
        "event_type",
        "stream_ordering",
    }
    assert FORBIDDEN_FIELDS.isdisjoint(event)

    auth = dataclasses.asdict(project_mas_authentication(_valid_password_row(), SERVER_NAME))
    assert auth.keys() == {
        "occurred_at",
        "authentication_id",
        "session_id",
        "matrix_user",
        "method",
        "upstream_provider_id",
    }
    assert FORBIDDEN_FIELDS.isdisjoint(auth)


@pytest.mark.parametrize("forbidden", sorted(FORBIDDEN_FIELDS))
def test_a_forbidden_source_column_fails_closed_instead_of_passing_through(forbidden: str) -> None:
    event_row = _valid_event_row() | {forbidden: "leak"}
    with pytest.raises(ProjectionError, match="pinned schema fingerprint"):
        project_matrix_event(event_row)
    mas_row = _valid_password_row() | {forbidden: "leak"}
    with pytest.raises(ProjectionError, match="pinned schema fingerprint"):
        project_mas_authentication(mas_row, SERVER_NAME)


def test_output_fields_match_the_published_json_schema_contract() -> None:
    event_schema = _load_schema("matrix_event.v1.schema.json")
    record_fields = {field.name for field in dataclasses.fields(MatrixEventRecord)}
    assert record_fields == schema_property_names(event_schema)
    assert schema_required(event_schema) == record_fields
    assert event_schema["additionalProperties"] is False

    mas_schema = _load_schema("mas_authentication.v1.schema.json")
    record_fields = {field.name for field in dataclasses.fields(MasAuthenticationRecord)}
    assert record_fields == schema_property_names(mas_schema)
    # upstream_provider_id is optional; every other output field is required.
    assert schema_required(mas_schema) == record_fields - {"upstream_provider_id"}
    assert mas_schema["additionalProperties"] is False


def test_serialized_records_validate_against_the_published_schema() -> None:
    event = project_matrix_event(_valid_event_row())
    assert event is not None
    validate_instance(event.as_record(), _load_schema("matrix_event.v1.schema.json"))

    mas_schema = _load_schema("mas_authentication.v1.schema.json")
    password = project_mas_authentication(_valid_password_row(), SERVER_NAME).as_record()
    # A password record OMITS upstream_provider_id rather than emitting a schema-invalid null.
    assert "upstream_provider_id" not in password
    validate_instance(password, mas_schema)
    upstream = project_mas_authentication(_valid_upstream_row(), SERVER_NAME).as_record()
    assert upstream["upstream_provider_id"] == "01H8PKNWKKRPCBW4YGH1RWV279"
    validate_instance(upstream, mas_schema)


def test_schema_validator_rejects_contract_violations() -> None:
    # Proves the validator is not a no-op: each violation the schema forbids is caught.
    mas_schema = _load_schema("mas_authentication.v1.schema.json")
    valid = project_mas_authentication(_valid_password_row(), SERVER_NAME).as_record()
    for violation in (
        {"upstream_provider_id": None},  # null where the schema demands a string
        {"method": "webauthn"},  # outside the method enum
        {"email": "leak@example.com"},  # a key outside the closed set
        {"matrix_user": "alice"},  # fails the MXID pattern
        {"occurred_at": ""},  # violates minLength
    ):
        with pytest.raises(AssertionError):
            validate_instance(valid | violation, mas_schema)


# --- Gate 3: rejected/outlier suppression and deterministic dedupe keys ----------------------------


def test_outlier_and_rejected_events_do_not_emit() -> None:
    assert project_matrix_event(_valid_event_row() | {"outlier": True}) is None
    assert project_matrix_event(_valid_event_row() | {"rejection_reason": "auth"}) is None


def test_accepted_event_emits_with_synapse_type_mapped_to_event_type() -> None:
    record = project_matrix_event(_valid_event_row())
    assert record is not None
    assert record.event_type == "m.room.message"
    assert record.schema == MATRIX_EVENT_SCHEMA
    assert record.dedupe_key == (MATRIX_EVENT_SCHEMA, "$abc123:fgentic.localhost")


def test_mas_dedupe_key_is_schema_and_authentication_id() -> None:
    record = project_mas_authentication(_valid_password_row(), SERVER_NAME)
    assert record.schema == MAS_AUTHENTICATION_SCHEMA
    assert record.dedupe_key == (MAS_AUTHENTICATION_SCHEMA, "auth-1")


# --- Gate 4: single-method selection, both success paths ------------------------------------------


def test_password_and_upstream_success_each_select_exactly_one_method() -> None:
    password = project_mas_authentication(_valid_password_row(), SERVER_NAME)
    assert password.method == "password"
    assert password.matrix_user == "@alice:fgentic.localhost"
    assert password.upstream_provider_id is None

    upstream = project_mas_authentication(_valid_upstream_row(), SERVER_NAME)
    assert upstream.method == "upstream_oidc"
    assert upstream.upstream_provider_id == "01H8PKNWKKRPCBW4YGH1RWV279"


@pytest.mark.parametrize("method", ["", "webauthn", "PASSWORD", "password ", "upstream"])
def test_ambiguous_or_unknown_method_fails_closed(method: str) -> None:
    with pytest.raises(ProjectionError):
        project_mas_authentication(_valid_password_row() | {"method": method}, SERVER_NAME)


def test_password_row_carrying_an_upstream_provider_is_ambiguous_and_fails_closed() -> None:
    with pytest.raises(ProjectionError, match="upstream_provider_id"):
        project_mas_authentication(_valid_password_row() | {"upstream_provider_id": "x"}, SERVER_NAME)


def test_upstream_row_without_a_provider_fails_closed() -> None:
    with pytest.raises(ProjectionError):
        project_mas_authentication(_valid_upstream_row() | {"upstream_provider_id": None}, SERVER_NAME)


# --- Gate 5: version drift, missing columns, malformed values fail closed --------------------------


def test_missing_source_column_fails_closed() -> None:
    row = _valid_event_row()
    del row["stream_ordering"]
    with pytest.raises(ProjectionError, match="pinned schema fingerprint"):
        project_matrix_event(row)


@pytest.mark.parametrize("bad", ["alice:evil", "@alice", "Alice", "alice bob", "al*ce", ""])
def test_malformed_localpart_fails_closed(bad: str) -> None:
    with pytest.raises(ProjectionError):
        project_mas_authentication(_valid_password_row() | {"username": bad}, SERVER_NAME)


@pytest.mark.parametrize("server", ["", "bad:server"])
def test_missing_or_malformed_server_name_fails_closed(server: str) -> None:
    with pytest.raises(ProjectionError):
        project_mas_authentication(_valid_password_row(), server)


def test_non_integer_timestamp_fails_closed() -> None:
    with pytest.raises(ProjectionError):
        project_matrix_event(_valid_event_row() | {"origin_server_ts": "soon"})


def test_boolean_is_not_accepted_as_an_integer_timestamp() -> None:
    with pytest.raises(ProjectionError):
        project_matrix_event(_valid_event_row() | {"stream_ordering": True})


def test_non_boolean_outlier_and_non_string_rejection_reason_fail_closed() -> None:
    with pytest.raises(ProjectionError):
        project_matrix_event(_valid_event_row() | {"outlier": "yes"})
    with pytest.raises(ProjectionError):
        project_matrix_event(_valid_event_row() | {"rejection_reason": 7})


def test_empty_required_string_fails_closed() -> None:
    with pytest.raises(ProjectionError):
        project_matrix_event(_valid_event_row() | {"event_id": ""})
