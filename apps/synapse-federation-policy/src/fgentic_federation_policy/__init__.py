"""Fail-closed, git-declared federation policy callbacks for Synapse."""

from __future__ import annotations

import hashlib
import importlib
import json
import logging
import os
import re
import threading
import time
from collections.abc import Awaitable, Callable, Mapping
from dataclasses import dataclass
from enum import StrEnum
from pathlib import Path
from typing import Protocol, cast

__all__ = ["FederationPolicyModule", "ModuleConfig", "Policy", "PolicyError"]

_LOGGER = logging.getLogger(__name__)
_MAX_POLICY_BYTES = 64 * 1024
_INITIAL_RELOAD_RETRY_SECONDS = 1.0
_MAX_RELOAD_RETRY_SECONDS = 30.0
_SERVER_NAME = re.compile(
    r"(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)(?:\.(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?))*"
    r"(?::[1-9][0-9]{0,4})?\Z"
)


class PolicyError(ValueError):
    """Raised when a policy does not match the versioned schema."""


class InviteRule(StrEnum):
    """Supported handling for invites received from an allowed server."""

    ALLOW_FROM_ALLOWED_SERVERS = "allow_from_allowed_servers"
    DENY_ALL = "deny_all"


class FederatedEvent(Protocol):
    """The content-free subset of Synapse's pinned ``EventBase`` callback type."""

    @property
    def event_id(self) -> str: ...

    @property
    def room_id(self) -> str: ...

    @property
    def sender(self) -> str: ...

    @property
    def type(self) -> str: ...


type DropCallback = Callable[[FederatedEvent], Awaitable[bool]]
type InviteCallback = Callable[[FederatedEvent], Awaitable[str]]


class DatabaseTransaction(Protocol):
    """The cursor operations used inside ``ModuleApi.run_db_interaction``."""

    def execute(self, sql: str, parameters: tuple[str, str]) -> object: ...

    def fetchone(self) -> tuple[object, ...] | None: ...


type StagingInteraction = Callable[[DatabaseTransaction, str, str], bool]


class ModuleApi(Protocol):
    """The Synapse 1.155.0 registration method used by this module."""

    @property
    def server_name(self) -> str: ...

    def register_spam_checker_callbacks(
        self,
        *,
        should_drop_federated_event: DropCallback,
        federated_user_may_invite: InviteCallback,
    ) -> None: ...

    def run_db_interaction(
        self,
        desc: str,
        interaction: StagingInteraction,
        room_id: str,
        event_id: str,
    ) -> Awaitable[bool]: ...


@dataclass(frozen=True, slots=True)
class ModuleConfig:
    """Validated Synapse module configuration."""

    policy_path: Path


@dataclass(frozen=True, slots=True)
class Policy:
    """An immutable, exact-match federation policy."""

    allowed_servers: frozenset[str]
    allowed_event_types: frozenset[str]
    invite_rule: InviteRule
    digest: str

    @classmethod
    def parse(cls, raw: bytes) -> Policy:
        """Parse strict UTF-8 JSON and return its canonical policy representation."""
        if len(raw) > _MAX_POLICY_BYTES:
            raise PolicyError(f"policy exceeds {_MAX_POLICY_BYTES} bytes")

        try:
            decoded = raw.decode("utf-8")
            document = json.loads(
                decoded,
                object_pairs_hook=_object_without_duplicate_keys,
                parse_constant=_reject_non_finite_number,
            )
        except (UnicodeDecodeError, json.JSONDecodeError) as error:
            raise PolicyError("policy must be valid UTF-8 JSON") from error

        if not isinstance(document, dict):
            raise PolicyError("policy must be a JSON object")

        expected_keys = {"allowed_event_types", "allowed_servers", "invite_rule", "version"}
        actual_keys = set(document)
        if actual_keys != expected_keys:
            missing = sorted(expected_keys - actual_keys)
            unknown = sorted(actual_keys - expected_keys)
            raise PolicyError(f"policy keys mismatch: missing={missing}, unknown={unknown}")

        if type(document["version"]) is not int or document["version"] != 1:
            raise PolicyError("version must be the integer 1")

        allowed_servers = _parse_exact_strings(document["allowed_servers"], "allowed_servers", _validate_server)
        allowed_event_types = _parse_exact_strings(
            document["allowed_event_types"], "allowed_event_types", _validate_event_type
        )
        if not allowed_servers:
            raise PolicyError("allowed_servers must not be empty")
        if not allowed_event_types:
            raise PolicyError("allowed_event_types must not be empty")

        invite_rule_value = document["invite_rule"]
        if not isinstance(invite_rule_value, str):
            raise PolicyError("invite_rule must be a string")
        try:
            invite_rule = InviteRule(invite_rule_value)
        except ValueError as error:
            supported = ", ".join(rule.value for rule in InviteRule)
            raise PolicyError(f"invite_rule must be one of: {supported}") from error

        return cls(
            allowed_servers=allowed_servers,
            allowed_event_types=allowed_event_types,
            invite_rule=invite_rule,
            digest=_policy_digest(allowed_servers, allowed_event_types, invite_rule),
        )


