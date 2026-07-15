---
name: github-flow
description: Work the Fgentic GitHub backlog — current milestones, epic trackers, labels, issue pickup, DCO commits, PRs, and how CI/CD behaves (digest pinning). Use when picking up an issue, triaging, opening a PR, or reasoning about the pipelines.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# Fgentic GitHub Flow

Repo: `fmind-ai/fgentic` (public, Apache-2.0). All work is issue-driven; automate with `gh`.

## Backlog model

1. The roadmap is the **current GitHub milestones** (sequenced sovereignty-first; the milestones page is the source of truth; dated history + mapping in [docs/roadmap.md](../../../docs/roadmap.md)). Each milestone has exactly one `kind/epic` tracker issue with the sweep order and definition of done — **start there, work top-to-bottom**.
1. List milestones: `gh api repos/fmind-ai/fgentic/milestones --jq '.[] | "\(.number) \(.title) — \(.open_issues) open"'`. Find an epic: `gh issue list --label kind/epic --milestone "<title>"`.
1. Labels: `agent-ready` = groomed, pick up as-is · `needs-human` = blocked on a maintainer decision/account/approval/spend — do the preparable parts, then hand off explicitly · `area/*` (bridge, infra, identity, matrix, models, federation, observability, security, ci, docs, community) · `kind/*` (feature, fix, test, chore, docs, research, epic).
1. Issue bodies cite `SPEC §N` — resolve via the mapping table in [.agents/AGENTS.md](../../AGENTS.md).
1. Standing rules: never start M8 (federation) items before its epic says the lab topology exists; never merge anything that assumes a single homeserver forever; settled designs (the current `D<n>` register and ADRs) are revisited by proposing a new ADR, not a drive-by PR.

## Picking up an issue

1. Inspect the issue's assignees, `status/in-progress` label, comments, related branches/worktrees, and open PRs. Skip any active claim.
1. Claim the issue before editing: `gh issue edit <N> --add-assignee @me --add-label status/in-progress`, then leave a UTC-stamped comment naming the session and intended branch. Re-read the issue immediately; if another active claim appeared, stop and choose another issue. This is a cooperative GitHub lease, not an atomic mutex.
1. Hosted agents that cannot mutate GitHub must be handed one already-claimed issue. They do not select an issue from the queue.
1. A claim is stale only after 12 hours with no heartbeat, branch, commit, or PR. Verify all four signals and comment with the takeover reason before replacing it. On abandonment, comment with the handoff state and run `gh issue edit <N> --remove-label status/in-progress --remove-assignee @me`; an active PR keeps the claim until merge or explicit handoff.
1. Read the issue's **Tasks + Acceptance criteria and follow them literally** — don't substitute your own scope.
1. Branch `<type>/<slug>` off `main`; one concern per branch.
1. During development, run only the focused checks that cover the changed boundary. In an installed local worktree, the commit hook runs warning-free `check` and the push hook runs warning-free `test`, both through the shared mutex; do not duplicate them manually. In a hookless hosted/disposable environment, run `mise run agent:gate` once near PR readiness. Re-run focused checks after narrow fixes and only the invalidated hook/gate after later changes. Never weaken an assertion, skip a test, or suppress a lint to get green.
1. Commit: Conventional Commits **with DCO sign-off** (`git commit -s`). No AI-attribution trailers.

## Authoring issues & epics (keep the backlog consistent)

1. Issue body format: `## Context` (why, citing `D<n>`/`SPEC §N`) · `## Tasks` (checkboxes, each independently verifiable) · `## Acceptance criteria` (observable outcomes, not implementation). Label with exactly one `kind/*`, the `area/*`s it touches, and `agent-ready` **only if** the tasks + criteria are executable without a maintainer decision (else `needs-human`, naming the blocking part). Always assign a milestone.
1. Epic body format (one per milestone, `kind/epic`): a paragraph with the theme, sweep-order rationale, and definition of done, then `## Issues (sweep top-to-bottom)` as a checklist of `#N` references — closing keywords in PRs tick it automatically.
1. Before opening a design-changing issue, check the current `D<n>` register and ADRs: settled designs need a proposed ADR (see the docs-spec skill), not an issue asking to relitigate.

## Pull requests

1. Fill the PR template (What / Why / How / Test plan); link the issue with closing keywords (`Fixes #N` — this ticks the epic checklist). PRs are squash-merged.
1. `gh pr create --title "..." --body-file <path>` then watch CI: `gh pr checks <n> --watch`. In a non-interactive shell `gh` may refuse with "prefer_editor_prompt not supported in non-tty mode" — run `gh config set prefer_editor_prompt disabled` once. Merge with `gh pr merge <n> --squash --delete-branch`, then `git checkout main && git pull`.
1. Create the `<type>/<slug>` branch **before the first commit**. A commit straight to `main` skips PR CI _and_ the `Fixes #N` auto-close (you must then `gh issue close` manually), and a bug that slips in reds `main` — recover by fixing forward or reverting.

## CI / CD behavior

1. `ci.yml` (push to main + PRs): `mise run format` → `check` → `test` → **clean-tree assert** (`git status --porcelain` empty) — unformatted files or generated drift fail CI even if checks pass.
1. `cd.yml` (push to main touching `apps/matrix-a2a-bridge/**`): multi-arch image build → push to GHCR → trivy vuln scan (HIGH/CRITICAL fails) → cosign keyless sign → pins the immutable digest into `apps/matrix-a2a-bridge/deploy/helmrelease.yaml` and commits it back with `[skip ci]`. **CI is the only writer of that digest line** — never edit it by hand; Flux deploys whatever digest is in git.
1. After a bridge merge, `main` moves again (the digest commit) — `git pull --rebase` before pushing.
1. `main` is protected by the `protect-main` ruleset: **force-pushes, branch deletion, and non-linear history are blocked**. Normal fast-forward pushes and the CD digest-pin commit still work; there is deliberately no repo-level PR/status-check rule (it would block the `github-actions[bot]` digest push). Never rewrite `main`'s history — a deliberate rewrite is maintainer-only (temporarily disable the ruleset). Full rationale in [.agents/AGENTS.md](../../AGENTS.md).

## Releases & security

1. Releases are semver `v*` tags with a git-cliff changelog + GitHub Release; bridge image and chart release together. Maintainers cut them (see the `release` skill / [GOVERNANCE.md](../../../GOVERNANCE.md)).
1. **Never file vulnerabilities as public issues** — private reporting per [SECURITY.md](../../../SECURITY.md).
