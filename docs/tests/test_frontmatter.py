"""Enforce the OKF metadata contract for every visible documentation page."""

from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import TestCase

import yaml
from yaml.nodes import MappingNode, ScalarNode

DOCS_ROOT = Path(__file__).resolve().parents[1]
ROOT_INDEX = "index.md"
SUB_INDEXES = frozenset(
    {
        "adopters/index.md",
        "adr/index.md",
        "onboarding/index.md",
        "security/index.md",
    }
)
REQUIRED_FIELDS = ("type", "title", "description")
SENTENCE_ENDINGS = (".", "!", "?")
DESCRIPTION_REQUIREMENT = "frontmatter field `description` must be a single-line sentence ending in punctuation"


def _frontmatter(markdown: str, path: str) -> tuple[dict[object, object] | None, bool, tuple[str, ...]]:
    """Safely parse a leading YAML frontmatter mapping."""
    lines = markdown.splitlines()
    if not lines or lines[0] != "---":
        return None, True, (f"{path}: missing YAML frontmatter",)
    try:
        closing = lines.index("---", 1)
    except ValueError:
        return None, True, (f"{path}: YAML frontmatter has no closing delimiter",)

    raw = "\n".join(lines[1:closing])
    try:
        metadata = yaml.safe_load(raw)
        node = yaml.compose(raw, Loader=yaml.SafeLoader)
    except yaml.YAMLError as error:
        problem = getattr(error, "problem", None) or str(error).splitlines()[0]
        return None, True, (f"{path}: invalid YAML frontmatter ({problem})",)
    if not isinstance(metadata, dict):
        return None, True, (f"{path}: YAML frontmatter must be a mapping",)

    description_single_line = True
    if isinstance(node, MappingNode):
        for key, value in node.value:
            if isinstance(key, ScalarNode) and key.value == "description":
                description_single_line = value.start_mark.line == value.end_mark.line
    return metadata, description_single_line, ()


def _concept_errors(markdown: str, path: str) -> tuple[str, ...]:
    """Return every required metadata error for one concept document."""
    metadata, description_single_line, errors = _frontmatter(markdown, path)
    if errors or metadata is None:
        return errors

    found: list[str] = []
    for field in REQUIRED_FIELDS:
        if field not in metadata:
            found.append(f"{path}: missing frontmatter field `{field}`")
            continue
        value = metadata[field]
        if not isinstance(value, str) or not value.strip():
            found.append(f"{path}: frontmatter field `{field}` must be a nonblank string")

    description = metadata.get("description")
    if (
        isinstance(description, str)
        and description.strip()
        and (
            not description_single_line
            or "\n" in description
            or "\r" in description
            or not description.rstrip().endswith(SENTENCE_ENDINGS)
        )
    ):
        found.append(f"{path}: {DESCRIPTION_REQUIREMENT}")
    return tuple(found)


def _root_index_errors(markdown: str) -> tuple[str, ...]:
    """Require the sole OKF root-index metadata exception."""
    metadata, _, errors = _frontmatter(markdown, ROOT_INDEX)
    if errors or metadata is None:
        return errors
    if metadata != {"okf_version": "0.1"}:
        return (f'{ROOT_INDEX}: frontmatter must contain only `okf_version: "0.1"`',)
    return ()


def _sub_index_errors(markdown: str, path: str) -> tuple[str, ...]:
    """Reject frontmatter on OKF directory listings."""
    if markdown.startswith("---\n") or markdown.startswith("---\r\n"):
        return (f"{path}: sub-index must not carry frontmatter",)
    return ()


def _visible_markdown(docs_root: Path = DOCS_ROOT) -> list[Path]:
    """Return authored Markdown while excluding hidden tool state."""
    return sorted(
        path
        for path in docs_root.rglob("*.md")
        if path.is_file() and not any(part.startswith(".") for part in path.relative_to(docs_root).parts)
    )


def _corpus_errors(docs_root: Path = DOCS_ROOT) -> tuple[str, ...]:
    """Return frontmatter errors across every visible documentation page."""
    errors: list[str] = []
    for page in _visible_markdown(docs_root):
        relative = page.relative_to(docs_root).as_posix()
        markdown = page.read_text(encoding="utf-8")
        if relative == ROOT_INDEX:
            errors.extend(_root_index_errors(markdown))
        elif relative in SUB_INDEXES:
            errors.extend(_sub_index_errors(markdown, relative))
        else:
            errors.extend(_concept_errors(markdown, relative))
    return tuple(errors)


def _require_valid_corpus(docs_root: Path = DOCS_ROOT) -> None:
    """Reject malformed, incomplete, or incorrectly exempted metadata."""
    errors = _corpus_errors(docs_root)
    if errors:
        raise AssertionError("documentation frontmatter drift:\n  " + "\n  ".join(errors))


class FrontmatterTest(TestCase):
    """Keep OKF metadata machine-readable, complete, and actionable."""

    def test_current_corpus_has_valid_frontmatter(self) -> None:
        _require_valid_corpus()

    def test_rejects_malformed_yaml(self) -> None:
        errors = _concept_errors(
            "---\ntype: Guide\ntitle: Example\ndescription: Broken: YAML.\n---\n",
            "guide.md",
        )

        self.assertEqual(errors, ("guide.md: invalid YAML frontmatter (mapping values are not allowed here)",))

    def test_reports_missing_non_string_and_blank_fields(self) -> None:
        errors = _concept_errors(
            "---\ntype: ''\ntitle: 42\nextra: value\n---\n",
            "guide.md",
        )

        self.assertEqual(
            errors,
            (
                "guide.md: frontmatter field `type` must be a nonblank string",
                "guide.md: frontmatter field `title` must be a nonblank string",
                "guide.md: missing frontmatter field `description`",
            ),
        )

    def test_rejects_invalid_description_shape(self) -> None:
        for description in ("No terminal punctuation", ">-\n  First line.\n  Second line."):
            with self.subTest(description=description):
                errors = _concept_errors(
                    f"---\ntype: Guide\ntitle: Example\ndescription: {description}\n---\n",
                    "guide.md",
                )

                self.assertEqual(
                    errors,
                    (f"guide.md: {DESCRIPTION_REQUIREMENT}",),
                )

    def test_accepts_quoted_colon_description(self) -> None:
        errors = _concept_errors(
            '---\ntype: Guide\ntitle: Example\ndescription: "Scope: one bounded sentence."\n---\n',
            "guide.md",
        )

        self.assertEqual(errors, ())

    def test_enforces_index_exceptions(self) -> None:
        self.assertEqual(_root_index_errors('---\nokf_version: "0.1"\n---\n'), ())
        self.assertEqual(
            _root_index_errors("---\ntype: Index\n---\n"),
            ('index.md: frontmatter must contain only `okf_version: "0.1"`',),
        )
        self.assertEqual(_sub_index_errors("# Security\n", "security/index.md"), ())
        self.assertEqual(
            _sub_index_errors("---\ntype: Index\n---\n", "security/index.md"),
            ("security/index.md: sub-index must not carry frontmatter",),
        )

    def test_ignores_hidden_tool_state(self) -> None:
        with TemporaryDirectory() as temporary:
            docs_root = Path(temporary)
            (docs_root / "index.md").write_text('---\nokf_version: "0.1"\n---\n', encoding="utf-8")
            hidden = docs_root / ".venv/share/example"
            hidden.mkdir(parents=True)
            (hidden / "broken.md").write_text("not frontmatter", encoding="utf-8")

            self.assertEqual(_corpus_errors(docs_root), ())
