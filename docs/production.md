# Production Installation

The production path reconciles a reviewed git revision through Flux, decrypts per-cluster SOPS secrets in-cluster, enables SSO and observability, and keeps the canonical HelmRelease values under `infra/` and `apps/`. It is intentionally different from the disposable [evaluation installer](../README.md#evaluate-in-15-minutes).

## Choose the model boundary

Choose where prompts and responses may travel before generating secrets. The exact settings, credential names, network paths, and acceptance gates are in [models.md](models.md).

| Tier | Profiles                        | Boundary                                                                                                                  |
| ---- | ------------------------------- | ------------------------------------------------------------------------------------------------------------------------- |
| 1    | `vllm`                          | Self-hosted serving; prompts stay in the cluster after the pinned model bootstrap, subject to verified NetworkPolicy      |
| 2    | `mistral`                       | EU API endpoint; contract, subprocessors, retention, and billing remain account controls                                  |
| 3    | `vertex`, `anthropic`, `openai` | Hyperscaler boundary; region/residency and retention depend on provider and account configuration                         |
| 3    | `azure-openai`                  | Azure resource boundary; select Regional or EU Data Zone rather than Global when geography must be constrained            |
| —    | `demo` evaluation fixture       | Not a language model and not supported by the `local` or `gcp` production overlays; never use it for a production install |

Set `llm_provider` and `llm_model` in `clusters/<env>/platform-settings.yaml`. API profiles require their documented environment variable when secrets are generated. Vertex uses Workload Identity on GKE and a cluster-only ADC Secret on k3d.

## Prerequisites

- A conformant Kubernetes cluster, or Docker for the local k3d reference.
- Git, [mise](https://mise.jdx.dev/), and the repository checkout. `mise install` installs the pinned `kubectl`, Flux, k3d, SOPS, age, Helm, and validation tools.
- A writable GitHub repository and token for the current `flux bootstrap github` reference workflow. Flux itself is provider-neutral; adapt its source bootstrap when using another Git host.
- An age private key whose recipient matches [.sops.yaml](../.sops.yaml).
- Provider credentials only for the selected model profile.

For the optional GKE reference, apply [`infra/terraform/bootstrap/`](../infra/terraform/bootstrap/) first to create the versioned state bucket, migrate the main Terraform state, review the plan and cost, and apply [`infra/terraform/`](../infra/terraform/) only with maintainer approval. The workloads remain portable Kubernetes manifests.

## Bootstrap order

1. Install the pinned tools and hooks:

   ```bash
   mise install
   mise run install:hooks
   ```

1. Create or select the cluster. For the local reference:

   ```bash
   mise run cluster:up
   ```

1. Install the pinned experimental Gateway API v1.4.0 CRDs. This is the only out-of-band CRD bundle:

   ```bash
   kubectl apply --server-side \
     -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.4.0/experimental-install.yaml
   ```

1. Set the chosen `llm_provider` and `llm_model` in `clusters/<env>/platform-settings.yaml`, export the selected API key if applicable, and generate the complete encrypted secret set:

   ```bash
   scripts/gen-secrets.sh <server_name> <local|gcp>
   ```

   Review the SOPS resources under `clusters/<env>/secrets/`, then commit and push them. Never commit plaintext credentials. The generator keeps the Matrix appservice tokens identical across the Matrix and bridge namespaces and scopes each database role to one service.

1. Install the decryption key in the cluster. Create the namespace first only when Flux bootstrap has not done so:

   ```bash
   kubectl get namespace flux-system >/dev/null 2>&1 || kubectl create namespace flux-system
   kubectl -n flux-system create secret generic sops-age \
     --from-file=age.agekey="$HOME/.config/sops/age/keys.txt"
   ```

1. For local k3d, create the local CA Secret and follow the printed host-trust instruction:

   ```bash
   scripts/local-ca.sh
   ```

1. For local Vertex only, create its cluster-only ADC Secret. API profiles use their generated SOPS Secret; vLLM uses no model credential:

   ```bash
   scripts/local-adc.sh <gcp-project>
   ```

1. Bootstrap Flux against the reviewed repository and cluster overlay:

   ```bash
   flux bootstrap github \
     --owner=<owner> \
     --repository=fgentic \
     --path=clusters/<local|gcp>
   ```

1. Local k3d cannot pull the private development bridge image, so build and side-load it. Production CD publishes, signs, and digest-pins the image instead:

   ```bash
   mise run bridge:load
   ```

Flux reconciles the DAG in dependency order: namespaces and secrets; controllers and observability; gateway, Postgres, and agentgateway; Matrix, Keycloak, kagent, and monitors; then the bridge. Inspect `flux get kustomizations` and `flux get helmreleases -A`; debug the first non-Ready layer instead of applying a workload around Flux.

## Provision the administrator and room

Run the supported interactive bootstrap after every layer is Ready:

```bash
scripts/bootstrap-admin.sh --server-name <server_name>
```

Open the one-time URL and authenticate as the IdP user whose immutable `matrix_localpart` is `alice`. The device grant provisions the exact Matrix ID, grants Synapse administrator access, and idempotently creates `#fgentic-demo:<server_name>` without storing a token or entering a pod. In Element at `https://chat.<server_name>`, send `!agents`, invite an allowed ghost, and mention it. Grafana is at `https://grafana.<server_name>`.

The complete identity contract is in [identity.md](identity.md); model runtime checks are in [models.md](models.md); attribution verification is in [audit.md](audit.md); secret rotation and mention-to-reply diagnostics are in the [matrix-agents runbook](../.agents/skills/matrix-agents/SKILL.md).

Slack and Telegram are optional external identity/data boundaries, not production prerequisites. Enable them only through the composable cluster components and provider gates in [external-network interop](interop.md); the default local and GCP overlays reconcile neither bridge. Their standard NetworkPolicy permits arbitrary non-private IPv4 TCP/443 for provider transports, so deployments requiring provider-FQDN enforcement must add a governed egress proxy or FQDN-aware CNI before acceptance.

## Production gates

1. Run `mise run check` and `mise run test` warning-free before reconciling a revision.
1. Review [security.md](security.md), including prompt-injection boundaries, A2A workload authorization, network-policy enforcement, secret handling, and supply-chain verification.
1. Prove NetworkPolicy on GKE Dataplane V2 or another known-enforcing engine; the constrained local k3d host is intent-only when its kube-router probe fails.
1. Confirm selected-provider retention, residency, billing cap, and low-token runtime acceptance. Static rendering is not runtime evidence.
1. Configure DNS and valid TLS for the apex plus `chat.`, `matrix.`, `auth.`, `id.`, and `grafana.` hosts. The GKE Terraform output provides the reserved ingress address.
1. Review CNPG backups and complete a restore drill. The local overlay intentionally strips GCS backup configuration.
1. Verify signed, digest-pinned bridge artifacts and collect one end-to-end attribution bundle before declaring the deployment ready.

## Why evaluation still embeds Flux

The source of Helm values is the set of Flux `HelmRelease` resources. Applying those CRs to a cluster without helm-controller does not install their charts, while independently translating each one into `helm install` commands would create a second renderer and invite value drift. The evaluation command therefore installs local Flux controllers and reconciles an ephemeral cluster-local Git snapshot of the checkout. It needs no GitHub account, commit, push, SOPS key, or checkout mutation, while production and evaluation consume the same HelmReleases.

Evaluation deliberately diverges in lifecycle and hardening: its secrets are cluster-only, its Git source disappears with the cluster, it omits Keycloak and observability, and its default provider is a deterministic response stub. `mise run demo:down` deletes only `fgentic-demo`; production teardown follows the chosen infrastructure provider's reviewed, approval-gated process.
