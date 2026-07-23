#!/usr/bin/env bash
# Restart the Synapse homeserver while a task is mid-poll and prove exactly-once reply delivery (#466).
set -euo pipefail

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-synapse-restart}"
export INTEGRATION_SCENARIO=synapse-restart
export FIXTURE_SETTINGS=synapse-restart-settings.yaml
export DRIVER_MANIFEST=synapse-restart-driver-job.yaml
export DRIVER_JOB_NAME=synapse-restart-driver
export DRIVER_WAIT_TIMEOUT=390s

exec bash "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/run.sh"
