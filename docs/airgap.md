---
type: Runbook
title: Air-gapped & Registry-Mirrored Installation
description: The disconnected-install procedure — populate a private OCI mirror from the release BOM, redirect pulls at the node, reconcile Flux, and the residual egress that remains.
---

# Air-gapped & Registry-Mirrored Installation

Sovereign, regulated, and public-sector operators routinely require installation from an internal OCI registry with no (or allowlisted-only) internet egress. This runbook describes how to mirror the reference release profile into a private registry and reconcile a cluster from that mirror. It composes existing tools (skopeo, oras/helm, and the adopter's Harbor/zot/Artifactory registry); Fgentic builds no registry and runs no mirror daemon.

Scope of what is decided today versus what remains:

1. **Decided and implemented** — the machine-readable [Bill of Materials](../release/bom.yaml) (`release/bom.yaml`), and the BOM-driven mirror-population script [`scripts/mirror-artifacts.sh`](../scripts/mirror-artifacts.sh).
1. **Recommended but not yet wired (the consumption seam, issue #457 Task 2)** — how workloads pull from the mirror. This runbook documents the **node-level containerd registry mirror** as the recommended approach and marks the wiring as the remaining decision.
1. **Not yet runtime-proven (issue #457 Task 4)** — a full reconcile of the platform on a cluster with upstream registry egress blocked. No air-gap install has been executed end-to-end; do not read this runbook as a proof of one.

## 1. Populate the mirror from the BOM

The [release BOM](releases.md) is the single source of truth for what a tag deploys: every chart source, HelmRelease chart version, and digest-pinned image of the reference profile. [`scripts/mirror-artifacts.sh`](../scripts/mirror-artifacts.sh) reads it and copies each artifact into a target registry, **preserving image digests** so the mirror is byte-for-byte identical to upstream (air-gap integrity requires the same digest — [design decision D13](design-decisions.md)).

```bash
# 1. Preview the copy plan (dry-run is the default; it performs NO push).
scripts/mirror-artifacts.sh registry.internal:5000

# 2. Execute the copies once the plan looks right.
MIRROR_APPLY=yes scripts/mirror-artifacts.sh registry.internal:5000
```

Behavior:

1. **OCI images and OCI charts** are copied with `skopeo copy` by digest (or by tag when the source has no digest), under a target path namespaced by the origin registry host — `registry.internal:5000/<origin-host>/<repo>@<digest>` — which is collision-free across registries and keeps the node-level mirror rewrite (below) transparent.
1. **Classic HTTP Helm repositories** are pulled with `helm pull --version` and re-pushed to the mirror's OCI path (`oci://registry.internal:5000/charts`). A classic repository carries no upstream OCI digest, so the **version pin** is the preserved identity for those charts, not a digest.
1. **Idempotent** — an artifact already present in the mirror is a success, not an error (the script probes with `skopeo inspect` and skips). **Fail-closed** — a genuine copy failure aborts non-zero with wrapped context.
1. **Ambient credentials** — the script never takes or prints a registry password; authenticate skopeo/helm to the target registry out of band (`skopeo login`, `helm registry login`) before running with `MIRROR_APPLY=yes`.

Re-running the script with a newer tag's BOM is the entire mirrored-upgrade flow — the same pattern the [release contract](releases.md) and the [Day-2 handbook upgrade section](operations-handbook.md) describe for connected clusters.

### What the script cannot mirror from the BOM alone

The script never reports a complete mirror while silently under-copying. The BOM generator ([`scripts/gen-bom.sh`](../scripts/gen-bom.sh)) now fully qualifies every image whose repository can be reconstructed from the HelmRelease manifests (recording `resolved: true` with a complete `registry/repository:tag@sha256:…` reference — this fixed the trivy-operator, trivy, and bridge images), so most images mirror directly. The remainder are marked `resolved: false` with a `note:` in the BOM; the script reports each as a residual and exits non-zero.

On the current reference BOM there are exactly **2 residual chart-default images** — their repository is a chart default that is not present in the repo manifests, so it cannot be recorded without a `helm template` render:

1. `0.2.1@sha256:50b4…` — the kagent-tools image (`infra/kagent/helmrelease.yaml`; the values set only `tag`).
1. `2.19.0` / `sha256:ede4…` — the jaeger image (`infra/observability/tracing-helmreleases.yaml`; the values set `tag` and `digest` but no `repository`).

Enumerate these two by rendering the chart (`helm template` of the pinned chart at its pinned values, the same render `check:admission-policies` exercises) to discover the default repository, then `skopeo copy` each into the mirror by digest, or rely on the node-level containerd mirror (below) to redirect the chart's default registry. Recording full repositories for these in the BOM is the remaining `helm template`-based enumeration work on the [Task-1 inventory](releases.md#the-bill-of-materials), tracked with issue #457.

Two further residuals are handled outside the image mirror and are **not** counted as failures:

1. **The `trivy-operator` chart's `GitRepository` source** is a git commit, not a registry artifact; the script reports it as an `INFO` line, and you mirror the git repository to an internal Git remote and point Flux's source there (step 3).
1. **The vLLM model snapshot.** The reference profile selects the Vertex provider, so the model weights are out of BOM scope ([`infra/models` is excluded](releases.md#what-the-bom-deliberately-excludes)); a `vllm`-profile air-gap must additionally mirror the pinned model snapshot and the vLLM chart, which this BOM does not enumerate.

Treat the script's non-zero exit and its `UNRESOLVED:` lines as the authoritative, itemized list of what still needs a hand — never as a failure to ignore.

## 2. Redirect pulls at the node (the consumption seam — Task 2, remaining)

Populating the mirror is not enough; the cluster must _pull_ from it. The recommended approach is a **node-level containerd registry mirror**, which is transparent to every manifest — no image reference or chart URL in the repository is rewritten.

For k3d/k3s, provide a `registries.yaml` that redirects the public registries to the private mirror, using the origin-host-namespaced layout the script writes:

```yaml
# /etc/rancher/k3s/registries.yaml  (k3d: --registry-config)
mirrors:
  docker.io:
    endpoint: ["https://registry.internal:5000/docker.io"]
  ghcr.io:
    endpoint: ["https://registry.internal:5000/ghcr.io"]
  quay.io:
    endpoint: ["https://registry.internal:5000/quay.io"]
  registry.k8s.io:
    endpoint: ["https://registry.internal:5000/registry.k8s.io"]
  cr.agentgateway.dev:
    endpoint: ["https://registry.internal:5000/cr.agentgateway.dev"]
configs:
  registry.internal:5000:
    auth:
      username: <mirror-user>
      password: <mirror-token>
    tls:
      ca_file: /etc/ssl/certs/mirror-ca.crt
```

> **This wiring is the remaining Task-2 decision.** The exact seam is a genuine architectural choice and is **not merged**. The alternative to the node-level mirror is a repo-wide `${registry_mirror}` Flux post-build substitution that rewrites every image registry and chart source URL where the pinned charts parameterize it (the [`infra/trivy-operator`](../infra/trivy-operator/helmrelease.yaml) `mirror.gcr.io` values are the existing per-component precedent). The node-level mirror is preferred because it is manifest-transparent and does not fork the pin-set, but a chart that hardcodes a registry may still need the substitution fallback. Choosing and wiring one path — and mapping the classic-HTTP charts pushed to `oci://…/charts` onto their Flux `HelmRepository` `type: oci` sources — is the outstanding work.

## 3. Point Flux at the mirror and internal Git, then reconcile

1. **Git source.** An air-gapped cluster cannot reach `github.com`. Mirror this repository (and the `trivy-operator` chart repository) to an internal Git remote and point the Flux `GitRepository` at it, tracking a release **tag** — never `main` ([release contract](releases.md#recommended-flux-flow-track-tags-never-main)).
1. **Chart sources.** With the node-level mirror in place, the OCI `OCIRepository`/`HelmRepository` sources resolve through the redirect. Classic HTTP `HelmRepository` sources must be repointed at the mirror's `oci://…/charts` path (the Task-2 mapping above).
1. **Secrets and settings.** Generate SOPS secrets and `platform-settings.yaml` as in [Production Installation](production.md); set the private-registry CA trust and any mirror pull credentials.
1. **Reconcile and verify.** Bootstrap Flux and run the seeded acceptance (`@mention` → A2A → reply). Capturing this on a cluster with upstream egress blocked is the [Task-4 proof](#4-residual-egress-and-the-remaining-proof) that remains.

## 4. Residual egress and the remaining proof

A mirrored install removes registry and chart egress, but some destinations may still be reached depending on the chosen profile. List them honestly for the deployment's threat model; **no silent internet dependency is acceptable**:

| Destination          | When it applies                                         | How to close it                                                                                                                                                                                                              |
| -------------------- | ------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| ACME / Let's Encrypt | Public TLS via cert-manager (`chat.`/`matrix.`/… hosts) | Use an internal CA / the local-CA issuer instead of the ACME issuer ([forking §4](forking.md)); air-gap requires an internal PKI.                                                                                            |
| Model backend        | `llm_provider=vertex` (default) reaches Vertex AI       | Select the self-hosted `vllm` profile so prompts stay in-cluster after the model bootstrap ([models.md](models.md)); then mirror the model snapshot (see [§1 residuals](#what-the-script-cannot-mirror-from-the-bom-alone)). |
| Hugging Face         | `vllm` profile bootstraps model weights                 | Mirror the pinned model snapshot into the private registry/object store before install; the reference BOM does not yet enumerate it.                                                                                         |
| DNS / NTP            | Always                                                  | Provide internal resolvers and time sources; the platform assumes name resolution and clock sync.                                                                                                                            |
| Git remote           | Flux source and `trivy-operator` chart                  | Mirror to an internal Git remote (step 3).                                                                                                                                                                                   |

**Task-4 runtime proof is deferred.** Reconciling every layer of the demo/local profile on a cluster with upstream registry egress denied — via a NetworkPolicy or a k3d mirror-only config — and passing the seeded acceptance is the runtime remainder of issue #457. It has not been executed; this runbook is the procedure, not its evidence.

## Related

- [Adopter Release & Upgrade Contract](releases.md) — the BOM this flow consumes and the tag-tracking model.
- [Production Installation](production.md) — the connected reference install this diverges from.
- [Day-2 Operations Handbook](operations-handbook.md) — mirrored upgrades follow the same BOM re-run flow.
- [Forking & Adapting](forking.md) — registry, domain, and CA changes for running under your own org.
