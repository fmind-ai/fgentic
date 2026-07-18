# A2A Integration submission draft

Status: prepared for maintainer submission; do not file automatically.

Verified on 2026-07-18 against the official [A2A Community Hub](https://github.com/a2aproject/A2A/blob/main/docs/community.md) and [contribution guide](https://github.com/a2aproject/A2A/blob/main/CONTRIBUTING.md).

## Upstream route

Fgentic belongs in **A2A Integrations**, not **Community SDKs**: it is a deployable Matrix-to-A2A client integration built with the official Go SDK, not another language SDK. The Community Hub publishes package, documentation, CI, Apache-2.0, and maintenance requirements only for Community SDKs, and its issue shortcut is SDK-specific. The Integrations section has no separate checklist or issue form. The contribution guide routes documentation changes through a pull request; the [Hector integration addition](https://github.com/a2aproject/A2A/pull/1129) followed that route directly.

The maintainer should first file the issue below as requested by [fmind-ai/fgentic#65](https://github.com/fmind-ai/fgentic/issues/65). If A2A maintainers invite a documentation pull request, add the prepared one-line entry under `## A2A Integrations` in `docs/community.md`.

## Copy-ready issue

### Title

```text
A2A Integration submission: Fgentic Matrix-to-A2A bridge
```

### Body

```markdown
## Project

[Fgentic](https://github.com/fmind-ai/fgentic) is an Apache-2.0, Kubernetes-native collaboration platform that lets humans invoke A2A agents from federated Matrix rooms. Its Go Matrix Application Service turns an `@mention` into an A2A delegation and posts the agent response back into the room.

## A2A integration

- **Requested category:** A2A Integrations
- **Role:** A2A client/host bridge; it is not an SDK or an A2A server implementation
- **Protocol:** A2A AgentCard/service protocol v1.0 over JSON-RPC/HTTP; the contract suite tracks the A2A v1.0.1 specification while preserving its v1.0 service version
- **Operations:** non-streaming `SendMessage`, with `GetTask` polling for long-running tasks
- **SDK:** [`github.com/a2aproject/a2a-go/v2` v2.3.1](https://github.com/fmind-ai/fgentic/blob/main/apps/matrix-a2a-bridge/go.mod)
- **Trust boundary:** local agents are reached through agentgateway; explicitly configured remote agents require an exact endpoint and a verified ES256/JCS Signed AgentCard

## Evidence

- [Architecture and quick start](https://github.com/fmind-ai/fgentic#readme)
- [Bridge behavior and configuration](https://github.com/fmind-ai/fgentic/tree/main/apps/matrix-a2a-bridge)
- [A2A contract tests](https://github.com/fmind-ai/fgentic/blob/main/apps/matrix-a2a-bridge/internal/a2aclient/client_contract_test.go)
- [Matrix appservice integration fixture](https://github.com/fmind-ai/fgentic/tree/main/apps/matrix-a2a-bridge/test/integration)
- [CI workflow](https://github.com/fmind-ai/fgentic/actions/workflows/ci.yml), including warning-free checks/tests and the isolated Matrix-to-A2A integration job
- Public OCI artifacts: [bridge image](https://github.com/orgs/fmind-ai/packages/container/package/matrix-a2a-bridge) and [Helm chart](https://github.com/orgs/fmind-ai/packages/container/package/charts%2Fmatrix-a2a-bridge)
- [Apache-2.0 license](https://github.com/fmind-ai/fgentic/blob/main/LICENSE), DCO contributions, and public maintenance history

The current scope is deliberately JSON-RPC/HTTP and non-streaming; it does not claim REST, gRPC, streaming, server-SDK, or full A2A TCK coverage.

Would the project accept Fgentic in the A2A Integrations list? If so, I am happy to open the one-line documentation PR below and address review feedback.
```

## Prepared upstream entry

```markdown
- [Fgentic](https://github.com/fmind-ai/fgentic) — Matrix-to-A2A collaboration bridge using the official Go SDK
```

## Filing checks

1. Re-read the upstream Community Hub and contribution guide in case the category or process changed.
1. Confirm the repository, bridge image, Helm chart, documentation, CI results, and license links remain public.
1. File the issue under the maintainer's identity; do not imply A2A endorsement or TCK coverage.
1. Open the upstream documentation PR only when A2A maintainers confirm the category or request the patch.
1. Record the upstream issue, PR, listing, or feedback on [#65](https://github.com/fmind-ai/fgentic/issues/65).
