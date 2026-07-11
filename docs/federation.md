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

### 8.3 Cross-org delegation plane

When org B's agents should be _invoked_ (not just conversed with): expose selected A2A endpoints through agentgateway on a dedicated Gateway listener with **JWT (OIDC federation between orgs) or mTLS** per A2A v1.0 security schemes, Signed AgentCard published at the well-known path, per-consumer token quotas. The bridge's agents map gains a `url:` variant (today it can only address kagent `namespace/name`) so remote signed agents become mentionable ghosts too.

### 8.4 E2EE revisit (supersedes ADR 0008's scope)

For federated rooms, "unencrypted" means the partner's server operators read everything, forever. Options, in order of preference: (a) keep federated agent rooms plaintext but **scoped** — dedicated per-project rooms, no sensitive-room federation, contractual controls (ship this first; it is what TI-Messenger-style deployments do in practice); (b) adopt appservice E2EE (mautrix crypto) later if demanded — acknowledged as officially "not recommended" and config-heavy. Document (a) as ADR 0008-bis when Phase 6 starts.

### 8.5 Disposable two-homeserver federation lab

`mise run fed:up` is the executable M8 baseline: two independently named Matrix homeservers in the separately owned `fgentic-fed` k3d cluster. The lab deliberately uses one Kubernetes control plane. That still exercises Matrix discovery, TLS, server signing, event replication, and federation authorization while keeping cross-cluster routing out of the first acceptance boundary. It does **not** claim infrastructure or failure-domain isolation; use two clusters when testing independent networks, control planes, or disaster recovery.

| Organization | Matrix server name        | Namespace  | Synapse database |
| ------------ | ------------------------- | ---------- | ---------------- |
| A            | `org-a.fgentic.localhost` | `matrix`   | `synapse`        |
| B            | `org-b.fgentic.localhost` | `matrix-b` | `synapse_b`      |

Both homeservers are Synapse-only ESS Community releases. They share one CloudNativePG cluster but have separate roles, databases, credentials, and namespace-local credential copies. Each server owns its apex `/.well-known/matrix/server` delegation and local-CA certificate; the public lab CA is mounted as outbound federation trust. No MAS, IdP, Matrix-to-A2A appservice, agent runtime, model endpoint, or provider account is part of this lab.

Federation is closed in both directions: each Synapse `federation_domain_whitelist` contains exactly the two lab server names. The lab disables public signing-key notaries with `trusted_key_servers: []`, so each server retrieves its partner's signing key directly. That direct-lookup choice is a local-lab exception, not the final trust policy; [issue #52](https://github.com/fmind-ai/fgentic/issues/52) owns the hardened room and federation policy layer.

Run the proof from a clean workstation with Docker, Git, and mise:

```bash
mise install
mise run fed:up
```

The command creates or reuses only the owned `fgentic-fed` cluster, reconciles the federation profile, provisions Alice on A and Bob on B, creates a federated room, and requires a message from each user to arrive through the other homeserver before it succeeds. It leaves the cluster running so the homeservers, room, and reconciliation state can be inspected. No provider connection or paid service is used.

Remove the lab when inspection is complete:

```bash
mise run fed:down
```

Teardown is ownership-guarded and removes only the disposable federation cluster and its locally built images. The normal local, demo, and production profiles are separate and remain untouched.
