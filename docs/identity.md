---
type: Specification
title: Identity and SSO
description: "Identity architecture: MAS as the Matrix-facing OIDC authority with Keycloak (or any upstream OIDC provider) for humans."
---

# Identity and SSO

Fgentic uses Matrix Authentication Service (MAS) as the Matrix-facing OAuth/OIDC authority and an upstream OpenID Connect provider as the human identity source. The reference provider is Keycloak; Entra ID or another conformant provider replaces the SOPS-backed MAS provider fragment without changing Element, Synapse, or the bridge.

Authentication audit is not inferred from MAS access logs or Synapse log formatting. [D19](design-decisions.md) and [ADR 0018](adr/0018-content-bounded-identity-audit.md) define the optional exact-version evidence boundary: a committed MAS authentication can be joined to a later Synapse event by full MXID, but the platform cannot prove that a particular MAS session or token submitted that event and does not attribute failed MAS attempts to a named user.

## Reference login contract

The browser flow is Element → MAS (`auth.<server_name>`) → Keycloak (`id.<server_name>`) → MAS. The upstream provider ID is the stable ULID `01H8PKNWKKRPCBW4YGH1RWV279`, so the matching redirect URI is always:

```text
https://auth.<server_name>/upstream/callback/01H8PKNWKKRPCBW4YGH1RWV279
```

The SOPS Secret `mas-upstream-oidc` supplies the exact MAS 1.19.0 `upstream_oauth2` fragment through ESS 26.7.0's `matrixAuthenticationService.additional.configSecret` interface. On the first successful upstream login, `claims_imports.skip_confirmation: true` completes MAS registration and queues Synapse provisioning automatically.

