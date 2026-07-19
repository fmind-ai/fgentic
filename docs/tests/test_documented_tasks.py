"""Keep public root mise commands synchronized with the task table."""

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

type TaskViolation = tuple[str, str]


def _task_vocabulary(mise_config: Path) -> set[str]:
    """Return every root task name and alias without invoking mise."""
    configuration = tomllib.loads(mise_config.read_text(encoding="utf-8"))
    tasks = configuration.get("tasks")
    if not isinstance(tasks, Mapping):
        msg = f"{mise_config}: missing TOML task table"
        raise TypeError(msg)

    vocabulary: set[str] = set()
    for name, definition in tasks.items():
        if not isinstance(name, str):
            msg = f"{mise_config}: task names must be strings"
            raise TypeError(msg)
        vocabulary.add(name)
        if not isinstance(definition, Mapping) or "alias" not in definition:
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
        vocabulary.update(aliases)
    return vocabulary


def _documented_tasks(markdown: str) -> set[str]:
    """Return root task names referenced in prose or code examples."""
    return {match.group("task") for match in _MISE_RUN.finditer(markdown)}


def _public_task_sources(repository_root: Path = REPOSITORY_ROOT) -> tuple[Path, ...]:
    """Return human and agent docs that publish root mise commands."""
    entrypoints = (repository_root / relative for relative in PUBLIC_ENTRYPOINTS)
    agent_skills = sorted((repository_root / ".agents/skills").rglob("SKILL.md"))
    return (*entrypoints, *agent_skills)


def _display_path(path: Path, repository_root: Path) -> str:
    """Return a stable repository-relative diagnostic path when possible."""
    try:
        return path.relative_to(repository_root).as_posix()
    except ValueError:
        return path.as_posix()


def _task_violations(
    sources: tuple[Path, ...],
    mise_config: Path,
    repository_root: Path,
) -> list[TaskViolation]:
    """Return missing sources and undocumented root task references."""
    vocabulary = _task_vocabulary(mise_config)
    violations: list[TaskViolation] = []
    for source in sources:
        source_name = _display_path(source, repository_root)
        if not source.is_file():
            violations.append((source_name, "(source)"))
            continue
        missing = sorted(_documented_tasks(source.read_text(encoding="utf-8")) - vocabulary)
        violations.extend((source_name, task) for task in missing)
    return violations


def _require_valid_task_references(
    sources: tuple[Path, ...],
    mise_config: Path,
    repository_root: Path,
) -> None:
    """Reject public root mise commands absent from the task table."""
    violations = _task_violations(sources, mise_config, repository_root)
    if not violations:
        return

    details = "\n".join(f"  {source}: mise run {task}" for source, task in violations)
    raise AssertionError(f"documented mise task drift:\n{details}")


class DocumentedTaskIntegrityTest(TestCase):
    """Reject stale root mise commands in human and agent entrypoints."""

    def test_current_public_entrypoint_tasks_exist(self) -> None:
        _require_valid_task_references(_public_task_sources(), MISE_CONFIG, REPOSITORY_ROOT)

    def test_accepts_task_names_and_aliases_in_prose_and_code(self) -> None:
        markdown = """
Run `mise run check` before opening a PR.
The deploy task also accepts `mise run ship`.

```console
mise run t
```

Unrelated text such as `mise tasks` is not a task invocation.
App-local commands such as `mise --cd apps/example run app-only` are not root task invocations.
"""
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            mise_config = repository_root / "mise.toml"
            mise_config.write_text(
                '[tasks.check]\nrun = "true"\n\n'
                '[tasks.test]\nalias = "t"\nrun = "true"\n\n'
                '[tasks.deploy]\nalias = ["d", "ship"]\nrun = "true"\n',
                encoding="utf-8",
            )
            source = repository_root / "README.md"
            source.write_text(markdown, encoding="utf-8")

            self.assertEqual(_task_violations((source,), mise_config, repository_root), [])

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
