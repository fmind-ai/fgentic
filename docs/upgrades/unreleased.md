---
type: Runbook
title: Unreleased Upgrade Notes
description: Accumulating operator-facing migration notes for the next Fgentic release.
---

# Unreleased Upgrade Notes

Accumulate operator-facing changes here as they land on `main`. When a release is cut, rename this file to `docs/upgrades/<version>.md`, finalize the entries against the [template](TEMPLATE.md), update the `docs/mkdocs.yml` navigation, and start a fresh `unreleased.md`. Empty-but-present is acceptable.

## Summary

No operator-facing migration is required beyond pinning the release tag and reconciling.

## Config and values migrations

- None.

## SOPS and secret changes

- None.

## Manual steps

- None.

## Rollback

- Pin the previous release tag in the Flux `GitRepository` and let Flux reconcile.

## BOM delta

- See [`release/bom.yaml`](../../release/bom.yaml) for the authoritative pin-set.
