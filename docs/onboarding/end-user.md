---
type: Guide
title: End-User Onboarding
description: Safely find, invite, mention, and interpret an Agent in a Fgentic Matrix room.
---

# End-user onboarding

## 1. Before you ask

Confirm the room owner, purpose, allowed data class, participants, and visible plaintext notice. Agent rooms are unencrypted. In a federated room, participating homeservers receive the room history; use only the content allowed by the room's [classification record](../adr/0015-federated-room-encryption.md).

Never paste passwords, API keys, access tokens, private keys, recovery material, or unrelated personal/confidential data. Treat files and copied content as narrowly as text: share only what the approved task needs.

## 2. Find and invoke an Agent

1. Run `!agents` to see the Agents available to you in this room. Run `!agents <name>` for the cached description and declared skills. Availability and descriptions do not grant new data access or prove that a remote Agent is currently trusted.
1. Invite the exact ghost `@agent-<name>:<server>` if it is not already a member. Check the complete Matrix ID; a similar display name or a ghost on another homeserver is not equivalent.
1. Send one explicit `@mention` with the task, desired output, relevant constraints, and only the minimum necessary context. Reply in the same room and mention the same Agent for a related follow-up.
1. Expect a threaded `m.notice` reply. Long work may first show a placeholder and later edit it; do not assume silence means a request is still running.
1. Verify important answers against cited sources or an authoritative system before acting.

One Agent's conversation context is separate from another Agent's, even in the same room. Starting a new room or using a different Agent does not transfer the previous context automatically.

## 3. Safety and rate expectations

Agent output can be incomplete, incorrect, manipulated by untrusted room content, or unsafe to execute. A reply is a notice for human review; bots and workflows must not treat it as an actionable automation event. Follow your organization's approval process for changes, messages, payments, credentials, or regulated decisions.

The bridge limits invocations per sender/Agent and per room, and separately bounds concurrent/queued work. Repeated requests can receive a rate/capacity notice or no additional response when the response plane itself is exhausted. Wait, reduce duplicate prompts, and ask the room owner if the legitimate workload needs a reviewed policy change. Do not evade a limit by changing rooms, identities, or spellings.

Failure notices are intentionally generic. “Not allowed,” “rate limited,” “unavailable,” or “timed out” does not disclose credentials or internal errors. Give the room owner the Matrix event ID and time, not the sensitive prompt, so an operator can use the content-free audit path.

## 4. What the room owner controls

The room owner and platform operators decide membership, Agent mappings, sender allowlists, model boundary, tools, rate limits, federation peers, retention, and incident handling. Your Matrix login establishes your account and room participation; it does not automatically authorize every Agent, tool, document, or partner route.

For a privacy or security concern, stop sharing data and contact the named room/operator channel. Suspected credential exposure is handled privately under [`SECURITY.md`](../../SECURITY.md), with rotation first; do not post it as a public issue.

> **Own vs compose.** Fgentic owns how an allowed Matrix mention is delegated and returned under governance controls. Matrix/Element provide the room and client; kagent runs the Agent; agentgateway routes the call; the model and tools generate the result. The room owner governs the assembled experience, while each component retains its own behavior and support boundary.
