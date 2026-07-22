"""Closed-record contract for the privileged admin-action audit stream (issue #455, ADR 0018).

The `fgentic.admin_action.v1` record is Fgentic-owned; these deterministic fixtures prove its closed
field set, the structural exclusion of every forbidden field, and its action-class/outcome enums,
without a Kubernetes runtime. The capture-point projection (which source attributes the acting admin on
a `/_synapse/admin/*` request) and the opt-in collector/component remain follow-up #455 tasks.
"""

from __future__ import annotations

from pathlib import Path

import pytest
from schemacheck import load_schema, schema_property_names, schema_required, validate_instance

SCHEMA = load_schema(Path(__file__).resolve().parent.parent / "schemas" / "admin_action.v1.schema.json")

ACTION_CLASSES = (
    "user_suspend",
    "user_reactivate",
    "room_purge",
    "media_quarantine",
    "registration_token_issue",
    "registration_token_revoke",
    "report_dismiss",
)

# Payload, credential, network-identity, and request values that must never appear in the record.
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


def _valid_record(**overrides: object) -> dict[str, object]:
    record: dict[str, object] = {
        "occurred_at": "2026-07-22T00:00:00Z",
        "acting_admin": "@alice:fgentic.localhost",
        "action_class": "user_suspend",
        "target": "@spammer:fgentic.localhost",
        "outcome": "succeeded",
    }
    record.update(overrides)
    return record


def test_schema_field_set_is_closed() -> None:
    assert schema_property_names(SCHEMA) == {
        "occurred_at",
        "acting_admin",
        "action_class",
        "target",
        "outcome",
    }
    assert schema_required(SCHEMA) == schema_property_names(SCHEMA)
    assert SCHEMA["additionalProperties"] is False
    assert FORBIDDEN_FIELDS.isdisjoint(schema_property_names(SCHEMA))


@pytest.mark.parametrize("action_class", ACTION_CLASSES)
def test_every_action_class_validates(action_class: str) -> None:
    validate_instance(_valid_record(action_class=action_class), SCHEMA)


@pytest.mark.parametrize("outcome", ["succeeded", "failed", "denied"])
def test_every_outcome_validates_including_denied(outcome: str) -> None:
    # A denied non-admin attempt is recorded, never silently dropped.
    validate_instance(_valid_record(outcome=outcome), SCHEMA)


@pytest.mark.parametrize("forbidden", sorted(FORBIDDEN_FIELDS))
def test_a_forbidden_field_is_rejected(forbidden: str) -> None:
    with pytest.raises(AssertionError):
        validate_instance(_valid_record(**{forbidden: "leak"}), SCHEMA)


def test_unknown_action_class_is_rejected() -> None:
    with pytest.raises(AssertionError):
        validate_instance(_valid_record(action_class="delete_server"), SCHEMA)


def test_unknown_outcome_is_rejected() -> None:
    with pytest.raises(AssertionError):
        validate_instance(_valid_record(outcome="maybe"), SCHEMA)


@pytest.mark.parametrize("acting_admin", ["alice", "@alice", "alice:fgentic.localhost", "@Alice:x", ""])
def test_acting_admin_must_be_a_full_mxid(acting_admin: str) -> None:
    with pytest.raises(AssertionError):
        validate_instance(_valid_record(acting_admin=acting_admin), SCHEMA)


def test_missing_required_field_is_rejected() -> None:
    record = _valid_record()
    del record["target"]
    with pytest.raises(AssertionError):
        validate_instance(record, SCHEMA)
