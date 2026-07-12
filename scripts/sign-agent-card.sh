#!/usr/bin/env bash
# Stable repository entrypoint for the provider-free AgentCard signing and verification tool.
set -euo pipefail

readonly ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

exec mise --cd "${ROOT_DIR}/apps/matrix-a2a-bridge" exec -- \
	go run ./cmd/sign-agent-card "$@"
