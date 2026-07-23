---
type: Runbook
title: Partner Federation Offboarding
description: Bilateral operator checklist to revoke a partner organization across every federation and A2A trust plane, with safe ordering and content-free evidence.
---

# Partner Federation Offboarding

This runbook is the mirror of the [onboarding runbook](federation-onboarding.md): it turns the federation design in [§8](federation.md) into a per-plane **revocation** checklist. Evicting a compromised or departing partner touches every plane at once — Matrix transport, room state, the Synapse policy border, pinned Signed AgentCards, and the OIDC client and its quota — and getting the **order** wrong leaves a window where the partner is denied on one plane while still admitted on another. Follow the ordering in §3; each plane's exact mechanism and evidence is in §4.

The provider-free `fgentic-fed` lab is the reference. In it, org B (`org-b.fgentic.localhost`) is the parameterized partner (`federation_partner_server_name` in [clusters/federation/platform-settings.yaml](../clusters/federation/platform-settings.yaml)), and org C (`org-c...`, `federation_denied_server_name`) is the permanently-denied negative control — **org C is what a fully revoked partner looks like on every plane**. Drill an eviction with `mise run fed:up` (see §9); the offline contracts in `mise run check:federation` already assert the target end state.

## 1. Scope and decision

Decide and record which revocation this is:

| Kind                   | Meaning                                                                                | Planes touched (§4)            |
| ---------------------- | -------------------------------------------------------------------------------------- | ------------------------------ |
| Full eviction          | End the partnership; the partner is denied on transport, rooms, policy, A2A, and quota | 1–5                            |
| Partial A2A revocation | Quarantine or rotate one exported agent while Matrix federation continues              | 4 (and 5 for that route/quota) |
| Emergency suspension   | Stop the blast radius first, investigate second; may precede either of the above       | Start at 5→4, then 1–3         |

Full eviction and partial revocation are separate decisions with separate blast radius. Never treat "rotate one agent's key" as "cut off the partner"; conversely, never leave a public A2A route live after a full eviction. Classify the trigger (planned departure, contract end, suspected compromise) and name the owner, change ticket, and rollback owner before touching production. Every git-declared plane is reconciled by Flux; never `kubectl apply` a revocation by hand.

## 2. Owners and change record

Create a private offboarding record (never in Git — no credentials, tokens, keys, room content, or personal contacts):

| Field                                                     | Value |
| --------------------------------------------------------- | ----- |
| Partner legal entity and Matrix server name               |       |
| Revocation kind (§1) and trigger                          |       |
| Security/incident owner and containment authority         |       |
| Technical owner and rollback owner                        |       |
| Change-ticket or pull-request reference                   |       |
| Affected rooms, routes, agents, and OIDC client           |       |
| Maintenance window and expected interruption              |       |
| Retained-copy and deletion obligations (§7 of onboarding) |       |
| Completion attestation owners (both orgs)                 |       |

In a genuine bilateral offboard each organization revokes on its **own** deployment and both sign the completion record; a unilateral revocation is insufficient. On a suspected compromise the security owner may suspend the affected room/route first (§3, emergency) and complete the ordered revocation second.

## 3. Safe ordering

Revoke from the **narrowest, fastest-failing plane outward to the slowest and most irreversible**, so no admitted plane outlives a denied one and history stops growing before you stop transport:

