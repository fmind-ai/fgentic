# Persona onboarding

Choose the guide closest to the decision you own. Each is a short route into the authoritative specifications and manifests, not a substitute for them.

| Persona                 | Start with this question                                                                                  | Guide                                     | Deeper evidence                                                                                                                                                 |
| ----------------------- | --------------------------------------------------------------------------------------------------------- | ----------------------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Security lead           | Which identities, network paths, workloads, and tools can cross each trust boundary?                      | [Security lead](security-lead.md)         | [Security boundaries](../security.md), [threat model](../security/threat-model.md), [D6–D18](../design-decisions.md)                                            |
| Data-protection officer | Which content is stored, replicated, backed up, or sent to a model provider?                              | [DPO](dpo.md)                             | [Model data flow](../models.md#data-flow-and-residency), [federation §8](../federation.md), [ADR 0015](../adr/0015-federated-room-encryption.md)                |
| Platform engineer       | How is a reviewed revision reconciled, configured, observed, recovered, and upgraded?                     | [Platform engineer](platform-engineer.md) | [Production installation](../production.md), [observability](../observability.md), [exit strategy](../exit-strategy.md)                                         |
| Developer               | How do I declare one governed Agent and expose it as a Matrix ghost without giving it a model credential? | [Developer](developer.md)                 | [Bridge §6](../bridge.md#6-async-delegation-as-implemented), [`infra/kagent/`](../../infra/kagent/), [MCP boundary](../security.md#74-governed-mcp-tool-egress) |
| End user                | How do I find, invite, mention, and safely interpret an agent in a Matrix room?                           | [End user](end-user.md)                   | [Bridge behavior](../bridge.md), [D7/D8](../design-decisions.md)                                                                                                |

## Shared facts before evaluation

1. Fgentic is experimental and pre-1.0. A rendered manifest or successful demo is not production evidence.
1. Agent rooms are intentionally unencrypted. Federated-room content replicates to participating homeservers and is governed by the classification limits in [ADR 0015](../adr/0015-federated-room-encryption.md).
1. Authorization uses complete Matrix IDs and explicit policy. Display names and localparts are not authority; `X-User-Id` is attribution, not downstream authentication.
1. The selected model profile decides whether complete prompts and responses remain in-cluster or leave through agentgateway. No Agent or bridge holds a model credential.
1. Agent replies are `m.notice`. They are information for a human to review, not commands for other automation to execute.

## Publication status

These Markdown files are the source of truth. Publication through the planned documentation site remains tracked by [#72](https://github.com/fmind-ai/fgentic/issues/72); a source file in this repository is not evidence that the site has published it.
