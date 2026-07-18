# Contributing to Fgentic

Thanks for considering a contribution — human or AI agent, the rules are the same. Start with [docs/](docs/) (the specification, split by topic: architecture, design decisions, security, federation, licensing) and [.agents/AGENTS.md](.agents/AGENTS.md) (conventions that bind all contributors).

## Where to start

1. The backlog is the current set of [GitHub milestones](https://github.com/fmind-ai/fgentic/milestones) (the milestones page is the source of truth), each with a `kind/epic` tracker issue listing its issues in sweep order. **Pickup is scoped by the `track/*` labels, not raw milestone order:** work `track/v1` (the Definitive v1 scope — see the [focus board #316](https://github.com/fmind-ai/fgentic/issues/316)) first, ordered by `priority/p0 → p1 → p2`; leave `track/vision` until v1 ships.
1. Issues labeled **`agent-ready`** are groomed with tasks and acceptance criteria — pick one up as-is. Issues labeled **`needs-human`** wait on a maintainer decision, account, approval, or spend — you can prepare the work, but flag the blocking part.
1. Issues labeled **`good first issue`** are the friendliest entry points.
1. For anything not covered by an issue, open one first — especially before changing a settled design (the `D<n>` register in [docs/design-decisions.md](docs/design-decisions.md) and the ADRs in [docs/adr/](docs/adr/) are revisited by proposing a new ADR, not by a drive-by PR).

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

1. Compute the version with `mise exec -- git-cliff --bumped-version` and verify it matches the intended compatibility change. The one-time `v1.0.0` gate uses the explicit major command below instead of pretending the current unreleased commits imply a breaking change.
1. Generate `CHANGELOG.md` with `mise exec -- git-cliff --bump -o CHANGELOG.md`, update the chart's `version` and `appVersion` to the same version, and commit those files as `chore(release): vX.Y.Z`. For `v1.0.0` only, pass `--bump major`; later releases return to automatic SemVer.
1. Create an annotated `vX.Y.Z` tag, push the release commit and tag, then publish a GitHub Release from `mise exec -- git-cliff --latest --strip all` output.

Published tags are immutable. If a release step fails, fix the cause and cut a new version; never move or replace an existing tag.

### v1.0.0 readiness gate

`v1.0.0` changes the compatibility promise, so it is not a normal version bump. The release PR must link #471 and contain the completed checklist below with exact issue, document, run, tag, release, or evidence-artifact links. An open issue, a local-only result, or a planned control is not a checked item.

1. **G1 — federation GA:** #316's G1 is accepted with the production-grade federation and production-reference epics (#85, #382, and #86) closed, and the retained acceptance evidence is linked.
1. **G2 — real cross-organization pilot:** #316 records two real organizations completing the governed Matrix + A2A flow, with both parties' approval for the bounded public evidence. Synthetic or same-operator lab identities do not count.
1. **G3 — trust proof:** the external security report in #459 is published; the compliance annex in #74 has recorded legal/DPO approval; and #86 links live reference-deployment evidence. Prepared source or offline renders do not count as publication or live proof.
1. **Adopter release contract:** #188 is closed after at least one prior tag-to-tag N-1→N upgrade follows only its published BOM and upgrade notes. `v0.1.0` alone cannot satisfy this gate.
1. **Security support:** [SECURITY.md](SECURITY.md) still states the supported-version window, security-fix/backport behavior, and private reporting route accepted for 1.0.
1. **Launch readiness:** #70 is closed after the public visibility/protection/CD checks, and #69 is closed after the launch publication and channel checklist. Drafts and account-level preparation do not count.
1. **Stability promotions:** every `Promote to Stable` row in [docs/stability.md](docs/stability.md) is changed to Stable in the release PR; every retained Beta or Experimental surface keeps its recorded rationale. The maintainer records acceptance in #471 before tagging.
1. **Mechanical SemVer:** `mise run check:release-versioning` passes and the root `git-cliff` tool has an exact version pin under #477. On the release branch, `mise exec -- git-cliff --unreleased --bump major --context | mise exec -- jq -r '.[0].version'` returns exactly `v1.0.0`; after 1.0 the same checked config maps breaking changes to major, compatible features to minor, and fixes to patch.
1. **Release artifacts:** the release commit contains the generated `CHANGELOG.md` and matching bridge chart `version`/`appVersion`; the annotated tag, image, chart, SBOM/provenance/signatures, and GitHub Release all use `v1.0.0`/`1.0.0` as defined by #7 and #188.
1. **Non-blocking G4:** record the current #464/CNCF status for transparency, but do not block the tag on foundation acceptance; #316 explicitly keeps G4 external to the product-release decision.

The maintainer performs the final acceptance and release trigger. Automation may prepare and verify the release PR, but it must not manufacture external-pilot, publication, audit, legal, or account evidence.

## Security

Never report vulnerabilities in public issues — see [SECURITY.md](SECURITY.md).
