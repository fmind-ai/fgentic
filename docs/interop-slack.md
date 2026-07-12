---
type: Runbook
title: Slack Interop Walkthrough
description: Operator walkthrough for enabling the opt-in mautrix-slack bridge unit over Socket Mode.
---

# Slack interop walkthrough

Status: manifests and offline policy tests are implementable without provider access. Live Slack workspace acceptance, bidirectional screenshots, and a real provider audit record require a workspace owner and remain a human gate.

This runbook uses the supported `mautrix-slack` app-login path: a dedicated internal Slack app receives events over Socket Mode and relays one explicitly approved channel. It does not use a user's browser token or `d` cookie. Slack remains a third-party data and identity authority; this path is optional and is not part of Fgentic's sovereign core.

## Before enabling it

1. Obtain approval from the Slack workspace owner for an internal app, the upstream manifest's exact scopes, Socket Mode, the selected channel, the Matrix destination room, retention, incident ownership, and the fact that approved content may be sent to the selected model provider through an agent invocation.
1. Review Slack's current application terms and distribution policy. This reference is for an operator's own workspace; do not present it as a generally distributed or Marketplace-approved Slack client.
1. Decide which immutable Slack user IDs may invoke which agents. Display names, email addresses, channel names, and the Matrix ghost prefix are not authorization inputs.
1. Confirm the target room is an unencrypted agent room. Fgentic does not claim end-to-end encryption across Slack, the bridge database, Matrix, the A2A path, or an external model provider.

## Deploy the bridge

1. Add `../../infra/bridges/slack/cluster` to the target cluster's `components` list and generate the encrypted Slack secret set as described in [external-network interop](interop.md#opt-in-to-a-reference-unit).
1. Run `mise run format`, `mise run check`, and `mise run test`, review the resulting manifests/ciphertext, then commit and push through the normal Flux workflow.
1. Wait for the dependency chain:

   ```bash
   flux get kustomization postgres
   flux get kustomization matrix
   flux get kustomization mautrix-slack
   kubectl -n bridges rollout status statefulset/mautrix-slack --timeout=5m
   kubectl -n bridges get pod -l app.kubernetes.io/instance=mautrix-slack \
     -o jsonpath='{range .items[*]}{.status.containerStatuses[0].imageID}{"\n"}{end}'
   ```

1. Confirm the image ID contains `sha256:f1de44e723a13484a6b09a26b93127e494c25a70d4d21c2300bfddf49a7dae03`, then inspect the bridge logs for readiness without printing Secret objects.

## Create the internal Slack app

1. Open Slack's app-management UI as the workspace owner and choose **Create New App → From an app manifest**.
1. Paste the `app-manifest.yaml` from the exact pinned `mautrix/slack` `v0.2606.0` release. Review every requested scope; do not substitute a manifest from `main` or `latest`.
1. Create an app-level token with the `connections:write` scope and retain the resulting `xapp-` token only long enough to complete login.
1. Install the app into the approved workspace and retain the resulting `xoxb-` bot token only long enough to complete login.
1. Add the app to only the approved Slack channel. Do not grant organization-wide installation or additional channels for convenience.

Slack's own Socket Mode documentation confirms that the app-level token opens the WebSocket connection and avoids a public request URL. The upstream bridge manifest remains the source of its bot scopes and subscribed events.

## Log in and bridge one channel

