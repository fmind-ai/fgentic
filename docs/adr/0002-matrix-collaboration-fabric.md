---
type: Architecture Decision Record
title: Matrix as the Human↔Agent Collaboration Fabric
description: Use federated Matrix rooms as the shared surface where humans and agents collaborate.
---

# 0002 — Matrix as the Human↔Agent Collaboration Fabric

Status: Accepted

## Context

Given the open-standard mandate ([ADR 0001](0001-open-standard-agent-platform.md)), the platform needs a chat fabric where **humans and agents are first-class co-members** of the same rooms and `@mention` is the delegation primitive. The fabric must supply: real, federatable identities for agents (`@agent-k8s:fgentic.fmind.ai`), a mature multi-client ecosystem (web + native mobile), typed mentions, and — decisively — a path to **external-network interoperability** without a rebuild.

Alternatives considered and rejected:

1. **Proprietary Slack / Discord / Teams.** Best-in-class UX and zero ops, but SaaS lock-in, no self-hosting/sovereignty, and no open identity model for agent members — fails [ADR 0001](0001-open-standard-agent-platform.md)'s core mandate.
1. **Mattermost.** Go, self-hostable, genuinely credible — but no sovereign end-to-end-encryption story and a far weaker bridge ecosystem; external-network interop would be bespoke per network.
1. **Zulip / Rocket.Chat.** Heavier stacks (Python+DB / Node), threading models that don't map cleanly onto fire-and-forget delegation, and again no comparable bridge fabric.
1. **A bespoke Go web chat UI.** Total control, but throws away the entire client ecosystem (Element Web + Element X mobile), interoperability, and identity federation — reinventing a decade of Matrix for a strictly worse result.

## Decision

Adopt **Matrix** as the collaboration fabric and UI. Each agent is a first-class room member with its own Matrix identity; a human (or another agent) delegates by addressing `@agent-name`, which populates the typed `m.mentions.user_ids` field (MSC3952) that the bridge keys off ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)). The homeserver, auth, and clients are supplied by Synapse + MAS + Element ([ADR 0003](0003-synapse-mas-element-ess.md)).

The **interop payoff** is structural, not bolted on: because a Matrix room is network-agnostic, the mature **mautrix** bridge ecosystem (Slack/Telegram/Signal/WhatsApp/…) later connects the same rooms to external networks as additional appservices — a config addition, not a rebuild.

## Consequences

1. `@mention` becomes the universal, already-understood delegation gesture — no bespoke command syntax to teach.
1. Agents get durable, federatable identities and full room semantics (replies, threads, membership) for free.
1. The bridge ecosystem is the strategic asset: "chat with my agents" upgrades to "chat with my agents _and_ anyone on any network."
1. Matrix's operational weight (a homeserver + auth) is inherited — mitigated by ESS packaging ([ADR 0003](0003-synapse-mas-element-ess.md)).
1. Matrix's E2EE complexity is deferred by keeping agent rooms unencrypted ([ADR 0008](0008-unencrypted-agent-rooms.md)).
