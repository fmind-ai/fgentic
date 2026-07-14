---
type: Architecture Decision Record
title: Federation Lab as the Permanent Acceptance Rig
description: Keep the provider-free federation lab as the permanent acceptance rig for cross-org changes.
---

# 0013 — Federation Lab as the Permanent Acceptance Rig

Status: Proposed

## Context

The `fgentic-fed` lab ([docs/federation.md](../federation.md) §8.5) is documented as a **disposable** hardening lab: Synapse-only, provider-free, deliberately without a Matrix appservice or bridge. The backlog has outgrown that framing: thirteen open issues across M8, M16, and parts of M2/M5/M6 anchor their acceptance criteria on the lab and its `seed-federation.sh`/`test-federation.sh` scripts — and three of them (#120, #155, #167, plus half of #142) require agent **ghosts acting inside federated rooms**, which the bridgeless lab cannot satisfy as written. The lab is already the project's cross-org acceptance environment in practice; the code pretends otherwise.

## Decision

1. The lab graduates to the **permanent, provider-free acceptance rig** for the cross-organization planes: its manifests and scripts are maintained code with the same review bar as the platform, not throwaway demo material. "Disposable" continues to describe the **cluster lifecycle** (`fed:up`/`fed:down` create and remove it wholesale), not the code.
1. The **baseline `fed:up` stays exactly as it is**: Synapse-only, bridge-free, minimal — its existing proofs (closed federation, policy border, signed-card delegation, quota) are unchanged and remain the default invocation.
1. A new **opt-in agents component** (`infra/federation/agents/`, tracked by #185) deploys the bridge with an appservice registration and one deterministic ghost mapping on org A. It is enabled explicitly (flag or environment variable on `fed:up`), and every ghost-dependent acceptance criterion (#120, #155, #167, #142's refusal half) runs only under that mode.
1. Acceptance criteria in issues must name which lab mode they need; criteria that only need the bridge's behavior without federation continue to use the kind fixture instead of the lab.
1. **Not done:** the default rig stays one Kubernetes control plane (the honest limit §8.5 already states); proving two independent control planes is a separate drill (#187), not a change to this rig.

## Consequences

1. The three unsatisfiable acceptance criteria found by the backlog audit become executable, without inflating the baseline proof every M8 contributor depends on.
1. The lab's maintenance cost rises deliberately: it is now load-bearing for three milestones, and changes to it require the same warning-free gates as everything else.
1. `docs/federation.md` §8.5 wording is updated when #185 lands (permanent rig, opt-in agents mode, disposability = cluster lifecycle).
1. Escape hatch: if the agents component destabilizes the baseline proofs, it moves to a separately-owned profile (`fed-agents:up`) rather than back into acceptance-by-fixture — the ghost-in-federated-room proofs are the thesis and must stay executable somewhere real.
