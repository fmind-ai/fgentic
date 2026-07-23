"""Keep every published documentation page discoverable exactly once."""

from collections import Counter
from pathlib import Path
from unittest import TestCase
from urllib.parse import urlsplit

import yaml
from yaml.nodes import MappingNode, Node, ScalarNode, SequenceNode

DOCS_ROOT = Path(__file__).resolve().parents[1]
MKDOCS_CONFIG = DOCS_ROOT / "mkdocs.yml"

type NavigationDrift = tuple[tuple[str, ...], tuple[str, ...], tuple[str, ...]]


def _navigation_paths(value: object) -> list[str]:
    """Flatten internal Markdown paths from a nested MkDocs nav value."""
    if isinstance(value, str):
        parsed = urlsplit(value)
        if parsed.scheme or parsed.netloc:
            return []
        return [parsed.path] if parsed.path.endswith(".md") else []
    if isinstance(value, list):
        return [path for item in value for path in _navigation_paths(item)]
    if isinstance(value, dict):
        paths: list[str] = []
        for label, item in value.items():
            if not isinstance(label, str):
                msg = f"MkDocs navigation label must be a string, got {type(label).__name__}"
                raise TypeError(msg)
            paths.extend(_navigation_paths(item))
        return paths
    msg = f"MkDocs navigation value has unsupported type {type(value).__name__}"
    raise TypeError(msg)


def _navigation_drift(pages: set[str], navigation: list[str]) -> NavigationDrift:
    """Return missing pages, stale nav paths, and duplicate nav paths."""
    counts = Counter(navigation)
    missing = tuple(sorted(pages - counts.keys()))
    stale = tuple(sorted(counts.keys() - pages))
    duplicates = tuple(sorted(path for path, count in counts.items() if count > 1))
    return missing, stale, duplicates


def _drift_message(drift: NavigationDrift) -> str:
    """Render each inventory error as an actionable path list."""
    missing, stale, duplicates = drift
    return "\n".join(
        (
            "documentation navigation drift:",
            f"  missing pages: {', '.join(missing) or '(none)'}",
            f"  stale paths: {', '.join(stale) or '(none)'}",
            f"  duplicate paths: {', '.join(duplicates) or '(none)'}",
        )
    )


def _require_complete_navigation(pages: set[str], navigation: list[str]) -> None:
    """Reject any difference between publishable pages and configured nav."""
    drift = _navigation_drift(pages, navigation)
    if drift != ((), (), ()):
        raise AssertionError(_drift_message(drift))


def _yaml_value(node: Node) -> object:
    """Convert ordinary YAML containers without constructing custom tags."""
    if isinstance(node, ScalarNode):
        return node.value
    if isinstance(node, SequenceNode):
        return [_yaml_value(item) for item in node.value]
    if isinstance(node, MappingNode):
        values: dict[str, object] = {}
        for key, value in node.value:
            if not isinstance(key, ScalarNode):
                msg = "MkDocs navigation keys must be scalars"
                raise TypeError(msg)
            values[key.value] = _yaml_value(value)
        return values
    msg = f"MkDocs navigation contains unsupported YAML node {type(node).__name__}"
    raise TypeError(msg)


def _load_navigation() -> list[str]:
    """Load nav without executing the config's trusted Python-tagged slugifier."""
    config = yaml.compose(MKDOCS_CONFIG.read_text(encoding="utf-8"))
    if isinstance(config, MappingNode):
        for key, value in config.value:
            if isinstance(key, ScalarNode) and key.value == "nav":
                return _navigation_paths(_yaml_value(value))
    msg = "docs/mkdocs.yml must define a navigation tree"
    raise TypeError(msg)


def _documentation_pages() -> set[str]:
    """Return Markdown pages MkDocs can publish, excluding hidden tool state."""
    pages: set[str] = set()
    for path in DOCS_ROOT.rglob("*.md"):
        relative = path.relative_to(DOCS_ROOT)
        if path.is_file() and not any(part.startswith(".") for part in relative.parts):
            pages.add(relative.as_posix())
    return pages


class NavigationCompletenessTest(TestCase):
    """Reject documentation that is hidden, stale, or multiply listed."""

    def test_current_corpus_is_listed_exactly_once(self) -> None:
        _require_complete_navigation(_documentation_pages(), _load_navigation())

    def test_flattens_nested_navigation(self) -> None:
        navigation: object = [
            {"Home": "index.md"},
            {
                "Guides": [
                    {"Operations": "operations.md#day-2"},
                    {"External": "https://example.com/readme.md"},
                ]
            },
        ]

        self.assertEqual(_navigation_paths(navigation), ["index.md", "operations.md"])

    def test_rejects_missing_stale_and_duplicate_paths(self) -> None:
        message = (
            r"missing pages: guide\.md\n"
            r"  stale paths: missing\.md\n"
            r"  duplicate paths: index\.md"
        )

        with self.assertRaisesRegex(AssertionError, message):
            _require_complete_navigation(
                {"index.md", "guide.md"},
                ["index.md", "index.md", "missing.md"],
            )
