---
type: Template
title: Adopter Case Study Template
description: A one-page structure for an approved, evidence-backed Fgentic evaluation, pilot, or production story.
---

# Adopter case-study template

Copy this file to `docs/adopters/<name>.md` and replace every bracketed prompt. Keep the finished page to roughly one rendered page: context up to 100 words, the two compact tables, no more than three outcome bullets, one short quote, and next steps up to 60 words. Delete these instructions and any unused optional field.

Use public facts approved by the adopter. Aggregate or omit sensitive metrics; never publish personal data, credentials, private topology, incident details, or internal commercial terms. Link evidence that readers can inspect, distinguish measurements from estimates, and state material limitations.

## [Organization or individual]: [evaluation, pilot, or production outcome]

**Deployment stage:** [Evaluation | Pilot | Production]\
**Period covered:** [YYYY-MM to YYYY-MM or ongoing]\
**Public contact:** [Name and role, public URL, or “Not published”]\
**Last fact check:** [YYYY-MM-DD]

### Context

[What collaboration or delegation problem was being evaluated? Name the relevant sovereignty, identity, model, or cross-organization constraints and the previous baseline. Do not turn this into general product marketing.]

### Profile chosen

| Boundary              | Public profile and rationale                                                                     |
| --------------------- | ------------------------------------------------------------------------------------------------ |
| Deployment            | [Local evaluation, self-managed Kubernetes, or other public description]                         |
| Model                 | [Self-hosted vLLM or approved API profile; state why, without exposing account details]          |
| Identity              | [Reference Keycloak, upstream organizational IdP, or evaluation-only identity posture]           |
| Federation            | [Single-organization evaluation, provider-free lab, or named partner boundary and authorization] |
| Agents and governance | [Public Agent roles, sender/room scope, and operator-owned policy boundary]                      |

### Scale observed

State the measurement period. Use `Not published` rather than guessing or disclosing sensitive values.

| Signal                   | Observed value or bounded description |
| ------------------------ | ------------------------------------- |
| Participants and rooms   | [Value]                               |
| Enabled agents           | [Value]                               |
| Delegations              | [Value per stated period]             |
| Cross-organization peers | [Value or not applicable]             |
| Model requests or tokens | [Aggregate value or not published]    |

### Outcome

- **[Outcome]:** [Measured change from the stated baseline, with a public evidence link where available.]
- **[Outcome]:** [Operational or governance result, including who owns the ongoing control.]
- **Limitations:** [What the evaluation did not prove, what remains manual, or what blocks broader adoption.]

### Approved quote

> “[Up to 40 approved words, or ‘No public quote provided.’]”

— [Name, role, and organization, or approved attribution]

### Next step

[The next bounded deployment or evaluation step, its owner, and the evidence required to call it complete.]

---

PR confirmation: [Name or role] approved this public case study and any quote/logo on [YYYY-MM-DD]. Evidence of approval was provided to [maintainer or public link]; do not commit private correspondence.
