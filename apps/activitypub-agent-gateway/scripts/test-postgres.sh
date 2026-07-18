#!/usr/bin/env bash
set -euo pipefail

readonly postgres_image="postgres@sha256:742f40ea20b9ff2ff31db5458d127452988a2164df9e17441e191f3b72252193"
readonly test_container="fgentic-ap-state-test-$$"

cleanup() {
  docker rm --force "${test_container}" >/dev/null 2>&1 || true
}
trap cleanup EXIT INT TERM

docker run --detach --rm \
  --name "${test_container}" \
  --publish 127.0.0.1::5432 \
  --env POSTGRES_DB=activitystate \
  --env POSTGRES_HOST_AUTH_METHOD=trust \
  "${postgres_image}" >/dev/null

for _ in $(seq 1 60); do
  if docker exec "${test_container}" pg_isready --username postgres --dbname activitystate >/dev/null 2>&1; then
    break
  fi
  sleep 0.5
done
docker exec "${test_container}" pg_isready --username postgres --dbname activitystate >/dev/null
# The in-container socket can accept slightly before Docker's published-port proxy is ready.
sleep 1

port_binding=$(docker port "${test_container}" 5432/tcp)
readonly test_port="${port_binding##*:}"
ACTIVITY_STATE_TEST_DATABASE_URL="postgres://postgres@127.0.0.1:${test_port}/activitystate?sslmode=disable" \
  go test -race -count=1 -tags integration ./internal/activitystate
