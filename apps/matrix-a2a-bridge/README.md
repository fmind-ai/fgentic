# matrix-a2a-bridge

A Matrix ↔ A2A bridge: a **Matrix Application Service** that turns an `@mention` into an **A2A** task delegation and posts the agent's reply back into the room. It is the piece of custom glue in the [`Fgentic`](../../README.md) platform — everything else is off-the-shelf open source.

```text
Human in a Matrix room:  "@agent-platform-helper why is the bridge pod not ready?"
   │  Synapse pushes the event to the bridge (it owns the @agent-* ghost namespace)
   ▼
   bridge:  detect @mention → map @agent-platform-helper → kagent/platform-helper
            or map a remote ghost → an exact URL with a pinned signed AgentCard identity
            commit the transaction + per-target job before HTTP 200
            fenced per-room lease → A2A SendMessage (non-streaming)
   ◀  reply text
   ▼  post as @agent-platform-helper, as a reply to the original message
Human sees the answer in Element.
```

At startup, and after every valid `agents.yaml` change, the bridge also fetches each mapped AgentCard. The routing file declares `schemaVersion: 1` and is covered by the published [`agents.schema.json`](agents.schema.json); unknown majors fail closed. Files without the field temporarily load as v1 with a deprecation warning. Local kagent cards use the authenticated agentgateway path, and the SDK's selected v1 JSON-RPC interface is rebound to that operator-configured base plus the exact agent route. Pointing the base directly at kagent preserves the same binding without hard-coding agentgateway. Remote cards are fetched directly from `<url>/.well-known/agent-card.json` and are trusted only when their A2A v1.0 ES256 signature and pinned identity match. The card's human-readable name becomes the ghost's Matrix display name; its description and skills back the local `!agents` directory. Local profile refresh failures retain the last-known profile. A remote card that becomes unsigned, mismatched, or tampered disables invocation until a later refresh validates it again.

## How it works

1. The bridge registers an **exclusive appservice namespace** `@agent-.*:fgentic.fmind.ai` plus the `@a2a-bridge` bot. Every kagent agent thus appears as a first-class room member.
1. At startup it resolves every mapped AgentCard and synchronizes the ghost's standard Matrix display name plus an optional configured `mxc://` avatar. Matrix v1.16 homeservers also receive a namespaced profile field containing the description and skills. The projected agent ConfigMap is polled and valid routing, policy, and profile changes reload without a pod restart; malformed updates leave the last-known configuration active.
1. On each `m.room.message`, it reads the typed `m.mentions` field (MSC3952) — with a plaintext-body fallback — to find which agent(s) were addressed.
1. It maps the ghost (`agent-platform-helper`) to either a local kagent `(namespace, name)` or a pinned remote A2A `url` via `agents.yaml` — the **authorization allowlist**: only mapped ghosts are invokable, only allowed senders/homeservers may invoke them (federated look-alikes are rejected), and per-sender/per-room token buckets guard LLM spend. Remote targets add a request timeout, partner-enforced token-budget metadata, and expected signed-card identity. Configured `bridgedOrigins` classify only anchored full-MXID namespaces; Slack, Telegram, and future bridged senders are denied until the target agent's `allowedSenders` explicitly matches them. Terminal audits expose only bounded `sender_origin_kind`/`sender_origin_network` attribution in addition to the existing Matrix identifiers.
1. An in-room `!agents` command replies locally as `@a2a-bridge` with a bounded gallery of only the mappings the sender may invoke: full ghost MXID, AgentCard description, declared skills, target type, and live/cached/fallback status. `!agents <name>` expands one authorized mapping without another AgentCard fetch. A quarantined remote remains visible as unavailable but exposes no stale card claims. The gallery uses the same sender and room admission controls as delegation but never calls A2A or an LLM.
1. Before acknowledging the appservice transaction, it hashes the exact bounded body and atomically stores the transaction plus one immutable row per eligible `(event, ghost)`. Serialized admission counts every non-terminal queued, delayed, or leased job against `ROOM_QUEUE_CAPACITY` and `GLOBAL_QUEUE_CAPACITY`. It checks the room limit first for each new target, then the global limit; accepted targets earlier in the same transaction consume capacity. A full target still commits as a content-scrubbed terminal `denied` row while other eligible targets can be accepted. An exact replay is idempotent; changed bytes under the same transaction ID are HTTP 409. One coordinator claims fenced leases under global `CONCURRENCY`, and Postgres keeps the oldest non-terminal job ahead of later work in the same room across process replacement.
1. It calls the agent over A2A `SendMessage`, threading a per-`(room, agent)` `contextId` for multi-turn conversations. A deterministic message ID and attempt boundary are persisted first, but they do not prove target idempotency: a send whose acknowledgement is unknown becomes `ambiguous` and is not repeated. An acknowledged long-running task is resumed via `GetTask`, with a deterministic placeholder event edited (`m.replace`) into the final answer.
1. It extracts the reply text from the returned `Task | Message` and posts it back **as the ghost user** (`m.notice`, so other bots ignore it), as a reply to the original message.
1. State (mautrix StateStore, versioned transaction/delegation ledger, contexts, and a legacy processed-event compatibility table) persists in Postgres (`DATABASE_URL`); without it the bridge runs dev-only in-memory state. Terminal transitions scrub event/prompt/result content, retain delivered/denied tombstones for at least 24 hours, and preserve ambiguous/dead evidence for operator review.
1. Durable recovery currently covers initial mentions, known-task polling, and Matrix reply/placeholder/edit projection. Typing, input-required continuation, room-reaction cancellation, intermediate progress posts, and pin state remain legacy in-memory behavior and are not crash-safe.

