#!/usr/bin/env bash
# Run the resource-bounded D3 regression scenario on the shared integration fixture definition.
set -euo pipefail

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-load}"
export INTEGRATION_SCENARIO=load
export FIXTURE_SETTINGS=load-settings.yaml
export DRIVER_MANIFEST=load-driver-job.yaml
export DRIVER_JOB_NAME=load-driver
export DRIVER_WAIT_TIMEOUT=240s

exec bash "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/run.sh"
