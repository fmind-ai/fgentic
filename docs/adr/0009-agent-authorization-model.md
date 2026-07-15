---
type: Architecture Decision Record
title: Agent Authorization Through Managed Room Membership
description: Authorize enterprise agent access via managed room membership rather than a parallel ACL system.
---

# 0009 — Enterprise Agent Authorization Through Managed Room Membership

Status: Proposed

Approval gate: a human must approve this ADR in [issue #19](https://github.com/fmind-ai/fgentic/issues/19) before implementation issues are created. Nothing below is a settled design decision yet.

Scope note: accepted [ADR 0017](0017-permission-aware-identity-binding.md) independently governs content-row ACLs and the audience of grounded Matrix output. It neither accepts nor depends on this proposal's IdP group reconciler; Matrix retrieval v1 uses typed exact full-principal ACLs and no group mapping.

## Context

Enterprise identity providers express access through groups and roles, while the bridge receives Matrix events containing a room ID and the sender's Matrix ID (MXID). It does not receive the user's upstream OIDC token or claims. The pinned ESS `26.6.2` chart contains MAS `1.19.0` and Synapse `1.155.0`; in that exact MAS version, upstream claim imports are limited to the subject, MXID localpart, display name, email, and account name. There is no persistent group attribute in the [MAS upstream provider schema](https://github.com/element-hq/matrix-authentication-service/blob/v1.19.0/crates/config/src/sections/upstream_oauth2.rs).

Keycloak can emit group membership as an OIDC claim. Its [group membership mapper](https://github.com/keycloak/keycloak/blob/26.7.0/services/src/main/java/org/keycloak/protocol/oidc/mappers/GroupMembershipMapper.java) supports full, unambiguous group paths and can include them in an ID token, access token, UserInfo response, or introspection response. That claim proves group membership only when the token is issued or UserInfo is queried; it is not attached to later Matrix events.

Matrix already has a federation-compatible authorization fact: current room state. The [room v12 authorization rules](https://spec.matrix.org/latest/rooms/v12/) reject ordinary events unless the sender's current `m.room.member` state is `join`; the same rules use room power levels for invite, kick, and ban operations. Removing a user from a room therefore prevents that MXID from sending the next message without changing or introspecting its Matrix access token.

The current bridge adds two controls from D6: `allowedServers` and anchored `allowedSenders` globs over the full MXID. They are useful ceilings, especially for federation, but are a poor dynamic group database. They have no room dimension and must be regenerated when a group changes. The Helm chart preserves both fields in `agents.yaml` and tests that rendering contract. The bridge still auto-accepts every valid invite for a mapped ghost and calls `EnsureJoined` during dispatch, so a room-membership design is incomplete until the bridge can restrict each ghost to explicitly managed rooms.

The relevant failure modes are:

1. **Stale group data.** Login-time claims and directory caches can retain a revoked membership; transient directory failures can also produce dangerously incomplete listings.
1. **Ghost invites.** A user who controls another room can invite a mapped ghost and create an unintended invocation surface unless the bridge validates the room-agent binding.
1. **Federated rooms.** The local IdP is authoritative only for local MXIDs. Rejecting otherwise-valid remote events through a Synapse module can split the room DAG, and a remote homeserver does not expose its users' IdP groups.
1. **Identity drift.** Keycloak usernames can change and are not a durable join key. A group member therefore needs an immutable, validated Matrix localpart rather than an MXID reconstructed from a mutable username.
1. **Over-broad rooms.** Room membership cannot distinguish agents within the same room unless the room is bound to an explicit agent set.

## Decision

If accepted, use **managed Matrix room membership as the authorization boundary**. IdP groups declare desired membership; a small reconciler materializes that intent into Matrix room state. The bridge authorizes only within that already-materialized state.

1. Define declarative bindings from one exact IdP group path to one managed Matrix room and an explicit set of agent ghosts. A room represents one access bundle. Different privileges require different rooms instead of hidden per-user rules inside one collaboration room.

   ```yaml
   bindings:
     - group: /fgentic/agent-access/platform
       roomAlias: "#agent-platform:fgentic.example"
       agents:
         - agent-k8s
         - agent-helm
   ```

1. Normalize the reference Keycloak provider to a string-list `groups` claim using the `oidc-group-membership-mapper` with `full.path: true`. The MAS client requests a dedicated `fgentic-groups` client scope. Group names containing `/` are forbidden so paths remain unambiguous. Generic OIDC deployments must normalize their provider's group or role claim to the same exact-path contract.
1. Give every local user a single-valued `matrix_localpart` IdP attribute. Keycloak's user-profile policy makes it required, invisible to end users, and administrator-read-only; its OIDC user-attribute mapper emits the same claim to MAS. MAS imports `{{ user.matrix_localpart }}` with `action: require` and `on_conflict: fail`. The reconciler reads that same directory attribute and forms `@<matrix_localpart>:<server_name>`, so neither it nor MAS derives identity from a mutable username. The stable upstream `sub` is the reconciliation key and duplicate localparts fail closed.
1. Treat OIDC claims as identity-contract diagnostics and interoperability evidence, **not** as runtime authorization credentials. The reconciler reads authoritative current group membership and `matrix_localpart` from the IdP directory; it does not need an OIDC token from a Matrix event or MAS's full-power `urn:mas:admin` scope. A generic IdP that cannot expose an immutable Matrix localpart needs a separate subject-to-MXID registry and is outside the first implementation.
1. Reconcile the full desired set at a 60-second interval. Additions and removals are applied only after a complete, successful, paginated directory read. A partial response, timeout, or ambiguous subject mapping creates no grants and no bulk removals; it retains the last Matrix state, emits a metric, and alerts after two missed intervals. This avoids a directory outage evicting every user while bounding an ordinary revocation to a proposed two-minute SLO.
1. Make every managed agent room version 12, private, and invite-only. The local access-manager identity creates it, remains its sole room creator, and is the only principal with invite, kick, ban, and authorization-state power. Humans and partner users get power level `0`; room-directory publication is disabled. `m.federate` is set deliberately at room creation because it is immutable.
1. On grant, the access-manager invites the local MXID through the normal Matrix client API; the user accepts the invite. On revocation, it withdraws any pending invite or kicks the joined member. Because the room is invite-only and humans cannot invite, the removed MXID cannot rejoin. This avoids a Synapse server-admin credential or an appservice that impersonates humans. Regrant follows the same normal invitation path. Emergency revocation remains a direct room kick/ban plus disabling the binding.
1. Harden the bridge before enabling group reconciliation:
   1. Add an exact `allowedRooms` set per agent and reject a target before dispatch unless the event room is bound to that ghost.
   1. Require the target ghost's current membership in that exact room; a message must never cause an ambient join.
   1. Accept ghost invites only from the access-manager and only for a declared room-agent binding.
   1. Preserve `allowedServers` and `allowedSenders` through the Helm values and rendered `agents.yaml`, with validation and tests.
1. Keep `allowedSenders` as a small, static defense-in-depth ceiling for exceptional individual or partner restrictions. Do not generate large IdP groups into MXID globs. For a local managed room, an empty `allowedSenders` means any **current room member** from an allowed server; room membership carries the dynamic decision. Federated senders remain deny-by-default through `allowedServers` as required by D6.
1. Apply local IdP groups only to local MXIDs. Partner users enter a federated managed room through explicit Matrix membership under the local access-manager's power levels and an `allowedServers` entry. Their homeserver vouches for their MXID; it does not vouch for an enterprise group understood by this deployment.
1. Do not claim this model expresses per-message risk, agent tool permissions, row-level data access, time-of-day policy, or different agent access for two users in the same room. Those require separate rooms/access bundles or a later policy engine. Agent tools still enforce least privilege independently of who can invoke the agent.

## Alternatives considered

1. **Query Keycloak from the bridge for every mention. Rejected.** Matrix events have no upstream token, so the bridge would need directory credentials plus a subject-to-MXID lookup. Caching reintroduces stale revocations; no cache adds an IdP network dependency and latency to every message; partner MXIDs have no local group record. It also mixes identity synchronization into the message data plane.
1. **Authorize through MAS claims or policy. Rejected.** MAS authenticates the session, but its pinned schema does not persist arbitrary groups and the resulting Matrix event carries no claim. Back-channel logout is valuable session hygiene, not a group-to-agent authorization boundary. Its [`urn:mas:admin` scope grants the whole Admin API](https://element-hq.github.io/matrix-authentication-service/reference/scopes.html#urnmasadmin), so using it merely to map an IdP subject to an MXID would also violate least privilege.
1. **Enforce group checks in a Synapse module. Rejected for the first implementation.** `user_may_join_room` and `user_may_invite` can guard transitions, but they do not evict an already-joined user after revocation. The broader `check_event_allowed` hook is explicitly experimental, runs on federated traffic, and [can diverge room history when it rejects remote events](https://element-hq.github.io/synapse/latest/modules/third_party_rules_callbacks.html). A module would still need the same directory and reconciliation logic.
1. **Generate `allowedSenders` from groups. Rejected.** This loses room context, expands mutable identity data into bridge configuration, and makes revocation depend on configuration delivery and process reload. It remains a useful static ceiling, not the source of group truth.
1. **Use a Matrix Space or restricted join rule as the group. Rejected.** Restricted joins check membership when a user joins a child room; removing the user from the Space does not remove an existing child-room membership. A reconciler is still required for revocation, so the Space would be presentation rather than enforcement.
1. **Manual room administration. Rejected for enterprise operation.** It is the smallest demo, but it cannot provide a bounded revocation SLO, deterministic group mapping, or drift detection.

## Consequences

1. Every authorization decision on the message path is local, low-latency Matrix state; an IdP or MAS outage does not halt existing conversations.
1. Revocations become visible, auditable membership events and take effect for the next message after reconciliation. The cost is a bounded stale-access window during normal operation and an unbounded one if reconciliation fails without operator response; the alert is therefore a security control, not optional observability.
1. Federation uses the same room semantics without pretending that organizations share an IdP. The local deployment controls who can invite and which partner homeservers may invoke each agent.
1. One room per access bundle is deliberately less flexible than a general policy engine, but it is understandable to users and operators, inspectable in any Matrix client, and compatible with standard federation.
1. The reconciler needs narrowly scoped read access to only the bound Keycloak groups, their members, and `matrix_localpart`. The access-manager needs a Matrix client token and power only in rooms it creates. The bridge needs neither IdP credentials nor permission to modify human membership; MAS and Synapse admin credentials are deliberately absent.
1. Group deletion or rename is a successful empty result for the old exact path and therefore revokes that binding after a complete reconciliation. Directory transport and pagination errors retain last-known membership instead of causing a mass eviction.
1. Room history remains available to former members according to Matrix history visibility and client caches. Removing membership stops future invocation; it cannot recall content already delivered. Sensitive access bundles therefore use dedicated rooms and the retention posture documented in [the federation spec](../federation.md).

## Migration and implementation gates

1. Human approval of this ADR is the first gate. Until then, D6 and the current room behavior remain authoritative and this file must not be added to `docs/design-decisions.md` as settled.
1. After approval, create separate implementation issues for the binding/reconciler, bridge room-binding hardening, managed-room bootstrap, metrics/alerts, and local/federated conformance tests.
1. Ship bridge hardening before any group grants. Ambient `EnsureJoined` and unrestricted mapped-ghost invite acceptance remain explicit blockers; the existing Helm preservation tests for `allowedServers`/`allowedSenders` stay as regression guards.
1. Roll out in audit-only mode first: validate `matrix_localpart`, compute membership diffs, and compare them with existing rooms without mutation. Any duplicate subject/localpart, missing or invalid localpart, nonexistent Matrix account, unmanaged room, unexpected creator, or power-level drift fails closed for grants.
1. Adopt each existing agent room only after the access-manager owns its invite/kick/ban controls, its exact agent set is recorded, and current membership has human review. Then enable additions, removals, and the revocation-SLO alert together.
1. Acceptance requires tests proving: invite and accepted grant; pending-invite and joined-member revocation within the approved SLO; no changes after a partial directory read; renamed/deleted group behavior; duplicate subject/localpart denial; nonexistent Matrix-account handling; ghost invite denial; an unbound-agent mention denial; `allowedServers` federation denial; and continued authorization during an IdP outage for last-known members.

## Approval required

The human decision is whether to accept room membership as the deliberately simple boundary, including user-accepted invitations, the one-room-per-access-bundle constraint, and the proposed 60-second reconciliation/two-minute alert-and-revocation SLO. Approval must also name the initial exact group-to-room-to-agent bindings. Only then can this ADR become `Accepted`, enter the design-decision register, and produce the follow-up implementation issues required by issue #19.
