"""Keep public root and app-local mise commands synchronized with task tables."""

import re
import tomllib
from collections.abc import Mapping
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import TestCase

REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
MISE_CONFIG = REPOSITORY_ROOT / "mise.toml"
PUBLIC_ENTRYPOINTS = (
    ".agents/AGENTS.md",
    "CONTRIBUTING.md",
    "README.md",
)
_MISE_RUN = re.compile(r"\bmise[ \t]+run[ \t]+(?P<task>[A-Za-z0-9][A-Za-z0-9:_-]*)")
_MISE_APP_RUN = re.compile(
    r"\bmise[ \t]+--cd[ \t]+(?P<directory>[A-Za-z0-9_./-]+)"
    r"[ \t]+run[ \t]+(?P<task>[A-Za-z0-9][A-Za-z0-9:_-]*)"
)

type TaskViolation = tuple[str, str]


def _task_definitions(mise_config: Path) -> dict[str, set[str]]:
    """Return task names and aliases from one config without invoking mise."""
    configuration = tomllib.loads(mise_config.read_text(encoding="utf-8"))
    tasks = configuration.get("tasks")
    if not isinstance(tasks, Mapping):
        msg = f"{mise_config}: missing TOML task table"
        raise TypeError(msg)

    definitions: dict[str, set[str]] = {}
    for name, definition in tasks.items():
        if not isinstance(name, str):
            msg = f"{mise_config}: task names must be strings"
            raise TypeError(msg)
        if not isinstance(definition, Mapping) or "alias" not in definition:
            definitions[name] = set()
            continue
        alias = definition["alias"]
        if isinstance(alias, str):
            aliases = (alias,)
        elif isinstance(alias, list):
            aliases = tuple(value for value in alias if isinstance(value, str))
            if len(aliases) != len(alias):
                aliases = ()
        else:
            aliases = ()
        if not aliases or any(not value.strip() for value in aliases):
            msg = f"{mise_config}: task {name!r} aliases must be a nonblank string or list of nonblank strings"
            raise TypeError(msg)
        definitions[name] = set(aliases)
    return definitions


def _task_vocabulary(mise_config: Path) -> set[str]:
    """Return every task name and alias from one config."""
    definitions = _task_definitions(mise_config)
    return set(definitions).union(*(aliases for aliases in definitions.values()))


def _documented_tasks(markdown: str) -> set[str]:
    """Return root task names referenced in prose or code examples."""
    return {match.group("task") for match in _MISE_RUN.finditer(markdown)}


def _documented_app_tasks(markdown: str) -> set[tuple[str, str]]:
    """Return explicit repository-directory and task pairs."""
    return {(match.group("directory"), match.group("task")) for match in _MISE_APP_RUN.finditer(markdown)}


def _visible_documentation(repository_root: Path) -> tuple[Path, ...]:
    """Return authored docs pages while excluding hidden tool state."""
    docs_root = repository_root / "docs"
    return tuple(
        sorted(
            path
            for path in docs_root.rglob("*.md")
            if path.is_file() and not any(part.startswith(".") for part in path.relative_to(docs_root).parts)
        )
    )


def _public_task_sources(repository_root: Path = REPOSITORY_ROOT) -> tuple[Path, ...]:
    """Return every public human, agent, and documentation task source."""
    entrypoints = (repository_root / relative for relative in PUBLIC_ENTRYPOINTS)
    agent_skills = sorted((repository_root / ".agents/skills").rglob("SKILL.md"))
    return (*entrypoints, *agent_skills, *_visible_documentation(repository_root))


def _display_path(path: Path, repository_root: Path) -> str:
    """Return a stable repository-relative diagnostic path when possible."""
    try:
        return path.relative_to(repository_root).as_posix()
    except ValueError:
        return path.as_posix()


