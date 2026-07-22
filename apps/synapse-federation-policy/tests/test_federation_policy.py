from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import re
import sys
import types
from collections.abc import Awaitable, Callable
from dataclasses import dataclass, field
from enum import StrEnum
from pathlib import Path
from typing import cast

import pytest

import fgentic_federation_policy as policy_module
from fgentic_federation_policy import FederationPolicyModule, ModuleConfig, Policy, PolicyError

SERVER_A = "org-a.fgentic.localhost"
SERVER_B = "org-b.fgentic.localhost"
# Second admitted partner in the multi-party lab (issue #354).
SERVER_D = "org-d.fgentic.localhost"
ROOM_ID = "!federated:org-a.fgentic.localhost"
EVENT_ID = "$event-from-b"


def policy_document(
    *,
    servers: list[object] | None = None,
    event_types: list[object] | None = None,
    invite_rule: object = "allow_from_allowed_servers",
    version: object = 1,
) -> dict[str, object]:
    return {
        "version": version,
        "allowed_servers": [SERVER_A, SERVER_B] if servers is None else servers,
        "allowed_event_types": ["m.room.member", "m.room.message"] if event_types is None else event_types,
        "invite_rule": invite_rule,
    }


def encoded_policy(**changes: object) -> bytes:
    document = policy_document()
    document.update(changes)
    return json.dumps(document).encode()


@dataclass(frozen=True, slots=True)
class Event:
    event_id: str = EVENT_ID
    room_id: str = ROOM_ID
    sender: str = f"@bob:{SERVER_B}"
    type: str = "m.room.message"
    content: str = "CONTENT-MUST-NEVER-BE-READ-OR-LOGGED"


type DropCallback = Callable[[policy_module.FederatedEvent], Awaitable[bool]]
type InviteCallback = Callable[[policy_module.FederatedEvent], Awaitable[str]]


@dataclass(slots=True)
class FakeTransaction:
    staged_events: set[tuple[str, str]]
    queries: list[tuple[str, tuple[str, str]]]
    parameters: tuple[str, str] | None = None

    def execute(self, sql: str, parameters: tuple[str, str]) -> None:
        self.queries.append((sql, parameters))
        self.parameters = parameters

    def fetchone(self) -> tuple[object, ...] | None:
        assert self.parameters is not None
        return (1,) if self.parameters in self.staged_events else None


@dataclass(slots=True)
class FakeModuleApi:
    server_name: str = SERVER_A
    drop_callback: DropCallback | None = None
    invite_callback: InviteCallback | None = None
    staged_events: set[tuple[str, str]] = field(default_factory=set)
    database_queries: list[tuple[str, tuple[str, str]]] = field(default_factory=list)
    database_error: Exception | None = None

    def register_spam_checker_callbacks(
        self,
        *,
        should_drop_federated_event: DropCallback,
        federated_user_may_invite: InviteCallback,
    ) -> None:
        self.drop_callback = should_drop_federated_event
        self.invite_callback = federated_user_may_invite

    async def run_db_interaction(
        self,
        desc: str,
        interaction: policy_module.StagingInteraction,
        room_id: str,
        event_id: str,
    ) -> bool:
        del desc
        if self.database_error is not None:
            raise self.database_error
        transaction = FakeTransaction(self.staged_events, self.database_queries)
        return interaction(transaction, room_id, event_id)


def write_policy(path: Path, raw: bytes | None = None) -> None:
    path.write_bytes(encoded_policy() if raw is None else raw)


def replace_policy(path: Path, raw: bytes) -> None:
    replacement = path.with_suffix(".replacement")
    replacement.write_bytes(raw)
    replacement.replace(path)


def create_module(path: Path, api: FakeModuleApi | None = None) -> tuple[FederationPolicyModule, FakeModuleApi]:
    api = api or FakeModuleApi()
    module = FederationPolicyModule(ModuleConfig(policy_path=path), api)
    return module, api


def run_drop(module: FederationPolicyModule, event: Event | None = None) -> bool:
    return asyncio.run(module.should_drop_federated_event(event or Event()))


def run_invite(module: FederationPolicyModule, event: Event | None = None) -> str:
    invite = event or Event(type="m.room.member")
    return asyncio.run(module.federated_user_may_invite(invite))


def violation_payload(caplog: pytest.LogCaptureFixture) -> dict[str, object]:
    return log_payload(caplog, "fgentic_federation_policy_violation")