## Build

This repo ships source only; resolve dependencies first:

```bash
mise install               # == go mod tidy   (creates go.sum)
mise run test              # race-enabled Go tests
mise run test:integration  # real Matrix appservice wire path in disposable kind
mise run test:availability # graceful replacement fixture in disposable kind
mise run test:crash-recovery # six persisted SIGKILL boundaries in disposable kind
mise run test:load         # §12.5 durable-ledger load regression in disposable kind
mise run eval:models -- --profile vertex --model google/gemini-2.5-flash
mise run build             # bin/matrix-a2a-bridge
mise run build:image       # distroless image
```

The load task's scenario, assertions, and dated reference measurements are recorded in [`../../docs/performance.md`](../../docs/performance.md).

The integration task is also the runtime-independence gate: its disposable kind cluster installs no kagent resources. A plain official-SDK A2A server runs in a separate restricted namespace behind a default-deny NetworkPolicy and in its own non-root distroless image, which contains no bridge binary. The bridge invokes it through a signed `url:` mapping and verifies the real Matrix reply, full sender attribution, token-budget activation, per-sender rate rejection, delegation metrics, and fail-closed card tampering.

The model-evaluation task port-forwards the local agentgateway, runs 10 fixed A2A scenarios for each of `platform-helper`, `docs-qa`, and `scribe`, and writes private machine JSON plus a Markdown comparison to `../../.agents/tmp/model-eval/`. Set `A2A_API_KEY` when the local A2A route requires its workload credential. The task calls the configured model, so run it only after approving that provider access. See [`../../docs/models.md`](../../docs/models.md) for scoring, metric-attribution, pricing-catalog, and review rules.

## Configuration (environment)

