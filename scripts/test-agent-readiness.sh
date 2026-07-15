#!/usr/bin/env bash
# Keep the shared Codex/Claude discovery and worktree setup contract executable in CI.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir

fail() {
	echo "error: $*" >&2
	exit 1
}

[[ -L "${root_dir}/AGENTS.md" ]] || fail "AGENTS.md must remain a symlink"
[[ "$(readlink "${root_dir}/AGENTS.md")" == ".agents/AGENTS.md" ]] ||
	fail "AGENTS.md must target .agents/AGENTS.md"
[[ "$(<"${root_dir}/CLAUDE.md")" == "@AGENTS.md" ]] ||
	fail "CLAUDE.md must include the shared root instructions"
[[ -L "${root_dir}/.claude/skills" ]] || fail ".claude/skills must remain a symlink"
[[ "$(readlink "${root_dir}/.claude/skills")" == "../.agents/skills" ]] ||
	fail ".claude/skills must target ../.agents/skills"

jq -e '
  .attribution.sessionUrl == false and
  .worktree.baseRef == "fresh" and
  ([.hooks.SessionStart[] |
    select(.matcher == "startup") |
    .hooks[] |
    select(
      .type == "command" and
      .command == "${CLAUDE_PROJECT_DIR}/scripts/agent-setup.sh" and
      .args == [] and
      .timeout == 600
    )] | length) == 1
' "${root_dir}/.claude/settings.json" >/dev/null ||
	fail "Claude project settings lost the setup, attribution, or fresh-worktree contract"

yq --input-format toml --output-format json '.setup.script' \
	"${root_dir}/.codex/environments/environment.toml" |
	jq -e '. == "mise run agent:setup"' >/dev/null ||
	fail "Codex local environment must use the shared agent setup task"

git -C "${root_dir}" check-ignore --quiet .claude/worktrees/probe ||
	fail ".claude/worktrees must be ignored"

if rg --line-number \
	'go mod tidy|mise run install|lefthook|k3d|kubectl|gcloud|sops' \
	"${root_dir}/scripts/agent-setup.sh"; then
	fail "agent setup contains a mutating, cluster, cloud, hook, or secret command"
fi

bash -n "${root_dir}/scripts/agent-setup.sh"
echo "Agent discovery and worktree setup contracts passed"
