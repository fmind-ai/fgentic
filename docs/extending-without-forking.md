---
type: Guide
title: Extending Fgentic Without Forking
description: Run the bridge from the signed standalone chart and ship agents from your own repository as an out-of-tree pack, consumed as one extra Flux Kustomization.
---

# Extending Fgentic Without Forking

Running or extending Fgentic must not require forking the monorepo. Two composition seams make that true, and both reuse controls that already exist — the CD-signed chart and the governed agent admission path (issue #190):

1. **Run the bridge from the signed standalone chart** published to GHCR.
1. **Ship agents from your own repository** as an out-of-tree pack, consumed as one extra Flux Kustomization — third parties extend the platform without touching core.

Neither seam relaxes a control: the chart is cosign-signed and consumed by immutable digest, and an out-of-tree agent passes the same admission policies and MCP governance as a core agent.

## Consume the bridge chart standalone

CD builds the multi-arch distroless bridge image and packages the Helm chart as an OCI artifact, cosign-signs both, and commits the image digest back into the deploy unit. The chart at [`apps/matrix-a2a-bridge/chart/`](../apps/matrix-a2a-bridge/chart) has **no monorepo-relative assumptions** — it renders from its own `values.yaml` alone (`helm template matrix-a2a-bridge apps/matrix-a2a-bridge/chart` produces a complete, valid manifest set with the default in-cluster Service URLs).

Verify the signature before installing (never install an unverified chart):

```bash
# Verify the OCI chart artifact's keyless cosign signature, then install by digest. The identity and
# issuer regexps mirror the exact subject CD signs with (cd.yml / the Flux OCIRepository verifier).
cosign verify ghcr.io/fmind-ai/charts/matrix-a2a-bridge@<digest> \
  --certificate-identity-regexp '^https://github\.com/fmind-ai/fgentic/\.github/workflows/cd\.yml@refs/heads/main$' \
  --certificate-oidc-issuer-regexp '^https://token\.actions\.githubusercontent\.com$'
helm install matrix-a2a-bridge oci://ghcr.io/fmind-ai/charts/matrix-a2a-bridge --version <version>
```

The values you will set are documented inline in [`values.yaml`](../apps/matrix-a2a-bridge/chart/values.yaml); the load-bearing ones:

1. `config.serverName` — your Matrix `server_name`.
1. `config.homeserverURL` / `config.a2aBaseURL` / `config.kagentAPIURL` — the in-cluster Service URLs for Synapse, agentgateway (the LLM egress chokepoint the bridge routes A2A through), and the kagent controller. The defaults target the ESS release name `ess` and the platform namespaces.
1. `image.tag` — pin an **immutable digest**; it defaults to the chart's `appVersion`, and CD stamps the released digest.
1. The appservice registration secret (`as_token`/`hs_token`) must be **identical** in the bridge and in Synapse — one Secret referenced from both namespaces.

The chart is one component: it expects a homeserver (ESS or a Tuwunel/continuwuity profile), agentgateway, and kagent to exist, but it does not assume they were installed from this repo.

## Ship an agent from your own repository

An **agent pack** is a small out-of-tree bundle a third party publishes and a cluster consumes without editing core. The reference lives at [`examples/agent-pack/`](../examples/agent-pack); its [README](../examples/agent-pack/README.md) is the copyable template. A pack contains:

1. A kagent `Agent` CRD (`agent.yaml`) — declarative, holding **no** model credential (`modelConfig: agentgateway-model` routes every call through the governed LLM chokepoint) and **reusing** the reviewed `agent-zoo-runtime` pod identity with the trace-content paths disabled (the admission policy pins pod identity — a pack cannot bring its own ServiceAccount), plus any prompt ConfigMap.
1. The `agents.yaml` mapping fragment (`agents-fragment.yaml`) — the ghost→agent routing, `dataClassification`, and the bridge's deny-by-default sender allowlist, merged into the cluster `agents.yaml` in a reviewed PR.
1. A Flux `GitRepository` + `Kustomization` (`flux-kustomization.yaml`) — pointing at the pack's own tagged repo. `prune: true` means removing the agent is deleting one Kustomization; it leaves zero footprint.

### The pack passes the same gates as core

An out-of-tree source grants no shortcut — it is held to the _same, deliberately strict_ controls as core:

1. The fail-closed [`infra/policies/`](../infra/policies) admission set (`approved-agent-references`) — the Agent must be `Declarative` in the `kagent` namespace, use `agentgateway-model`, reuse the exact reviewed `agent-zoo-runtime` pod identity with the three GenAI trace-content paths disabled, and declare **no tools**. The policy pins the entire governed MCP tool surface to the one reviewed `platform-helper` agent, so a pack ships tool-free; extending the platform's tool surface is itself a separate reviewed core-policy change, not something a pack grants itself. `mise run check:mcp-governance` independently rejects any uncataloged tool reference.
1. The bridge sender policy — `allowedSenders` is an explicit full-MXID allowlist; federated and bridged senders stay deny-by-default ([D6](design-decisions.md)).
1. The contract pin — the `agents.yaml` mapping carries `agentContractSHA256`, the digest of the effective Agent contract, which the bridge records for every delegation and which must match the rendered Agent. Do not hand-write it: it is produced and validated by the platform's reviewed agent tooling (`mise run agent:new` scaffolds and pins the digest for a governed agent; `mise run check:agents` validates that a mapping's pin matches its effective contract and fails a split contract). Wiring a pack's Agent into that contract flow is part of the reviewed integration PR that adds its mapping — the same governance a core agent passes.

### Lifecycle

Publish and immutably tag the pack repo; add its `GitRepository`+`Kustomization` to the cluster overlay; merge the `agents.yaml` fragment in a reviewed PR; then invite the ghost into a staging room, `@mention` it, and promote `stage: dev` → `prod` only after the golden gate and staging acceptance pass — the same [add-an-agent runbook](../.agents/skills/matrix-agents/SKILL.md#runbook-add-an-agent) as a core agent, just sourced externally. See [CONTRIBUTING.md](../CONTRIBUTING.md) for the contribution and review conventions.

The live demo-profile proof (the pack's agent answering from an external source, removable by deleting one Kustomization) and the Artifact Hub listing for the standalone chart are the runtime and maintainer-account steps of #190; this guide and the reference pack are the preparable part.
