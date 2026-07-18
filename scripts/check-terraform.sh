#!/usr/bin/env bash
# Hermetic Terraform static gate: fmt-check + per-directory init/validate + the mocked test suite.
#
# Each init/validate/test runs under a task-owned ABSOLUTE `TF_DATA_DIR`, so a working tree where a
# real `terraform init` once configured the GCS backend (cached in `.terraform/terraform.tfstate`)
# can never make this offline gate read `gs://fgentic-ai-tfstate` for backend migration — which is
# what broke every commit in that worktree the moment Google ADC expired (2026-07-16 incident). The
# operator's real `.terraform/` is never read or mutated; the temp data dirs are removed on exit.
#
# `TF_CHECK_ROOT` overrides the repository root (used by scripts/test-terraform-hermetic.sh to run
# this exact gate against a poisoned throwaway copy).
set -euo pipefail

root="${TF_CHECK_ROOT:-$(git rev-parse --show-toplevel)}"
data_root="$(mktemp -d)"
trap 'rm -rf "${data_root}"' EXIT

terraform fmt -check -recursive "${root}/infra"

# One absolute data dir per target directory. Absolute so `-chdir` cannot reinterpret it, and under
# ${data_root} so the trap reclaims it.
data_dir_for() {
	local directory_name
	directory_name="$(printf '%s' "$1" | tr '/' '_')"
	printf '%s/%s' "${data_root}" "${directory_name}"
}

for dir in "${root}/infra/terraform" "${root}/infra/terraform/bootstrap"; do
	[ -d "${dir}" ] || continue
	data_dir="$(data_dir_for "${dir}")"
	TF_DATA_DIR="${data_dir}" terraform -chdir="${dir}" init -backend=false -input=false >/dev/null
	TF_DATA_DIR="${data_dir}" terraform -chdir="${dir}" validate
done

# The mocked contract tests reuse infra/terraform's already-initialized data dir.
tf_dir="${root}/infra/terraform"
tf_data_dir="$(data_dir_for "${tf_dir}")"
TF_DATA_DIR="${tf_data_dir}" terraform -chdir="${tf_dir}" test
