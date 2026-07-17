"""Property-based tests for the strict federation policy parser.

The parser is the git-reloadable Synapse callback border: arbitrary input must always yield either a
``PolicyError`` rejection or a valid, non-permissive ``Policy`` — never an unexpected exception
escape and never a silently permissive result.
"""

from __future__ import annotations

import json

from hypothesis import given
from hypothesis import strategies as st

import fgentic_federation_policy as policy_module
from fgentic_federation_policy import Policy, PolicyError

_VALID_SERVERS = [
    "org-a.fgentic.localhost",
    "org-b.fgentic.localhost",
    "matrix.partner.example",
    "example.org",
    "a.b.c.example:8448",
]
_VALID_EVENT_TYPES = [
    "m.room.message",
    "m.room.member",
    "m.room.create",
    "m.room.server_acl",
    "m.room.power_levels",
]
_INVITE_RULES = [rule.value for rule in policy_module.InviteRule]

_json_values = st.recursive(
    st.none() | st.booleans() | st.integers() | st.floats(allow_nan=False) | st.text(),
    lambda children: st.lists(children) | st.dictionaries(st.text(), children),
    max_leaves=20,
)


def _valid_document(servers: list[str], event_types: list[str], invite_rule: str) -> dict[str, object]:
    return {
        "version": 1,
        "allowed_servers": servers,
        "allowed_event_types": event_types,
        "invite_rule": invite_rule,
    }


@given(st.binary(max_size=70_000))
def test_arbitrary_bytes_reject_or_valid(raw: bytes) -> None:
    """Any byte string parses to a valid Policy or fails closed with PolicyError only."""
    try:
        parsed = Policy.parse(raw)
    except PolicyError:
        return
    _assert_valid_policy(parsed)


@given(_json_values)
def test_arbitrary_json_documents(document: object) -> None:
    """Any JSON-serialisable document rejects with PolicyError or yields a valid Policy."""
    raw = json.dumps(document).encode("utf-8")
    try:
        parsed = Policy.parse(raw)
    except PolicyError:
        return
    _assert_valid_policy(parsed)


@given(
    servers=st.lists(st.sampled_from(_VALID_SERVERS), min_size=1, max_size=5, unique=True),
    event_types=st.lists(st.sampled_from(_VALID_EVENT_TYPES), min_size=1, max_size=5, unique=True),
    invite_rule=st.sampled_from(_INVITE_RULES),
)
def test_valid_policies_round_trip(servers: list[str], event_types: list[str], invite_rule: str) -> None:
    """A genuinely valid policy is accepted and parses to exactly its declared sets — never broader."""
    raw = json.dumps(_valid_document(servers, event_types, invite_rule)).encode("utf-8")
    parsed = Policy.parse(raw)
    _assert_valid_policy(parsed)
    assert parsed.allowed_servers == frozenset(servers)
    assert parsed.allowed_event_types == frozenset(event_types)
    assert parsed.invite_rule.value == invite_rule


@given(
    servers=st.lists(st.sampled_from(_VALID_SERVERS), min_size=1, max_size=3, unique=True),
    event_types=st.lists(st.sampled_from(_VALID_EVENT_TYPES), min_size=1, max_size=3, unique=True),
    invite_rule=st.sampled_from(_INVITE_RULES),
    mutation=st.sampled_from(
        [
            "empty_servers",
            "empty_event_types",
            "unknown_key",
            "drop_version",
            "wrong_version",
            "bad_invite_rule",
            "server_not_string",
            "glob_event_type",
        ]
    ),
)
def test_near_valid_mutations_reject(
    servers: list[str],
    event_types: list[str],
    invite_rule: str,
    mutation: str,
) -> None:
    """A single invalidating mutation of a valid policy must fail closed, never silently accept."""
    document = _valid_document(servers, event_types, invite_rule)
    if mutation == "empty_servers":
        document["allowed_servers"] = []
    elif mutation == "empty_event_types":
        document["allowed_event_types"] = []
    elif mutation == "unknown_key":
        document["extra"] = True
    elif mutation == "drop_version":
        del document["version"]
    elif mutation == "wrong_version":
        document["version"] = 2
    elif mutation == "bad_invite_rule":
        document["invite_rule"] = "allow_everything"
    elif mutation == "server_not_string":
        document["allowed_servers"] = [*servers, 123]
    elif mutation == "glob_event_type":
        document["allowed_event_types"] = [*event_types, "m.room.*"]

    raw = json.dumps(document).encode("utf-8")
    try:
        Policy.parse(raw)
    except PolicyError:
        return
    msg = f"invalidating mutation {mutation!r} was silently accepted"
    raise AssertionError(msg)


def _assert_valid_policy(parsed: Policy) -> None:
    assert isinstance(parsed, Policy)
    assert parsed.allowed_servers, "a valid policy is never permissive with empty servers"
    assert parsed.allowed_event_types, "a valid policy is never permissive with empty event types"
    assert parsed.invite_rule in set(policy_module.InviteRule)
    assert parsed.digest
