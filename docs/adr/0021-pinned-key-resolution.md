---
type: Architecture Decision Record
title: Out-of-Band Pinned Key Resolution for the ActivityPub Border
description: Verify a closed set of operator-pinned ActivityPub signers from an out-of-band key without a network fetch, so an in-cluster peer that the SSRF guard cannot fetch is still governed — while every unpinned actor stays on the unchanged guarded resolver.
---

# 0021 — Out-of-Band Pinned Key Resolution for the ActivityPub Border

Status: Accepted

## Context

The ActivityPub federation border verifies every inbound activity's HTTP Signature before any A2A call: it fetches the signer's public key from the signature `keyId` URL, binds the resolved owner to the activity actor, and checks the git-reloadable allowlist ([fediverse spec §3](../fediverse.md), [0014](0014-activitypub-second-federation-transport.md)). That key fetch is deliberately hardened against SSRF ([#320](https://github.com/fmind-ai/fgentic/issues/320)): `internal/safehttp` permits only public HTTPS destinations and re-resolves DNS to a public address at dial time, so a compromised or malicious remote cannot pivot the gateway into cloud metadata or in-cluster services.

That guard is correct for the real Fediverse, where peers are public. It also makes an **in-cluster peer unverifiable**: the demo profile's GoToSocial/Mastodon-wire interop peer ([#489](https://github.com/fmind-ai/fgentic/issues/489) Task 7) is reachable only at a private ClusterIP over plain HTTP, which the guard rejects before dialing. With no way to resolve the peer's key, the border denies even a legitimate, allowlisted peer at `key_unresolved`, so the whole governed-reply path cannot be exercised on demo. There is no production injection point in the resolver, and **weakening the SSRF guard is not an option** — it is a hard prerequisite that closed before the gateway was reconciled.

The platform already solves the analogous problem for A2A: an explicitly configured remote agent is trusted through a **pinned** identity — a currently verified, pinned Signed AgentCard and pinned P-256 JWK per remote agent, verified per call ([federation spec §8.3](../federation.md)). The same first principle — an operator vouches for a specific counterparty's key out of band — applies to the ActivityPub transport.

## Decision

Add an **optional, out-of-band pinned-key resolver** to the ActivityPub HTTP-Signature border (`internal/httpsig.PinnedResolver`, wired via `PINNED_KEYS_PATH`).

1. **Pinned actors skip the network entirely.** For an actor URI present in the operator-provided pin map (`{"pins": {"<actorURI>": "<publicKeyPem>"}}`), the resolver returns the pinned key and binds the owner to that exact actor URI — with **no HTTP request at all**. Signature verification, owner→actor binding, allowlist admission, budget reservation, dedup, and FEP-8b32 outbound signing are otherwise unchanged and still enforced.

1. **Unpinned actors are unchanged.** Any actor not in the pin map falls through to the existing `#320`-guarded HTTPS resolver, byte-for-byte as before. The pinned resolver wraps, never replaces, the guarded one.

1. **Pinning strictly reduces SSRF surface — it never widens it.** A pinned actor causes _fewer_ network requests (zero); an unpinned actor causes the _same_ guarded request as today. A pin can only ADD a key an operator placed by hand; it can never redirect, relax, or bypass the guarded path for a non-pinned actor. Therefore this mode cannot weaken #320 by construction.

1. **Fail closed.** The resolver is opt-in (empty `PINNED_KEYS_PATH` keeps today's pure guarded-HTTP behavior). When enabled, an unreadable file, malformed JSON, an empty map, a blank actor URI, an actor URI carrying a fragment, or an unparseable/empty PEM all fail at construction — never a resolver that silently falls through to the network for a key the operator believed was pinned. A pin is selected only by an **exact** actor match (the fragment-stripped `keyId`), so no prefix or substring can select it, and the resolved owner is always the pinned actor URI, so a pinned key can never speak for a different actor.

1. **Demo-scoped enablement.** Only the demo profile enables pinning, to admit the two in-cluster interop peers ([#489](https://github.com/fmind-ai/fgentic/issues/489)); `local` and `gcp` keep the pure guarded resolver. A production deployment MAY enable it to pin a specific, out-of-band-verified partner whose key is known but who is not conveniently SSRF-fetchable — the same posture as a pinned Signed AgentCard.

## Consequences

1. An in-cluster (or otherwise non-SSRF-fetchable) peer can be verified against a key the operator vouches for, so the demo profile can prove the full governed border — signature, allowlist, budget, dedup, and signed reply — over the real transport without exposing a public inbox or weakening #320.
1. Trust in a pinned key is exactly as strong as the operator's out-of-band verification of it, identical to the pinned-Signed-AgentCard model. Pins are configuration an operator curates; a stale or wrong pin is an operator error, not a remotely exploitable one (the key still only authorizes its own exact actor, still subject to allowlist and budget).
1. The resolver is unit-tested for the load-bearing invariants: a pinned actor resolves with zero fallback calls; an unpinned private/loopback `keyId` is still rejected by the guard; malformed pins fail closed at construction; and only an exact actor match selects a pin (no prefix/substring bypass).
1. This is additive and reversible: removing `PINNED_KEYS_PATH` restores the pure guarded resolver with no other change.

Cross-references: [#320](https://github.com/fmind-ai/fgentic/issues/320) (SSRF guard), [#489](https://github.com/fmind-ai/fgentic/issues/489) (demo interop), [fediverse spec §3](../fediverse.md) (the border and its twin controls), [0014](0014-activitypub-second-federation-transport.md) (the transport).
