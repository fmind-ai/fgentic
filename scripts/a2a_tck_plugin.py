"""Fgentic adapter and scope guard for the pinned upstream A2A TCK."""

from __future__ import annotations

import copy
import importlib
import json
import os
import re
from pathlib import Path
from typing import Any, Literal, NotRequired, TypedDict, cast

import pytest


class ScopeRule(TypedDict):
    """One explicit allow or skip rule for a pinned TCK node ID."""

    id: str
    pattern: str
    reason: str
    expectedOutcome: NotRequired[Literal["passed", "skipped"]]
    expectedReason: NotRequired[str]


class ScopeDocument(TypedDict):
    """Versioned policy for the deliberately narrow exported A2A route."""

    schemaVersion: int
    tck: dict[str, str]
    tier: str
    transport: str
    allow: list[ScopeRule]
    skip: list[ScopeRule]


_ITEMS: dict[str, dict[str, Any]] = {}
_SCOPE: ScopeDocument | None = None
_ADAPTER_INSTALLED = False


def _required_environment(name: str) -> str:
    value = os.environ.get(name, "")
    if not value:
        raise pytest.UsageError(f"required environment variable is unset: {name}")
    return value


def validate_scope_document(path: Path) -> ScopeDocument:
    """Load and structurally validate a TCK scope document."""
    try:
        document = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as error:
        raise pytest.UsageError(f"could not read TCK scope document {path}: {error}") from error
    if not isinstance(document, dict) or document.get("schemaVersion") != 1:
        raise pytest.UsageError("TCK scope document must use schemaVersion 1")
    for collection in ("allow", "skip"):
        rules = document.get(collection)
        if not isinstance(rules, list) or not rules:
            raise pytest.UsageError(f"TCK scope document requires non-empty {collection} rules")
        for rule in rules:
            if not isinstance(rule, dict):
                raise pytest.UsageError(f"TCK scope {collection} rules must be objects")
            if not all(isinstance(rule.get(field), str) and rule[field] for field in ("id", "pattern", "reason")):
                raise pytest.UsageError(f"TCK scope {collection} rules require non-empty id, pattern, and reason")
            try:
                re.compile(rule["pattern"])
            except re.error as error:
                raise pytest.UsageError(f"invalid TCK scope {collection} rule: {error}") from error
            expected = rule.get("expectedOutcome")
            if collection == "allow" and expected not in ("passed", "skipped"):
                raise pytest.UsageError(f"TCK scope allow rule {rule['id']} requires an expected outcome")
            if expected == "skipped" and not rule.get("expectedReason"):
                raise pytest.UsageError(f"TCK scope allow rule {rule['id']} requires an expected skip reason")
    return cast(ScopeDocument, document)


def _load_scope() -> ScopeDocument:
    return validate_scope_document(Path(_required_environment("FGENTIC_TCK_SCOPE_FILE")))


def _matching_rules(nodeid: str, rules: list[ScopeRule]) -> list[ScopeRule]:
    return [rule for rule in rules if re.fullmatch(rule["pattern"], nodeid)]


def _interface_request_url(interface_url: str, url: Any) -> str:
    candidate = str(url)
    if candidate in ("/", interface_url):
        return interface_url
    raise pytest.UsageError(f"unexpected TCK JSON-RPC request URL: {candidate}")


def _patch_jsonrpc_client() -> None:
    global _ADAPTER_INSTALLED
    if _ADAPTER_INSTALLED:
        return

    interface_url = _required_environment("FGENTIC_TCK_INTERFACE_URL")
    bearer_token = _required_environment("FGENTIC_TCK_BEARER_TOKEN")
    extension_uri = _required_environment("FGENTIC_TCK_EXTENSION_URI")
    raw_max_tokens = _required_environment("FGENTIC_TCK_MAX_TOKENS")
    try:
        max_tokens = int(raw_max_tokens)
    except ValueError as error:
        raise pytest.UsageError("FGENTIC_TCK_MAX_TOKENS must be an integer") from error
    if max_tokens < 1:
        raise pytest.UsageError("FGENTIC_TCK_MAX_TOKENS must be positive")

    module: Any = importlib.import_module("tck.transport.jsonrpc_client")
    client_class: Any = module.JsonRpcClient
    original_init = client_class.__init__
    original_send_message = client_class.send_message

    def adapted_init(self: Any, _card_interface_url: str) -> None:
        original_init(self, interface_url)
        http_client = vars(self).get("_client")
        if http_client is None:
            raise pytest.UsageError("pinned TCK JsonRpcClient no longer exposes its HTTP client")
        original_post = http_client.post
        original_build_request = http_client.build_request

        def adapted_post(url: Any, *args: Any, **kwargs: Any) -> Any:
            # HTTPX normalizes the pinned client's nested base URL with a trailing slash.
            # Supplying the absolute reviewed URL preserves the exact exported route.
            return original_post(_interface_request_url(interface_url, url), *args, **kwargs)

        def adapted_build_request(method: str, url: Any, *args: Any, **kwargs: Any) -> Any:
            return original_build_request(
                method,
                _interface_request_url(interface_url, url),
                *args,
                **kwargs,
            )

        http_client.post = adapted_post
        http_client.build_request = adapted_build_request
        http_client.headers.update(
            {
                "Authorization": f"Bearer {bearer_token}",
                "A2A-Extensions": extension_uri,
            }
        )

    def adapted_send_message(
        self: Any,
        message: dict[str, Any],
        *,
        configuration: dict[str, Any] | None = None,
        metadata: dict[str, Any] | None = None,
    ) -> Any:
        adapted_message = copy.deepcopy(message)
        extensions = adapted_message.setdefault("extensions", [])
        if not isinstance(extensions, list):
            raise pytest.UsageError("TCK message extensions must be a list")
        if extension_uri not in extensions:
            extensions.append(extension_uri)
        message_metadata = adapted_message.setdefault("metadata", {})
        if not isinstance(message_metadata, dict):
            raise pytest.UsageError("TCK message metadata must be an object")
        budget = message_metadata.setdefault(extension_uri, {})
        if not isinstance(budget, dict):
            raise pytest.UsageError("TCK token-budget metadata must be an object")
        budget.setdefault("maxTokens", max_tokens)
        return original_send_message(
            self,
            adapted_message,
            configuration=configuration,
            metadata=metadata,
        )

    client_class.__init__ = adapted_init
    client_class.send_message = adapted_send_message
    _ADAPTER_INSTALLED = True


