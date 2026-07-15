---
type: Specification
title: Identity and SSO
description: Identity architecture: MAS as the Matrix-facing OIDC authority with Keycloak (or any upstream OIDC provider) for humans.
---

# Identity and SSO

Fgentic uses Matrix Authentication Service (MAS) as the Matrix-facing OAuth/OIDC authority and an upstream OpenID Connect provider as the human identity source. The reference provider is Keycloak; Entra ID or another conformant provider replaces the SOPS-backed MAS provider fragment without changing Element, Synapse, or the bridge.

Authentication audit is not inferred from MAS access logs or Synapse log formatting. [ADR 0018](adr/0018-content-bounded-identity-audit.md) defines the optional exact-version evidence boundary: a committed MAS authentication can be joined to a later Synapse event by full MXID, but the platform cannot prove that a particular MAS session or token submitted that event and does not attribute failed MAS attempts to a named user.

## Reference login contract

The browser flow is Element → MAS (`auth.<server_name>`) → Keycloak (`id.<server_name>`) → MAS. The upstream provider ID is the stable ULID `01H8PKNWKKRPCBW4YGH1RWV279`, so the matching redirect URI is always:

```text
https://auth.<server_name>/upstream/callback/01H8PKNWKKRPCBW4YGH1RWV279
```

The SOPS Secret `mas-upstream-oidc` supplies the exact MAS 1.19.0 `upstream_oauth2` fragment through ESS 26.6.2's `matrixAuthenticationService.additional.configSecret` interface. On the first successful upstream login, `claims_imports.skip_confirmation: true` completes MAS registration and queues Synapse provisioning automatically.

Identity mapping is deliberately fail-closed:

1. `matrix_localpart` is a single-valued, required, administrator-managed IdP attribute. Keycloak's user-profile policy hides it from end users and only administrators can edit it.
1. MAS imports `{{ user.matrix_localpart }}` with `action: require` and `on_conflict: fail`. It never derives an MXID from a mutable username or email address, and it never attaches an upstream identity to an existing Matrix account implicitly.
1. `name` becomes the Matrix display name and `email` becomes the account email with `action: force`; absence does not change the stable MXID.
1. The optional `fgentic-groups` scope emits full-path string values such as `/platform/admins`. This is interoperability and diagnostic data only. It is not a runtime authorization credential; [ADR 0009](adr/0009-agent-authorization-model.md) remains **Proposed**.

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

Entra's native `groups` claim contains object IDs, not Keycloak-style full paths. MAS does not consume groups for authorization, so either omit it or normalize it in a separate, approved authorization design; do not treat object IDs as the proposed ADR 0009 contract.

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
