#!/usr/bin/env bash
# Prepare a fresh clone or worktree without installing hooks or mutating dependency manifests.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir

mise_bin="$(command -v mise || true)"
if [[ -z "${mise_bin}" && -x "${HOME}/.local/bin/mise" ]]; then
	mise_bin="${HOME}/.local/bin/mise"
fi
[[ -n "${mise_bin}" ]] || {
	echo "error: mise is required; install it from https://mise.jdx.dev/" >&2
	exit 2
}
command -v git >/dev/null 2>&1 || {
	echo "error: git is required" >&2
	exit 2
}

before="$(git -C "${root_dir}" status --porcelain=v1 --untracked-files=no)"

run() {
	echo "+ $*" >&2
	"$@" >&2
}

run "${mise_bin}" trust --all --yes --cd "${root_dir}"
run "${mise_bin}" --cd "${root_dir}" install

for app in matrix-a2a-bridge activitypub-agent-gateway; do
	app_dir="${root_dir}/apps/${app}"
	run "${mise_bin}" --cd "${app_dir}" install
	run "${mise_bin}" --cd "${app_dir}" exec -- go mod download
done

policy_dir="${root_dir}/apps/synapse-federation-policy"
run "${mise_bin}" --cd "${policy_dir}" install
run "${mise_bin}" --cd "${policy_dir}" exec -- uv sync --frozen

after="$(git -C "${root_dir}" status --porcelain=v1 --untracked-files=no)"
if [[ "${after}" != "${before}" ]]; then
	echo "error: agent setup changed tracked repository state" >&2
	git -C "${root_dir}" status --short >&2
	exit 1
fi

echo "Fgentic agent environment ready."
