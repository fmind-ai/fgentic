#!/usr/bin/env bash
# Offline contract for the time-bounded-trust review/expiry alerts (issue #463): the committed rules render
# deterministically from the registry (no hand-edits) with the expected default lead window, both rendered
# PrometheusRules are valid, and the alert logic fires correctly under promtool (rendered with a small lead
# so real epochs need not be evaluated). The recording-rule timestamps match the registry dates.
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly ROOT_DIR
# shellcheck source=scripts/lib.sh
source "${ROOT_DIR}/scripts/lib.sh"
WORK_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fgentic-trust-alert.XXXXXX")"
readonly WORK_DIR
trap 'rm -rf "${WORK_DIR}"' EXIT INT TERM

require_commands yq jq promtool date diff

readonly COMPONENT="${ROOT_DIR}/infra/observability/monitors/components/federation-trust-review"
readonly RECORDING="${COMPONENT}/recording-rules.yaml"
readonly ALERTS="${COMPONENT}/alert-rules.yaml"

# 1. Deterministic render: re-render from the registry and require byte-identical committed rule files.
bash "${ROOT_DIR}/scripts/fed-trust-alert-render.sh" --out-root "${WORK_DIR}/render" >/dev/null
for rel in recording-rules.yaml alert-rules.yaml; do
	diff -u "${COMPONENT}/${rel}" "${WORK_DIR}/render/infra/observability/monitors/components/federation-trust-review/${rel}" >/dev/null \
		|| fail "${rel} drifted from the registry — run 'mise run fed:trust-alert-render' (no hand-edits)"
done

# 2. The committed alert file carries the default 14-day lead window.
grep -Fq 'time()) < 1209600' "${ALERTS}" || fail "committed alert must use the default 1209600s (14-day) lead window"

# 3. Each recording-rule epoch matches the registry date (content-free: server_name + timestamp only).
registry="${ROOT_DIR}/infra/federation/registry/partners.yaml"
admitted_partners="$(yq -r '.partners[] | select(.role == "admitted") | .server_name' "${registry}")"
while IFS= read -r partner; do
	[ -n "${partner}" ] || continue
	review_by="$(yq -r ".partners[] | select(.server_name == \"${partner}\") | .review_by" "${registry}")"
	review_epoch="$(date -u -d "${review_by}" +%s)"
	grep -Fq "vector(${review_epoch})" "${RECORDING}" \
		|| fail "recording rule missing review_by epoch ${review_epoch} for ${partner}"
	valid="$(yq -r ".partners[] | select(.server_name == \"${partner}\") | .valid_until // \"\"" "${registry}")"
	if [ -n "${valid}" ]; then
		valid_epoch="$(date -u -d "${valid}" +%s)"
		grep -Fq "vector(${valid_epoch})" "${RECORDING}" \
			|| fail "recording rule missing valid_until epoch ${valid_epoch} for ${partner}"
	fi
done <<<"${admitted_partners}"

# 4. Both rendered rules are valid Prometheus rules.
for rel in recording-rules.yaml alert-rules.yaml; do
	yq 'select(.kind == "PrometheusRule") | .spec' "${COMPONENT}/${rel}" >"${WORK_DIR}/${rel}"
	promtool check rules "${WORK_DIR}/${rel}" >/dev/null || fail "${rel} is not a valid PrometheusRule"
done

# 5. Alert logic: render the alert rules with a small lead (300s) and unit-test them against the fixture's
#    synthetic timestamps, so firing/quiet behavior is proven without evaluating real-epoch time ranges.
bash "${ROOT_DIR}/scripts/fed-trust-alert-render.sh" --lead-seconds 300 --out-root "${WORK_DIR}/small" >/dev/null
small_alerts="${WORK_DIR}/small-alert-rules.yaml"
yq 'select(.kind == "PrometheusRule") | .spec' \
	"${WORK_DIR}/small/infra/observability/monitors/components/federation-trust-review/alert-rules.yaml" >"${small_alerts}"
test_file="${WORK_DIR}/federation-trust-review.test.yaml"
RULES_FILE="${small_alerts}" yq '.rule_files = [strenv(RULES_FILE)]' \
	"${ROOT_DIR}/scripts/testdata/federation-trust-review.test.yaml" >"${test_file}"
promtool test rules "${test_file}" >/dev/null || fail "the time-bounded-trust alert unit tests failed"

echo "Time-bounded-trust alert contract passed: rules render deterministically, epochs match the registry, alerts fire correctly"
