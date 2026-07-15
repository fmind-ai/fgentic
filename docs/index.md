---
okf_version: "0.1"
---

# Specifications

The topic specs split from the retired root `SPEC.md`; `§N` numbering is preserved (mapping in [.agents/AGENTS.md](../.agents/AGENTS.md)).

- [Architecture & Vision](architecture.md) - what Fgentic is, why it matters, and the end-to-end architecture (§1–§3, §11)
- [Open Agentic Stack](open-stack.md) - governance, protocol boundaries, and current reuse across the independently stewarded layers
- [Design Decisions D1–D18](design-decisions.md) - the durable register of settled decisions with evidence (§4)
- [Bridge Specification](bridge.md) - behavioral contract of the Matrix-to-A2A bridge appservice (§5, §6, §12)
- [Security Boundaries & Threat Model](security.md) - trust zones and the controls that hold them (§7)
- [Federation Spec](federation.md) - Matrix collaboration plane + A2A delegation plane across organizations (§8)
- [Fediverse Interop Spec](fediverse.md) - ActivityPub as a second, additive cross-org transport with every M8 control mapped to an AP twin (M18)
- [Observability Spec](observability.md) - metrics, traces, dashboards, and the LLM token-burn alert (§9)
- [Licensing & Foundation Strategy](licensing.md) - Apache-2.0 rationale, AGPL boundaries, homeserver triggers (§10)
- [Roadmap](roadmap.md) - phase history and the mapping to GitHub milestones M0–M24 (§13)
- [Forking & Adapting](forking.md) - the checklist to run Fgentic under your own org, domain, GCP project, and registry
- [Exit Strategy](exit-strategy.md) - per-layer replacement targets, migration boundaries, one-way doors, and required exit evidence
- [Identity and SSO](identity.md) - MAS as the Matrix OIDC authority, Keycloak as the reference upstream IdP
- [Model Provider Profiles](models.md) - per-cluster model backends behind agentgateway (D16)
- [Sovereign Grounding Store](grounding.md) - the composed CNPG + pgvector schema, ACL metadata, and exact-ranking contract (D18)
- [External-Network Interop](interop.md) - opt-in Slack/Telegram bridges as governed identity boundaries
- [Public Surface Stability Contract](stability.md) - stability tiers for surfaces partners pin

# Runbooks & evidence

- [Production Installation](production.md) - Flux-reconciled production path with SOPS secrets and acceptance gates
- [Partner Federation Onboarding](federation-onboarding.md) - bilateral checklist to federate with a partner org
- [Partner Federation Offboarding](federation-offboarding.md) - bilateral checklist to revoke a partner across every plane
- [Delegation Attribution Audit](audit.md) - proving who invoked which agent, when, over which model path
- [Slack Interop Walkthrough](interop-slack.md) - enabling the opt-in mautrix-slack unit
- [Bridge Performance Evidence](performance.md) - dated load-sanity evidence for §12.5

# Security deep dives

- [security/](security/) - threat model, prompt-injection controls, supply-chain verification

# Architecture Decision Records

- [adr/](adr/) - settled designs are revisited by proposing a new ADR, never a drive-by PR
