# Synapse federation policy

`fgentic_federation_policy` is Fgentic's fail-closed policy border for events received over Matrix federation. It registers Synapse's `federated_user_may_invite` and `should_drop_federated_event` spam-checker callbacks and evaluates only content-free event metadata against a git-declared JSON policy.

The module targets the platform-pinned Synapse 1.156.0 callback contract and its Python 3.13 runtime. It uses the Python standard library plus the stable `NOT_SPAM` and `errors.Codes.FORBIDDEN` decisions provided by the host's Synapse module API. It has no separately installed runtime dependencies: the deployed artifact is one Python source file mounted into the unchanged Synapse container.

## Policy

In the source repository, `apps/synapse-federation-policy/policy/policy.json` is the canonical git-declared deployment policy. Deployment assets are intentionally not part of the standalone wheel. The schema is deliberately small and strict:

| Field                 | Contract                                                                                             |
| --------------------- | ---------------------------------------------------------------------------------------------------- |
| `version`             | Integer `1`. Other versions fail closed.                                                             |
| `allowed_servers`     | Non-empty unique, exact lowercase DNS Matrix server names, including the local server. No globs.     |
| `allowed_event_types` | Non-empty unique, exact Matrix event types. No globs.                                                |
| `invite_rule`         | `allow_from_allowed_servers` or `deny_all`; the server and event-type allowlists always apply first. |

Unknown keys, duplicate JSON keys or list entries, non-UTF-8 input, oversized files, invalid values, and unreadable replacements activate a deny-all state. A valid projected ConfigMap update is detected by source metadata on the next callback and swapped in atomically; Synapse does not need a restart.

Transient projected-volume failures are retried with exponential backoff capped at 30 seconds. Reload errors are emitted once per failure streak; a recovered policy emits a fresh loaded record.

Violations are logged as a stable `fgentic_federation_policy_violation` prefix followed by compact canonical JSON. The record contains only the reason, sender server, event type, room ID, event ID, policy digest, invite rule, and allowlist counts. Event content, sender localparts, and policy values are never logged.

## Synapse configuration

The source repository's `apps/synapse-federation-policy/kustomization.yaml` Component generates:

1. `fgentic-synapse-federation-policy-v1`, an immutable source ConfigMap with `fgentic_federation_policy.py`.
1. `fgentic-federation-policy`, a stable mutable ConfigMap with `policy.json`.

Mount the source file on Synapse's Python path and configure the module with the mounted policy path:

```yaml
modules:
  - module: fgentic_federation_policy.FederationPolicyModule
    config:
      policy_path: /etc/fgentic/federation-policy/policy.json
```

Source ConfigMap names are versioned because Kubernetes immutable resources cannot be changed in place. Policy keeps a stable name so Flux can reconcile git policy changes without repackaging the callback code.

## Staged-event stability

Synapse 1.156.0 calls `should_drop_federated_event` both before inserting a new inbound event and again while draining its staging queue. Applying a stricter policy between those calls would otherwise leave an already-staged event at the front of the queue indefinitely.

The module therefore uses the public `ModuleApi.run_db_interaction` API only after the active policy denies an event:

1. An allowed event returns immediately and performs no database query.
1. An exact `(room_id, event_id)` row in `federation_inbound_events_staging` is grandfathered once so Synapse can process and remove it.
1. An absent row or database error remains denied. Database error logs are coalesced and contain no exception or event content.

This is a stable staging decision, not a policy bypass for newly received events: the pre-insert callback cannot find a row and still drops the event. It also lets a restarted module drain rows staged under an earlier policy, including while a replacement policy is temporarily invalid.

The table name and its `room_id`/`event_id` columns are a deliberate private-schema dependency pinned to Synapse 1.156.0. Every Synapse upgrade must verify the inbound staging schema and both callback sites in `federation_server.py`, run the staging regression suite, and bump the immutable source ConfigMap version when compatibility changes. An upgrade must not proceed on the assumption that the private schema is stable.

## Development

```bash
mise run install
mise run format
mise run check
mise run test
mise run build
```

The test suite holds statement and branch coverage at 100%, including malformed policy input, fail-closed file replacement, recovery, callback registration, exact policy decisions, and content-free log records.

## License boundary

This module and its policy are Apache-2.0 under the repository license. They are standalone configuration/plugin code written against Synapse's documented module API; they do not copy or modify Synapse implementation code. At deployment, the source is mounted into and invoked by a separately distributed, unchanged Synapse process. Synapse remains under its own upstream license, and distributors remain responsible for complying with it. This documents the engineering boundary, not legal advice.
