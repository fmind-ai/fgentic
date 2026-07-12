---
type: Architecture Decision Record
title: ActivityPub as a Second Federation Transport
description: Propose ActivityPub as an additive second cross-org transport alongside Matrix, delivered as one self-contained app that never couples the mautrix bridge.
---

# 0014 — ActivityPub as a Second Federation Transport

Status: Proposed

## Context

Matrix federation is Fgentic's decided cross-organization destination ([0002](0002-matrix-collaboration-fabric.md), [federation spec §8](../federation.md), [D16](../design-decisions.md) on per-cluster sovereignty). That decision carried an implicit corollary — **Matrix is the sole federation transport** — which was never stated as an ADR and is now worth making explicit so it can be revisited on the record.

The standing federation rule is "never assume a single homeserver forever" ([0008](0008-unencrypted-agent-rooms.md) closes on exactly this obligation for federated rooms). By the same first principle, a sovereignty-first platform must not assume a single federation _protocol_ forever. The Fediverse — Mastodon, GoToSocial, and roughly ten million actors — speaks **ActivityPub**, a W3C Recommendation and an open standard with no license concern. Reaching it lets sovereign agents collaborate beyond Matrix homeservers without introducing any proprietary SaaS into the critical path ([0001](0001-open-standard-agent-platform.md)).

The forces:

1. **Reach vs. focus.** ActivityPub is a large, already-federated network of humans and services. Matrix remains the richer collaboration fabric (shared rooms, threading, appservice API). AP is additive reach, not a replacement for the room-based collaboration plane.
1. **Governance parity is non-negotiable.** M8 spent its whole budget proving that cross-org exposure is safe _before_ it is public: closed-federation allowlists, a git-reloadable signed policy border, object integrity, pinned identity, and per-consumer token-budget admission ([federation spec §8.2–§8.3](../federation.md), [0013](0013-federation-lab-acceptance-rig.md)). Opening a second transport with weaker controls would silently widen the attack surface. Every M8 control must have a proven AP twin.
1. **License and portability risk.** The obvious-but-wrong shortcut is to teach the existing mautrix bridge to speak ActivityPub. That would couple AP surface to the bridge's lifecycle, risk pulling AGPL-class dependencies into a component we keep MPL/Apache-clean ([licensing spec §10](../licensing.md), [0012](0012-bridge-decomposition-surface-budget.md) caps what the bridge core may grow), and break the deliberate homeserver-portability of a bridge that only uses stable-spec appservice endpoints.
1. **Credential containment.** No agent may hold a model credential ([0006](0006-agentgateway-chokepoint.md)). Any AP surface must reach kagent the same governed way every other caller does — through agentgateway over A2A.

The trade-off: adopt ActivityPub as a second transport now, at the cost of a new app and a full second governance rig — or defer, keeping the surface minimal but leaving the wider Fediverse unreachable.

## Decision

Adopt **ActivityPub as a second, additive federation transport**, subject to the governance and structural constraints below. This ADR is the _design gate_; it authorizes the sweep, it does not merge implementation.

1. **Additive, never a replacement.** Matrix federation remains the primary cross-org collaboration plane. AP is a parallel transport for reaching Fediverse actors. Nothing merged may assume AP is the only federation protocol, and nothing may degrade the Matrix path. This makes the previously-implicit "Matrix as the sole federation transport" position explicit and **superseded** by an explicitly dual-transport position.

1. **One self-contained app, never the bridge.** All ActivityPub surface lives in a new app, `apps/activitypub-agent-gateway/`, with its own Go module, Dockerfile, Helm chart, and `deploy/` Flux unit — self-contained exactly like `apps/matrix-a2a-bridge/` and `apps/synapse-federation-policy/`. It reuses the `a2a-go` client ([0004](0004-a2a-delegation.md)) to reach kagent through agentgateway. It is **not** bundled into the mautrix bridge, keeping the bridge AGPL-free and homeserver-portable.

1. **Governance-first sweep, exposure last.** The M18 sweep lands controls before reach: the design gate (this ADR + [fediverse spec](../fediverse.md)), then the core Service actor, then the signing/allowlist border, object integrity, per-actor budget admission, and honest bot/attribution audit — all proven fail-closed **before** any public discovery, group-actor collaboration, identity binding, status feeds, or handle resolution is exposed.

1. **Every M8 control maps to an AP twin.** The allowlist, signed border, integrity proof, pinned identity, and budget reservation each have a documented ActivityPub equivalent (the mapping table lives in [the fediverse spec](../fediverse.md)); no AP feature ships without its twin control.

1. **Adoption is a human gate.** This ADR is **Proposed**. It is promoted to **Accepted** only by explicit maintainer review of the design, consistent with how M8's own exposure was gated. The AGPL Kazarma human↔human bridge (issue #221) is separately human-gated on its license and account posture.

## Consequences

1. **A second full governance rig is now owed.** AP reach is not free: the sweep must reproduce the M8 controls in AP terms and prove them fail-closed, or the transport stays unexposed. The fediverse spec's mapping table is the checklist; an AP feature without its twin control is not merge-eligible.
1. **The bridge stays clean.** Because AP surface is a separate app, the mautrix bridge keeps its surface budget ([0012](0012-bridge-decomposition-surface-budget.md)), its license hygiene, and its homeserver portability. The cost is a second deployable to build, scan, sign, and operate.
1. **Two transports to keep honest.** Cross-org claims must now name their transport. As with Matrix federation, AP object replication and deletion are best-effort across instances; data residency remains a **contractual** control, not a technical one — stated plainly per the federation spec's honesty rule.
1. **Explicit, revisitable, and reversible.** If ActivityPub adoption proves unjustified, the app is removed without touching the Matrix path or the bridge — the isolation that protects the bridge also bounds the blast radius of walking this back. Recorded here so the escape hatch is known.
1. **Supersedes an implicit decision, not an ADR.** No prior ADR asserted a single federation transport; this record names and closes that gap. [0002](0002-matrix-collaboration-fabric.md) and [0008](0008-unencrypted-agent-rooms.md) stand — Matrix remains the collaboration fabric and the federated-room encryption obligation is unchanged.
