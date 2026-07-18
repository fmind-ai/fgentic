---
type: Runbook
title: Security Release Process
description: Maintainer workflow from private report through a signed patch release, GHSA and CVE publication, and adopter notification.
---

# Security Release Process

This runbook completes the outbound half of [SECURITY.md](../../SECURITY.md): a confirmed vulnerability becomes a minimal, signed patch release and a coordinated GitHub Security Advisory (GHSA). It does not replace the normal release contract established by [#7](https://github.com/fmind-ai/fgentic/issues/7) in [CONTRIBUTING.md](../../CONTRIBUTING.md#releases), the signed artifact evidence in [Bridge Supply-Chain Verification](supply-chain.md), or the adopter release and upgrade contract owned by [#188](https://github.com/fmind-ai/fgentic/issues/188).

## Invariants

1. Keep the report, reproducer, impact analysis, fix discussion, and embargo date inside the private GHSA. Public issues, ordinary branches, pull requests, CI logs, and Matrix rooms are not disclosure channels.
1. Never request a CVE for an unconfirmed or rehearsal-only report. Never publish a rehearsal advisory.
1. Prefer a fixed advisory: GitHub warns that publishing without a fixed version leaves Dependabot unable to recommend a safe upgrade.
1. Published tags are immutable. Correct a failed release with a new patch version; never move or replace a tag.
1. Security artifacts use the normal D13 path. The bridge image and chart are scanned, attested, keyless-signed, and published by `.github/workflows/cd.yml`. Tag CD produces release artifacts; only an eligible bridge/chart/CD change on `main` commits bridge deployment digests, and Flux accepts that `main` workflow identity. A source-only manifest or composed-component pin fix instead proves its reviewed immutable source pin. Treat those as separate evidence surfaces.
1. A reservation, planned version, passing local check, or draft release is not proof that the fixed artifact exists. Verify the public tag, image, chart, signatures, attestations, SBOM, and release notes before publication.

## Report to triage

The security lead owns the GHSA and timeline. A maintainer may implement the fix, but no one copies confidential details into a normal work queue.

1. Acknowledge receipt within the 7-day commitment in [SECURITY.md](../../SECURITY.md). Confirm the reporter's preferred private channel and credit preference.
1. Create or accept a private GHSA. Do not create a temporary private fork until code collaboration requires it.
1. Reproduce the report against an identified revision. Record affected components, attack preconditions, impact, and evidence without secrets or production personal data.
1. Within 30 days, record one outcome: confirmed with a remediation plan, rejected with evidence, duplicate, upstream-owned, or blocked on requested reporter information.
1. For a confirmed vulnerability, populate:
   - the exact package/component and affected version range;
   - the planned fixed version;
   - CWE identifiers;
   - a CVSS 3.1 or 4.0 vector and calculated severity, not an unsupported label;
   - reporter/finder, remediation developer, reviewer, and verifier credits after consent;
   - an embargo target, review checkpoints, and early-disclosure triggers.
1. Agree on coordinated publication. Revisit the target if exploitation is active, details become public, users cannot mitigate material harm, or a legal/regulatory obligation applies. Record the reason for any change in the GHSA.

## Fix and patch release

The out-of-band patch is based on the latest supported release rather than absorbing unrelated features from `main`.

1. Start from the latest supported annotated tag in the GHSA temporary private fork or another access-controlled checkout.
1. Apply only the vulnerability fix, deterministic regression coverage, and required release metadata. If a release-tooling correction is essential to run the current signing path, backport it as a separately explained commit; do not silently include feature work.
1. Run focused checks, then the same warning-free `check` and `test` gates used by hooks and CI. Obtain a second review inside the private advisory.
1. Set the next SemVer patch version. Generate the git-cliff changelog, set the bridge chart `version` and `appVersion` to the same value, and write the upgrade note required by [#188](https://github.com/fmind-ai/fgentic/issues/188). Until that file convention lands, use an explicit upgrade section in the GitHub Release notes. Name configuration, values, secret, and manual-step changes; say `none` where appropriate.
1. Prepare the GitHub Release notes and GHSA before the disclosure window. The release notes must include the fixed tag, affected range, upgrade action, artifact verification links, and advisory reference without revealing embargoed details early.
1. In the coordinated window, publish the reviewed fix-only branch and create its annotated patch tag. Land the same fix on `main`. If it changes the bridge image, chart, or CD workflow, wait for the `main` CD digest-pin commit and verify the bridge deployment manifest separately. If it changes only manifests or composed-component pins, verify those immutable source pins and record that the bridge digest did not need to move. If `main` contains unrelated unreleased work, its deployment state is not proof that the fix-only tag is a complete platform BOM.
1. Wait for tag CD to publish and verify the release image and chart. Confirm Trivy, provenance, OCI SBOM attestations, and Cosign signatures. Record the tag-produced immutable digests in the release notes/BOM. The tag workflow runs from the tag commit, so required CD fixes must already be in that commit; [#324](https://github.com/fmind-ai/fgentic/issues/324) documents this boundary.
1. Publish the GitHub Release, then wait for the separate Release-event CD job to attach its downloadable SPDX SBOM. Re-read the public artifact digests, Release asset, and release note before recording the fixed version in the GHSA.

The exact tag commands and SemVer rules remain in [CONTRIBUTING.md](../../CONTRIBUTING.md#releases). This process adds confidentiality and coordinated publication; it does not create a side-channel hotfix image. CD deliberately does not rewrite an immutable release tag with its own output digests. Until [#188](https://github.com/fmind-ai/fgentic/issues/188) publishes and exercises the tag-to-tag BOM/upgrade contract, do not claim that a fix-only tag is itself a proven full-platform Flux upgrade source; state the verified image/chart artifacts and the separate `main` deployment pin precisely.

## Advisory and CVE publication

GitHub requires a draft advisory with affected-version metadata before a CVE can be requested. For a confirmed project vulnerability:

1. Recheck the title, description, affected/fixed ranges, package ecosystem/name, CWE, CVSS vector, workarounds, credits, and references against the released artifacts.
1. Request a CVE through the draft GHSA when the vulnerability originates in this repository and no identifier already exists. Do not request a second CVE for an upstream component or a report already assigned elsewhere.
1. Add the returned CVE to the release notes when timing permits. The GHSA remains the canonical record if assignment is still pending at the coordinated release time.
1. Publish the advisory only after the fixed tag is available. Link the GitHub Release and immutable tag; name any required operator action and the earliest safe version.
1. Verify the public GHSA and GitHub Release from a logged-out or unaffiliated view. Confirm both surfaces agree on ranges, severity, fix version, and credit.
1. Notify directly coordinated adopters through their agreed security contacts when applicable. The public canonical subscriptions remain [Security Advisories](https://github.com/fmind-ai/fgentic/security/advisories) and [Releases](https://github.com/fmind-ai/fgentic/releases).

If a published record is materially wrong, edit the GHSA and release notes with a dated correction. Do not mutate the tag or overwrite an artifact.

## Reusable GHSA template

Copy this structure into a private draft and replace every bracketed value. Delete headings that are genuinely inapplicable; do not leave placeholders in a published advisory.

```markdown
## Summary

[One sentence naming the affected boundary and impact.]

## Impact

- Affected component: [package, image, chart, or composed pin]
- Affected versions: [exact range]
- Preconditions: [authentication, network, configuration]
- Confidentiality / integrity / availability impact: [evidence]

## Severity

- CVSS vector and score: [vector] / [score]
- CWE: [identifier and name]
- Rationale: [why each material metric applies]

## Patches

- Fixed version: [vX.Y.Z]
- Fixed tag: [immutable tag URL]
- Release notes: [GitHub Release URL]
- Image/chart verification: [digest and supply-chain evidence]

## Workarounds

[Bounded mitigation, or "None; upgrade to the fixed version."]

## Detection and response

[Observable evidence, containment, credential/key rotation, or data review.]

## Credits

[Reporter/finder and remediation credits, with consent.]

## Timeline

- <UTC timestamp>: report received
- <UTC timestamp>: acknowledgment
- <UTC timestamp>: triage outcome
- <UTC timestamp>: fix verified
- <UTC timestamp>: patch and advisory published

## References

- [Fixed release]
- [Relevant security/control documentation]
```

Before publication, the GHSA metadata outside the description must also carry the affected product, affected and patched ranges, CVSS vector/severity, CWE, CVE choice, and accepted credits.

## Composed upstream vulnerabilities

Fgentic does not assign a second CVE for Synapse, MAS, Element, kagent, agentgateway, CloudNativePG, Traefik, or another upstream project.

1. Trigger triage from a verified upstream advisory/release, Dependabot or Trivy finding, or the runtime image-vulnerability drift alert in [Production Installation](../production.md). Resolve the package, image, or chart digest actually deployed; a name match alone is not impact proof.
1. Confirm the advisory at its authoritative upstream source, affected range, exploitability in the composed Fgentic configuration, fixed stable version, and any coupled pins. Do not turn an unresolvable GHSA or prerelease into a manufactured emergency upgrade.
1. Open a normal reviewed pin-bump only after confidential details are public upstream. Run the focused compatibility and security gates for the affected component and its coupled set.
1. Ship the bump as an out-of-band patch when deployed users are affected. Add an upgrade note under the [#188](https://github.com/fmind-ai/fgentic/issues/188) convention—or the GitHub Release fallback above—link the upstream advisory, and state whether configuration or manual remediation is required.
1. Notify through the Fgentic GitHub Release. A project GHSA is reserved for a Fgentic-originated vulnerability or material Fgentic-specific exposure that needs its own coordinated record.

[Issue #39](https://github.com/fmind-ai/fgentic/issues/39) is the worked negative example: the cited GHSA returned 404, no upstream fix was verifiable, and the only newer agentgateway build was a forbidden prerelease. The correct response was to keep the known stable pin and wait for an executable, authoritative target.

## Draft-only rehearsal evidence

On 2026-07-18, the maintainer workflow was rehearsed against a clearly marked hypothetical bridge authorization bypass. No vulnerability was discovered, no code was changed, and no version was actually affected.

| Evidence              | Recorded value                                                        |
| --------------------- | --------------------------------------------------------------------- |
| Private advisory      | `GHSA-27pj-56w8-4f49`                                                 |
| Draft created         | `2026-07-18T21:03:38Z`                                                |
| Draft closed          | `2026-07-18T21:03:57Z`                                                |
| State transition      | `draft` → `closed`                                                    |
| Publication           | `published_at: null`                                                  |
| CVE                   | none requested; `cve_id: null`                                        |
| Private fork          | none created                                                          |
| Hypothetical product  | Go module `github.com/fmind-ai/matrix-a2a-bridge`                     |
| Hypothetical versions | affected `<= 0.1.0`; patched `0.1.1`                                  |
| Classification        | CWE-862; CVSS 3.1 `AV:N/AC:L/PR:L/UI:N/S:U/C:L/I:L/A:N` (Medium, 5.4) |
| Credits               | none invited, avoiding rehearsal notifications                        |

The exercise proved that repository-advisory permissions can create, read back, and close a fully populated private draft without publishing it, requesting a CVE, or creating a private fork. A real incident must still exercise private collaboration, the patch tag, signed CD artifacts, publication, and adopter notification; this synthetic record does not claim those outcomes.

## Authoritative references

- [GitHub: create a repository security advisory](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/fix-reported-vulnerabilities/creating-a-repository-security-advisory)
- [GitHub: write security advisories](https://docs.github.com/en/code-security/tutorials/fix-reported-vulnerabilities/write-security-advisories)
- [GitHub: publish an advisory and request a CVE](https://docs.github.com/en/code-security/how-tos/report-and-fix-vulnerabilities/fix-reported-vulnerabilities/publish-repository-advisory)
- [GitHub REST API: repository security advisories](https://docs.github.com/en/rest/security-advisories/repository-advisories)
- [CVSS v3.1 specification](https://www.first.org/cvss/v3-1/specification-document)
- [CVSS v4.0 specification](https://www.first.org/cvss/v4-0/specification-document)