def _effective_task_vocabulary(
    directory: str,
    mise_config: Path,
    repository_root: Path,
) -> tuple[set[str] | None, str | None]:
    """Return the checked-in root-to-directory task vocabulary."""
    relative = Path(directory)
    if relative.is_absolute():
        return None, "working directory must be repository-relative"
    if ".." in relative.parts:
        return None, "working directory must not traverse parents"

    repository = repository_root.resolve()
    working_directory = (repository / relative).resolve()
    if not working_directory.is_relative_to(repository):
        return None, "working directory escapes the repository"
    if not working_directory.is_dir():
        return None, "working directory does not exist"

    definitions = _task_definitions(mise_config)
    current = repository
    for part in working_directory.relative_to(repository).parts:
        current /= part
        nested_config = current / "mise.toml"
        if nested_config.is_file():
            definitions.update(_task_definitions(nested_config))
    vocabulary = set(definitions).union(*(aliases for aliases in definitions.values()))
    return vocabulary, None


def _task_violations(
    sources: tuple[Path, ...],
    mise_config: Path,
    repository_root: Path,
) -> list[TaskViolation]:
    """Return missing sources and unresolved root or app-local tasks."""
    root_vocabulary = _task_vocabulary(mise_config)
    violations: list[TaskViolation] = []
    for source in sources:
        source_name = _display_path(source, repository_root)
        if not source.is_file():
            violations.append((source_name, "(source)"))
            continue
        markdown = source.read_text(encoding="utf-8")
        missing = sorted(_documented_tasks(markdown) - root_vocabulary)
        violations.extend((source_name, f"mise run {task}") for task in missing)
        for directory, task in sorted(_documented_app_tasks(markdown)):
            command = f"mise --cd {directory} run {task}"
            vocabulary, reason = _effective_task_vocabulary(directory, mise_config, repository_root)
            if reason is not None:
                violations.append((source_name, f"{command} ({reason})"))
            elif vocabulary is not None and task not in vocabulary:
                violations.append((source_name, command))
    return violations


def _require_valid_task_references(
    sources: tuple[Path, ...],
    mise_config: Path,
    repository_root: Path,
) -> None:
    """Reject public mise commands absent from their effective task tables."""
    violations = _task_violations(sources, mise_config, repository_root)
    if not violations:
        return

    details = "\n".join(f"  {source}: {command}" for source, command in violations)
    raise AssertionError(f"documented mise task drift:\n{details}")


