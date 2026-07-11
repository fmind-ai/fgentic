# External-network interop

Fgentic treats every external-network bridge as an optional identity boundary, not as a feature of the core Matrix-to-A2A bridge. The repository ships reusable deployment machinery plus opt-in Slack and Telegram reference units. Neither unit is enabled by the local or GCP profile, and a rendered manifest is not evidence that a provider account, tenant policy, or live message path has been accepted.

## Deployment-unit contract

Every network unit composes the generic `infra/bridges/chart/` chart and owns all of the following:

1. An immutable upstream image digest and direct bridge binary invocation with config rewriting disabled.
1. One appservice ID, bot MXID, and exclusive remote-user namespace that do not overlap another bridge or `@agent-*`.
1. One CloudNativePG login role and database. Sharing the Postgres cluster is supported; sharing a database is not.
1. One SOPS secret set containing the role password, database URI, appservice tokens, and the identical Synapse registration copy. Provider credentials belong in that same network-scoped set only when the bridge requires them at startup.
1. A single-replica StatefulSet, dedicated ServiceAccount, read-only root filesystem, non-root UID/GID, dropped capabilities, seccomp, probes, resource limits, and a read-only config mount.
1. Default-deny ingress and egress with pod-selected Synapse ingress, pod-selected ESS HAProxy egress, scoped Postgres, and DNS. Standard Kubernetes NetworkPolicy cannot select provider FQDNs, so the reference also admits arbitrary non-private IPv4 TCP/443—not only Slack or Telegram endpoints. Treat that as residual egress risk; a governed proxy or FQDN-aware CNI is required for an endpoint allowlist claim.
1. One self-contained network profile under `infra/bridges/<network>/`: runtime, cluster selector, Matrix registration, Postgres role/database/HBA pair, Matrix-to-A2A origin, and `NOLOGIN` offboard components. Selecting its `cluster/` component extends the canonical Flux layers without a second network-specific source directory.
1. A `bridgedOrigins` namespace declaration in the Matrix-to-A2A bridge. Recognition never grants authority: every bridged sender is denied until its full Matrix ID is present in an agent's `allowedSenders` list.

The bridge runtime stores remote login sessions in its database. Do not put browser tokens, QR payloads, cookies, phone numbers, or chat history in Git, Helm values, Kubernetes annotations, logs, screenshots, or issue comments.

## Shipped references

| Network  | Manifest status         | Upstream pin                                                                                                                | Login boundary                                                                                                        | Operator decision                                                                                                                                    |
| -------- | ----------------------- | --------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------- |
| Slack    | Opt-in reference unit   | `mautrix/slack` `v0.2606.0`, multi-arch digest `sha256:f1de44e723a13484a6b09a26b93127e494c25a70d4d21c2300bfddf49a7dae03`    | Dedicated internal Slack app in Socket Mode; `xoxb-` and `xapp-` tokens are entered at runtime and stored in Postgres | Workspace owner approves scopes, retention boundary, app policy, and live test. Public/commercial distribution needs a separate Slack policy review. |
| Telegram | Opt-in second reference | `mautrix/telegram` `v0.2606.0`, multi-arch digest `sha256:8c6c559446f049c1f3c4cbc4b284aed14c27aefde9b88a785d262633bdafe510` | API ID/hash at deployment; an established account logs in at runtime by QR or six-digit code                          | Account owner accepts third-party-client and anti-abuse risk plus Telegram's restrictions on using platform data for AI development.                 |

The pins above are deployment artifacts under the upstream bridges' AGPL-3.0 license; they are never linked into Fgentic's Apache-2.0 Go binary. Review [licensing.md](licensing.md) before redistributing an image or modified bridge.

## Template for another mautrix network

The reusable deployment boundary is **one network profile directory plus one encrypted multi-document secret file**. That is the mechanical infrastructure contract, not a claim that a new provider is accepted without source and policy review.

1. Copy one shipped `infra/bridges/<network>/` profile. Keep all network-specific declarations inside the new directory:
   - root runtime `HelmRelease` and `Kustomization`;
   - `cluster/` selector and `cluster-offboard/` teardown selector;
   - `matrix/` appservice registration component;
   - `postgres/` role, database, and exact HBA pair plus `postgres-offboard/` `NOLOGIN` declaration;
   - `a2a/` anchored `bridgedOrigins` patch.
