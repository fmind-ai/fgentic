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
ISSUE_TEMPLATE_DIRECTORY = REPOSITORY_ROOT / ".github/ISSUE_TEMPLATE"
ISSUE_TEMPLATE_CONFIG = REPOSITORY_ROOT / ".github/ISSUE_TEMPLATE/config.yml"
SUPPORT_POLICY = REPOSITORY_ROOT / ".github/SUPPORT.md"
NEW_DISCUSSION_PATH = "/fmind-ai/fgentic/discussions/new"
NEW_ISSUE_PATH = "/fmind-ai/fgentic/issues/new"
SAME_REPOSITORY_MAIN_PREFIXES = (
    "/fmind-ai/fgentic/blob/main/",
    "/fmind-ai/fgentic/tree/main/",
)
SAME_REPOSITORY_MAIN_URL = re.compile(r"https://github\.com/fmind-ai/fgentic/(?:blob|tree)/main/[^\s<>()\[\]`\"']+")
FORM_ID = re.compile(r"[A-Za-z0-9_-]+")
FORM_ELEMENT_TYPES = frozenset({"checkboxes", "dropdown", "input", "markdown", "textarea", "upload"})
RAW_URL_TRAILING_DELIMITERS = ".,;:!?*~}"
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
type RouteViolation = tuple[str, str, str]
type SchemaViolation = tuple[str, str]


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
    for match in SAME_REPOSITORY_MAIN_URL.finditer(markdown):
        target = match.group().rstrip(RAW_URL_TRAILING_DELIMITERS)
        for delimiter in ("__", "_"):
            prefix_start = match.start() - len(delimiter)
            if prefix_start >= 0 and markdown[prefix_start : match.start()] == delimiter and target.endswith(delimiter):
                target = target[: -len(delimiter)]
                break
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


def _issue_form_route_error(target: str, templates: set[str]) -> str | None:
    """Return an error for an invalid structured Fgentic issue-form route."""
    parsed = urlsplit(target)
    if parsed.scheme != "https" or parsed.netloc != "github.com" or parsed.path != NEW_ISSUE_PATH:
        return None

    query = parse_qs(parsed.query, keep_blank_values=True)
    if "template" not in query:
        return None

    values = query["template"]
    if len(values) != 1 or not values[0]:
        return "template query must contain one nonblank value"

    template = values[0]
    if Path(template).name != template or not template.endswith(".yml"):
        return f"invalid structured template name: {template}"
    if template not in templates:
        return f"structured template does not exist: {template}"
    return None


def _form_markdown(path: Path) -> str:
    """Return Markdown blocks embedded in one structured GitHub form."""
    document = yaml.safe_load(path.read_text(encoding="utf-8"))
    if not isinstance(document, dict) or not isinstance(body := document.get("body"), list):
        return ""

    blocks: list[str] = []
    for item in body:
        if not isinstance(item, dict) or item.get("type") != "markdown":
            continue
        attributes = item.get("attributes")
        if isinstance(attributes, dict) and isinstance(value := attributes.get("value"), str):
            blocks.append(value)
    return "\n".join(blocks)


def _structured_forms(repository_root: Path = REPOSITORY_ROOT) -> tuple[Path, ...]:
    """Return issue and discussion forms that can embed rendered Markdown."""
    issue_directory = repository_root / ".github/ISSUE_TEMPLATE"
    discussion_directory = repository_root / ".github/DISCUSSION_TEMPLATE"
    issue_forms = (path for path in issue_directory.glob("*.yml") if path.name != "config.yml")
    return tuple(sorted((*issue_forms, *discussion_directory.glob("*.yml"))))


