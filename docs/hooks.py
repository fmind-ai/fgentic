"""Render canonical repository links correctly on the documentation site."""

from __future__ import annotations

import os
import re
from pathlib import Path
from urllib.parse import quote, unquote, urlsplit, urlunsplit

from mkdocs.config.defaults import MkDocsConfig
from mkdocs.structure.files import Files
from mkdocs.structure.pages import Page

_DOCS_ROOT = Path(__file__).resolve().parent
_REPO_ROOT = _DOCS_ROOT.parent
_FENCED_BLOCK = re.compile(
    r"^[ \t]*(?P<fence>`{3,}|~{3,})[^\n]*\n.*?^[ \t]*(?P=fence)[ \t]*$",
    flags=re.MULTILINE | re.DOTALL,
)
_MARKDOWN_LINK = re.compile(
    r"(?P<prefix>!?\[[^\]\n]*\]\()"
    r"(?P<target><[^>\n]+>|[^)\s\n]+)"
    r"(?P<suffix>(?:\s+(?:\"[^\"]*\"|'[^']*'))?\))",
)


def _relative_docs_target(target: Path, source_directory: Path) -> str:
    """Return a POSIX path from the current page to another documentation file."""
    return Path(os.path.relpath(target, source_directory)).as_posix()


def _rewrite_target(target: str, source_path: Path) -> str:
    """Map one Markdown target to its generated-site or repository-browser location."""
    wrapped = target.startswith("<") and target.endswith(">")
    raw_target = target[1:-1] if wrapped else target
    parsed = urlsplit(raw_target)
    if parsed.scheme or parsed.netloc or not parsed.path or parsed.path.startswith("/"):
        return target

    source_directory = (_DOCS_ROOT / source_path).parent
    resolved = (source_directory / unquote(parsed.path)).resolve()
    if not resolved.is_relative_to(_REPO_ROOT) or not resolved.exists():
        return target

    if resolved.is_dir() and (resolved / "index.md").is_file():
        resolved = resolved / "index.md"

    if resolved.is_relative_to(_DOCS_ROOT):
        path = _relative_docs_target(resolved, source_directory)
        rewritten = urlunsplit(("", "", path, parsed.query, parsed.fragment))
    else:
        kind = "tree" if resolved.is_dir() else "blob"
        repository_path = quote(resolved.relative_to(_REPO_ROOT).as_posix(), safe="/")
        path = f"/{kind}/main/{repository_path}"
        rewritten = urlunsplit(("https", "github.com", f"/fmind-ai/fgentic{path}", parsed.query, parsed.fragment))

    return f"<{rewritten}>" if wrapped else rewritten


def _rewrite_links(markdown: str, source_path: Path) -> str:
    """Rewrite Markdown links outside fenced code blocks."""

    def replace_link(match: re.Match[str]) -> str:
        target = _rewrite_target(match.group("target"), source_path)
        return f"{match.group('prefix')}{target}{match.group('suffix')}"

    rendered: list[str] = []
    cursor = 0
    for fenced_block in _FENCED_BLOCK.finditer(markdown):
        rendered.append(_MARKDOWN_LINK.sub(replace_link, markdown[cursor : fenced_block.start()]))
        rendered.append(fenced_block.group(0))
        cursor = fenced_block.end()
    rendered.append(_MARKDOWN_LINK.sub(replace_link, markdown[cursor:]))
    return "".join(rendered)


def on_page_markdown(markdown: str, page: Page, config: MkDocsConfig, files: Files) -> str:
    """Normalize docs links and send repository evidence links to GitHub's source browser."""
    del config, files
    return _rewrite_links(markdown, Path(page.file.src_path))