def _object_without_duplicate_keys(pairs: list[tuple[str, object]]) -> dict[str, object]:
    result: dict[str, object] = {}
    for key, value in pairs:
        if key in result:
            raise PolicyError(f"duplicate JSON key: {key}")
        result[key] = value
    return result


def _reject_non_finite_number(value: str) -> object:
    raise PolicyError(f"non-finite JSON number is forbidden: {value}")


def _parse_exact_strings(
    value: object,
    field: str,
    validator: Callable[[str], None],
) -> frozenset[str]:
    if not isinstance(value, list):
        raise PolicyError(f"{field} must be an array")

    parsed: set[str] = set()
    for item in cast("list[object]", value):
        if not isinstance(item, str):
            raise PolicyError(f"{field} entries must be strings")
        validator(item)
        if item in parsed:
            raise PolicyError(f"{field} entries must be unique")
        parsed.add(item)
    return frozenset(parsed)


def _validate_server(server: str) -> None:
    if len(server) > 255 or _SERVER_NAME.fullmatch(server) is None:
        raise PolicyError("allowed_servers entries must be canonical lowercase DNS server names")
    if ":" in server and int(server.rsplit(":", maxsplit=1)[1]) > 65535:
        raise PolicyError("allowed_servers ports must be in the range 1..65535")


def _validate_event_type(event_type: str) -> None:
    if not 1 <= len(event_type) <= 255:
        raise PolicyError("allowed_event_types entries must contain 1..255 characters")
    if event_type != event_type.strip() or any(character.isspace() for character in event_type):
        raise PolicyError("allowed_event_types entries must not contain whitespace")
    if "*" in event_type or "?" in event_type:
        raise PolicyError("allowed_event_types entries are exact values, not glob patterns")


def _policy_digest(
    allowed_servers: frozenset[str],
    allowed_event_types: frozenset[str],
    invite_rule: InviteRule,
) -> str:
    canonical = json.dumps(
        {
            "allowed_event_types": sorted(allowed_event_types),
            "allowed_servers": sorted(allowed_servers),
            "invite_rule": invite_rule.value,
            "version": 1,
        },
        ensure_ascii=True,
        separators=(",", ":"),
        sort_keys=True,
    ).encode()
    return hashlib.sha256(canonical).hexdigest()


@dataclass(frozen=True, slots=True)
class _FileFingerprint:
    device: int
    inode: int
    mode: int
    size: int
    modified_ns: int
    changed_ns: int

    @classmethod
    def from_stat(cls, stat: os.stat_result) -> _FileFingerprint:
        return cls(
            device=stat.st_dev,
            inode=stat.st_ino,
            mode=stat.st_mode,
            size=stat.st_size,
            modified_ns=stat.st_mtime_ns,
            changed_ns=stat.st_ctime_ns,
        )


@dataclass(frozen=True, slots=True)
class _PolicySnapshot:
    policy: Policy
    available: bool


_DENY_ALL_POLICY = Policy(
    allowed_servers=frozenset(),
    allowed_event_types=frozenset(),
    invite_rule=InviteRule.DENY_ALL,
    digest=_policy_digest(frozenset(), frozenset(), InviteRule.DENY_ALL),
)
_UNAVAILABLE_POLICY = _PolicySnapshot(policy=_DENY_ALL_POLICY, available=False)