1. Replace the immutable image digest, direct binary, port, appservice ID/bot/ghost prefix, runtime config schema, provider egress requirement, role/database name, and all registration namespace/token references. Namespace overlap, broad origin globs, writable config, default portal creation, or a database shared with another program are blockers.
1. Add one SOPS-encrypted multi-document file under `clusters/<env>/secrets/` containing the scoped Postgres credential, runtime Secret, and identical Matrix registration copy. Add a matching shape-only example under `infra/secrets/`. Provider startup credentials may join that same file; interactive login sessions stay in the scoped database.
1. Add the network to the consolidated `scripts/test-mautrix-bridges.sh` provenance matrix so the exact release/commit/image, generated registration, secret keys, privacy defaults, HBA rule, dual-component composition, and no-provider startup are executable contracts. Generator/rotator cases are optional convenience only when automatic secret generation is safe; otherwise create and encrypt the one file through the normal SOPS workflow.
1. Update licensing, threat-model, provider-terms, consent, live acceptance, and removal documentation. Those review artifacts are deliberately not generated from deployment values. Until they and the provider-owner live gate pass, describe the directory as a candidate unit—not a supported network.

Thus a third deployment unit does not require hand-editing canonical Matrix, Postgres, or Matrix-to-A2A manifests. It does require an explicit test-matrix entry and evidence review; “one directory + one secret” describes wiring, not automatic provider authorization.

## Candidate networks, deliberately not shipped

| Network  | Current upstream shape                                                                                    | Why it is not a reference unit                                                                                                                                                                                    |
| -------- | --------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Signal   | `mautrix/signal` `v0.2606.0`; a secondary device linked from a Signal mobile account                      | The bridge can technically use the generic unit, but Signal forbids unauthorized access and bulk/automated messaging. Ship only after an operator-specific legal and account-risk decision plus integration test. |
| WhatsApp | `mautrix/whatsapp` `v0.2606.0`; a linked Web client whose primary phone must remain active                | WhatsApp restricts non-personal, automated, reverse-engineered, and unauthorized access. The upstream bridge also documents account-ban and linked-device expiry risks. Deferred from the reference platform.     |
| Teams    | Supported bot/RSC/Graph surfaces exist, but no reviewed production Matrix bridge meets the enterprise bar | [ADR 0011](adr/0011-teams-coexistence-not-bridge.md) selects coexistence and a possible Teams-native A2A adapter after customer validation; it rejects web-token puppeting and bridge claims.                     |

Signal and WhatsApp image compatibility is not authorization to deploy them. Provider terms and customer consent are external controls that a Kubernetes chart cannot satisfy.

## Opt in to a reference unit

The examples below change GitOps source and generate encrypted files; review the diff before committing. Flux remains the only production deployer.

1. Add exactly one component to the target cluster's `components` list:

   ```yaml
   components:
     - ../base/provider-selection
     - ../../infra/bridges/slack/cluster
   ```

   Use `../../infra/bridges/telegram/cluster` for Telegram. The components compose, so both may be listed when the cluster is sized for two additional bridge pods and both provider gates are approved.

1. Generate only that network's coherent SOPS set:

   ```bash
   FGENTIC_SECRET_SET=slack mise exec -- scripts/gen-secrets.sh <server_name> <local|gcp>
   ```

   Telegram additionally requires the values created at `my.telegram.org/apps`:

   ```bash
   read -rp 'Telegram API ID: ' TELEGRAM_API_ID
   read -rsp 'Telegram API hash: ' TELEGRAM_API_HASH
   export TELEGRAM_API_ID TELEGRAM_API_HASH
   FGENTIC_SECRET_SET=telegram mise exec -- scripts/gen-secrets.sh <server_name> <local|gcp>
   unset TELEGRAM_API_ID TELEGRAM_API_HASH
   ```

1. Run the offline gates before reconciliation:

   ```bash
   mise run format
   mise run check
   mise run test
   ```

