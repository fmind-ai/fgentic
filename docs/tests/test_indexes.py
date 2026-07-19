"""Keep every OKF directory index synchronized with its local concept pages."""

import re
from collections import Counter
from pathlib import Path
from unittest import TestCase
from urllib.parse import unquote, urlsplit

DOCS_ROOT = Path(__file__).resolve().parents[1]
RESERVED_INDEXES = frozenset(
    {
        "index.md",
        "adopters/index.md",
        "adr/index.md",
        "onboarding/index.md",
        "security/index.md",
    }
)
_MARKDOWN_LINK_TARGET = re.compile(
    r"(?<!!)\[[^\]\n]*\]\("
    r"(?P<target><[^>\n]+>|[^)\s\n]+)"
    r"(?:\s+(?:\"[^\"]*\"|'[^']*'))?\)"
)

type IndexDrift = tuple[tuple[str, ...], tuple[str, ...], tuple[str, ...]]
type ReservedIndexDrift = tuple[tuple[str, ...], tuple[str, ...]]


def _local_index_targets(markdown: str, index_path: Path) -> list[str]:
    """Return direct Markdown concept targets owned by one directory index."""
    directory = index_path.parent.resolve()
    targets: list[str] = []
    for match in _MARKDOWN_LINK_TARGET.finditer(markdown):
        target = match.group("target")
        raw_target = target[1:-1] if target.startswith("<") and target.endswith(">") else target
        parsed = urlsplit(raw_target)
        if parsed.scheme or parsed.netloc or not parsed.path or parsed.path.startswith("/"):
            continue

        resolved = (directory / unquote(parsed.path)).resolve()
        if resolved.parent == directory and resolved.suffix == ".md" and resolved.name != "index.md":
            targets.append(resolved.name)
    return targets


def _concept_pages(directory: Path) -> set[str]:
    """Return direct concept pages owned by a documentation directory."""
    return {path.name for path in directory.glob("*.md") if path.is_file() and path.name != "index.md"}


def _index_drift(pages: set[str], targets: list[str]) -> IndexDrift:
    """Return missing pages, stale targets, and duplicate targets."""
    counts = Counter(targets)
    missing = tuple(sorted(pages - counts.keys()))
    stale = tuple(sorted(counts.keys() - pages))
    duplicates = tuple(sorted(target for target, count in counts.items() if count > 1))
    return missing, stale, duplicates


def _index_drift_message(index_path: str, drift: IndexDrift) -> str:
    """Render one directory index error as actionable path lists."""
    missing, stale, duplicates = drift
    return "\n".join(
        (
            f"documentation index drift for {index_path}:",
            f"  missing pages: {', '.join(missing) or '(none)'}",
            f"  stale targets: {', '.join(stale) or '(none)'}",
            f"  duplicate targets: {', '.join(duplicates) or '(none)'}",
        )
    )


def _require_complete_index(pages: set[str], targets: list[str], index_path: str) -> None:
    """Reject any difference between local concept pages and their index."""
    drift = _index_drift(pages, targets)
    if drift != ((), (), ()):
        raise AssertionError(_index_drift_message(index_path, drift))


def _reserved_index_drift(found: set[str]) -> ReservedIndexDrift:
    """Return missing and unexpected directory indexes."""
    return tuple(sorted(RESERVED_INDEXES - found)), tuple(sorted(found - RESERVED_INDEXES))


def _require_reserved_indexes(found: set[str]) -> None:
    """Reject directory indexes outside the docs-spec reserved set."""
    missing, unexpected = _reserved_index_drift(found)
    if missing or unexpected:
        message = "\n".join(
            (
                "documentation reserved-index drift:",
                f"  missing indexes: {', '.join(missing) or '(none)'}",
                f"  unexpected indexes: {', '.join(unexpected) or '(none)'}",
            )
        )
        raise AssertionError(message)


def _documentation_indexes() -> set[str]:
    """Return every tracked-shape directory index under docs."""
    return {path.relative_to(DOCS_ROOT).as_posix() for path in DOCS_ROOT.rglob("index.md") if path.is_file()}


class IndexCompletenessTest(TestCase):
    """Reject hidden, stale, duplicate, or unreserved OKF index entries."""

    def test_current_indexes_list_local_concepts_exactly_once(self) -> None:
        indexes = _documentation_indexes()
        _require_reserved_indexes(indexes)

        for relative in sorted(indexes):
            index_path = DOCS_ROOT / relative
            _require_complete_index(
                _concept_pages(index_path.parent),
                _local_index_targets(index_path.read_text(encoding="utf-8"), index_path),
                relative,
            )

    def test_rejects_missing_stale_and_duplicate_targets(self) -> None:
        message = (
            r"documentation index drift for guides/index\.md:\n"
            r"  missing pages: missing\.md\n"
            r"  stale targets: stale\.md\n"
            r"  duplicate targets: listed\.md"
        )

        with self.assertRaisesRegex(AssertionError, message):
            _require_complete_index(
                {"listed.md", "missing.md"},
                ["listed.md", "listed.md", "stale.md"],
                "guides/index.md",
            )

    def test_counts_fragments_but_ignores_external_and_cross_directory_links(self) -> None:
        markdown = "\n".join(
            (
                "[Local](guide.md#section)",
                "[External](https://example.com/guide.md)",
                "[Cross-directory](../security.md)",
                "[Absolute](/guide.md)",
            )
        )

        self.assertEqual(_local_index_targets(markdown, DOCS_ROOT / "onboarding/index.md"), ["guide.md"])

    def test_rejects_reserved_index_set_drift(self) -> None:
        found = set(RESERVED_INDEXES)
        found.remove("security/index.md")
        found.add("guides/index.md")
        message = (
            r"documentation reserved-index drift:\n"
            r"  missing indexes: security/index\.md\n"
            r"  unexpected indexes: guides/index\.md"
        )

        with self.assertRaisesRegex(AssertionError, message):
            _require_reserved_indexes(found)