class _PolicyStore:
    """Atomically reload a ConfigMap-projected policy while retaining fail-closed state."""

    def __init__(self, path: Path, local_server: str) -> None:
        self._path = path
        self._local_server = local_server
        self._lock = threading.Lock()
        self._fingerprint: _FileFingerprint | None = None
        self._checked = False
        self._snapshot = _UNAVAILABLE_POLICY
        self._reload_failure_active = False
        self._reload_retry_at = 0.0
        self._reload_retry_seconds = _INITIAL_RELOAD_RETRY_SECONDS
        self.current()

    def current(self) -> _PolicySnapshot:
        with self._lock:
            marker = self._source_fingerprint()
            now = time.monotonic()
            retry_due = not self._snapshot.available and now >= self._reload_retry_at
            if not self._checked or marker != self._fingerprint or retry_due:
                self._reload(marker, now)
            return self._snapshot

    def _source_fingerprint(self) -> _FileFingerprint | None:
        try:
            return _FileFingerprint.from_stat(self._path.stat())
        except OSError:
            return None

    def _reload(self, marker: _FileFingerprint | None, now: float) -> None:
        try:
            raw, opened_marker = self._read()
            policy = Policy.parse(raw)
            if self._local_server not in policy.allowed_servers:
                raise PolicyError("allowed_servers must include the local Synapse server_name")
        except OSError:
            self._activate_unavailable(marker, "file_unavailable", now)
            return
        except PolicyError:
            self._activate_unavailable(marker, "policy_invalid", now)
            return

        self._snapshot = _PolicySnapshot(policy=policy, available=True)
        self._fingerprint = opened_marker
        self._checked = True
        self._reload_failure_active = False
        self._reload_retry_at = 0.0
        self._reload_retry_seconds = _INITIAL_RELOAD_RETRY_SECONDS
        _log_policy_state(logging.INFO, "fgentic_federation_policy_loaded", "policy_loaded", self._snapshot)

    def _read(self) -> tuple[bytes, _FileFingerprint]:
        with self._path.open("rb") as policy_file:
            raw = policy_file.read(_MAX_POLICY_BYTES + 1)
            marker = _FileFingerprint.from_stat(os.fstat(policy_file.fileno()))
        if len(raw) > _MAX_POLICY_BYTES:
            raise PolicyError(f"policy exceeds {_MAX_POLICY_BYTES} bytes")
        return raw, marker

    def _activate_unavailable(self, marker: _FileFingerprint | None, reason: str, now: float) -> None:
        self._snapshot = _UNAVAILABLE_POLICY
        self._fingerprint = marker
        self._checked = True
        self._reload_retry_at = now + self._reload_retry_seconds
        self._reload_retry_seconds = min(self._reload_retry_seconds * 2, _MAX_RELOAD_RETRY_SECONDS)
        if not self._reload_failure_active:
            self._reload_failure_active = True
            _log_policy_state(logging.ERROR, "fgentic_federation_policy_reload_failed", reason, self._snapshot)


@dataclass(frozen=True, slots=True)
class _EventMetadata:
    event: str
    room: str
    sender: str
    server: str
    type: str
    valid: bool

    @classmethod
    def from_event(cls, event: FederatedEvent) -> _EventMetadata:
        event_id = _required_string(event.event_id)
        room_id = _required_string(event.room_id)
        sender = _required_string(event.sender)
        event_type = _required_string(event.type)
        server = _server_from_sender(sender)
        valid = "<invalid>" not in {event_id, room_id, sender, event_type, server}
        return cls(event=event_id, room=room_id, sender=sender, server=server, type=event_type, valid=valid)


def _required_string(value: object) -> str:
    return value if isinstance(value, str) and value else "<invalid>"


def _server_from_sender(sender: str) -> str:
    _localpart, separator, server = sender.partition(":")
    return server if separator and server else "<invalid>"


class FederationPolicyModule:
    """Register the two pinned Synapse spam-checker callbacks."""

    def __init__(self, config: ModuleConfig, api: ModuleApi) -> None:
        self._api = api
        self._store = _PolicyStore(config.policy_path, api.server_name)
        if not self._store.current().available:
            raise RuntimeError("initial federation policy is unavailable")
        decisions = _load_synapse_decisions()
        self._not_spam = decisions.not_spam
        self._forbidden = decisions.forbidden
        self._database_state_lock = threading.Lock()
        self._database_error_active = False
        api.register_spam_checker_callbacks(
            federated_user_may_invite=self.federated_user_may_invite,
            should_drop_federated_event=self.should_drop_federated_event,
        )

    @staticmethod
    def parse_config(config: Mapping[str, object]) -> ModuleConfig:
        """Validate the exact Synapse module configuration shape."""
        if set(config) != {"policy_path"}:
            raise ValueError("module config must contain only policy_path")
        value = config["policy_path"]
        if not isinstance(value, str) or not value or "\x00" in value:
            raise ValueError("policy_path must be a non-empty string")
        path = Path(value)
        if not path.is_absolute():
            raise ValueError("policy_path must be absolute")
        return ModuleConfig(policy_path=path)

    async def should_drop_federated_event(self, event: FederatedEvent) -> bool:
        """Drop remote events that violate the active policy."""
        metadata = _EventMetadata.from_event(event)
        snapshot = self._store.current()
        reason = _event_denial_reason(metadata, snapshot)
        if reason is None:
            return False
        if metadata.valid:
            try:
                staged = await self._api.run_db_interaction(
                    "fgentic_federation_policy_staging_lookup",
                    _is_staged_event,
                    metadata.room,
                    metadata.event,
                )
            except Exception:
                self._log_database_error_once(metadata, snapshot)
                return True
            else:
                self._log_database_recovery_once(metadata, snapshot)
                if staged:
                    _log_event_state(
                        logging.INFO,
                        "fgentic_federation_policy_staged_event_grandfathered",
                        metadata,
                        reason,
                        snapshot,
                    )
                    return False
        _log_violation(metadata, reason, snapshot)
        return True

    async def federated_user_may_invite(self, event: FederatedEvent) -> str:
        """Allow only policy-approved remote invitations."""
        metadata = _EventMetadata.from_event(event)
        snapshot = self._store.current()
        reason = _event_denial_reason(metadata, snapshot)
        if reason is None and snapshot.policy.invite_rule is InviteRule.DENY_ALL:
            reason = "invite_rule_denied"
        if reason is None:
            return self._not_spam
        _log_violation(metadata, reason, snapshot)
        return self._forbidden

    def _log_database_error_once(self, metadata: _EventMetadata, snapshot: _PolicySnapshot) -> None:
        with self._database_state_lock:
            if self._database_error_active:
                return
            self._database_error_active = True
        _log_event_state(
            logging.ERROR,
            "fgentic_federation_policy_staging_lookup_failed",
            metadata,
            "staging_lookup_failed",
            snapshot,
        )

    def _log_database_recovery_once(self, metadata: _EventMetadata, snapshot: _PolicySnapshot) -> None:
        with self._database_state_lock:
            if not self._database_error_active:
                return
            self._database_error_active = False
        _log_event_state(
            logging.INFO,
            "fgentic_federation_policy_staging_lookup_recovered",
            metadata,
            "staging_lookup_recovered",
            snapshot,
        )