1. **Stop new A2A invocations first** — remove the agentgateway `azp` authorization and public route, then disable the OIDC client (§4.5). This fails closed immediately (403) and stops fresh cross-org token burn; residual access tokens expire within `accessTokenLifespan` (5 min).
1. **Withdraw A2A discovery and the bridge pin** (§4.4) — a partner that cannot discover or verify the card cannot start a delegation; in-flight bridge delegations quarantine (§5).
1. **Tighten the room** (§4.2) — send an `m.room.server_acl` dropping the partner, then make the affected rooms read-only or retire them as agreed. Do this before transport so the partner is ACL-denied even for any event still in flight.
1. **Deny at the policy border** (§4.3) — remove the partner from `policy.json` and `fed:policy-reload`. This live-reloads without restarting Synapse, so it is the fastest homeserver-side deny and a good emergency lever.
1. **Close Matrix transport last** (§4.1) — remove the partner from `federation_domain_whitelist` and `trusted_key_servers`. This triggers a Synapse restart, so it is the slowest step; by now every faster plane already denies the partner.
1. **Then apply local purge, retention, and backup-expiry duties** (§6). ACL/allowlist removal never retracts already-replicated history — see §7.

The emergency short-path is steps 1→4 (data-plane + live policy reload, no restart) to stop the blast radius, followed by the full ordered sequence.

### 3.1 Break-glass automation (`fed:break-glass`)

