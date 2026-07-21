# Example agent pack (extend Fgentic without forking)

This directory is a **reference** for issue #190: it shows that a third party can ship an agent for a Fgentic cluster from **their own git repository**, consumed as one extra Flux Kustomization, without touching the Fgentic core tree. It is not reconciled by any cluster in this repo — copy the shape into a standalone repository, or point the demo at it via a second `GitRepository` (see below).

See the full narrative in [docs/extending-without-forking.md](../../docs/extending-without-forking.md).

## What a pack contains

1. **`agent.yaml`** — a kagent `Agent` CRD (here a minimal, **tool-free** conversational agent). It holds no model credential (`modelConfig: agentgateway-model` → the single governed LLM chokepoint) and reuses the reviewed `agent-zoo-runtime` pod identity. The admission policy pins the governed MCP tool surface to the one reviewed `platform-helper` agent, so a pack ships tool-free; extending the platform's tool surface is a separate reviewed core-policy change, not something a pack grants itself.
1. **`agents-fragment.yaml`** — the mapping entry an operator merges into their cluster `agents.yaml` in a reviewed PR so the bridge routes to and authorizes the ghost `@agent-example-greeter`. It is deny-by-default (`allowedSenders`) and starts in `stage: dev` (the bridge's `STAGING_ROOMS` blast-radius boundary).
1. **`flux-kustomization.yaml`** — the `GitRepository` + `Kustomization` a cluster adds to consume the pack from its own repo. `prune: true` means deleting the Kustomization removes the agent — zero footprint.

## The review gates a pack must pass

An out-of-tree source relaxes **no** control. The pack's agent is admitted only if it clears the same gates as a core agent:

1. The fail-closed [`infra/policies/`](../../infra/policies) admission set (`approved-agent-references`) — `Declarative` in the `kagent` namespace, `agentgateway-model`, the reviewed `agent-zoo-runtime` pod identity with trace-content disabled, no `:latest`, and **no tools** (tools are pinned to the reviewed `platform-helper`). `mise run check:mcp-governance` independently rejects any uncataloged tool reference.
1. The bridge sender policy — the `agents.yaml` fragment's `allowedSenders` is an explicit full-MXID allowlist; federated and bridged senders stay deny-by-default.
1. The contract pin — `agentContractSHA256` in the mapping must match the effective Agent contract. It is produced and validated by the platform's reviewed agent tooling (`mise run agent:new` pins a governed agent's digest; `mise run check:agents` validates the pin and fails a split contract), not hand-written.

## Consuming it

1. Publish this pack (its `agent.yaml` and any prompt ConfigMap) in a git repo and tag it immutably.
1. Add `flux-kustomization.yaml` (with the real `url`/`ref`/`path`) to the cluster overlay.
1. Open a reviewed PR that merges `agents-fragment.yaml` into the cluster `agents.yaml`.
1. Invite `@agent-example-greeter` into a staging room, `@mention` it, and promote `stage: dev` → `prod` only after the golden gate and staging acceptance pass.

The demo-profile proof (the pack's agent answering from an external source, removable by deleting one Kustomization) and the Artifact Hub listing for the standalone chart are the runtime and account steps of #190; this reference and its docs are the preparable part.
