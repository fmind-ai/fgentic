---
type: Specification
title: IdP Group Sync
description: One-way GitOps reconciler from authoritative Keycloak groups to managed Matrix room membership through a scoped access-manager identity.
---

# IdP Group Sync

`apps/matrix-group-sync` is the first implementation of [ADR 0009](adr/0009-agent-authorization-model.md) / [D20](design-decisions.md): a one-way GitOps reconciler that materializes authoritative IdP-group membership into managed Matrix room membership. IdP groups declare desired access; the reconciler converges it into room state; the bridge authorizes only within that already-materialized state ([docs/bridge.md](bridge.md), [docs/security.md](security.md)). It is a self-contained app, deliberately separate from the mautrix bridge.

The design principle is fail-closed convergence through the smallest possible authority. The reconciler holds a narrowly scoped Matrix access-manager identity and a read-only IdP credential; every ambiguous input produces no grant, and an incomplete read additionally produces no removal.

## Data flow

1. Git declares exact `Keycloak group path -> managed room` bindings, each with an explicit agent set.
1. Keycloak is the authoritative membership source. The reconciler reads each bound group's members and the administrator-managed single-valued `matrix_localpart` attribute ([docs/identity.md](identity.md)), keys reconciliation by the stable upstream `sub`, and forms the full local MXID `@<matrix_localpart>:<server_name>`.
1. Every 60 seconds it reconciles a complete paginated directory snapshot against live Matrix room state and drives normal invites and kicks through the access-manager identity.

The reconciler is stateless-from-truth: desired state comes from the IdP directory and actual state from live Matrix room state, so a restart loses nothing and **no scoped database role is required**. The only cross-cycle memory is the in-process missed-interval counter and pending-revocation ages that back the two alerts.

## Fail-closed decision points

| Condition                                                                                    | Behavior                                                                                                                     |
| -------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------- |
| Partial read, timeout, or transport error                                                    | No grants and no bulk removals; retain last-known Matrix state; raise the stall alert after two consecutive missed intervals |
| Ambiguous mapping (duplicate `sub`, duplicate `matrix_localpart`, or a member with no `sub`) | Whole-cycle abort of mutation (no grants, no removals)                                                                       |
| Missing or invalid `matrix_localpart` for a member                                           | That member is ungrantable and skipped; identity is never guessed                                                            |
| Nonexistent Matrix account (profile lookup) or a failed lookup                               | No invite                                                                                                                    |
| Unmanaged room, unexpected creator, non-v12 room, or power-level drift                       | Grants blocked for that room                                                                                                 |
| A remote (partner) member                                                                    | Never revoked by a local IdP group — local groups apply only to local MXIDs                                                  |

Local IdP groups apply only to local MXIDs. Partner users enter a federated managed room through explicit Matrix membership under the access-manager's power levels and an `allowedServers` entry; their homeserver vouches for their MXID, not for an enterprise group (federation-safe — [D6](design-decisions.md)).

## Access-manager credential scope

The access-manager is an ordinary Matrix account, not an administrator. It creates and solely owns each managed room and is the only principal with invite, kick, ban, and authorization-state power; humans and partner users are kept at power level 0. The reconciler uses **only normal Client-Server API endpoints**: resolve alias, read room state, profile lookup, invite, kick, ban.

It deliberately does **not** use:

- a Synapse server-admin credential,
- the MAS `urn:mas:admin` scope (which grants the whole Admin API),
- Matrix Spaces or a restricted join rule as the group, or
- an appservice that impersonates humans.

The IdP credential is a Keycloak client-credentials **read** client scoped to reading the bound groups, their members, and the `matrix_localpart` attribute — nothing that mutates a user or issues a token for one. Both credentials are mounted as files from SOPS-backed Secrets (`secrets.matrix.secretName`, `secrets.keycloak.secretName`), never placed in the process environment.

## NetworkPolicy

The chart ships a default-deny `NetworkPolicy`. Ingress is allowed only from the monitoring namespace (metrics scrape). Egress is allowed only to DNS, the homeserver namespace (Synapse's client API), and the IdP namespace (Keycloak's directory read). kagent, the model path, and the public internet are all unreachable — it is an internal control-plane worker with no request-serving surface.

## Bindings configuration

Bindings are a git-declared ConfigMap. One binding is one access bundle: an exact group path, one server-qualified room alias, and the explicit agent set the room is bound to.

```yaml
schemaVersion: 1
bindings:
  - group: /fgentic/agent-access/platform
    roomAlias: "#agent-platform:fgentic.fmind.ai"
    agents: [agent-k8s, agent-helm]
```

Group paths are absolute and unambiguous (a group name may not contain the `/` separator); room aliases are always server-qualified; group and room are each unique. Different privileges require different rooms, not hidden per-user rules inside one room.

## Operational rollout (audit-only first)

`config.enforce=false` is the default. In audit-only mode the reconciler validates `matrix_localpart`, computes membership diffs, and reports them through logs and metrics while making **zero** Matrix mutations and raising **no** revocation-SLO alert. Adopt each existing agent room only after the access-manager owns its invite/kick/ban controls, its exact agent set is recorded, and current membership has human review. Then set `config.enforce=true`, which enables additions, removals, and the revocation-SLO alert together.

Two alert series back the security contract:

- `matrix_group_sync_reconcile_stalled` — the directory read has been incomplete or ambiguous for two consecutive intervals; a stale-access window is now unbounded until an operator responds (the alert is a security control, not optional observability).
- `matrix_group_sync_revocation_slo_breach` — a computed revocation has stayed unapplied past the two-minute SLO (enforce mode).

## Comparison with Element Server Suite Pro Group Sync

Element's commercial Element Server Suite (ESS) Pro offers a **Group Sync** capability that provisions Matrix **Spaces** and room membership from an external identity source and is operated through Element's proprietary provisioning tooling. It is a supported, packaged feature of a commercial suite.

Fgentic's reconciler is a different, deliberately narrower design and is not a reimplementation of it. To our reading of the two approaches:

- It is Apache-2.0 and self-hostable end to end, with no proprietary component in the path.
- It authorizes through **plain room membership**, not Spaces; ADR 0009 rejects Spaces because removing a user from a Space does not remove an existing child-room membership, so a Space is presentation rather than enforcement.
- It uses a scoped, ordinary **access-manager client identity** rather than a Synapse-admin or provisioning-service credential.
- Desired state is **git-declared** exact bindings reconciled on a fixed interval, with an explicit audit-only rollout and a bounded revocation SLO.
- It is federation-safe by construction: a local IdP group only ever asserts a local MXID.

This comparison is a factual positioning of scope and licensing, not a feature-by-feature benchmark or a performance or completeness claim against ESS Pro.
