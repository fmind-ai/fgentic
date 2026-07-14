#!/usr/bin/env bash
# Interrupt one active delegation and prove graceful drain plus cross-pod persistent deduplication.
set -euo pipefail

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-availability}"
export INTEGRATION_SCENARIO=availability
export FIXTURE_SETTINGS=availability-settings.yaml
export DRIVER_MANIFEST=availability-driver-job.yaml
export DRIVER_JOB_NAME=availability-driver
export DRIVER_WAIT_TIMEOUT=150s

exec bash "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/run.sh"
