---
type: Architecture Decision Record
title: Authenticated Query-Embedding Egress for Permission-Aware Retrieval
description: Permit exactly one authenticated egress edge from the retrieval service to the agentgateway embeddings listener so it can embed a natural-language query server-side without weakening ADR 0017's egress minimization.
---

# 0021 — Authenticated Query-Embedding Egress for Permission-Aware Retrieval

Status: Proposed

Approval: pending — this amends an accepted security-boundary ADR ([0017](0017-permission-aware-identity-binding.md)) on the p0 wedge and requires an explicit maintainer decision before [#333](https://github.com/fmind-ai/fgentic/issues/333) implements it.

## Context

[ADR 0017](0017-permission-aware-identity-binding.md) makes `knowledge-retrieval` the single chunk-row ACL enforcement point (§37) and deliberately minimizes its reachable surface: its §49 states the retrieval namespace is default-deny and _"the service may reach only DNS, its scoped knowledge database, and separately declared observability sinks."_ That minimization is load-bearing — the wedge's whole point is that the enforcement service cannot exfiltrate.

Two facts now collide with §49 as written:

1. **The tool contract requires server-side query embedding.** [#333](https://github.com/fmind-ai/fgentic/issues/333) exposes one read-only MCP tool, `search_knowledge`, taking a bounded natural-language query; the ACL/classification `WHERE` predicate runs before vector distance ranking. The pgvector store holds fixed `vector(1024)` bge-m3 embeddings ([#331](https://github.com/fmind-ai/fgentic/issues/331)), so the query must be turned into a 1024-dim vector to rank. Accepting a precomputed vector as a tool argument is explicitly out of contract (authorization operands and ranking operands are never caller-supplied), so the embedding must happen server-side, inside `knowledge-retrieval`.
1. **A sovereign embeddings runtime now exists behind agentgateway.** [#340](https://github.com/fmind-ai/fgentic/issues/340) (merged) serves `BAAI/bge-m3` at `knowledge-embeddings.models.svc.cluster.local:8000` reachable only through the agentgateway embeddings listener, credential-free and metered. Ingestion ([#332](https://github.com/fmind-ai/fgentic/issues/332)) already embeds through that listener via an authenticated `knowledge-ingestion` API-key workload (`infra/knowledge/base/embeddings-route.yaml`).

But §49 permits `knowledge-retrieval` no path to that listener. There is therefore **no legal way to embed the query**: making the vector a tool argument, adding an unreviewed lexical fallback, or silently opening an embeddings egress edge would each contradict the accepted tool, schema, NetworkPolicy, or ADR contract. #333 (and its downstream [#336](https://github.com/fmind-ai/fgentic/issues/336)) is blocked on this gap, not on runtime availability.

## Options considered

1. **Authenticated retrieval → agentgateway embeddings egress edge (recommended).** Amend §49 to add exactly one egress destination for `knowledge-retrieval`: the agentgateway embeddings listener, reached with a dedicated `knowledge-retrieval` API-key workload distinct from `knowledge-ingestion`. This mirrors the already-accepted ingestion pattern, keeps the credential-free chokepoint and token metering intact, and adds no direct model or database credential to the retrieval pod.
1. **Caller/gateway pre-embeds the query and passes the vector.** Rejected. It makes the ranking operand a tool argument (out of contract), moves an untrusted 1024-float blob across the ext-auth boundary, and lets a caller substitute an arbitrary vector to probe the store's geometry independent of its text — expanding, not shrinking, the trust surface.
1. **Lexical/keyword DB prefilter instead of vector search.** Rejected. #331 defines a vector-only store with no reviewed lexical index; adding a full-text path is a separate, unreviewed retrieval-quality and injection surface and does not deliver the intended semantic grounding.

## Decision (proposed)

1. Amend [ADR 0017](0017-permission-aware-identity-binding.md) §49 to read: the retrieval service may reach DNS, its scoped knowledge database, separately declared observability sinks, **and the agentgateway embeddings listener for query embedding only.** No other egress is added; kagent and Agent pods still have no direct service or database path.
1. `knowledge-retrieval` embeds the query through the agentgateway embeddings listener using a dedicated `knowledge-retrieval` API-key workload (its own SOPS Secret, distinct from `knowledge-ingestion`), authorized by a CEL policy that admits only `apiKey.workload == "knowledge-retrieval"` on `POST /v1/embeddings` (and `POST /tokenize` if a token preflight is used). The retrieval consumer route + authorization remain [#333](https://github.com/fmind-ai/fgentic/issues/333)'s owned scope, exactly as ingestion's route/auth are #332's; #340 continues to own only the runtime, catalog, and proxy-to-model NetworkPolicy edge.
1. The egress NetworkPolicy admits the retrieval pod to the agentgateway proxy on the embeddings-listener port only — never to the knowledge database of another tenant, another agentgateway listener, kagent, or the public internet. Bound the embedding request bytes and rate exactly as ingestion bounds its listener.
1. Everything else in ADR 0017 is unchanged: the gateway-projected identity, the parameterized ACL/classification `WHERE` predicate before ranking, statelessness across delegations (§46), the bridge poll/cancel and public-timeout rules (§47), the single-carrier MCP route (§48), and the `public`/`approved_non_public` classification ceiling (§50).

## Consequences

1. **#333 becomes implementable** without contradicting any accepted contract; the wedge's security review can proceed on a coherent design.
1. **The added egress surface is narrow and non-exfiltrating.** The new edge reaches only the embeddings listener, which accepts text and returns vectors; it carries no corpus content and no ACL metadata. The ACL/classification prefilter still runs _inside_ `knowledge-retrieval` on the database read, which is upstream of and independent from this egress. A compromised retrieval pod could embed arbitrary attacker text (already possible for any caller of the tool) but gains no new path to read or exfiltrate ACL-restricted chunks; the classification ceiling and the fail-closed carrier validation are unchanged.
1. **Metering and the chokepoint invariant hold** (D7/D16): the query embedding is one more metered call through agentgateway; no agent or service holds a model credential.
1. **Cost:** every retrieval is now at least one embedding call (bounded query bytes, rate-limited per the retrieval workload). This is inherent to semantic retrieval and is capped by the same per-workload limits ingestion uses.
1. If the maintainer rejects this amendment, #333 stays blocked and an alternative (e.g. a signed capability that lets a trusted upstream component embed and hand the retrieval service a bound, non-forgeable query vector) must be designed instead — a larger change than Option 1.

## Human decision required

This is a security-boundary change to an accepted ADR on the definitive-v1 wedge. It is prepared up to the acceptance gate only: no code, NetworkPolicy, or route in this change opens the egress. A coding agent must not implement #333's query-embedding edge until the maintainer accepts this ADR (flip Status to Accepted with an approval link, as ADR 0016/0017 record).
