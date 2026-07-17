---
type: Architecture Decision Record
title: Partner Trust Registry Trust-Domain Model
description: Choose the trust-domain contract a single-source-of-truth partner registry renders, so revocation and rotation never conflate inbound consumer trust, local provider identity, remote-agent pins, and room-instance state.
---

# 0020 — Partner Trust Registry Trust-Domain Model

Status: Proposed

Maintainer decision required. This ADR selects the trust-domain contract that [issue #349](https://github.com/fmind-ai/fgentic/issues/349) implements and that [#350](https://github.com/fmind-ai/fgentic/issues/350) (break-glass containment), [#352](https://github.com/fmind-ai/fgentic/issues/352) (signing-key rotation), [#353](https://github.com/fmind-ai/fgentic/issues/353) (bilateral agreement), and [#354](https://github.com/fmind-ai/fgentic/issues/354) (multi-party rooms) build directly on. Until it is accepted, #349 cannot be implemented faithfully: any registry shape chosen inside the implementation PR would silently commit the project's cross-org trust model. The three options below were surfaced by an independent premise audit on #349; this ADR states them, recommends one, and defines the room-ACL reconciliation the recommendation requires.

## Context

Cross-org trust is spread across five hand-edited planes today, and a partner can be admitted on one and forgotten on another. #349 proposes one schema-validated registry that is the single source of truth and _renders_ the enforcement planes rather than replacing them. The premise audit found that the five planes are not five projections of one partner record — they mix directions and lifetimes:

1. **Matrix admission** — `apps/synapse-federation-policy/policy/policy.json` (`allowed_servers`, `invite_rule`) and Synapse `federation_domain_whitelist` admit a partner as a remote Matrix _server_. Inbound consumer trust, keyed by exact `server_name` (D6).
1. **Room ACL** — `m.room.server_acl` is runtime room-creation input in `scripts/lib/federation-matrix.sh`, not a Flux-managed field. It is _per-room instance state_: changing a room constructor does not mutate rooms that already exist, so removal needs an explicit reconciliation model, and no plane may claim ACL removal retracts already-replicated history (docs/federation.md §8.1).
1. **Inbound A2A consumer** — `infra/federation/delegation/policies.yaml` admits the partner as an OAuth client/issuer (`issuer: https://id.<partner>/realms/fgentic-federation`, matched `audiences`, `jwks`, `azp == "org-b-a2a"`, `maxTokens` bounds), and `rate-limit.yaml` turns the verified `maxTokens` into per-`azp` Redis reservations. Inbound consumer trust again, but keyed by the OAuth tuple `(issuer, audience, azp)`, which is not a `server_name` and identifies a machine client, not a person (D11).
1. **Local provider identity** — `infra/federation/delegation/agent-card.json` is the _opposite direction_: org A's own exported docs-qa provider identity. Its P-256 key is generated into the lifecycle-owned bootstrap Secret and its public JWK is injected into the ephemeral Flux snapshot. This is not partner trust at all; revoking a partner must never rotate org A's signing identity.
1. **Remote-agent card pins** — production `agents.yaml` pins each explicitly configured remote agent to a verified ES256 Signed AgentCard `kid`/JWK. The federation lab profile renders none today (it exports org A's card and admits org B only as a Matrix + A2A consumer), so "absent from every rendered plane" needs a defined scope before a registry can assert it.

A registry keyed only by partner `server_name` would conflate inbound consumer trust, org A's outbound provider identity, remote-agent pins, and room-instance state into one record. That is unsafe for removal and rotation: it invites "revoke org B" to rotate org A's signing key, or "edit a room constructor" to be reported as retracting existing room ACLs — a false cross-plane consistency claim, which is worse than five honest planes. The registry must therefore encode _direction_ and _lifetime_, not just identity.

Invariants that constrain every option: match servers by exact `server_name`, never localpart (D6); `X-User-Id` is attribution, not authentication (D11); deny-by-default sender policy; Flux is the sole field manager of rendered artifacts; the registry holds verify-only public material and never a private key or secret; the denied control server (org C) stays absent from every rendered plane.

## Options considered

1. **Directional trust graph (recommended).** One record per partner _organization_ identity, carrying four separate directional projections: `matrixAdmission` (allowlist + room-policy template), `inboundA2AConsumer` (`issuer`/`audience`/`azp`/JWKS/`maxTokens`/classification), `outboundRemoteAgents` (pinned remote AgentCard `kid`/JWK for agents this org _calls_), and a `roomPolicy` template that is reconciled against a separate room-instance inventory. Local provider signing identities (org A's exported card) live in a distinct **local-identity registry**, never in a partner record, so partner revocation cannot touch them. Direction and lifetime are explicit in the schema; each plane renders from exactly one projection.
1. **Bilateral agreement model.** One relationship record per partner, but every directional identity/key is an explicit named field inside it and room instances are a separately reconciled inventory. Functionally close to option 1 for two organizations; it centers the _relationship_ rather than the directional graph. It is a cleaner fit for #353's signed bilateral agreement, but it does not naturally express one org that both consumes from and provides to several partners, and it still must keep local provider identity out of the partner record to stay rotation-safe.
1. **Lab-only registry.** Render only today's A/B Matrix allowlist + inbound JWT/quota + room constructor, explicitly excluding AgentCard pins and existing-room reconciliation. This is deliverable immediately and byte-checkable against the current lab, but it is narrower than #349's stated production single-source-of-truth acceptance ("removing a partner removes it from every plane") and would have to be widened later — earning the drift-consistency claim only for the planes it happens to cover.

## Decision (recommended, pending acceptance)

Adopt **Option 1, the directional trust graph**, with option 2's bilateral-agreement record modeled as a _view_ over it for #353 rather than as the primary schema. Concretely:

1. The registry (`infra/federation/registry/partners.yaml` + JSON Schema) keys each entry by exact partner `server_name`. Each entry carries only inbound-and-outbound _partner_ trust in four labelled projections; a partner absent from a projection is denied on that plane by construction. No entry ever carries a private key.
1. Org A's own exported provider identity (the signed AgentCard, its `kid`, and the bootstrap-Secret-held key material) lives in a separate `infra/federation/registry/local-identity.yaml`, and the renderer treats it as an independent source. Partner add/remove/rotate operations cannot read or write it. This is the load-bearing separation that makes #350 revocation and #352 rotation safe.
1. `roomPolicy` in a partner entry is a _template_, not room state. The renderer emits the room-creation ACL input consumed by `scripts/lib/federation-matrix.sh` for new rooms, and #349 additionally defines a **room-instance inventory** listing existing federated rooms and their current ACL. Removing a partner updates the template _and_ enumerates the existing rooms whose `m.room.server_acl` must be reconciled by an explicit runtime mutation; the registry never claims the template edit alone retracted a live room's ACL, and neither plane claims ACL removal retracts already-replicated history.
1. `check:fed-registry` fails closed on: unknown fields, duplicate or empty allowlist, localpart-based matching, a partner present in one projection's rendered artifact but absent from the registry (or vice-versa), and any rendered artifact that is not byte-identical to a fresh render. The org-C control asserts deny-by-default survives rendering by being absent from the registry and therefore from every rendered plane.
1. Rendered artifacts stay Flux-owned; the registry is the only field-managed source; no `kubectl apply` by hand. The lab remains the acceptance rig ([ADR 0013](0013-federation-lab-acceptance-rig.md)).

## Consequences

- #349 implements one schema, one renderer (`fed:registry-render`), one drift gate, and the room-instance reconciliation contract; #350/#352/#353/#354 inherit a rotation-safe, direction-aware source of truth instead of re-deriving trust identities each.
- Option 3's speed is deliberately declined: shipping a lab-only registry would meet a green check while quietly weakening #349's production acceptance to the planes it covers, exactly the false-consistency risk this ADR exists to prevent.
- The local-identity separation adds one file and one renderer source but is the difference between "revoke a partner" and "accidentally rotate our own signing key". It is not optional under this decision.
- This is a strategic trust-boundary decision; #352/#353/#354 depend on it. It is recorded as Proposed and takes effect only on maintainer acceptance. No registry or renderer is implemented until then.

## References

- [issue #349](https://github.com/fmind-ai/fgentic/issues/349) (implementation), premise audit thread; [issue #85](https://github.com/fmind-ai/fgentic/issues/85) (M8 epic), [#382](https://github.com/fmind-ai/fgentic/issues/382) (M29 GA hardening)
- docs/design-decisions.md D6 (federation-safe resolution — never match by localpart), D11 (attribution, not authentication), D16 (per-cluster model backend)
- docs/federation.md §8.1–§8.2 (honest replication/residency stance), docs/audit.md (evidence chain)
- [ADR 0013](0013-federation-lab-acceptance-rig.md) (lab gates cross-org changes), [ADR 0015](0015-federated-room-encryption.md) (bilateral room controls)
