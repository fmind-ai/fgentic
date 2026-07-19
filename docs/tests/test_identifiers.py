"""Protect stable specification, decision, and ADR identifiers."""

import re
from collections import Counter
from collections.abc import Iterator, Mapping
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest import TestCase

DOCS_ROOT = Path(__file__).resolve().parents[1]
DECISION_MAXIMUM = 20
ADR_MAXIMUM = 20
SECTION_OWNERS = {
    "architecture.md": (1, 2, 3, 11),
    "design-decisions.md": (4,),
    "bridge.md": (5, 6, 12),
    "security.md": (7,),
    "federation.md": (8,),
    "observability.md": (9,),
    "licensing.md": (10,),
    "roadmap.md": (13,),
}
_SECTION_REFERENCE = re.compile(r"§(?P<start>[1-9][0-9]*)(?:[\u2013-]§?(?P<end>[1-9][0-9]*))?")
_DECISION_RANGE = re.compile(r"\bD1[\u2013-]D(?P<maximum>[1-9][0-9]*)\b")
_DECISION_ENTRY = re.compile(r"^### D(?P<identifier>[1-9][0-9]*) — ")
_ADR_FILE = re.compile(r"^(?P<identifier>[0-9]{4})-[a-z0-9][a-z0-9-]*\.md$")
_ADR_HEADING = re.compile(r"^# (?P<identifier>[0-9]{4}) — ")
_FENCE = re.compile(r"^ {0,3}(?P<marker>`{3,}|~{3,})")


def _markdown_lines(markdown: str) -> Iterator[str]:
    """Yield source lines outside fenced code blocks."""
    fence_character: str | None = None
    fence_length = 0
    for line in markdown.splitlines():
        fence = _FENCE.match(line)
        if fence is not None:
            marker = fence.group("marker")
            if fence_character is None:
                tail = line[fence.end() :]
                if marker[0] != "`" or "`" not in tail:
                    fence_character = marker[0]
                    fence_length = len(marker)
                    continue
            if marker[0] == fence_character and len(marker) >= fence_length and not line[fence.end() :].strip():
                fence_character = None
                fence_length = 0
                continue
        if fence_character is None:
            yield line


def _first_h1(markdown: str) -> str | None:
    """Return the first level-one heading."""
    return next((line for line in _markdown_lines(markdown) if line.startswith("# ")), None)


def _section_identifiers(heading: str) -> list[int]:
    """Expand section references and inclusive ranges from one topic heading."""
    identifiers: list[int] = []
    for reference in _SECTION_REFERENCE.finditer(heading):
        start = int(reference.group("start"))
        end_value = reference.group("end")
        if end_value is None:
            identifiers.append(start)
        else:
            identifiers.extend(range(start, int(end_value) + 1))
    return identifiers


def _section_errors(headings: Mapping[str, str | None]) -> tuple[str, ...]:
    """Return topic ownership and global section inventory errors."""
    errors: list[str] = []
    all_identifiers: list[int] = []
    for path, expected_values in SECTION_OWNERS.items():
        heading = headings.get(path)
        if heading is None:
            errors.append(f"{path}: missing topic-spec H1")
            continue
        actual = _section_identifiers(heading)
        all_identifiers.extend(actual)
        counts = Counter(actual)
        expected = set(expected_values)
        missing = sorted(expected - counts.keys())
        unexpected = sorted(counts.keys() - expected)
        duplicates = sorted(identifier for identifier, count in counts.items() if count > 1)
        if missing:
            errors.append(f"{path}: missing SPEC sections {', '.join(f'§{value}' for value in missing)}")
        if unexpected:
            errors.append(f"{path}: unexpected SPEC sections {', '.join(f'§{value}' for value in unexpected)}")
        if duplicates:
            errors.append(f"{path}: duplicate SPEC sections {', '.join(f'§{value}' for value in duplicates)}")

    global_counts = Counter(all_identifiers)
    expected_all = set(range(1, 14))
    missing_all = sorted(expected_all - global_counts.keys())
    unexpected_all = sorted(global_counts.keys() - expected_all)
    duplicate_all = sorted(identifier for identifier, count in global_counts.items() if count > 1)
    if missing_all:
        errors.append(f"SPEC mapping: missing sections {', '.join(f'§{value}' for value in missing_all)}")
    if unexpected_all:
        errors.append(f"SPEC mapping: unexpected sections {', '.join(f'§{value}' for value in unexpected_all)}")
    if duplicate_all:
        errors.append(f"SPEC mapping: duplicate sections {', '.join(f'§{value}' for value in duplicate_all)}")
    return tuple(errors)


