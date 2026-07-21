---
type: Runbook
title: Partner Federation Onboarding
description: Bilateral operator checklist to onboard a partner organization into Matrix federation and A2A delegation.
---

# Partner Federation Onboarding

This runbook turns the federation design in [§8](federation.md) into a bilateral operator checklist. Each organization completes the same gates for its own deployment, exchanges the resulting evidence, and enables the partner only after both sides sign off.

## 1. Scope and trust levels

The workflow covers Matrix federation for shared rooms and the agreements needed before either side exposes an A2A delegation endpoint. It does not make an internet-reachable homeserver trustworthy by itself.

Classify every onboarding record by how it was established:

| Trust level             | Meaning                                                                           | Examples                                                                                  |
| ----------------------- | --------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------- |
| Public probe            | Unauthenticated observation that can change without notice                        | DNS/TLS reachability, `/.well-known/matrix/server`, federation software version           |
| Operator evidence       | Authenticated or local proof supplied by the operator responsible for the control | Git revision, Synapse allowlist, firewall rule, room state, sender policy, rotation drill |
| Contractual attestation | Approved commitment between the organizations                                     | DPA, residency, retention, redaction, incident and offboarding obligations                |

Public probe evidence is useful preflight data. It does **not** prove mutual allowlisting, room authorization, data handling, or the identity of a natural person. This document is operational guidance, not legal advice; qualified reviewers must approve the contractual terms.

## 2. Owners and change record

Both organizations create a private onboarding record before exchanging configuration. Do not put credentials, access tokens, private signing keys, room content, or personal contact details in this repository.

| Field                                      | Organization A | Organization B |
| ------------------------------------------ | -------------- | -------------- |
| Legal entity and Matrix server name        |                |                |
| Technical owner and backup                 |                |                |
| Security incident contact                  |                |                |
| Privacy/legal approver                     |                |                |
| Change-ticket or pull-request reference    |                |                |
| Intended rooms and business purpose        |                |                |
| Data classification allowed in those rooms |                |                |
| Planned activation and review dates        |                |                |
| Offboarding owner                          |                |                |

Agree on a maintenance window and rollback owner. Every GitOps change must be reviewable and independently reversible; neither operator applies production manifests by hand.

## 3. Public discovery preflight

Each operator runs the bounded, read-only probe against the other organization's apex Matrix server name. First observe the delegation:

```bash
scripts/fed-check.sh partner.example
```

Confirm the reported `delegated_server` with the partner out of band. Then pin that exact value so an unexpected discovery change fails closed:

```bash
scripts/fed-check.sh --expect-server matrix.partner.example:443 partner.example \
  > partner-public-preflight.json
```

The script uses HTTPS only, follows no redirects, reads no credentials or `.netrc`, limits each request to 10 seconds by default, and caps response files at 64 KiB. It checks:

1. `https://partner.example/.well-known/matrix/server` returns a JSON object with a valid DNS `m.server` value and explicit port (normally `:443`). An omitted port invokes Matrix SRV/8448 discovery; this deliberately small preflight rejects that ambiguous branch instead of implementing a second federation resolver.
1. The observed delegation matches `--expect-server` when supplied.
1. `https://<m.server>/_matrix/federation/v1/version` returns non-empty software name and version fields.

The output deliberately sets `trust_level` to `public_unauthenticated_probe` and `governance_verified` to `false`. Store it in the private onboarding record, not in Git. A probe failure blocks activation until the owner explains and fixes the public boundary; bypassing TLS verification is not an accepted workaround.

When the partner will also export an A2A agent (section 6), run the staged onboarding preflight, which adds an opt-in AgentCard **conformance** stage on top of the same connectivity probe. Pin the partner's public P-256 signing JWK out of band first — a complete JOSE public JWK (`kty: EC`, `crv: P-256`, `alg: ES256`, `use: sig`, `key_ops: ["verify"]`, and the exported `kid`), never a bare-coordinate key — then:

```bash
scripts/fed-onboard.sh --expect-server matrix.partner.example:443 \
  --a2a-url https://a2a.partner.example/api/a2a/kagent/docs-qa \
  --public-jwk partner-agent-card.jwk.json \
  partner.example > partner-onboarding-preflight.json
```

