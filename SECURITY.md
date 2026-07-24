# Security Policy

## Reporting a vulnerability

**Do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.**

Report privately via [GitHub private vulnerability reporting](https://github.com/fmind-ai/fgentic/security/advisories/new). If that is unavailable, email **fgentic@fmind.ai**.

You can expect an acknowledgment within 7 days and a remediation plan or triage outcome within 30 days. Coordinated disclosure is appreciated; we will credit reporters unless they prefer otherwise.

## Supported versions

Security fixes are guaranteed only for the latest published release line. A backport to the immediately preceding line may be offered when the change is low risk and maintainer capacity permits, but it is not promised. Older immutable tags remain available for verification, not maintenance; affected users must upgrade to a fixed release.

The adopter-facing release boundary and general support posture are documented in the [Adopter Release & Upgrade Contract](docs/releases.md) and [Public Surface Stability Contract](docs/stability.md#support-and-tested-upgrade-paths). This section remains authoritative for the security-fix window above; those broader contracts do not promise additional backports. [Issue #188](https://github.com/fmind-ai/fgentic/issues/188) remains open for the first `v0.2.0` publication under that contract and a live tag-to-tag upgrade drill, not for missing source documentation.

## Handling and disclosure

Confirmed vulnerabilities follow one private, evidence-bound lifecycle:

1. Acknowledge the report within 7 days. Keep vulnerability details in the private GitHub Security Advisory (GHSA), not a public issue, pull request, branch, or chat room.
1. Complete the initial triage within 30 days: validate impact and affected versions, assign a CWE and CVSS vector/severity, name an owner, propose an embargo target with the reporter, and record whether exploitation or public disclosure changes that target.
1. Develop the smallest fix privately. Add a regression test, review the affected and fixed version ranges, invite reporter credit with their consent, and request a CVE from GitHub only for a real confirmed vulnerability.
1. Cut an out-of-band patch from the latest supported release, containing only the fix and required release metadata. Run the normal tag-triggered CD path so the bridge image and chart are scanned, attested, keyless-signed, and published under immutable references. When the fix changes the bridge image, chart, or CD workflow, verify the separate `main` CD digest-pin commit. For a manifest or composed-component pin fix, verify the immutable source pin and record that no bridge digest-pin commit is expected.
1. Publish the fixed GitHub Release and GHSA in the coordinated disclosure window. The advisory names the CVE/GHSA, CVSS severity, CWE, affected and fixed ranges, accepted reporter credit, fixed tag, release notes, and any workarounds. Never publish an advisory without rechecking that its fixed version actually exists.
1. Notify adopters through the canonical [GitHub Security Advisories](https://github.com/fmind-ai/fgentic/security/advisories) and [Releases](https://github.com/fmind-ai/fgentic/releases) feeds. Correct material errors in both surfaces; never move an immutable tag.

The detailed maintainer checklist, advisory template, upstream-component path, and completed draft-only rehearsal are in the [security release process](docs/security/release-process.md).

## Scope

1. The `matrix-a2a-bridge` Go application and its Helm chart.
1. The platform manifests in `infra/` and `clusters/` (NetworkPolicies, gateway routes, secrets handling) — misconfigurations that break a documented security control are in scope.
1. The CI/CD supply chain (image signing, digest pinning).

Vulnerabilities in upstream components (Synapse, MAS, Element, kagent, agentgateway, CloudNativePG, Traefik, …) should go to their respective projects. We verify upstream advisories, track runtime Trivy drift and upstream releases, then ship a reviewed pin-bump patch with an adopter upgrade note. Issue [#39](https://github.com/fmind-ai/fgentic/issues/39) is the worked example for rejecting an unverifiable GHSA premise and a prerelease instead of manufacturing an unsafe upgrade target.

## Security model

The stable trust-boundary summary is in [docs/security.md](docs/security.md), with the assets, actors, STRIDE analysis, control evidence, and residual risks in the [full threat model](docs/security/threat-model.md). The [security and auditor dossier](docs/security-whitepaper.md) maps those concrete controls and limitations to the OWASP Agentic and LLM Top 10. The [delegation attribution runbook](docs/audit.md) states exactly what the Matrix → bridge → kagent → agentgateway/Prometheus evidence chain can and cannot prove. Known, deliberately accepted limits — including the explicit [prompt-injection limits](docs/security/prompt-injection.md), unauthenticated kagent behind layered gateway/NetworkPolicy controls, unencrypted agent rooms, and organization-level federation identity — are stated there rather than hidden; reports that materially change those assessments are very welcome.