| Variable                                            | Default                                                                | Purpose                                                                                             |
| --------------------------------------------------- | ---------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `HOMESERVER_URL`                                    | `http://synapse.matrix.svc.cluster.local:8008`                         | Matrix Client-Server API (Synapse).                                                                 |
| `SERVER_NAME`                                       | `fgentic.fmind.ai`                                                     | Matrix server_name (the `:domain` of every user ID).                                                |
| `A2A_BASE_URL`                                      | `http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8080` | A2A base; agents at `/api/a2a/<ns>/<name>`. Point at kagent (`…kagent…:8083`) to skip agentgateway. |
| `A2A_API_KEY`                                       | _(empty)_                                                              | Bridge workload credential for the protected agentgateway A2A route.                                |
| `GHOST_PREFIX`                                      | `agent-`                                                               | Local-part prefix for agent ghosts.                                                                 |
| `REGISTRATION_PATH`                                 | `/etc/matrix-a2a-bridge/registration.yaml`                             | Appservice registration (as_token/hs_token).                                                        |
| `AGENTS_PATH`                                       | `/etc/matrix-a2a-bridge/agents/agents.yaml`                            | Bridged-origin namespaces, ghost → agent routing, profile fallback, and allowlist map.              |
| `AGENTS_RELOAD_INTERVAL`                            | `5s`                                                                   | Poll interval for atomic `agents.yaml` reloads.                                                     |
| `AGENT_CARD_REFRESH_INTERVAL`                       | `5m`                                                                   | Independent revalidation interval for signed remote AgentCards.                                     |
| `OTEL_EXPORTER_OTLP_ENDPOINT`                       | _(empty)_                                                              | Standard OTLP/HTTP endpoint; tracing is disabled when unset.                                        |
| `DATABASE_URL`                                      | _(empty)_                                                              | Postgres URL for persistent state (empty = in-memory, dev only).                                    |
| `APPSERVICE_TRANSACTION_MAX_BYTES`                  | `16777216`                                                             | Exact pre-ACK appservice JSON body cap (16 MiB).                                                    |
| `REQUEST_TIMEOUT`                                   | `60s`                                                                  | Transport deadline on a single A2A SendMessage.                                                     |
| `TASK_TIMEOUT`                                      | `10m`                                                                  | Whole-task deadline from the first persisted A2A attempt; durable backlog time is excluded.         |
| `SHUTDOWN_TIMEOUT`                                  | `25s`                                                                  | Grace for active leased jobs after Matrix intake and new claims stop.                               |
| `CONCURRENCY`                                       | `16`                                                                   | Max active leased delegations across all rooms; Postgres enforces per-room FIFO.                    |
| `ROOM_QUEUE_CAPACITY`                               | `32`                                                                   | Max non-terminal durable jobs per room; room capacity is checked before global capacity.            |
| `GLOBAL_QUEUE_CAPACITY`                             | `256`                                                                  | Max non-terminal durable jobs across rooms; must be at least `CONCURRENCY`.                         |
| `DELEGATION_CLAIM_INTERVAL`                         | `1s`                                                                   | Idle coordinator poll interval; successful intake also wakes it immediately.                        |
| `DELEGATION_LEASE_DURATION`                         | `30s`                                                                  | Expiring fenced job lease; active work heartbeats before half this duration.                        |
| `DELEGATION_RETRY_INITIAL` / `DELEGATION_RETRY_MAX` | `1s` / `30s`                                                           | Capped exponential recovery backoff.                                                                |
| `DELEGATION_MAX_ATTEMPTS`                           | `5`                                                                    | Consecutive recovery failures before terminal `dead`.                                               |
| `SENDER_RATE_PER_MINUTE` / `SENDER_RATE_BURST`      | `6` / `3`                                                              | Token bucket per (sender, agent).                                                                   |
| `ROOM_RATE_PER_MINUTE` / `ROOM_RATE_BURST`          | `30` / `10`                                                            | Token bucket per room.                                                                              |
| `RATE_LIMIT_BUCKET_CAPACITY`                        | `4096`                                                                 | Hard cap for each invocation/notice sender/room map; unknown keys fail closed when full.            |
| `LISTEN_HOST` / `LISTEN_PORT`                       | `0.0.0.0` / `29331`                                                    | Appservice HTTP bind.                                                                               |
| `LOG_LEVEL` / `LOG_FORMAT`                          | `info` / `json`                                                        | Structured logging.                                                                                 |

Durable admission serializes capacity accounting so concurrent appservice transactions cannot oversubscribe the configured 32-per-room/256-global defaults. Terminal tombstones do not consume capacity. A refused target is still durable before HTTP 200: its row is terminal `denied`, carries only identifiers plus `queue_room_capacity_rejected` or `queue_global_capacity_rejected`, and has its event/prompt/payload content cleared. For a newly accepted transaction, the intake bridge consumes the returned denial list once and preserves the established content-free signal: it increments `fgentic_delegations_total{outcome="queue_full"}` and emits a terminal audit with `outcome=queue_full`, `terminal_stage=queue`, the same stable reason, `rate_limit_verdict=not_checked`, and `a2a_attempted=false`; no A2A request or invocation-budget token is produced. After mautrix consumes the transaction, a fixed-size best-effort worker may post one actionable failure notice through the shared notice buckets; handoff or bucket exhaustion suppresses it. Exact replay does not repeat the signal or notice. The terminal ledger row remains the authoritative refusal evidence if the process exits after commit but before process-local audit, metric, or notice emission.

On termination, the bridge-owned appservice server stops new intake and force-closes any transaction connection that exceeds its five-second grace. The synchronous Matrix event processor drains, the durable queue stops new claims, and active leases receive `SHUTDOWN_TIMEOUT` to finish under the chart's 45-second pod grace. Cancellation after that window leaves non-terminal jobs recoverable when their leases expire; it does not manufacture a terminal `shutdown` result. `mise run test:availability` exercises this graceful SIGTERM path. The separate `mise run test:crash-recovery` fixture SIGKILLs the real bridge at six persisted boundaries against Postgres, Synapse, and A2A; run it on the candidate revision before claiming hard-crash recovery. Neither fixture establishes node-loss behavior or a production RTO.

## Remote A2A targets