**OIDC backchannel logout (session hygiene, issue #278).** The provider fragment sets `on_backchannel_logout: logout_all`, and the Keycloak `fgentic` client is configured with front-channel logout **off** and a backchannel-logout URL pointed at the **internal** MAS Service — `http://ess-matrix-authentication-service.matrix.svc.cluster.local:8080/upstream/backchannel-logout/01H8PKNWKKRPCBW4YGH1RWV279` — with the session id required. An **explicit** Keycloak user or session logout then POSTs a logout token to MAS, which terminates every session started by that upstream OIDC session (the MAS browser session **and** its client sessions); a user who stays enabled can log in again afterward. Keycloak's `keycloak` NetworkPolicy allows exactly that egress to the MAS pod on `:8080` and nothing broader. Stated precisely: this is **defense-in-depth session hygiene, not the IdP-disable trigger** — setting a Keycloak user `enabled=false` does not emit backchannel logout and does not deactivate the MAS account (that is [#153](https://github.com/fmind-ai/fgentic/issues/153)). `mise run check:identity` asserts this wiring offline; the live proof (an explicit logout terminating the derived MAS sessions) is a cluster step.

Identity mapping is deliberately fail-closed:

1. `matrix_localpart` is a single-valued, required, administrator-managed IdP attribute. Keycloak's user-profile policy hides it from end users and only administrators can edit it.
1. MAS imports `{{ user.matrix_localpart }}` with `action: require` and `on_conflict: fail`. It never derives an MXID from a mutable username or email address, and it never attaches an upstream identity to an existing Matrix account implicitly.
1. `name` becomes the Matrix display name and `email` becomes the account email with `action: force`; absence does not change the stable MXID.
1. The optional `fgentic-groups` scope emits full-path string values such as `/platform/admins`. This is interoperability and diagnostic data only. It is not a runtime authorization credential; accepted [ADR 0009](adr/0009-agent-authorization-model.md) and [D20](design-decisions.md) authorize from reconciled Matrix room membership instead.

Group names themselves must not contain `/`; the slash is the Keycloak path separator. Every externally managed user must receive `matrix_localpart` before login or MAS rejects registration.

## Password-login policy

`mas_local_login_enabled` is a per-cluster setting consumed by the ESS values:

- `clusters/local/platform-settings.yaml` sets it to `true`, retaining password login as a local recovery and development path alongside SSO.
- `clusters/gcp/platform-settings.yaml` sets it to `false`, making the reference production profile SSO-only.

For another production cluster, set the value to `"false"` in its `platform-settings` ConfigMap or in the optional `platform-settings-overrides` ConfigMap. Disabling login does not enable self-service password registration; that remains disabled by MAS by default.

## Secret lifecycle

`scripts/gen-secrets.sh <server_name> <local|gcp>` creates one random OIDC client secret and writes two resources to the bootstrap-only `keycloak-bootstrap.sops.yaml`:

1. `keycloak/keycloak-credentials`, consumed by Keycloak's one-time realm import.
1. `matrix/mas-upstream-oidc`, containing the MAS provider config and the same client secret.

The generator preserves this file even with `--force`; rotating only one side would break login because Keycloak skips startup import after the realm exists. To rotate deliberately, update the live Keycloak client through its Admin API, update both encrypted Secret payloads with `sops`, and restart MAS after Flux reconciles. Never commit a decrypted copy.

Because the realm import is bootstrap-only, changing the committed realm config (for example the #278 backchannel-logout attributes) reaches only **fresh** clusters. To bring an **existing** cluster's live `fgentic` client to the same state, run the idempotent Admin-API migration `mise run identity:backchannel-migrate` (it execs `kcadm.sh` inside the running Keycloak pod, so it needs no exposed admin endpoint or extra NetworkPolicy; re-running is a no-op). Fresh bootstrap and a migrated realm then expose identical backchannel-logout configuration.

## Break-glass administration (SSO-outage recovery)

The reference production profile is SSO-only: `clusters/gcp/platform-settings.yaml` sets `mas_local_login_enabled: "false"`, so every human login flows Element → MAS → Keycloak. A Keycloak outage, a bad realm change, or a broken upstream-OIDC fragment therefore locks out **every** human, including the administrators who must fix it, and the [Ketesa admin console](admin-console.md) too because it authenticates through MAS OIDC. Break-glass is the tested, bounded, audited path to administer the identity plane during that window. It is **composition, not new platform**: the mechanisms already exist upstream (the MAS Admin API, MAS compatibility tokens, the GitOps-flippable `mas_local_login_enabled`, and SOPS-sealed credentials); Fgentic owns only the ladder, its fail-closed default posture, and its audit join.

**Security-review answer — who can administer when SSO is down, and how is that use audited?** Nobody, by default: the reference cluster ships no standing recovery credential and no local login. Administration is possible only after a deliberate, reviewed GitOps break-glass window opens the ladder below, and every MAS-plane admin action it performs is recorded content-free (see [Audit of break-glass use](#audit-of-break-glass-use)). Prefer the lowest sufficient rung.

1. **MAS Admin API (client credentials) — user and session administration.** The MAS Admin API is MAS-local and keeps working while the upstream IdP is unreachable, so it is the primary rung for suspending a user or terminating sessions (the same surface as [#153](https://github.com/fmind-ai/fgentic/issues/153)). It authenticates a confidential client through the OAuth 2.0 `client_credentials` grant with the `urn:mas:admin` scope (which grants the whole Admin API). Enabling this rung is a composition step: register the break-glass client in the MAS `clients` fragment (alongside the existing `00-login-policy` clients in [`infra/matrix/helmrelease.yaml`](../infra/matrix/helmrelease.yaml)) with its secret supplied through a SOPS Secret, then reconcile. It requires no password login and no interactive session.
1. **Scoped MAS compatibility token — Synapse admin actions.** For actions that only the Synapse admin API can perform (for example redacting or purging content, media administration — see [retention](retention.md)), issue a **scoped, short-lived** MAS compatibility token for an administrator principal and call the Synapse admin API with it. This reuses MAS's compatibility-token surface rather than minting a permanent Synapse admin token, so the grant is bounded to the incident.
1. **Last-resort interactive access — GitOps flip plus the pre-provisioned recovery admin.** When neither API rung suffices (for example MAS policy itself must be inspected interactively), deliberately flip `mas_local_login_enabled` to `"true"` in the cluster's `platform-settings` (or `platform-settings-overrides`) ConfigMap, reconcile, and log in as the **pre-provisioned, disabled-by-default local recovery admin** whose password exists only in `clusters/<env>/secrets/break-glass-recovery.sops.yaml`. Close the window by reverting the flip and re-disabling the account.

### The recovery credential and what "disabled by default" means

The recovery admin credential is the SOPS Secret `break-glass-recovery` (namespace `matrix`, username `break-glass`), templated in [`infra/secrets/break-glass-recovery.sops.yaml.example`](../infra/secrets/break-glass-recovery.sops.yaml.example). It is **absent by default and never a standing superuser**: `scripts/gen-secrets.sh` emits it only when `FGENTIC_SECRET_SET=break-glass` is set explicitly, never on the default `all` bootstrap or a `rotatable` sweep, so a normal SSO-first cluster ships nothing. `scripts/test-break-glass.sh` (`mise run check:break-glass`) gates this fail-closed posture offline.

"Disabled by default" is layered, and no single flip is enough to authenticate:

1. **Absent** — the credential is not generated, so there is nothing to seal, leak, or use.
1. **No local login** — `mas_local_login_enabled` stays `"false"`, so even a provisioned recovery account cannot authenticate by password until the deliberate flip.
1. **Not provisioned/enabled** — even after the flip, the account is usable only once an operator provisions it in MAS (from the sealed password) and enables it for the window; both the provisioning and the enable are explicit, reviewed steps, not a side effect of generating the file.

This layering keeps the [SSO-first bootstrap stance](#sso-first-demo-bootstrap) intact — the platform stores no unattended superuser — and is federation-safe: break-glass is an identity-plane, single-homeserver recovery control and grants no cross-organization authority (cross-org containment is the distinct [#350](https://github.com/fmind-ai/fgentic/issues/350) surface).

### Rotation

The recovery password is **bootstrap-once**, for the same reason as the [Keycloak bootstrap file](#secret-lifecycle): once it provisions the live MAS recovery account it becomes that account's source of truth, so silently regenerating it (via `--force` or a bulk rotation) would drift the sealed value from the live account. `gen-secrets.sh` therefore preserves it even with `--force`, and `scripts/rotate-secrets.sh` deliberately does not sweep it — matching the precedent that no rotation set touches the Keycloak admin or demo-user passwords. To rotate, change the live MAS recovery account during a controlled window (MAS Admin API / `mas-cli manage`), then re-seal the file (`sops` edit, or delete and re-run `FGENTIC_SECRET_SET=break-glass`); never commit a decrypted copy.

### Audit of break-glass use

Break-glass use is not exempt from the audit trail, but the coverage is uneven at the pinned versions and is stated honestly rather than overclaimed — the full mapping and limits are in [Audit → break-glass administration](audit.md#break-glass-administration):

1. **Rung 2 (Synapse admin actions)** are captured **content-free** by the opt-in `fgentic.admin_action.v1` stream, which resolves the acting admin's MXID, action class, target, and outcome from the pinned Synapse admin request log.
1. **Rung 3 (recovery-admin password login)** produces a successful MAS authentication recorded by `fgentic.mas_authentication.v1` (`method` = password), joinable by full MXID to the recovery admin's later Synapse events.
1. **Rung 1 (MAS Admin API)** is the honest gap: `fgentic.admin_action.v1` deliberately does **not** attribute MAS-plane admin actions, and MAS 1.19.0 request telemetry attributes neither the user nor a stable outcome ([ADR 0018](adr/0018-content-bounded-identity-audit.md#admin-action-audit-455)). Prefer rung 2/3 when an attributable record is required; treat rung 1 as least-audited and confirm its live behavior in the drill.

None of this bypasses durable audit retention ([#157](https://github.com/fmind-ai/fgentic/issues/157)/[#363](https://github.com/fmind-ai/fgentic/issues/363)) — the streams follow the same 90-day retention and operator/auditor access controls as the rest of the identity audit. The identity-plane lockout runbook is the [operations handbook §5 incident table](operations-handbook.md#5-respond-from-the-signal-not-the-guess).

**Deferred acceptance (runtime drill, out of scope here).** The offline core above delivers the ladder, the disabled-by-default credential, its default-posture gate, and the audit join. It does **not** by itself prove the live behavior: the scripted drill on a local SSO-only cluster — scale Keycloak to zero, confirm normal SSO login fails, exercise a MAS admin action through the break-glass client, exercise a Synapse admin action through a scoped compatibility token, restore Keycloak, and confirm every action appears in the audit trail attributed to the break-glass identity — is the acceptance proof for the installed versions ([#467](https://github.com/fmind-ai/fgentic/issues/467) Task 4).

## Entra ID variant

Register a single-tenant Entra application with the MAS redirect URI above and a web client secret. Configure an administrator-controlled directory extension or claims-mapping policy to emit a single-valued `matrix_localpart` claim; do not map `preferred_username`, `email`, or the display name to the MXID because they can change.

Replace `provider.yaml` in the environment's encrypted `mas-upstream-oidc` Secret with:

```yaml
upstream_oauth2:
  providers:
    - id: 01H8PKNWKKRPCBW4YGH1RWV279
      issuer: https://login.microsoftonline.com/<tenant-id>/v2.0
      human_name: Microsoft Entra ID
      client_id: <application-client-id>
      client_secret: <application-client-secret>
      token_endpoint_auth_method: client_secret_post
      scope: openid profile email
      # MAS 1.19's upstream Entra sample uses relaxed metadata validation. Token signatures,
      # issuer, audience, and nonce are still validated; retest strict `oidc` mode on upgrades.
      discovery_mode: insecure
      claims_imports:
        skip_confirmation: true
        localpart:
          action: require
          template: "{{ user.matrix_localpart }}"
          on_conflict: fail
        displayname:
          action: force
          template: "{{ user.name }}"
        email:
          action: force
          template: "{{ user.email }}"
```

Entra's native `groups` claim contains object IDs, not Keycloak-style full paths. MAS does not consume groups for authorization, so either omit it or normalize it into the exact-path directory contract in accepted ADR 0009; do not treat token object IDs as runtime authorization.

## Generic OIDC variant

Use a tenant-specific issuer whose discovery document advertises that exact issuer. The provider must support authorization code flow, confidential-client authentication, and the `openid` scope. Prefer PKCE support. Create a fresh stable ULID if this is an additional provider; if it replaces Keycloak, retain the existing ULID and redirect URI.

The minimum provider fragment is the Keycloak example in `infra/secrets/keycloak-bootstrap.sops.yaml.example` with these substitutions:

1. Set `issuer`, `human_name`, `client_id`, `client_secret`, and the supported `token_endpoint_auth_method`.
1. Request the provider's profile/email scopes.
1. Emit an immutable, administrator-controlled `matrix_localpart` string and retain `action: require` plus `on_conflict: fail`.
1. Map display name and email only when those claims are present. With `skip_confirmation: true`, their actions must be `ignore`, `force`, or `require`, never `suggest`.
1. Configure the provider-side redirect URI exactly; wildcard callbacks are not acceptable.

Run the pinned chart render and MAS config check before reconciliation, then test a new identity. A successful existing-user login alone does not prove first-login provisioning.

## SSO-first demo bootstrap

After Flux reports Keycloak, MAS, and Synapse Ready, run:

```bash
scripts/bootstrap-admin.sh --server-name fgentic.localhost
```

The script uses the declaratively configured public MAS bootstrap client and the OAuth 2.0 device authorization grant. A stable client avoids accumulating dynamic registrations across repeated runs and has no client secret to leak. It prints a one-time URL where Alice authenticates through Keycloak, verifies the returned Matrix identity is exactly `@alice:<server_name>`, promotes the already auto-provisioned Synapse user, and creates `#fgentic-demo:<server_name>` if absent. A repeated run is safe.

This is intentionally interactive: creating the first administrator without proving control of the designated identity would require storing a permanent registration shared secret or unattended superuser credential. Fgentic stores neither. The issued access token is user-backed, short-lived, held only in memory, and never printed.
