---
type: Template
title: Upgrade Note Template
description: The per-release upgrade-note structure an operator follows to move between two tags.
---

# Upgrade Note Template

Copy this file to `docs/upgrades/<version>.md` (for example `docs/upgrades/0.2.0.md`) when cutting a release, and add it to the `docs/mkdocs.yml` navigation. Fill in only the sections that apply; an **empty-but-present** note is acceptable when a release needs no manual action — state that explicitly rather than omitting the file. Each note describes moving **to** its version from the immediately preceding release (see the tested-path caveat in the [support statement](../stability.md#support-and-tested-upgrade-paths)).

## Summary

One sentence: what this release changes for an operator, and whether any manual action is required before or after reconciling the new tag.

## Config and values migrations

- Changed `platform-settings` keys, HelmRelease values, or Kustomize overlays an operator must edit in `clusters/<env>/`.
- New required settings and their defaults; removed or renamed settings.
- State "None." when the pin bump is transparent.

## SOPS and secret changes

- New secrets to generate (`scripts/gen-secrets.sh`), rotated keys, or changed secret shapes.
- Any re-encryption or recipient change.
- State "None." when unchanged.

## Manual steps

- Ordered actions outside `git`-driven Flux reconciliation (for example a one-time CRD apply, a database migration to observe, or a controlled drain).
- Note anything that must happen **before** updating the `GitRepository` tag versus **after** reconciliation.
- State "None." when the upgrade is reconcile-only.

## Rollback

- How to revert: pin the previous tag, plus any manual undo for migrations that are not automatically reversible.

## BOM delta

- Notable chart or image pin changes in [`release/bom.yaml`](../../release/bom.yaml) since the previous release (operator-relevant highlights; the BOM itself is the authoritative diff).
