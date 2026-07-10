package bridge

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind/matrix-a2a-bridge/internal/config"
	"github.com/fmind/matrix-a2a-bridge/internal/state"
)

const (
	// pollInitial/pollMax bound the tasks/get backoff for long-running tasks (SPEC §6).
	pollInitial = 2 * time.Second
	pollMax     = 15 * time.Second
	// pollErrorBudget tolerates transient tasks/get failures before giving up on a task.
	pollErrorBudget = 3

	workingText     = "⏳ working on it…"
	emptyReplyText  = "(the agent returned no content)"
	rateLimitedText = "⚠️ rate limit reached — please retry in a moment."
)

// Bridge orchestrates the @mention -> A2A -> reply flow for one appservice.
type Bridge struct {
	cfg    config.Config
	as     *appservice.AppService
	agents *AgentMap
	client *a2aclient.Client
	store  state.Store
	log    *slog.Logger

	mentionRe    *regexp.Regexp
	dispatcher   *dispatcher
	senderLimits *limiters
	roomLimits   *limiters

	runCtx context.Context // process lifetime; delegations run under it, not the handler ctx
}

// New builds a Bridge. The mention regex is the plaintext-body fallback used when a client
// does not populate the typed m.mentions field; it captures an optional ":server" suffix so
// federated look-alikes can be rejected (SPEC §4 F6).
func New(cfg config.Config, as *appservice.AppService, agents *AgentMap, client *a2aclient.Client, store state.Store, log *slog.Logger) *Bridge {
	mentionRe := regexp.MustCompile(
		`@(` + regexp.QuoteMeta(cfg.GhostPrefix) + `[a-zA-Z0-9._=\-]+)(:[a-zA-Z0-9.\-]+(?::\d+)?)?`,
	)
	return &Bridge{
		cfg:          cfg,
		as:           as,
		agents:       agents,
		client:       client,
		store:        store,
		log:          log,
		mentionRe:    mentionRe,
		dispatcher:   newDispatcher(cfg.Concurrency),
		senderLimits: newLimiters(cfg.SenderRatePerMinute, cfg.SenderRateBurst),
		roomLimits:   newLimiters(cfg.RoomRatePerMinute, cfg.RoomRateBurst),
	}
}

// Start binds the bridge to the process lifetime context under which delegations run.
func (b *Bridge) Start(ctx context.Context) {
	b.runCtx = ctx
}

// Stop waits for in-flight delegations to finish (graceful shutdown).
func (b *Bridge) Stop() {
	b.dispatcher.Wait()
}

// HandleMessage is the appservice event handler for m.room.message events. It only classifies
// and enqueues (SPEC §4 F3): the A2A round trip happens on the dispatcher's worker pool, with
// per-room FIFO ordering and a global concurrency cap.
func (b *Bridge) HandleMessage(ctx context.Context, evt *event.Event) {
	if b.isOwnUser(evt.Sender) {
		return // ignore our own bot/ghost messages — otherwise replies would loop
	}
	msg := evt.Content.AsMessage()
	// m.notice is bot output by Matrix convention (our ghosts reply with it); never treating it
	// as a delegating message breaks agent-to-agent reply loops (SPEC §4 F8).
	if msg == nil || msg.MsgType != event.MsgText {
		return
	}
	targets := b.resolveTargets(evt, msg)
	if len(targets) == 0 {
		return
	}
	// At-least-once transaction delivery collapses to effectively-once invocation (SPEC §4 F4).
	// On store failure we proceed anyway: a rare duplicate beats dropping the delegation.
	first, err := b.store.MarkEventProcessed(ctx, evt.ID.String())
	if err != nil {
		b.log.Error("event dedup check failed, proceeding", "event", evt.ID, "err", err)
	} else if !first {
		b.log.Info("skipping already-processed event", "event", evt.ID)
		return
	}
	prompt := b.stripMentions(msg.Body)
	for _, localpart := range targets {
		b.dispatcher.Enqueue(b.runCtx, evt.RoomID, func(ctx context.Context) {
			b.dispatch(ctx, evt, localpart, prompt)
		})
	}
}