def _form_schema_violations(source: Path, repository_root: Path) -> list[SchemaViolation]:
    """Return violations of GitHub's documented structured-form contract."""
    source_name = _display_path(source, repository_root)
    document = yaml.safe_load(source.read_text(encoding="utf-8"))
    if not isinstance(document, dict):
        return [(source_name, "form must be a mapping")]

    is_issue_form = source.parent.name == "ISSUE_TEMPLATE"
    violations: list[SchemaViolation] = (
        [
            (source_name, f"{key} must be a nonblank string")
            for key in ("name", "description")
            if not isinstance(value := document.get(key), str) or not value.strip()
        ]
        if is_issue_form
        else []
    )

    body = document.get("body")
    if not isinstance(body, list):
        violations.append((source_name, "body must be an array"))
        return violations

    identifiers: set[str] = set()
    has_input = False
    for index, element in enumerate(body):
        location = f"body[{index}]"
        if not isinstance(element, dict):
            violations.append((source_name, f"{location} must be a mapping"))
            continue

        element_type = element.get("type")
        if not isinstance(element_type, str) or element_type not in FORM_ELEMENT_TYPES:
            violations.append((source_name, f"{location}.type is unsupported: {element_type!r}"))
            continue
        if element_type != "markdown":
            has_input = True

        identifier = element.get("id")
        if identifier is None:
            continue
        if element_type == "markdown":
            violations.append((source_name, f"{location}.id is not supported for markdown elements"))
        elif not isinstance(identifier, str) or FORM_ID.fullmatch(identifier) is None:
            violations.append((source_name, f"{location}.id is invalid: {identifier!r}"))
        elif identifier in identifiers:
            violations.append((source_name, f"{location}.id is duplicated: {identifier!r}"))
        else:
            identifiers.add(identifier)

    if not has_input:
        violations.append((source_name, "body must contain at least one non-Markdown field"))
    return violations


def _require_valid_form_schemas(sources: tuple[Path, ...], repository_root: Path) -> None:
    """Reject structured GitHub forms that violate the documented schema."""
    violations = [violation for source in sources for violation in _form_schema_violations(source, repository_root)]
    if not violations:
        return

    details = "\n".join(f"  {source}: {reason}" for source, reason in violations)
    raise AssertionError(f"structured GitHub form schema drift:\n{details}")


def _issue_form_route_violations(sources: dict[str, list[str]], templates: set[str]) -> list[RouteViolation]:
    """Return invalid same-repository issue-form routes by public source."""
    violations: list[RouteViolation] = []
    for source, targets in sources.items():
        violations.extend(
            (source, target, error)
            for target in targets
            if (error := _issue_form_route_error(target, templates)) is not None
        )
    return violations


def _require_valid_issue_form_routes(sources: dict[str, list[str]], templates: set[str]) -> None:
    """Reject public routes to malformed or missing structured issue forms."""
    violations = _issue_form_route_violations(sources, templates)
    if not violations:
        return

    details = "\n".join(f"  {source}: {target} ({reason})" for source, target, reason in violations)
    raise AssertionError(f"structured issue-form route drift:\n{details}")


def _public_markdown(repository_root: Path = REPOSITORY_ROOT) -> tuple[Path, ...]:
    """Return RED-owned public Markdown, including the MkDocs corpus."""
    entrypoints = [repository_root / relative for relative in PUBLIC_ENTRYPOINTS]
    agent_skills = (repository_root / ".agents/skills").rglob("SKILL.md")
    community = (repository_root / ".github/community").rglob("*.md")
    docs_root = repository_root / "docs"
    documentation = (
        path
        for path in docs_root.rglob("*.md")
        if not any(part.startswith(".") for part in path.relative_to(docs_root).parts)
    )
    return tuple(sorted((*entrypoints, *agent_skills, *community, *documentation)))


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


def _markdown_link_violations(markdown: str, source: Path, repository_root: Path) -> list[LinkViolation]:
    """Return missing and repository-escaping tracked targets in Markdown."""
    source_name = _display_path(source, repository_root)
    violations: list[LinkViolation] = []
    for target in _tracked_targets(markdown):
        candidate = _tracked_target(target, source, repository_root)
        if candidate is None:
            continue

        resolved = candidate.resolve()
        if not resolved.is_relative_to(repository_root.resolve()):
            violations.append((source_name, target, "target escapes repository"))
        elif not resolved.exists():
            violations.append((source_name, target, "target does not exist"))
    return violations


def _link_violations(source: Path, repository_root: Path) -> list[LinkViolation]:
    """Return missing and repository-escaping tracked targets in one source."""
    if not source.is_file():
        return [(_display_path(source, repository_root), "(source)", "source file is missing")]
    return _markdown_link_violations(source.read_text(encoding="utf-8"), source, repository_root)


