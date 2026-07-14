#!/usr/bin/env bash
set -euo pipefail

bash scripts/check-reference-grants.sh scripts/testdata/reference-grants.covered.yaml
if bash scripts/check-reference-grants.sh scripts/testdata/reference-grants.uncovered.yaml >/dev/null 2>&1; then
  echo "error: ReferenceGrant audit accepted an uncovered cross-namespace backend" >&2
  exit 1
fi
if bash scripts/check-reference-grants.sh scripts/testdata/reference-grants.policy-uncovered.yaml >/dev/null 2>&1; then
  echo "error: ReferenceGrant audit accepted an uncovered agentgateway tracing backend" >&2
  exit 1
fi

echo "ReferenceGrant negative test passed"