Remote targets are explicit and fail closed; the default chart enables none. Configure exactly one target form per ghost:

```yaml
schemaVersion: 1
agents:
  agent-partner-reviewer:
    url: https://agents.partner.example/a2a/reviewer
    timeout: 30s
    tokenBudget: 4096
    cardIdentity:
      name: Partner reviewer
      organization: Partner Example
      keyID: partner-reviewer-2026-07
      publicKey:
        kty: EC
        crv: P-256
        x: <base64url-x-coordinate>
        y: <base64url-y-coordinate>
    allowedSenders:
      - "@alice:fgentic.fmind.ai"
```

`url` is the exact, canonical A2A JSONRPC or HTTP+JSON endpoint, without a trailing slash; it is not a discovery root. Its public AgentCard must be available at `<url>/.well-known/agent-card.json`, advertise that exact binding with no unpinned tenant, include `https://fgentic.fmind.ai/a2a/extensions/token-budget/v1`, require no other unsupported extension, and carry an A2A v1.0 detached JWS over the JCS-canonical card. The bridge accepts only `ES256`: the signature must verify under the configured P-256 public JWK, while the protected `kid`, card name, provider organization, and endpoint URL must match their pins. `timeout` is an additional whole-delegation ceiling combined with the global `REQUEST_TIMEOUT` and `TASK_TIMEOUT`; the whole-task clock starts at the first persisted A2A attempt boundary, so time waiting in the durable room backlog does not falsely consume an agent's deadline. The positive scalar `tokenBudget` is sent as `{maxTokens: …}` extension metadata and activated with `A2A-Extensions`; it is a partner-enforced request contract, not bridge-observed or hard local model-token accounting.

The bridge normally verifies the card before the delegation worker registers the ghost or admits a limiter token, revalidates it every `AGENT_CARD_REFRESH_INTERVAL`, and checks the verified generation again at the actual HTTP boundary. The independent Matrix membership-invite handler may still register a mapped ghost without invoking it. An unsigned, invalid, or identity-mismatched card before initial dispatch produces a bounded `agent_card_untrusted` audit with `a2a_attempted=false`; no invocation reaches the endpoint. If trust changes after an earlier request created a task, the bridge stops polling that task. The current remote transport deliberately sends neither the local `A2A_API_KEY` nor a separate mTLS/OIDC credential; credentialed partner listeners are follow-up work. A Signed AgentCard authenticates the declared agent identity, not the Matrix sender or transport by itself.

## Discovering agents in a room

Invite `@a2a-bridge:<server-name>` or one of the mapped ghosts into an unencrypted agent room so Synapse sends that room's events to the appservice. Then send:

```text
!agents
```

Element clients interpret leading `/` commands before sending a message. Use the client-safe `!ask <agent> <prompt>`, `!agents [name]`, and `!budget` forms in Element. The bridge also accepts `/ask`, `/agents`, and `/budget` from clients and integrations that send leading slashes unchanged.

The bridge replies with the agents authorized for your exact Matrix identity. Use the full MXID shown in that reply when mentioning an agent, for example `@agent-platform-helper:fgentic.fmind.ai inspect the bridge pod`.

Use `!agents <name>` (for example, `!agents platform-helper`) to inspect one authorized agent's bounded description and declared skills. Remote mappings whose Signed AgentCard is not currently trusted remain visible as unavailable, but the bridge hides their cached metadata until trust is restored.

Element reliably displays the standard Matrix display name and configured avatar. The description is also synchronized through Matrix's standard arbitrary-profile-field API when the homeserver supports v1.16, but Element clients do not consistently render arbitrary profile fields in member details. `!agents` is therefore the portable, authoritative description/status/skills view. `AgentCard cached (refresh failed)` means the last successful local card is still in use; `AgentCard unavailable (configured fallback)` means no local card has yet been fetched and the chart-derived startup description is being shown.

## Registering the appservice

The bridge and the homeserver must share the same registration file (matching `as_token`/`hs_token`):

```bash
# 1. Generate a registration (fills the tokens):
REGISTRATION_PATH=./registration.yaml go run ./cmd/bridge -generate-registration
# 2. Give the SAME file to the homeserver (ESS/Synapse `appservice` config) and to the bridge
#    (as a SOPS-encrypted Secret with key registration.yaml). Never commit real tokens.
```

See [`registration.example.yaml`](registration.example.yaml) and [`agents.example.yaml`](agents.example.yaml). Rooms are unencrypted by design (the crypto package is intentionally not wired). Full architecture: [`../../docs/architecture.md`](../../docs/architecture.md).
