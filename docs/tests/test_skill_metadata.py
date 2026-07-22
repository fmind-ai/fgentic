"""Offline Agent Skills metadata gate (issue #883).

Every `.agents/skills/**/SKILL.md` is validated against the official `skills-ref` reference validator, so
a malformed `name`/`description` or a name that no longer matches its parent directory fails discovery
here instead of silently breaking an agent at runtime. Diagnostics are deterministic and
repository-relative, and every invalid skill is reported rather than stopping at the first failure.
"""

from __future__ import annotations

from pathlib import Path
from tempfile import TemporaryDirectory
from textwrap import dedent
from unittest import TestCase

from skills_ref import validate

REPOSITORY_ROOT = Path(__file__).resolve().parents[2]


def _skill_directories(root: Path) -> tuple[Path, ...]:
    """Every directory under `.agents/skills` that contains a SKILL.md, recursively and sorted."""
    return tuple(sorted(skill.parent for skill in (root / ".agents" / "skills").rglob("SKILL.md")))


def _invalid_skills(root: Path) -> list[tuple[str, list[str]]]:
    """Repository-relative `(path, sorted errors)` for every skill the reference validator rejects."""
    invalid: list[tuple[str, list[str]]] = []
    for directory in _skill_directories(root):
        errors = validate(directory)
        if errors:
            invalid.append((directory.relative_to(root).as_posix(), sorted(errors)))
    return invalid


def _write_skill(directory: Path, frontmatter: str) -> None:
    directory.mkdir(parents=True, exist_ok=True)
    (directory / "SKILL.md").write_text(dedent(frontmatter), encoding="utf-8")


class SkillMetadataTest(TestCase):
    def test_every_project_skill_passes_the_reference_validator(self) -> None:
        self.assertEqual(_invalid_skills(REPOSITORY_ROOT), [])

    def test_at_least_the_known_project_skills_are_discovered(self) -> None:
        discovered = {directory.name for directory in _skill_directories(REPOSITORY_ROOT)}
        self.assertLessEqual(
            {"bridge-dev", "docs-spec", "flux-gitops", "github-flow", "matrix-agents", "sops-secrets"},
            discovered,
        )

    def test_recursive_discovery_finds_a_nested_skill(self) -> None:
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            nested = root / ".agents" / "skills" / "group" / "nested"
            _write_skill(nested, "---\nname: nested\ndescription: A nested skill.\n---\n# Nested\n")
            self.assertIn(nested, _skill_directories(root))

    def test_missing_required_metadata_fails_with_the_repository_path(self) -> None:
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_skill(root / ".agents" / "skills" / "broken", "---\nname: broken\n---\n# Broken\n")
            invalid = _invalid_skills(root)
            self.assertEqual([path for path, _ in invalid], [".agents/skills/broken"])
            self.assertTrue(any("description" in error for error in invalid[0][1]))

    def test_name_directory_mismatch_fails(self) -> None:
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_skill(
                root / ".agents" / "skills" / "broken",
                "---\nname: other-name\ndescription: A valid description.\n---\n# Broken\n",
            )
            invalid = _invalid_skills(root)
            self.assertEqual([path for path, _ in invalid], [".agents/skills/broken"])
            self.assertTrue(any("match" in error for error in invalid[0][1]))

    def test_multiple_invalid_skills_are_all_reported(self) -> None:
        with TemporaryDirectory() as temporary:
            root = Path(temporary)
            _write_skill(root / ".agents" / "skills" / "a-broken", "---\nname: a-broken\n---\n# A\n")
            _write_skill(root / ".agents" / "skills" / "b-broken", "---\nname: b-broken\n---\n# B\n")
            self.assertEqual(
                [path for path, _ in _invalid_skills(root)], [".agents/skills/a-broken", ".agents/skills/b-broken"]
            )