def _require_valid_links(sources: tuple[Path, ...], repository_root: Path) -> None:
    """Reject public Markdown whose rendered local targets do not resolve."""
    violations = [violation for source in sources for violation in _link_violations(source, repository_root)]
    if not violations:
        return

    details = "\n".join(f"  {source}: {target} ({reason})" for source, target, reason in violations)
    raise AssertionError(f"repository Markdown link drift:\n{details}")


def _require_valid_form_markdown_links(sources: tuple[Path, ...], repository_root: Path) -> None:
    """Reject broken tracked targets in Markdown blocks embedded in forms."""
    violations = [
        violation
        for source in sources
        for violation in _markdown_link_violations(_form_markdown(source), source, repository_root)
    ]
    if not violations:
        return

    details = "\n".join(f"  {source}: {target} ({reason})" for source, target, reason in violations)
    raise AssertionError(f"repository Markdown link drift:\n{details}")


class RepositoryLinkIntegrityTest(TestCase):
    """Reject broken tracked links in public repository Markdown."""

    def test_current_public_markdown_targets_resolve(self) -> None:
        _require_valid_links(_public_markdown(), REPOSITORY_ROOT)

    def test_discovers_agent_skills_and_reports_broken_skill_links(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            skill = repository_root / ".agents/skills/example/SKILL.md"
            skill.parent.mkdir(parents=True)
            skill.write_text("[Missing](../../../missing.md)\n", encoding="utf-8")
            notes = skill.with_name("notes.md")
            notes.touch()

            sources = _public_markdown(repository_root)

            self.assertIn(skill, sources)
            self.assertNotIn(notes, sources)
            self.assertEqual(
                _link_violations(skill, repository_root),
                [(".agents/skills/example/SKILL.md", "../../../missing.md", "target does not exist")],
            )
            with self.assertRaisesRegex(
                AssertionError,
                r"repository Markdown link drift:\n"
                r"  \.agents/skills/example/SKILL\.md: \../\../\../missing\.md \(target does not exist\)",
            ):
                _require_valid_links((skill,), repository_root)

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
                "**Documentation: https://github.com/fmind-ai/fgentic/blob/main/README.md**",
                "__https://github.com/fmind-ai/fgentic/blob/main/README.md__",
                "_https://github.com/fmind-ai/fgentic/blob/main/README.md_",
                "{Source: https://github.com/fmind-ai/fgentic/tree/main/docs}",
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

    def test_discovers_issue_and_discussion_forms(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            issue_directory = repository_root / ".github/ISSUE_TEMPLATE"
            discussion_directory = repository_root / ".github/DISCUSSION_TEMPLATE"
            issue_directory.mkdir(parents=True)
            discussion_directory.mkdir(parents=True)
            (issue_directory / "bug.yml").touch()
            (issue_directory / "config.yml").touch()
            (discussion_directory / "q-a.yml").touch()

            self.assertEqual(
                [path.relative_to(repository_root).as_posix() for path in _structured_forms(repository_root)],
                [".github/DISCUSSION_TEMPLATE/q-a.yml", ".github/ISSUE_TEMPLATE/bug.yml"],
            )

    def test_embedded_form_markdown_targets_resolve(self) -> None:
        _require_valid_form_markdown_links(_structured_forms(), REPOSITORY_ROOT)

    def test_structured_forms_follow_github_schema(self) -> None:
        _require_valid_form_schemas(_structured_forms(), REPOSITORY_ROOT)

    def test_rejects_invalid_issue_form_structure_and_elements(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "body:",
                        "  - type: button",
                        "  - type: input",
                        "    id: bad.id",
                        "  - type: textarea",
                        "    id: repeated",
                        "  - type: dropdown",
                        "    id: repeated",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/broken.yml", "name must be a nonblank string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "description must be a nonblank string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].type is unsupported: 'button'"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[1].id is invalid: 'bad.id'"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[3].id is duplicated: 'repeated'"),
                ],
            )

    def test_rejects_markdown_only_discussion_form(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/DISCUSSION_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        'title: "[Broken] "',
                        "body:",
                        "  - type: markdown",
                        "    attributes:",
                        "      value: No input is collected.",
                    )
                ),
                encoding="utf-8",
            )

            with self.assertRaisesRegex(
                AssertionError,
                r"structured GitHub form schema drift:\n"
                r"  \.github/DISCUSSION_TEMPLATE/broken\.yml: "
                r"body must contain at least one non-Markdown field",
            ):
                _require_valid_form_schemas((source,), repository_root)

    def test_rejects_markdown_only_issue_form(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Collects no user input",
                        "body:",
                        "  - type: markdown",
                        "    attributes:",
                        "      value: No input is collected.",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body must contain at least one non-Markdown field",
                    )
                ],
            )

    def test_reports_broken_embedded_form_markdown_against_its_source(self) -> None:
        canonical_existing = "https://github.com/fmind-ai/fgentic/blob/main/docs/guide.md"
        local_existing = "../../docs/guide.md"
        local_missing = "../../docs/missing.md"
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/DISCUSSION_TEMPLATE/q-a.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        'title: "[Question] "',
                        "body:",
                        "  - type: markdown",
                        "    attributes:",
                        "      value: |",
                        f"        [Canonical guide]({canonical_existing})",
                        f"        [Local guide]({local_existing})",
                        f"        [Missing]({local_missing})",
                    )
                ),
                encoding="utf-8",
            )
            guide = repository_root / "docs/guide.md"
            guide.parent.mkdir()
            guide.touch()

            with self.assertRaisesRegex(
                AssertionError,
                r"repository Markdown link drift:\n"
                r"  \.github/DISCUSSION_TEMPLATE/q-a\.yml: "
                r"\.\./\.\./docs/missing\.md "
                r"\(target does not exist\)",
            ):
                _require_valid_form_markdown_links((source,), repository_root)

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

    def test_structured_issue_form_routes_resolve(self) -> None:
        templates = {path.name for path in ISSUE_TEMPLATE_DIRECTORY.glob("*.yml") if path != ISSUE_TEMPLATE_CONFIG}
        sources = {
            ".github/SUPPORT.md": _rendered_targets(SUPPORT_POLICY.read_text(encoding="utf-8")),
            **{
                path.relative_to(REPOSITORY_ROOT).as_posix(): _rendered_targets(_form_markdown(path))
                for path in sorted(DISCUSSION_TEMPLATE_DIRECTORY.glob("*.yml"))
            },
        }

        _require_valid_issue_form_routes(sources, templates)

    def test_rejects_malformed_and_missing_issue_form_routes(self) -> None:
        missing = "https://github.com/fmind-ai/fgentic/issues/new?template=missing.yml"
        blank = "https://github.com/fmind-ai/fgentic/issues/new?template="
        duplicated = (
            "https://github.com/fmind-ai/fgentic/issues/new?template=bug_report.yml&template=feature_request.yml"
        )
        traversal = "https://github.com/fmind-ai/fgentic/issues/new?template=../bug_report.yml"
        sources = {"SUPPORT.md": [missing, blank, duplicated, traversal]}

        self.assertEqual(
            _issue_form_route_violations(sources, {"bug_report.yml", "feature_request.yml"}),
            [
                ("SUPPORT.md", missing, "structured template does not exist: missing.yml"),
                ("SUPPORT.md", blank, "template query must contain one nonblank value"),
                ("SUPPORT.md", duplicated, "template query must contain one nonblank value"),
                ("SUPPORT.md", traversal, "invalid structured template name: ../bug_report.yml"),
            ],
        )
        with self.assertRaisesRegex(
            AssertionError,
            r"structured issue-form route drift:\n"
            r"  SUPPORT\.md: https://github\.com/fmind-ai/fgentic/issues/new\?template=missing\.yml "
            r"\(structured template does not exist: missing\.yml\)",
        ):
            _require_valid_issue_form_routes({"SUPPORT.md": [missing]}, {"bug_report.yml"})

    def test_ignores_non_template_and_other_repository_issue_routes(self) -> None:
        targets = [
            "https://github.com/fmind-ai/fgentic/issues/1",
            "https://github.com/fmind-ai/fgentic/pull/1",
            "https://github.com/fmind-ai/fgentic/issues/new",
            "https://github.com/fmind-ai/fgentic/security/advisories/new",
            "https://github.com/example/fgentic/issues/new?template=missing.yml",
            "http://github.com/fmind-ai/fgentic/issues/new?template=missing.yml",
        ]

        self.assertEqual(_issue_form_route_violations({"source": targets}, set()), [])
