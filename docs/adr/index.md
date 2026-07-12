# Architecture Decision Records

Settled designs (including the D1–D16 register) are revisited by proposing a new ADR, never a drive-by PR. Structure and authoring rules: `.agents/skills/docs-spec/SKILL.md`.

- [0001 — Open-Standard Agent Collaboration Platform](0001-open-standard-agent-platform.md) - build exclusively on open protocols and OSS; every layer swappable
- [0002 — Matrix as the Human↔Agent Collaboration Fabric](0002-matrix-collaboration-fabric.md) - federated Matrix rooms as the shared collaboration surface
- [0003 — Synapse + MAS + Element via ESS Community](0003-synapse-mas-element-ess.md) - reference homeserver profile, with governed fallback triggers
- [0004 — A2A Delegation, Non-Streaming, via a2a-go](0004-a2a-delegation.md) - message/send + tasks/get polling on the official SDK
- [0005 — Bridge as a mautrix/go Appservice](0005-matrix-a2a-bridge-appservice.md) - plain appservice, not bridgev2
- [0006 — agentgateway as the Egress Chokepoint](0006-agentgateway-chokepoint.md) - no agent holds a model credential
- [0007 — Shared CloudNativePG, Database-per-Service](0007-shared-postgres-db-per-service.md) - one cluster, scoped database + role per service
- [0008 — Agent Rooms Unencrypted, Enforced Server-Side](0008-unencrypted-agent-rooms.md) - by policy, revisited for federated rooms
- [0009 — Agent Authorization Through Managed Room Membership](0009-agent-authorization-model.md) - no parallel ACL system
- [0010 — Defer SPIFFE Workload Identity](0010-defer-spiffe-workload-identity.md) - until both protocol endpoints can consume it
- [0011 — Coexist with Microsoft Teams; No Production Bridge](0011-teams-coexistence-not-bridge.md) - coexistence over a fragile bridge promise
- [0012 — Bridge Decomposition and Surface Budget](0012-bridge-decomposition-surface-budget.md) - cap what the bridge core may grow
- [0013 — Federation Lab as the Permanent Acceptance Rig](0013-federation-lab-acceptance-rig.md) - provider-free lab gates cross-org changes