def _decision_errors(
    markdown: str,
    path: str = "design-decisions.md",
    stable_maximum: int = DECISION_MAXIMUM,
) -> tuple[str, ...]:
    """Return advertised-range, gap, duplicate, and overflow errors."""
    heading = _first_h1(markdown)
    advertised = _DECISION_RANGE.search(heading or "")
    if advertised is None:
        return (f"{path}: H1 must advertise the contiguous D1-D<n> range",)

    maximum = int(advertised.group("maximum"))
    identifiers = [
        int(match.group("identifier"))
        for line in _markdown_lines(markdown)
        if (match := _DECISION_ENTRY.match(line)) is not None
    ]
    counts = Counter(identifiers)
    expected = set(range(1, maximum + 1))
    missing = sorted(expected - counts.keys())
    unexpected = sorted(counts.keys() - expected)
    duplicates = sorted(identifier for identifier, count in counts.items() if count > 1)
    errors: list[str] = []
    if maximum != stable_maximum:
        errors.append(f"{path}: advertised maximum D{maximum} must remain D{stable_maximum}")
    if missing:
        errors.append(f"{path}: missing decisions {', '.join(f'D{value}' for value in missing)}")
    if unexpected:
        errors.append(f"{path}: decisions exceed advertised range {', '.join(f'D{value}' for value in unexpected)}")
    if duplicates:
        errors.append(f"{path}: duplicate decisions {', '.join(f'D{value}' for value in duplicates)}")
    return tuple(errors)


def _adr_errors(documents: dict[str, str], stable_maximum: int = ADR_MAXIMUM) -> tuple[str, ...]:
    """Return ADR filename continuity and heading mismatch errors."""
    identifiers: list[int] = []
    errors: list[str] = []
    for filename, markdown in sorted(documents.items()):
        file_match = _ADR_FILE.fullmatch(filename)
        if file_match is None:
            errors.append(f"{filename}: invalid numbered ADR filename")
            continue
        file_identifier = int(file_match.group("identifier"))
        identifiers.append(file_identifier)
        heading_match = _ADR_HEADING.match(_first_h1(markdown) or "")
        if heading_match is None:
            errors.append(f"{filename}: first H1 must start with ADR number {file_identifier:04d}")
            continue
        heading_identifier = int(heading_match.group("identifier"))
        if heading_identifier != file_identifier:
            errors.append(
                f"{filename}: ADR heading {heading_identifier:04d} does not match filename {file_identifier:04d}"
            )

    counts = Counter(identifiers)
    expected = set(range(1, stable_maximum + 1))
    missing = sorted(expected - counts.keys())
    unexpected = sorted(counts.keys() - expected)
    duplicates = sorted(identifier for identifier, count in counts.items() if count > 1)
    if missing:
        errors.append(f"ADR inventory: missing identifiers {', '.join(f'{value:04d}' for value in missing)}")
    if unexpected:
        errors.append(f"ADR inventory: unexpected identifiers {', '.join(f'{value:04d}' for value in unexpected)}")
    if duplicates:
        errors.append(f"ADR inventory: duplicate identifiers {', '.join(f'{value:04d}' for value in duplicates)}")
    return tuple(errors)


def _adr_documents(adr_root: Path) -> dict[str, str]:
    """Read every direct ADR Markdown document except its directory index."""
    return {
        path.name: path.read_text(encoding="utf-8")
        for path in adr_root.glob("*.md")
        if path.name != "index.md" and not path.name.startswith(".") and path.is_file()
    }


def _identifier_errors(docs_root: Path = DOCS_ROOT) -> tuple[str, ...]:
    """Return every stable-identifier error in the documentation corpus."""
    headings: dict[str, str | None] = {}
    for path in SECTION_OWNERS:
        topic = docs_root / path
        headings[path] = _first_h1(topic.read_text(encoding="utf-8")) if topic.is_file() else None

    decisions_path = docs_root / "design-decisions.md"
    decision_errors = (
        _decision_errors(decisions_path.read_text(encoding="utf-8"))
        if decisions_path.is_file()
        else ("design-decisions.md: missing decision register",)
    )
    documents = _adr_documents(docs_root / "adr")
    return _section_errors(headings) + decision_errors + _adr_errors(documents)


def _require_stable_identifiers(docs_root: Path = DOCS_ROOT) -> None:
    """Reject any public specification identifier drift."""
    errors = _identifier_errors(docs_root)
    if errors:
        raise AssertionError("documentation identifier drift:\n  " + "\n  ".join(errors))


