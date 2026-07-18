# Five-minute kagent community demo

Status: presenter-ready script; the live runtime and community presentation are human-owned gates.

## Before the call

Use a dedicated machine or an explicitly leased runtime. Do not share Docker, k3d, ports 80/443, or the `fgentic-demo` cluster with another active agent session.

1. Rehearse from the exact revision to be presented.
1. Run `mise run demo:up` for the credential-free transport proof. For a real self-hosted answer, choose the model before bootstrap with `FGENTIC_LLM_PROVIDER=vllm mise run demo:up`; allow for the documented download and memory cost.
1. Keep the final bootstrap output private: it contains the generated Alice password. Log into the printed Element URL as `@alice:fgentic.localhost` and open `#lobby:fgentic.localhost`.
1. Confirm the Element-safe `!agents`, `!budget`, and this exact request work:

   ```text
   !ask agent-docs-qa How does a Matrix mention reach a kagent agent?
   ```

1. With the default `demo` provider, expect the fixed response below. Say explicitly that it proves protocol wiring, not answer quality:

   ```text
   Fgentic's deterministic evaluation model is working through agentgateway and kagent.
   ```

1. Open the [README interaction diagram](https://github.com/fmind-ai/fgentic#the-core-interaction) and [one-pager](one-pager.md) in separate tabs. Increase the Element font size and hide notifications, credentials, unrelated rooms, terminals, and browser history.
1. Prepare a recording as fallback, but label it with its revision and model profile. Never present a recording as a live run.

## 0:00-0:35 — Frame the gap

**Show:** the README title and core interaction diagram.

**Say:**

> kagent gives Kubernetes a native Agent runtime and exposes those agents over A2A. Fgentic asks a narrower question: how do several people work with those agents in shared, organization-owned chat rooms? It composes Matrix, a small Go appservice bridge, agentgateway, and unmodified kagent Agents.

## 0:35-1:15 — Explain the boundary

**Point through:** Matrix -> bridge -> agentgateway -> kagent -> model -> Matrix.

**Say:**

> The bridge owns chat intake and reply projection. It maps an agent's Matrix ghost to one kagent namespace and name, then uses the official Go A2A SDK. kagent still owns the Agent CRD, sessions, tasks, tools, and per-task usage metadata. agentgateway remains the governed model-egress boundary.

> This is additive to kagent's Slack and Discord examples. Matrix adds multi-user rooms and an open federation protocol rather than another tenant-specific bot surface.

## 1:15-2:00 — Show the governed directory

**In Element, send:**

```text
!agents
```

**Say while the directory appears:**

> This list is not ambient discovery. It is generated from the operator's exact mapping and filtered for this sender. A similar-looking ghost on another homeserver cannot silently resolve to a local agent.

**Then send:**

```text
!budget
```

**Say:**

> Admission is visible before invocation: sender and room request limits plus any configured remote reservation ceiling. A reservation is never displayed as consumed tokens.

## 2:00-3:05 — Delegate from the room

**Send:**

```text
!ask agent-docs-qa How does a Matrix mention reach a kagent agent?
```

**Point out:** the working notice, the reply relationship, and the agent ghost that authored the result.

**Say:**

> The Matrix appservice transaction is durably admitted before acknowledgement. The bridge delegates with A2A `SendMessage`; a long-running task is polled with `GetTask` and projected as edits instead of spawning unrelated messages. A completed result returns as the selected agent identity and relates to the original request.

**If using the deterministic profile, add:**

> This fixed sentence is intentional: the free demo proves the entire transport without a credential, prompt egress, or token charge. It does not claim model quality. The same path can select a self-hosted or external model profile.

## 3:05-4:10 — Make the governance claim precise

**Show:** the one-pager's “Why this belongs beside kagent” section.

**Say:**

> The full Matrix user ID is forwarded to kagent and joins the local session and task evidence. That is strong attribution from the trusted appservice path, but it is not a second login at kagent. An accepted deployment must therefore isolate the local A2A endpoint with enforced NetworkPolicy. This laptop k3d demo renders the policy objects but deliberately disables enforcement, so it is not isolation evidence.

> Before dispatch, Fgentic applies exact agent and sender policy, per-sender and per-room rate limits, bounded concurrency, and durable replay handling. agentgateway exposes aggregate model-token telemetry; kagent retains exact task usage. We do not turn correlation or reservations into a billable-cost claim.

## 4:10-4:40 — State the federation destination

**Say:**

> Matrix federation is the collaboration plane: participating organizations can share a purpose-scoped room while keeping their own homeserver boundary. Direct cross-organization A2A is a separate machine-to-machine boundary with signed AgentCards, explicit authorization, and token reservation. Neither plane makes arbitrary remote users or agents trusted by default.

## 4:40-5:00 — Ask for the next smallest step

**Say:**

> Would a maintained Matrix/A2A integration guide or “works with kagent” entry be useful to the community? We can own its tests and version notes. If there is a more reusable upstream boundary, we would like maintainer guidance before proposing anything larger.

Stop there and invite questions. Do not claim endorsement, listing, or subproject status until the kagent community records it.

## Likely questions

**Why not just use the kagent UI?** The kagent UI manages and interacts with Agents. Fgentic targets durable multi-user collaboration rooms, existing Matrix clients, and cross-organization federation.

**Why Matrix instead of Slack or Discord?** Those adapters are useful and already documented by kagent. Matrix supplies an open, self-hostable collaboration protocol and organization-to-organization federation. It also adds operational weight, which Fgentic accepts for that use case rather than claiming it is the smallest single-user path.

**Does `X-User-Id` authenticate the human to kagent?** No. Synapse authenticated a Matrix session and the trusted bridge asserts the full Matrix ID downstream. kagent's local endpoint does not reauthenticate it, so enforced network isolation is part of the security boundary. The repo-owned k3d demo disables NetworkPolicy enforcement and cannot prove that boundary; use the dedicated Calico conformance test and repeat the proof on the target cluster.

**Are agent rooms encrypted?** Current ordinary agent rooms are unencrypted by policy. The v1 federated-room exception is private, invite-only, joined-history, purpose-scoped, visibly plaintext, classification-bounded, and contract-bound. An E2EE requirement blocks deployment until the documented crypto escape hatch exists.

**Can this show exact cost per user?** It can join a local kagent task to its per-task token metadata and show aggregate gateway token signals. It does not claim exact currency cost per Matrix user. Cross-organization token reservations are admission ceilings, not consumption.

**Is Fgentic production-ready?** No. It is pre-1.0 and experimental. The local end-to-end path and deterministic gates are live; production adoption still requires the documented security, identity, model, retention, and operational decisions.
