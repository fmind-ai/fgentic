---
name: sops-secrets
description: Manage Fgentic's SOPS-age encrypted secrets — generate the per-cluster set, edit, rotate, and the local ADC/CA helper scripts. Use for anything touching clusters/<env>/secrets/, credentials, tokens, or TLS material.
metadata:
  author: Médéric Hurier (Fmind)
  created: 2026-07-11
---

# SOPS Secrets

Only SOPS-age ciphertext ever exists in the tree; Flux (kustomize-controller) decrypts in-cluster via the `sops-age` Secret in `flux-system`. A gitleaks pre-commit hook and `.gitignore` patterns (`*.dec.yaml`, `*.agekey`, …) are the backstops — **never commit plaintext, and never write a decrypted secret to disk outside the sops workflow**.

## Layout

1. Real encrypted secrets: `clusters/<env>/secrets/*.sops.yaml`, applied from git by the `platform-secrets` Kustomization. Each cluster owns its own set (registration regexes embed the server_name; credentials never span environments).
1. `infra/secrets/` holds `*.sops.yaml.example` **templates only** — never real values.
1. SOPS is not operational corpus storage. Keep source bytes out of this GitOps/deployment repository and provision the operator-owned `knowledge-source-bundle` PVC separately under the deployment's storage policy. A separately governed source Git repository may be input to the #335 connector.
1. Age recipient comes from the repo `.sops.yaml`; the private key lives at `~/.config/sops/age/keys.txt` (and in-cluster as `sops-age`).

## Generate / edit / rotate

1. **Generate the core set**: `scripts/gen-secrets.sh <server_name> <local|gcp>` — fresh core Postgres role passwords plus the knowledge owner/retrieval roles, Keycloak bootstrap/demo/client credentials, appservice `as_token`/`hs_token`, bridge A2A and platform-helper MCP workload credentials, derived connection URLs, and exactly the API-key Secret selected by `platform-settings.data.llm_provider`. Vertex and self-hosted vLLM emit no model API-key file. Optional layers are deliberately excluded: generate sovereign ingestion with `FGENTIC_SECRET_SET=knowledge-ingestion`, Slack with `FGENTIC_SECRET_SET=slack`, or Telegram with `TELEGRAM_API_ID=<id> TELEGRAM_API_HASH=<hash> FGENTIC_SECRET_SET=telegram`. Skips existing files unless `--force`; the selected API profile's environment variable is required only when its encrypted file must be created. The one-time `keycloak-bootstrap.sops.yaml` and explicitly generated optional sets are preserved by normal `--force` generation. Plaintext is piped directly into SOPS and ciphertext is moved into place atomically. Commit the result — Flux applies from git.
1. **Edit one value**: `sops clusters/<env>/secrets/<file>.sops.yaml` (opens decrypted in `$EDITOR`, re-encrypts on save). Never decrypt to a file.
1. **Rotate one coherent class**: `scripts/rotate-secrets.sh <server_name> <local|gcp> <secret-set>`. Supported sets are `appservice`, `a2a`, `mcp`, `db-synapse`, `db-mas`, `db-bridge`, `db-kagent`, `db-core`, `db-knowledge-owner`, `db-knowledge-retrieval`, `knowledge-db`, `knowledge-ingestion`, `provider`, `keycloak-db`, `slack`, `telegram`, `keycloak-client`, and `all`. The command fails before mutation when a target is missing, dirty, undecryptable, or missing its required external key; stages and validates all ciphertext before replacement; and never touches the cluster. Optional bridge rotation preserves the generated appservice sender identity; Telegram also preserves its API ID/hash. Commit/push, reconcile `platform-secrets`, then follow the exact CNPG/restart order in [matrix-agents](../matrix-agents/SKILL.md#runbook-rotate-secrets).
1. **Respect bootstrap boundaries**: `all` excludes optional ingestion and both optional network sets, `keycloak-client`, and the Keycloak bootstrap admin/demo identities. Rotate optional sets by name. Rotate the live Keycloak OIDC client first, then run the explicit `keycloak-client` set with `FGENTIC_CLIENT_SECRET` and `KEYCLOAK_CLIENT_UPDATED=yes`; the script updates only the matching Keycloak-recovery and MAS values. Startup import never overwrites a live realm.
1. **Bootstrap the decryption key** (once per cluster): `kubectl -n flux-system create secret generic sops-age --from-file=age.agekey="$HOME/.config/sops/age/keys.txt"`.

## Out-of-git local helpers (k3d)

1. `scripts/local-ca.sh` — creates + trusts the `local-ca` CA and its cert-manager Secret (`*.localhost` is not ACME-issuable; ESS bakes https URLs, so local runs terminate real TLS on loopback 80/443).
1. `scripts/local-adc.sh` — creates the `gcp-adc` Secret for agentgateway's Vertex AI auth (no ambient Workload Identity on k3d). Both produce cluster-only material, deliberately kept out of git.

## Invariants

1. The appservice `as_token`/`hs_token` must be **identical** in the bridge and Synapse — one SOPS Secret, referenced from both namespaces; never fork them.
1. The A2A workload key must be **identical** in `agentgateway-system/a2a-bridge-callers` and `bridge/a2a-bridge-credential`. Rotate both with the `a2a` set; agentgateway authenticates the workload while `X-User-Id` remains end-user attribution only.
1. The platform-helper MCP key must be **identical** in `agentgateway-system/mcp-agent-callers` and the `Bearer` value in `kagent/platform-helper-mcp-credential`. Rotate both with the `mcp` set; agentgateway authenticates this one Agent and authorizes only its five declared tools. It is not a model credential or non-exportable pod identity.
1. Each optional mautrix bridge keeps its scoped database password, runtime AS/HS tokens, and Matrix registration in one network-specific SOPS file. Runtime tokens must match the registration; rotation preserves its generated `sender_localpart`. Slack `xoxb`/`xapp` values are runtime database state, not deployment Secrets. Telegram's API ID/hash are deployment inputs but remain unchanged during rotation.
1. The two `pg-keycloak` Secrets must carry the **same** password: CNPG reads the `postgres` copy to manage the role, while the chart reads the `keycloak` copy. They live together in `keycloak-db.sops.yaml` so existing clusters can add the optional layer without rotating established roles.
1. `knowledge-db.sops.yaml` keeps `pg-knowledge-owner` only in `postgres`; no workload receives schema-owner credentials. Its `pg-knowledge-retrieval` Secrets in `postgres` and `knowledge` must carry the **same** username and password so CNPG and the retrieval consumer converge atomically.
1. `knowledge-ingestion.sops.yaml` keeps the DML-only `pg-knowledge-ingestion` copies in `postgres` and `knowledge` identical, and independently keeps the gateway caller key identical between `agentgateway-system/knowledge-ingestion-callers` and the `Bearer` value in `knowledge/knowledge-ingestion-credential`. It contains neither the schema-owner password nor a model/provider credential.
1. `keycloak-bootstrap.sops.yaml` carries both `keycloak/keycloak-credentials` and `matrix/mas-upstream-oidc`; their OIDC client secrets must be **identical**. The file is bootstrap-only and never rotates under `--force`, because Keycloak ignores an import for an existing realm while MAS would immediately adopt a changed secret.
1. No agent or workload ever holds a model credential — only agentgateway does. API profiles use provider-scoped Secrets (`mistral-secret`, `anthropic-secret`, `openai-secret`, or `azure-openai-secret`); Vertex uses Workload Identity or the local `gcp-adc` helper and requires no Git-tracked model Secret.
1. Report suspected leaked credentials privately per [SECURITY.md](../../../SECURITY.md); rotate first, discuss second.
