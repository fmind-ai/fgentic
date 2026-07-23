---
type: Contract
title: Adopter Release & Upgrade Contract
description: What a Fgentic release is for an adopter — a tag is a tested pin-set with a machine-readable BOM, upgrade notes, and a Flux tag-tracking flow.
---

# Adopter Release & Upgrade Contract

Fgentic is consumed by pointing Flux at this repository. Without a release contract that means tracking `main` with no tested boundary. This document defines the distribution semantics an operator needs: a **tag is a tested pin-set**, described by a machine-readable Bill of Materials, shipped with upgrade notes, and tracked through a Flux `GitRepository` that follows tags rather than a moving branch.

## What a release is

A release is a git **tag** (`vMAJOR.MINOR.PATCH`) that marks a specific, reviewed revision of `infra/` and `apps/` as a coherent pin-set. Three artifacts define it:

1. **The tag** — the immutable git revision an adopter reconciles.
1. **The BOM** ([`release/bom.yaml`](../release/bom.yaml)) — every pinned chart source, HelmRelease chart version, and digest-pinned container image reconciled by the reference release profile at that revision.
1. **The upgrade note** ([`docs/upgrades/<version>.md`](upgrades/TEMPLATE.md)) — the config/values migrations, SOPS/secret changes, and manual steps to move to that version. Empty-but-present is acceptable when a release needs no manual action.

The tag is the unit an adopter pins; the BOM makes the pin-set auditable for supply-chain and air-gap review; the upgrade note makes the move between two tags followable.

## The Bill of Materials

[`release/bom.yaml`](../release/bom.yaml) is **generated** by [`scripts/gen-bom.sh`](../scripts/gen-bom.sh) and **verified** by `mise run check:bom` ([`scripts/check-bom.sh`](../scripts/check-bom.sh)); do not edit it by hand. It is deterministic — every list is sorted and no generation timestamp is embedded — so the verify gate can regenerate it and diff.

### Scope

The BOM enumerates the **reference release profile**: the reconciled `clusters/local` + `clusters/gcp` Flux DAG under `infra/` plus the `apps/matrix-a2a-bridge/deploy` unit, with the tracked default `platform-settings` (`llm_provider=vertex`; the admin, synthetic-canary, alert-delivery, knowledge-ingestion, and embeddings profiles disabled; no external mautrix bridge composed). It records:

- `chartSources` — pinned `OCIRepository`/`GitRepository` chart artifacts (OCI tag or digest, immutable git commit).
- `helmReleases` — each HelmRelease with its chart, resolved version, and source.
- `helmRepositories` — the chart hosts (a classic Helm repo carries no in-file version pin; the version lives on the HelmRelease).
- `images` — every container image pinned by an inline `tag@sha256:` reference. Each entry is keyed by its `sha256:` digest, the globally-unique, registry-independent supply-chain identifier. Images configured as chart values may record only the `tag@sha256:` portion because the repository is a chart default; the digest is always complete.
- `taggedImages` — every image pinned by an explicit `image:` tag override in a reconciled HelmRelease's values (for example the OpenTelemetry, Jaeger, and Keycloak images), captured as registry/repository/tag plus the sibling digest when present. These would otherwise be invisible to an air-gap adopter mirroring strictly from the BOM; the verify gate re-derives and enforces them fail-closed exactly like the digest images.

### What the BOM deliberately excludes

Optional and disabled-by-default surfaces are **not** silently dropped — every excluded path is in the committed allowlist in [`scripts/lib/bom-scope.sh`](../scripts/lib/bom-scope.sh) with a one-line reason. The excluded classes are: test fixtures (`*_test.go`, integration Dockerfiles, app test scripts), disposable labs (`clusters/demo/*`, `infra/federation/*`), disabled-by-default profiles (admin, canary, alert-delivery, knowledge, embeddings, external bridges, self-hosted models), the opt-in `matrix-group-sync` and `activitypub-agent-gateway` units, and governance metadata such as the MCP catalog and surface pin. A deployment that enables one of those profiles must extend the BOM scope before it can claim the same pin-set guarantee.

### Completeness is enforced, fail-closed

`check:bom` re-derives the pin census from the repository and fails when:

1. The committed BOM differs from `gen-bom.sh`'s current output (drift from reality).
1. Any in-scope image digest is missing from the BOM.
1. Any pin-bearing file is **neither** in-scope **nor** covered by the exclusion allowlist — so a new chart or image added later that nobody classified fails the gate.

This is the point of the artifact: an incomplete BOM that silently under-covers would give false air-gap and supply-chain confidence. Regenerate and re-verify after any pin change:

```bash
bash scripts/gen-bom.sh
mise run check:bom
```

## Recommended Flux flow: track tags, never `main`

Adopters reconcile a **tag**, not `main`. `main` receives fast-forward digest-pin commits from CD and unreleased work; tracking it means reconciling untested pin-sets. Point the Flux `GitRepository` at a tag (or a semver range) so an upgrade is a reviewed, deliberate bump:

```yaml
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata:
  name: fgentic
  namespace: flux-system
spec:
  interval: 30m
  url: https://github.com/fmind-ai/fgentic.git
  ref:
    tag: v0.2.0 # pin the tested pin-set; bump deliberately after reading the upgrade note
    # Or a bounded range to auto-adopt patches within a minor line:
    # semver: ">=0.2.0 <0.3.0"
```

Upgrading is then: read [`docs/upgrades/<next>.md`](upgrades/TEMPLATE.md), apply any listed migrations, update the `ref.tag`, and let Flux reconcile. See the [Day-2 Operations Handbook](operations-handbook.md) for the reconciliation and rollback procedure.

## Support statement

The tested upgrade paths and the single-maintainer reality are stated honestly in the [Public Surface Stability Contract](stability.md#support-and-tested-upgrade-paths). Do not assume an untested tag-to-tag jump is supported.

## Status and deferred steps

The offline core of this contract — the BOM, its fail-closed verify gate, the upgrade-notes convention, and this documentation — is in place. Two steps remain and are **not** covered by the offline core:

1. **Live tag-to-tag upgrade drill** (runtime): reconciling a running local cluster from one tag to the next following only the notes. The offline gate proves the pin-set is complete and internally consistent; it cannot prove a live upgrade succeeds.
1. **Cutting `v0.2.0` under the maintainer's identity** (human gate): wiring this contract into the release process (issue #7) and publishing the first tag shipped under it. Releases are published under the maintainer's name.
