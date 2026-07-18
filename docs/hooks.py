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
_HTML_CODE_BLOCK = re.compile(
    r"<(?P<tag>pre|code)\b[^>]*>.*?</(?P=tag)>\s*",
    flags=re.IGNORECASE | re.DOTALL,
)
_INLINE_CODE = re.compile(r"(?P<ticks>`+)(?P<code>[^\n]*?)(?P=ticks)")
_MARKDOWN_LINK = re.compile(
    r"(?P<prefix>!?\[[^\]\n]*\]\()"
    r"(?P<target><[^>\n]+>|[^)\s\n]+)"
    r"(?P<suffix>(?:\s+(?:\"[^\"]*\"|'[^']*'))?\))",
)


def _relative_docs_target(target: Path, source_directory: Path) -> str:
    """Return a POSIX path from the current page to another documentation file."""
    return Path(os.path.relpath(target, source_directory)).as_posix()


def _content_after_blockquotes(line: str) -> str:
    """Return line content after consuming Markdown blockquote prefixes once."""
    cursor = 0
    while cursor < len(line):
        marker = cursor
        while marker < len(line) and line[marker] in " \t":
            marker += 1
        if marker >= len(line) or line[marker] != ">":
            break
        cursor = marker + 1
        if cursor < len(line) and line[cursor] in " \t":
            cursor += 1
    return line[cursor:]


def _fence_delimiter(line: str) -> tuple[str, str] | None:
    """Parse a fenced-code delimiter after optional blockquote prefixes."""
    content = _content_after_blockquotes(line).lstrip(" \t")
    if not content or content[0] not in "`~":
        return None
    fence_character = content[0]
    fence_length = 1
    while fence_length < len(content) and content[fence_length] == fence_character:
        fence_length += 1
    if fence_length < 3:
        return None
    return content[:fence_length], content[fence_length:].rstrip("\r\n")


def _is_indented_code(line: str) -> bool:
    """Recognize indented code at the root or inside nested blockquotes."""
    return _content_after_blockquotes(line).startswith(("    ", "\t"))


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


def _rewrite_links_in_prose(markdown: str, source_path: Path) -> str:
    """Rewrite links in prose while preserving inline and indented code."""

    rendered: list[str] = []
    for line in markdown.splitlines(keepends=True):
        if _is_indented_code(line):
            rendered.append(line)
            continue

        inline_code_ranges = [match.span() for match in _INLINE_CODE.finditer(line)]

        def replace_link(match: re.Match[str], code_ranges: list[tuple[int, int]] = inline_code_ranges) -> str:
            if any(start <= match.start() and match.end() <= end for start, end in code_ranges):
                return match.group(0)
            target = _rewrite_target(match.group("target"), source_path)
            return f"{match.group('prefix')}{target}{match.group('suffix')}"

        rendered.append(_MARKDOWN_LINK.sub(replace_link, line))
    return "".join(rendered)


def _rewrite_links_outside_html_code(markdown: str, source_path: Path) -> str:
    """Rewrite prose outside raw HTML code and preformatted blocks."""
    rendered: list[str] = []
    cursor = 0
    for html_code in _HTML_CODE_BLOCK.finditer(markdown):
        rendered.append(_rewrite_links_in_prose(markdown[cursor : html_code.start()], source_path))
        rendered.append(html_code.group(0))
        cursor = html_code.end()
    rendered.append(_rewrite_links_in_prose(markdown[cursor:], source_path))
    return "".join(rendered)


def _rewrite_links(markdown: str, source_path: Path) -> str:
    """Rewrite Markdown links outside all Markdown and HTML code contexts."""
    rendered: list[str] = []
    prose: list[str] = []
    fence_character = ""
    fence_length = 0

    def flush_prose() -> None:
        rendered.append(_rewrite_links_outside_html_code("".join(prose), source_path))
        prose.clear()

    for line in markdown.splitlines(keepends=True):
        delimiter = _fence_delimiter(line)
        if fence_character:
            rendered.append(line)
            if delimiter:
                fence, tail = delimiter
                if fence[0] == fence_character and len(fence) >= fence_length and not tail.strip():
                    fence_character = ""
                    fence_length = 0
            continue
        if delimiter:
            flush_prose()
            rendered.append(line)
            fence, _ = delimiter
            fence_character = fence[0]
            fence_length = len(fence)
            continue
        prose.append(line)
    flush_prose()
    return "".join(rendered)


def on_page_markdown(markdown: str, page: Page, config: MkDocsConfig, files: Files) -> str:
    """Normalize docs links and send repository evidence links to GitHub's source browser."""
    del config, files
    return _rewrite_links(markdown, Path(page.file.src_path))