The conformance stage fetches `<a2a-url>/.well-known/agent-card.json` under the same HTTPS-only, no-redirect, no-credential, size-bounded rules, then verifies the card's ES256/JCS JWS with the exact verifier the bridge uses (`scripts/sign-agent-card.sh` → `agentcardjws.Verify`), and checks that the card advertises the JSONRPC A2A v1.0 interface it is served at plus the required token-budget extension. It fails closed on an unsigned, tampered, wrong-key, wrong-interface, or missing-token-budget card. Each self-serve check maps to the operator-evidence gate it satisfies:

| Self-serve check (fed-onboard.sh)          | Evidence field                                 | Gate it feeds                           |
| ------------------------------------------ | ---------------------------------------------- | --------------------------------------- |
| Matrix delegation + federation version     | `connectivity`                                 | Section 4 DNS/TLS and closed-federation |
| Signed AgentCard verifies under pinned JWK | `agentcard_conformance.signature_verified`     | Section 6 Exported identity             |
| Advertised JSONRPC A2A v1.0 interface      | `agentcard_conformance.interface_url`          | Section 6 Route and Authorization       |
| Required token-budget extension            | `agentcard_conformance.token_budget_extension` | Section 6 Quota                         |

The record sets `governance_verified` to `false` throughout and `eligible_for_registry_review` to `true` only when both connectivity and conformance pass. That flag is evidence that the technical gates are satisfied; it is not a grant. Trust is granted only by an explicit, reviewed registry admission (the partner trust registry, [#349](https://github.com/fmind-ai/fgentic/issues/349)), and the contractual and privacy gates in section 7 remain human steps that no script can pass. A conformant public card proves the declared identity and route shape, never that the partner is governed, authorized, or contractually bound.

## 4. Matrix technical controls

Each operator supplies authenticated or local evidence for the following controls. Hash or link to the reviewed evidence rather than copying secrets or room content. The partner's entry in the trust registry (`infra/federation/registry/partners.yaml`, §8.2.1) is the single validated source these controls derive from: reference the one registry entry and its `check:fed-registry` result rather than five separately captured configurations, since the gate proves each plane already agrees with it.

| Gate                   | Required control                                                                                                                                                               | Evidence owner records                                               |
| ---------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | -------------------------------------------------------------------- |
| Trust registry         | The partner has exactly one registry entry (exact `server_name`, allowlist membership, admitted A2A `azp`/issuer, classification); `check:fed-registry` passes                 | Registry entry reference plus the clean `check:fed-registry` run     |
| DNS and TLS            | Apex `/.well-known` delegates to the agreed DNS server; publicly trusted certificate covers every advertised name; renewal is monitored                                        | Public preflight plus certificate/renewal monitor reference          |
| Closed federation      | `federation_domain_whitelist` contains only the local and approved partner server names; network ingress is restricted to partner addresses where stable addressing permits it | Redacted effective config and Git revision                           |
| Signing-key retrieval  | The approved partner is reachable as configured; any notary choice and failure behavior are documented                                                                         | Effective Synapse config and negative test                           |
| Room version           | Federated rooms use room version 12 or newer                                                                                                                                   | Authenticated room-version state query                               |
| Membership and history | Room is private; initial `m.room.join_rules` is `invite`, initial `m.room.history_visibility` is `joined`, and membership contains only approved users and agents              | Authenticated join-rules, history-visibility, and membership queries |
| Server ACL             | Initial `m.room.server_acl` permits only approved servers and sets `allow_ip_literals: false`                                                                                  | Authenticated room-state query                                       |
| Non-federated rooms    | Rooms outside the agreement are created with immutable `m.federate: false` state                                                                                               | Creation policy and representative state query                       |
| Policy border          | Synapse callbacks admit only agreed sender servers and event types and emit content-free denial evidence                                                                       | Policy digest, deployment revision, positive and negative test IDs   |
| Bridge sender policy   | Every invocable agent has an exact `allowedServers`/`allowedSenders` decision; federated senders remain deny-by-default                                                        | Reviewed agent mapping and denied mention evidence                   |
| Observability          | Alerts cover federation failures and policy denials without logging message content                                                                                            | Monitor references and alert-routing owner                           |

Before activation, both operators run their own deterministic checks and retain the clean revision:

```bash
mise run check
mise run test
```

Use `mise run fed:up` as the provider-free reference proof for Fgentic's room-v12, allowlist, ACL, callback, and denied-control behavior. It proves the repository contract, not the prospective partner's deployment.

## 5. Room and sender policy

Approve the smallest useful collaboration surface:

1. Name the exact rooms, owners, purpose, allowed data classification, retention class, and participating server names. Apply [ADR 0015](adr/0015-federated-room-encryption.md): v1 federated agent rooms may carry public or explicitly partner-approved non-public data only; restricted, regulated, secret, and authentication material is prohibited.
1. Create a new dedicated room rather than federating an existing history-bearing room.
1. Create it as private and install `m.room.join_rules: invite` plus `m.room.history_visibility: joined` in the initial state. Invite only the exact approved users and agents; a server ACL does not restrict users on an admitted server.
1. Install room version and `m.room.server_acl` state at creation so no ungoverned event window exists.
1. Invite a low-privilege test user from each organization and verify one message in each direction.
1. Attempt a join and event from an unapproved server; both must fail and produce content-free evidence.
1. Add agents only after the human collaboration controls pass. For every agent, record exact allowed Matrix senders/servers and a business owner.
1. Keep sensitive or unrelated rooms local with `m.federate: false`; removing a partner from an ACL does not retract history already replicated to its homeserver. Put the room's plaintext/replication warning, purpose, allowed class, and owner in visible room guidance.

## 6. A2A delegation agreement

Matrix room federation and direct A2A invocation are separate trust boundaries. If the organizations enable A2A delegation, agree and record all of the following before publishing a route:

| Gate               | Bilateral decision                                                                                                                |
| ------------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| Exported identity  | Exact agent name, provider organization, skill/interface, Signed AgentCard key ID, public JWK fingerprint, and rotation owner     |
| Transport identity | Exact OIDC issuer, audience, authorized party/client ID, and JWKS URI; or the equivalent mTLS subject/SAN and issuing CA          |
| Route              | Exact HTTPS origin and path; no prefix exposure and no direct public kagent Service                                               |
| Authorization      | Supported A2A methods, required extensions, input/data classification, and allowed consumer identity                              |
| Quota              | Reservation window, maximum token ceiling, rate limit, expected 429 behavior, and escalation contact                              |
| Accounting         | Reservation evidence is not described as measured token consumption; agree which downstream metrics or receipts are authoritative |
| Revocation         | Credential, certificate, and AgentCard-key rotation/revocation procedure plus maximum response time                               |
| Failure handling   | Timeout, retry, idempotency, task cancellation, and incident behavior                                                             |

Exchange only verify-only public keys and public discovery material. Private signing keys, client secrets, bearer tokens, and model credentials never cross organizations or enter Git. A Signed AgentCard authenticates the declared card under the pinned key; it does not replace transport authentication or prove the identity of the Matrix user who initiated a task.

Record the agreed quota, allowed classification, and residency as a **signed agreement artifact** (`infra/federation/agreements/<partner>.yaml`, detached ES256 signature) so the enforced values render from the signed contract and cannot silently diverge — see [federation §8.3.1](federation.md#831-signed-bilateral-agreement-as-the-enforcement-source). `mise run check:fed-agreement` fails closed on a tampered agreement, an [ADR 0015](adr/0015-federated-room-encryption.md)-out-of-bound classification, or a registry that disagrees with the signed terms.

## 7. Contractual and privacy gates

Legal and privacy owners must approve the following in the bilateral agreement. Matrix history is replicated to each participating homeserver and cross-server redaction is best-effort, so technical controls cannot substitute for these commitments.

| Gate                  | Decision to record                                                                                         |
| --------------------- | ---------------------------------------------------------------------------------------------------------- |
| Purpose and roles     | Processing purpose; controller/processor roles; permitted users, agents, and rooms                         |
| Residency             | Allowed storage, backup, log, and support-access regions for each organization                             |
| Retention             | Room events, attachments, logs, metrics, backups, and A2A task artifacts; deletion schedule and exceptions |
| Redaction/deletion    | Best-effort Matrix redaction expectations, local purge duties, backup expiry, and response evidence        |
| Data minimization     | Allowed classifications, prohibited data, prompt/artifact filtering, and content-free audit requirements   |
| Subprocessors         | Approved providers and change-notification/objection process, including model providers if used            |
| Security incident     | Notification window, evidence contacts, containment authority, and joint investigation process             |
| Data-subject requests | Intake, identity verification, search, export, correction, deletion, and bilateral coordination            |
| Audit and review      | Evidence access, control-review frequency, exception expiry, and material-change triggers                  |
| Termination           | Access revocation, room departure, route removal, retained-copy handling, and completion attestation       |

No production room or route is enabled while a required contractual field is blank, disputed, or awaiting approval.

## 8. Staged activation

Both operators execute and sign off each stage; a unilateral success is insufficient.

| Stage           | Organization A evidence                                                  | Organization B evidence                  | Exit condition                                             |
| --------------- | ------------------------------------------------------------------------ | ---------------------------------------- | ---------------------------------------------------------- |
| Public boundary | Pinned partner preflight                                                 | Pinned partner preflight                 | DNS/TLS/delegation agree in both directions                |
| GitOps controls | Reviewed allowlist, policy, route, and sender changes                    | Same                                     | Static checks pass on exact revisions                      |
| Contract        | Approved bilateral record                                                | Approved bilateral record                | All §7 gates have owners and decisions                     |
| Limited room    | Positive A↔B events and denied-control probe                             | Same event IDs observed locally          | Room state and policy evidence agree                       |
| Optional A2A    | Authorized call plus authentication, budget, quota, and tamper negatives | Corresponding provider/consumer evidence | Exact exported route works and every negative fails closed |
| Production      | Monitoring and incident contacts tested                                  | Same                                     | Both change owners approve activation                      |

Start with one dedicated, low-sensitivity room and no consequential agent actions. Expand only from measured need and reviewed evidence.

## 9. Rotation, incident response, and offboarding

Run a rotation rehearsal before production and at the agreed interval:

1. Introduce the replacement signing key, OIDC credential, or mTLS certificate without deleting the currently valid material.
1. Exchange and pin the replacement public identity through the approved channel.
1. Prove the new identity on the limited route, then revoke the old identity and prove it fails.
1. Record timestamps, owners, evidence IDs, and observed interruption; never record secret material.

On a suspected compromise, either organization's security owner may suspend the affected room/route first and investigate second. For a fast, reversible, evidenced containment use `mise run fed:break-glass contain <partner>` (registry-native break-glass — see the offboarding runbook [§3.1](federation-offboarding.md#31-break-glass-automation-fedbreak-glass)) and `mise run fed:evidence-pack <partner>` for the content-free regulator pack. Preserve content-free event IDs, policy digests, key IDs, Git revisions, and timestamps according to the agreement.

**Time-bounded trust and renewal rehearsal (issue #463).** Every admitted partner carries a `review_by` and optional `valid_until` in the trust registry and the signed agreement, so cross-org access is never indefinite. `review_by` raises the `FederationPartnerReviewDue` alert when the contracted control-review date nears; a passed `valid_until` raises `FederationPartnerAccessExpired` **and** fails `mise run check:fed-registry`/`check:fed-agreement` closed, blocking reconciliation of federation trust config until the partner is renewed or offboarded. Rehearse renewal at the agreed cadence: (1) confirm the alert fires as the window nears; (2) renew by editing the window and **re-signing** the agreement (`fed:agreement-render` then re-sign; the signature covers the new dates, so a window cannot change without re-signing); (3) confirm the check gates pass again and record the timestamps, owners, and evidence IDs. State the boundary honestly: an expired window blocks _reconciliation and renewal_ and raises the alert — it is **not** a silent live-traffic kill-switch. Immediate cutoff of an in-window partner is the break-glass path above, not expiry.

Offboard in this order (the dedicated [offboarding runbook](federation-offboarding.md) expands each plane's exact mechanism, evidence, partial revocation, and re-admission):

1. Stop new agent invocations and revoke the partner's transport identity and quota entry.
1. Remove bridge sender grants, public A2A routes, and Signed AgentCard discovery.
1. Remove the partner from room ACLs and federation allowlists, then make the affected rooms read-only or retire them as agreed.
1. Apply local purge, retention, backup-expiry, and contractual deletion duties. Do not claim that ACL or allowlist removal retracts already replicated history.
1. Prove former credentials, routes, joins, and sends fail; both operators sign the completion record.

## 10. Completion record

The onboarding record is complete only when it contains:

1. Both organizations' named owners and approvals.
1. Exact public preflight outputs with expected delegations pinned.
1. Reviewed Git revisions and static-test results from both deployments.
1. Authenticated room state and positive/negative event IDs without message content.
1. Optional A2A identity, route, quota, and revocation evidence.
1. Contractual decisions for every §7 gate.
1. Activation, review, rotation, incident, and offboarding dates.

Review the record after any server-name, delegation, certificate authority, signing key, identity provider, policy, model provider, data-classification, or contractual change.
