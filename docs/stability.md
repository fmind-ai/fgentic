---
type: Contract
title: Public Surface Stability Contract
description: Stability tiers for the public surfaces partners pin: extension URIs, event namespaces, schemas.
---

# Public Surface Stability Contract

Fgentic mints public surfaces beyond its code: extension URIs partners pin, event namespaces clients parse, schemas operators write. Anyone building on the platform needs to know which of these are contracts and which are experiments. This registry is the single source of truth; adding or changing a public surface requires a PR touching this file. Operators apply these tiers through the [Day-2 Operations Handbook](operations-handbook.md) upgrade procedure.

## Tiers

1. **Stable** — versioned; breaking changes require a new major version of the surface, a deprecation entry here, and an upgrade note in the release contract (#188). The old version keeps working for at least one minor release.
1. **Beta** — may change between releases; every change ships a migration note. Safe to build on with pinning.
1. **Experimental** — may change or disappear without notice; namespaces are reserved here so they never collide, but nothing else is promised.

## Registry

| Surface                                                                                                     | Tier         | Defined in                                                                                      | Notes                                                                                                               |
| ----------------------------------------------------------------------------------------------------------- | ------------ | ----------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Delegation audit schema `fgentic.delegation.v1`                                                             | Stable       | [docs/audit.md](audit.md)                                                                       | Content-free by design; schema stability is an explicit deliverable of #37                                          |
| A2A extension URI `…/a2a/extensions/token-budget/v1`                                                        | Stable       | [docs/bridge.md](bridge.md) §6                                                                  | Partner-enforced request contract; already pinned by remote configurations                                          |
| A2A extension URIs for quotes, receipts, mandate profiles                                                   | Experimental | #142, #141, #144                                                                                | Reserved under `…/a2a/extensions/`; versioned from first release; fgentic-specific, never presented as standard A2A |
| `agents.yaml` schema v1                                                                                     | Stable       | [agents.schema.json](../apps/matrix-a2a-bridge/agents.schema.json), [docs/bridge.md](bridge.md) | Explicit `schemaVersion: 1`; omitted version is a deprecated v1 compatibility path and unknown majors fail closed   |
| Matrix namespaces `ai.fgentic.*` (profile fields, task state events, structured result metadata)            | Experimental | #120, #121, #167                                                                                | Reserved namespace; content-free rule applies to state events; schemas versioned in-body from first release         |
| Prometheus metric names `fgentic_*`                                                                         | Beta         | [docs/observability.md](observability.md)                                                       | Renames ship with dashboard/alert updates and a migration note                                                      |
| `mise` task vocabulary (`demo:up`, `fed:up`, `fed:up:constrained`, `fed:status`, `fed:stop`, `fed:down`, …) | Beta         | `mise.toml`, README                                                                             | Documented commands keep working within a minor line; removals get a deprecation cycle                              |
| Bridge Helm chart values                                                                                    | Beta         | `apps/matrix-a2a-bridge/chart/`                                                                 | Standalone consumption tracked by #190; values changes ship migration notes per the release contract                |
| Federation lab acceptance interface (`fed:up` proofs, constrained mode, lifecycle, resource trace)          | Beta         | [docs/federation.md](federation.md) §8.5, ADR 0013                                              | The canonical proof remains the baseline; constrained mode changes capacity and install order, not the proof set    |

## Policy

1. New public surfaces start Experimental with a reserved namespace and an owning issue; promotion to Beta/Stable happens in the PR that ships the guarantee.
1. A breaking change to a Stable surface is a design change: it needs the deprecation entry here, the release-contract upgrade note (#188), and — where it touches a settled decision — an ADR.
1. The Stable `agents.yaml` schema uses major versions in `schemaVersion`: additive fields evolve within the current minor release, while removals or incompatible meaning changes require a new major plus an upgrade note. The bridge never guesses how to read an unknown major.
1. Internal interfaces (Go packages, unexported config, cluster-internal Services) are deliberately absent: absence from this table means **no contract**.
