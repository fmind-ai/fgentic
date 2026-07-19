"""Keep public repository links and community routes consistent."""

from html.parser import HTMLParser
from pathlib import Path
from tempfile import TemporaryDirectory
from typing import cast
from unittest import TestCase
from urllib.parse import parse_qs, unquote, urlsplit

import yaml
from markdown import markdown as render_markdown

REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
DISCUSSION_TEMPLATE_DIRECTORY = REPOSITORY_ROOT / ".github/DISCUSSION_TEMPLATE"
ISSUE_TEMPLATE_CONFIG = REPOSITORY_ROOT / ".github/ISSUE_TEMPLATE/config.yml"
SUPPORT_POLICY = REPOSITORY_ROOT / ".github/SUPPORT.md"
NEW_DISCUSSION_PATH = "/fmind-ai/fgentic/discussions/new"
PUBLIC_ENTRYPOINTS = (
    ".agents/AGENTS.md",
    ".github/PULL_REQUEST_TEMPLATE.md",
    ".github/SUPPORT.md",
    "ADOPTERS.md",
    "CODE_OF_CONDUCT.md",
    "CONTRIBUTING.md",
    "GOVERNANCE.md",
    "MAINTAINERS.md",
    "README.md",
    "SECURITY.md",
)

type LinkViolation = tuple[str, str, str]


class _RenderedTargetParser(HTMLParser):
    """Collect link and image targets from rendered Markdown."""

    def __init__(self) -> None:
        super().__init__()
        self.targets: list[str] = []

    def handle_starttag(self, tag: str, attrs: list[tuple[str, str | None]]) -> None:
        attribute = "href" if tag == "a" else "src" if tag == "img" else ""
        if not attribute:
            return
        for name, value in attrs:
            if name == attribute and value:
                self.targets.append(value)


def _rendered_targets(markdown: str) -> list[str]:
    """Return targets that Markdown renders as links or images."""
    parser = _RenderedTargetParser()
    parser.feed(render_markdown(markdown, extensions=["fenced_code", "md_in_html", "tables"]))
    return parser.targets


def _structured_discussion_categories(targets: list[str]) -> set[str]:
    """Return structured Fgentic discussion categories from rendered targets."""
    categories: set[str] = set()
    for target in targets:
        parsed = urlsplit(target)
        if parsed.scheme != "https" or parsed.netloc != "github.com" or parsed.path != NEW_DISCUSSION_PATH:
            continue
        values = parse_qs(parsed.query).get("category", [])
        if len(values) == 1:
            categories.add(values[0])
    return categories


def _public_markdown(repository_root: Path = REPOSITORY_ROOT) -> tuple[Path, ...]:
    """Return RED-owned public Markdown outside the MkDocs corpus."""
    entrypoints = [repository_root / relative for relative in PUBLIC_ENTRYPOINTS]
    community = (repository_root / ".github/community").rglob("*.md")
    return tuple(sorted((*entrypoints, *community)))


def _display_path(path: Path, repository_root: Path) -> str:
    """Return a stable repository-relative diagnostic path when possible."""
    try:
        return path.relative_to(repository_root).as_posix()
    except ValueError:
        return path.as_posix()


def _link_violations(source: Path, repository_root: Path) -> list[LinkViolation]:
    """Return missing and repository-escaping local targets in one source."""
    source_name = _display_path(source, repository_root)
    if not source.is_file():
        return [(source_name, "(source)", "source file is missing")]

    violations: list[LinkViolation] = []
    for target in _rendered_targets(source.read_text(encoding="utf-8")):
        parsed = urlsplit(target)
        if parsed.scheme or parsed.netloc or not parsed.path:
            continue

        resolved = (source.parent / unquote(parsed.path)).resolve()
        if not resolved.is_relative_to(repository_root.resolve()):
            violations.append((source_name, target, "target escapes repository"))
        elif not resolved.exists():
            violations.append((source_name, target, "target does not exist"))
    return violations


def _require_valid_links(sources: tuple[Path, ...], repository_root: Path) -> None:
    """Reject public Markdown whose rendered local targets do not resolve."""
    violations = [violation for source in sources for violation in _link_violations(source, repository_root)]
    if not violations:
        return

    details = "\n".join(f"  {source}: {target} ({reason})" for source, target, reason in violations)
    raise AssertionError(f"repository Markdown link drift:\n{details}")


class RepositoryLinkIntegrityTest(TestCase):
    """Reject broken local links outside the MkDocs site corpus."""

    def test_current_public_markdown_targets_resolve(self) -> None:
        _require_valid_links(_public_markdown(), REPOSITORY_ROOT)

    def test_ignores_external_fragments_and_link_shaped_code(self) -> None:
        markdown = """
[Local](guide.md#section)
![Asset](asset.svg)
[External](https://example.com/missing.md)
[Mail](mailto:security@example.com)
[Fragment](#same-page)
`[Inline code](missing-inline.md)`

```markdown
[Fenced code](missing-fenced.md)
```
"""
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / "README.md"
            source.write_text(markdown, encoding="utf-8")
            (repository_root / "guide.md").touch()
            (repository_root / "asset.svg").touch()

            self.assertEqual(_link_violations(source, repository_root), [])

    def test_rejects_missing_and_repository_escaping_targets(self) -> None:
        markdown = "[Missing](missing.md)\n[Escape](../outside.md)\n"
        message = (
            r"repository Markdown link drift:\n"
            r"  README\.md: missing\.md \(target does not exist\)\n"
            r"  README\.md: \../outside\.md \(target escapes repository\)"
        )
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / "README.md"
            source.write_text(markdown, encoding="utf-8")

            with self.assertRaisesRegex(AssertionError, message):
                _require_valid_links((source,), repository_root)


class CommunityRouteIntegrityTest(TestCase):
    """Reject drift between structured forms and their public routes."""

    def test_structured_discussion_routes_stay_in_sync(self) -> None:
        expected = {path.stem for path in DISCUSSION_TEMPLATE_DIRECTORY.glob("*.yml")}
        config = cast("dict[str, object]", yaml.safe_load(ISSUE_TEMPLATE_CONFIG.read_text(encoding="utf-8")))
        contact_links = cast("list[dict[str, object]]", config["contact_links"])
        config_targets = [url for link in contact_links if isinstance(url := link.get("url"), str)]
        support_targets = _rendered_targets(SUPPORT_POLICY.read_text(encoding="utf-8"))

        self.assertSetEqual(
            _structured_discussion_categories(config_targets),
            expected,
            "issue chooser structured-discussion routes differ from discussion templates",
        )
        self.assertSetEqual(
            _structured_discussion_categories(support_targets),
            expected,
            "support-policy structured-discussion routes differ from discussion templates",
        )
