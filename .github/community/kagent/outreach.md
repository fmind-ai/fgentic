# kagent community outreach handoff

Status: prepared for maintainer delivery; do not post, schedule, or present automatically.

Verified on 2026-07-18 against the official [kagent community page](https://kagent.dev/community), [community repository](https://github.com/kagent-dev/community), [Slack A2A example](https://kagent.dev/docs/kagent/examples/slack-a2a), and [Discord A2A example](https://kagent.dev/docs/kagent/examples/discord-a2a).

## Recommended route

1. Join the public [kagent Discord](https://discord.com/invite/Fu3k65f2k3) or [CNCF `#kagent` Slack channel](https://cloud-native.slack.com/archives/C08ETST0076) and ask which upcoming weekly community call can take a five-minute integration demo.
1. Use the current [community calendar](https://calendar.google.com/calendar/u/0?cid=Y183OTI0OTdhNGU1N2NiNzVhNzE0Mjg0NWFkMzVkNTVmMTkxYTAwOWVhN2ZiN2E3ZTc5NDA5Yjk5NGJhOTRhMmVhQGdyb3VwLmNhbGVuZGFyLmdvb2dsZS5jb20) rather than copying a meeting time that can drift.
1. Share the [one-pager](one-pager.md) before the call and rehearse the [five-minute demo](demo-script.md) on the exact revision to be presented.
1. Ask first for the smallest durable relationship: an integration guide or “works with kagent” ecosystem entry. Explore a larger relationship only if maintainers identify a reusable upstream boundary.
1. Record the public outcome and follow-ups on [fmind-ai/fgentic#67](https://github.com/fmind-ai/fgentic/issues/67).

## Copy-ready introduction

```text
Hi kagent community — I maintain Fgentic, an experimental Apache-2.0 Matrix-to-A2A collaboration platform built around kagent Agents.

kagent already has useful Slack and Discord A2A examples. Fgentic explores a complementary surface: self-hosted Matrix rooms where humans and agent identities collaborate, with organization-to-organization federation as the destination. The Go appservice bridge maps an exact Matrix ghost to an unmodified kagent Agent, applies sender/rate/capacity policy before dispatch, delegates through A2A, and returns the result into the room.

Could I show a five-minute demo at a community call and get feedback on whether a maintained Matrix/A2A integration guide or “works with kagent” entry would be useful? Fgentic is pre-1.0, and I will present the current attribution, encryption, and cost-evidence limits explicitly rather than imply endorsement or production readiness.

One-pager: https://github.com/fmind-ai/fgentic/blob/main/.github/community/kagent/one-pager.md
Demo script: https://github.com/fmind-ai/fgentic/blob/main/.github/community/kagent/demo-script.md
```

## Copy-ready session abstract

**Fgentic: federated Matrix rooms as a human interface for kagent**

Fgentic connects Matrix rooms to Kubernetes-native kagent Agents through A2A. This five-minute demo follows one room request through a Go Matrix appservice, agentgateway, and an unmodified kagent Agent, then shows the reply, sender attribution, and admission controls. It explains what Matrix federation adds beyond single-tenant chat adapters and states the current security and cost-evidence limits. The discussion asks whether the community wants a maintained integration guide or “works with kagent” entry.

## Outcome record

Add a comment to #67 after the interaction with facts, not interpretation:

```markdown
Presented/contacted: <UTC date and public meeting, Discord thread, or Slack thread>

Revision and profile shown: <git SHA; demo, vllm, or named configured provider>

Public evidence: <recording, notes, or thread URL; "none public" if unavailable>

Outcome:

- Integration guide: <accepted, requested changes, declined, or not discussed>
- “Works with kagent” listing: <accepted, requested changes, declined, or not discussed>
- Larger relationship: <maintainer guidance; do not infer subproject status>

Owners and follow-ups:

- <owner> — <specific action> — <date or trigger>

Claims to update:

- <documentation that must change because of confirmed upstream feedback>
```

The issue remains open until the pitch is delivered and the relationship or explicit community outcome is documented. A presentation alone is not an upstream listing, integration acceptance, or subproject relationship.
