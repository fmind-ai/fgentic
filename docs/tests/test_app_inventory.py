"""Keep public repository maps synchronized with self-contained apps."""

import re
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import TestCase

REPOSITORY_ROOT = Path(__file__).resolve().parents[2]
PUBLIC_APP_MAPS = (
    "README.md",
    ".agents/AGENTS.md",
    "docs/agent-reference.md",
)

type AppInventoryViolation = tuple[str, str]


def _self_contained_apps(repository_root: Path) -> tuple[str, ...]:
    """Return app directories that own a checked-in mise config."""
    return tuple(sorted(config.parent.name for config in (repository_root / "apps").glob("*/mise.toml")))


def _mentions_app(markdown: str, app: str) -> bool:
    """Return whether Markdown names one exact app identifier."""
    identifier = re.compile(rf"(?<![A-Za-z0-9_-]){re.escape(app)}(?![A-Za-z0-9_-])")
    return identifier.search(markdown) is not None


def _app_inventory_violations(
    repository_root: Path,
    sources: tuple[str, ...] = PUBLIC_APP_MAPS,
) -> list[AppInventoryViolation]:
    """Return each public map and checked-in app it omits."""
    apps = _self_contained_apps(repository_root)
    violations: list[AppInventoryViolation] = []
    for source in sources:
        markdown = (repository_root / source).read_text(encoding="utf-8")
        violations.extend((source, app) for app in apps if not _mentions_app(markdown, app))
    return violations


def _require_complete_app_inventories(
    repository_root: Path,
    sources: tuple[str, ...] = PUBLIC_APP_MAPS,
) -> None:
    """Reject public repository maps that omit a self-contained app."""
    violations = _app_inventory_violations(repository_root, sources)
    if not violations:
        return

    details = "\n".join(f"  {source}: missing {app}" for source, app in violations)
    raise AssertionError(f"public app inventory drift:\n{details}")


class PublicAppInventoryTest(TestCase):
    """Reject public repository maps that drift from checked-in apps."""

    def test_current_public_maps_name_every_self_contained_app(self) -> None:
        _require_complete_app_inventories(REPOSITORY_ROOT)

    def test_reports_map_and_missing_app(self) -> None:
        with TemporaryDirectory() as temporary:
            repository_root = Path(temporary)
            for app in ("app-one", "app-two"):
                app_root = repository_root / f"apps/{app}"
                app_root.mkdir(parents=True)
                (app_root / "mise.toml").touch()

            readme = repository_root / "README.md"
            readme.write_text("Apps: app-one.\n", encoding="utf-8")

            message = r"public app inventory drift:\n  README\.md: missing app-two"
            with self.assertRaisesRegex(AssertionError, message):
                _require_complete_app_inventories(repository_root, ("README.md",))
