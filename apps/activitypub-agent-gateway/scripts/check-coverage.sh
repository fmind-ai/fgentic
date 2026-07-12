#!/usr/bin/env bash
# Package-level ratchets for the governance-critical AP gateway core. Wiring packages have focused
# smoke tests but are not distorted to chase an arbitrary aggregate percentage.
set -euo pipefail

profile="${1:-coverage.out}"
if [ ! -s "${profile}" ]; then
  echo "error: coverage profile not found or empty: ${profile}" >&2
  exit 2
fi

check_package() {
  local package="$1"
  local floor="$2"
  local result
  result="$(awk -v package="/${package}/" '
    index($1, package) {
      statements += $(NF - 1)
      if ($NF > 0) covered += $(NF - 1)
    }
    END {
      if (statements == 0) exit 2
      printf "%.1f", 100 * covered / statements
    }
  ' "${profile}")"
  if ! awk -v result="${result}" -v floor="${floor}" 'BEGIN { exit !(result + 0 >= floor + 0) }'; then
    echo "error: ${package} coverage ${result}% is below the ${floor}% ratchet" >&2
    return 1
  fi
  echo "${package} coverage: ${result}% (floor ${floor}%)"
}

check_package internal/apgateway 80
check_package internal/a2a 70
check_package internal/config 80
check_package internal/httpsig 75
check_package internal/policy 90
check_package internal/integrity 85
