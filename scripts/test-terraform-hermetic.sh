#!/usr/bin/env bash
# Regression for the 2026-07-16 incident: a worktree where a real `terraform init` once configured
# the GCS backend caches it in `.terraform/terraform.tfstate`, and every later `init -backend=false`
# tried to migrate from `gs://fgentic-ai-tfstate` — so an expired Google ADC failed the offline
# commit gate. This proves `scripts/check-terraform.sh` is hermetic: it runs the EXACT gate against
# a throwaway copy whose `.terraform` caches are poisoned with that GCS backend, and it must pass
# without reading the backend and without writing into the working `.terraform` directories.
#
# It never touches the operator's real tree (it runs against a temp TF_CHECK_ROOT), so it is safe to
# run in parallel with the ordinary `check:terraform` inside the aggregate gate.
set -euo pipefail

root="$(git rev-parse --show-toplevel)"
cd "${root}"
tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

mkdir -p "${tmp}/infra"
cp -r infra/terraform "${tmp}/infra/terraform"

poison_backend() {
  # A backend cache pointing at the private GCS state bucket with no credentials configured.
  rm -rf "$1/.terraform"
  mkdir -p "$1/.terraform"
  cat > "$1/.terraform/terraform.tfstate" <<'JSON'
{"version":3,"serial":1,"lineage":"poison-regression","backend":{"type":"gcs","hash":1,"config":{"bucket":"fgentic-ai-tfstate","prefix":"fgentic"}}}
JSON
}

poison_backend "${tmp}/infra/terraform"
[ -d "${tmp}/infra/terraform/bootstrap" ] && poison_backend "${tmp}/infra/terraform/bootstrap"

if ! TF_CHECK_ROOT="${tmp}" bash scripts/check-terraform.sh >/dev/null 2>&1; then
  echo "FAIL: check-terraform.sh read the poisoned GCS backend cache (not hermetic)" >&2
  exit 1
fi

# The poison must be ignored (redirection), not migrated or overwritten...
for cache in \
  "${tmp}/infra/terraform/.terraform/terraform.tfstate" \
  "${tmp}/infra/terraform/bootstrap/.terraform/terraform.tfstate"; do
  [ -f "${cache}" ] || continue
  grep -q '"type":"gcs"' "${cache}" || {
    echo "FAIL: the gate mutated the poisoned backend cache at ${cache}" >&2
    exit 1
  }
done

# ...and providers must land in the task-owned TF_DATA_DIR, never in the working .terraform.
for provider_dir in \
  "${tmp}/infra/terraform/.terraform/providers" \
  "${tmp}/infra/terraform/bootstrap/.terraform/providers"; do
  [ ! -e "${provider_dir}" ] || {
    echo "FAIL: providers written to the working .terraform (${provider_dir}) instead of TF_DATA_DIR" >&2
    exit 1
  }
done

echo "terraform hermetic gate: poisoned GCS backend ignored; working .terraform left untouched"
