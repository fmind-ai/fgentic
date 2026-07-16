---
type: Architecture Decision Record
title: Snapshot-Backed Synapse Media PVC
description: Keep Synapse media on a retained PVC and make GKE recovery explicit through CSI snapshots.
---

# 0019 — Snapshot-Backed Synapse Media PVC

Status: Accepted

Decision register: [D12](../design-decisions.md#d12--data-durability-was-zero-backups)

Implementation: #62 owns the declarative storage boundary; #64 owns the first observed, timed database+media restore.

## Context

Synapse stores room history and media metadata in PostgreSQL but stores uploaded bytes and generated thumbnails under its media directory. The [pinned ESS 26.6.2 chart](../../infra/flux/sources.yaml) mounts that directory from a dedicated `ess-synapse-media` PersistentVolumeClaim, defaults it to 10 GiB, and marks the claim with Helm's `keep` resource policy. Database recovery alone therefore cannot recover attachments or avatars.

Two approaches were evaluated against the local k3d profile and optional GKE reference:

1. **External S3/GCS media provider.** This can decouple media from a node-attached volume and an S3-compatible endpoint can be cloud-agnostic. The pinned Synapse image does not include `synapse-s3-storage-provider` or its runtime dependencies, however, so the reference would need a separately built and digest-pinned provider image. S3-compatible authentication would also add HMAC credentials that cannot reuse the current keyless GCS Workload Identity boundary. A native-GCS provider would instead make the default provider-specific.
1. **PVC plus CSI snapshot.** This uses the storage surface ESS already owns and adds no credential or application-image boundary. [Kubernetes snapshots](https://kubernetes.io/docs/concepts/storage/volume-snapshots/) require a CSI driver, snapshot CRDs/controller, a matching `VolumeSnapshotClass`, and an explicit `VolumeSnapshot` request. The local `rancher.io/local-path` provisioner has no snapshot implementation; [GKE's PD CSI driver](https://cloud.google.com/kubernetes-engine/docs/how-to/persistent-volumes/gce-pd-csi-driver) provides `standard-rwo` and volume snapshots when its Standard-cluster addon is enabled.

A filesystem snapshot is only one recovery artifact. The restored PostgreSQL state and media bytes must represent a deliberately coordinated recovery point, and a bound replacement PVC is not evidence that a known upload survived.

## Decision

1. The default and reference profiles keep Synapse media on ESS's dedicated retained PVC. Both overlays set the size and StorageClass explicitly so a chart or cluster-default change cannot silently move the data boundary. The initial size remains 10 GiB and is deployment configuration, not a capacity recommendation.
1. Local k3d uses `local-path`. It preserves media across Pod restarts and ordinary Helm reconciliation, but it has no snapshot or backup claim. Local cluster or volume loss may destroy the media.
1. The GKE reference enables the managed Compute Engine persistent disk CSI addon and uses its pre-installed `standard-rwo` StorageClass. It declares the non-default `fgentic-synapse-media` `VolumeSnapshotClass` with driver `pd.csi.storage.gke.io` and `deletionPolicy: Retain`.
1. Fgentic does not create snapshots on an unbounded schedule. The isolated restore drill creates a named snapshot, waits for `readyToUse: true`, records the database and media recovery timestamps, restores a new PVC from that exact snapshot, and verifies a deterministic uploaded fixture by content hash. The drill must quiesce media writes while capturing its recovery point.
1. Synapse remains one main replica with no PDB. A retained ReadWriteOnce PVC and recoverable snapshots improve durability; they do not provide a concurrently writable shared media store or continuous availability.
1. S3/GCS media providers remain optional production extensions. Adopting one requires a separate review of the pinned provider image, immutable supply chain, credentials, egress, retention, deletion, and restore evidence; no runtime `pip install` or implicit reuse of the CNPG backup identity is allowed.

## Consequences

1. The default path stays small, provider-independent above the StorageClass, and aligned with the unmodified ESS chart.
1. GKE gains a declarative snapshot mechanism without creating a cluster, snapshot, or bill during repository validation. A live paid-cluster restore remains an explicit gate.
1. Local development remains intentionally weaker: a retained local-path claim is persistence, not backup. Documentation and tests must preserve that distinction.
1. CSI snapshots consume provider storage until their retained contents are deliberately removed. Operators must apply an approved retention decision after preserving drill evidence; deleting only the `VolumeSnapshot` request does not necessarily delete the retained provider snapshot.
1. Point-in-time PostgreSQL recovery and volume snapshots are separate mechanisms. The first successful, timed, coordinated restore is required before publishing an RPO or RTO.
