# Vetted MCP catalog

This directory is the human- and machine-readable allowlist of MCP servers that Fgentic can route to an Agent. Each subdirectory contains one versioned [`server.json`](https://github.com/modelcontextprotocol/registry/blob/main/docs/reference/server-json/generic-server-json.md) document using the pinned `2025-12-11` official registry schema.

The catalog stays as plain git data. `mise run check:mcp-governance` consumes it while rendering the agentgateway and kagent resources; adding an MCP backend or `RemoteMCPServer` without an exact `fgentic.dev/mcp-catalog-entry` annotation fails. There is no catalog Deployment, writable API, or second source of runtime truth.

## Vetting checklist

Before adding or changing an entry:

1. **Source and maintenance:** inspect the canonical repository, record its stable forge ID and immutable reviewed revision, and confirm that the release is maintained rather than abandoned or a prerelease selected by convenience.
1. **License:** record the detected SPDX license and reject a server whose obligations are incompatible with the platform or its intended deployment.
1. **Executable provenance:** pin the deployed image by digest. A tag alone is not review evidence.
1. **Transport and authentication:** expose only the governed agentgateway route; document its authentication input without recording a credential. Keep the backend and direct Agent-to-server NetworkPolicy boundaries fail-closed.
1. **Tools and injection:** review initialization instructions plus every tool description, annotation, and input schema as untrusted model-facing content. Link the complete surface pin and list only the tools the gateway authorizes.
1. **Least privilege:** verify server flags, RBAC, namespaces, Secret access, write behavior, and the exact Agent allowlist. Metadata such as `readOnlyHint` is not enforcement.
1. **Approval:** record the approving maintainer group and UTC date. A catalog change is effective only through normal review and passing checks.

The current `kagent-tools` entry is Apache-2.0, digest-pinned, run with `--read-only`, constrained by namespace-scoped RBAC, and reduced to five read-only operations by agentgateway. Its raw surface still advertises broader and pessimistic annotations; the catalog links the exact reviewed surface instead of restating or sanitizing it.

## Why not agentregistry yet?

[agentregistry](https://github.com/agentregistry-dev/agentregistry) is an active Apache-2.0 project and the natural future candidate when Fgentic needs searchable multi-artifact discovery, publication, or lifecycle management. The latest stable release evaluated on 2026-07-15 was `v0.3.3`. Its primary workflow adds a daemon and imperative `arctl apply`/deployment operations that then configure agentgateway; Helm installation exists, but adopting the service today would add a pre-1.0 stateful control plane beside Flux for one static server.

Re-evaluate it when a second independent server, delegated catalog administration, or client-facing discovery makes plain reviewed git documents insufficient. Until then, git + the existing Flux render gate is the smaller sovereign control.