1. In Element, start a private unencrypted room with `@slackbot:<server_name>`. Do not add other users or bots.
1. Send `login app` and provide the `xoxb-` and `xapp-` values only when the bridge bot prompts for them. The session is stored in the `slackbridge` database; it is not a Kubernetes deployment secret.
1. Treat that private Matrix room as a transient credential channel, not as proof of DB-only storage. The pinned bridge requests redaction of token messages after parsing them, but each token first crosses Synapse as plaintext and redaction cannot guarantee erasure from homeserver logs, backups, notification caches, or clients. Include this Matrix retention path in the approval and incident boundary; revoke the Slack app tokens if exposure cannot be excluded.
1. Delete any local clipboard/note copies after the bridge confirms login. Never paste the tokens into a shell command, issue, screenshot, or Git-managed file.
1. Follow the bridge bot's current `help` output to bridge the one approved Slack channel and invite the selected `@agent-*` ghost into the resulting portal room. Commands are versioned behavior; do not copy an unverified command from an older runbook.
1. In that portal, have Alice run `!slack set-relay [login ID]` and verify the bot confirms her approved Slack login. The bridge has no default relay, only Alice may manage one, and relay-assisted channel creation remains disabled. Without this explicit per-portal step, an `@agent-*` Matrix ghost has no Slack login and its `m.notice` reply cannot return to Slack.
1. Read the portal room state and record the exact Slack ghost MXID representing the test user. The pinned bridge derives a lowercase, workspace-scoped localpart from both immutable Slack IDs, for example `@slack_t0123456789-u0123456789:<server_name>`; do not authorize a display name or user ID without the team prefix.

## Authorize one sender

Selecting `infra/bridges/slack/cluster` already injects Slack's exclusive namespace as a bridged origin. Do not duplicate that patch or put a provider MXID in `apps/matrix-a2a-bridge/deploy/helmrelease.yaml`: canonical values reach every cluster, including clusters where Slack is absent and the same local MXID would be misclassified as ordinary Matrix.

Append the exact identity through the target cluster's `bridge.spec.patches` list. Use an outer JSON patch at the top-level `patches` key, after any existing patch that targets `bridge`, so the entry is appended without replacement; for example, the local overlay already carries its image patch:

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

After the cluster-scoped sender patch and the network component compose, the effective configuration will contain:

```yaml
bridgedOrigins:
  slack:
    - "@slack_*:${server_name}"
agents:
  agent-docs-qa:
    namespace: kagent
    name: docs-qa
    allowedSenders:
      - "@alice:${server_name}"
      - "@slack_t0123456789-u0123456789:${server_name}"
```

The `bridgedOrigins` block above is rendered evidence, not an additional operator edit. A new agent also needs its exact Matrix ghost MXID in the Slack profile's relay permission map, as described below. Remove the cluster-scoped sender patch and reconcile that denial before offboarding the provider component.

After Flux rolls the Matrix-to-A2A bridge, confirm its configuration reload succeeds. A recognized Slack sender that is absent from `allowedSenders` must receive a polite `m.notice`; the bridge must emit a policy-denied audit result and make no A2A request. Slack display name changes must not alter either result.

The provider bridge independently blocks every Matrix identity except Alice and the exact A2A appservice identities declared in `infra/bridges/slack/helmrelease.yaml`. A newly added agent will receive Matrix replies but cannot relay them to Slack until its full `@agent-*:<server_name>` MXID is added to that permission map and the offline bridge contract passes.

## Bidirectional acceptance

Use synthetic, non-sensitive content and a read-only/no-tool agent for the first test.

1. From the allowlisted Slack user in the approved channel, mention `@agent-docs-qa` with a unique nonce and a harmless documentation question.
1. Verify the portal has one Matrix event from the expected `@slack_<immutable-id>` ghost and one threaded `m.notice` reply from the expected agent ghost.
1. Verify Slack receives exactly one reply in the approved channel. Confirm its underlying `app_id`/`bot_id` belongs to the selected relay login and that the rendered agent label matches the Matrix ghost; record observed thread placement. The label is presentation, not a distinct authenticated Slack agent identity, and Matrix reply relations do not prove Slack thread parity.
1. Repeat from a second, unallowlisted Slack identity. It must receive the policy notice, and bridge logs must contain `sender_origin_kind=bridge`, `sender_origin_network=slack`, `a2a_attempted=false`, and the policy-denied outcome for that Matrix event ID.
1. Run the normal attribution collector for the allowed event and verify the end-to-end join. Store the content-free JSON outside Git.

## Screenshot evidence

