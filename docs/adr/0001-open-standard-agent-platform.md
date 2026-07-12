---
type: Architecture Decision Record
title: Open-Standard Agent Collaboration Platform
description: Build exclusively on open protocols and OSS components; every layer is swappable.
---

# 0001 — Open-Standard Agent Collaboration Platform

Status: Accepted

## Context

kagent (agents on Kubernetes) and agentgateway (the AI data plane) are strong at _creating and governing_ AI assets — LLMs, MCP tools, A2A agents — but neither ships a good **human↔agent collaboration surface**. The question this project answers: what is the right UI/fabric for humans and agents to co-inhabit, address each other by `@mention`, and delegate work?

The honest 80/20 framing (recorded so nobody has to re-litigate it):

1. For a **single solo developer** who only wants "let me chat with my agents," standing up a full homeserver is over-engineering. kagent already ships Slack/Discord/Telegram→A2A bridge examples that deliver that UX with **zero homeserver** — that is the correct answer for that case, and Matrix is genuinely over-built for it.
1. A **proprietary SaaS** (Slack/Discord/Teams) is the fastest path to a chat UI, but bakes vendor lock-in, foreign data residency, and per-seat cost into the critical path — disqualifying for a _sovereignty_ showcase.

Matrix (and the wider open-standard stack) earns its keep only when three requirements are simultaneously real: **(a) multiple humans**, **(b) self-hosted / sovereign** operation, and **(c) multi-network interoperability**.

## Decision

Build the platform entirely on **open, mature, governed standards**, and treat it as the reference build for exactly the (a)+(b)+(c) case above:

1. **Matrix** as the collaboration fabric + UI ([ADR 0002](0002-matrix-collaboration-fabric.md)) — an IETF-track, foundation-governed protocol already run by NATO, the French and German governments, and Sweden's eSam.
1. **A2A** (Linux Foundation) as the agent-to-agent delegation protocol ([ADR 0004](0004-a2a-delegation.md)).
1. **MCP** (Linux Foundation / Agentic AI Foundation) for agent tool access.
1. **Kubernetes** + **OIDC** (via MAS, [ADR 0003](0003-synapse-mas-element-ess.md)) as the runtime and identity substrate.

There is **no proprietary SaaS in the loop** and every layer is swappable. Unlike the sibling `dev.fmind` (a cost-capped free-tier mini-cloud), Fgentic deliberately does **not** optimise for billing or a single tiny node — it optimises for demonstrating the complete, enterprise-credible pattern, verified against upstream HEAD on 2026-07-08.

## Consequences

1. The platform is a genuine de-risking artifact for enterprises: every hop is currently-shipping OSS with no vendor to be captured by, and every fork chooses the enterprise-credible option.
1. The cost is real operational weight (a homeserver, an auth service, a bridge) that a solo "just chat" use case would not justify — accepted, because that weight _is_ the thing being demonstrated.
1. Open standards buy **interoperability for free later** (Matrix bridges to WhatsApp/Slack/Signal/…), a payoff no proprietary UI can match.
1. Provider independence is a hard constraint: workloads stay plain Kubernetes; only an _optional_ Terraform GKE reference is cloud-specific.
