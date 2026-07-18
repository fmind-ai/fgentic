#!/usr/bin/env bash
# Update or verify the reviewed MCP server-surface pin with the bridge Go module.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
PIN_MCP_BIN="$(mktemp)"
readonly PIN_MCP_BIN
trap 'rm -f "${PIN_MCP_BIN}"' EXIT

mise --cd "${ROOT_DIR}/apps/matrix-a2a-bridge" exec -- \
	go build -o "${PIN_MCP_BIN}" ./cmd/pin-mcp
"${PIN_MCP_BIN}" "$@"
