# matrix-a2a-bridge

A Matrix ↔ A2A bridge: a **Matrix Application Service** that turns an `@mention` into an **A2A** task delegation and posts the agent's reply back into the room. It is the piece of custom glue in the [`Fgentic`](../../README.md) platform — everything else is off-the-shelf open source.

```text
Human in a Matrix room:  "@agent-k8s why is pod payments-7c9 crashing?"
   │  Synapse pushes the event to the bridge (it owns the @agent-* ghost namespace)
   ▼
   bridge:  detect @mention → map @agent-k8s → kagent/k8s-agent
            A2A message/send (non-streaming) to <A2A_BASE_URL>/api/a2a/kagent/k8s-agent
            (routed through agentgateway → kagent; the agent's LLM egress also goes through agentgateway)
   ◀  reply text
   ▼  post as @agent-k8s, as a reply to the original message
Human sees the answer in Element.
```

## How it works

1. The bridge registers an **exclusive appservice namespace** `@agent-.*:fgentic.fmind.ai` plus the `@a2a-bridge` bot. Every kagent agent thus appears as a first-class room member.
1. On each `m.room.message`, it reads the typed `m.mentions` field (MSC3952) — with a plaintext-body fallback — to find which agent(s) were addressed.
1. It maps the ghost (`agent-k8s`) to a kagent agent `(namespace, name)` via `agents.yaml` — the **authorization allowlist**: only mapped ghosts are invokable, only allowed senders/homeservers may invoke them (federated look-alikes are rejected), and per-sender/per-room token buckets guard LLM spend.
1. It enqueues the delegation (per-room FIFO, bounded global concurrency — the appservice transaction push is never blocked) and calls the agent over A2A `message/send`, threading a per-`(room, agent)` `contextId` for multi-turn conversations. Long-running tasks are polled via `tasks/get`, with the placeholder reply edited (`m.replace`) into the final answer.
1. It extracts the reply text from the returned `Task | Message` and posts it back **as the ghost user** (`m.notice`, so other bots ignore it), as a reply to the original message.
1. State (mautrix StateStore, contexts, processed-event dedup) persists in Postgres (`DATABASE_URL`); without it the bridge runs dev-only in-memory state.

## Build

This repo ships source only; resolve dependencies first:

```bash
mise install          # == go mod tidy   (creates go.sum)
mise run test         # unit tests (config parsing + A2A reply extraction + agent map)
mise run build        # bin/matrix-a2a-bridge
mise run build:image  # distroless image
```

## Configuration (environment)

| Variable                                       | Default                                                                | Purpose                                                                                             |
| ---------------------------------------------- | ---------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------- |
| `HOMESERVER_URL`                               | `http://synapse.matrix.svc.cluster.local:8008`                         | Matrix Client-Server API (Synapse).                                                                 |
| `SERVER_NAME`                                  | `fgentic.fmind.ai`                                                     | Matrix server_name (the `:domain` of every user ID).                                                |
| `A2A_BASE_URL`                                 | `http://agentgateway-proxy.agentgateway-system.svc.cluster.local:8080` | A2A base; agents at `/api/a2a/<ns>/<name>`. Point at kagent (`…kagent…:8083`) to skip agentgateway. |
| `GHOST_PREFIX`                                 | `agent-`                                                               | Local-part prefix for agent ghosts.                                                                 |
| `REGISTRATION_PATH`                            | `/etc/matrix-a2a-bridge/registration.yaml`                             | Appservice registration (as_token/hs_token).                                                        |
| `AGENTS_PATH`                                  | `/etc/matrix-a2a-bridge/agents.yaml`                                   | Ghost → agent routing/allowlist map.                                                                |
| `DATABASE_URL`                                 | _(empty)_                                                              | Postgres URL for persistent state (empty = in-memory, dev only).                                    |
| `REQUEST_TIMEOUT`                              | `60s`                                                                  | Transport deadline on a single A2A message/send.                                                    |
| `TASK_TIMEOUT`                                 | `10m`                                                                  | Overall deadline when polling a long-running task (tasks/get).                                      |
| `CONCURRENCY`                                  | `16`                                                                   | Max in-flight delegations across all rooms (per-room FIFO regardless).                              |
| `SENDER_RATE_PER_MINUTE` / `SENDER_RATE_BURST` | `6` / `3`                                                              | Token bucket per (sender, agent).                                                                   |
| `ROOM_RATE_PER_MINUTE` / `ROOM_RATE_BURST`     | `30` / `10`                                                            | Token bucket per room.                                                                              |
| `LISTEN_HOST` / `LISTEN_PORT`                  | `0.0.0.0` / `29331`                                                    | Appservice HTTP bind.                                                                               |
| `LOG_LEVEL` / `LOG_FORMAT`                     | `info` / `json`                                                        | Structured logging.                                                                                 |

## Registering the appservice

The bridge and the homeserver must share the same registration file (matching `as_token`/`hs_token`):

```bash
# 1. Generate a registration (fills the tokens):
REGISTRATION_PATH=./registration.yaml go run ./cmd/bridge -generate-registration
# 2. Give the SAME file to the homeserver (ESS/Synapse `appservice` config) and to the bridge
#    (as a SOPS-encrypted Secret with key registration.yaml). Never commit real tokens.
```

See [`registration.example.yaml`](registration.example.yaml) and [`agents.example.yaml`](agents.example.yaml). Rooms are unencrypted by design (the crypto package is intentionally not wired). Full architecture: [`../../PLAN.md`](../../PLAN.md).
