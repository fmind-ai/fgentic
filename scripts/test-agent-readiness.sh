#!/usr/bin/env bash
# Keep the shared Codex/Claude discovery and worktree setup contract executable in CI.
set -euo pipefail

root_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly root_dir
readonly agent_instruction_budget=28672

fail() {
	echo "error: $*" >&2
	exit 1
}

[[ -L "${root_dir}/AGENTS.md" ]] || fail "AGENTS.md must remain a symlink"
agents_link="$(readlink "${root_dir}/AGENTS.md")"
[[ "${agents_link}" == ".agents/AGENTS.md" ]] \
	|| fail "AGENTS.md must target .agents/AGENTS.md"

root_guidance="${root_dir}/.agents/AGENTS.md"
root_guidance_bytes="$(wc -c <"${root_guidance}")"
((root_guidance_bytes <= agent_instruction_budget)) \
	|| fail ".agents/AGENTS.md is ${root_guidance_bytes} bytes; budget is ${agent_instruction_budget}"

largest_combined_bytes="${root_guidance_bytes}"
largest_combined_path=".agents/AGENTS.md"
guidance_files="$(git -C "${root_dir}" ls-files '*AGENTS.md' | sort)"
while IFS= read -r guidance; do
	case "${guidance}" in
		AGENTS.md | .agents/AGENTS.md) continue ;;
		*) : ;;
	esac

	combined_guidance_bytes="${root_guidance_bytes}"
	guidance_dir="$(dirname "${guidance}")"
	while [[ "${guidance_dir}" != "." ]]; do
		candidate="${guidance_dir}/AGENTS.md"
		if [[ -f "${root_dir}/${candidate}" ]]; then
			nested_guidance_bytes="$(wc -c <"${root_dir}/${candidate}")"
			combined_guidance_bytes="$((combined_guidance_bytes + nested_guidance_bytes))"
		fi
		guidance_dir="$(dirname "${guidance_dir}")"
	done
	((combined_guidance_bytes <= agent_instruction_budget)) \
		|| fail "instruction chain ending at ${guidance} is ${combined_guidance_bytes} bytes; budget is ${agent_instruction_budget}"
	if ((combined_guidance_bytes > largest_combined_bytes)); then
		largest_combined_bytes="${combined_guidance_bytes}"
		largest_combined_path=".agents/AGENTS.md + ${guidance}"
	fi
done <<<"${guidance_files}"

[[ "$(<"${root_dir}/CLAUDE.md")" == "@AGENTS.md" ]] \
	|| fail "CLAUDE.md must include the shared root instructions"
[[ -L "${root_dir}/.claude/skills" ]] || fail ".claude/skills must remain a symlink"
claude_skills_link="$(readlink "${root_dir}/.claude/skills")"
[[ "${claude_skills_link}" == "../.agents/skills" ]] \
	|| fail ".claude/skills must target ../.agents/skills"

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
' "${root_dir}/.claude/settings.json" >/dev/null \
	|| fail "Claude project settings lost the setup, attribution, or fresh-worktree contract"

yq --input-format toml --output-format json '.setup.script' \
	"${root_dir}/.codex/environments/environment.toml" \
	| jq -e '. == "mise run agent:setup"' >/dev/null \
	|| fail "Codex local environment must use the shared agent setup task"

yq --input-format toml --output-format json '.tasks."agent:gate".run' \
	"${root_dir}/mise.toml" \
	| jq -e '. == "bash scripts/agent-gate.sh"' >/dev/null \
	|| fail "mise must expose the portable final-gate mutex"

yq --output-format json '
  {
    "check": ."pre-commit".commands.check.run,
    "test": ."pre-push".commands.test.run
  }
' "${root_dir}/lefthook.yml" \
	| jq -e '
    .check == "mise run agent:gate -- check" and
    .test == "mise run agent:gate -- test"
  ' >/dev/null || fail "git hooks must serialize aggregate gates across worktrees"

for guidance in \
	"${root_dir}/.agents/AGENTS.md" \
	"${root_dir}/.agents/skills/github-flow/SKILL.md" \
	"${root_dir}/CONTRIBUTING.md"; do
	rg --quiet 'status/in-progress' "${guidance}" \
		|| fail "${guidance#"${root_dir}/"} lost the issue-claim protocol"
	rg --quiet 'mise run agent:gate' "${guidance}" \
		|| fail "${guidance#"${root_dir}/"} lost the serialized validation contract"
done

for runbook in \
	"${root_dir}/README.md" \
	"${root_dir}/.agents/skills/bridge-dev/SKILL.md" \
	"${root_dir}/.agents/skills/flux-gitops/SKILL.md" \
	"${root_dir}/.agents/skills/local-cluster/SKILL.md" \
	"${root_dir}/.agents/skills/matrix-agents/SKILL.md" \
	"${root_dir}/.agents/skills/terraform-gke/SKILL.md"; do
	rg --quiet 'mise run agent:gate' "${runbook}" \
		|| fail "${runbook#"${root_dir}/"} lost the serialized validation contract"
done

git -C "${root_dir}" check-ignore --quiet .claude/worktrees/probe \
	|| fail ".claude/worktrees must be ignored"

if rg --line-number \
	'go mod tidy|mise run install|lefthook|k3d|kubectl|gcloud|sops' \
	"${root_dir}/scripts/agent-setup.sh"; then
	fail "agent setup contains a mutating, cluster, cloud, hook, or secret command"
fi

bash -n "${root_dir}/scripts/agent-setup.sh"
bash -n "${root_dir}/scripts/agent-gate.sh"
if bash "${root_dir}/scripts/agent-gate.sh" invalid >/dev/null 2>&1; then
	fail "agent gate accepted an unsupported mode"
fi
if rg --quiet '\bflock\b' "${root_dir}/scripts/agent-gate.sh"; then
	fail "agent gate must remain portable to macOS without flock"
fi
echo "Agent instruction budget passed: ${largest_combined_bytes}/${agent_instruction_budget} bytes (${largest_combined_path})"
echo "Agent discovery and worktree setup contracts passed"
