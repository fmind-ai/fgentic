# Contributing to Fgentic

Thanks for considering a contribution — human or AI agent, the rules are the same. Start with [docs/](docs/) (the specification, split by topic: architecture, design decisions, security, federation, licensing) and [.agents/AGENTS.md](.agents/AGENTS.md) (conventions that bind all contributors).

## Where to start

1. The backlog is the set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) (M0–M11), each with a `kind/epic` tracker issue listing its issues in sweep order.
1. Issues labeled **`agent-ready`** are groomed with tasks and acceptance criteria — pick one up as-is. Issues labeled **`needs-human`** wait on a maintainer decision, account, approval, or spend — you can prepare the work, but flag the blocking part.
1. Issues labeled **`good first issue`** are the friendliest entry points.
1. For anything not covered by an issue, open one first — especially before changing a settled design (decisions D1–D16 in [docs/design-decisions.md](docs/design-decisions.md) and the ADRs in [docs/adr/](docs/adr/) are revisited by proposing a new ADR, not by a drive-by PR).

## Development workflow

1. **Setup:** install [mise](https://mise.jdx.dev/), then `mise install` (pinned toolchain + lefthook git hooks). The local platform runs on k3d — see the README quickstart.
1. **Validate:** `mise run check` and `mise run test` must pass warning-free before you push; the git hooks and CI run the same tasks. Never weaken an assertion, add a skip, or suppress a lint error to get green.
1. **Code standards:** Go (default) and Python; type-safe, small composable units, errors wrapped with `%w`, no ignored errors, no tech debt. Match the surrounding code's conventions.
1. **Secrets:** never commit plaintext secrets — SOPS-encrypted `*.sops.yaml` only (gitleaks enforces this pre-commit).
1. **Commits:** [Conventional Commits](https://www.conventionalcommits.org/) (`feat:`, `fix:`, `refactor:`, `chore:`, …) with **DCO sign-off** (`git commit -s`). No CLA. No AI-attribution trailers.
1. **Pull requests:** one concern per PR; fill the PR template (What / Why / How / Test plan); link the issue with closing keywords (`Fixes #N`). PRs are squash-merged.

## Licensing

Contributions are accepted under **Apache-2.0** with DCO sign-off. Never add an AGPL dependency to the bridge (mautrix/go is MPL-2.0 — keep the `NOTICE` files current); see [docs/licensing.md](docs/licensing.md) for the full licensing map.

## Releases

Releases are semver, tagged `v*`, with a git-cliff changelog and a GitHub Release; the bridge image and chart release together. Maintainers cut releases (see [GOVERNANCE.md](GOVERNANCE.md)).

## Security

Never report vulnerabilities in public issues — see [SECURITY.md](SECURITY.md).
