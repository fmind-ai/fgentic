---
type: Specification
title: Fediverse Interop Spec
description: ActivityPub as a second, additive cross-org federation transport, with every Matrix/A2A governance control mapped to a proven ActivityPub twin.
---

# Fediverse Interop Spec — ActivityPub as a second federation transport, milestone M18

Design position: **ActivityPub is a second, additive federation transport** that reaches the wider Fediverse (Mastodon, GoToSocial, ~10M actors), sitting alongside — never replacing — Matrix federation ([federation spec §8](federation.md)). The decision to adopt is gated by [ADR 0014](adr/0014-activitypub-second-federation-transport.md) in **Proposed** status; this spec is its design surface and the checklist for the M18 sweep (issues #209–#221).

The standing rule holds twice over: Fgentic assumes neither a single homeserver nor a single federation _protocol_ forever. AP is open (W3C Recommendation), so it fits the open-standards-only principle with no license concern — but reach is only earned once every M8 governance control has a proven ActivityPub twin (§3).

## §1 — Scope and non-goals

1. **In scope.** A sovereign agent is reachable from the Fediverse by a stable handle; a Fediverse user follows it, `@mentions` it, and receives one governed, signed A2A-backed reply. Discovery (WebFinger, NodeInfo), integrity, identity binding, per-actor budget admission, and honest bot/attribution audit are all in scope as governed surface.
1. **Non-goals.** ActivityPub does not replace Matrix rooms as the collaboration plane; it does not become a general-purpose social server; it does not carry model credentials; and it does not couple into the mautrix bridge. Human↔human Fediverse bridging (Kazarma, #221) is a separate, AGPL-gated, human-approved profile, not part of the agent gateway.

## §2 — Architectural spine

All ActivityPub surface lives in ONE new self-contained app, **`apps/activitypub-agent-gateway/`** — its own Go module, Dockerfile, Helm chart, and `deploy/` Flux unit, exactly like `apps/matrix-a2a-bridge/` and `apps/synapse-federation-policy/`. It:

1. Reuses the `a2a-go` client ([ADR 0004](adr/0004-a2a-delegation.md)) to reach kagent **through agentgateway** — the same egress chokepoint every caller uses, so no agent holds a model credential ([ADR 0006](adr/0006-agentgateway-chokepoint.md)).
1. Is **never** bundled into the mautrix bridge, keeping that bridge AGPL-free and homeserver-portable and inside its surface budget ([ADR 0012](adr/0012-bridge-decomposition-surface-budget.md), [licensing spec §10](licensing.md)).
1. Exposes public AP endpoints only through the Gateway API with TLS, and reaches kagent on `ClusterIP` behind NetworkPolicy — the AP gateway is a caller of agentgateway, never a peer of kagent.

Each exposed agent is presented as an ActivityPub **`Service` actor** (§3, row _bot typing_); a collaboration room may additionally be presented as a **`Group` actor** for cross-org collaboration (#217). The gateway is a translation and governance border between the AP object graph and A2A `message/send`, mirroring how the mautrix bridge translates Matrix events to A2A ([bridge spec §6](bridge.md)).

The dormant `deploy/` unit already bounds its `activitypub` namespace with the shared `small` ResourceQuota and container defaults. Its future Flux Kustomization **must** source `platform-settings` (plus the optional overrides ConfigMap) through post-build substitution before activation; applying the unit directly with unresolved quota variables is unsupported.

## §3 — Control mapping: every M8 control has an ActivityPub twin

No ActivityPub feature ships without the twin control in this table proven fail-closed first. The left column is the settled Matrix/A2A control; the right is its AP equivalent and the issue that lands it.

| Governance control | Matrix / A2A mechanism (settled)                                                                       | ActivityPub twin (M18)                                                                                             | Issue |
| ------------------ | ------------------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------------------------------------------------ | ----- |
| Limited federation | Synapse `federation_domain_whitelist` + `m.room.server_acl` allowlist ([§8.2](federation.md))          | Git-declared instance/actor **allowlist** enforced at the AP inbox border; unlisted origins deny-by-default        | #211  |
| Signed border      | Synapse module callbacks (`should_drop_federated_event`), git-reloadable ([§8.2](federation.md))       | **HTTP Signatures + authorized-fetch** border, policy reloaded from git without replacing the gateway              | #211  |
| Object integrity   | A2A v1.0 **Signed AgentCard** (ES256 / JCS) ([§8.3](federation.md))                                    | **FEP-8b32** object integrity proofs (`eddsa-jcs-2022`, Ed25519) on every agent reply activity                     | #212  |
| Pinned identity    | Pinned **P-256 JWK** per remote agent, verified per call                                               | **FEP-c390** identity proof binding the AP actor to the A2A AgentCard key/DID; keys published, verified per call   | #218  |
| Budget admission   | Per-`azp` `maxTokens` reservation at admission ([D7/D8](design-decisions.md))                          | Per-**actor/domain** token-budget admission before any A2A call; reservation ≠ consumption, metrics stay aggregate | #213  |
| Honest attribution | Asserted `X-User-Id` MXID + bounded origin audit fields ([audit spec](audit.md))                       | **Bot/Service** actor typing + ActivityPub attribution audit fields (actor URI, domain, delivery id)               | #214  |
| Egress containment | agentgateway is the sole model-credential chokepoint ([ADR 0006](adr/0006-agentgateway-chokepoint.md)) | AP gateway calls kagent only via agentgateway/A2A; kagent stays `ClusterIP` behind NetworkPolicy                   | #210  |

Reading the table: the _shape_ of each control is preserved — allowlist deny-by-default, a git-reloadable signed border, per-object integrity, pinned per-caller identity, admission-time budget reservation, and content-free honest audit — expressed in ActivityPub's native primitives (HTTP Signatures, FEPs, actor types) instead of Matrix's.

**Object integrity (#212, delivered).** HTTP Signatures authenticate only the transport _hop_; a relayed or cached activity loses that provenance. The gateway therefore signs every outbound reply with a **FEP-8b32 `DataIntegrityProof`** (`eddsa-jcs-2022`: Ed25519 over the RFC 8785 JCS-canonicalized activity) and publishes each actor's `assertionMethod` **Multikey**, so any remote verifier confirms a sovereign agent authored the reply even after relaying. When `requireInbound` is set, the border _also_ verifies an inbound proof and binds its key controller to the activity actor: a missing, invalid, or mis-bound proof fails closed with content-free evidence and **no A2A call** — untrusted room content cannot be laundered through a trusted actor. The signing key is a SOPS-backed Ed25519 PKCS#8 secret, never committed plaintext. Interop with the **apsig** reference verifier is pinned byte-for-byte by a golden test vector and re-derivable live with `mise run interop`.

**Budget admission (#213, delivered).** Every AP mention that reaches an agent is an LLM invocation, so cost is a correctness constraint (D7/D8): without a ceiling, one remote instance could drive unbounded spend. The border therefore reserves each delegation's token estimate from the **verified** actor's and domain's per-window pools — declared in the same git-reloadable `policy.json` (`budgets`), keyed on the F3/F4-verified actor URI, never a spoofable handle — before any A2A call. Both pools must have room (all-or-nothing, so an over-budget actor cannot partially spend a domain's budget); an allowlisted-but-unbudgeted domain is **denied by default**. A reservation gates admission and is **never consumption** (D8): actual model-token metering stays aggregate at agentgateway, and the gateway's own reservation counter is labelled by ghost + outcome only — never by remote actor — so a remote org cannot mine another's usage.

## §4 — Discovery and instance description

1. **WebFinger + FEP-844e** (#215) resolves a `acct:agent-<name>@<domain>` handle to the agent's `Service` actor and publishes the A2A AgentCard as actor metadata, so a Fediverse client can both follow the agent and discover its A2A endpoint.
1. **NodeInfo + an instance application actor** (#216) advertises which agents/skills the instance exposes, honestly and enumerably, without leaking internal topology.
1. Discovery is exposure: it ships only after the §3 border, integrity, and budget twins are proven fail-closed.

**Cross-protocol discovery (#215, delivered — _novel_).** One WebFinger lookup on a fediverse handle reveals **both** transports, so a remote org can _choose_ the higher-fidelity A2A delegation over degraded Note-passing — with no proprietary directory in the path. The flow is fully open-standard and decentralized (the per-actor complement to the AGNTCY directory, #146):

1. `GET /.well-known/webfinger?resource=acct:agent-<name>@<domain>` returns two links: `rel="self"` → the ActivityPub `Service` actor, and `rel="https://fgentic.fmind.ai/ns/a2a#agent-card"` → the agent's published **A2A AgentCard**.
1. The actor document advertises the same capability inline via the **FEP-844e `implements`** shape (`{href: <A2A endpoint>, name: "A2A", agentCard: <card URL>}`).
1. `GET …/agent-card.json` returns a synthesized A2A AgentCard (protocol version, name, description, endpoint, transport) built from the `agents.yaml` allowlist — so **only allowlisted agents are discoverable**, and the authoritative full card (skills, exact capabilities) is fetched from the endpoint's own well-known path. Endpoint reachability stays governed by the §3 federation A2A route; discovery advertises the capability, exposure remains gated.

**Instance self-description (#216, delivered).** The instance-scope twin of per-actor discovery, the Fediverse parallel of `/.well-known/matrix/server`. `GET /.well-known/nodeinfo` points to a **NodeInfo 2.1** document (FEP-0151, 2025 ed.) at `/nodeinfo/2.1` that advertises `openRegistrations: false`, the exposed agents (handle, summary, actor + card pointers) sourced **live from the `agents.yaml` allowlist**, and the implemented open protocols (ActivityPub, A2A, FEP-8b32, FEP-844e). A **FEP-2677 `Application`** actor at `/ap/instance` machine-describes the whole instance with the same `implements` list. Adding or removing an agent in `agents.yaml` changes the advertised set deterministically; no unlisted agent is ever announced, and no hand-maintained manifest can drift from the allowlist.

## §5 — Novel collaboration surface

Three items extend the governed core with capabilities Matrix federation does not have a one-to-one analog for, and are marked _novel_ in the backlog:

1. **Group actor per collaboration room** (#217) — a room is projected as an AP `Group` so cross-org participants collaborate through follow/announce semantics, the Fediverse equivalent of a shared federated room.
1. **FEP-c390 identity proof** (#218) — unifies the AP actor identity with the A2A AgentCard key/DID so one verifiable identity spans both transports.
1. **Follow-to-subscribe status/outbox feed** (#219) — following an agent subscribes to its status/outbox, turning task progress into a governed, observable feed rather than an opaque call.

**Group collaboration (#217, delivered — _novel, the M18 headline_).** A designated Matrix collaboration room is projected as an ActivityPub **`Group` actor** at `/ap/groups/<room>` (WebFinger-resolvable), so a remote org collaborates cross-organization **without running Matrix federation at all** — a peer needs only an AP client. The flow, modelled on the guppe fan-out design (reimplemented in Go with `go-ap/activitypub`, not embedded):

1. A remote actor sends a signed `Follow`; the group records it (keyed on the **full actor URI**, never a localpart, so it never assumes a single homeserver) and delivers a **signed `Accept`** to the follower's resolved inbox.
1. On an inbound `Create(Note)`, the group **auto-`Announce`s** it to all _other_ followers (never echoing the author) — the guppe rebroadcast.
1. An `@agent-<name>` mention routes through the governed path (`ReserveBudget` → A2A `message/send`); the agent's reply is published as a FEP-8b32-signed `Note` and `Announce`d back to the group.
1. **The F3/F4/F5 border gates all inbound group traffic**: signature + allowlist on every activity, and a per-mention budget reservation before any A2A call. Off-allowlist or over-budget traffic is dropped before an agent is invoked.

Outbound `Accept`/`Announce` deliveries are **HTTP-Signature signed** with the group's Ed25519 key (published as the actor's `publicKey`), the symmetric counterpart of the inbound signature border. The Group is the AP projection of the room; wiring the two transports' message bodies at runtime is deployment configuration and stays outside this AGPL-free app.

**Cross-transport identity (#218, delivered — _novel_).** An agent now has two federation faces — an ActivityPub actor and an A2A endpoint. **FEP-c390** cryptographically links them: each actor attaches a `VerifiableIdentityStatement` proving it also-knows-as a **P-256 `did:key`**, and its A2A AgentCard publishes the **matching JWK**. A verifier confirms — from the two published faces alone — that they share **one sovereign key**, failing closed on a key mismatch or a missing statement. Because the **key, not the hostname, is the anchor**, the identity **survives a domain move**: re-issuing the statement for the new actor URI verifies unchanged and yields the same `did`, so a verifier who pinned the DID still recognizes the principal. Implemented with stdlib `crypto/ecdsa` + a minimal `did:key` (P-256 multicodec) and `ecdsa-jcs-2019` Data Integrity proof — Go's DID tooling is thin, so this deliberately avoids a heavy framework; the did:key encoding is pinned against the Python `cryptography` reference by a golden test.

**Follow-to-subscribe status feed (#219, delivered).** Sovereign, pull-based notifications with **no webhook lock-in**: an agent publishes operational updates (deploy events, task completions, **LLM cost-alert firings**) as public `Note`s in its outbox, and any human or org **`Follow`s to subscribe** — unfollowing is the unsubscribe. The event source is the **existing Prometheus alert layer**: Alertmanager POSTs to an `/alerts` receiver on the gateway's **internal** metrics port (never the public AP surface), and each firing alert labelled with an allowlisted `agent` becomes a **FEP-8b32-signed** status `Note` fanned out to that agent's followers. Output is **rate-limited per agent** so a flapping alert cannot spam. The `Follow`/`Undo(Follow)` handshake is the whole subscription lifecycle, gated by the F3/F4 border; the cost alert (the token-burn `PrometheusRule`, [observability §9](observability.md)) becomes a first-class, self-hosted, standard-protocol signal — no per-subscriber outbound webhook is ever introduced.

## §6 — Honesty clauses (stated out loud)

1. **Replication and deletion are best-effort**, exactly as with Matrix ([§8.1](federation.md)): an activity delivered to remote inboxes cannot be technically recalled; data residency across instances is a **contractual** control, not a technical one.
1. **A signed, reachable actor is not a governed one.** As with the Matrix onboarding preflight, public discoverability (WebFinger/NodeInfo resolving) never proves that allowlist, budget, and integrity policy are in force — those require separate operator evidence.
1. **Reservations are not consumption.** Per-actor `maxTokens` admission reserves budget; actual model token metrics remain aggregate at agentgateway ([D7/D8](design-decisions.md)). Never present a reservation as spend.
1. **Two transports, name yours.** Any cross-org capability claim must state whether it rode Matrix or ActivityPub; they are additive, not interchangeable.

## §7 — Definition of done (M18 epic)

On the demo profile, a Mastodon/GoToSocial user follows a sovereign agent by its Fediverse handle, `@mentions` it, and receives **one governed, signed, A2A-backed reply** — with the ActivityPub federation policy border proven **fail-closed before any public exposure**, and every §3 twin control demonstrably in force.
