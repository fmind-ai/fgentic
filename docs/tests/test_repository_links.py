"""Keep public repository links and community routes consistent."""

import re
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
SAME_REPOSITORY_MAIN_PREFIXES = (
    "/fmind-ai/fgentic/blob/main/",
    "/fmind-ai/fgentic/tree/main/",
)
SAME_REPOSITORY_MAIN_URL = re.compile(r"https://github\.com/fmind-ai/fgentic/(?:blob|tree)/main/[^\s<>()\[\]`\"']+")
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


def _tracked_targets(markdown: str) -> list[str]:
    """Return rendered targets plus copy-ready same-repository main URLs."""
    targets = _rendered_targets(markdown)
    for match in SAME_REPOSITORY_MAIN_URL.findall(markdown):
        target = match.rstrip(".,;:!?")
        if target not in targets:
            targets.append(target)
    return targets


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
    """Return RED-owned public Markdown, including the MkDocs corpus."""
    entrypoints = [repository_root / relative for relative in PUBLIC_ENTRYPOINTS]
    community = (repository_root / ".github/community").rglob("*.md")
    docs_root = repository_root / "docs"
    documentation = (
        path
        for path in docs_root.rglob("*.md")
        if not any(part.startswith(".") for part in path.relative_to(docs_root).parts)
    )
    return tuple(sorted((*entrypoints, *community, *documentation)))


def _display_path(path: Path, repository_root: Path) -> str:
    """Return a stable repository-relative diagnostic path when possible."""
    try:
        return path.relative_to(repository_root).as_posix()
    except ValueError:
        return path.as_posix()


def _tracked_target(target: str, source: Path, repository_root: Path) -> Path | None:
    """Resolve local and canonical same-repository main targets offline."""
    parsed = urlsplit(target)
    if not parsed.path:
        return None
    if not parsed.scheme and not parsed.netloc:
        return source.parent / unquote(parsed.path)
    if parsed.scheme != "https" or parsed.netloc != "github.com":
        return None
    for prefix in SAME_REPOSITORY_MAIN_PREFIXES:
        if parsed.path.startswith(prefix):
            return repository_root / unquote(parsed.path.removeprefix(prefix))
    return None


def _link_violations(source: Path, repository_root: Path) -> list[LinkViolation]:
    """Return missing and repository-escaping tracked targets in one source."""
    source_name = _display_path(source, repository_root)
    if not source.is_file():
        return [(source_name, "(source)", "source file is missing")]

    violations: list[LinkViolation] = []
    for target in _tracked_targets(source.read_text(encoding="utf-8")):
        candidate = _tracked_target(target, source, repository_root)
        if candidate is None:
            continue

        resolved = candidate.resolve()
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
    """Reject broken tracked links in public repository Markdown."""

    def test_current_public_markdown_targets_resolve(self) -> None:
        _require_valid_links(_public_markdown(), REPOSITORY_ROOT)

    def test_ignores_external_fragments_and_link_shaped_code(self) -> None:
        markdown = """
[Local](guide.md#section)
![Asset](asset.svg)
[External](https://example.com/missing.md)
[Issue](https://github.com/fmind-ai/fgentic/issues/1)
[Other branch](https://github.com/fmind-ai/fgentic/blob/feature/missing.md)
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

    def test_resolves_same_repository_main_targets(self) -> None:
        markdown = "\n".join(
            (
                "[File](https://github.com/fmind-ai/fgentic/blob/main/docs/guide.md)",
                "[Directory](https://github.com/fmind-ai/fgentic/tree/main/docs)",
                "```text",
                "License: https://github.com/fmind-ai/fgentic/blob/main/LICENSE.",
                "```",
            )
        )
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / "README.md"
            source.write_text(markdown, encoding="utf-8")
            (repository_root / "docs").mkdir()
            (repository_root / "docs/guide.md").touch()
            (repository_root / "LICENSE").touch()

            self.assertEqual(_link_violations(source, repository_root), [])

    def test_rejects_missing_and_escaping_same_repository_targets(self) -> None:
        missing = "https://github.com/fmind-ai/fgentic/blob/main/docs/missing.md"
        escaping = "https://github.com/fmind-ai/fgentic/tree/main/%2E%2E/outside"
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / "README.md"
            source.write_text(f"{missing}\n[Escape]({escaping})\n", encoding="utf-8")

            self.assertCountEqual(
                _link_violations(source, repository_root),
                [
                    ("README.md", missing, "target does not exist"),
                    ("README.md", escaping, "target escapes repository"),
                ],
            )


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
