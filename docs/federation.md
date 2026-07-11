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