def log_payload(caplog: pytest.LogCaptureFixture, prefix: str) -> dict[str, object]:
    message = next(
        record.getMessage() for record in reversed(caplog.records) if record.getMessage().startswith(f"{prefix} ")
    )
    _prefix, encoded = message.split(" ", maxsplit=1)
    return cast("dict[str, object]", json.loads(encoded))


@pytest.fixture(autouse=True)
def synapse_module_api(monkeypatch: pytest.MonkeyPatch) -> None:
    class Codes(StrEnum):
        FORBIDDEN = "M_FORBIDDEN"

    module_api = types.ModuleType("synapse.module_api")
    module_api.__dict__.update({"NOT_SPAM": "NOT_SPAM", "errors": types.SimpleNamespace(Codes=Codes)})
    monkeypatch.setitem(sys.modules, "synapse.module_api", module_api)


def test_policy_parses_exact_values_and_semantic_digest() -> None:
    first = Policy.parse(encoded_policy())
    second = Policy.parse(
        b'{"invite_rule":"allow_from_allowed_servers","allowed_event_types":["m.room.message","m.room.member"],'
        b'"allowed_servers":["org-b.fgentic.localhost","org-a.fgentic.localhost"],"version":1}'
    )

    assert first.allowed_servers == frozenset({SERVER_A, SERVER_B})
    assert first.allowed_event_types == frozenset({"m.room.member", "m.room.message"})
    assert first.invite_rule is policy_module.InviteRule.ALLOW_FROM_ALLOWED_SERVERS
    assert first.digest == second.digest
    assert re.fullmatch(r"[0-9a-f]{64}", first.digest)


@pytest.mark.parametrize(
    ("raw", "message"),
    [
        (b"\xff", "valid UTF-8 JSON"),
        (b"{", "valid UTF-8 JSON"),
        (b"[]", "JSON object"),
        (b'{"version":1,"version":1}', "duplicate JSON key"),
        (b'{"version":NaN}', "non-finite JSON number"),
        (json.dumps({"version": 1}).encode(), "keys mismatch"),
        (encoded_policy(extra=True), "keys mismatch"),
        (encoded_policy(version=True), "integer 1"),
        (encoded_policy(version=2), "integer 1"),
        (encoded_policy(allowed_servers="not-an-array"), "must be an array"),
        (encoded_policy(allowed_servers=[SERVER_A, 7]), "entries must be strings"),
        (encoded_policy(allowed_servers=[SERVER_A, SERVER_A]), "entries must be unique"),
        (encoded_policy(allowed_servers=[]), "must not be empty"),
        (encoded_policy(allowed_servers=["ORG-A.example"]), "canonical lowercase DNS"),
        (encoded_policy(allowed_servers=["org-a.example:65536"]), "range 1..65535"),
        (encoded_policy(allowed_event_types="not-an-array"), "must be an array"),
        (encoded_policy(allowed_event_types=["m.room.message", 7]), "entries must be strings"),
        (encoded_policy(allowed_event_types=["m.room.message", "m.room.message"]), "entries must be unique"),
        (encoded_policy(allowed_event_types=[]), "must not be empty"),
        (encoded_policy(allowed_event_types=[""]), "1..255 characters"),
        (encoded_policy(allowed_event_types=["m.room. message"]), "must not contain whitespace"),
        (encoded_policy(allowed_event_types=["m.room.*"]), "not glob patterns"),
        (encoded_policy(invite_rule=7), "must be a string"),
        (encoded_policy(invite_rule="sometimes"), "must be one of"),
    ],
)
def test_policy_rejects_invalid_documents(raw: bytes, message: str) -> None:
    with pytest.raises(PolicyError, match=re.escape(message)):
        Policy.parse(raw)


def test_policy_rejects_oversized_input() -> None:
    with pytest.raises(PolicyError, match="exceeds"):
        Policy.parse(b" " * (policy_module._MAX_POLICY_BYTES + 1))


def test_policy_accepts_explicit_deny_all() -> None:
    policy = Policy.parse(encoded_policy(invite_rule="deny_all"))

    assert policy.allowed_servers == frozenset({SERVER_A, SERVER_B})
    assert policy.allowed_event_types == frozenset({"m.room.member", "m.room.message"})
    assert policy.invite_rule is policy_module.InviteRule.DENY_ALL


