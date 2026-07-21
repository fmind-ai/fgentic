---
type: Reference
title: External Security Audit Readiness Package
description: Scope, trust boundaries, evidence pointers, known limits, engagement options, and remediation workflow so an external auditor can start without a walkthrough call (G3 trust proof).
---

# External Security Audit Readiness Package

Focus-board gate **G3 — trust proof** distinguishes a self-assessment from an **independent third-party audit**: regulated and public-sector procurement reviews weigh the latter, and the CNCF path (G4) considers one too. The audit itself is a spend + external-counterparty gate (the maintainer selects and funds the firm — see [Engagement options](#engagement-options)); everything up to signing the engagement is prepared here so the paid time goes to unknowns, not orientation. This package builds on — and does not duplicate — the [security whitepaper and auditor dossier](../security-whitepaper.md) (the OWASP Agentic / LLM Top-10 control map) and the [threat model](threat-model.md).

An auditor should be able to scope and begin from this page alone: the boundary, the evidence, the honest limits, and the verification runbooks are all one link away.

## Audit scope

The audit targets the boundary Fgentic **owns and composes** — not the upstream platform it reuses — mirroring [`SECURITY.md`](../../SECURITY.md).

**In scope (the owned surface):**

1. The Matrix↔A2A bridge — [`apps/matrix-a2a-bridge/`](../../apps/matrix-a2a-bridge): delegation admission, sender/server allowlists, the Signed AgentCard verification and rotation logic, rate/budget limits, content-free audit, and the reply-secret/media boundaries ([bridge spec](../bridge.md)).
1. The A2A Signed AgentCard border and the federation delegation plane — the ES256/JCS card verification, the usage-receipt signer/verifier, org-B JWT authorization, and per-`azp` reservation ([federation §8.3](../federation.md)).
1. The Synapse federation-policy callback module — [`apps/synapse-federation-policy/`](../../apps/synapse-federation-policy): the strict policy parser and the fail-closed event/invite border.
1. The ActivityPub agent gateway — [`apps/activitypub-agent-gateway/`](../../apps/activitypub-agent-gateway): the signed inbound/outbound border, SSRF guard, activity dedup, and per-actor budget admission.
1. The partner trust registry, break-glass containment, and signed bilateral agreements — [`infra/federation/registry/`](../../infra/federation/registry) and their fail-closed gates.
1. The cluster admission invariants — [`infra/policies/`](../../infra/policies): the fail-closed `ValidatingAdmissionPolicy` set (approved kagent references, no `:latest`, PSS retention, Service-exposure limits).
1. The CI/CD supply chain — [`.github/workflows/`](../../.github/workflows): the signed digest-pinned image and chart, SLSA + SBOM attestations, and Flux keyless verification ([supply-chain verification](supply-chain.md)).

**Out of scope (upstream components — route findings to their projects):** Synapse, MAS, Element, kagent, agentgateway, CloudNativePG, Traefik, Keycloak, and the pinned MCP servers. Fgentic pins them by immutable digest and constrains them with NetworkPolicy and admission controls (those _constraints_ are in scope), but a vulnerability inside an upstream component goes to that project per [`SECURITY.md`](../../SECURITY.md), not this audit.

## Component inventory and trust boundaries

| Owned component                     | Trust boundary it enforces                                                                                                   | Primary evidence                                                                                   |
| ----------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| `matrix-a2a-bridge`                 | Room content is untrusted input; only allowlisted senders invoke a mapped agent; agent replies are `m.notice` and non-acting | [bridge spec](../bridge.md), [audit chain](../audit.md)                                            |
| Signed AgentCard border             | A remote card is trusted only under a pinned, non-revoked ES256/P-256 key; tamper/revocation fail closed                     | [federation §8.3](../federation.md)                                                                |
| `synapse-federation-policy`         | Only admitted servers/event types cross the callback; unknown/malformed/duplicate/empty policy denies all                    | [federation §8.2](../federation.md)                                                                |
| `activitypub-agent-gateway`         | Signed inbound border, SSRF-guarded key resolution, at-least-once-safe dedup, per-actor budgets                              | [fediverse design](../fediverse.md)                                                                |
| Partner trust registry + agreements | One validated source renders every enforcement plane; signed agreement is the enforcement input; deny-by-default             | [federation §8.2.1](../federation.md)                                                              |
| `infra/policies` admission          | Fail-closed cluster invariants at admission (references, tags, PSS, Service exposure)                                        | [security §7](../security.md)                                                                      |
| CI/CD supply chain                  | Signed, digest-pinned artifacts; Flux verifies the chart before helm-controller                                              | [supply-chain verification](supply-chain.md), [OpenSSF self-assessment](openssf-best-practices.md) |

## Reachable evidence

Everything an auditor needs is linked from here:

1. [Security spec §7](../security.md) — assets, actors, STRIDE analysis, control evidence, residual risk.
1. [Threat model](threat-model.md) — STRIDE evidence map (Implemented / Configured / Deferred per control).
1. [Security whitepaper & auditor dossier](../security-whitepaper.md) — the OWASP Agentic / LLM Top-10 control mapping ([#374](https://github.com/fmind-ai/fgentic/issues/374)).
1. [Prompt-injection controls and limits](prompt-injection.md) — the #1 threat and its honest limits.
1. [Bridge supply-chain verification](supply-chain.md) — independently verifying the signed image, SBOM, and provenance.
1. [OpenSSF Best Practices & Scorecard self-assessment](openssf-best-practices.md) — measurable supply-chain posture.
1. [Audit evidence chain](../audit.md) — the content-free attribution and evidence contracts.
1. [Security release process](release-process.md) — how a finding becomes a signed, published fix.

## Known limits (stated up front)

Auditors should not re-bill to discover what the project already discloses:

1. **Prompt injection is unsolved** by design — the bridge does not silently sanitize or rewrite untrusted room content and makes no prevention claim; the settled stance is no-sanitization with consequential actions decided by humans, and a model's refusal is never read as proof of safety ([prompt-injection controls](prompt-injection.md)).
1. **kagent's A2A endpoint is unauthenticated** by default; the NetworkPolicies on the agentgateway and kagent namespaces are the load-bearing controls, and a D18 retrieval path uses a gateway-projected identity instead ([security §7](../security.md)).
1. **Agent rooms are unencrypted** by policy ([ADR 0008](../adr/0008-unencrypted-agent-rooms.md)); federated real-partner rooms are permitted only within the tightly-scoped, classified, contract-bound surface of [ADR 0015](../adr/0015-federated-room-encryption.md).
1. **Matrix federation replicates room state irrevocably** to every participating homeserver; residency is a contractual control, not a technical one ([federation §8.1](../federation.md)).
1. **A reservation is admission accounting, not measured consumption** (D7/D8); cross-org `maxTokens` is a per-`azp` ceiling, never billed as spend.

## Engagement options

The maintainer decides from this section alone. Each is a numbered trade-off with rough effort and what it buys for G3.

1. **OSTIF-funded audit after CNCF Sandbox acceptance.** The [Open Source Technology Improvement Fund](https://ostif.org/) coordinates and (often) funds audits for CNCF projects. Effort/cost to the project: low direct spend, but gated on Sandbox acceptance (G4) and OSTIF scheduling (months). Buys: a credible, published, community-recognized audit at little cost — the strongest G3 outcome, but the slowest and dependency-gated.
1. **Direct engagement with an EU OSS-experienced firm** (e.g. Radically Open Security, or a Cure53-class firm). Effort/cost: a paid engagement (typically low-to-mid five figures for a scoped, bounded surface like this one), schedulable now. Buys: a fast, independent, publishable report on the owned boundary without waiting on Sandbox; the maintainer controls scope and timing. Strongest fit for the sovereignty/EU positioning.
1. **Scoped community / red-team review as an interim step.** Effort/cost: minimal spend, a bounded volunteer or bug-bounty-style review of the highest-risk surfaces (bridge admission, the federation border, the AP inbound border). Buys: early findings and a credibility down-payment — but it is **not** an independent third-party audit and does not on its own satisfy G3; use it to de-risk before options 1–2.

Prerequisite common to all: the pre-audit sweep below and this readiness package (done here). Options 1 and 2 both require the report to be publishable under the maintainer's name (option 6 in the issue).

## Remediation and publication workflow (pre-agreed)

Fix this before the audit starts so findings flow through an existing channel:

1. **Intake.** Findings arrive via the existing private channel — [GitHub private vulnerability reporting / GHSA](../../SECURITY.md) — never a public issue or PR. The audit firm reports into the same advisory workflow.
1. **Severity.** Each finding gets a CWE and a CVSS vector/severity, an owner, and a proposed fix window, per the [security release process](release-process.md): acknowledge within 7 days, triage within 30.
1. **Fix.** The smallest private fix with a regression test, reviewed across affected/fixed version ranges, then a signed patch release (the CD signing + digest-pin flow).
1. **Publish (post-human-gate).** The full report and a remediation-status table are published under [`docs/security/`](index.md) and linked from [`SECURITY.md`](../../SECURITY.md), the [whitepaper](../security-whitepaper.md), and the focus-board G3 gate. Upstream-component findings route upstream per `SECURITY.md`, not into this report.

## Pre-audit sweep

Close or explicitly accept these open self-known gaps before the engagement so paid time goes to unknowns rather than re-discovering documented residue:

1. [#39](https://github.com/fmind-ai/fgentic/issues/39) — track the next stable agentgateway release (the GHSA-jwm2-83f3-52xc premise is unconfirmed / upstream-gated); accept-with-rationale until an upstream fix ships.
1. [#44](https://github.com/fmind-ai/fgentic/issues/44) — the secrets-rotation runbook + script.
1. [#45](https://github.com/fmind-ai/fgentic/issues/45) — the remaining supply-chain residue (SLSA provenance, SBOM, Flux cosign verification; much is already shipped — see [supply-chain verification](supply-chain.md)).

Each is either resolved or recorded as an accepted deviation with a rationale before the audit, mirroring the [OpenSSF accepted-deviation discipline](openssf-best-practices.md#scorecard-accepted-deviations).

## Terminal human gate

Two steps are the maintainer's and cannot be prepared here: **select and fund the audit firm and sign the engagement** (spend + external counterparty), and **approve publication of the report under the maintainer's name**. After the engagement, publish the report + remediation table, link them from `SECURITY.md` and the whitepaper, and record the G3 evidence on the focus board.
