---
type: Runbook
title: Hosted Coding Agents
description: Configure Claude Code on the web and Codex Cloud so a hosted session can work one pre-assigned issue end-to-end under the repository's agent conventions.
---

# Hosted Coding Agents

Hosted (web) sessions differ from local CLI worktrees in three ways: no local cluster, no installed git hooks, and — depending on the provider — no GitHub access beyond pushing branches and opening pull requests. This runbook is the one-time operator setup per provider plus the contract every hosted session prompt must carry. The binding conventions stay in [.agents/AGENTS.md](../.agents/AGENTS.md) and [CONTRIBUTING.md](../CONTRIBUTING.md); verified provider behavior below is as of 2026-07.

## What a hosted session may do

1. Work exactly **one already-claimed issue** handed to it in the session prompt (the github-flow rule) — hosted sessions never sweep the queue and never merge pull requests.
1. Ship only offline-provable work: focused checks for the changed boundary, then `mise run agent:gate` once near PR readiness (hosted environments are hookless).
1. Open a pull request with the template, DCO sign-off, `Fixes #N`, and the `by/hosted` label — or `lane: hosted` in the PR body when labeling is unavailable.
1. Prepare-then-list anything it cannot prove: hosted sandboxes have no k3d/kind, so work needing `dev:*`, `demo:*`, `fed:*`, `cluster:*`, or live credentials is delivered up to the offline boundary with the remainder listed explicitly in the PR body.

## Claude Code on the web (claude.ai/code)

1. Install the Claude GitHub App on the repository (one-time). Sessions read and comment on issues and PRs through a scoped GitHub proxy, push their branch, and open PRs; sessions can also be started and monitored from the mobile app.
1. Create an environment for the repository: network access **Custom** — keep the default allowlist and add `mise.jdx.dev` (installer domain) — with the setup script `curl https://mise.jdx.dev/install.sh | sh` (cached between sessions).
1. Nothing else is needed: the tracked [.claude/settings.json](../.claude/settings.json) SessionStart hook runs [scripts/agent-setup.sh](../scripts/agent-setup.sh) in every new session, and that script already resolves `mise` from `~/.local/bin`.

## Codex Cloud (chatgpt.com/codex)

1. Install the ChatGPT Codex Connector GitHub App on the repository (one-time, repository admin).
1. Cloud environments are configured only in the Codex web UI — the tracked [.codex/environments/environment.toml](../.codex/environments/environment.toml) applies to the Codex desktop app, not to the cloud. Create the environment on the `universal` image with: setup script `mise run agent:setup` (`mise` is preinstalled in that image), the same command as the maintenance script (it is idempotent), and git identity variables (`GIT_AUTHOR_NAME`, `GIT_AUTHOR_EMAIL`, `GIT_COMMITTER_NAME`, `GIT_COMMITTER_EMAIL`) so DCO sign-offs match the commit author.
1. Keep agent-phase internet **off** (the setup phase pre-warms all pinned tools and locked dependencies). If a task genuinely needs runtime downloads, allow exact domains only — never a broad preset, because issue-driven work processes untrusted input.
1. The sandbox exposes no GitHub token and no `gh`: a Codex Cloud session cannot read or mutate issues, so its prompt must carry the issue's Tasks and Acceptance criteria verbatim, and the claim must already exist on GitHub.
1. PR review integration: commenting `@codex review` on a pull request posts a review from the connector — usable as one of the peer-review lanes.

## Session prompt contract

Every hosted session prompt names exactly one issue and includes:

1. The claim statement (who reserved the issue and where — normally the issue itself carries the lease per [CONTRIBUTING.md](../CONTRIBUTING.md)), and the instruction to stop and report if the session can see a competing active claim.
1. The issue's Tasks + Acceptance criteria verbatim for providers without GitHub issue access.
1. The shipping rules: branch `<type>/<slug>` off `main`, Conventional Commits with DCO sign-off (`git commit -s`), the What / Why / How / Test plan PR template, `Fixes #N`, lane label `by/hosted`.
1. The offline-only rule and the prepare-then-list fallback from the contract above.