def pytest_configure(config: pytest.Config) -> None:
    """Install the authenticated, extension-aware client adapter before fixtures run."""
    del config
    _patch_jsonrpc_client()


def pytest_collection_modifyitems(
    session: pytest.Session,
    config: pytest.Config,
    items: list[pytest.Item],
) -> None:
    """Fail closed on unknown MUST tests and annotate every deliberate scope skip."""
    del session, config
    global _SCOPE
    _SCOPE = _load_scope()
    allow_rules = _SCOPE["allow"]
    skip_rules = _SCOPE["skip"]
    _ITEMS.clear()

    for item in items:
        if item.get_closest_marker("must") is None:
            continue
        allowed = _matching_rules(item.nodeid, allow_rules)
        skipped = _matching_rules(item.nodeid, skip_rules)
        if len(allowed) > 1 or len(skipped) > 1 or (allowed and skipped):
            raise pytest.UsageError(f"TCK node ID has ambiguous scope rules: {item.nodeid}")
        if allowed:
            rule = allowed[0]
            expected = rule.get("expectedOutcome")
            if expected is None:
                raise pytest.UsageError(f"allowed TCK rule omits expectedOutcome: {rule['id']}")
            _ITEMS[item.nodeid] = {
                "decision": "run",
                "rule": rule["id"],
                "reason": rule["reason"],
                "expectedOutcome": expected,
                "expectedReason": rule.get("expectedReason", ""),
            }
            continue
        if skipped:
            rule = skipped[0]
            reason = f"{rule['id']}: {rule['reason']}"
            item.add_marker(pytest.mark.skip(reason=reason))
            _ITEMS[item.nodeid] = {
                "decision": "skip",
                "rule": rule["id"],
                "reason": rule["reason"],
                "expectedOutcome": "skipped",
                "expectedReason": re.escape(rule["id"]),
            }
            continue
        raise pytest.UsageError(f"unclassified MUST test at pinned TCK revision: {item.nodeid}")


def _skip_reason(report: pytest.TestReport) -> str:
    longrepr = report.longrepr
    if isinstance(longrepr, tuple) and len(longrepr) == 3:
        return str(longrepr[2])
    return str(longrepr)


def pytest_runtest_logreport(report: pytest.TestReport) -> None:
    """Capture the terminal outcome for every scoped MUST test."""
    item = _ITEMS.get(report.nodeid)
    if item is None:
        return
    if report.failed:
        item["outcome"] = "failed"
        item["detail"] = report.longreprtext
    elif report.skipped:
        item["outcome"] = "skipped"
        item["detail"] = _skip_reason(report)
    elif report.when == "call" and report.passed:
        item["outcome"] = "passed"
        item["detail"] = ""


def pytest_sessionfinish(session: pytest.Session, exitstatus: int) -> None:
    """Write the machine-readable scope report and reject outcome drift."""
    if _SCOPE is None or session.config.option.collectonly:
        return
    mismatches: list[str] = []
    results: list[dict[str, Any]] = []
    for nodeid, item in sorted(_ITEMS.items()):
        outcome = item.get("outcome", "missing")
        expected = item["expectedOutcome"]
        detail = str(item.get("detail", ""))
        if outcome != expected:
            mismatches.append(f"{nodeid}: expected {expected}, got {outcome}")
        expected_reason = str(item.get("expectedReason", ""))
        if outcome == "skipped" and expected_reason and re.search(expected_reason, detail) is None:
            mismatches.append(f"{nodeid}: skip reason did not match {expected_reason!r}")
        results.append({"nodeid": nodeid, **item, "outcome": outcome, "detail": detail})

    counts = {
        outcome: sum(result.get("outcome") == outcome for result in results)
        for outcome in ("passed", "skipped", "failed", "missing")
    }
    report = {
        "schemaVersion": 1,
        "tck": _SCOPE["tck"],
        "tier": _SCOPE["tier"],
        "transport": _SCOPE["transport"],
        "summary": counts,
        "mismatches": mismatches,
        "results": results,
    }
    output = Path(_required_environment("FGENTIC_TCK_SCOPE_REPORT"))
    output.parent.mkdir(parents=True, exist_ok=True)
    output.write_text(json.dumps(report, indent=2, sort_keys=True) + "\n", encoding="utf-8")
    if mismatches and exitstatus == pytest.ExitCode.OK:
        session.exitstatus = pytest.ExitCode.TESTS_FAILED
