package bridge

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	agentDirectoryCommand         = "!agents"
	maxDirectoryAgents            = 20
	maxDirectoryDescriptionRunes  = 240
	maxDirectorySummarySkillCount = 3
	maxDirectoryDetailSkillCount  = 10
)

func isAgentDirectoryCommand(body string) bool {
	fields := strings.Fields(body)
	return len(fields) > 0 && fields[0] == agentDirectoryCommand
}

func agentDirectoryQuery(body string) string {
	fields := strings.Fields(body)
	if len(fields) < 2 || fields[0] != agentDirectoryCommand {
		return ""
	}
	return fields[1]
}

func (b *Bridge) handleAgentDirectory(ctx context.Context, evt *event.Event, query string) {
	if b.handleCommandNotice(ctx, evt, agentDirectoryCommand, func() string {
		if query != "" {
			return b.agentDirectoryDetailText(evt.Sender, query)
		}
		return b.agentDirectoryText(evt.Sender)
	}) {
		b.log.Info("served local agent directory", "sender", evt.Sender, "room", evt.RoomID)
	}
}

func (b *Bridge) agentDirectoryText(sender id.UserID) string {
	// Keep the routing and profile snapshots coherent across a remap. Card refreshes may still
	// move a profile from fallback to live while this renders, which is safe and intentional.
	b.agentConfigMu.RLock()
	defer b.agentConfigMu.RUnlock()
	identity := b.agents.IdentifySender(sender)
	var lines []string
	hiddenByLimit := 0
	for _, entry := range b.agents.Entries() {
		if !entry.Ref.AllowsSender(identity, b.cfg.ServerName) {
			continue
		}
		if len(lines) == maxDirectoryAgents {
			hiddenByLimit++
			continue
		}
		profile, ok := b.profiles.get(entry.Ghost)
		if !ok {
			profile = fallbackProfile(entry.Ref)
		}
		if b.directoryEntryUnavailable(entry, profile) {
			fallback := fallbackProfile(entry.Ref)
			lines = append(lines, fmt.Sprintf(
				"- %s — %s — remote · unavailable (AgentCard trust required) — capabilities hidden",
				fallback.DisplayName,
				id.NewUserID(entry.Ghost, b.cfg.ServerName),
			))
			continue
		}
		description := directoryDescription(profile.Description)
		lines = append(lines, fmt.Sprintf(
			"- %s — %s — %s · %s — %s%s",
			profile.DisplayName,
			id.NewUserID(entry.Ghost, b.cfg.ServerName),
			directoryTargetKind(entry.Ref),
			profileStatusText(profile.Status),
			description,
			directorySkillsSummary(profile.Skills, maxDirectorySummarySkillCount),
		))
	}
	if len(lines) == 0 {
		return fmt.Sprintf(
			"No mapped agents are available to %s. Ask an operator to review the sender allowlists.",
			sender,
		)
	}
	if hiddenByLimit > 0 {
		lines = append(lines, fmt.Sprintf(
			"- … %d more authorized agent(s); use %s <name> or %s <name> for a specific mapping.",
			hiddenByLimit, agentDirectoryCommand, agentsCommand,
		))
	}
	return fmt.Sprintf(
		"Agents available to %s:\n%s\n\nUse %s <name> or %s <name> for details. Delegate with %s <name> <prompt> or mention an agent by its full MXID.",
		sender,
		strings.Join(lines, "\n"),
		agentDirectoryCommand,
		agentsCommand,
		askCommand,
	)
}

func (b *Bridge) agentDirectoryDetailText(sender id.UserID, query string) string {
	b.agentConfigMu.RLock()
	defer b.agentConfigMu.RUnlock()
	localpart, ref, ok := b.directoryTarget(query)
	identity := b.agents.IdentifySender(sender)
	if !ok || !ref.AllowsSender(identity, b.cfg.ServerName) {
		return fmt.Sprintf(
			"No invocable agent named %q is available to %s. Run %s or %s to list available agents.",
			normalizeProfileText(query, maxProfileNameRunes), sender, agentDirectoryCommand, agentsCommand,
		)
	}
	entry := AgentEntry{Ghost: localpart, Ref: ref}
	profile, found := b.profiles.get(localpart)
	if !found {
		profile = fallbackProfile(ref)
	}
	ghost := id.NewUserID(localpart, b.cfg.ServerName)
	if b.directoryEntryUnavailable(entry, profile) {
		fallback := fallbackProfile(ref)
		return fmt.Sprintf(
			"%s (%s)\nType: remote\nStatus: unavailable (AgentCard trust required)\nCapabilities: hidden until the card is verified.",
			fallback.DisplayName, ghost,
		)
	}
	return fmt.Sprintf(
		"%s (%s)\nType: %s\nStatus: %s\nDescription: %s\nDeclared skills/capabilities: %s",
		profile.DisplayName,
		ghost,
		directoryTargetKind(ref),
		profileStatusText(profile.Status),
		directoryDescription(profile.Description),
		directorySkillList(profile.Skills, maxDirectoryDetailSkillCount),
	)
}

func (b *Bridge) directoryTarget(query string) (string, *AgentRef, bool) {
	localpart := strings.TrimSpace(query)
	if strings.HasPrefix(localpart, "@") {
		localpart = strings.TrimPrefix(localpart, "@")
		var server string
		localpart, server, _ = strings.Cut(localpart, ":")
		if server != b.cfg.ServerName {
			return "", nil, false
		}
	}
	if ref, ok := b.agents.Lookup(localpart); ok {
		return localpart, ref, true
	}
	if !strings.HasPrefix(localpart, b.cfg.GhostPrefix) {
		localpart = b.cfg.GhostPrefix + localpart
		if ref, ok := b.agents.Lookup(localpart); ok {
			return localpart, ref, true
		}
	}
	return "", nil, false
}

func (b *Bridge) directoryEntryUnavailable(entry AgentEntry, profile agentProfile) bool {
	return entry.Ref.Target().IsRemote() &&
		(b.client == nil || !b.client.IsReady(entry.Ref.Target()) ||
			profile.Status == profileStatusRejected || profile.Status == profileStatusUnavailable)
}

func directoryTargetKind(ref *AgentRef) string {
	if ref.Target().IsRemote() {
		return "remote"
	}
	return "local"
}

func directoryDescription(description string) string {
	description = normalizeProfileText(description, maxDirectoryDescriptionRunes)
	if description == "" {
		return "No description published."
	}
	return description
}

func directorySkillsSummary(skills []string, limit int) string {
	if len(skills) == 0 {
		return " — no skills declared"
	}
	return " — skills: " + directorySkillList(skills, limit)
}

func directorySkillList(skills []string, limit int) string {
	if len(skills) == 0 {
		return "none declared"
	}
	visible := skills[:min(len(skills), limit)]
	result := strings.Join(visible, ", ")
	if hidden := len(skills) - len(visible); hidden > 0 {
		result += fmt.Sprintf(" (+%d more)", hidden)
	}
	return result
}

func profileStatusText(status profileStatus) string {
	switch status {
	case profileStatusLive:
		return "AgentCard live"
	case profileStatusCached:
		return "AgentCard cached (refresh failed)"
	case profileStatusRejected:
		return "AgentCard rejected"
	case profileStatusUnavailable:
		return "AgentCard unavailable"
	default:
		return "AgentCard unavailable (configured fallback)"
	}
}
