---
type: Reference
title: OpenSSF Best Practices & Scorecard Self-Assessment
description: Passing-level OpenSSF Best Practices criteria mapped to in-repo evidence, plus the accepted Scorecard deviations, prepared for the CNCF-path trust signal.
---

# OpenSSF Best Practices & Scorecard Self-Assessment

The [adoption path](../architecture.md) runs through CNCF Sandbox, and both CNCF due diligence and sovereign-adopter security reviews check the OpenSSF signals first — the [Scorecard](https://securityscorecards.dev/) score and the [Best Practices](https://www.bestpractices.dev/) badge. The underlying posture is already strong (signed digest-pinned image and chart, SLSA + SBOM attestations, Flux keyless verification, DCO, private vulnerability reporting, SHA-pinned workflow actions), so this is **measurement and packaging, not new controls** — and it never weakens the CD digest-pin flow or any existing control to chase a score.

This document is the **preparable** part of issue #460. Two terminal steps ride on the repository going public ([#70](https://github.com/fmind-ai/fgentic/issues/70)) and are a **human** action under the maintainer account: registering the bestpractices.dev project entry and adding the rendered badges to the README; flipping `publish_results: true` and enabling SARIF upload to code scanning in `.github/workflows/scorecard.yml`. Until then the Scorecard workflow runs with `publish_results: false` and keeps its SARIF as a bounded workflow artifact (SARIF-to-code-scanning is unavailable on the private plan).

## Best Practices passing-level criteria → evidence

Every row below maps a passing-level criterion to concrete in-repo evidence. No criterion is claimed that the repository does not implement; where a criterion is only partially met the row says so.

### Basics

1. **Project description and homepage** — [`README.md`](../../README.md) states what the project does and how to evaluate it.
1. **Contribution process** — [`CONTRIBUTING.md`](../../CONTRIBUTING.md) documents the issue→PR workflow, labels, and **DCO sign-off** (no CLA); [`GOVERNANCE.md`](../../GOVERNANCE.md) and [`MAINTAINERS.md`](../../MAINTAINERS.md) record project governance.
1. **FLOSS license** — Apache-2.0 ([`LICENSE`](../../LICENSE), rationale in [`docs/licensing.md`](../licensing.md)); an OSI-approved license, stored in the standard location.
1. **Documentation (basics + interface)** — [`README.md`](../../README.md) plus the topic specs under [`docs/`](../index.md) and the agent-facing [`AGENTS.md`](../../AGENTS.md) describe the architecture and interfaces.
1. **English / other** — the project communicates in English; the issue and PR templates live under [`.github/`](../../.github/).

### Change control

1. **Public version-controlled source** — Git on GitHub, with the full history public once [#70](https://github.com/fmind-ai/fgentic/issues/70) flips visibility.
1. **Unique, semantic versioning** — releases are `v`-prefixed semver via the `release` skill; the `main` branch is protected by the `protect-main` ruleset (no force-push, deletion, or non-linear history).
1. **Release notes / changelog** — releases publish a `git-cliff` changelog (Conventional Commits drive it); see the `release` skill and tagged GitHub releases.

### Reporting

1. **Bug-reporting process** — [`.github/ISSUE_TEMPLATE/`](../../.github/ISSUE_TEMPLATE) provides structured issue forms; the backlog convention is in the `github-flow` skill.
1. **Vulnerability-reporting process** — [`SECURITY.md`](../../SECURITY.md) defines **private** vulnerability reporting; security bugs are never filed as public issues.
1. **Vulnerability-report response** — the [security release process](release-process.md) covers private report → signed patch → GHSA/CVE publication → adopter notification.

### Quality

1. **Working build system** — `mise` is the single source of truth for `install`/`build`/`format`/`check`/`test`; lefthook and CI reuse the same tasks.
1. **Automated test suite + CI** — `mise run test` runs the Go suites (race + coverage), the Python/knowledge suites, and the deterministic manifest/policy gates; `.github/workflows/ci.yml` runs the identical gates on every PR, and `.github/workflows/fuzz.yml` runs an extended nightly fuzz.
1. **Tests for new functionality** — the [testing standard](../../AGENTS.md) requires deterministic tests for changes; new agents are gated by `mise run test:agents-golden` and the offline `mise run agent:test <name>` loop.
1. **Warning flags treated as errors** — the lint gates run warning-free (golangci-lint, `ruff`, `shellcheck --severity=style`, `dprint`, `trivy config`), and a change that introduces a warning fails `mise run check`.

### Security

1. **Secure development knowledge** — the [security spec §7](../security.md) and the [threat model](threat-model.md) map controls (prompt injection is the stated #1 threat); [prompt-injection controls](prompt-injection.md) states each control's honest limits.
1. **Good cryptographic practices** — ES256/JCS (RFC 8785) Signed AgentCards and usage receipts, P-256 keys, TLS for all web traffic, SOPS-age for secrets; no weak/rolled-own primitives, and verify-only public material is exchanged across organizations.
1. **Secured delivery against MITM** — the bridge image and Helm chart are cosign-signed and consumed by **immutable digest**; Flux source-controller keyless-verifies the chart before helm-controller sees it ([supply-chain verification](supply-chain.md)); all workflow actions are **SHA-pinned**.
1. **Publicly known vulnerabilities addressed** — `trivy`, `govulncheck`, and CodeQL gate every change; advisories are remediated promptly (e.g. the mid-cycle `golang.org/x/text` GO-2026-5970 bump).
1. **No leaked credentials** — a `gitleaks` pre-commit and CI gate plus SOPS-age encryption keep plaintext secrets out of Git; only `*.sops.yaml` ciphertext is committed.

### Analysis

1. **Static analysis** — golangci-lint (Go), `ruff` (Python), `shellcheck` (shell), `trivy config` (IaC), and CodeQL via GitHub code scanning default setup (actions/go/python, on every PR and weekly) run on every change.
1. **Dynamic analysis** — Go native fuzzing over the owned untrusted-input parsers (`mise run test:fuzz`, nightly extended budget) and the isolated Matrix↔A2A integration fixture.

## Scorecard: accepted deviations

`.github/workflows/scorecard.yml` runs the OpenSSF Scorecard on a weekly schedule and on `main` pushes. The following checks score below maximum for reasons that are either private-plan limits or deliberate design; each is an **accepted deviation with a written rationale**, not a silent control weakening.

1. **Branch-Protection** — `main` uses the `protect-main` ruleset (blocks force-push, deletion, non-linear history), but **intentionally omits repo-level required-PR/required-status-checks**: CD's digest-pin step fast-forwards a commit to `main` as `github-actions[bot]`, which a repo-level PR/check rule would block, and a _repo_ ruleset cannot grant the built-in Actions app a bypass (that needs an _org_-scoped ruleset). This trade-off is documented in [`AGENTS.md`](../../AGENTS.md); enforced review for outside contributors is the documented org-ruleset next step.
1. **SARIF upload / publish_results** — SARIF upload to code scanning and `publish_results: true` are **unavailable on the private plan** and are deferred to the [#70](https://github.com/fmind-ai/fgentic/issues/70) public flip; until then the SARIF is a bounded workflow artifact.
1. **Pinned-Dependencies, Token-Permissions, Dangerous-Workflow** — expected to pass: every workflow action is SHA-pinned with a version hint (`mise run check:github-actions` enforces it), every workflow declares an explicit least-privilege `permissions` map, and no workflow checks out and executes untrusted code.
1. **Fuzzing, SAST, CI-Tests, Vulnerabilities, Security-Policy** — expected to pass on the evidence above (Go fuzz, CodeQL/golangci-lint/trivy, the CI gate, `govulncheck`, and `SECURITY.md`).

When the repository is public, re-run Scorecard, record the actual per-check scores here, and resolve or re-justify any newly non-passing check before adding the badges.
