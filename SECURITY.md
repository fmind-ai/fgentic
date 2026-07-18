# Security Policy

## Reporting a vulnerability

**Do not report security vulnerabilities through public GitHub issues, discussions, or pull requests.**

Report privately via [GitHub private vulnerability reporting](https://github.com/fmind-ai/fgentic/security/advisories/new). If that is unavailable, email **fgentic@fmind.ai**.

You can expect an acknowledgment within 7 days and a remediation plan or triage outcome within 30 days. Coordinated disclosure is appreciated; we will credit reporters unless they prefer otherwise.

## Supported versions and security fixes

Before `v1.0.0`, only the latest tagged release is supported. From `v1.0.0`, the latest minor release line receives security fixes; the immediately preceding minor line in the same major series receives fixes for maintainer-assessed critical vulnerabilities for 90 days after its successor is published. Older lines, untagged commits, and modified deployments are unsupported.

Fixes are released as new immutable patch tags on each affected supported line. An advisory identifies affected versions, the fixed version, known workarounds, and any required configuration or secret rotation. If a backport cannot be made safely, the advisory requires upgrading to the latest supported line instead of moving an existing tag or shipping an untested patch. The acknowledgment and triage targets above are response targets, not a promised fix or disclosure date.

Fgentic pins but does not maintain its upstream components. An upstream report follows that project's process; when it affects a supported Fgentic release, the Fgentic advisory and patch identify the updated pin or deployment mitigation. Operators remain responsible for applying supported releases and for validating their own overlays, providers, and runtime controls.

## Scope

1. The `matrix-a2a-bridge` Go application and its Helm chart.
1. The platform manifests in `infra/` and `clusters/` (NetworkPolicies, gateway routes, secrets handling) — misconfigurations that break a documented security control are in scope.
1. The CI/CD supply chain (image signing, digest pinning).

Vulnerabilities in upstream components (Synapse, MAS, Element, kagent, agentgateway, CloudNativePG, Traefik, …) should go to their respective projects; we track and apply upstream fixes (see, e.g., issue [#39](https://github.com/fmind-ai/fgentic/issues/39)).

## Security model

The stable trust-boundary summary is in [docs/security.md](docs/security.md), with the assets, actors, STRIDE analysis, control evidence, and residual risks in the [full threat model](docs/security/threat-model.md). The [security and auditor dossier](docs/security-whitepaper.md) maps those concrete controls and limitations to the OWASP Agentic and LLM Top 10. The [delegation attribution runbook](docs/audit.md) states exactly what the Matrix → bridge → kagent → agentgateway/Prometheus evidence chain can and cannot prove. Known, deliberately accepted limits — including the explicit [prompt-injection limits](docs/security/prompt-injection.md), unauthenticated kagent behind layered gateway/NetworkPolicy controls, unencrypted agent rooms, and organization-level federation identity — are stated there rather than hidden; reports that materially change those assessments are very welcome.