// dispatch delegates one prompt to one agent ghost and posts the reply back.
func (b *Bridge) dispatch(ctx context.Context, evt *event.Event, localpart, prompt string) {
	ref, ok := b.agents.Lookup(localpart)
	if !ok {
		b.log.Warn("no agent mapped for ghost", "ghost", localpart)
		return
	}
	ghost := id.NewUserID(localpart, b.cfg.ServerName)
	intent := b.as.Intent(ghost)
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure ghost registered", "ghost", ghost, "err", err)
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("ensure ghost joined", "ghost", ghost, "room", evt.RoomID, "err", err)
		return
	}

	// LLM-spend guards (SPEC §4 F7): per (sender, agent) and per room.
	if !b.senderLimits.Allow(evt.Sender.String()+"|"+localpart) || !b.roomLimits.Allow(evt.RoomID.String()) {
		b.log.Warn("rate limited", "sender", evt.Sender, "ghost", localpart, "room", evt.RoomID)
		b.postReply(ctx, intent, evt, rateLimitedText)
		return
	}

	// Typing feedback while the agent works; best-effort, cleared on exit.
	if _, err := intent.UserTyping(ctx, evt.RoomID, true, b.cfg.RequestTimeout); err != nil {
		b.log.Debug("set typing", "ghost", ghost, "err", err)
	}
	defer func() {
		if _, err := intent.UserTyping(ctx, evt.RoomID, false, 0); err != nil {
			b.log.Debug("clear typing", "ghost", ghost, "err", err)
		}
	}()

	contextID, err := b.store.Context(ctx, evt.RoomID.String(), localpart)
	if err != nil {
		b.log.Error("load context, starting fresh thread", "room", evt.RoomID, "ghost", localpart, "err", err)
	}

	callCtx, cancel := context.WithTimeout(a2aclient.WithUser(ctx, evt.Sender.String()), b.cfg.RequestTimeout)
	res, err := b.client.Call(callCtx, ref.Path(), prompt, contextID)
	cancel()
	if err != nil {
		b.log.Error("a2a call failed", "agent", ref.Path(), "room", evt.RoomID, "err", err)
		// Deliberately generic: internal endpoints/errors must not leak into rooms (SPEC §6).
		b.postReply(ctx, intent, evt, fmt.Sprintf("⚠️ could not reach agent %q — see the bridge logs.", localpart))
		return
	}
	if res.ContextID != "" {
		if err := b.store.SetContext(ctx, evt.RoomID.String(), localpart, res.ContextID); err != nil {
			b.log.Error("store context", "room", evt.RoomID, "ghost", localpart, "err", err)
		}
	}

	if !res.Terminal {
		b.awaitTask(ctx, intent, evt, ref, localpart, res)
		return
	}
	b.postReply(ctx, intent, evt, orDefault(res.Text, emptyReplyText))
	b.log.Info("delegated to agent", "ghost", localpart, "agent", ref.Path(), "room", evt.RoomID)
}

