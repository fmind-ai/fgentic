#!/usr/bin/env bash
# Shared constants for the ActivityPub demo interop acceptance (issue #489, Task 7). Sourced by BOTH
# the demo secret path (scripts/lib/demo-secrets.sh — pins the peer public keys into the gateway) and
# the acceptance harness (scripts/test-activitypub-interop.sh — drives the peer), so the pinned actor
# URIs, the allowlisted identity, the ghost, and the in-cluster endpoint never drift between them.
# The two peer identities are pinned out-of-band via ADR 0021 because an in-cluster peer cannot be
# SSRF-fetched by the #320-guarded key resolver.
# shellcheck disable=SC2034 # every constant is consumed by a sourcing caller, not within this file
if [ -z "${AP_INTEROP_SOURCED:-}" ]; then
	AP_INTEROP_SOURCED=1

	AP_INTEROP_NAMESPACE="activitypub-interop"
	AP_INTEROP_GHOST="agent-docs-qa"
	AP_INTEROP_SERVER_NAME="fgentic.localhost"
	AP_INTEROP_HANDLE="${AP_INTEROP_GHOST}@${AP_INTEROP_SERVER_NAME}"
	# The allowlisted signer and the deliberately off-allowlist signer (fail-closed proof). Both are
	# pinned; only the first is added to the border allowlist at acceptance time.
	AP_INTEROP_ALLOWED_ACTOR="https://interop-peer.demo.${AP_INTEROP_SERVER_NAME}/actor"
	AP_INTEROP_DENIED_ACTOR="https://denied-peer.demo.${AP_INTEROP_SERVER_NAME}/actor"
	AP_INTEROP_ALLOWED_DOMAIN="interop-peer.demo.${AP_INTEROP_SERVER_NAME}"
	# Bootstrap Secret keys holding the two peer RSA private keys (cluster-only demo secret path).
	AP_INTEROP_ALLOWED_KEY="ap-peer-allowed-key"
	AP_INTEROP_DENIED_KEY="ap-peer-denied-key"
	# The gateway-side Secret that carries the pinned PUBLIC keys (mounted via pinnedKeys.secretName).
	AP_INTEROP_PINS_SECRET="activitypub-agent-gateway-pinned-keys"
fi