1. Commit and push the reviewed component and ciphertext through the normal GitHub/Flux workflow. Then wait for `postgres`, `matrix`, and the selected `mautrix-<network>` Flux Kustomization in that order. Do not bypass a failed layer with `kubectl apply` or `helm upgrade`.
1. Complete the network-specific runtime login in a private management room, then bridge only the approved channel/chat. Runtime login material must never be copied back into a manifest.
1. In that portal, have Alice explicitly set her approved provider login as its relay (`!slack set-relay [login ID]` or `!tg set-relay [login ID]`). There are no default relays and relay-assisted bridge creation is disabled. Only Alice may manage the relay; only the explicitly configured `@a2a-bridge` and `@agent-*` appservice identities may send through it. Adding an agent therefore requires adding its exact MXID to the selected network unit's permission map.
1. Invite the selected `@agent-*:<server_name>` ghost into the portal and confirm it joined before testing a remote mention. Synapse delivers a room transaction to the Matrix-to-A2A appservice only when one of that appservice's users participates; portal creation alone does not establish the A2A path.
1. Add the exact resulting ghost MXID to only the agents that remote identity may invoke. The selected network component already injects its reviewed origin namespace; do not duplicate or widen that patch, and never add a provider MXID to canonical `apps/matrix-a2a-bridge/deploy/helmrelease.yaml` values. Canonical grants reach clusters where the provider component is absent, so the same local MXID would no longer be classified as bridge-owned. Append the grant through the target cluster's `bridge.spec.patches` list with an outer JSON patch placed after any existing patch that targets `bridge`:

   ```yaml
   # Add under clusters/<env>/kustomization.yaml top-level patches:
   - target:
       group: kustomize.toolkit.fluxcd.io
       version: v1
       kind: Kustomization
       name: bridge
       namespace: flux-system
     patch: |
       - op: add
         path: /spec/patches/-
         value:
           target:
             kind: HelmRelease
             name: matrix-a2a-bridge
           patch: |-
             - op: add
               path: /spec/values/agents/agent-docs-qa/allowedSenders/-
               value: "@slack_t0123456789-u0123456789:${server_name}"
   ```

   For Slack, the effective composed configuration will look like this after the cluster-scoped sender-policy patch:

   ```yaml
   bridgedOrigins:
     slack:
       - "@slack_*:${server_name}"
   agents:
     agent-docs-qa:
       allowedSenders:
         - "@alice:${server_name}"
         - "@slack_t0123456789-u0123456789:${server_name}"
   ```

   The `bridgedOrigins` block is rendered evidence from `infra/bridges/slack/a2a`, not a second operator edit. Telegram follows the same pattern through its own component. Remove and reconcile the cluster-scoped grant before replacing the network component with its offboard component.

1. Prove both sides of the authorization boundary: the allowed remote identity receives one agent reply, and another bridged identity receives the policy notice without an A2A attempt. Retain the content-free audit fields `sender_origin_kind=bridge` and `sender_origin_network=<network>` with the Matrix event ID.

## Telegram live gate

The Telegram unit intentionally starts with dialog sync, automatic portal creation, default relays, relay-assisted bridge creation, backfill, and encryption disabled. Admin-only relay mode exists solely for an explicit per-portal `set-relay` after approval. A successful pod therefore proves only configuration and database connectivity; it does not read an account or create chat rooms.

1. Review the current Telegram API and content-licensing terms with the account owner. Telegram's current content terms prohibit using platform data for AI/ML development or deployment unless every relevant user gives explicit, informed, affirmative, continued consent limited to the specific chat and context. Do not bridge a chat into an agent room without that evidence.
1. Use an established Telegram account; upstream warns that a brand-new account using a third-party client can trigger anti-abuse controls. Keep an official client logged in for the initial code flow, or use the documented QR-linked-device flow.
1. Start a private unencrypted Matrix room with `@telegrambot:<server_name>`, send `login qr`, and scan the response from the official Telegram client. A six-digit-code login is also supported, but the code is delivered to an already logged-in client rather than SMS.
1. Treat that room as a transient credential channel. The pinned login flow uploads the QR image to Matrix media and includes the raw `tg://login` payload in the event body before requesting redaction after the step. Password inputs also cross Synapse before a redaction request, and the current bridge-v2 command path does not automatically redact Telegram's six-digit `2fa_code` input. Redaction is not erasure from media storage, logs, backups, notification caches, or clients. Use a synthetic/private room with the minimum retention available, manually redact residual login events, and revoke the linked Telegram session immediately if exposure cannot be excluded.
1. Ask the pinned management bot for `help bridge`, then bridge exactly one consented synthetic test chat. The current command shape is `bridge [login ID] <chat ID>`; treat the bot's versioned help as authoritative. The fail-closed portal filter still permits this explicit administrative action.
1. In the resulting portal, have Alice run `!tg set-relay [login ID]` and verify the bot confirms her approved login. Obtain Alice's explicit consent to use her Telegram account as the relay principal: every outbound agent or policy notice is authored provider-side by that account, while the in-message agent label is presentation rather than a separately authenticated Telegram bot or agent. Without this explicit per-portal relay, agent `m.notice` replies have no Telegram login and cannot traverse the return path.
1. Invite the selected `@agent-*:<server_name>` ghost into the Telegram portal and confirm its membership before sending the test mention from Telegram.
1. Record the resulting immutable `@telegram_<id>:<server_name>` ghost and allow only that full MXID on the selected no-tool/read-only agent. Never authorize by phone number, display name, username, or a whole `@telegram_*` namespace.
1. Prove the same allowed/denied, audit, fidelity, retention, and offboarding checks as Slack. On Telegram, verify both the underlying Alice relay account and the rendered Matrix-agent label; record this identity collapse explicitly rather than claiming end-to-end agent identity preservation. Logout through the management bot and revoke the linked session from Telegram's Devices screen when the test ends.

