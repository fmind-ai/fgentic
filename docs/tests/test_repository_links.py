"""Keep public repository links and community routes consistent."""

import re
from collections.abc import Hashable
from html.parser import HTMLParser
from pathlib import Path
from tempfile import TemporaryDirectory
from typing import cast
from unittest import TestCase
from urllib.parse import parse_qs, unquote, urlsplit

import yaml
from markdown import markdown as render_markdown
from yaml.nodes import MappingNode, Node, ScalarNode, SequenceNode

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
ISSUE_FORM_KEYS = frozenset({"assignees", "body", "description", "labels", "name", "projects", "title", "type"})
DISCUSSION_FORM_KEYS = frozenset({"body", "labels", "title"})
FORM_COMMON_ELEMENT_KEYS = frozenset({"attributes", "id", "type", "validations"})
FORM_ELEMENT_KEYS = {
    "checkboxes": FORM_COMMON_ELEMENT_KEYS,
    "dropdown": FORM_COMMON_ELEMENT_KEYS,
    "input": FORM_COMMON_ELEMENT_KEYS,
    "markdown": frozenset({"attributes", "type"}),
    "textarea": FORM_COMMON_ELEMENT_KEYS,
    "upload": FORM_COMMON_ELEMENT_KEYS,
}
FORM_ATTRIBUTE_KEYS = {
    "checkboxes": frozenset({"description", "label", "options"}),
    "dropdown": frozenset({"default", "description", "label", "multiple", "options"}),
    "input": frozenset({"description", "label", "placeholder", "value"}),
    "markdown": frozenset({"value"}),
    "textarea": frozenset({"description", "label", "placeholder", "render", "value"}),
    "upload": frozenset({"description", "label"}),
}
FORM_VALIDATION_KEYS = {
    "checkboxes": frozenset({"required"}),
    "dropdown": frozenset({"required"}),
    "input": frozenset({"required"}),
    "markdown": frozenset(),
    "textarea": frozenset({"required"}),
    "upload": frozenset({"accept", "required"}),
}
CHECKBOX_OPTION_KEYS = frozenset({"label", "required"})
ISSUE_CHOOSER_KEYS = frozenset({"blank_issues_enabled", "contact_links"})
ISSUE_CHOOSER_CONTACT_LINK_KEYS = frozenset({"about", "name", "url"})
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
type YamlScalarKey = tuple[str, Hashable]


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


def _is_nonblank_string(value: object) -> bool:
    """Return whether a form value is a nonblank string."""
    return isinstance(value, str) and bool(value.strip())


def _is_nonblank_string_collection(value: object) -> bool:
    """Return whether form metadata is a nonblank string collection."""
    if isinstance(value, str):
        return all(part.strip() for part in value.split(","))
    return isinstance(value, list) and all(_is_nonblank_string(item) for item in value)


def _unsupported_key_reasons(
    mapping: dict[object, object],
    allowed_keys: frozenset[str],
    location: str = "",
) -> list[str]:
    """Return stable exact-path diagnostics for unsupported mapping keys."""
    prefix = f"{location}." if location else ""
    unsupported = sorted(
        (key for key in mapping if not isinstance(key, str) or key not in allowed_keys),
        key=repr,
    )
    return [f"{prefix}{key if isinstance(key, str) else repr(key)} is not permitted" for key in unsupported]


def _yaml_scalar_key(node: ScalarNode) -> tuple[YamlScalarKey, str]:
    """Return a resolved identity and diagnostic label for one YAML scalar key."""
    loader = yaml.SafeLoader("")
    try:
        value = loader.construct_object(node, deep=True)
    finally:
        loader.dispose()
    if not isinstance(value, Hashable):
        raise TypeError(f"YAML scalar key resolved to unhashable {type(value).__name__}")
    label = value if isinstance(value, str) else repr(value)
    return (node.tag, value), label


