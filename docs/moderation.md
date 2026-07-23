---
type: Runbook
title: Moderation Stack
description: Enable the opt-in Draupnir policy-list moderation bot, seed the shared policy room, order power levels safely, and route reported events to review.
---

# Moderation stack

Federated agent rooms need moderation that spans homeservers. Matrix provides the primitives: **policy lists** (MSC2313 — `m.policy.rule.*` state in a policy room, shareable across organizations) and **policy servers** (MSC4284). **Draupnir** — the active Mjolnir successor — is the enforcement bot that consumes those policy lists and applies bans, redactions, and `m.room.server_acl` events across every room it protects.

Fgentic ships Draupnir as an **opt-in, disabled-by-default** component under `infra/moderation/`. It is never referenced by the base Flux DAG; a cluster reconciles it only by composing the `infra/moderation/cluster` component (mirroring the opt-in bridge and admin-console conventions). Enforcement of a _shared_ policy list across two homeservers — the "moderation policy federation" differentiator — is validated in the federation lab (issue #136 Task 3); this runbook covers the offline component and the operational workflow.

## What the offline component provides

The component (`infra/moderation/`) renders, in a restricted `moderation` namespace:

1. A single Draupnir Deployment, image referenced upstream at an immutable digest (`ghcr.io/the-draupnir-project/draupnir`), pinned by digest, run non-root with a read-only root filesystem, all capabilities dropped, and `RuntimeDefault` seccomp.
1. A default-deny NetworkPolicy: egress only to ESS Synapse (client-server API) and cluster DNS; ingress only from the Matrix namespace for the opt-in antispam webhook. There is deliberately no database egress.
1. A ConfigMap holding Draupnir's config; the bot's Matrix access token is supplied only through `--access-token-path` from a SOPS Secret, never stored in the ConfigMap.
1. A small namespace quota and limit range.

Draupnir bot mode keeps its state in Matrix account data plus a local SQLite room-state backing store; it has **no PostgreSQL dependency** (verified against the v3.1.0 default config). This component therefore adds no CloudNativePG role or database — a deliberate deviation from the per-service database convention that only applies to workloads that actually use Postgres. The authoritative policy data lives server-side in Matrix policy rooms, so the SQLite store is a startup-speed cache that re-syncs from the homeserver and is safe to lose.

The offline gate `mise run check:moderation` proves all of the above and proves the default `base`, `local`, `gcp`, `demo`, and `federation` renders contain zero moderation resources.

## Provision the moderation bot account

Draupnir logs in as an ordinary Matrix user, `@moderation-bot`, provisioned with a MAS registration token (the same mechanism used for platform service accounts).

1. Mint a MAS registration token and register `@moderation-bot`, then log it in once and copy its access token. See the `matrix-agents` skill for the MAS registration-token flow.
1. Generate the moderation SOPS secret and replace the placeholder access token with the real one:

   ```bash
   FGENTIC_SECRET_SET=moderation scripts/gen-secrets.sh <server_name> <local|gcp>
   sops clusters/<env>/secrets/draupnir.sops.yaml   # paste the real access-token
   ```

1. Compose the component into the target cluster overlay by adding `../../infra/moderation/cluster` to that overlay's `components:` list, then let Flux reconcile.

## Seed the shared policy room and protect rooms

1. Create the management room `#fgentic-moderation:<server_name>` — invite-only and human-operators-only. Everyone in it fully controls the bot, so treat membership as an admin boundary. Invite `@moderation-bot` and confirm it reports "Now monitoring rooms".
1. Create the shared policy list room `#fgentic-policy:<server_name>` (this is the MSC2313 policy list). From the management room, have Draupnir watch and, for cross-org sharing, create/own it:

   ```text
   !draupnir list create fgentic-policy #fgentic-policy:<server_name>
   !draupnir watch #fgentic-policy:<server_name>
   ```

1. Add each demo/agent room to Draupnir's protected set explicitly — `protectAllJoinedRooms` is off by design so a watched list cannot silently pull unrelated rooms under enforcement:

   ```text
   !draupnir rooms add #agents:<server_name>
   ```

1. In a federated setup, both organizations subscribe to the same `#fgentic-policy` room; a ban issued on one side propagates as shared policy state (validated in the federation lab, not by this offline component).

## Power-level ordering (do not outrank the bridge)

The moderation bot and the platform bridge bot both hold power in agent rooms, and their ordering is a security invariant:

- `@a2a-bridge:<server_name>` owns the appservice ghosts and must retain the higher power level (it manages `@agent-*` membership and profile state).
- `@moderation-bot:<server_name>` needs enough power to ban/redact/kick offenders but **must be granted a strictly lower power level than `@a2a-bridge`**. A moderation bot that outranks the bridge could evict the bridge or its ghosts and break every agent in the room.

Matrix power levels are per-room state set when you invite and promote the bot; there is no Draupnir config knob for its own level, so this ordering is enforced operationally when adding the bot to each room (grant, for example, moderator `50` to `@moderation-bot` while `@a2a-bridge` holds `100`). Draupnir's `admin.enableMakeRoomAdminCommand` is kept `false` so the bot can never self-escalate by taking over a local account.

## Reported-events review workflow

An end user reporting abuse in Element reaches review through one of two surfaces:

1. **Draupnir** intercepts or polls Matrix abuse reports and prints readable reports into the `#fgentic-moderation` management room, where an operator issues a ban/redact command. Draupnir's web `abuseReporting` and `pollReports` options are off by default; enable the one that fits the deployment's reverse-proxy topology.
1. **Ketesa** (the opt-in administrator console — see [Administrator console](admin-console.md)) surfaces server-side reports and account/room actions to an operator authenticated through MAS.

Abuse of _agents_ — spam `@mention` invocations that each trigger an LLM call — is governed separately by the bridge's per-sender/per-room rate limits and agentgateway token metering (D7/D8, see [Design decisions](design-decisions.md)). Moderation of _content_ (Draupnir) and throttling of _invocation_ (D7 budgets) are complementary controls: a user who is banned via policy list can no longer invoke agents, and a user who stays within content policy is still bounded by their invocation budget.

## Optional: server-side antispam

The `infra/moderation/antispam` component wires maunium's `synapse-http-antispam` module (MIT) into ESS through Synapse `additional` config, letting Synapse consult Draupnir before delivering invites/joins/events. It is an additional opt-in on top of the moderation component and has two prerequisites, documented so they are not assumed:

1. The `synapse_http_antispam` module must be importable by Synapse. ESS ships the unmodified upstream Synapse image without it, so the module is referenced upstream and never vendored into this repository; the operator supplies it on Synapse's Python path.
1. Draupnir's `web` listener and a shared `authorization` token must be enabled on the bot side, with the same token delivered to Synapse through the `draupnir-antispam-module` SOPS Secret (the module fragment is never committed in plaintext).

## Licensing boundary

Draupnir is AFL-3.0 + Apache-2.0 and `synapse-http-antispam` is MIT. Both are deployed as standalone services referenced upstream at immutable digests; neither is vendored into the bridge or project code, and neither image is mirrored into a project registry. See [Licensing](licensing.md) §10.2.

## Runtime remainder (deferred)

Two parts of issue #136 require a running cluster and are not covered by the offline component:

1. **Ban propagation across homeservers** (Task 3): both federation participants subscribe to `#fgentic-policy`; a ban on org A removes/blocks the user on org B's view of the shared room while denied server C stays excluded. This extends the federation lab acceptance and needs `mise run fed:up`.
1. **Policy-server evaluation** (Task 4): evaluating `matrix-org/policyserv` (Apache-2.0) as the room policy server, recording latency/availability trade-offs before any production recommendation.
