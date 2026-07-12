# Public Surface Stability Contract

Fgentic mints public surfaces beyond its code: extension URIs partners pin, event namespaces clients parse, schemas operators write. Anyone building on the platform needs to know which of these are contracts and which are experiments. This registry is the single source of truth; adding or changing a public surface requires a PR touching this file.

## Tiers

1. **Stable** — versioned; breaking changes require a new major version of the surface, a deprecation entry here, and an upgrade note in the release contract (#188). The old version keeps working for at least one minor release.
1. **Beta** — may change between releases; every change ships a migration note. Safe to build on with pinning.
1. **Experimental** — may change or disappear without notice; namespaces are reserved here so they never collide, but nothing else is promised.

## Registry

| Surface                                                                                          | Tier         | Defined in                                         | Notes                                                                                                               |
| ------------------------------------------------------------------------------------------------ | ------------ | -------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------- |
| Delegation audit schema `fgentic.delegation.v1`                                                  | Stable       | [docs/audit.md](audit.md)                          | Content-free by design; schema stability is an explicit deliverable of #37                                          |
| A2A extension URI `…/a2a/extensions/token-budget/v1`                                             | Stable       | [docs/bridge.md](bridge.md) §6                     | Partner-enforced request contract; already pinned by remote configurations                                          |
| A2A extension URIs for quotes, receipts, mandate profiles                                        | Experimental | #142, #141, #144                                   | Reserved under `…/a2a/extensions/`; versioned from first release; fgentic-specific, never presented as standard A2A |
| `agents.yaml` schema                                                                             | Beta         | [docs/bridge.md](bridge.md), #189                  | `schemaVersion` field and published JSON Schema tracked by #189; moves to Stable when shipped                       |
| Matrix namespaces `ai.fgentic.*` (profile fields, task state events, structured result metadata) | Experimental | #120, #121, #167                                   | Reserved namespace; content-free rule applies to state events; schemas versioned in-body from first release         |
| Prometheus metric names `fgentic_*`                                                              | Beta         | [docs/observability.md](observability.md)          | Renames ship with dashboard/alert updates and a migration note                                                      |
| `mise` task vocabulary (`demo:up`, `fed:up`, `fed:down`, `fed:policy-reload`, …)                 | Beta         | `mise.toml`, README                                | Documented commands keep working within a minor line; removals get a deprecation cycle                              |
| Bridge Helm chart values                                                                         | Beta         | `apps/matrix-a2a-bridge/chart/`                    | Standalone consumption tracked by #190; values changes ship migration notes per the release contract                |
| Federation lab acceptance interface (`fed:up` proofs, agents mode)                               | Beta         | [docs/federation.md](federation.md) §8.5, ADR 0013 | The lab is the permanent acceptance rig; its baseline proof set only grows deliberately                             |

## Policy

1. New public surfaces start Experimental with a reserved namespace and an owning issue; promotion to Beta/Stable happens in the PR that ships the guarantee.
1. A breaking change to a Stable surface is a design change: it needs the deprecation entry here, the release-contract upgrade note (#188), and — where it touches a settled decision — an ADR.
1. Internal interfaces (Go packages, unexported config, cluster-internal Services) are deliberately absent: absence from this table means **no contract**.
