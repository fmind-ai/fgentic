# Contributing to Fgentic

Thanks for considering a contribution — human or AI agent, the rules are the same. Start with [docs/](docs/) (the specification, split by topic: architecture, design decisions, security, federation, licensing) and [.agents/AGENTS.md](.agents/AGENTS.md) (conventions that bind all contributors).

## Where to start

1. The backlog is the set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) (M0–M24 — the milestones page is the current source of truth), each with a `kind/epic` tracker issue listing its issues in sweep order. **Pickup is scoped by the `track/*` labels, not raw milestone order:** work `track/v1` (the Definitive v1 scope — see the [focus board #316](https://github.com/fmind-ai/fgentic/issues/316)) first, ordered by `priority/p0 → p1 → p2`; leave `track/vision` until v1 ships.
1. Issues labeled **`agent-ready`** are groomed with tasks and acceptance criteria — pick one up as-is. Issues labeled **`needs-human`** wait on a maintainer decision, account, approval, or spend — you can prepare the work, but flag the blocking part.
1. Issues labeled **`good first issue`** are the friendliest entry points.
1. For anything not covered by an issue, open one first — especially before changing a settled design (decisions D1–D16 in [docs/design-decisions.md](docs/design-decisions.md) and the ADRs in [docs/adr/](docs/adr/) are revisited by proposing a new ADR, not by a drive-by PR).

## Development workflow

1. **Setup:** install [mise](https://mise.jdx.dev/), then `mise run install` (pinned toolchain + lefthook git hooks + per-app toolchains). Bare `mise install` only fetches the root tools — it does **not** wire the git hooks or install the app toolchains, so use `mise run install`. The local platform runs on k3d — see the README quickstart.
1. **Validate:** `mise run check` and `mise run test` must pass warning-free before you push; the git hooks and CI run the same tasks. Run the two gates **one at a time** — both are heavy, and on a constrained host running them concurrently starves each into spurious failures. Never weaken an assertion, add a skip, or suppress a lint error to get green.
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
