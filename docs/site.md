---
type: Guide
title: Documentation Site
description: The generator decision, authoring checks, and human-gated GitHub Pages publication contract.
---

# Documentation Site

## 1. Decision

After the [one-time publication gate](#3-publication-gate), Fgentic will publish the existing `docs/` Markdown tree as a static GitHub Pages site. Markdown stays the only content source; the rendered site is a discoverability and search layer, not a second documentation system.

The generator is exactly pinned Material for MkDocs 9.7.7 with a committed uv lock. Material entered maintenance mode in November 2025: its maintainers committed to critical bug and security fixes for at least 12 months, but no new features. This known support window is acceptable for the v1 launch because the configuration deliberately avoids custom templates and third-party plugins.

[Zensical](https://zensical.org/compatibility/) is the planned follow-on. It reads existing `mkdocs.yml` files and aims to preserve content, URLs, anchors, and Material-compatible customization. Re-evaluate the switch before Material's maintenance commitment ends or earlier if an unfixed security or compatibility issue appears.

## 2. Authoring and validation

Use the repository tasks from its root:

```bash
mise run docs:build
mise run docs:serve
```

`docs:build` uses the frozen `docs/uv.lock`, builds every page with warnings promoted to errors, and writes only to the ignored `.agents/tmp/docs-site/` directory. The root `check` aggregate includes the same build. The site loads no analytics, remote fonts, or custom browser scripts; Material's bundled search and Mermaid support remain local to the generated artifact.

Update `docs/mkdocs.yml` navigation whenever a page is added or renamed. Keep `docs/index.md` as the source-tree inventory required by the documentation specification.

## 3. Publication gate

The `Docs` workflow builds relevant changes on `main`, but its Pages configuration, artifact upload, and deploy steps remain disabled until the repository variable `FGENTIC_PAGES_ENABLED` is exactly `true`. This keeps `main` green before the repository-level Pages setting exists.

The maintainer performs the publication action under their account:

1. In **Settings → Pages**, select **GitHub Actions** as the source.
1. Add the repository Actions variable `FGENTIC_PAGES_ENABLED=true`.
1. Dispatch the `Docs` workflow once and verify `https://fmind-ai.github.io/fgentic/`.
1. Change the repository homepage from the source-tree URL to the verified Pages URL.

After that one-time gate, every relevant push to `main` rebuilds and publishes automatically through the protected `github-pages` environment. Remove or set the variable to any other value to stop publication without changing source or workflow history.
