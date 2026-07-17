#!/usr/bin/env bash
# Bounded fuzz smoke over every owned untrusted-input parser (issue #461).
#
# The fuzz SEED corpora and Hypothesis examples already ride the normal per-commit gates
# deterministically (they run as ordinary subtests under `go test` / `pytest`). This script adds the
# bounded *mutation* smoke: it discovers every Go `Fuzz*` target in the bridge and ActivityPub
# gateway modules and runs each for a short, fixed budget. It is deliberately kept out of the
# mutex-serialized aggregate `test` gate — where nondeterministic mutation would add recurring cost
# and flakiness to a gate all worktrees contend on — and is instead driven on demand and by the
# scheduled fuzz CI job (.github/workflows/fuzz.yml), where a longer budget is affordable.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR

# Seconds per target. Short by default for a quick smoke; the scheduled CI job passes a longer value.
readonly FUZZTIME="${FGENTIC_FUZZTIME:-5s}"
readonly MODULES=(
	"apps/matrix-a2a-bridge"
	"apps/activitypub-agent-gateway"
)

fail() {
	echo "error: $*" >&2
	exit 1
}

command -v go >/dev/null 2>&1 || fail "go toolchain not found"

total=0
for module in "${MODULES[@]}"; do
	module_dir="${ROOT_DIR}/${module}"
	[ -d "${module_dir}" ] || fail "module not found: ${module}"
	while IFS= read -r package; do
		[ -n "${package}" ] || continue
		# Listing compiles the test binary; a compile failure must surface, not be swallowed, so a
		# broken fuzz file can never silently drop its targets while the smoke still "passes".
		listing="$(cd "${module_dir}" && go test "${package}" -list '^Fuzz' 2>&1)" ||
			fail "listing fuzz targets failed for ${package}: ${listing}"
		# Each package's Fuzz targets, one per invocation (go test fuzzes a single target at a time).
		while IFS= read -r target; do
			[ -n "${target}" ] || continue
			echo "==> fuzz ${package} ${target} (${FUZZTIME})"
			(cd "${module_dir}" && go test "${package}" \
				-run '^$' -fuzz "^${target}$" -fuzztime "${FUZZTIME}") ||
				fail "fuzz target failed: ${package} ${target}"
			total=$((total + 1))
		done < <(printf '%s\n' "${listing}" | grep '^Fuzz' || true)
	done < <(cd "${module_dir}" && go list ./... 2>/dev/null || true)
done

[ "${total}" -gt 0 ] || fail "no fuzz targets were discovered"
echo "Fuzz smoke passed: ${total} target(s) at ${FUZZTIME} each."