The shared runtime limits `/tmp` to 256 MiB and total container ephemeral storage to 512 MiB. Media conversion, logs, and writable runtime state share that budget, so the effective provider-file ceiling is lower and format-dependent. Oversized files must fail visibly without filling node storage; changing these limits is a reviewed capacity and denial-of-service decision, not a fidelity tweak.

## Acceptance evidence

A network is accepted only when the installed version passes all of these checks:

1. Flux reports the selected bridge, Matrix, and Postgres layers Ready from the reviewed revision.
1. The bridge image ID equals the declared digest and the pod satisfies restricted Pod Security admission.
1. Synapse loads the generated registration, the bridge is ready, and its namespace is exclusive and disjoint.
1. The scoped database role cannot connect to another service database.
1. NetworkPolicy runtime probes prove pod-selected Synapse/ESS HAProxy and scoped-Postgres paths, private/lateral denies, and the non-private IPv4 TCP/443 provider path on an enforcing CNI. They do not claim a provider-FQDN allowlist.
1. A real provider message traverses network → Matrix → A2A → Matrix → network. Evidence distinguishes the stable inbound Matrix ghost from the outbound provider principal: Slack uses the selected app/bot login, while Telegram uses the consenting relay user's account; rendered agent labels are presentation only.
1. An unallowlisted bridged identity is denied before invocation rate limiting or A2A; its policy notices use separate bounded rate limiting. An allowlisted identity is rate-limited under a key that includes network, full MXID, and agent.
1. Thread, edit/delete, file, typing, retention, logout/offboarding, provider outage, and oversized-media rejection behavior are recorded as observed behavior rather than inferred parity.

## Removal and retained state

1. Remove every external MXID from agent `allowedSenders` and verify the stricter Matrix-to-A2A config is active before disconnecting the provider.
1. Unset the portal relay, unbridge the channel/chat, log out through the management bot, and revoke/delete the remote provider session or app installation.
1. Replace `../../infra/bridges/<network>/cluster` with `../../infra/bridges/<network>/cluster-offboard` while keeping the encrypted secret file. Reconcile through Flux and wait for CNPG to apply the credential-free `NOLOGIN` role declaration. Through the approved database-administration path, verify three booleans without printing a password/hash: `pg_roles.rolcanlogin` is false, `pg_authid.rolpassword IS NULL` is true, and `pg_hba_file_rules` contains no database/user rule for `slackbridge` or `telegrambridge`. Workload pruning or Secret deletion alone is not credential revocation.
1. Only after that verification, remove the offboard component and encrypted secret in a second reviewed GitOps change. The role becomes unmanaged while retaining `NOLOGIN`; the workload, relay, and Synapse registration are already absent.
1. Apply the approved retention/legal-hold decision to the retained CNPG role, database, and backups. Optional Database resources explicitly use `databaseReclaimPolicy: retain`. Component removal is therefore not a data-erasure action; re-enabling/dropping the role or destroying the database/backups requires a separate, irreversible, approved procedure.

## Primary references

- [mautrix Go bridge setup](https://docs.mau.fi/bridges/go/setup.html) and [appservice registration](https://docs.mau.fi/bridges/general/registering-appservices.html)
- [mautrix Slack authentication](https://docs.mau.fi/bridges/go/slack/authentication.html) and [Slack Socket Mode](https://docs.slack.dev/apis/events-api/using-socket-mode/)
- [Slack's current non-Marketplace app policy/rate-limit notice](https://api.slack.com/changelog/2025-05-terms-rate-limit-update-and-faq)
- [mautrix Telegram authentication](https://docs.mau.fi/bridges/go/telegram/authentication.html), [pinned QR-login source](https://github.com/mautrix/telegram/blob/v0.2606.0/pkg/connector/loginqr.go), [pinned Matrix login-command source](https://github.com/mautrix/go/blob/v0.28.1/bridgev2/commands/login.go), and [Telegram API terms](https://core.telegram.org/api/terms)
- [Telegram content-licensing and AI restrictions](https://telegram.org/tos/content-licensing)
- [mautrix Signal authentication](https://docs.mau.fi/bridges/go/signal/authentication.html) and [Signal terms](https://signal.org/legal/)
- [mautrix WhatsApp authentication](https://docs.mau.fi/bridges/go/whatsapp/authentication.html) and [WhatsApp terms](https://www.whatsapp.com/legal/terms-of-service)
- [CloudNativePG declarative databases](https://cloudnative-pg.io/docs/1.27/declarative_database_management/), [roles](https://cloudnative-pg.io/docs/1.27/declarative_role_management/), and [ordered HBA configuration](https://cloudnative-pg.io/docs/1.27/postgresql_conf/#the-pg_hba-section)