@pytest.mark.parametrize(
    "config",
    [
        {},
        {"policy_path": "/policy.json", "unknown": True},
        {"policy_path": 7},
        {"policy_path": ""},
        {"policy_path": "bad\x00path"},
        {"policy_path": "relative/policy.json"},
    ],
)
def test_module_config_rejects_invalid_shape(config: dict[str, object]) -> None:
    with pytest.raises(ValueError, match=r"module config|policy_path"):
        FederationPolicyModule.parse_config(config)


def test_module_config_returns_typed_absolute_path() -> None:
    assert FederationPolicyModule.parse_config({"policy_path": "/policy.json"}) == ModuleConfig(
        policy_path=Path("/policy.json")
    )


def test_module_registers_both_callbacks(tmp_path: Path) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)

    module, api = create_module(path)

    assert api.drop_callback == module.should_drop_federated_event
    assert api.invite_callback == module.federated_user_may_invite


def test_allowed_event_and_invite_pass(tmp_path: Path) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    module, api = create_module(path)

    assert run_drop(module) is False
    assert run_invite(module) == "NOT_SPAM"
    assert api.database_queries == []


@pytest.mark.parametrize(
    ("event", "reason"),
    [
        (Event(sender="@mallory:org-c.fgentic.localhost"), "server_not_allowed"),
        (Event(type="com.fgentic.blocked"), "event_type_not_allowed"),
        (Event(sender="invalid-sender"), "invalid_event_metadata"),
        (Event(event_id=""), "invalid_event_metadata"),
        (Event(room_id=""), "invalid_event_metadata"),
        (Event(type=""), "invalid_event_metadata"),
    ],
)
def test_denied_event_logs_exact_content_free_record(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
    event: Event,
    reason: str,
) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    module, api = create_module(path)
    caplog.clear()

    assert run_drop(module, event) is True

    payload = violation_payload(caplog)
    assert set(payload) == {
        "allowed_event_type_count",
        "allowed_server_count",
        "event",
        "invite_rule",
        "policy_digest",
        "reason",
        "room",
        "server",
        "type",
    }
    assert payload["reason"] == reason
    assert payload["allowed_server_count"] == 2
    assert payload["allowed_event_type_count"] == 2
    assert payload["event"] == (event.event_id or "<invalid>")
    assert payload["room"] == (event.room_id or "<invalid>")
    assert payload["type"] == (event.type or "<invalid>")
    assert re.fullmatch(r"[0-9a-f]{64}", cast("str", payload["policy_digest"]))
    assert "CONTENT-MUST-NEVER-BE-READ-OR-LOGGED" not in caplog.text
    assert "@bob" not in caplog.text
    if reason == "invalid_event_metadata":
        assert api.database_queries == []
    else:
        assert api.database_queries[-1][1] == (event.room_id, event.event_id)


def test_new_denied_event_is_scoped_by_room_and_event(tmp_path: Path) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    event = Event(type="com.fgentic.blocked")
    api = FakeModuleApi(
        staged_events={
            ("!other:org-a.fgentic.localhost", event.event_id),
            (event.room_id, "$other-event"),
        }
    )
    module, api = create_module(path, api)

    assert run_drop(module, event) is True

    assert len(api.database_queries) == 1
    sql, parameters = api.database_queries[0]
    assert "FROM federation_inbound_events_staging" in sql
    assert "WHERE room_id = ? AND event_id = ?" in sql
    assert parameters == (event.room_id, event.event_id)


