"""Regression tests for documentation link rewriting."""

from pathlib import Path
from unittest import TestCase

from hooks import _rewrite_links


class RewriteLinksTest(TestCase):
    """Keep canonical links portable without mutating code examples."""

    def test_rewrites_repository_source_link_to_github(self) -> None:
        markdown = "[workflow](../.github/workflows/docs.yml)"

        rewritten = _rewrite_links(markdown, Path("site.md"))

        self.assertEqual(
            rewritten,
            "[workflow](https://github.com/fmind-ai/fgentic/blob/main/.github/workflows/docs.yml)",
        )

    def test_normalizes_link_to_another_docs_page(self) -> None:
        markdown = "[security](security.md#boundaries)"

        rewritten = _rewrite_links(markdown, Path("site.md"))

        self.assertEqual(rewritten, markdown)

    def test_rewrites_link_with_inline_code_in_label(self) -> None:
        markdown = "[`workflow`](../.github/workflows/docs.yml)"

        rewritten = _rewrite_links(markdown, Path("site.md"))

        self.assertEqual(
            rewritten,
            "[`workflow`](https://github.com/fmind-ai/fgentic/blob/main/.github/workflows/docs.yml)",
        )

    def test_preserves_fenced_code(self) -> None:
        markdown = "```markdown\n[workflow](../.github/workflows/docs.yml)\n```"

        self.assertEqual(_rewrite_links(markdown, Path("site.md")), markdown)

    def test_preserves_fenced_code_in_blockquote(self) -> None:
        markdown = "> ```markdown\n> [workflow](../.github/workflows/docs.yml)\n> ```"

        self.assertEqual(_rewrite_links(markdown, Path("site.md")), markdown)

    def test_preserves_inline_code(self) -> None:
        markdown = "Use `[workflow](../.github/workflows/docs.yml)` as an example."

        self.assertEqual(_rewrite_links(markdown, Path("site.md")), markdown)

    def test_preserves_indented_code(self) -> None:
        markdown = "    [workflow](../.github/workflows/docs.yml)\n"

        self.assertEqual(_rewrite_links(markdown, Path("site.md")), markdown)

    def test_preserves_indented_code_in_blockquote(self) -> None:
        markdown = ">     [workflow](../.github/workflows/docs.yml)\n"

        self.assertEqual(_rewrite_links(markdown, Path("site.md")), markdown)

    def test_handles_long_blockquote_prefix_in_linear_pass(self) -> None:
        prefix = " \t>" * 5_000
        markdown = f"{prefix} [workflow](../.github/workflows/docs.yml)\n"

        rewritten = _rewrite_links(markdown, Path("site.md"))

        self.assertTrue(rewritten.startswith(prefix))
        self.assertTrue(
            rewritten.endswith("[workflow](https://github.com/fmind-ai/fgentic/blob/main/.github/workflows/docs.yml)\n")
        )

    def test_preserves_raw_html_code(self) -> None:
        markdown = "<pre><code>[workflow](../.github/workflows/docs.yml)</code></pre>"

        self.assertEqual(_rewrite_links(markdown, Path("site.md")), markdown)