Screenshots are acceptance artifacts, not source files. Attach redacted captures to issue #48 or its pull request only after the real test succeeds:

1. Slack channel: synthetic request nonce, immutable user/member detail visible separately, and returned agent reply/thread.
1. Element portal: matching nonce, Slack ghost MXID, agent ghost MXID, and reply relation.
1. Policy denial: second Slack identity and polite denial notice, with no private tokens or unrelated channel history.
1. Flux/runtime: Ready Kustomizations and digest image ID. Do not include Secret values, pod environment, private workspace URLs, email addresses, or access tokens.

Record the test date, bridge release/digest, Synapse/ESS version, Slack workspace type, channel type, agent/model profile, event IDs, and every redaction. Absence of these live artifacts keeps the issue at “prepared, human acceptance pending.”

## Fidelity and sovereignty limits

1. **Identity:** Fgentic trusts the Slack bridge's mapping from a Slack member to a Matrix ghost. A Matrix MXID does not independently prove the human or Slack tenant. In the outbound direction, all relayed agent and policy notices are authenticated to Slack through Alice's selected app login and therefore share that app/bot identity. Relay message formats and custom display names provide attribution for readers, but they do not create a separately authenticated Slack identity for each Matrix agent.
1. **Threads and replies:** Matrix relations and Slack thread timestamps are different models. Verify the selected channel type and reply direction; nested or moved context may flatten.
1. **Typing and presence:** ephemeral signals are best-effort and may be absent, delayed, or intentionally disabled.
1. **Edits, deletes, and retention:** provider-side edits/deletions may arrive late or fail, and copies can remain in Matrix history, bridge state, logs, clients, backups, or a selected model provider. Slack deletion is not a global erasure primitive.
1. **Files and formatting:** file access depends on app scopes, size limits, retention, and media conversion. The shared pod bounds `/tmp` at 256 MiB and total ephemeral storage at 512 MiB, so the effective conversion ceiling is lower and format-dependent; oversized media must fail rather than consume node storage. Rich blocks, mentions, emoji, and formatting do not have guaranteed lossless Matrix equivalents.
1. **Encryption:** Slack terminates its own transport and storage controls; the Fgentic agent room is intentionally unencrypted. There is no cross-network E2EE claim.
1. **Availability and limits:** Socket Mode, Slack API changes/rate limits, app policy, token revocation, the bridge, Synapse, and the model path can each interrupt the round trip.
1. **Network egress:** Kubernetes NetworkPolicy admits arbitrary non-private IPv4 TCP/443 for Slack's HTTPS and Socket Mode endpoints; it cannot enforce an FQDN allowlist. A governed egress proxy or FQDN-aware CNI is required if that residual path is unacceptable.
1. **AI use:** only intentionally mentioned content should reach an agent. Do not backfill or bulk-export a workspace into an agent context, and honor customer/provider restrictions on data retention and model training.

## Offboard

1. Remove the allowed Slack MXIDs from every agent and verify the stricter bridge config is active first.
1. Run `!slack unset-relay`, unbridge the channel, and log out through the management bot.
1. Revoke/delete the Slack app tokens and remove the app from the workspace.
1. Replace `../../infra/bridges/slack/cluster` with `../../infra/bridges/slack/cluster-offboard` while keeping the encrypted Slack secret. Reconcile through Flux, wait for CNPG, and verify through the approved database-administration path—without printing password data—that `pg_roles.rolcanlogin` is false, `pg_authid.rolpassword IS NULL` is true, and `pg_hba_file_rules` contains no `slackbridge` database/user rule.
1. Only then remove the offboard component and encrypted secret in a second reviewed change. The role and database remain retained, but the credential-free role stays `NOLOGIN`; workload or Secret deletion alone would not revoke it.
1. Apply the approved retention/legal-hold policy to `slackbridge`. Its Database resource uses `databaseReclaimPolicy: retain`; dropping the role or destroying the database/backups is a separate irreversible procedure requiring explicit approval. Deleting Kubernetes resources is not erasure.
