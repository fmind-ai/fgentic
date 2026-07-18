---
name: docs-spec
description: Maintain Fgentic's documentation system — the topic specs under docs/ with stable §N numbering, authoring ADRs to revisit settled decisions, and keeping README/AGENTS.md in sync. Use when writing or restructuring any documentation, or when a change touches a settled design.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Docs & Spec Maintenance

The specification lives as topic docs under `docs/` (architecture, design-decisions, bridge, security, federation, observability, licensing, roadmap), split from the retired root `SPEC.md` **with its `§N` numbering preserved** — issues and code comments cite `SPEC §N` and resolve via the mapping table in [.agents/AGENTS.md](../../AGENTS.md).

## Rules for editing docs/

1. **Never renumber.** `§N` anchors are a stable public contract (issue bodies, ADRs, code comments cite them). Add new content as new subsections (`§N.M`); mark removed content as retired in place rather than shifting numbers.
1. Keep the `D<n>` design-decision register (`docs/design-decisions.md`) authoritative: a new settled decision gets the next `D` number with its evidence; reference decisions as `D<n>` everywhere else instead of restating them.
1. `docs/roadmap.md` records phase **history** + the milestone mapping; the forward roadmap lives only in GitHub milestones — don't grow a parallel plan in the file.
1. Markdown conventions: dprint-formatted (`mise run format`), every numbered list item uses `1.`, relative links between docs, no absolute paths.
1. **OKF conformance.** `docs/` is an [Open Knowledge Format](https://github.com/GoogleCloudPlatform/knowledge-catalog/blob/main/okf/SPEC.md) bundle: every concept doc starts with YAML frontmatter carrying `type` (required — e.g. `Specification`, `Runbook`, `Architecture Decision Record`), `title`, and a one-sentence `description`. `index.md` files are reserved directory listings; the current set is the bundle root plus `adopters/`, `adr/`, `onboarding/`, and `security/`. Keep their entries and descriptions in sync when adding, renaming, or removing a doc. Per OKF §6, sub-indexes carry no frontmatter; only the bundle root uses the §11 `okf_version` frontmatter exception.

## Authoring an ADR (docs/adr/)

Settled designs (the current `D<n>` register and existing ADRs) are revisited by **proposing a new ADR**, never a drive-by PR.

1. Next number, kebab title: `docs/adr/NNNN-<slug>.md`.
1. Structure (match ADR 0008): `# NNNN — Title` · `Status: Proposed|Accepted|Superseded by [NNNN]` · `## Context` (the forces + trade-off, with evidence) · `## Decision` (numbered, declarative, including what is _not_ done) · `## Consequences` (numbered, honest — costs and escape hatches, not just benefits).
1. Cross-link related ADRs and `D<n>` decisions; if it supersedes an ADR, update the old one's Status.
1. An ADR that changes behavior ships with the change that implements it (or an issue tracking it).

## README / AGENTS.md sync

1. `README.md` is for humans (what/why/quickstart); `.agents/AGENTS.md` is for agents (layout, protocols, principles, conventions). Update whichever a change makes stale — a new directory, task, skill, or convention belongs in the AGENTS.md layout/conventions sections.
1. `apps/matrix-a2a-bridge/AGENTS.md` owns app-level layout and code conventions — keep app detail there, not in the root file.
1. Never write unsolicited summary/report/plan `*.md` files; scratch work goes in `.agents/tmp/` (git-ignored).