class IdentifierIntegrityTest(TestCase):
    """Keep public docs references stable without constraining semantics."""

    def test_current_identifiers_are_stable(self) -> None:
        _require_stable_identifiers()

    def test_expands_section_ranges(self) -> None:
        self.assertEqual(_section_identifiers("Spec (§1\u2013§3, §11)"), [1, 2, 3, 11])

    def test_reports_section_ownership_drift(self) -> None:
        headings = {path: "# " + ", ".join(f"§{value}" for value in values) for path, values in SECTION_OWNERS.items()}
        headings["architecture.md"] = "# Architecture (§1, §2)"
        headings["federation.md"] = "# Federation (§7, §8)"

        errors = _section_errors(headings)

        self.assertIn("architecture.md: missing SPEC sections §3, §11", errors)
        self.assertIn("federation.md: unexpected SPEC sections §7", errors)
        self.assertIn("SPEC mapping: missing sections §3, §11", errors)
        self.assertIn("SPEC mapping: duplicate sections §7", errors)

    def test_reports_decision_gaps_duplicates_and_overflow(self) -> None:
        markdown = "\n".join(
            (
                "# Design Decisions D1\u2013D4",
                "### D1 — One",
                "### D2 — Two",
                "### D2 — Duplicate",
                "### D4 — Four",
                "### D5 — Overflow",
            )
        )

        self.assertEqual(
            _decision_errors(markdown, stable_maximum=4),
            (
                "design-decisions.md: missing decisions D3",
                "design-decisions.md: decisions exceed advertised range D5",
                "design-decisions.md: duplicate decisions D2",
            ),
        )

    def test_ignores_fenced_decision_pseudo_entries(self) -> None:
        markdown = "\n".join(
            (
                "# Design Decisions D1-D1",
                "```markdown",
                "### D1 — Example only",
                "```",
            )
        )

        self.assertEqual(
            _decision_errors(markdown, stable_maximum=1),
            ("design-decisions.md: missing decisions D1",),
        )

    def test_keeps_heading_after_same_line_backtick_code(self) -> None:
        markdown = "\n".join(
            (
                "# Design Decisions D1-D1",
                "```literal```",
                "### D1 — Real decision",
            )
        )

        self.assertEqual(_decision_errors(markdown, stable_maximum=1), ())

    def test_reports_terminal_identifier_removal(self) -> None:
        decisions = "\n".join(
            ("# Design Decisions D1\u2013D19", *(f"### D{value} — Decision" for value in range(1, 20)))
        )
        adrs = {f"{value:04d}-decision.md": f"# {value:04d} — Decision\n" for value in range(1, 20)}

        self.assertEqual(
            _decision_errors(decisions),
            ("design-decisions.md: advertised maximum D19 must remain D20",),
        )
        self.assertEqual(_adr_errors(adrs), ("ADR inventory: missing identifiers 0020",))

    def test_reports_adr_gaps_duplicates_and_heading_mismatch(self) -> None:
        documents = {
            "0001-first.md": "# 0002 — Wrong heading\n",
            "0003-third.md": "# 0003 — Third\n",
            "0003-third-copy.md": "# 0003 — Third copy\n",
        }

        self.assertEqual(
            _adr_errors(documents, stable_maximum=3),
            (
                "0001-first.md: ADR heading 0002 does not match filename 0001",
                "ADR inventory: missing identifiers 0002",
                "ADR inventory: duplicate identifiers 0003",
            ),
        )

    def test_requires_numbered_first_adr_h1(self) -> None:
        documents = {"0001-example.md": "# Overview\n\n# 0001 — Secondary\n"}

        self.assertEqual(
            _adr_errors(documents, stable_maximum=1),
            ("0001-example.md: first H1 must start with ADR number 0001",),
        )

    def test_discovers_malformed_adr_filenames(self) -> None:
        with TemporaryDirectory() as directory:
            adr_root = Path(directory)
            (adr_root / "0021_invalid.md").write_text("# 0021 — Invalid\n", encoding="utf-8")
            (adr_root / "index.md").write_text("# Index\n", encoding="utf-8")
            (adr_root / ".scratch.md").write_text("# Local scratch\n", encoding="utf-8")

            self.assertEqual(
                _adr_errors(_adr_documents(adr_root), stable_maximum=0),
                ("0021_invalid.md: invalid numbered ADR filename",),
            )
