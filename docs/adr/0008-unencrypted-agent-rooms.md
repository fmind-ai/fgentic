---
type: Architecture Decision Record
title: Agent Rooms Unencrypted, Enforced Server-Side
description: Keep collaboration/agent rooms unencrypted by policy, force-disabled server-side; revisit for federated rooms.
---

# 0008 — Agent Rooms Unencrypted, Enforced Server-Side

Status: Accepted

## Context

Matrix supports end-to-end encryption (E2EE) per room. The bridge ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)) is an appservice with ghost members in the agent/collaboration rooms; if those rooms were encrypted, the bridge would have to participate in crypto to read and post messages. mautrix's appservice-E2EE support exists but is officially **"not recommended"** and is config-heavy (device keys, key backup, cross-signing plumbing). The deployment is private, TLS-terminated at the Gateway, and entirely in-cluster.

The trade-off: does the agent-delegation path need sovereign E2EE, given its cost?

## Decision

Keep **agent/collaboration rooms unencrypted**, force-disabled **server-side and git-declaratively**:

1. `io.element.e2ee.default: false` and `force_disable: true` in `/.well-known/matrix/client`, so clients do not even offer to encrypt these rooms.
1. The bridge **does not wire the mautrix crypto package** — it receives plaintext in unencrypted rooms and posts plaintext replies.

Rationale: a bot/appservice in a room does **not** force crypto (it simply receives plaintext when the room is unencrypted); appservice-E2EE is mautrix-"not recommended" and config-heavy; and for a private, TLS-terminated, in-cluster deployment, unencrypted agent rooms are a defensible trade-off that sidesteps the whole crypto cost cliff.

**Human-only rooms may still be encrypted** — the force-disable targets the agent-delegation rooms specifically; humans who want sovereign E2EE among themselves keep it.

## Consequences

1. The bridge stays small and crypto-free — no device keys, key backup, or cross-signing to operate ([ADR 0005](0005-matrix-a2a-bridge-appservice.md)).
1. Server-side force-disable is explicit, declarative, and auditable in git — not a per-user client setting that can drift.
1. Agent-room message content is protected only by transport TLS + in-cluster NetworkPolicies, not by E2EE — accepted for this deployment posture, and documented plainly rather than hidden.
1. This is an **explicit, revisitable** decision: if sovereign E2EE across agent rooms becomes a hard requirement, the bridge adopts the mautrix crypto package and the force-disable is lifted — a bounded change, recorded here so the escape hatch is known.