def test_staged_event_is_grandfathered_after_policy_tightening(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    path = tmp_path / "policy.json"
    event = Event(type="com.fgentic.blocked")
    write_policy(
        path,
        encoded_policy(allowed_event_types=["m.room.member", "m.room.message", event.type]),
    )
    module, api = create_module(path)

    assert run_drop(module, event) is False
    assert api.database_queries == []

    api.staged_events.add((event.room_id, event.event_id))
    replace_policy(path, encoded_policy())
    caplog.set_level(logging.INFO)
    caplog.clear()

    assert run_drop(module, event) is False

    payload = log_payload(caplog, "fgentic_federation_policy_staged_event_grandfathered")
    assert payload["reason"] == "event_type_not_allowed"
    assert payload["room"] == event.room_id
    assert payload["event"] == event.event_id
    assert "fgentic_federation_policy_violation " not in caplog.text


def test_new_module_grandfathers_existing_staged_row(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"
    event = Event(type="com.fgentic.blocked")
    write_policy(path)
    api = FakeModuleApi(staged_events={(event.room_id, event.event_id)})
    caplog.set_level(logging.INFO)

    module, _api = create_module(path, api)

    assert run_drop(module, event) is False
    assert log_payload(caplog, "fgentic_federation_policy_staged_event_grandfathered")["reason"] == (
        "event_type_not_allowed"
    )


def test_unavailable_policy_grandfathers_existing_staged_row(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"
    event = Event(type="com.fgentic.blocked")
    write_policy(path)
    module, api = create_module(path)
    api.staged_events.add((event.room_id, event.event_id))
    replace_policy(path, b"not-json")
    caplog.set_level(logging.INFO)
    caplog.clear()

    assert run_drop(module, event) is False
    assert log_payload(caplog, "fgentic_federation_policy_staged_event_grandfathered")["reason"] == (
        "policy_unavailable"
    )


def test_database_failure_is_fail_closed_coalesced_and_recovers(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    path = tmp_path / "policy.json"
    event = Event(type="com.fgentic.blocked")
    write_policy(path)
    module, api = create_module(path)
    api.database_error = RuntimeError("DATABASE-CONTENT-MUST-NOT-BE-LOGGED")
    caplog.set_level(logging.INFO)
    caplog.clear()

    assert run_drop(module, event) is True
    assert run_drop(module, event) is True

    lookup_failures = [
        record
        for record in caplog.records
        if record.getMessage().startswith("fgentic_federation_policy_staging_lookup_failed ")
    ]
    assert len(lookup_failures) == 1
    assert log_payload(caplog, "fgentic_federation_policy_staging_lookup_failed")["reason"] == ("staging_lookup_failed")
    assert "fgentic_federation_policy_violation " not in caplog.text
    assert "DATABASE-CONTENT-MUST-NOT-BE-LOGGED" not in caplog.text

    api.database_error = None
    api.staged_events.add((event.room_id, event.event_id))

    assert run_drop(module, event) is False
    assert run_drop(module, event) is False

    recoveries = [
        record
        for record in caplog.records
        if record.getMessage().startswith("fgentic_federation_policy_staging_lookup_recovered ")
    ]
    assert len(recoveries) == 1
    assert log_payload(caplog, "fgentic_federation_policy_staging_lookup_recovered")["reason"] == (
        "staging_lookup_recovered"
    )

    api.database_error = RuntimeError("another hidden failure")
    assert run_drop(module, event) is True
    lookup_failures = [
        record
        for record in caplog.records
        if record.getMessage().startswith("fgentic_federation_policy_staging_lookup_failed ")
    ]
    assert len(lookup_failures) == 2


def test_deny_all_invite_rule_rejects_only_invite_callback(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"
    write_policy(path, encoded_policy(invite_rule="deny_all"))
    module, _api = create_module(path)
    invite = Event(type="m.room.member")
    caplog.clear()

    assert run_drop(module, invite) is False
    assert run_invite(module, invite) == "M_FORBIDDEN"
    assert violation_payload(caplog)["reason"] == "invite_rule_denied"


def test_missing_initial_policy_prevents_module_start(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"

    with pytest.raises(RuntimeError, match="initial federation policy is unavailable"):
        create_module(path)

    assert "file_unavailable" in caplog.text


def test_invalid_replacement_fails_closed_then_recovers(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    module, _api = create_module(path)
    assert run_drop(module) is False

    replace_policy(path, b"not-json")
    caplog.clear()

    assert run_drop(module) is True
    assert violation_payload(caplog)["reason"] == "policy_unavailable"
    assert "policy_invalid" in caplog.text

    replace_policy(path, encoded_policy(allowed_event_types=["m.room.member"]))
    caplog.clear()

    assert run_drop(module) is True
    assert violation_payload(caplog)["reason"] == "event_type_not_allowed"

    replace_policy(path, encoded_policy())
    assert run_drop(module) is False


def test_removed_policy_fails_closed_without_repeated_reload_log(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    module, _api = create_module(path)
    path.unlink()
    caplog.clear()

    assert run_drop(module) is True
    assert run_drop(module) is True

    reload_failures = [
        record
        for record in caplog.records
        if record.getMessage().startswith("fgentic_federation_policy_reload_failed ")
    ]
    assert len(reload_failures) == 1


def test_transient_policy_read_failure_retries_with_bounded_coalesced_logs(
    tmp_path: Path,
    caplog: pytest.LogCaptureFixture,
    monkeypatch: pytest.MonkeyPatch,
) -> None:
    now = [100.0]
    monkeypatch.setattr(policy_module.time, "monotonic", lambda: now[0])
    path = tmp_path / "policy.json"
    write_policy(path)
    module, _api = create_module(path)
    original_read = module._store._read
    attempts = 0

    def transient_read() -> tuple[bytes, policy_module._FileFingerprint]:
        nonlocal attempts
        attempts += 1
        if attempts <= 2:
            raise OSError("transient projected-volume error")
        return original_read()

    monkeypatch.setattr(module._store, "_read", transient_read)
    replace_policy(path, encoded_policy(allowed_event_types=["m.room.member"]))
    caplog.set_level(logging.INFO)
    caplog.clear()

    assert run_drop(module) is True
    assert attempts == 1
    assert run_drop(module) is True
    assert attempts == 1

    now[0] += 1.0
    assert run_drop(module) is True
    assert attempts == 2

    now[0] += 2.0
    assert run_drop(module) is True
    assert attempts == 3
    assert violation_payload(caplog)["reason"] == "event_type_not_allowed"

    reload_failures = [
        record
        for record in caplog.records
        if record.getMessage().startswith("fgentic_federation_policy_reload_failed ")
    ]
    assert len(reload_failures) == 1
    assert sum(record.getMessage().startswith("fgentic_federation_policy_loaded ") for record in caplog.records) == 1


def test_oversized_file_activates_fail_closed_state(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"
    path.write_bytes(b" " * (policy_module._MAX_POLICY_BYTES + 1))

    with pytest.raises(RuntimeError, match="initial federation policy is unavailable"):
        create_module(path)

    assert "policy_invalid" in caplog.text


def test_policy_must_include_local_server_at_start_and_after_reload(
    tmp_path: Path, caplog: pytest.LogCaptureFixture
) -> None:
    path = tmp_path / "policy.json"
    write_policy(path, encoded_policy(allowed_servers=[SERVER_B]))

    with pytest.raises(RuntimeError, match="initial federation policy is unavailable"):
        create_module(path)

    assert "policy_invalid" in caplog.text

    write_policy(path)
    module, _api = create_module(path)
    replace_policy(path, encoded_policy(allowed_servers=[SERVER_B]))
    caplog.clear()

    assert run_drop(module) is True
    assert violation_payload(caplog)["reason"] == "policy_unavailable"
    assert "policy_invalid" in caplog.text


@pytest.mark.parametrize(("not_spam", "forbidden"), [(None, "M_FORBIDDEN"), ("NOT_SPAM", None)])
def test_module_rejects_incompatible_synapse_decisions(
    tmp_path: Path,
    monkeypatch: pytest.MonkeyPatch,
    not_spam: object,
    forbidden: object,
) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    module_api = types.ModuleType("synapse.module_api")
    module_api.__dict__.update(
        {"NOT_SPAM": not_spam, "errors": types.SimpleNamespace(Codes=types.SimpleNamespace(FORBIDDEN=forbidden))}
    )
    monkeypatch.setitem(sys.modules, "synapse.module_api", module_api)

    with pytest.raises(RuntimeError, match="does not expose"):
        create_module(path)


def test_policy_loaded_log_has_digest_and_counts(tmp_path: Path, caplog: pytest.LogCaptureFixture) -> None:
    path = tmp_path / "policy.json"
    write_policy(path)
    caplog.set_level(logging.INFO)

    create_module(path)

    message = next(
        record.getMessage()
        for record in caplog.records
        if record.getMessage().startswith("fgentic_federation_policy_loaded ")
    )
    payload = cast("dict[str, object]", json.loads(message.split(" ", maxsplit=1)[1]))
    assert payload["allowed_server_count"] == 2
    assert payload["allowed_event_type_count"] == 2
    assert payload["reason"] == "policy_loaded"
    assert len(cast("str", payload["policy_digest"])) == hashlib.sha256().digest_size * 2


def test_canonical_repository_policy_is_valid() -> None:
    path = Path(__file__).parents[1] / "policy" / "policy.json"

    policy = Policy.parse(path.read_bytes())

    assert policy.allowed_servers == frozenset({SERVER_A, SERVER_B, SERVER_D})
    assert policy.allowed_event_types == frozenset(
        {
            "m.room.create",
            "m.room.guest_access",
            "m.room.history_visibility",
            "m.room.join_rules",
            "m.room.member",
            "m.room.message",
            "m.room.name",
            "m.room.power_levels",
            "m.room.server_acl",
        }
    )
    assert "com.fgentic.blocked" not in policy.allowed_event_types
    assert policy.invite_rule is policy_module.InviteRule.ALLOW_FROM_ALLOWED_SERVERS
