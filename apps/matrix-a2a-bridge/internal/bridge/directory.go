package bridge

import (
	"context"
	"fmt"
	"strings"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const agentDirectoryCommand = "!agents"

func isAgentDirectoryCommand(body string) bool {
	fields := strings.Fields(body)
	return len(fields) > 0 && fields[0] == agentDirectoryCommand
}

func (b *Bridge) handleAgentDirectory(ctx context.Context, evt *event.Event) {
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.allowNotice(sender, evt.RoomID, agentDirectoryCommand) {
		b.log.Info(
			"suppressing agent directory response after notice rate limit",
			"sender_origin_network", sender.origin.network,
			"room", evt.RoomID,
		)
		return
	}
	intent := b.as.BotIntent()
	if intent == nil {
		b.log.Error("create bot intent for agent directory")
		return
	}
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure directory bot registered", "user", intent.UserID, "err", err)
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("join directory bot to room", "user", intent.UserID, "room", evt.RoomID, "err", err)
		return
	}
	body := b.agentDirectoryText(evt.Sender)
	b.postReply(ctx, intent, evt, body)
	b.log.Info("served local agent directory", "sender", evt.Sender, "room", evt.RoomID)
}

func (b *Bridge) agentDirectoryText(sender id.UserID) string {
	// Keep the routing and profile snapshots coherent across a remap. Card refreshes may still
	// move a profile from fallback to live while this renders, which is safe and intentional.
	b.agentConfigMu.RLock()
	defer b.agentConfigMu.RUnlock()
	identity := b.agents.IdentifySender(sender)
	var lines []string
	for _, entry := range b.agents.Entries() {
		if !entry.Ref.AllowsSender(identity, b.cfg.ServerName) {
			continue
		}
		if entry.Ref.Target().IsRemote() && (b.client == nil || !b.client.IsReady(entry.Ref.Target())) {
			continue
		}
		profile, ok := b.profiles.get(entry.Ghost)
		if !ok {
			profile = fallbackProfile(entry.Ref)
		}
		if profile.Status == profileStatusRejected || profile.Status == profileStatusUnavailable {
			continue
		}
		description := profile.Description
		if description == "" {
			description = "No description published."
		}
		lines = append(lines, fmt.Sprintf(
			"- %s — %s — allowed · %s — %s",
			profile.DisplayName,
			id.NewUserID(entry.Ghost, b.cfg.ServerName),
			profileStatusText(profile.Status),
			description,
		))
	}
	if len(lines) == 0 {
		return fmt.Sprintf(
			"No mapped agents are available to %s. Ask an operator to review the sender allowlists.",
			sender,
		)
	}
	return fmt.Sprintf(
		"Agents available to %s:\n%s\n\nMention an agent by its full MXID.",
		sender,
		strings.Join(lines, "\n"),
	)
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
