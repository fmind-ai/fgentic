---
type: Runbook
title: Reading the Cost Dashboards
description: Operator guide to the token, delegation, reservation, attribution, and currency boundaries of Fgentic dashboards.
---

# Reading the cost dashboards

Fgentic currently answers two cost-safety questions: how much model-token activity agentgateway reported, and whether bridge delegation pressure moved at the same time. It does not yet answer which person, room, team, or partner consumed those tokens, and it does not compute an invoice.

## Start with the two dashboards

| Dashboard                          | Panel or signal                                | What the number can be attributed to                                            | What it cannot be attributed to                                            |
| ---------------------------------- | ---------------------------------------------- | ------------------------------------------------------------------------------- | -------------------------------------------------------------------------- |
| `Fgentic — Bridge`                 | Delegations by agent and outcome               | One configured ghost and one bounded terminal outcome                           | A unique invocation, sender, room, team, token count, or currency amount   |
| `Fgentic — Bridge`                 | Rate-limit rejections                          | One configured ghost whose bridge admission was rejected                        | Which sender/room bucket rejected it or how many model tokens were avoided |
| `Fgentic — LLM Token & Cost Guard` | Token rate by provider, model, route, and type | Aggregate provider-reported token activity for the displayed gateway dimensions | A ghost, Matrix sender, room, tenant, partner, task, or invoice            |
| `Fgentic — LLM Token & Cost Guard` | 15-minute token burn versus guard              | Aggregate token increase compared with the configured warning threshold         | A hard quota, remaining budget, per-team ceiling, or currency spend        |
| `Fgentic — LLM Token & Cost Guard` | Cost-catalog lookup coverage                   | Whether agentgateway resolved a catalog entry for a provider/model request      | A currency-cost value or proof that a reviewed Fgentic price was applied   |

The bridge and gateway signals can corroborate platform activity, but their clocks and aggregate labels do not establish a deterministic join. A simultaneous increase is a diagnostic lead, not proof that one sender or room caused the model usage.

## Investigate a token-burn warning

1. Open `Fgentic — LLM Token & Cost Guard` and identify the provider, model, route, and token-type series that increased.
1. Compare that interval with per-ghost delegation outcomes, rate-limit rejections, queue depth, and in-flight work in `Fgentic — Bridge`.
1. Check for an invocation loop, unexpected automation, route change, or model behavior before changing the threshold. Preserve D7 sender/room rate limits and the queue bounds.
1. When an exact sender, room, event, or task matters, use the restricted [delegation attribution runbook](audit.md). Do not add its raw or hashed identifiers to Prometheus.
1. Report model tokens as aggregate consumption. Report currency, person/team attribution, and partner consumption as unavailable unless a later versioned price and authenticated-correlation contract supplies direct evidence.

## Partner reservations are not consumption

The federation border verifies the client credential and reserves the request's declared `maxTokens` against a per-client admission window. That reservation limits accepted work before execution. It is not the token count later reported by the model, and unused capacity is not consumed usage.

The current federation rate-limit component keeps reservation state in Redis with StatsD disabled and provisions no per-client Prometheus series. Consequently no committed dashboard can show per-`azp` reservation posture today. Even after such telemetry exists, its panel must say **reserved, not consumed** and remain separate from the agentgateway token panels.

## MCP admissions are not consumption

The governed MCP route checks a per-authenticated-Agent/tool burst ceiling and a broader per-authenticated-Agent hourly ceiling. These fixed-window counters record admitted call attempts before tool execution; they do not prove that a tool succeeded, measure tool work, reserve model tokens, or represent currency spend. A future human-approval control is independent: approval cannot bypass the ceilings, and an admitted approved attempt still advances them.

JSON-RPC batches are rejected before quota admission because one HTTP-level descriptor cannot account each batched tool call independently. Their fixed `batch_rejected` audit classification is a policy denial, not consumption and not evidence that any tool ran.

The MCP rate-limit service also keeps StatsD disabled, so no committed dashboard exposes per-Agent or per-tool counters. Use the restricted content-free audit record to investigate HTTP 429 outcomes, and do not turn its authenticated Agent or tool names into unbounded Prometheus labels without a separate observability and privacy decision.

## Why there is no room or identity series

MXIDs and Matrix room IDs are linkable personal/operational data with unbounded churn. Hashing preserves both linkability and cardinality; allowlisting raw room IDs still accumulates series over Prometheus retention. Fgentic therefore keeps exact room/sender attribution in the access-controlled audit path and keeps Prometheus bounded to ghost/outcome plus provider/model/route/token-type dimensions.

The pinned [agentgateway v1.3.1 GenAI label type](https://github.com/agentgateway/agentgateway/blob/v1.3.1/crates/agentgateway/src/telemetry/metrics.rs) has no stable Fgentic user or team identity, and the platform does not add one as a custom metric field. Per-team showback needs a later bounded tenant identity backed by an authenticated mapping; timing correlation or a forwarded `X-User-Id` must not be promoted into a billing principal.

## Verification boundary

`mise run check:dashboards` proves the versioned JSON, panel queries, sidecar configuration, and Flux-rendered ConfigMap. It cannot manufacture traffic or prove a live series. Installed-cluster acceptance still requires representative activity, non-empty dashboard queries, and negative checks that no MXID/room-ID labels appear. The runtime result must state the exact tested revision and profile; do not infer it from this source contract.
