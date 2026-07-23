---
type: Runbook
title: Ketesa Administrator Console
description: Enable, validate, operate, and remove the opt-in Ketesa client without weakening Synapse or MAS authorization.
---

# Ketesa Administrator Console

[Ketesa](https://github.com/etkecc/ketesa) is the optional Apache-2.0 administrator client at `admin.<server_name>`. Fgentic pins v1.3.0 by multi-architecture image digest and disables it in every tracked cluster profile. The console is a static browser application: it stores no service credential and calls the existing Synapse and Matrix Authentication Service (MAS) APIs with the interactive user's token.

This separation is the security boundary. Loading the public static UI is not administrative access. The dedicated Gateway listener accepts routes only from the `admin` Namespace, the Pod accepts ingress only from the Gateway Namespace and has no egress, and Synapse/MAS decide whether each browser API operation is authorized. A non-administrator must still receive no admin capability. Gateway reachability, a successful login, or a rendered manifest does not prove that negative authorization behavior.

## Enable the console

The structural flag is `admin_console` in the target cluster's tracked `platform-settings.yaml`. It accepts only `enabled` or `disabled`; Kustomize resolves the matching admin and shared-Gateway profile before Flux reads either source path. Do not put this flag in `platform-settings-overrides`: post-build substitution occurs too late to select a Flux source path.

1. Change `admin_console: disabled` to `admin_console: enabled` in `clusters/<env>/platform-settings.yaml`.
1. For a public deployment, create `admin.<server_name>` DNS pointing at the shared Gateway address. Local `.localhost` routing needs no DNS record.
1. Run `mise run check:admin-console`, `mise run check:overlays`, and `mise run check:manifests`. The focused check proves both selected paths, the disabled zero-inventory profile, the enabled listener and route, the immutable image, restricted Pod posture, quota, and NetworkPolicy.
1. Commit the change and let Flux reconcile. Wait for `gateway` before `admin`; the latter depends on the Gateway and Matrix layers.
1. Open `https://admin.<server_name>`. The locked `config.json` removes the homeserver picker, resolves the apex well-known document, selects external authentication, and sends no browser cookies to cross-origin APIs. Choose the OIDC login path and authenticate through MAS.

Ketesa v1.3.0 uses MAS dynamic client registration for this browser flow. It registers a public client (`token_endpoint_auth_method: none`) with the exact `https://admin.<server_name>/auth-callback/` redirect and requests `urn:mas:admin` alongside the Matrix client scopes. MAS grants that interactive admin scope only to a user permitted by `policy.data.admin_users` (or the corresponding per-user `can_request_admin` attribute); the Fgentic reference designates `alice` for bootstrap acceptance. Do not add Ketesa to `policy.data.admin_clients`: MAS reserves that list for non-interactive `client_credentials` clients, while a static SPA cannot hold a client secret.

ESS 26.6.2 already exposes MAS's `adminapi` resource on the public port-8080 MAS listener and keeps secure dynamic-registration defaults (`allow_host_mismatch: false`, `allow_insecure_uris: false`). The offline admin-console gate renders that exact pinned chart and rejects listener drift. Do not add a second listener or override the chart-owned `http.listeners` configuration.

## Required authorization acceptance

Use two disposable identities and record the Git revision, server name, Ketesa version, user MXIDs, timestamps, and HTTP status evidence without retaining access tokens or room content.

1. Sign in as the designated Synapse/MAS administrator. Confirm the Users, Rooms, Reported events, Media, MAS sessions, and Registration Tokens surfaces load and one read-only detail request succeeds.
1. In a separate browser profile, sign in as an ordinary user. The static application may load, but Synapse `/_synapse/admin/*` and MAS admin requests must be rejected and no administrative data or action may succeed. Record the bounded endpoint and status only; never capture the bearer token.
1. Suspend a disposable user, confirm its existing session or next login is rejected, reactivate it, and confirm login recovers.
1. From Element, file an event report against synthetic content. Confirm the report appears under **Reported events**, inspect it, then dismiss it only after the test action is complete.

If the ordinary user can read or mutate any admin surface, disable `admin_console` immediately and treat it as an authorization incident. A gateway IP allowlist can be added by a deployment as defense in depth, but it is environment-specific and cannot replace the application-level negative test. The reference does not add an auth proxy whose session or group semantics could silently diverge from MAS.

## Five common operator tasks

### Suspend and reactivate a user

1. Open **Users**, select the full MXID, and open **Edit**.
1. Set **Deactivated** and save. Under MAS, use the MAS account-state control shown by Ketesa; do not use **Erase** or a destructive delete path for a temporary suspension.
1. Verify the user's access is blocked.
1. Clear **Deactivated**, save, and verify access returns. Attribution of who performed each privileged admin-API mutation (room purge, media quarantine, report dismissal), when, and with what outcome is captured by the opt-in content-bounded admin-action audit stream `fgentic.admin_action.v1` ([ADR 0018 — Admin-action audit](adr/0018-content-bounded-identity-audit.md#admin-action-audit-455)), not a manual note. Note its honest boundaries: it does not assert suspend vs reactivate direction (a body-only request argument), does not attribute registration-token or MAS-plane actions, and covers admin-API calls only — record any out-of-scope approval rationale (e.g. a `kubectl` or direct-database change) in the change ticket.

### Review an event report

1. Open **Reported events** and select the report.
1. Read **Basic** for the reporter, sender, room, and reason; use **Details** for the stored event JSON. Do not rank reports by the legacy severity score.
1. Take the required user, room, or media action separately.
1. Use **Delete** only to dismiss the report record after handling it; dismissal does not affect the reported event or sender.

### Quarantine media

1. Open **Rooms**, select the room, then open its **Media** tab.
1. Use **Quarantine** for one file or **Quarantine all media** for the room and confirm the scope.
1. Verify the file is no longer retrievable. Quarantine is reversible with **Unquarantine**; **Delete** is not.

### Purge room history

1. Open **Rooms**, select the room, and choose **Purge history**.
1. Set **Purge events before**. Enable **Also delete events sent by local users** only when the approved retention or incident scope requires it.
1. Confirm and retain the background-operation result. Purging removes events before the cutoff; it does not delete or block the room.

### Issue a registration token

1. Open **Registration Tokens** and choose **Create**.
1. Set an explicit usage limit and expiry. Avoid unlimited, non-expiring tokens unless a reviewed policy requires one.
1. Deliver the token through the approved channel and record its owner and expiry without putting it in Git or logs.
1. MAS tokens are retired with **Revoke**; use **Unrevoke** only after a new approval.

## Disable and verify zero footprint

1. Set `admin_console: disabled` in the tracked cluster settings, commit, and reconcile through Flux.
1. Wait for both `admin` and `gateway` Kustomizations to become Ready. The empty admin profile prunes its previous inventory; the disabled Gateway profile removes the `https-admin` listener and its certificate SAN.
1. Verify the `admin` Namespace, Ketesa Deployment, Service, ConfigMap, HTTPRoute, quotas, and NetworkPolicy are absent, and that the shared Gateway has no `https-admin` listener.
1. Remove public DNS only after reconciliation. DNS removal alone is not decommissioning evidence.

No secret rotation is required: Ketesa stores no client secret or administrator token. Browser sessions and upstream user tokens remain governed by MAS and Synapse.

## Evidence boundary

`mise run check:admin-console` is offline declared-posture evidence. Closing the feature acceptance additionally requires the live local-cluster admin and non-admin proof above. This repository change does not claim that shared-runtime evidence.