class DocumentedTaskIntegrityTest(TestCase):
    """Reject stale mise commands across public human and agent documentation."""

    def test_current_public_markdown_tasks_exist(self) -> None:
        _require_valid_task_references(_public_task_sources(), MISE_CONFIG, REPOSITORY_ROOT)

    def test_accepts_task_names_and_aliases_in_prose_and_code(self) -> None:
        markdown = """
Run `mise run check` before opening a PR.
The deploy task also accepts `mise run ship`.

```console
mise run t
```

Unrelated text such as `mise tasks` is not a task invocation.
App-local commands inherit root tasks (`mise --cd apps/example run check`) and use their local config
(`mise --cd apps/example run ship-app`).
"""
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text(
                '[tasks.check]\nrun = "true"\n\n'
                '[tasks.test]\nalias = "t"\nrun = "true"\n\n'
                '[tasks.deploy]\nalias = ["d", "ship"]\nrun = "true"\n\n'
                '[tasks.replaced]\nalias = "old-alias"\nrun = "true"\n',
                encoding="utf-8",
            )
            app = repository_root / "apps/example"
            app.mkdir(parents=True)
            (app / "mise.toml").write_text(
                '[tasks.app-only]\nalias = "ship-app"\nrun = "true"\n\n[tasks.replaced]\nrun = "true"\n',
                encoding="utf-8",
            )
            source = repository_root / "README.md"
            source.write_text(markdown, encoding="utf-8")

            self.assertEqual(_task_violations((source,), mise_config, repository_root), [])

    def test_app_override_replaces_parent_aliases(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text(
                '[tasks.replaced]\nalias = "old-alias"\nrun = "true"\n',
                encoding="utf-8",
            )
            app = repository_root / "apps/example"
            app.mkdir(parents=True)
            (app / "mise.toml").write_text('[tasks.replaced]\nrun = "true"\n', encoding="utf-8")
            source = repository_root / "README.md"
            source.write_text("Run `mise --cd apps/example run old-alias`.\n", encoding="utf-8")

            self.assertEqual(
                _task_violations((source,), mise_config, repository_root),
                [("README.md", "mise --cd apps/example run old-alias")],
            )

    def test_discovers_agent_skills_and_reports_stale_skill_tasks(self) -> None:
        message = r"documented mise task drift:\n  \.agents/skills/example/SKILL\.md: mise run missing"
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text('[tasks.check]\nrun = "true"\n', encoding="utf-8")
            skill = repository_root / ".agents/skills/example/SKILL.md"
            skill.parent.mkdir(parents=True)
            skill.write_text("Run `mise run missing`.\n", encoding="utf-8")

            self.assertIn(skill, _public_task_sources(repository_root))
            with self.assertRaisesRegex(AssertionError, message):
                _require_valid_task_references((skill,), mise_config, repository_root)

    def test_rejects_missing_task_with_source_and_command(self) -> None:
        message = r"documented mise task drift:\n  README\.md: mise run missing"
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text('[tasks.check]\nrun = "true"\n', encoding="utf-8")
            source = repository_root / "README.md"
            source.write_text("Run `mise run missing`.\n", encoding="utf-8")

            with self.assertRaisesRegex(AssertionError, message):
                _require_valid_task_references((source,), mise_config, repository_root)

    def test_discovers_visible_docs_and_reports_stale_tasks(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text('[tasks.check]\nrun = "true"\n', encoding="utf-8")
            guide = repository_root / "docs/onboarding/guide.md"
            guide.parent.mkdir(parents=True)
            guide.write_text("Run `mise run missing`.\n", encoding="utf-8")
            hidden = repository_root / "docs/.venv/share/ignored.md"
            hidden.parent.mkdir(parents=True)
            hidden.write_text("Run `mise run also-missing`.\n", encoding="utf-8")

            sources = _public_task_sources(repository_root)
            self.assertIn(guide, sources)
            self.assertNotIn(hidden, sources)
            self.assertIn(
                ("docs/onboarding/guide.md", "mise run missing"),
                _task_violations(sources, mise_config, repository_root),
            )

    def test_rejects_missing_app_task_with_source_and_command(self) -> None:
        message = r"documented mise task drift:\n  README\.md: mise --cd apps/example run missing"
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text('[tasks.check]\nrun = "true"\n', encoding="utf-8")
            app = repository_root / "apps/example"
            app.mkdir(parents=True)
            (app / "mise.toml").write_text('[tasks.test]\nrun = "true"\n', encoding="utf-8")
            source = repository_root / "README.md"
            source.write_text("Run `mise --cd apps/example run missing`.\n", encoding="utf-8")

            with self.assertRaisesRegex(AssertionError, message):
                _require_valid_task_references((source,), mise_config, repository_root)

    def test_rejects_invalid_app_working_directories(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text('[tasks.test]\nrun = "true"\n', encoding="utf-8")
            source = repository_root / "README.md"
            source.write_text(
                "Run `mise --cd apps/missing run test`, `mise --cd ../outside run test`, "
                "or `mise --cd /tmp run test`.\n",
                encoding="utf-8",
            )

            self.assertEqual(
                _task_violations((source,), mise_config, repository_root),
                [
                    (
                        "README.md",
                        "mise --cd ../outside run test (working directory must not traverse parents)",
                    ),
                    (
                        "README.md",
                        "mise --cd /tmp run test (working directory must be repository-relative)",
                    ),
                    (
                        "README.md",
                        "mise --cd apps/missing run test (working directory does not exist)",
                    ),
                ],
            )
