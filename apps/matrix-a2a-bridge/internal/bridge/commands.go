package bridge

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	askCommand      = "/ask"
	askAlias        = "!ask"
	agentsCommand   = "/agents"
	budgetCommand   = "/budget"
	budgetAlias     = "!budget"
	commandScope    = "/commands"
	maxBudgetAgents = maxDirectoryAgents
)

type plaintextCommandKind uint8

const (
	plaintextCommandNone plaintextCommandKind = iota
	plaintextCommandAsk
	plaintextCommandAgents
	plaintextCommandBudget
	plaintextCommandInvalid
)

type plaintextCommand struct {
	kind   plaintextCommandKind
	agent  string
	prompt string
	query  string
}

type textMessageClassification struct {
	command    plaintextCommand
	targets    targetResolution
	prompt     string
	knownAgent bool
}

func parsePlaintextCommand(body string) plaintextCommand {
	name, rest := splitLeadingToken(body)
	if !strings.HasPrefix(name, "/") && name != askAlias && name != budgetAlias {
		return plaintextCommand{}
	}
	switch name {
	case askCommand, askAlias:
		agent, prompt := splitLeadingToken(rest)
		if agent == "" || prompt == "" {
			return plaintextCommand{kind: plaintextCommandInvalid}
		}
		return plaintextCommand{kind: plaintextCommandAsk, agent: agent, prompt: prompt}
	case agentsCommand:
		query, extra := splitLeadingToken(rest)
		if extra != "" {
			return plaintextCommand{kind: plaintextCommandInvalid}
		}
		return plaintextCommand{kind: plaintextCommandAgents, query: query}
	case budgetCommand, budgetAlias:
		if strings.TrimSpace(rest) != "" {
			return plaintextCommand{kind: plaintextCommandInvalid}
		}
		return plaintextCommand{kind: plaintextCommandBudget}
	default:
		return plaintextCommand{kind: plaintextCommandInvalid}
	}
}

func splitLeadingToken(value string) (string, string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", ""
	}
	index := strings.IndexFunc(value, unicode.IsSpace)
	if index < 0 {
		return value, ""
	}
	return value[:index], strings.TrimSpace(value[index:])
}

func (b *Bridge) resolveAskCommand(evt *event.Event, command plaintextCommand) (targetResolution, bool) {
	localpart, _, ok := b.directoryTarget(command.agent)
	if !ok {
		return targetResolution{
			sender: b.agents.IdentifySender(evt.Sender),
			refs:   make(map[string]*AgentRef),
		}, false
	}
	message := &event.MessageEventContent{
		MsgType: event.MsgText,
		Mentions: &event.Mentions{
			UserIDs: []id.UserID{id.NewUserID(localpart, b.cfg.ServerName)},
		},
	}
	return b.resolveTargets(evt, message), true
}

// classifyTextMessage is the shared pre-ACK and handler classifier. Keeping command-to-target
// normalization here prevents durable intake and legacy dispatch from applying different policy.
func (b *Bridge) classifyTextMessage(
	evt *event.Event,
	message *event.MessageEventContent,
) textMessageClassification {
	command := parsePlaintextCommand(message.Body)
	classification := textMessageClassification{command: command}
	if command.kind == plaintextCommandAsk {
		classification.targets, classification.knownAgent = b.resolveAskCommand(evt, command)
		classification.prompt = command.prompt
		return classification
	}
	if command.kind == plaintextCommandNone {
		classification.targets = b.resolveTargets(evt, message)
		classification.prompt = b.stripMentions(message.Body)
	}
	return classification
}

func (b *Bridge) handleCommandNotice(
	ctx context.Context,
	evt *event.Event,
	scope string,
	body func() string,
) bool {
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.allowNotice(sender, evt.RoomID, scope) {
		b.log.Info(
			"suppressing command response after notice rate limit",
			"sender_origin_network", sender.origin.network,
			"room", evt.RoomID,
		)
		return false
	}
	intent := b.as.BotIntent()
	if intent == nil {
		b.log.Error("create bot intent for command response")
		return false
	}
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure command bot registered", "user", intent.UserID, "err", err)
		return false
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("join command bot to room", "user", intent.UserID, "room", evt.RoomID, "err", err)
		return false
	}
	return b.postReply(ctx, intent, evt, body()) != ""
}

func commandHelpText() string {
	return "Command not recognized. Use !ask <agent> <prompt>, !agents [name], or !budget. The /ask, /agents, and /budget forms also work when your Matrix client sends leading slashes unchanged."
}

func unknownCommandAgentText() string {
	return "No invocable agent with that name is available. Run !agents to list agents you may use."
}

func (b *Bridge) budgetText(senderID id.UserID, roomID id.RoomID) string {
	b.agentConfigMu.RLock()
	defer b.agentConfigMu.RUnlock()

	sender := b.agents.IdentifySender(senderID)
	room := b.roomLimits.snapshot(roomID.String())
	lines := []string{
		"Current configured limits (read-only):",
		fmt.Sprintf(
			"- Room invocation rate: %g/minute, burst %d; %d whole request(s) available now.",
			room.perMinute, room.burst, room.available,
		),
		fmt.Sprintf(
			"- Sender + agent invocation rate: %g/minute, burst %d.",
			b.senderLimits.perMinute, b.senderLimits.burst,
		),
	}

	var remoteReservations []string
	visible := 0
	hidden := 0
	for _, entry := range b.agents.Entries() {
		if !entry.Ref.AllowsSender(sender, b.cfg.ServerName) {
			continue
		}
		if visible == maxBudgetAgents {
			hidden++
			continue
		}
		visible++
		status := b.senderLimits.snapshot(sender.rateLimitKey(entry.Ghost))
		lines = append(lines, fmt.Sprintf("  - %s: %d whole request(s) available now.", entry.Ghost, status.available))
		if budget := entry.Ref.Target().TokenBudget(); budget > 0 {
			reservation := fmt.Sprintf("  - %s: maxTokens %d per request", entry.Ghost, budget)
			if maxCost := entry.Ref.MaxCost(); maxCost > 0 {
				reservation += fmt.Sprintf("; maxCost %d credit units", maxCost)
			}
			remoteReservations = append(remoteReservations, reservation+".")
		}
	}
	if visible == 0 {
		lines = append(lines, "  - No invocable agents are currently visible to you.")
	}
	if hidden > 0 {
		lines = append(lines, fmt.Sprintf("  - … %d more authorized agent limit(s) omitted.", hidden))
	}
	lines = append(lines, "Remote per-request token reservations:")
	if len(remoteReservations) == 0 {
		lines = append(lines, "  - None for the visible invocable agents.")
	} else {
		lines = append(lines, remoteReservations...)
	}
	lines = append(
		lines,
		"These are admission limits and reservation ceilings, not observed or spent token consumption. Reading them does not consume invocation capacity.",
	)
	return strings.Join(lines, "\n")
}
