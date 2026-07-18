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

The `v1.0.0 decision` column is binding input to the [v1.0.0 release gate](../CONTRIBUTING.md#v100-readiness-gate). A `Promote to Stable` row changes tier only in the release PR that ships `v1.0.0`; until then its current tier remains authoritative.

| Surface                                                                                                     | Current tier | `v1.0.0` decision   | Defined in                                                                                      | Rationale / guarantee                                                                                                   |
| ----------------------------------------------------------------------------------------------------------- | ------------ | ------------------- | ----------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| Delegation audit schema `fgentic.delegation.v1`                                                             | Stable       | Retain Stable       | [docs/audit.md](audit.md)                                                                       | Content-free by design; schema stability is an explicit deliverable of #37.                                             |
| A2A extension URI `…/a2a/extensions/token-budget/v1`                                                        | Stable       | Retain Stable       | [docs/bridge.md](bridge.md) §6                                                                  | Partner-enforced request contract already pinned by remote configurations.                                              |
| A2A extension URI for per-skill quotes                                                                      | Experimental | Remain Experimental | #142                                                                                            | The extension is implemented, but no external adopter has accepted its bilateral credit-unit contract yet.              |
| A2A extension URI for signed usage receipts                                                                 | Experimental | Remain Experimental | #141                                                                                            | The shipped proof cannot attribute actual per-client consumption or countersign receipts; its schema may still evolve.  |
| A2A extension URI for mandate profiles                                                                      | Experimental | Remain Experimental | #144                                                                                            | The AP2-shaped authorization chain is vision-scoped and not implemented; the namespace remains reserved only.           |
| `agents.yaml` schema v1                                                                                     | Stable       | Retain Stable       | [agents.schema.json](../apps/matrix-a2a-bridge/agents.schema.json), [docs/bridge.md](bridge.md) | Explicit `schemaVersion: 1`; omitted version is a deprecated v1 compatibility path and unknown majors fail closed.      |
| Matrix namespaces `ai.fgentic.*` (profile fields, task state events, structured result metadata)            | Experimental | Remain Experimental | #120, #121, #167                                                                                | The namespace is reserved, but the owning schemas are not all delivered or accepted for stable external consumption.    |
| Prometheus metric names `fgentic_*`                                                                         | Beta         | Promote to Stable   | [docs/observability.md](observability.md)                                                       | Adopter dashboards and alerts pin these names; after 1.0, a rename requires a versioned replacement and deprecation.    |
| `mise` task vocabulary (`demo:up`, `fed:up`, `fed:up:constrained`, `fed:status`, `fed:stop`, `fed:down`, …) | Beta         | Promote to Stable   | `mise.toml`, README                                                                             | These commands are the operator and acceptance interface; after 1.0, replacement commands overlap for one minor.        |
| Bridge Helm chart values                                                                                    | Beta         | Remain Beta         | `apps/matrix-a2a-bridge/chart/`                                                                 | Standalone consumption and its compatibility boundary remain tracked by #190; every change still needs migration notes. |
| Federation lab acceptance interface (`fed:up` proofs, constrained mode, lifecycle, resource trace)          | Beta         | Promote to Stable   | [docs/federation.md](federation.md) §8.5, ADR 0013                                              | The definitive-v1 federation claim depends on this proof contract; constrained mode may not weaken the proof set.       |

## Policy

1. New public surfaces start Experimental with a reserved namespace and an owning issue; promotion to Beta/Stable happens in the PR that ships the guarantee.
1. A breaking change to a Stable surface is a design change: it needs the deprecation entry here, the release-contract upgrade note (#188), and — where it touches a settled decision — an ADR.
1. The Stable `agents.yaml` schema uses major versions in `schemaVersion`: additive fields evolve within the current minor release, while removals or incompatible meaning changes require a new major plus an upgrade note. The bridge never guesses how to read an unknown major.
1. The `v1.0.0` release PR must apply every `Promote to Stable` decision above and leave every retained Beta or Experimental rationale intact. A decision change requires maintainer acceptance in #471 before the release tag.
1. Internal interfaces (Go packages, unexported config, cluster-internal Services) are deliberately absent: absence from this table means **no contract**.