def _event_denial_reason(metadata: _EventMetadata, snapshot: _PolicySnapshot) -> str | None:
    if not snapshot.available:
        return "policy_unavailable"
    if not metadata.valid:
        return "invalid_event_metadata"
    if metadata.server not in snapshot.policy.allowed_servers:
        return "server_not_allowed"
    if metadata.type not in snapshot.policy.allowed_event_types:
        return "event_type_not_allowed"
    return None


def _log_violation(metadata: _EventMetadata, reason: str, snapshot: _PolicySnapshot) -> None:
    _log_event_state(logging.WARNING, "fgentic_federation_policy_violation", metadata, reason, snapshot)


def _log_event_state(
    level: int,
    message: str,
    metadata: _EventMetadata,
    reason: str,
    snapshot: _PolicySnapshot,
) -> None:
    policy = snapshot.policy
    payload: dict[str, str | int] = {
        "allowed_event_type_count": len(policy.allowed_event_types),
        "allowed_server_count": len(policy.allowed_servers),
        "event": metadata.event,
        "invite_rule": policy.invite_rule.value,
        "policy_digest": policy.digest,
        "reason": reason,
        "room": metadata.room,
        "server": metadata.server,
        "type": metadata.type,
    }
    _LOGGER.log(level, "%s %s", message, _canonical_log(payload))


def _log_policy_state(level: int, message: str, reason: str, snapshot: _PolicySnapshot) -> None:
    policy = snapshot.policy
    payload: dict[str, str | int] = {
        "allowed_event_type_count": len(policy.allowed_event_types),
        "allowed_server_count": len(policy.allowed_servers),
        "invite_rule": policy.invite_rule.value,
        "policy_digest": policy.digest,
        "reason": reason,
    }
    _LOGGER.log(level, "%s %s", message, _canonical_log(payload))


def _canonical_log(payload: Mapping[str, str | int]) -> str:
    return json.dumps(payload, ensure_ascii=True, separators=(",", ":"), sort_keys=True)


def _is_staged_event(transaction: DatabaseTransaction, room_id: str, event_id: str) -> bool:
    transaction.execute(
        """
        SELECT 1
        FROM federation_inbound_events_staging
        WHERE room_id = ? AND event_id = ?
        LIMIT 1
        """,
        (room_id, event_id),
    )
    return transaction.fetchone() is not None


@dataclass(frozen=True, slots=True)
class _SynapseDecisions:
    not_spam: str
    forbidden: str


def _load_synapse_decisions() -> _SynapseDecisions:
    """Load only the stable decisions exported by the host Synapse module API."""
    module_api = importlib.import_module("synapse.module_api")
    not_spam = getattr(module_api, "NOT_SPAM", None)
    errors = getattr(module_api, "errors", None)
    codes = getattr(errors, "Codes", None)
    forbidden = getattr(codes, "FORBIDDEN", None)
    if not isinstance(not_spam, str) or not isinstance(forbidden, str):
        raise RuntimeError("Synapse module API does not expose the required spam-checker decisions")
    return _SynapseDecisions(not_spam=not_spam, forbidden=forbidden)
