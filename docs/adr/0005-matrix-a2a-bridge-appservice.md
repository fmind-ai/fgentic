---
type: Architecture Decision Record
title: Bridge as a mautrix/go Appservice
description: Build the Matrix↔A2A bridge as a plain mautrix/go appservice, not bridgev2.
---

# 0005 — The Matrix↔A2A Bridge is a `mautrix/go` Appservice (not bridgev2)

Status: Accepted

## Context

The one novel component is the **bridge** that stitches Matrix to A2A. mautrix/go (`v0.28.1`, import `maunium.net/go/mautrix`; the framework behind Beeper and every mautrix bridge) offers two framings, and the choice is load-bearing:

1. **`bridgev2`** — models a _foreign network mirrored into Matrix portals_ (logins, portals, remote-user puppets). Purpose-built for "bring Slack/Telegram _into_ Matrix."
1. **The plain `appservice` package** — namespace registration, event transport, and multi-ghost puppeting, with no assumption of a foreign network.

For **native human+agent rooms there is no foreign network to portal** — the agents live inside this platform. Choosing `bridgev2` would force inventing portals and logins for a network that does not exist: the wrong shape.

## Decision

Build the bridge as a single Go binary on the **`mautrix/go` `appservice` package** (`v0.28.1`), **not** `bridgev2`. It:

1. Owns the **exclusive `@agent-.*` ghost namespace** plus the `@a2a-bridge:fgentic.fmind.ai` bot, declared in the appservice `registration.yaml`.
1. Receives events via `PUT /_matrix/app/v1/transactions`; the `EventProcessor` dispatches `event.EventMessage`, reads `evt.Content.AsMessage().Mentions.UserIDs`, and matches the `@agent-.*` regex — with a **plaintext-body fallback** for clients that omit `m.mentions`.
1. Maps `@agent-k8s → (namespace=kagent, name=k8s-agent)` via an **allowlist** (this is the who-may-invoke-which-agent authorization), optionally validating the target's AgentCard, then calls A2A `message/send` ([ADR 0004](0004-a2a-delegation.md)) — routed through agentgateway ([ADR 0006](0006-agentgateway-chokepoint.md)).
1. Posts the reply **as the ghost** via `as.Intent(@agent-k8s)` → `EnsureRegistered` → `EnsureJoined` → send with a typed `m.mentions` and an `m.relates_to` reply pointing at the original event (multi-ghost puppeting).
1. Backs its **StateStore with the shared Postgres `bridge` database** ([ADR 0007](0007-shared-postgres-db-per-service.md)) so ghost registrations survive pod restarts.

Off-the-shelf `bridgev2` bridges are reserved for the **external-network interop** phase (Slack/Telegram/… as separate appservices).

## Consequences

1. The code owned is small and honest: a registration file, an `@agent → (namespace,name)` map, a `SendMessageResult → string` extractor, and the reply wiring — everything else is mautrix/go and a2a-go.
1. A push-based Go appservice idles at ~15–40 MiB and runs no per-ghost `/sync` — negligible footprint.
1. Typed `m.mentions` (MSC3952) is the primary trigger; the body fallback keeps non-conforming clients working.
1. Multi-turn context is preserved by storing `roomID → contextId` for the A2A call.
1. Adding external-network interop later does **not** touch this bridge — those are independent `bridgev2` appservices with disjoint namespaces (e.g. `@telegram_.*`).
