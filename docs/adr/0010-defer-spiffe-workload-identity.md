# ADR 0010: Defer SPIFFE workload identity until both protocol endpoints can consume it

- **Status:** Accepted
- **Date:** 2026-07-11
- **Decision owners:** Fgentic maintainers
- **Related:** D11, issue #20, issue #40, issue #42, issue #93

## Context

Fgentic needs to distinguish the Matrix bridge workload from an arbitrary in-cluster caller. The implemented boundary is a random bridge-only API key, strict agentgateway authentication, fail-closed CEL authorization on the A2A route, and NetworkPolicies. `X-User-Id` remains end-user attribution rather than a caller credential. The key is stronger than the old header-only design, but it is static secret material and does not give kagent a cryptographic workload identity.

SPIFFE could issue short-lived X.509-SVIDs such as `spiffe://fgentic.local/ns/bridge/sa/matrix-a2a-bridge`. SPIRE would attest the Pod/ServiceAccount, rotate the SVID, and expose it through the Workload API. The useful security outcome would be mutual TLS in which agentgateway verifies that URI and authorizes it through CEL, followed by an authenticated gateway-to-kagent hop. Merely installing an issuer does not produce that outcome.

This review used the current stable components on 2026-07-11:

1. SPIRE [v1.15.2](https://github.com/spiffe/spire/releases/tag/v1.15.2) is current. The hardened SPIFFE chart release is [`spire-0.29.0`](https://github.com/spiffe/helm-charts-hardened/releases/tag/spire-0.29.0) and currently packages SPIRE 1.14.5.
1. A default render of that chart creates four workload controllers: a SPIRE Server StatefulSet (server plus controller-manager), SPIRE Agent DaemonSet, CSI Driver DaemonSet (driver plus node registrar), and OIDC Discovery Provider Deployment (provider plus helper). The chart declares no default CPU or memory requests for those seven containers. The upstream [service matrix](https://spiffe.io/docs/latest/spire-helm-charts-hardened-about/service-selection/) confirms those five logical services are enabled by default.
1. agentgateway v1.3.1's lower-level runtime understands strict client-certificate roots and exposes certificate/SPIFFE identity to CEL. Its Kubernetes `AgentgatewayPolicy` API does not expose a client CA or mTLS mode: [`FrontendTLS`](https://github.com/agentgateway/agentgateway/blob/v1.3.1/controller/api/v1alpha1/agentgateway/agentgateway_policy_types.go#L640-L685) contains protocol/cipher tuning and a TODO to mirror backend TLS controls. The available `source.identity` is the Istio workload identity when an authenticated mesh/tunnel supplies it; source-IP workload metadata is explicitly `unverified`.
1. The bridge HTTP client does not consume the SPIFFE Workload API or present a client SVID. kagent 0.9.11 exposes plaintext A2A under its unsecure authenticator and still installs a no-op authorizer (D11). It cannot verify a bridge or gateway SVID.

## Options considered

1. **Install SPIRE now and mount SVIDs into the bridge.** This adds an issuer, node agents, CSI, registration controller, rotation, persistence, and upgrade surface. The pinned gateway CRD cannot require the presented client SVID and kagent cannot consume one, so the identity would terminate in untrusted header projection. Rejected: operational weight without an enforceable end-to-end boundary.
1. **Adopt an ambient service mesh to project SPIFFE identities into agentgateway.** The pinned runtime contains Istio/HBONE identity support and could authorize `source.identity` when the mesh authenticates it. This adds a mesh control plane and node data plane on top of SPIRE-like identity operations, while the final kagent hop and its authorizer remain unchanged. Rejected for the current single-cluster reference profile; revisit only if the platform independently adopts a mesh for multi-network requirements.
1. **Keep the scoped API-key/CEL boundary and defer SPIFFE.** One random credential is generated into the gateway verifier and bridge consumer together, removed before proxying, rotatable, and backed by namespace NetworkPolicies. It is less elegant than SVID rotation but is fully consumed by the pinned gateway today. Selected.

## Decision

Defer SPIRE and do not deploy a prototype to the shared local cluster. A prototype that only shows an SVID being issued would test SPIRE, not Fgentic's trust boundary. The first meaningful prototype must replace—not wrap—the bridge API key on an isolated A2A listener, authorize an exact SPIFFE ID at agentgateway, and carry authenticated workload identity to a kagent boundary that rejects unauthenticated callers.

Until the adoption triggers below are met:

1. Keep the bridge's agentgateway API key independent from `X-User-Id` and rotate both verifier and consumer copies atomically.
1. Keep strict API-key authentication plus the bridge workload, kagent path, and GET/POST CEL restrictions on the A2A route, including AgentCards.
1. Keep NetworkPolicies as defense in depth and require runtime conformance before production.
1. Do not call `X-User-Id`, Kubernetes source IP metadata, a mounted certificate that no server verifies, or a sidecar-added header a workload identity.

SPIFFE would not authenticate the Matrix human, solve prompt injection, authorize an individual agent/tool, or establish partner-organization policy. Those remain separate controls even after workload mTLS adoption.

## Adoption triggers

Reopen this ADR only when all mandatory triggers are true:

1. **Gateway consumption:** a non-prerelease agentgateway Kubernetes API can require a client CA on the A2A listener and exposes the verified URI SAN/SPIFFE ID to authorization policy without an unauthenticated header or source-IP substitution.
1. **Backend consumption:** a non-prerelease kagent supports a validated non-browser workload credential and non-no-op authorization, or its network/API design guarantees every A2A and session call terminates at an mTLS-authenticated gateway with no direct fallback.
1. **Real requirement:** at least two clusters/trust domains need workload federation, a service mesh is independently approved, or a compliance policy rejects rotatable static workload keys.
1. **Foundation works:** NetworkPolicy conformance passes on the target cluster; SPIFFE must not be used to distract from a broken basic isolation layer.
1. **Operability fits:** the selected SPIRE distribution packages the current stable SPIRE, declares resource requests/limits, supports HA storage and tested upgrades, and fits the cluster's approved capacity and the $85/month reference budget.

The following factors strengthen the case but are not sufficient alone: more first-party workloads, shorter credential rotation objectives, cross-cloud portability, or a desire to use SPIFFE identities for database/cloud access.

## Required adoption prototype

When the triggers are met, acceptance requires a disposable local or staging prototype that:

1. Attests the bridge ServiceAccount and issues the exact expected SVID without a Kubernetes Secret containing its private key.
1. Requires mTLS on a dedicated agentgateway A2A listener and authorizes only the bridge SPIFFE ID; no certificate, an untrusted root, and a wrong URI SAN must all fail.
1. Proves kagent receives authenticated workload context without trusting a caller-set header.
1. Removes the bridge API-key Secret and direct bridge-to-kagent network allowance.
1. Exercises rotation during an in-flight long task and records availability.
1. Adds metrics/alerts for SVID expiry, attestation failure, server/agent health, and rejected identities, plus backup/restore and upgrade runbooks.
1. Quantifies steady resource requests and GCP monthly cost before production approval.

## Consequences

The platform avoids four new controllers/DaemonSets and a second identity control plane that the current protocol endpoints cannot yet use. The trade-off is continued operation and rotation of one scoped A2A workload key, and reliance on a NetworkPolicy engine whose local constrained-host failure remains visible in issue #40. The decision is intentionally reversible once the gateway and kagent can enforce SVIDs end to end.
