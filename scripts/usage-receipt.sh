#!/usr/bin/env bash
# Stable repository entrypoint for the federation usage-receipt signer and verifier.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR

exec mise --cd "${ROOT_DIR}/apps/matrix-a2a-bridge" exec -- \
	go run ./cmd/usage-receipt "$@"
