#!/usr/bin/env bash
# Prove hard process-loss recovery at the durable Postgres, A2A, and Matrix boundaries.
set -euo pipefail

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-crash-recovery}"
export INTEGRATION_SCENARIO=crash-recovery
export FIXTURE_SETTINGS=crash-recovery-settings.yaml
export DRIVER_MANIFEST=crash-recovery-driver-job.yaml
export DRIVER_JOB_NAME=crash-recovery-driver
export DRIVER_WAIT_TIMEOUT=360s

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
readonly SCRIPT_DIR
exec bash "${SCRIPT_DIR}/run.sh"
