---
name: matrix-agents
description: Runbooks for the Fgentic platform — bootstrap the Matrix + agent stack, register the A2A bridge, add an agent, add an external-network bridge, DNS/TLS, and verify the @mention->A2A->reply flow. Use when operating or extending the platform.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-08
---

# Fgentic Runbook

An open-standard AI-agent collaboration platform: humans + agents share Matrix rooms and `@mention` to delegate over A2A. Layers: Matrix (ESS: Synapse + MAS + Element), the `matrix-a2a-bridge` (Go appservice), agentgateway (governed egress), kagent (agents). CD is Flux v2 pull-based; secrets are SOPS-age. See [docs/architecture.md](../../../docs/architecture.md).

## Golden rules

1. Never `kubectl apply` / `helm upgrade` prod by hand — **commit to git, let Flux reconcile**.
1. Never commit a plaintext secret — only `*.sops.yaml`. A gitleaks pre-commit hook enforces it.
1. The appservice registration (`as_token`/`hs_token`) must be **identical** in the bridge and in Synapse — one SOPS Secret, referenced from both namespaces.
1. Agent rooms are **unencrypted** by design (force-disabled server-side). Do not enable E2EE on agent rooms.

## Runbook: disposable evaluation

`mise run demo:up` creates the separately owned `fgentic-demo` k3d cluster, installs local Flux controllers, reconciles a cluster-local snapshot of the canonical HelmReleases, generates cluster-only credentials, and idempotently seeds Alice plus `#lobby:fgentic.localhost`. Its default model endpoint is a deterministic response fixture, not a language model. It needs no GitHub account, SOPS key, provider account, or checkout mutation, but the pinned images require at least 10 GiB of free disk. `mise run demo:down` verifies the cluster's ownership label and deletes only that demo cluster. Before deletion it atomically records exact Docker identities in a content-free user-state receipt; rerun `demo:down` after interruption. `demo:status` reports pending recovery without mutation, while `demo:up` refuses reuse until the exact cleanup completes.

For repository development, run `mise run dev:up` once, then `dev:reload` or `watch` for bridge-only changes. Reuse does not rebuild the local Git source, reinstall Flux, reconcile every layer, or reseed; `dev:stop` preserves state while releasing active CPU/RAM. These commands use temporary kubeconfigs and never switch the operator's default context. Return to `demo:up` after manifests/profile changes and for the full acceptance proof.

