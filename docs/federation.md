# Federation Spec (formerly SPEC §8) — the flagship differentiator, milestone M8

Design position: **Matrix federation for the collaboration plane (humans + agents in shared rooms), A2A for the delegation plane (direct org-to-org machine calls)** — they compose rather than compete, and Fgentic ships both:

### 8.1 What Matrix federation gives — stated honestly

1. Identity is **org-level, not agent-level**: events are signed by the homeserver; org B can forge any `@user:org-b.com`, including its own agents. The honest claim is "cryptographically attributable to the partner organization". Per-agent identity is layered on with **A2A v1.0 Signed AgentCards** on the delegation plane.
1. Room state and history replicate **fully and irrevocably** to every participating homeserver; redaction cross-server is best-effort ("gentlemen's agreement" — Matrix Foundation's own words). Data residency is a **contractual** control (DPA + mutual retention policy), not a technical one. The spec says so out loud; enterprises respect honesty about this more than silence.

### 8.2 Required hardening (all git-declared)

1. **Room version 12 minimum** for any federated room (Hydra fix for malicious state resets — CVE-2025-49090 class).
1. **Closed federation**: Synapse `federation_domain_whitelist` (mutual, includes own domain + a reachable key notary), federation listener firewalled to partner IPs where feasible.
1. **Server ACLs** (`m.room.server_acl`) allowlisting partner servers per room; `m.federate: false` on rooms that must never federate (it is immutable at creation — set it deliberately, always).
1. **Synapse module callbacks** (`federated_user_may_invite`, `should_drop_federated_event`) as the programmatic policy border — this is Fgentic's open equivalent of the "Secure Border Gateway" that TI-Messenger mandates between parties (and which Element gates behind ESS Pro; ours is policy-as-code instead of a paid appliance).
1. **Bridge sender policy** (D6): per-agent `allowedServers`/`allowedSenders`; federated senders deny-by-default — already implemented and tested, ahead of Phase 6.

The executable lab encodes the first four controls as one defense-in-depth contract. Every Synapse release sets `default_room_version: "12"`. Organizations A and B each allow exactly their own server and the partner in `federation_domain_whitelist`; the partner also serves as the reachable `trusted_key_servers` notary, so the proof has no public notary dependency. The denied control server is deliberately absent from both allowlists.

Room creation is equally explicit. The bootstrap helper is the lab's supported federated-room constructor: it requests room version 12, sets `m.federate: true`, and installs an initial `m.room.server_acl` state event whose `allow` list contains only A and B and whose `allow_ip_literals` is `false`. Applying the ACL as initial state prevents an ungoverned-event race. A separate local-only room deliberately sets `m.federate: false`; this creation-time flag is the operational default for rooms that must never federate, because it cannot be changed later. Flux owns the homeserver configuration, while the bootstrap owns and verifies the per-room state.

`apps/synapse-federation-policy` is the programmatic border loaded on A and B, but deliberately not C. Its strict, versioned JSON policy names the exact admitted sender servers, exact federated event types, and invite rule. Unknown fields, malformed identifiers, duplicate values, an empty allowlist, an unreadable file, or an invalid replacement deny every new event. `federated_user_may_invite` rejects an invite whose sender server is not admitted; `should_drop_federated_event` rejects an unadmitted sender server or event type. Synapse does not pass the authenticated transaction origin to these callbacks, so `federation_domain_whitelist` remains the transport-origin control.

Each rejection emits one structured, content-free policy record with the reason, sender server, event type, room ID, event ID, policy digest, and policy counts; event content is never logged. Normal `/send` events reach the callback after signature and hash validation. Synapse invokes the invite endpoint's early drop callback before its signature check, so an invite-path record is diagnostic policy evidence, not authenticated attribution by itself; only the later accepted federation flow can supply that evidence.

The module source is projected from the immutable, versioned `fgentic-synapse-federation-policy-v1` ConfigMap. The operator-edited policy uses the stable `fgentic-federation-policy` name so Kubernetes can update its full-directory projection without a `subPath` mount or workload rollout. Both volumes reach the ESS config-check hook and Synapse runtime. The module detects an atomically projected policy replacement on the next callback, validates the whole file before use, and logs its digest. Flux remains the only field manager: changing `policy.json` in git and reconciling the new source revision changes the border without rebuilding Synapse.

Synapse 1.155.0 calls the drop callback once before staging an inbound PDU and may call it again while draining that room's staging queue. A policy tightening between those calls must not reverse the first admission: upstream's second-call drop path leaves the row in place and would otherwise select it forever. On a denial, the module therefore asks Synapse's database whether that exact `(room_id, event_id)` is already in `federation_inbound_events_staging`. An existing row is a local prior-admission fact and is allowed to drain; an absent row is a new event and remains denied. This query deliberately couples the workaround to the pinned Synapse schema without adding a table or retaining event content. Every Synapse upgrade must re-run the module and live reload gates; remove the workaround only after upstream deletes a staged row on its drop path.

### 8.3 Cross-org delegation plane

When org B's agents should be _invoked_ (not just conversed with): expose selected A2A endpoints through agentgateway on a dedicated Gateway listener with **JWT (OIDC federation between orgs) or mTLS** per A2A v1.0 security schemes, Signed AgentCard published at the well-known path, per-consumer token quotas. The bridge's agents map gains a `url:` variant (today it can only address kagent `namespace/name`) so remote signed agents become mentionable ghosts too.

### 8.4 E2EE revisit (supersedes ADR 0008's scope)

For federated rooms, "unencrypted" means the partner's server operators read everything, forever. Options, in order of preference: (a) keep federated agent rooms plaintext but **scoped** — dedicated per-project rooms, no sensitive-room federation, contractual controls (ship this first; it is what TI-Messenger-style deployments do in practice); (b) adopt appservice E2EE (mautrix crypto) later if demanded — acknowledged as officially "not recommended" and config-heavy. Document (a) as ADR 0008-bis when Phase 6 starts.

### 8.5 Disposable federation hardening lab

`mise run fed:up` is the executable M8 baseline: two independently named participating Matrix homeservers plus one denied control homeserver in the separately owned `fgentic-fed` k3d cluster. The lab deliberately uses one Kubernetes control plane. That still exercises Matrix discovery, TLS, server signing, event replication, federation authorization, and rejection of an untrusted server while keeping cross-cluster routing out of the first acceptance boundary. It does **not** claim infrastructure or failure-domain isolation; use separate clusters when testing independent networks, control planes, or disaster recovery.

| Organization | Matrix server name        | Namespace  | Synapse database | Lab role                |
| ------------ | ------------------------- | ---------- | ---------------- | ----------------------- |
| A            | `org-a.fgentic.localhost` | `matrix`   | `synapse`        | Federated participant   |
| B            | `org-b.fgentic.localhost` | `matrix-b` | `synapse_b`      | Federated participant   |
| C            | `org-c.fgentic.localhost` | `matrix-c` | `synapse_c`      | Denied negative control |

All three homeservers are Synapse-only ESS Community releases. They share one CloudNativePG cluster but have separate roles, databases, credentials, and namespace-local credential copies. Each server owns its apex `/.well-known/matrix/server` delegation and local-CA certificate; the public lab CA is mounted as outbound federation trust. No MAS, IdP, Matrix-to-A2A appservice, agent runtime, model endpoint, or provider account is part of this lab.

Federation is closed between the two participants: A and B each admit exactly A and B through `federation_domain_whitelist`, and each trusts the other participant as its signing-key notary. C is routable only so the acceptance test can make a real federation attempt; it is not admitted by either participant's domain allowlist or the shared room's server ACL. Each Gateway listener also accepts `HTTPRoute` attachments only from its owning Matrix namespace (and the HTTP redirect only from `gateway`). The duplicated controls are intentional: ingress ownership and homeserver policy limit federation globally, while room state remains an independently replicated authorization boundary.

Run the proof from a clean workstation with Docker, Git, and mise:

```bash
mise install
mise run fed:up
```

The command creates or reuses only the owned `fgentic-fed` cluster and reconciles the federation profile through Flux. It provisions Alice on A, Bob on B, and Charlie on C; verifies the room-v12, ACL, and explicit federation state; and requires a message from each participant to arrive through the other homeserver. For the negative paths, Charlie's join must fail, a locally accepted C-signed transaction first proves the probe signature is valid, and that same signed federation send must be forbidden by both A and B. Bob then sends a disallowed custom event as the final event in a dedicated throwaway room: B must retain it, A must return `M_NOT_FOUND`, and A must log the exact content-free denial record without the event's unique content sentinel. Keeping this probe final avoids extending a room DAG after one participant deliberately dropped an event. The command leaves the cluster running so the homeservers, room, and reconciliation state can be inspected. No provider connection or paid service is used.

Run `mise run fed:policy-reload` for the stronger reconciliation proof. It starts from the canonical deny policy, records both Synapse pod UIDs, publishes an ephemeral local-git revision that admits only the probe event type, proves the event reaches A, and then reconciles the canonical deny policy again. The same pods must serve all three revisions, and the final denial must pass again. A failed drill deletes only the owned disposable lab rather than leaving the relaxed probe policy active.

Remove the lab when inspection is complete:

```bash
mise run fed:down
```

Teardown is ownership-guarded and removes only the disposable federation cluster and its locally built images. The normal local, demo, and production profiles are separate and remain untouched.