The manual per-plane sequence in §3 is the authoritative mechanism; for a compromise you want it fast, ordered, reversible, and evidenced without a hand-scramble across five planes. `mise run fed:break-glass contain <partner_server_name>` (issue #350) is the registry-native break-glass: it flips the partner's `contained` flag in the [partner trust registry](federation.md#821-partner-trust-registry--one-validated-source-of-truth) and re-renders, which drops the partner from the callback border (`policy.json` `allowed_servers`) so every federated event from it is denied at [§4.3](#43-policy-border--appssynapse-federation-policy). Because containment is GitOps-native, it is auditable (a git commit) and reversible (`mise run fed:break-glass restore <partner>`); `mise run fed:break-glass status` lists contained partners. Committing the re-render and letting Flux reconcile applies the border deny; the remaining planes (transport whitelist, per-room ACL, bridge sender grants, `azp` quota) are revoked by the ordered runtime steps the command prints, matching §3's order.

Each `contain`/`restore` writes one **content-free containment record** (`fgentic.federation.containment.v1`): the partner `server_name`/`azp`/`classification`, the git revision, a UTC timestamp, and the four safe-order planes — never event content, prompts, or message bodies. `mise run fed:evidence-pack <partner>` assembles that record plus the partner's verify-only identity facts (`server_name`, `azp`, `issuer`, key IDs — no secrets or private keys) into a `fgentic.federation.evidence.v1` **regulator evidence pack** that names the already-emitted content-free record streams a reviewer collects live — the Synapse callback denials, agentgateway authz/429 decisions, and bridge attribution audit (see [docs/audit.md](audit.md)) — reconstructing who/when/what from event IDs, policy digests, key IDs, and revisions only. The pack states its honest limits inline: ACL/whitelist removal does not retract already-replicated Matrix history (§7), and `X-User-Id` is attribution, not authentication ([D11](design-decisions.md)).

The distinction a regulator asks for:

1. **Public probe** — what an unauthenticated third party can observe (the partner's route now fails closed). Evidence: the [preflight](federation-onboarding.md#3-public-discovery-preflight); never sufficient on its own — reachability is not governance.
1. **Operator evidence** — the content-free records above, collected by the owning operator with cluster access. This is the substantive who/when/what.
1. **Contractual attestation** — both operators' signed completion record ([§10](#10-completion-record)) that the eviction and its evidence are accepted. Trust decisions live here, not in the tooling.

**Abuse-report intake.** A partner reports abuse with a `fgentic.federation.abuse-report.v1` document (schema: [infra/federation/registry/abuse-report.schema.json](../infra/federation/registry/abuse-report.schema.json)): reporter and subject `server_name`, category, severity, requested action, and **reference-only** `evidence_refs` (Matrix event IDs, policy digests, key IDs — never message content). A `requested_action: contain` maps directly to `fed:break-glass contain <subject>` and the evidence pack above; intake logs the references, never the reported content. `mise run check:fed-break-glass` proves the border drop/restore, the content-free record and pack, and the abuse schema offline; triggering the live lab and asserting every plane denies the contained partner is the runtime-owner proof.

## 4. Per-plane revocation

Each plane below names the exact control, how the change is applied, whether a reload/restart is needed, and the content-free evidence the revocation leaves. `${federation_partner_server_name}` is the partner being removed.

### 4.1 Matrix transport — `federation_domain_whitelist`

- **Control:** the `10-federation` Synapse config in [infra/federation/matrix-a/kustomization.yaml](../infra/federation/matrix-a/kustomization.yaml) (org A) / [infra/federation/matrix-b/helmrelease.yaml](../infra/federation/matrix-b/helmrelease.yaml) (org B's own deployment). Remove the partner from `federation_domain_whitelist` **and** from `trusted_key_servers`; if the partner's ingress is deployment-owned, also remove its `federation-*` Gateway, HTTPRoutes, and TLS Certificate.
- **Apply:** GitOps/Flux. This is homeserver config in the ESS HelmRelease, so the change triggers a Helm upgrade and a **Synapse pod restart** (the slowest plane — order it last).
- **Evidence:** `GET /_synapse/client/v1/config/federation_whitelist` must equal `[own server]`; a partner-signed `POST /_matrix/federation/v1/send` returns `403 M_FORBIDDEN` — the same mechanism the denied org-C control proves. `check:federation` asserts the whitelist excludes the denied server.

### 4.2 Room state — `m.room.server_acl`

- **Control:** send a new `m.room.server_acl` state event (`PUT /_matrix/client/v3/rooms/{roomId}/state/m.room.server_acl`) dropping the partner from `allow` (optionally adding it to `deny`), keeping `allow_ip_literals: false`. The lab installs the initial ACL as room-creation state (`create_federated_room` in `scripts/lib/federation-matrix.sh`); tightening is a client/admin action, not a script.
- **Apply:** authenticated Matrix client action; **no restart**. Then make the room read-only or retire it as agreed.
- **Evidence:** an authenticated `GET .../state/m.room.server_acl` shows the partner removed. **Caveat:** ACL removal does not retract history already replicated to the partner's homeserver (§7).

### 4.3 Policy border — `apps/synapse-federation-policy`

- **Control:** [apps/synapse-federation-policy/policy/policy.json](../apps/synapse-federation-policy/policy/policy.json) — delete the partner from `allowed_servers`, keeping the local `server_name` (an `allowed_servers` list that omits the local server, or is empty, fails closed to deny-all). Optionally set `invite_rule: deny_all`.
- **Apply:** GitOps/Flux, then `mise run fed:policy-reload`. The ConfigMap is projected as a directory and the module validates and swaps the whole policy on the next callback, so this **live-reloads without restarting Synapse** — the fastest homeserver-side deny.
- **Evidence:** the `should_drop_federated_event` / `federated_user_may_invite` callbacks emit a content-free `fgentic_federation_policy_violation` log with `reason=server_not_allowed` (or `invite_rule_denied`), the affected `server`/`room`/`event`, and a `policy_digest`; the digest changes when `allowed_servers` changes, giving the revocation a clean fingerprint. The offline unit suite and `check:federation` cover the deny-all and `server_not_allowed` paths.

### 4.4 A2A Signed AgentCard

- **Withdraw discovery (publisher, org A):** delete the card's public routing in [infra/federation/delegation/routes.yaml](../infra/federation/delegation/routes.yaml) and [policies.yaml](../infra/federation/delegation/policies.yaml) (the `federated-docs-qa-card` HTTPRoute + `AgentgatewayPolicy` and the card `GET` rule) and the public-JWK evidence ConfigMap. GitOps/Flux, no restart; the card `GET` then 404s.
- **Revoke the bridge pin (consumer):** remove the partner's remote entry from `agents.yaml`, or remove/rotate its `cardIdentity`, or empty its `allowedServers`/`allowedSenders`. A pin mismatch **quarantines** the target: profile sync records `agent_card_untrusted` and the worker re-reads policy and fails closed before spending a limiter token or dispatching A2A (docs/bridge.md §6).
- **Evidence:** the agent-card audit logs `outcome=rejected`, `reason=agent_card_untrusted`; a delegation attempt audits `outcome=denied`, `terminal_stage=agent_card`, `terminal_reason=agent_card_untrusted`, `a2a_attempted=false`. (Re-admission logs the `agent_card_verified` accept — §7.)

### 4.5 OIDC client, quota, and public A2A route

- **OIDC client:** in [infra/federation/delegation/keycloak/kustomization.yaml](../infra/federation/delegation/keycloak/kustomization.yaml), set the partner client (`org-b-a2a`) `enabled: false` or delete it, and/or rotate its client secret. Realm re-import reconciles via Flux; `accessTokenLifespan: 300` bounds any already-issued token to at most five minutes.
- **agentgateway authorization + quota:** in [infra/federation/delegation/policies.yaml](../infra/federation/delegation/policies.yaml), remove the `jwt.azp == "org-b-a2a"` authorization clause (or delete the `federated-docs-qa` policy); the fail-closed rate-limit descriptor keyed on `jwt.azp` then no longer reserves for the partner. A revoked client's request fails authorization with `403`.
- **Public route:** delete the `federated-docs-qa-public` HTTPRoute in [routes.yaml](../infra/federation/delegation/routes.yaml) (and, to sever the JWKS fetch, its `ReferenceGrant`/NetworkPolicy) so the exact `POST /api/a2a/kagent/docs-qa` origin returns `404` with no agentgateway listener reachable.
- **Apply:** all GitOps/Flux, data-plane reload, no Synapse involvement.
- **Evidence:** record the reviewed render showing the partner `azp` authorization, descriptor, and public route removed; then prove that a client-credentials call with the old secret is unauthenticated/unauthorized and that the exact A2A `POST` fails `403` or `404`. The current rate-limit component stores transient reservation counters in Redis with StatsD disabled, so it exposes no per-`azp` Prometheus series; do not make a nonexistent series an offboarding condition or report reservation state as measured token consumption.

## 5. In-flight and mid-task behavior

Revocation is fail-closed at admission, not a kill switch for a request already mid-flight:

1. A remote target whose pin was removed or whose trust changed is quarantined; the bridge worker re-reads mapping, origin, and sender policy before each limiter token and A2A call, so a delegation queued under the old policy is refused with `agent_card_untrusted` (or `sender_policy_rejected`) and never dispatched.
1. A delegation already dispatched when trust changes mid-poll stops polling with `agent_card_untrusted` and a content-free room notice; token burn at the source is the partner's to stop.
1. Directory (`!agents`), policy-denial, and rate-limit notices carry no message content, and every revocation audit record is content-free (identifiers, reasons, digests, timestamps — never prompt or reply text).
1. On the inbound A2A path, an in-flight cross-org task loses its next reservation the moment the `azp` authorization is removed; `accessTokenLifespan` bounds residual authenticated calls.

## 6. Partial A2A revocation (one agent)

To quarantine or re-key **one** exported agent while the Matrix partnership continues, operate only on plane 4 (and plane 5 for that agent's route/quota); leave planes 1–3 intact:

1. Rotate the agent's ES256/P-256 signing key on the publisher side (the private key and key ID live only in the lifecycle-owned bootstrap Secret, never in Git or a workload namespace) and re-sign the card with `scripts/sign-agent-card.sh`. The rotation guards refuse to rotate a missing key while public artifacts still exist and refuse to overwrite an already-pinned public JWK, so a genuine rotation clears the old key material first, then re-signs and re-publishes.
1. On the consumer, pin the new key ID/JWK (or remove just that `agents.yaml` entry). Until the new public identity is exchanged and pinned, the old-key card fails verification and the target stays quarantined — the intended fail-closed state during rotation.
1. Prove the old identity fails and the new one verifies before considering the rotation complete.

## 7. Re-admission

Re-admission reverses each plane; treat it as a scoped re-onboarding (invert the onboarding activation table) and re-collect evidence:

1. Re-add the partner to `federation_domain_whitelist` and `trusted_key_servers` (Synapse restart).
1. Send an `m.room.server_acl` restoring the partner to `allow` on the agreed rooms (no restart).
1. Re-add the partner to `policy.json` `allowed_servers` and `mise run fed:policy-reload` (no restart).
1. Re-publish the signed card and restore the `agents.yaml` entry/`cardIdentity`; confirm the bridge logs `agent_card_verified`.
1. Re-enable the `org-b-a2a` OIDC client and restore its route and quota.

Because Matrix history replicated before eviction cannot be retracted, re-admission does not restore any prior expectation of confidentiality over that history; treat the re-admitted partnership as new and re-approve the §7 contractual gates of the onboarding runbook.

## 8. Staged eviction and sign-off

Both operators execute and sign each stage; a unilateral success is insufficient. This inverts the onboarding activation table.

| Stage         | Action                                                         | Exit condition (both orgs)                                              |
| ------------- | -------------------------------------------------------------- | ----------------------------------------------------------------------- |
| A2A stop      | Remove `azp` authorization + public route; disable OIDC client | Reviewed render omits `azp`/route; old client and exact POST fail       |
| A2A discovery | Withdraw card discovery; revoke bridge pin                     | Card `GET` 404s; delegation audits `agent_card_untrusted`               |
| Room          | Tighten `m.room.server_acl`; retire/read-only rooms            | Authenticated ACL state shows partner removed                           |
| Policy border | Remove partner from `policy.json`; `fed:policy-reload`         | `server_not_allowed` violation with changed `policy_digest`, no restart |
| Transport     | Remove partner from `federation_domain_whitelist`              | `federation_whitelist` excludes partner; partner send `403`             |
| Data duties   | Local purge, retention, backup expiry per contract             | Deletion evidence recorded; replicated-history caveat stated            |
| Completion    | Both change owners attest                                      | Every plane provably closed; audit trail collected                      |

## 9. Verification and lab drill

Prove every plane fails closed and collect the content-free evidence:

```bash
mise run check            # static checks on the exact revocation revision
mise run test
mise run check:federation # offline: whitelist/policy/ACL exclude the denied server; deny-all fails closed
```

`mise run fed:up` reconciles the lab and runs the seeded proof (denied-server join `403`, signed-federation send forbidden by both A and B, content-free policy-violation evidence) — org C already demonstrates the fully-revoked end state, so an eviction drill points the partner (`org-b`) at that same configuration and re-runs the proof. `mise run fed:policy-reload` proves the policy-border deny live without restarting Synapse. `mise run fed:down` removes only the owned lab cluster and images.

The offline `check:federation` contracts assert the invariants a revocation must reach — the partner absent from the whitelist, `policy.json allowed_servers`, and room ACL — so the runbook is validated against the repository contract even without a live drill; the live drill validates the prospective deployment.

## 10. Completion record

The offboarding record is complete only when it contains:

1. Both organizations' named owners and completion attestations.
1. The revocation kind, trigger, and change/rollback references.
1. Reviewed Git revisions for every git-declared plane and clean static-test results.
1. Content-free evidence per plane: `federation_whitelist` query, ACL state, `policy_digest` and `server_not_allowed` violation IDs, `agent_card_untrusted` audit IDs, the rendered revision without the partner `azp`/route, and the old client's failed token and exact-POST status codes.
1. Local purge, retention, and backup-expiry completion, with the explicit statement that replicated history is not retracted.
1. Re-admission owner and conditions, if the partnership may resume.

Removing a partner from an ACL or allowlist does not retract history already replicated to its homeserver; technical revocation cannot substitute for the contractual deletion, redaction, and retained-copy obligations agreed at onboarding.