def _duplicate_yaml_key_reasons(node: Node | None, location: str = "") -> list[str]:
    """Return stable exact-path diagnostics for duplicate scalar mapping keys."""
    if isinstance(node, SequenceNode):
        return [
            reason
            for index, child in enumerate(node.value)
            for reason in _duplicate_yaml_key_reasons(child, f"{location}[{index}]")
        ]
    if not isinstance(node, MappingNode):
        return []

    reasons: list[str] = []
    seen: set[YamlScalarKey] = set()
    for key_node, value_node in node.value:
        child_location = location
        if isinstance(key_node, ScalarNode):
            identity, label = _yaml_scalar_key(key_node)
            child_location = f"{location}.{label}" if location else label
            if identity in seen:
                reasons.append(f"{child_location} is duplicated")
            else:
                seen.add(identity)
        reasons.extend(_duplicate_yaml_key_reasons(value_node, child_location))
    return reasons


def _load_structured_yaml(source: Path, repository_root: Path) -> tuple[object, list[SchemaViolation]]:
    """Compose YAML to reject duplicate keys before constructing its document."""
    raw_document = source.read_text(encoding="utf-8")
    source_name = _display_path(source, repository_root)
    node = yaml.compose(raw_document, Loader=yaml.SafeLoader)
    violations = [(source_name, reason) for reason in _duplicate_yaml_key_reasons(node)]
    return yaml.safe_load(raw_document), violations


def _issue_chooser_config_violations(source: Path, repository_root: Path) -> list[SchemaViolation]:
    """Return violations of GitHub's documented issue chooser configuration."""
    source_name = _display_path(source, repository_root)
    document, violations = _load_structured_yaml(source, repository_root)
    if not isinstance(document, dict):
        return [*violations, (source_name, "config must be a mapping")]

    violations.extend(
        (source_name, reason)
        for reason in _unsupported_key_reasons(cast(dict[object, object], document), ISSUE_CHOOSER_KEYS)
    )
    blank_issues_enabled = document.get("blank_issues_enabled")
    if "blank_issues_enabled" in document and not isinstance(blank_issues_enabled, bool):
        violations.append((source_name, "blank_issues_enabled must be a Boolean"))

    if "contact_links" not in document:
        return violations
    contact_links = document.get("contact_links")
    if not isinstance(contact_links, list):
        violations.append((source_name, "contact_links must be an array"))
        return violations

    for index, contact_link in enumerate(contact_links):
        location = f"contact_links[{index}]"
        if not isinstance(contact_link, dict):
            violations.append((source_name, f"{location} must be a mapping"))
            continue
        violations.extend(
            (source_name, reason)
            for reason in _unsupported_key_reasons(
                cast(dict[object, object], contact_link),
                ISSUE_CHOOSER_CONTACT_LINK_KEYS,
                location,
            )
        )
        violations.extend(
            (source_name, f"{location}.{key} must be a nonblank string")
            for key in ("name", "url", "about")
            if not _is_nonblank_string(contact_link.get(key))
        )
    return violations


def _validated_issue_chooser_contact_links(source: Path, repository_root: Path) -> list[dict[str, str]]:
    """Return contact links after rejecting malformed issue chooser configuration."""
    violations = _issue_chooser_config_violations(source, repository_root)
    if violations:
        details = "\n".join(f"  {source_name}: {reason}" for source_name, reason in violations)
        raise AssertionError(f"issue chooser configuration drift:\n{details}")

    document = cast("dict[str, object]", yaml.safe_load(source.read_text(encoding="utf-8")))
    return cast("list[dict[str, str]]", document.get("contact_links", []))


