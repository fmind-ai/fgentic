# Contributing to Fgentic

Thanks for considering a contribution — human or AI agent, the rules are the same. Start with [docs/](docs/) (the specification, split by topic: architecture, design decisions, security, federation, licensing) and [.agents/AGENTS.md](.agents/AGENTS.md) (conventions that bind all contributors).

## Where to start

1. The backlog is the current set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) (the milestones page is the source of truth), each with a `kind/epic` tracker issue listing its issues in sweep order. **Pickup is scoped by the `track/*` labels, not raw milestone order:** work `track/v1` (the Definitive v1 scope — see the [focus board #316](https://github.com/fmind-ai/fgentic/issues/316)) first, ordered by `priority/p0 → p1 → p2`; leave `track/vision` until v1 ships.
1. Issues labeled **`agent-ready`** are groomed with tasks and acceptance criteria — pick one up as-is. Issues labeled **`needs-human`** wait on a maintainer decision, account, approval, or spend — you can prepare the work, but flag the blocking part.
1. Issues labeled **[`good first issue`](https://github.com/fmind-ai/fgentic/issues?q=state%3Aopen%20label%3A%22good%20first%20issue%22)** are small, self-contained entry points reserved for human newcomers. Autonomous agents must skip them, even when another label says `agent-ready`. An agent may help only when the human who already claimed the issue explicitly asks for assistance.
1. For anything not covered by an issue, open one first — especially before changing a settled design (the `D<n>` register in [docs/design-decisions.md](docs/design-decisions.md) and the ADRs in [docs/adr/](docs/adr/) are revisited by proposing a new ADR, not by a drive-by PR).

## Your first contribution

1. Fork the repository, clone your fork, and create a short `<type>/<slug>` branch from current `main`.
1. Choose an open [`good first issue`](https://github.com/fmind-ai/fgentic/issues?q=state%3Aopen%20label%3A%22good%20first%20issue%22). Read its full Tasks and Acceptance criteria; check its assignees, `status/in-progress` label, claim comments, related branches, and open PRs; then comment with your intent and branch only when no active claim exists. A maintainer will confirm there is no competing worktree, assign the issue, and mark the lease before you start.
1. Run `mise run install`, make only the issue-sized change, and use the smallest focused checks while iterating. The installed hooks run the canonical repository gates before your commit and push complete.
1. Certify the Developer Certificate of Origin with `git commit -s` on every commit. This adds your own `Signed-off-by` trailer; it is not a CLA or an attribution trailer.
1. Push your branch and open a pull request using the repository template. Explain What / Why / How / Test plan, use `Fixes #N`, keep the PR focused, and respond to CI and review findings. Maintainers squash-merge accepted PRs.

If the pool is empty, do not relabel an arbitrary issue yourself. Comment on [#191](https://github.com/fmind-ai/fgentic/issues/191) so a maintainer can select work whose context, risk, and acceptance boundary are genuinely suitable for a newcomer.

## Maintaining the newcomer pool

Maintainers review the pool once per calendar quarter and record the audit on [#191](https://github.com/fmind-ai/fgentic/issues/191) or its successor. Keep at least ten open, unclaimed issues: add only human-selected, low-context work with complete acceptance criteria; remove `agent-ready` while an issue is reserved; remove `good first issue` when scope or prerequisites grow; and replace claimed or closed items without displacing the contributor already working them. If the pool falls below ten, record the deficit and select replacements before the quarterly review closes.

## Development workflow

1. **Setup:** install [mise](https://mise.jdx.dev/), then `mise run install` (pinned toolchain + lefthook git hooks + per-app toolchains). Bare `mise install` only fetches the root tools — it does **not** wire the git hooks or install the app toolchains, so use `mise run install`. The local platform runs on k3d — see the README quickstart.
1. **Claim:** before editing, skip issues with an active assignee or `status/in-progress` lease. Assign yourself, add `status/in-progress`, and leave a UTC-stamped owner/branch comment. Hosted agents without GitHub write access must receive an already-claimed issue (provider setup and the hosted session contract: [docs/hosted-agents.md](docs/hosted-agents.md)). See the [github-flow skill](.agents/skills/github-flow/SKILL.md) for stale-claim and handoff rules.
1. **Validate:** use focused checks during development. Installed commit/push hooks run the required warning-free `check` and `test` through a shared worktree mutex; do not duplicate them manually. In a hookless hosted/disposable environment, run `mise run agent:gate` once near PR readiness. Never weaken an assertion, add a skip, or suppress a lint error to get green.
1. **Code standards:** Go (default) and Python; type-safe, small composable units, errors wrapped with `%w`, no ignored errors, no tech debt. Match the surrounding code's conventions.
1. **Secrets:** never commit plaintext secrets — SOPS-encrypted `*.sops.yaml` only (gitleaks enforces this pre-commit).
1. **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `refactor:`, `chore:`, …) with **DCO sign-off** (`git commit -s`). No CLA. No AI-attribution trailers.
1. **Pull requests:** one concern per PR; fill the PR template (What / Why / How / Test plan); link the issue with closing keywords (`Fixes #N`). PRs are squash-merged.

`mise run check` includes Helm unit tests for the bridge chart, strict offline `flux build kustomization` renders for both `clusters/local` and `clusters/gcp`, and kubeconform validation of those renders. A chart conditional, overlay patch, or unresolved post-build variable therefore fails before Flux sees it. The empty `clusters/gcp/flux-system` Kustomization is an offline-validation placeholder; the spend-gated GKE bootstrap replaces it with the generated Flux controllers and sync manifests.

## Licensing

Contributions are accepted under **Apache-2.0** with DCO sign-off. Never add an AGPL dependency to the bridge (mautrix/go is MPL-2.0 — keep the `NOTICE` files current); see [docs/licensing.md](docs/licensing.md) for the full licensing map.

## Releases

Releases use SemVer, `v`-prefixed annotated tags, the repository's git-cliff configuration, and a GitHub Release. Before `v1.0.0`, breaking changes bump the minor version while features and fixes bump the patch version; from `v1.0.0`, breaking changes bump major, features bump minor, and fixes bump patch. The bridge image and Helm chart always carry the same version and release together.

Maintainers cut releases from a clean, up-to-date `main` (see [GOVERNANCE.md](GOVERNANCE.md)):

1. Compute the version with `mise exec -- git-cliff --bumped-version` and verify it matches the intended compatibility change.
1. Generate `CHANGELOG.md` with `mise exec -- git-cliff --bump -o CHANGELOG.md`, update the chart's `version` and `appVersion` to the same version, and commit those files as `chore(release): vX.Y.Z`.
1. Create an annotated `vX.Y.Z` tag, push the release commit and tag, then publish a GitHub Release from `mise exec -- git-cliff --latest --strip all` output.

Published tags are immutable. If a release step fails, fix the cause and cut a new version; never move or replace an existing tag.

## Security

Never report vulnerabilities in public issues — see [SECURITY.md](SECURITY.md).