Use [README.md](../../../README.md#evaluate-in-15-minutes) for evaluation choices and [docs/production.md](../../../docs/production.md) for the full GitOps/SOPS path. Never promote evaluation credentials, its local Git source, or the deterministic provider into production.

## Runbook: disposable federation lab

`mise run fed:up` creates or reuses only the separately owned `fgentic-fed` k3d cluster and reconciles three Synapse-only ESS homeservers plus the provider-free cross-organization delegation plane. Alice on `org-a.fgentic.localhost` and Bob on `org-b.fgentic.localhost` are the admitted Matrix participants; Charlie on `org-c.fgentic.localhost` is the denied control. Org B also gets a machine-client Keycloak realm that can invoke org A's exact public `docs-qa` A2A route through agentgateway. kagent stays private, and the selected agent uses the deterministic in-cluster model. The lab needs no MAS, appservice, external identity/model provider, account, or paid service; reserve at least 10 GiB of free disk for its pinned images.

The proof verifies the ES256/JCS AgentCard and OIDC discovery contract; rejects missing, malformed, wrong-audience, and unauthorized tokens; validates the A2A method, extension, and bounded integer `maxTokens`; accepts one org-B request; observes aggregate downstream token growth; and rejects a second maximum reservation with HTTP 429. Reservation is admission accounting per verified `azp`, not measured consumption. The Matrix half still requires a room-v12 A/B exchange under an exact server ACL, a separate explicitly local-only room, rejected join plus signed-federation-send attempts from C, and a final disallowed custom event that B retains while A's callback drops and logs it without content. The command leaves the cluster running for inspection.

The homeservers live in `matrix`, `matrix-b`, and `matrix-c`. They share a CloudNativePG cluster but use separate `synapse`, `synapse_b`, and `synapse_c` roles/databases; the lab adds scoped `keycloak` and `kagent` roles without enabling the production MAS or bridge databases. A and B each admit exactly A and B through `federation_domain_whitelist`, trust the other participant as their key notary, default new rooms to version 12, and load `apps/synapse-federation-policy` from git-declared ConfigMaps. C can route a genuine federation attempt but is excluded from the participant allowlists, room ACL, and callback deployment. Rooms that must remain local are created with immutable `m.federate: false` state.

Run `mise run fed:policy-reload` to exercise policy projection separately. The drill reconciles deny, allow-probe, then deny revisions from the ephemeral local Git source, proves the callback result at every step, and requires both Synapse pod UIDs to remain unchanged. It removes the disposable lab if the relaxed phase fails, so no allow-probe state is left behind. The module also preserves an exact event's first admission when Synapse rechecks an existing staging row; this is a pinned 1.155.0 queue-safety workaround, so validate it before every Synapse upgrade.

Inspect the lab after running `export KUBECONFIG="$(k3d kubeconfig write fgentic-fed)"`; the installer deliberately does not switch the default context. When finished, run `mise run fed:down`. Teardown verifies ownership, atomically records exact identities, and deletes only the federation cluster and its locally built images. If interrupted, rerun the same command; do not broadly prune same-named resources. The canonical topology, recovery boundary, and acceptance contract are in [docs/federation.md](../../../docs/federation.md#85-disposable-federation-hardening-lab).

## Runbook: onboard a federation partner

Follow the bilateral [partner onboarding runbook](../../../docs/federation-onboarding.md) before enabling a real organization. Both operators complete the same owner, technical, contractual, activation, rotation, and offboarding gates; a unilateral test is insufficient.

Use `scripts/fed-check.sh partner.example` only to observe public Matrix discovery, then repeat with `--expect-server <agreed-host:port>` to fail closed on a delegation change. The script is credential-free, bounded, and read-only. Its output is public-probe evidence, not proof of mutual allowlists, room policy, A2A authorization, or contractual governance. Store onboarding evidence privately and never include tokens, private keys, room content, or personal contact details in Git.

## Runbook: one-time bootstrap

1. **(Optional) Provision a cluster** — `cd infra/terraform && cp terraform.tfvars.example terraform.tfvars` (set your `/32`), then `terraform init && terraform apply`. Or use any conformant cluster / local k3d (`mise run cluster:up`).
1. **Gateway API CRDs** (the one out-of-band install): `kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml`.
1. **Choose the model profile and create secrets** — follow [docs/models.md](../../../docs/models.md), edit the environment's `llm_provider`/`llm_model`, export the selected API profile's key if applicable, then run `scripts/gen-secrets.sh <server_name> <local|gcp>`. It writes the consistent SOPS set (Postgres roles, Keycloak bootstrap/demo/client credentials, appservice/A2A/MCP authorization, connection URLs, and the selected provider Secret) into `clusters/<env>/secrets/`; commit + push (Flux applies from git).
1. **SOPS-age key**: `kubectl -n flux-system create secret generic sops-age --from-file=age.agekey="$HOME/.config/sops/age/keys.txt"` (create the namespace first if bootstrapping later).
1. **Local TLS (k3d only)**: `scripts/local-ca.sh` — generates + loads the `local-ca` CA secret (ESS bakes https URLs, so even local runs terminate real TLS at the Gateway on loopback 80/443).
1. **Local Vertex auth (Vertex profile only)**: `scripts/local-adc.sh` creates the cluster-only `gcp-adc` Secret. API profiles use the SOPS Secret generated in the previous step; the self-hosted profile uses no model credential.
1. **Bootstrap Flux**: `GITHUB_TOKEN=$(gh auth token) flux bootstrap github --owner=fmind-ai --repository=fgentic --path=clusters/<env>` — commits the flux-system manifests and starts reconciling.
1. **DNS A records (gcp)** — point `fgentic.fmind.ai`, `chat.`, `matrix.`, `auth.`, `id.`, and `grafana.` at the ingress IP (`terraform output -raw ingress_ip`); cert-manager then issues the multi-SAN Let's Encrypt cert on `fgentic-gateway`.
1. Flux reconciles in order: namespaces + secrets → controllers (cert-manager, Traefik, CNPG, agentgateway, kagent) + observability → gateway + postgres + agentgateway → matrix + the reference Keycloak IdP + kagent → the bridge. MAS reads the upstream provider from the bootstrap-only SOPS Secret; see [docs/identity.md](../../../docs/identity.md) for an external-IdP replacement.

## Runbook: rotate secrets

`scripts/rotate-secrets.sh <server_name> <local|gcp> <secret-set>` rewrites reviewed SOPS ciphertext only. It never reconciles Flux, restarts a workload, prints a secret, or overwrites a dirty encrypted file. It stages and decrypt-validates every output before replacing the first tracked file. Run one set at a time unless this is a planned full drill.

| Secret set               | Rotated material                                                | Blast radius and reload                                                                                         |
| ------------------------ | --------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------- |
| `appservice`             | `as_token` and `hs_token`, identical in `matrix` and `bridge`   | Matrix→bridge delivery pauses while Synapse and the bridge hold different copies; restart Synapse, then bridge. |
| `a2a`                    | Bridge workload API key, identical at agentgateway and bridge   | Agent delegation fails after the policy adopts the new key until the bridge restarts; human Matrix chat stays.  |
| `mcp`                    | platform-helper MCP API key, identical at gateway and kagent    | Tool calls fail after the policy adopts the new key until the kagent controller regenerates platform-helper.    |
| `db-synapse`             | Synapse role and both namespace copies                          | Homeserver database reconnects; wait for CNPG, then restart Synapse.                                            |
| `db-mas`                 | MAS role and both namespace copies                              | New login/token operations pause during the MAS restart; existing Matrix sessions remain.                       |
| `db-bridge`              | Bridge role and derived `DATABASE_URL`                          | Agent delegation pauses during the bridge restart; persistent context and dedup state remain in Postgres.       |
| `db-kagent`              | kagent role and derived URL                                     | A2A agent execution pauses during the controller restart.                                                       |
| `db-core`                | All four core roles and derived URLs                            | Combined blast radius of the four rows above.                                                                   |
| `db-knowledge-owner`     | Knowledge schema-owner role in `postgres` only                  | No workload restart; wait for its `DatabaseRole`, then prove the new owner login cannot reach another database. |
| `db-knowledge-retrieval` | Retrieval role and identical `postgres`/`knowledge` copies      | Wait for its `DatabaseRole`; restart the retrieval consumer from #333 once that consumer exists.                |
| `knowledge-db`           | Both knowledge roles and the retrieval namespace copy           | Combined blast radius of the two knowledge rows; the owner credential remains absent from `knowledge`.          |
| `provider`               | Selected Mistral/Anthropic/OpenAI/Azure OpenAI API key          | Model calls only; agentgateway consumes the Secret dynamically. Vertex and vLLM have no tracked provider key.   |
| `keycloak-db`            | Keycloak role and both namespace copies                         | New SSO redirects pause during the Keycloak restart; existing Matrix sessions remain.                           |
| `keycloak-client`        | The live `fgentic` OIDC client secret mirrored for MAS/recovery | SSO is unavailable between live Keycloak rotation and MAS reload; explicit acknowledgement is mandatory.        |
| `slack`                  | Slack bridge DB password and matching AS/HS tokens              | Wait for `slackbridge`, then restart Synapse and only `mautrix-slack`; provider app-login state remains in DB.  |
| `telegram`               | Telegram bridge DB password and matching AS/HS tokens           | Wait for `telegrambridge`, then restart Synapse and only `mautrix-telegram`; API ID/hash and sender stay fixed. |
| `all`                    | Automatable core and knowledge rows                             | Excludes optional networks, Keycloak client, bootstrap admin, and demo users; rotate those explicitly.          |

### Prepare, generate, and reconcile

1. Confirm `git status --short -- clusters/<env>/secrets` is empty, Flux is healthy, and the current login/mention flow works. Do not begin another rotation until the previous ciphertext diff is committed or discarded deliberately.
1. For `provider`, create a second provider key without revoking the old key, then export the new raw value in the profile-specific variable shown by `scripts/rotate-secrets.sh --help`. For `all`, do the same when the selected profile is API-backed.
1. Run the selective rotation. Examples:

   ```bash
   mise exec -- scripts/rotate-secrets.sh fgentic.localhost local appservice
   OPENAI_API_KEY='<new-key>' mise exec -- scripts/rotate-secrets.sh fgentic.localhost local provider
   mise exec -- scripts/rotate-secrets.sh fgentic.localhost local all
   ```

1. Review only the expected encrypted files, run `mise run check:secret-rotation`, commit, and push. Never paste decrypted output into a review or shell log.
1. Reconcile the committed ciphertext and wait for the Secret layer before restarting anything:

   ```bash
   flux reconcile source git flux-system
   flux reconcile kustomization platform-secrets --with-source
   flux get kustomization platform-secrets
   ```

### Database-role ordering

CloudNativePG reports the exact `postgres`-namespace Secret resource version applied to each role. Wait for equality before restarting a consumer; `Cluster Ready=True` alone does not prove that the new password reached PostgreSQL. The five legacy roles report through the `Cluster`; the two knowledge roles report directly on their `DatabaseRole` resources.

```bash
wait_role() {
  role="$1"
  secret_rv="$(kubectl -n postgres get secret "pg-${role}" -o jsonpath='{.metadata.resourceVersion}')"
  until [ "$(kubectl -n postgres get cluster platform-pg -o jsonpath="{.status.managedRolesStatus.passwordStatus.${role}.resourceVersion}")" = "${secret_rv}" ]; do
    sleep 2
  done
}

wait_role synapse # use mas, bridge, kagent, or keycloak for the other sets

wait_database_role() {
  resource="$1"
  secret="$2"
  secret_rv="$(kubectl -n postgres get secret "${secret}" -o jsonpath='{.metadata.resourceVersion}')"
  until [ "$(kubectl -n postgres get databaserole "${resource}" -o jsonpath='{.status.secretResourceVersion}')" = "${secret_rv}" ] &&
    [ "$(kubectl -n postgres get databaserole "${resource}" -o jsonpath='{.status.applied}')" = true ]; do
    sleep 2
  done
}

wait_database_role knowledge-owner pg-knowledge-owner
wait_database_role knowledge-retrieval pg-knowledge-retrieval
```

After the relevant wait succeeds, restart only its consumer:

```bash
# db-synapse
kubectl -n matrix rollout restart statefulset/ess-synapse-main
kubectl -n matrix rollout status statefulset/ess-synapse-main --timeout=5m

# db-mas
kubectl -n matrix rollout restart deployment/ess-matrix-authentication-service
kubectl -n matrix rollout status deployment/ess-matrix-authentication-service --timeout=5m

# db-bridge
kubectl -n bridge rollout restart deployment/matrix-a2a-bridge
kubectl -n bridge rollout status deployment/matrix-a2a-bridge --timeout=2m

# db-kagent
kubectl -n kagent rollout restart deployment/kagent-controller
kubectl -n kagent rollout status deployment/kagent-controller --timeout=2m

# keycloak-db
kubectl -n keycloak rollout restart statefulset/keycloak
kubectl -n keycloak rollout status statefulset/keycloak --timeout=5m
```

For `db-core`, wait for all four core roles before restarting Synapse, MAS, kagent, and finally the bridge. Restarting the bridge last avoids loading its new database password before both its dependency and the role are ready.

For a knowledge-role rotation, use `wait_database_role` for only the affected resource and prove the new login before retiring the old credential. The schema owner has no workload to restart and its Secret must remain absent from `knowledge`. The retrieval consumer is introduced by #333; once deployed, restart only that consumer after `knowledge-retrieval` reports the new Secret resource version.

### Appservice, A2A, MCP, and provider ordering

For `appservice`, both Kubernetes Secrets reconcile together but both pods load the registration only at startup. Reload the homeserver first, then the bridge immediately:

```bash
kubectl -n matrix rollout restart statefulset/ess-synapse-main
kubectl -n matrix rollout status statefulset/ess-synapse-main --timeout=5m
kubectl -n bridge rollout restart deployment/matrix-a2a-bridge
kubectl -n bridge rollout status deployment/matrix-a2a-bridge --timeout=2m
```

For `slack` or `telegram`, first wait for the matching CNPG managed role to report the new Secret resource version. Restart Synapse to load the new registration, then restart only the selected external bridge:

```bash
NETWORK=slack # or: telegram
kubectl -n matrix rollout restart statefulset/ess-synapse-main
kubectl -n matrix rollout status statefulset/ess-synapse-main --timeout=5m
kubectl -n bridges rollout restart "statefulset/mautrix-${NETWORK}"
kubectl -n bridges rollout status "statefulset/mautrix-${NETWORK}" --timeout=5m
```

Re-run that provider's allowed and denied message path before revoking or deleting any old external session.

For `a2a`, wait until the policy is accepted with the new Secret, then restart the bridge so its environment reads the matching key:

```bash
kubectl -n agentgateway-system wait agentgatewaypolicy/a2a-bridge-authorization \
  --for=jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}'=True \
  --timeout=2m
kubectl -n bridge rollout restart deployment/matrix-a2a-bridge
kubectl -n bridge rollout status deployment/matrix-a2a-bridge --timeout=2m
```

For `mcp`, wait until the authenticated route policy reports accepted, then restart the controller. kagent 0.9.11 resolves a Tool `headersFrom` Secret while generating the managed Agent configuration but does not watch that Secret reference directly; restarting the controller causes a fresh Agent reconciliation and config-hash rollout.

```bash
kubectl -n agentgateway-system wait agentgatewaypolicy/platform-helper-mcp-authorization \
  --for=jsonpath='{.status.ancestors[0].conditions[?(@.type=="Accepted")].status}'=True \
  --timeout=2m
kubectl -n kagent rollout restart deployment/kagent-controller
kubectl -n kagent rollout status deployment/kagent-controller --timeout=2m
kubectl -n kagent rollout status deployment/platform-helper --timeout=2m
```

For `provider`, wait for `agentgatewaybackend/llm-<llm_provider>` to remain `Accepted`, make a real model request through agentgateway, and only then revoke the old provider key. A proxy restart is not normally required because no workload receives the provider credential.

### Keycloak client and full-drill ordering

`keycloak-client` is deliberately two-phase because startup realm import never updates an existing client. Regenerate the `fgentic` client secret in the live Keycloak Admin Console first. Copy it without shell history, acknowledge that live mutation, and update only the two encrypted recovery/MAS copies:

```bash
read -rsp 'New live Keycloak client secret: ' FGENTIC_CLIENT_SECRET
export FGENTIC_CLIENT_SECRET
KEYCLOAK_CLIENT_UPDATED=yes \
  scripts/rotate-secrets.sh <server_name> <local|gcp> keycloak-client
```

After commit/reconciliation, restart `deployment/ess-matrix-authentication-service`, complete a fresh SSO login, then `unset FGENTIC_CLIENT_SECRET`. The script proves the MAS and Keycloak recovery copies match and that the bootstrap admin/Alice/Bob fields did not change.

For `all`, reconcile once, wait for the five legacy CNPG roles and both knowledge `DatabaseRole` resources, restart Keycloak, Synapse, MAS, and kagent, wait for the A2A/MCP policies and provider backend, then restart the bridge last. Complete a fresh SSO login, one platform-helper tool call, its MCP audit record, a knowledge-role login check, and an `@mention` round trip before revoking the old provider key.

### Rehearsal and downtime record

Start the timer at the first failed bridge readiness/mention probe and stop it at the first successful post-rotation `@mention` reply. Record the secret set, start/end timestamps, observed seconds, and failed step in the issue or PR; never record credential material. The 2026-07-11 offline fixture exercised the appservice transition (`old/old → mixed rejected → new/new`) with real SOPS ciphertext in about 0.5 seconds. That is tooling time, not live service downtime. The `<1 minute` acceptance target still requires a reconciled disposable/local cluster drill.

## Runbook: add an agent

1. **Scaffold the composition seam** — run `mise run agent:new <name>`. The name must be one lowercase Kubernetes DNS label. The task adds a declarative kagent `Agent` under `infra/kagent/agents/<name>/`, a matching bridge Kustomize Component under `apps/matrix-a2a-bridge/deploy/agents/<name>/`, and one deterministic `evals/<name>/golden.json` scenario. It also composes both GitOps units into their parent Kustomizations. The emitted `agent-<name>` ghost resolves to `/api/a2a/kagent/<name>`, starts at `stage: dev`, admits only `@alice:<server_name>`, uses `agentgateway-model`, and holds no model credential.
1. **Review the narrow contract** — replace the generic description, system message, skill, and golden task with the intended behavior while retaining the shared `agent-zoo-runtime` ServiceAccount, prompt-injection boundary, three content-free tracing switches, explicit full-MXID sender allowlist, and `stage: dev`. Add only reviewed tools through the governed MCP catalog; never place a model credential in an Agent. Run `mise run check:agent-new` and `mise run check:agent-zoo` before committing. These checks validate the seeded fixture's shape but do not execute its answer yet; [#370](https://github.com/fmind-ai/fgentic/issues/370) owns the shared in-repo golden CI gate.
1. **Promote deliberately** — once #370's golden CI gate and staging-room acceptance pass, change only the generated mapping's `stage: dev` to `stage: prod` in a reviewed PR. kagent serves the Agent over A2A at `…/api/a2a/kagent/<name>` with an AgentCard; the bridge ghost is `@agent-<name>:<server_name>`.

The shared single-agent pre-PR loop and longer authoring guide are tracked by [#372](https://github.com/fmind-ai/fgentic/issues/372); do not invent a separate runner meanwhile.

1. **Authorize optional network replies** — if Slack or Telegram interop is enabled, add the exact `@agent-<name>:<server_name>` to that network unit's mautrix `bridge.permissions` map at `relay` level. The provider bridge blocks undeclared Matrix identities, so omitting this step safely prevents that agent's reply from leaving Matrix.
1. **Use it** — invite `@agent-<name>` into a room, run `!agents` to confirm its sender-filtered gallery entry, use `!agents <name>` to inspect the cached description and declared skills, then `@mention` it. A remote mapping shown as unavailable is quarantined; do not treat cached card claims as current until trust is restored.
1. **(Optional) In-flight task pins** — to turn on `PIN_IN_FLIGHT_TASKS`, grant agent ghosts the room's `m.room.pinned_events` state-event power level (raise the ghost's user level or lower the pinned-events event default in the room's power levels); without it the bridge skips pinning silently. Threaded working-state progress (`MAX_TASK_PROGRESS_POSTS`) needs no extra power.

## Runbook: seed an onboarding room

1. Create a new private, invite-only, unencrypted room for one declared purpose. Use room version 12 or newer and the reviewed server ACL/classification posture for any federated room; do not derive a room ID, alias, or participant from a localpart.
1. Invite the exact bot MXID `@a2a-bridge:<server_name>`. With `WELCOME_ENABLED=true` (the default), the bot accepts and posts one automated `m.notice` containing only the agents that the inviter's full MXID may invoke, plus `@mention`, client-safe `!ask`/`!agents`/`!budget`, and raw-client `/ask`/`/agents`/`/budget` guidance.
1. Verify the welcome appears once, is plaintext and visibly automated, and excludes a sender-denied mapping. A leave/re-invite must not post another welcome; the bridge keeps a permanent content-free room marker. Notice-plane exhaustion suppresses the single attempt without a rate-limit reply.
1. Give the room a human-managed alias only if the deployment needs one. Keep room creation in the operator workflow rather than hardcoding IDs in bridge configuration; the bridge reacts to the invitation and does not own room lifecycle.

## Runbook: add an external-network bridge (interop)

1. Read the [external-network interop contract](../../../docs/interop.md) and the provider-specific gate. Slack also has a [live workspace walkthrough](../../../docs/interop-slack.md). Do not infer provider compatibility from a successful render.
1. Add `../../infra/bridges/slack/cluster` or `../../infra/bridges/telegram/cluster` to the target cluster's `components` list. Each self-contained network directory appends its role/database/HBA pair, ESS registration, and Matrix-to-A2A origin without replacing canonical paths; multiple approved networks compose.
1. Generate only the selected SOPS set: `FGENTIC_SECRET_SET=slack mise exec -- scripts/gen-secrets.sh <server_name> <env>`, or supply `TELEGRAM_API_ID`/`TELEGRAM_API_HASH` and select `telegram`. Provider login state is entered later in a private bridge-management room and never copied to Git.
1. Run the focused `mise run check:mautrix-bridges` contract while iterating. Let installed commit/push hooks serialize the complete gates, or run `mise run agent:gate` once near PR readiness in a hookless environment, then deliver the reviewed revision through Flux. Wait for `platform-secrets`, `postgres`, `matrix`, and `mautrix-<network>` in that order; never apply the optional workload by hand.
1. Bridge only the approved channel/chat, then have Alice explicitly set her approved login as the portal relay with `!slack set-relay [login ID]` or `!tg set-relay [login ID]`. There are no default relays and no relay-assisted bridge creation. Without this step, appservice `m.notice` replies cannot return to the provider.
1. Configure the exact workspace-scoped Slack MXID (for example `@slack_t0123456789-u0123456789:<server_name>`) or exact `@telegram_<immutable-id>:<server_name>` only in the intended agent's `allowedSenders`, using an outer JSON patch that appends `/spec/patches/-` on the target cluster's `bridge` Flux Kustomization. Place it after any existing outer patch for `bridge`. Never put a provider MXID in the canonical app HelmRelease: that grant would reach clusters where the provider origin is absent and be treated as ordinary Matrix. The network component injects the broader `bridgedOrigins` namespace for classification; it grants nothing.
1. Prove both paths: the exact allowlisted remote identity receives an agent reply, while another bridge-owned MXID gets a policy `m.notice`, no A2A attempt, and an audit with `sender_origin_kind=bridge` plus the expected network. Record observed thread/edit/delete/file/retention semantics and complete provider offboarding.
1. Offboard in two GitOps phases: first replace `../../infra/bridges/<network>/cluster` with `../../infra/bridges/<network>/cluster-offboard`, retain the ciphertext, and verify CNPG sets `NOLOGIN`, clears the password, and removes the network's HBA pair. Query only boolean results; never print `rolpassword`. Only then remove the offboard component and ciphertext. Workload or Secret deletion alone does not revoke a retained database login.

## Runbook: verify the flow

1. `kubectl -n bridge logs deploy/matrix-a2a-bridge` shows "matrix-a2a-bridge started" and the loaded agent map; the pod is Ready (probes hit mautrix's `/_matrix/mau/ready`).
1. **A2A through the gateway**: `kubectl -n agentgateway-system port-forward svc/agentgateway-proxy 8080:8080`, then fetch `http://localhost:8080/api/a2a/kagent/platform-assistant/.well-known/agent-card.json` and POST a JSON-RPC `SendMessage` to that configured route. The bridge binds the card's selected v1 JSON-RPC interface back to this operator-configured base because the pinned gateway rewrites only the pre-v1 top-level URL; a raw completion goes to `POST /v1/chat/completions` with model `google/gemini-2.5-flash`.
1. **Provision the demo administrator and room through supported APIs**: run `scripts/bootstrap-admin.sh --server-name <server_name>`, open the one-time URL, and authenticate as the IdP user whose immutable `matrix_localpart` is `alice`. MAS provisions the user on first SSO login; the script verifies the exact MXID, grants Synapse admin, and idempotently creates `#fgentic-demo:<server_name>`. It stores no token and never enters a pod. Password login remains enabled only in the local profile; production disables it with `mas_local_login_enabled: "false"`.
1. **The core flow**: in Element (`https://chat.<server_name>`) or via the client API — create a room, **invite** `@agent-assistant:<server_name>` (the bridge auto-accepts invites for mapped ghosts; Synapse only delivers room traffic once a bridge user is a member), run `!agents` and `!agents assistant` to verify the sender-filtered cached gallery/details, then post `@agent-assistant <task>` with the mention. The ghost replies in-thread (`m.notice`); a follow-up mention continues the same kagent session (contextId threading).
1. **Metrics**: Grafana at `https://grafana.<server_name>` (admin password: `kubectl -n monitoring get secret kube-prometheus-stack-grafana -o jsonpath='{.data.admin-password}' | base64 -d`); Prometheus has `fgentic_delegations_total` (bridge) and `agentgateway_gen_ai_client_token_usage_sum` (token metering; the `LLMTokenBurnHigh` alert watches its burn rate).
1. **Attribution audit**: retain the Matrix event ID, then run `mise exec -- scripts/audit-attribution.sh '$event-id' 15m > audit-evidence.json` and apply the deterministic join checks in [docs/audit.md](../../../docs/audit.md). The bundle is content-free but contains linkable identifiers; never commit it. Gateway logs and Prometheus are corroborating aggregates, not user identity evidence.

## Runbook: cost / scale levers

Unlike the sibling `dev.fmind` (a $30/mo free-tier cluster), this platform is sized for the full stack. To trim: reduce kagent agents, run Synapse/MAS at one replica, or `terraform destroy` the reference cluster. To scale up: raise `node_count`/`machine_type`, set CNPG `instances: 3` for HA, and add read replicas.