def _form_schema_violations(source: Path, repository_root: Path) -> list[SchemaViolation]:
    """Return violations of GitHub's documented structured-form contract."""
    source_name = _display_path(source, repository_root)
    document, violations = _load_structured_yaml(source, repository_root)
    if not isinstance(document, dict):
        return [*violations, (source_name, "form must be a mapping")]
    document = cast("dict[object, object]", document)

    is_issue_form = source.parent.name == "ISSUE_TEMPLATE"
    allowed_form_keys = ISSUE_FORM_KEYS if is_issue_form else DISCUSSION_FORM_KEYS
    violations.extend((source_name, reason) for reason in _unsupported_key_reasons(document, allowed_form_keys))
    violations.extend(
        [
            (source_name, f"{key} must be a nonblank string")
            for key in ("name", "description")
            if not isinstance(value := document.get(key), str) or not value.strip()
        ]
        if is_issue_form
        else []
    )
    string_keys = ("title", "type") if is_issue_form else ("title",)
    violations.extend(
        (source_name, f"{key} must be a nonblank string")
        for key in string_keys
        if key in document and not _is_nonblank_string(document[key])
    )
    collection_keys = ("labels", "assignees", "projects") if is_issue_form else ("labels",)
    violations.extend(
        (
            source_name,
            f"{key} must be a nonblank comma-delimited string or an array of nonblank strings",
        )
        for key in collection_keys
        if key in document and not _is_nonblank_string_collection(document[key])
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
        allowed_element_keys = (
            FORM_ELEMENT_KEYS[element_type]
            if isinstance(element_type, str) and element_type in FORM_ELEMENT_TYPES
            else FORM_COMMON_ELEMENT_KEYS
        )
        unsupported_element_keys = _unsupported_key_reasons(
            cast(dict[object, object], element),
            allowed_element_keys,
            location,
        )
        if not isinstance(element_type, str) or element_type not in FORM_ELEMENT_TYPES:
            violations.append((source_name, f"{location}.type is unsupported: {element_type!r}"))
            violations.extend((source_name, reason) for reason in unsupported_element_keys)
            continue
        if element_type != "markdown":
            has_input = True

        violations.extend((source_name, reason) for reason in unsupported_element_keys)

        identifier = element.get("id")
        if (
            identifier is not None
            and element_type != "markdown"
            and (not isinstance(identifier, str) or FORM_ID.fullmatch(identifier) is None)
        ):
            violations.append((source_name, f"{location}.id is invalid: {identifier!r}"))
        elif element_type != "markdown" and identifier is not None and identifier in identifiers:
            violations.append((source_name, f"{location}.id is duplicated: {identifier!r}"))
        elif element_type != "markdown" and isinstance(identifier, str):
            identifiers.add(identifier)

        attributes = element.get("attributes")
        if not isinstance(attributes, dict):
            violations.append((source_name, f"{location}.attributes must be a mapping"))
        else:
            violations.extend(
                (source_name, reason)
                for reason in _unsupported_key_reasons(
                    cast(dict[object, object], attributes),
                    FORM_ATTRIBUTE_KEYS[element_type],
                    f"{location}.attributes",
                )
            )
            required_attribute = "value" if element_type == "markdown" else "label"
            if not _is_nonblank_string(attributes.get(required_attribute)):
                violations.append(
                    (source_name, f"{location}.attributes.{required_attribute} must be a nonblank string")
                )

            text_attributes = {
                "checkboxes": ("description",),
                "dropdown": ("description",),
                "input": ("description", "placeholder", "value"),
                "markdown": (),
                "textarea": ("description", "placeholder", "value", "render"),
                "upload": ("description",),
            }[element_type]
            violations.extend(
                (source_name, f"{location}.attributes.{key} must be a string")
                for key in text_attributes
                if key in attributes and not isinstance(attributes.get(key), str)
            )

            options = attributes.get("options")
            if element_type == "dropdown":
                multiple = attributes.get("multiple")
                if "multiple" in attributes and not isinstance(multiple, bool):
                    violations.append((source_name, f"{location}.attributes.multiple must be a Boolean"))

                default = attributes.get("default")
                if "default" in attributes and (isinstance(default, bool) or not isinstance(default, int)):
                    violations.append((source_name, f"{location}.attributes.default must be an integer"))
                elif (
                    isinstance(default, int)
                    and isinstance(options, list)
                    and options
                    and not 0 <= default < len(options)
                ):
                    violations.append((source_name, f"{location}.attributes.default must index an available option"))

                if not isinstance(options, list) or not options:
                    violations.append((source_name, f"{location}.attributes.options must be a nonempty array"))
                else:
                    choices: set[str] = set()
                    for option_index, option in enumerate(options):
                        option_location = f"{location}.attributes.options[{option_index}]"
                        if not isinstance(option, str) or not option.strip():
                            violations.append((source_name, f"{option_location} must be a nonblank string"))
                        elif option in choices:
                            violations.append((source_name, f"{option_location} is duplicated: {option!r}"))
                        else:
                            choices.add(option)
            elif element_type == "checkboxes":
                if not isinstance(options, list) or not options:
                    violations.append((source_name, f"{location}.attributes.options must be a nonempty array"))
                else:
                    choices = set()
                    for option_index, option in enumerate(options):
                        option_location = f"{location}.attributes.options[{option_index}]"
                        label = option.get("label") if isinstance(option, dict) else None
                        if isinstance(option, dict):
                            violations.extend(
                                (source_name, reason)
                                for reason in _unsupported_key_reasons(
                                    cast(dict[object, object], option),
                                    CHECKBOX_OPTION_KEYS,
                                    option_location,
                                )
                            )
                        if (
                            isinstance(option, dict)
                            and "required" in option
                            and not isinstance(option.get("required"), bool)
                        ):
                            violations.append((source_name, f"{option_location}.required must be a Boolean"))
                        if not isinstance(label, str) or not label.strip():
                            violations.append((source_name, f"{option_location}.label must be a nonblank string"))
                        elif label in choices:
                            violations.append((source_name, f"{option_location}.label is duplicated: {label!r}"))
                        else:
                            choices.add(label)

        validations = element.get("validations")
        if "validations" not in FORM_ELEMENT_KEYS[element_type]:
            continue
        if validations is not None and not isinstance(validations, dict):
            violations.append((source_name, f"{location}.validations must be a mapping"))
        elif isinstance(validations, dict):
            violations.extend(
                (source_name, reason)
                for reason in _unsupported_key_reasons(
                    cast(dict[object, object], validations),
                    FORM_VALIDATION_KEYS[element_type],
                    f"{location}.validations",
                )
            )
            required = validations.get("required")
            if "required" in validations and not isinstance(required, bool):
                violations.append((source_name, f"{location}.validations.required must be a Boolean"))
            accept = validations.get("accept")
            if element_type == "upload" and "accept" in validations and not isinstance(accept, str):
                violations.append((source_name, f"{location}.validations.accept must be a string"))

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

    def test_rejects_invalid_issue_chooser_config_structure(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/config.yml"
            source.parent.mkdir(parents=True)

            source.write_text("- not a mapping\n", encoding="utf-8")
            self.assertEqual(
                _issue_chooser_config_violations(source, repository_root),
                [(".github/ISSUE_TEMPLATE/config.yml", "config must be a mapping")],
            )

            source.write_text("contact_links: {}\n", encoding="utf-8")
            self.assertEqual(
                _issue_chooser_config_violations(source, repository_root),
                [(".github/ISSUE_TEMPLATE/config.yml", "contact_links must be an array")],
            )

            source.write_text("contact_links:\n", encoding="utf-8")
            self.assertEqual(
                _issue_chooser_config_violations(source, repository_root),
                [(".github/ISSUE_TEMPLATE/config.yml", "contact_links must be an array")],
            )

    def test_rejects_invalid_issue_chooser_config_fields(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/config.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        'blank_issues_enabled: "false"',
                        "unexpected: true",
                        "false: invalid top-level key",
                        "contact_links:",
                        '  - name: " "',
                        "    url: false",
                        "    description: Misspelled about",
                        "    false: invalid key",
                        "  - not a mapping",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _issue_chooser_config_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/config.yml", "unexpected is not permitted"),
                    (".github/ISSUE_TEMPLATE/config.yml", "False is not permitted"),
                    (".github/ISSUE_TEMPLATE/config.yml", "blank_issues_enabled must be a Boolean"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[0].description is not permitted"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[0].False is not permitted"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[0].name must be a nonblank string"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[0].url must be a nonblank string"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[0].about must be a nonblank string"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[1] must be a mapping"),
                ],
            )

            with self.assertRaisesRegex(
                AssertionError,
                r"issue chooser configuration drift:\n"
                r"  \.github/ISSUE_TEMPLATE/config\.yml: unexpected is not permitted",
            ):
                _validated_issue_chooser_contact_links(source, repository_root)

    def test_rejects_duplicate_issue_chooser_keys(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/config.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "blank_issues_enabled: false",
                        "blank_issues_enabled: true",
                        "contact_links:",
                        "  - name: First",
                        "    name: Second",
                        "    url: https://example.com",
                        "    about: Help",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _issue_chooser_config_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/config.yml", "blank_issues_enabled is duplicated"),
                    (".github/ISSUE_TEMPLATE/config.yml", "contact_links[0].name is duplicated"),
                ],
            )

    def test_compares_duplicate_scalar_keys_by_resolved_tag_and_value(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/config.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "false: first Boolean",
                        "FALSE: second Boolean",
                        '"false": quoted string',
                        "11: decimal integer",
                        "0xB: hexadecimal integer",
                        '"11": quoted integer',
                    )
                ),
                encoding="utf-8",
            )

            document, violations = _load_structured_yaml(source, repository_root)

            self.assertEqual(
                violations,
                [
                    (".github/ISSUE_TEMPLATE/config.yml", "False is duplicated"),
                    (".github/ISSUE_TEMPLATE/config.yml", "11 is duplicated"),
                ],
            )
            self.assertEqual(
                document,
                {
                    False: "second Boolean",
                    "false": "quoted string",
                    11: "hexadecimal integer",
                    "11": "quoted integer",
                },
            )

    def test_embedded_form_markdown_targets_resolve(self) -> None:
        _require_valid_form_markdown_links(_structured_forms(), REPOSITORY_ROOT)

    def test_structured_forms_follow_github_schema(self) -> None:
        _require_valid_form_schemas(_structured_forms(), REPOSITORY_ROOT)

    def test_rejects_duplicate_issue_form_keys_at_exact_paths(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: First",
                        "name: Second",
                        "description: Exercises duplicate keys",
                        "body:",
                        "  - type: input",
                        "    type: textarea",
                        "    attributes:",
                        "      label: First",
                        "      label: Second",
                        "    validations:",
                        "      required: false",
                        "      required: true",
                        "  - type: checkboxes",
                        "    attributes:",
                        "      label: Choices",
                        "      options:",
                        "        - label: First",
                        "          label: Second",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/broken.yml", "name is duplicated"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].type is duplicated"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].attributes.label is duplicated"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].validations.required is duplicated"),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[1].attributes.options[0].label is duplicated",
                    ),
                ],
            )

    def test_rejects_duplicate_discussion_form_keys(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/DISCUSSION_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        'title: "[First] "',
                        'title: "[Second] "',
                        "body:",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Proposal",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [(".github/DISCUSSION_TEMPLATE/broken.yml", "title is duplicated")],
            )

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
                        "    attributes:",
                        "      label: Bad identifier",
                        "  - type: textarea",
                        "    id: repeated",
                        "    attributes:",
                        "      label: First identifier",
                        "  - type: dropdown",
                        "    id: repeated",
                        "    attributes:",
                        "      label: Repeated identifier",
                        "      options:",
                        "        - Choice",
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

    def test_rejects_invalid_issue_form_metadata(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Exercises invalid issue metadata",
                        "title: true",
                        'type: ""',
                        "labels: {}",
                        "assignees:",
                        "  - octocat",
                        '  - " "',
                        'projects: "octo-org/1, "',
                        "body:",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Details",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/broken.yml", "title must be a nonblank string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "type must be a nonblank string"),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "labels must be a nonblank comma-delimited string or an array of nonblank strings",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "assignees must be a nonblank comma-delimited string or an array of nonblank strings",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "projects must be a nonblank comma-delimited string or an array of nonblank strings",
                    ),
                ],
            )

    def test_accepts_empty_form_metadata_arrays(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/empty.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Empty metadata",
                        "description: Exercises documented empty arrays",
                        "labels: []",
                        "assignees: []",
                        "projects: []",
                        "body:",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Details",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(_form_schema_violations(source, repository_root), [])

    def test_rejects_invalid_discussion_form_metadata(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/DISCUSSION_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "title: false",
                        "labels:",
                        "  - Ideas",
                        "  - false",
                        "body:",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Proposal",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/DISCUSSION_TEMPLATE/broken.yml", "title must be a nonblank string"),
                    (
                        ".github/DISCUSSION_TEMPLATE/broken.yml",
                        "labels must be a nonblank comma-delimited string or an array of nonblank strings",
                    ),
                ],
            )

    def test_rejects_invalid_form_attributes_and_choices(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Exercises invalid element attributes",
                        "body:",
                        "  - type: markdown",
                        "    attributes: {}",
                        "  - type: input",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: true",
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Empty choices",
                        "      options: []",
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Invalid choices",
                        "      options:",
                        "        - Choice",
                        "        - Choice",
                        "        - true",
                        "  - type: checkboxes",
                        "    attributes:",
                        "      label: Invalid checks",
                        "      options:",
                        '        - label: ""',
                        "        - label: Repeated",
                        "        - label: Repeated",
                        "  - type: input",
                        "    attributes:",
                        "      label: Invalid validation",
                        "    validations:",
                        '      required: "true"',
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[0].attributes.value must be a nonblank string",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[1].attributes must be a mapping",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[2].attributes.label must be a nonblank string",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[3].attributes.options must be a nonempty array",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[4].attributes.options[1] is duplicated: 'Choice'",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[4].attributes.options[2] must be a nonblank string",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[5].attributes.options[0].label must be a nonblank string",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[5].attributes.options[2].label is duplicated: 'Repeated'",
                    ),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[6].validations.required must be a Boolean",
                    ),
                ],
            )

    def test_rejects_invalid_optional_issue_form_fields(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Exercises invalid optional element fields",
                        "body:",
                        "  - type: input",
                        "    attributes:",
                        "      label: Contact",
                        "      description: true",
                        "      placeholder: []",
                        "      value: {}",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Logs",
                        "      render: false",
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Version",
                        "      description: 1",
                        '      multiple: "false"',
                        "      default: true",
                        "      options:",
                        "        - Stable",
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Channel",
                        "      default: 1",
                        "      options:",
                        "        - Stable",
                        "  - type: checkboxes",
                        "    attributes:",
                        "      label: Agreement",
                        "      description: false",
                        "      options:",
                        "        - label: I agree",
                        "  - type: upload",
                        "    attributes:",
                        "      label: Evidence",
                        "      description: []",
                        "    validations:",
                        "      accept:",
                        "        - .png",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].attributes.description must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].attributes.placeholder must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].attributes.value must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[1].attributes.render must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[2].attributes.description must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[2].attributes.multiple must be a Boolean"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[2].attributes.default must be an integer"),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[3].attributes.default must index an available option",
                    ),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[4].attributes.description must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[5].attributes.description must be a string"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[5].validations.accept must be a string"),
                ],
            )

    def test_rejects_invalid_optional_discussion_form_fields(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/DISCUSSION_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "body:",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Proposal",
                        "      placeholder: true",
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Area",
                        "      multiple: 1",
                        "      options:",
                        "        - Documentation",
                        "  - type: upload",
                        "    attributes:",
                        "      label: Evidence",
                        "    validations:",
                        "      accept: false",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (
                        ".github/DISCUSSION_TEMPLATE/broken.yml",
                        "body[0].attributes.placeholder must be a string",
                    ),
                    (
                        ".github/DISCUSSION_TEMPLATE/broken.yml",
                        "body[1].attributes.multiple must be a Boolean",
                    ),
                    (
                        ".github/DISCUSSION_TEMPLATE/broken.yml",
                        "body[2].validations.accept must be a string",
                    ),
                ],
            )

    def test_accepts_valid_optional_form_fields(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/valid.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Valid",
                        "description: Exercises valid optional element fields",
                        "body:",
                        "  - type: input",
                        "    attributes:",
                        "      label: Contact",
                        '      description: ""',
                        '      placeholder: ""',
                        '      value: ""',
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Logs",
                        '      render: ""',
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Version",
                        "      multiple: false",
                        "      default: 0",
                        "      options:",
                        "        - Stable",
                        "  - type: checkboxes",
                        "    attributes:",
                        "      label: Agreement",
                        '      description: ""',
                        "      options:",
                        "        - label: I agree",
                        "  - type: upload",
                        "    attributes:",
                        "      label: Evidence",
                        '      description: ""',
                        "    validations:",
                        '      accept: ""',
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(_form_schema_violations(source, repository_root), [])

    def test_rejects_unsupported_issue_form_keys(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Exercises unsupported issue form keys",
                        "unexpected: true",
                        "body:",
                        "  - type: input",
                        "    validation: {}",
                        "    attributes:",
                        "      label: Contact",
                        "      placehoder: Email",
                        "    validations:",
                        "      optional: true",
                        "  - type: checkboxes",
                        "    attributes:",
                        "      label: Agreement",
                        "      options:",
                        "        - label: I agree",
                        "          checked: true",
                        "  - type: dropdown",
                        "    attributes:",
                        "      label: Version",
                        "      placeholder: Stable",
                        "      options:",
                        "        - Stable",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/broken.yml", "unexpected is not permitted"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].validation is not permitted"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].attributes.placehoder is not permitted"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].validations.optional is not permitted"),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[1].attributes.options[0].checked is not permitted",
                    ),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[2].attributes.placeholder is not permitted"),
                ],
            )

    def test_rejects_unsupported_discussion_form_keys(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/DISCUSSION_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Discussion",
                        "body:",
                        "  - type: markdown",
                        "    id: context",
                        "    attributes:",
                        "      value: Context",
                        "    validations: {}",
                        "  - type: textarea",
                        "    attributes:",
                        "      label: Proposal",
                        "    validations:",
                        "      accept: .txt",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/DISCUSSION_TEMPLATE/broken.yml", "name is not permitted"),
                    (".github/DISCUSSION_TEMPLATE/broken.yml", "body[0].id is not permitted"),
                    (".github/DISCUSSION_TEMPLATE/broken.yml", "body[0].validations is not permitted"),
                    (".github/DISCUSSION_TEMPLATE/broken.yml", "body[1].validations.accept is not permitted"),
                ],
            )

    def test_rejects_unsupported_keys_with_invalid_element_types(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Exercises invalid element types with unsupported keys",
                        "body:",
                        "  - type: button",
                        "    placehoder: missed",
                        "    false: missed-too",
                        "    attributes:",
                        "      label: Action",
                        "  - validation: {}",
                        "    attributes:",
                        "      label: Missing type",
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].type is unsupported: 'button'"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].placehoder is not permitted"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[0].False is not permitted"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[1].type is unsupported: None"),
                    (".github/ISSUE_TEMPLATE/broken.yml", "body[1].validation is not permitted"),
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body must contain at least one non-Markdown field",
                    ),
                ],
            )

    def test_rejects_non_boolean_checkbox_option_requirement(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            source = repository_root / ".github/ISSUE_TEMPLATE/broken.yml"
            source.parent.mkdir(parents=True)
            source.write_text(
                "\n".join(
                    (
                        "name: Broken",
                        "description: Exercises an invalid checkbox option requirement",
                        "body:",
                        "  - type: checkboxes",
                        "    attributes:",
                        "      label: Public evidence",
                        "      options:",
                        "        - label: I removed private content.",
                        '          required: "true"',
                    )
                ),
                encoding="utf-8",
            )

            self.assertEqual(
                _form_schema_violations(source, repository_root),
                [
                    (
                        ".github/ISSUE_TEMPLATE/broken.yml",
                        "body[0].attributes.options[0].required must be a Boolean",
                    )
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
        contact_links = _validated_issue_chooser_contact_links(ISSUE_TEMPLATE_CONFIG, REPOSITORY_ROOT)
        config_targets = [link["url"] for link in contact_links]
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
