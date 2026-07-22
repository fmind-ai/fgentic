#!/usr/bin/env bash
# Prove bounded retries and the §6.1 catalog notice when the model backend fails mid-delegation (#466).
set -euo pipefail

export KIND_CLUSTER_NAME="${KIND_CLUSTER_NAME:-fgentic-bridge-model-outage}"
export INTEGRATION_SCENARIO=model-outage
export FIXTURE_SETTINGS=model-outage-settings.yaml
export DRIVER_MANIFEST=model-outage-driver-job.yaml
export DRIVER_JOB_NAME=model-outage-driver
export DRIVER_WAIT_TIMEOUT=270s
# Must match delegationMaxAttempts in model-outage-settings.yaml.
export MODEL_OUTAGE_MAX_ATTEMPTS="${MODEL_OUTAGE_MAX_ATTEMPTS:-3}"

exec bash "$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)/run.sh"
