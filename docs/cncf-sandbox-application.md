---
type: Reference
title: CNCF Sandbox Application Dossier
description: Copy-ready application answers, live-criteria self-assessment, and prerequisite gates for a future Fgentic CNCF Sandbox submission.
---

# CNCF Sandbox Application Dossier

Status: **not ready to submit**. This dossier was checked against the live CNCF process on **2026-07-18**. It prepares the public, technical answers; it does not grant permission to submit, accept foundation policies, provide private signatory data, or transfer trademarks and accounts.

The current blockers are substantive: CNCF requires at least three maintainers employed by at least two organizations, while Fgentic has one maintainer; the project must resolve how its MPL-2.0 library and referenced AGPL-3.0 runtime components fit CNCF's dependency-license policy; and the TOC's current guidance rejects reference architectures, so the application must establish that Fgentic is reusable software rather than only an integration example. Named adopters and a public kagent relationship would strengthen the application but do not replace those gates.

## Sources and refresh rule

The application owner must refresh this document immediately before submission. The 2026-07-18 check used:

- the [CNCF Sandbox application form](https://github.com/cncf/sandbox/blob/bae09d111a072fa58148626c0434fe829b73e911/.github/ISSUE_TEMPLATE/application.yml), last changed 2026-05-26;
- the [CNCF project lifecycle and common Sandbox closure reasons](https://github.com/cncf/toc/blob/dffcced3d94fb8eba2fa7d29ebdbe74977800e41/process/README.md#applying-to-sandbox), last changed 2026-04-16;
- the non-binding [Sandbox Reviewer Guide](https://github.com/cncf/toc/blob/bde61ce06db13b439e807f843b2360f1991eb5a0/toc_subprojects/project-reviews-subproject/sandbox-review-guide.md), last changed 2025-09-06;
- the [General Technical Review questions](https://github.com/cncf/toc/blob/55c2ec8b497fb53529546bba312db151cc8b519a/toc_subprojects/project-reviews-subproject/general-technical-questions.md), last changed 2026-04-25; and
- the [CNCF Allowlist License Policy](https://github.com/cncf/foundation/blob/4abf5951d08350014dafb6a1dcf1fe815eeca5d5/policies-guidance/allowed-third-party-license-policy.md), last changed 2025-10-29.

Before copying these answers into the form:

1. Compare every form label below with the live issue form and add, remove, or rename answers as needed.
1. Replace repository links with immutable links to the exact release or commit being submitted.
1. Refresh project age, maintainer, adopter, release, Landscape, LFX Insights, TAG-review, and prerequisite status.
1. Have the maintainer supply and approve identity-bound contact and Contribution Agreement data.
1. Submit only after every item marked **BLOCKED** in [Prerequisite decision](#prerequisite-decision) is closed.

## Copy-ready application answers

The headings in this section reproduce the live form's question labels. Text marked **HUMAN** is deliberately not pre-accepted on the maintainer's behalf.

### Basic project information

#### Project summary

Fgentic is reusable Kubernetes-native software for governed human-to-agent and cross-organization agent collaboration over Matrix and A2A.

#### Project description

Fgentic lets people delegate work to AI agents from shared Matrix rooms while keeping the collaboration fabric, deployment, model path, and policy controls under the operator's control. Its reusable core is a Go Matrix Application Service that converts a typed mention or explicit command into an A2A request, preserves room and sender attribution, enforces bounded routing and rate limits, and returns the result as a non-authoritative Matrix notice. The repository also provides a signed remote-agent trust boundary, federation policy code, packaging, conformance tests, and GitOps components for Kubernetes.

The project fills a gap between cloud-native agent runtimes and human collaboration. It does not implement an agent framework, model server, gateway, identity provider, or Matrix distribution. Instead, it composes open protocols and independently maintained projects—including Matrix, A2A, MCP, kagent, agentgateway, Kubernetes, and Flux—while owning the bridge, governance, and federation seam. This gives regulated and sovereignty-sensitive organizations a self-hostable collaboration path with explicit trust boundaries and a documented exit at each layer. See [Architecture & Vision](architecture.md), [Open Agentic Stack](open-stack.md), and the [Bridge Specification](bridge.md).

### Project details

#### Org repo URL (provide if all repos under the org are in scope of the application)

N/A. The application covers one repository, not every repository in the `fmind-ai` organization.

#### Project repo URL in scope of application

https://github.com/fmind-ai/fgentic

#### Additional repos in scope of the application

N/A.

#### Website URL

https://github.com/fmind-ai/fgentic

Fgentic does not yet have a separately published documentation website. [Issue #72](https://github.com/fmind-ai/fgentic/issues/72) tracks that optional publication surface.

#### Roadmap

https://github.com/fmind-ai/fgentic/milestones

#### Roadmap context

Current GitHub milestones are the executable roadmap. The [`track/v1` focus board](https://github.com/fmind-ai/fgentic/issues/316) is the pickup cut line, and [docs/roadmap.md](roadmap.md) preserves dated history rather than creating a second plan. Work is issue-sized, publicly discussed, and linked to milestone epic checklists. Fgentic is pre-1.0; the current `v0.1.0` release and `main` may change incompatibly under the [stability contract](stability.md).

#### Contributing guide

https://github.com/fmind-ai/fgentic/blob/main/CONTRIBUTING.md

#### Code of Conduct (CoC)

https://github.com/fmind-ai/fgentic/blob/main/CODE_OF_CONDUCT.md

#### Adopters

https://github.com/fmind-ai/fgentic/blob/main/ADOPTERS.md

As of 2026-07-18, the file contains no named evaluation, pilot, or production adopter. [Issue #76](https://github.com/fmind-ai/fgentic/issues/76) owns the consent-based adopter pipeline and first real entry.

#### Maintainers file

https://github.com/fmind-ai/fgentic/blob/main/MAINTAINERS.md

As of 2026-07-18, the project has one maintainer, Médéric Hurier, from one organization. This does not meet CNCF's current minimum of three maintainers from at least two employer organizations. [Issue #71](https://github.com/fmind-ai/fgentic/issues/71) owns that blocker; the public ladder is in [GOVERNANCE.md](../GOVERNANCE.md).

#### Security policy file

https://github.com/fmind-ai/fgentic/blob/main/SECURITY.md

The policy provides private reporting through GitHub Security Advisories or email, response targets, scope, and links to the project's [security model](security.md), [threat model](security/threat-model.md), and [auditor dossier](security-whitepaper.md). It is not a completed CNCF TAG Security assessment or an independent audit.

#### Standard or specification?

N/A. Fgentic is an implementation and integration project, not a new standard or specification. It implements existing Matrix and A2A protocol surfaces and uses MCP at the agent-tool boundary. Project topic specifications document implementation behavior and trust boundaries; they do not claim standards authority. See [Architecture & Vision](architecture.md) and the [Bridge Specification](bridge.md).

#### Business product or service to project separation

The complete project in scope is public in `fmind-ai/fgentic`; there is no withheld enterprise edition or private source repository in the application scope. Fmind may provide paid deployment, integration, operation, or support services around the project, but those services do not own a separate Fgentic product codebase. The repository's Apache-2.0 project code, public governance, issue tracker, releases, and contribution path remain available independently of consulting. Upstream AGPL components are referenced rather than forked or mirrored; the boundary and service posture are documented in [Licensing & Foundation Strategy §10.4](licensing.md#104-consulting-engagement-agpl-compliance--what-is-and-isnt-permitted).

### Cloud native context

#### Why CNCF?

Fgentic's reusable software is deployed and governed through Kubernetes, and its value depends on composing cloud-native projects without absorbing their responsibilities. CNCF offers the most relevant neutral home for the bridge and federation seam because it already stewards Kubernetes, Flux, cert-manager, Prometheus, OpenTelemetry, CloudNativePG, and kagent. Neutral IP and governance would reduce single-vendor risk, make the maintainer ladder accountable to a broader community, and give adjacent project communities a durable place to review the integration boundary. CNCF's Cloud Native AI Working Group under TAG Runtime is the likely first technical discussion venue; no TAG endorsement or review has occurred yet.

The request is not for CNCF to endorse a reference architecture. Before applying, Fgentic must demonstrate that the independently deployable bridge, federation-policy module, signed-agent trust controls, Helm packaging, and conformance gates are a reusable project whose users can adopt without copying the reference deployment. The full reference stack remains test and integration evidence for that software.

#### Benefit to the landscape

Cloud-native agent projects expose agent and gateway APIs, but they do not provide a vendor-neutral, federated human collaboration fabric. Fgentic adds a reusable Matrix-to-A2A application-service boundary: explicit agent routing, sender and room attribution, bounded task lifecycle, signed remote-agent identity, rate and token admission controls, and fail-closed federation policy. Its differentiator is cross-organization collaboration without anchoring both organizations to one SaaS tenant.

The project benefits adjacent projects by giving kagent-hosted or compatible A2A agents a chat-native interface without changing the agent runtime, and by giving agentgateway a concrete governed A2A/LLM path. It documents where each control actually lives rather than presenting the composed stack as one security product. The ownership and non-goals are defined in [Architecture & Vision §1](architecture.md#1-vision--goals) and [Open Agentic Stack](open-stack.md).

#### Cloud native 'fit'

Fgentic is declarative, containerized, and Kubernetes-native. Applications are independently packaged; Flux reconciles Helm releases and Kustomize overlays; Gateway API defines ingress; SOPS provides encrypted GitOps secrets; and NetworkPolicy, workload identity, immutable image digests, observability, and offline render validation are treated as deployment contracts. The bridge itself is stateless except for explicit durable task state and external services, and exposes health and Prometheus metrics. Operators choose public, private, hybrid, or self-hosted model backends per cluster. See [Architecture & Vision §2](architecture.md#2-architecture-current-verified-state), [Production Installation](production.md), and [Security Boundaries](security.md).

#### Cloud native 'integration'

Fgentic complements and depends on CNCF projects without replacing them:

- **Kubernetes** runs the application and policy units.
- **kagent** hosts the local A2A agents; Fgentic provides the Matrix collaboration interface and cross-organization boundary.
- **Flux** reconciles the GitOps dependency graph and immutable artifacts.
- **cert-manager** and **Gateway API** provide certificate and routing primitives.
- **CloudNativePG** provides the shared PostgreSQL substrate with scoped roles and databases.
- **Prometheus** and **OpenTelemetry** carry operational metrics and traces.

The project also composes non-CNCF open standards and projects: Matrix for collaboration, A2A for delegation, MCP for tool contracts, and AAIF-hosted agentgateway for the governed agent/model data plane. Exact responsibilities are mapped in [Open Agentic Stack](open-stack.md).

#### Cloud native overlap

Fgentic does not intentionally overlap a CNCF agent framework, gateway, model server, GitOps controller, database, observability system, identity provider, or Matrix homeserver. kagent owns agent lifecycle and execution; agentgateway owns gateway policy and model traffic; the corresponding upstream projects own the remaining layers. Fgentic owns the reusable Matrix-to-A2A bridge, the cross-organization trust and federation seam, its policy modules and packaging, and evidence that the composition fails closed.

The main TOC classification risk is that the repository also contains a reference deployment. CNCF's current lifecycle guidance says reference architectures and reference implementations are not accepted as Sandbox projects. The maintainers must obtain CNCF feedback that the reusable software boundary above is sufficient, narrow the application scope if requested, or publish the integration design through CNCF Reference Architectures instead. This is a prerequisite, not a claim of eligibility.

#### Similar projects

No project found in the project's 2026-07 competitive scan implements the same Matrix Application Service to A2A boundary. [Architecture & Vision §1](architecture.md#1-vision--goals) records the reviewed categories and closest prior art:

- MindRoom, baibot, and Beeper ai-bridge put model-oriented bots into Matrix but do not bridge Matrix delegation to independently hosted A2A agents.
- Microsoft Teams/Entra Agent ID, Slack AI, and Google's agent ecosystem provide tenant-anchored proprietary collaboration rather than a self-hosted federated fabric.
- kagent is complementary: it creates and runs Kubernetes-native agents and exposes A2A; Fgentic supplies the human collaboration and federation boundary.
- agentgateway is complementary: it governs A2A and model traffic but is not a Matrix client or homeserver bridge.

This comparison must be refreshed immediately before submission and should incorporate any similar CNCF Sandbox applications published after 2026-07-18.

#### Landscape

No. A live search on 2026-07-18 found no Fgentic entry in the Cloud Native Landscape. [Issue #66](https://github.com/fmind-ai/fgentic/issues/66) tracks the separate Landscape application and its current project-maturity gate.

#### Insights

No. A live search on 2026-07-18 found no Fgentic entry in LFX Insights. Recheck by repository URL immediately before submission.

### CNCF policies

#### Trademark and accounts

**HUMAN — unchecked.** The maintainer must understand the current CNCF IP Policy, identify every project trademark, domain, social account, package, and repository account in scope, confirm authority to transfer them, and personally select the required checkbox. This dossier does not consent to transfer.

#### IP policy

**HUMAN — unchecked.** The maintainer and proposed signatory must review the live CNCF IP Policy and Contribution Agreement and personally select the required checkbox. This dossier does not accept legal terms.

#### Will the project require a license exception?

**Unresolved; treat as yes until CNCF confirms otherwise.** Fgentic's original code is Apache-2.0, but the bridge binary imports `mautrix/go` under MPL-2.0 and the reference deployment retrieves unmodified upstream AGPL-3.0 components including Synapse, Matrix Authentication Service, Element, ESS Community, and optional observability or network bridges. MPL-2.0 and AGPL-3.0 are not on CNCF's current third-party allowlist.

The project must request a CNCF licensing determination before submission. If CNCF treats these libraries or runtime artifacts as project dependencies, the maintainers must obtain the required exception or change the submitted dependency/profile scope. Do not answer `N/A` merely because the repository's own license is Apache-2.0. The inventory and packaging boundaries are in [Licensing & Foundation Strategy §10](licensing.md) and the root [`NOTICE`](../NOTICE).

#### Project "Domain Technical Review"

No. As of 2026-07-18, Fgentic has not presented to a CNCF TAG, completed the Day-0 General Technical Review with CNCF reviewers, or received a domain review. The likely first venue is the Cloud Native AI Working Group under TAG Runtime; TAG Security is relevant to the cross-organization trust boundary. [Issue #67](https://github.com/fmind-ai/fgentic/issues/67) separately tracks a relationship with the kagent community and includes a prepared technical pitch, but no presentation or endorsement has occurred.

### Contact information

#### Application contact email(s)

fgentic@fmind.ai

**HUMAN:** confirm that this public project address is monitored and is the address CNCF should use before submission. Add any individual contact addresses only with their approval.

#### Contributing or sponsoring entity signatory information

**HUMAN — do not infer or publish private identity data.** The contributing individual or legal entity must provide the exact name, country or registered address, entity type where applicable, authorized signatory name and title, and email required by the live form. The signatory must have authority over every trademark, domain, account, and repository included in the Contribution Agreement.

### Additional information

#### CNCF contacts

None as of 2026-07-18. Do not name a CNCF or TAG participant without their consent and direct familiarity with the project.

#### Additional information

Fgentic is very early: the public repository was created on 2026-07-10, `v0.1.0` was published on 2026-07-14, and there is currently one human maintainer and no named adopter. The project should be evaluated as experimental, not production-proven. Current evidence includes warning-free offline gates, unit and integration suites, a provider-free three-homeserver federation lab, signed remote-agent identity and quota fixtures, and documented security limits. The repository's k3d profiles do not enforce NetworkPolicy at runtime; a separate kind/Calico test provides policy evidence. No public deployment, external security audit, production cross-organization pilot, or formal CNCF review is claimed.

The project seeks a foundation path only for the reusable software it owns: the Matrix-to-A2A bridge, cross-organization federation and trust controls, policy modules, packaging, and conformance evidence. It does not seek to donate or rebrand Matrix, kagent, agentgateway, or another upstream project. The [Exit Strategy](exit-strategy.md) documents how every composed layer can be replaced.

## Sandbox criteria self-assessment

This assessment separates published CNCF closure reasons from useful readiness evidence. A green row is not CNCF approval.

| Criterion or reviewer concern                     | State on 2026-07-18 | Evidence and action                                                                                                                                                                                                   |
| ------------------------------------------------- | ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Public reusable project                           | **AT RISK**         | The repo is public and contains reusable applications, policy modules, charts, and tests, but also a reference deployment. Resolve the TOC's project-vs-reference-architecture classification before applying.        |
| Project license                                   | **PARTIAL**         | Original code is Apache-2.0 ([LICENSE](../LICENSE)); dependency policy remains unresolved below.                                                                                                                      |
| Dependency-license compliance                     | **BLOCKED**         | MPL-2.0 is linked into the bridge and AGPL-3.0 artifacts are retrieved by the reference profile. Obtain a CNCF license determination and any exception or scope change.                                               |
| MAINTAINERS file shape                            | **MET**             | [MAINTAINERS.md](../MAINTAINERS.md) has name, GitHub ID, organization, and areas.                                                                                                                                     |
| Three maintainers from two employer organizations | **BLOCKED**         | One maintainer from one organization. Recruit through [#71](https://github.com/fmind-ai/fgentic/issues/71) and update governance records only after sustained contributions.                                          |
| Parent-project separation                         | **N/A**             | Fgentic is not a subdirectory or subproject of another project's organization. Recheck if application scope changes.                                                                                                  |
| Trademark and account transfer authority          | **BLOCKED / HUMAN** | Maintainer must inventory assets, confirm authority, accept the live policy, and sign the Contribution Agreement.                                                                                                     |
| Vendor-neutral governance                         | **PARTIAL**         | Public maintainer-led governance and ladder exist ([GOVERNANCE.md](../GOVERNANCE.md)); one maintainer still decides. Diversity blocker is [#71](https://github.com/fmind-ai/fgentic/issues/71).                       |
| DCO contribution path                             | **MET**             | DCO sign-off, issue claims, checks, and PR expectations are documented in [CONTRIBUTING.md](../CONTRIBUTING.md).                                                                                                      |
| Code of Conduct                                   | **MET**             | [CODE_OF_CONDUCT.md](../CODE_OF_CONDUCT.md) with confidential contact.                                                                                                                                                |
| Security reporting                                | **MET, UNREVIEWED** | Private reporting and response targets exist in [SECURITY.md](../SECURITY.md); no external audit or CNCF security review is claimed.                                                                                  |
| Named adopters                                    | **UNMET**           | [ADOPTERS.md](../ADOPTERS.md) has no entries; [#76](https://github.com/fmind-ai/fgentic/issues/76) is the consent-based path. Not listed as a published Sandbox minimum, but important evidence.                      |
| Domain/community review                           | **UNMET**           | No TAG, Cloud Native AI WG, kagent, or agentgateway review outcome yet. Use prepared outreach in [#67](https://github.com/fmind-ai/fgentic/issues/67).                                                                |
| Roadmap and project direction                     | **MET**             | Public milestones, [focus board #316](https://github.com/fmind-ai/fgentic/issues/316), and [roadmap history](roadmap.md).                                                                                             |
| Release process                                   | **PARTIAL**         | `v0.1.0`, SemVer rules, immutable tags, CI/CD, signing and SBOM controls exist; the project is days old and the next release still has open readiness work in [#471](https://github.com/fmind-ai/fgentic/issues/471). |
| Landscape and LFX visibility                      | **UNMET**           | No listings found on 2026-07-18; recheck live and follow [#66](https://github.com/fmind-ai/fgentic/issues/66).                                                                                                        |

## Day-0 technical review preparation

The current Sandbox Reviewer Guide says only the Day-0 General Technical Review questions are needed for a Sandbox review, and the review is non-binding until CNCF performs it. The table below is a self-assessment, not a completed CNCF review.

| Day-0 topic                         | Current answer and traceable evidence                                                                                                                                                                                                                              | Known gap                                                                                                                                                              |
| ----------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Roadmap and scope                   | GitHub milestones are authoritative; [#316](https://github.com/fmind-ai/fgentic/issues/316) defines the v1 cut line; [Architecture §1](architecture.md#1-vision--goals) defines ownership and non-goals.                                                           | Broad v1 scope and limited external validation; no adopter feedback loop yet.                                                                                          |
| Target personas                     | Platform engineer, security lead, DPO, developer, and end-user paths are listed under [Persona Onboarding](onboarding/index.md).                                                                                                                                   | No completed end-user research report.                                                                                                                                 |
| Primary and unsupported use cases   | Shared Matrix-room delegation and governed cross-org collaboration are primary. Building an agent framework, gateway, model server, identity provider, or Matrix distribution is explicitly out of scope in [Architecture §1](architecture.md#1-vision--goals).    | Production cross-org pilot remains absent.                                                                                                                             |
| Intended adopters                   | Sovereignty-sensitive regulated, public-sector, and platform-engineering organizations; [Adopter Decision Brief](adopter-decision-brief.md) frames selection.                                                                                                      | No named adopter or pilot.                                                                                                                                             |
| User interaction and UX             | Humans use Matrix mentions or explicit commands; operators use GitOps and runbooks. [Bridge §5](bridge.md) defines intake and reply behavior.                                                                                                                      | The client experience is not supported by published user research or a public recorded demo.                                                                           |
| Production integration              | Matrix, A2A, kagent, agentgateway, Kubernetes, Flux, CNPG, Gateway API, SOPS, Prometheus, and OTel boundaries are mapped in [Architecture §2](architecture.md#2-architecture-current-verified-state).                                                              | The public reusable-project boundary needs TOC confirmation.                                                                                                           |
| Design principles                   | Open standards, federation readiness, fail-closed trust boundaries, cost controls, small independent apps, and swappable layers are documented in [Architecture](architecture.md), [Design Decisions](design-decisions.md), and [Exit Strategy](exit-strategy.md). | Some profiles remain experimental and pre-1.0.                                                                                                                         |
| Environment requirements            | The [README](../README.md), [Production Installation](production.md), and [Hosted Coding Agents](hosted-agents.md) separate local, test, and production paths.                                                                                                     | GKE reference is spend-gated and has no published production evidence.                                                                                                 |
| Service dependencies                | Exact components and responsibility boundaries are listed in [Architecture §2](architecture.md#2-architecture-current-verified-state) and [Open Agentic Stack](open-stack.md).                                                                                     | License and lifecycle dependence on non-Apache upstream profiles requires CNCF review.                                                                                 |
| Identity and access                 | Matrix identity, MAS/OIDC, appservice namespaces, sender allowlists, remote-agent Signed AgentCards, JWT authorization, and downstream limits are in [Identity and SSO](identity.md), [Bridge §6](bridge.md), and [Security](security.md).                         | Attribution is not universal downstream authentication; docs state that limit.                                                                                         |
| Sovereignty                         | Self-hosting, per-cluster model choice, open protocols, data boundaries, federation, and exit paths are explicit in [Architecture](architecture.md), [Models](models.md), and [Exit Strategy](exit-strategy.md).                                                   | Some default upstream components are AGPL/open-core; permissive alternatives carry documented trade-offs.                                                              |
| Compliance posture                  | The [Security and Auditor Dossier](security-whitepaper.md), [Retention](retention.md), and [Audit](audit.md) map controls and evidence.                                                                                                                            | No certification, legal opinion, independent audit, or production DPO acceptance is claimed.                                                                           |
| High availability                   | Production replica, placement and disruption profiles are described in project agent docs and validated offline; [Operations Handbook](operations-handbook.md) states operational boundaries.                                                                      | No production HA or split-control-plane drill evidence; avoid claiming hard-crash exactly-once recovery.                                                               |
| Resource requirements               | [Production Installation](production.md), workload resource manifests, and [Performance Evidence](performance.md) provide current sizing and load-sanity evidence.                                                                                                 | No multi-tenant soak baseline; [#465](https://github.com/fmind-ai/fgentic/issues/465) tracks it.                                                                       |
| Storage requirements                | CNPG database boundaries, Matrix media, durable bridge state, and optional knowledge storage are documented in [Architecture](architecture.md), [Bridge](bridge.md), and [Grounding](grounding.md).                                                                | Backup/restore and some retention drills remain open roadmap work.                                                                                                     |
| API topology and defaults           | Matrix Application Service events enter the bridge; A2A `SendMessage`/`GetTask` reaches local or pinned remote agents; MCP remains an agent-tool boundary. [Bridge §§5–6](bridge.md) and [Federation](federation.md) define defaults and versioning.               | Pre-1.0 public surfaces can change under [Stability](stability.md).                                                                                                    |
| Release process                     | [CONTRIBUTING Releases](../CONTRIBUTING.md#releases) defines SemVer, immutable tags, git-cliff, matching image/chart versions, signing, and GitHub Releases.                                                                                                       | Only one release exists; [#471](https://github.com/fmind-ai/fgentic/issues/471) owns v1 readiness.                                                                     |
| Installation and validation         | The [README quickstart](../README.md#quickstart-local-k3d) and [Production Installation](production.md) use repo-owned `mise` tasks and Flux reconciliation; offline check/test gates validate code and manifests.                                                 | Runtime acceptance must be performed by the designated runtime owner; no CNCF reviewer has reproduced it.                                                              |
| Security self-assessment            | [Security](security.md), [Threat Model](security/threat-model.md), [Prompt-Injection Limits](security/prompt-injection.md), and [Auditor Dossier](security-whitepaper.md) provide project-authored assessment.                                                     | No formal CNCF cloud-native security self-assessment or TAG Security review.                                                                                           |
| Secure defaults and loosening       | Fail-closed routes, allowlists, signed remote cards, restricted PSS, SOPS secrets, NetworkPolicies, immutable artifacts, and explicit opt-in profiles are documented in [Security](security.md).                                                                   | Repo k3d profiles do not enforce NetworkPolicy; the kind/Calico test is evidence, not equivalent runtime enforcement.                                                  |
| Security hygiene and risk ownership | Private vulnerability reporting, threat modeling, CI scanning, release signing, SBOM, admission policy, and known residual risks are linked from [SECURITY.md](../SECURITY.md).                                                                                    | External audit [#459](https://github.com/fmind-ai/fgentic/issues/459) and security-release process [#462](https://github.com/fmind-ai/fgentic/issues/462) remain open. |
| Least privilege                     | Scoped app/database identities, restricted namespaces, exact routes, NetworkPolicies, sender authorization, and secret boundaries are in [Security](security.md) and the [Threat Model](security/threat-model.md).                                                 | Runtime enforcement varies by cluster profile and is stated honestly.                                                                                                  |
| Certificate and key rotation        | SOPS/age, TLS issuers, and signed-card keys are documented operational boundaries.                                                                                                                                                                                 | Rotation rehearsal is incomplete: [#44](https://github.com/fmind-ai/fgentic/issues/44) and [#352](https://github.com/fmind-ai/fgentic/issues/352) remain open.         |
| Software supply chain               | Immutable digests, Trivy, gitleaks, CodeQL, SBOM, provenance/signing and Flux verification design are documented in [Supply-chain Verification](security/supply-chain.md).                                                                                         | Some end-to-end release and Flux verification gates remain open; do not claim full SLSA or audit completion.                                                           |

## Prerequisite decision

Submission is intentionally blocked until the table below is green. “Green” requires evidence in the linked issue or repository, not a planned date.

| Gate                                                                                                     | State               | Evidence required before submission                                                                                                                                          |
| -------------------------------------------------------------------------------------------------------- | ------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Repository is public and launch-safe ([#70](https://github.com/fmind-ai/fgentic/issues/70))              | **PARTIAL**         | Repository is public; close the remaining history-scan, protection, metadata, and post-flip CD acceptance tasks recorded by the issue.                                       |
| Reusable project, not only reference architecture                                                        | **BLOCKED**         | Obtain public CNCF/TAG/TOC feedback that the bridge/federation software boundary is eligible, or narrow the submission scope / use CNCF Reference Architectures as directed. |
| Three maintainers from two employer organizations ([#71](https://github.com/fmind-ai/fgentic/issues/71)) | **BLOCKED**         | `MAINTAINERS.md` lists at least three active maintainers employed by at least two organizations, promoted through the public governance ladder.                              |
| CNCF dependency-license determination                                                                    | **BLOCKED**         | CNCF confirms the MPL/AGPL treatment in writing; any required exception is approved or the submitted dependency/profile scope is changed.                                    |
| Named adopter pipeline ([#76](https://github.com/fmind-ai/fgentic/issues/76))                            | **UNMET**           | At least one adopter-approved evaluation, pilot, or production entry. This is credibility evidence even if not a published Sandbox minimum.                                  |
| kagent/community relationship ([#67](https://github.com/fmind-ai/fgentic/issues/67))                     | **UNMET**           | Public presentation, integration review, listing, or explicit outcome with follow-up owners. No endorsement should be inferred.                                              |
| Current application and technical answers                                                                | **PREPARED**        | Refresh this dossier against the live form, current commit, GTR template, repo state, Landscape, and LFX Insights immediately before submission.                             |
| Contact and signatory data                                                                               | **BLOCKED / HUMAN** | Approved application contacts and the exact Contribution Agreement signatory table are complete.                                                                             |
| Trademark, accounts, IP Policy and Contribution Agreement                                                | **BLOCKED / HUMAN** | Maintainer inventories transferrable assets, confirms authority and intent, accepts the live checkboxes, and signs under their own identity.                                 |

Once every blocker is resolved, the human submitter should copy the refreshed answers into a new CNCF Sandbox application, record the application URL in [issue #464](https://github.com/fmind-ai/fgentic/issues/464), and answer TOC questions publicly. Until CNCF votes and the Contribution Agreement is signed and countersigned, do not describe Fgentic as donated, contributed, accepted, or a CNCF project.