// awaitTask handles a long-running task (SPEC §6): post a working placeholder, poll tasks/get
// with backoff until the task is terminal or TaskTimeout elapses, then edit the placeholder
// into the final answer (Matrix edits are the open-standard substitute for streaming).
func (b *Bridge) awaitTask(ctx context.Context, intent *appservice.IntentAPI, evt *event.Event, ref *AgentRef, localpart string, res a2aclient.Result) {
	placeholder := b.postReply(ctx, intent, evt, orDefault(res.Text, workingText))

	pollCtx, cancel := context.WithTimeout(a2aclient.WithUser(ctx, evt.Sender.String()), b.cfg.TaskTimeout)
	defer cancel()

	delay, errors := pollInitial, 0
	for {
		select {
		case <-pollCtx.Done():
			b.editReply(ctx, intent, evt.RoomID, placeholder,
				fmt.Sprintf("⚠️ agent %q did not finish within %s.", localpart, b.cfg.TaskTimeout))
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > pollMax {
			delay = pollMax
		}

		polled, err := b.client.PollTask(pollCtx, ref.Path(), res.TaskID)
		if err != nil {
			if errors++; errors < pollErrorBudget {
				b.log.Warn("tasks/get failed, retrying", "task", res.TaskID, "err", err)
				continue
			}
			b.log.Error("tasks/get failed", "task", res.TaskID, "agent", ref.Path(), "err", err)
			b.editReply(ctx, intent, evt.RoomID, placeholder,
				fmt.Sprintf("⚠️ lost track of agent %q's task — see the bridge logs.", localpart))
			return
		}
		errors = 0
		if polled.Terminal {
			b.editReply(ctx, intent, evt.RoomID, placeholder, orDefault(polled.Text, emptyReplyText))
			b.log.Info("delegated to agent (long task)", "ghost", localpart, "agent", ref.Path(), "room", evt.RoomID)
			return
		}
	}
}

// postReply sends text into the room as the ghost, as an m.notice reply to the original message
// (notice, so other bots/agents ignore it by convention — SPEC §4 F8). Returns the event ID.
func (b *Bridge) postReply(ctx context.Context, intent *appservice.IntentAPI, evt *event.Event, text string) id.EventID {
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	content.SetReply(evt) // m.relates_to reply pointing at the human's original message
	resp, err := intent.SendMessageEvent(ctx, evt.RoomID, event.EventMessage, content)
	if err != nil {
		b.log.Error("post reply", "room", evt.RoomID, "err", err)
		return ""
	}
	return resp.EventID
}

// editReply replaces a previously-posted reply (m.replace); falls back to logging when the
// placeholder was never posted.
func (b *Bridge) editReply(ctx context.Context, intent *appservice.IntentAPI, roomID id.RoomID, target id.EventID, text string) {
	if target == "" {
		b.log.Error("no placeholder to edit", "room", roomID, "text", text)
		return
	}
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	content.SetEdit(target)
	if _, err := intent.SendMessageEvent(ctx, roomID, event.EventMessage, content); err != nil {
		b.log.Error("edit reply", "room", roomID, "err", err)
	}
}

// resolveTargets returns the mapped agent ghost local-parts a message addresses, from the typed
// m.mentions field first (MSC3952), then a plaintext-body fallback. Only local ghosts survive
// (a federated @agent-x:other.org must never resolve to the local agent — SPEC §4 F6), and only
// when the sender passes the agent's policy.
func (b *Bridge) resolveTargets(evt *event.Event, msg *event.MessageEventContent) []string {
	seen := make(map[string]struct{})
	var out []string
	add := func(uid id.UserID) {
		localpart := uid.Localpart()
		if !strings.HasPrefix(localpart, b.cfg.GhostPrefix) {
			return
		}
		if uid.Homeserver() != b.cfg.ServerName {
			b.log.Warn("ignoring agent-like mention from foreign homeserver",
				"mention", uid, "sender", evt.Sender)
			return
		}
		ref, ok := b.agents.Lookup(localpart)
		if !ok {
			return // unknown/unmapped target — reject fast
		}
		if !ref.AllowsSender(evt.Sender, b.cfg.ServerName) {
			b.log.Warn("sender not allowed to invoke agent",
				"sender", evt.Sender, "ghost", localpart, "room", evt.RoomID)
			return
		}
		if _, dup := seen[localpart]; dup {
			return
		}
		seen[localpart] = struct{}{}
		out = append(out, localpart)
	}
	if msg.Mentions != nil {
		for _, uid := range msg.Mentions.UserIDs {
			add(uid)
		}
	}
	for _, m := range b.mentionRe.FindAllStringSubmatch(msg.Body, -1) {
		server := strings.TrimPrefix(m[2], ":")
		if server == "" {
			server = b.cfg.ServerName // bare "@agent-x" in text means a local ghost
		}
		add(id.NewUserID(m[1], server))
	}
	return out
}

// stripMentions removes "@agent-*[:server]" tokens so the agent receives a clean task prompt.
func (b *Bridge) stripMentions(body string) string {
	stripped := strings.TrimSpace(b.mentionRe.ReplaceAllString(body, ""))
	if stripped == "" {
		return body // the message was only a mention — send it verbatim rather than nothing
	}
	return stripped
}

// isOwnUser reports whether sender is the bridge's own bot or one of its agent ghosts.
func (b *Bridge) isOwnUser(sender id.UserID) bool {
	if sender.Homeserver() != b.cfg.ServerName {
		return false
	}
	localpart := sender.Localpart()
	return localpart == b.as.Registration.SenderLocalpart || strings.HasPrefix(localpart, b.cfg.GhostPrefix)
}

func orDefault(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
