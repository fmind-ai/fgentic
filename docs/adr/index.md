# Architecture Decision Records

Settled designs (including the D1–D20 register) are revisited by proposing a new ADR, never a drive-by PR. Structure and authoring rules: `.agents/skills/docs-spec/SKILL.md`.

- [0001 — Open-Standard Agent Collaboration Platform](0001-open-standard-agent-platform.md) - build exclusively on open protocols and OSS; every layer swappable
- [0002 — Matrix as the Human↔Agent Collaboration Fabric](0002-matrix-collaboration-fabric.md) - federated Matrix rooms as the shared collaboration surface
- [0003 — Synapse + MAS + Element via ESS Community](0003-synapse-mas-element-ess.md) - reference homeserver profile, with governed fallback triggers
- [0004 — A2A Delegation, Non-Streaming, via a2a-go](0004-a2a-delegation.md) - original non-streaming delegation decision on the official SDK; task polling added later
- [0005 — Bridge as a mautrix/go Appservice](0005-matrix-a2a-bridge-appservice.md) - plain appservice, not bridgev2
- [0006 — agentgateway as the Egress Chokepoint](0006-agentgateway-chokepoint.md) - no agent holds a model credential
- [0007 — Shared CloudNativePG, Database-per-Service](0007-shared-postgres-db-per-service.md) - one cluster, scoped database + role per service
- [0008 — Agent Rooms Unencrypted, Enforced Server-Side](0008-unencrypted-agent-rooms.md) - by policy, revisited for federated rooms
- [0009 — Agent Authorization Through Managed Room Membership](0009-agent-authorization-model.md) - invocation access through room membership, not a parallel invocation ACL
- [0010 — Defer SPIFFE Workload Identity](0010-defer-spiffe-workload-identity.md) - until both protocol endpoints can consume it
- [0011 — Coexist with Microsoft Teams; No Production Bridge](0011-teams-coexistence-not-bridge.md) - coexistence over a fragile bridge promise
- [0012 — Bridge Decomposition and Surface Budget](0012-bridge-decomposition-surface-budget.md) - cap what the bridge core may grow
- [0013 — Federation Lab as the Permanent Acceptance Rig](0013-federation-lab-acceptance-rig.md) - provider-free lab gates cross-org changes
- [0014 — ActivityPub as a Second Federation Transport](0014-activitypub-second-federation-transport.md) - additive AP transport in a self-contained app, Proposed (M18)
- [0015 — Federated Agent Rooms Remain Unencrypted for v1](0015-federated-room-encryption.md) - purpose-scoped plaintext rooms with classification and bilateral controls
- [0016 — Durable Delegation Ledger with At-Most-Once A2A Recovery](0016-durable-delegation-ledger.md) - pre-ACK jobs, fenced recovery, explicit A2A ambiguity, and deterministic Matrix projection
- [0017 — Permission-Aware Retrieval Identity Binding](0017-permission-aware-identity-binding.md) - gateway-projected identity and complete output-audience ACL prefiltering
- [0018 — Content-Bounded Matrix Identity Audit](0018-content-bounded-identity-audit.md) - opt-in authentication/event evidence from exact pinned source records, never generic logs
- [0019 — Snapshot-Backed Synapse Media PVC](0019-synapse-media-store.md) - retained local PVC and explicit CSI snapshot recovery for the GKE reference
- [0020 — Retain Bash Acceptance Rigs Until a Measured Pilot Trigger](0020-retain-bash-acceptance-rigs.md) - keep ShellCheck-gated rigs until one bounded Go pilot meets explicit evidence thresholds
- [0021 — Out-of-Band Pinned Key Resolution for the ActivityPub Border](0021-pinned-key-resolution.md) - verify operator-pinned in-cluster signers without a network fetch; unpinned actors stay on the unchanged SSRF guard
