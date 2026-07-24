---
type: Runbook
title: Unreleased Upgrade Notes
description: Accumulating operator-facing migration notes for the next Fgentic release.
---

# Unreleased Upgrade Notes

Accumulate operator-facing changes here as they land on `main`. When a release is cut, rename this file to `docs/upgrades/<version>.md`, finalize the entries against the [template](TEMPLATE.md), update the `docs/mkdocs.yml` navigation, and start a fresh `unreleased.md`. Empty-but-present is acceptable.

## Summary

The next release automatically migrates every enabled self-hosted model-cache PVC to the revision-isolated [`snapshot-v2` publication contract](../models.md#self-hosted-vllm) during reconciliation. No configuration, secret, or pre-upgrade action is required.

## Config and values migrations

- None.

## SOPS and secret changes

- None.

## Manual steps

- No pre-upgrade action. After reconciliation, wait for each enabled one-shot model loader Job to succeed and its serving Pod to become Ready before judging model availability. Do not delete the retained cache PVC or rewrite its `.ready` marker.

## Rollback

- Pin the previous release tag in the Flux `GitRepository` and let Flux reconcile. A pre-`snapshot-v2` loader does not recognize the new readiness marker and can re-download the configured revision before its serving Pod becomes Ready; keep the loader's existing prompt-free public HTTPS path available and let that Job finish.

## BOM delta

- See [`release/bom.yaml`](../../release/bom.yaml) for the authoritative pin-set.
