# Platform secret templates

The `*.sops.yaml.example` files document the shape of every Secret the platform needs. The REAL files live per cluster in `clusters/<env>/secrets/*.sops.yaml` (SOPS-encrypted, committed — Flux applies them from git): generate a full consistent set with `scripts/gen-secrets.sh <server_name> <env>`.

Rotate one coherent class with `scripts/rotate-secrets.sh <server_name> <env> <secret-set>` rather than regenerating everything with `gen-secrets.sh --force`. The rotation command validates every output before changing ciphertext, refuses dirty targets, and never reconciles the cluster. The ordered CNPG/appservice/workload procedure and blast-radius table live in the [Matrix operator runbook](../../.agents/skills/matrix-agents/SKILL.md#runbook-rotate-secrets).

`mcp-authorization.sops.yaml` keeps platform-helper's two copies of its narrow MCP credential together: agentgateway validates the raw key and attaches authenticated `apiKey.agent` metadata, while kagent resolves the matching `Bearer` value into only that Agent's tool configuration. Rotate them atomically with the `mcp` secret set; this credential cannot authorize A2A or model traffic.

`keycloak-db.sops.yaml` keeps the optional IdP's two copies of `pg-keycloak` together so they rotate atomically. `keycloak-bootstrap.sops.yaml` holds the one-time admin, OIDC-client, and demo-user values and is preserved even with `--force`: Keycloak skips startup import when the realm exists, so silently regenerating these values would drift from its database. Rotate live realm credentials with the Admin API and update the encrypted bootstrap file deliberately. The realm JSON contains only environment placeholders; no credential is stored in the ConfigMap.

External bridges are absent from the normal `all`/`rotatable` sets. Select them explicitly only when the matching cluster component is approved:

| Network  | Initial generation                                                                                                                  | Rotation                                              | Preserved identity/provider material                                                              |
| -------- | ----------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------- | ------------------------------------------------------------------------------------------------- |
| Slack    | `FGENTIC_SECRET_SET=slack mise exec -- scripts/gen-secrets.sh <server_name> <env>`                                                  | `mise exec -- scripts/rotate-secrets.sh … … slack`    | Generated appservice `sender_localpart`; Slack `xoxb`/`xapp` login state remains in the scoped DB |
| Telegram | `TELEGRAM_API_ID=<id> TELEGRAM_API_HASH=<hash> FGENTIC_SECRET_SET=telegram mise exec -- scripts/gen-secrets.sh <server_name> <env>` | `mise exec -- scripts/rotate-secrets.sh … … telegram` | Generated appservice `sender_localpart` and operator-owned Telegram API ID/hash                   |

Each optional rotation changes the network's scoped database password plus AS/HS tokens as one ciphertext transaction. It preserves the appservice sender identity; Telegram also preserves the API pair. Reconcile `platform-secrets`, wait for the CNPG role, restart Synapse so it reloads the matching registration, then restart only that bridge StatefulSet. Runtime user/app sessions are provider credentials in the bridge database and are not copied into SOPS.

Rotation is not offboarding. Before deleting an optional bridge ciphertext, replace `../../infra/bridges/<network>/cluster` with its sibling `cluster-offboard` component and verify CNPG has applied `NOLOGIN`, cleared the role password, and removed the network's HBA pair. Query only booleans and never print `rolpassword`. Remove the temporary component and ciphertext only in a second reconciliation; deleting a Secret does not revoke a database login.

The generator reads `llm_provider` from that environment's `platform-settings.yaml`. Vertex uses ambient GCP credentials (or the cluster-only local ADC helper), while self-hosted vLLM is cluster-internal; neither needs a model API-key Secret. API providers require exactly one matching environment variable and produce one provider-scoped Secret. Generation also rewrites the cluster Secret directory's `kustomization.yaml` with every encrypted filename; review and commit that inventory with the ciphertext so Flux reconciles newly added provider and optional-network Secrets.

| Profile        | Environment variable   | Encrypted output                      | Kubernetes Secret     |
| -------------- | ---------------------- | ------------------------------------- | --------------------- |
| `mistral`      | `MISTRAL_API_KEY`      | `agentgateway-mistral.sops.yaml`      | `mistral-secret`      |
| `anthropic`    | `ANTHROPIC_API_KEY`    | `agentgateway-anthropic.sops.yaml`    | `anthropic-secret`    |
| `openai`       | `OPENAI_API_KEY`       | `agentgateway-openai.sops.yaml`       | `openai-secret`       |
| `azure-openai` | `AZURE_OPENAI_API_KEY` | `agentgateway-azure-openai.sops.yaml` | `azure-openai-secret` |

All four Secrets use the literal data key `Authorization`, as required by agentgateway v1.3.1. Pass raw API keys without a `Bearer` prefix. The generator never deletes a previous provider's encrypted Secret when switching profiles; remove obsolete ciphertext deliberately after the new profile is verified.
