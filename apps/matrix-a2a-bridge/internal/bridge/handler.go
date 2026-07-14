package bridge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	// pollInitial/pollMax bound the tasks/get backoff for long-running tasks (SPEC §6).
	pollInitial = 2 * time.Second
	pollMax     = 15 * time.Second
	// pollErrorBudget tolerates transient tasks/get failures before giving up on a task.
	pollErrorBudget = 3

	workingText          = "⏳ working on it…"
	emptyReplyText       = "(the agent returned no content)"
	rateLimitedText      = "⚠️ rate limit reached — please retry in a moment."
	policyDeniedText     = "⚠️ you are not allowed to invoke this agent — ask an operator to review its sender allowlist."
	stageDeniedText      = "⚠️ this agent is a staging (dev) build and can only be invoked in its designated staging room."
	mediaInputDeniedText = "⚠️ the attached file was refused by the media policy (type, size, encryption, or a partner opt-out) — nothing was sent to the agent."

	provenanceStart = "--- BEGIN FGENTIC BRIDGE PROVENANCE ---"
	provenanceEnd   = "--- END FGENTIC BRIDGE PROVENANCE ---"
	contentStart    = "--- BEGIN UNTRUSTED MATRIX CONTENT ---"
	contentEnd      = "--- END UNTRUSTED MATRIX CONTENT ---"

	// automatedMixinKey stamps every bridge-authored event with the MSC3955 m.automated mixin, a
	// machine-readable "this is automation" marker at the content layer. It hardens the trust
	// boundary — automation must not act on agent replies (docs/security.md) — without brittle MXID
	// heuristics, so it stays federation-safe (no localpart matching). MSC3955's "Unstable prefix"
	// section deliberately reuses the PARENT MSC1767 namespace ("org.matrix.msc1767.*") rather than
	// its own, and interoperating implementations (e.g. Ruma) emit exactly this key; using
	// "org.matrix.msc3955.*" would be invisible to them — the non-portable outcome the issue rejects.
	// Flip to the stable "m.automated" only once the MSC lands.
	automatedMixinKey = "org.matrix.msc1767.automated"
	// newContentKey is the m.replace replacement block (event.MessageEventContent.NewContent's tag);
	// the mixin is mirrored inside it so edit-aware clients keep the marker after applying the edit.
	newContentKey = "m.new_content"

	delegationAuditSchema         = "fgentic.delegation.v1"
	delegationAuditStream         = "audit"
	tracerName                    = "github.com/fmind-ai/matrix-a2a-bridge/internal/bridge"
	traceEventA2AMessageSendError = "a2a.message.send.error"
	traceEventA2ATaskTimeout      = "a2a.task.timeout"
	traceEventA2ATaskPollError    = "a2a.task.poll.error"

	outcomeDeduplicated = "deduplicated"

	dedupVerdictAccepted   auditDedupVerdict = "accepted"
	dedupVerdictDuplicate  auditDedupVerdict = "duplicate"
	dedupVerdictCheckError auditDedupVerdict = "check_error"

	rateLimitVerdictNotChecked auditRateLimitVerdict = "not_checked"
	rateLimitVerdictAllowed    auditRateLimitVerdict = "allowed"
	rateLimitVerdictRejected   auditRateLimitVerdict = "rejected"
)

type auditDedupVerdict string

type auditRateLimitVerdict string

type targetResolution struct {
	sender        senderIdentity
	allowed       []string
	deniedBridged []string
	refs          map[string]*AgentRef
}

type a2aClient interface {
	Call(ctx context.Context, target a2aclient.Target, text, contextID string, files []a2aclient.InboundFile) (a2aclient.Result, error)
	Continue(ctx context.Context, target a2aclient.Target, text, contextID, taskID string) (a2aclient.Result, error)
	PollTask(ctx context.Context, target a2aclient.Target, taskID string) (a2aclient.Result, error)
	CancelTask(ctx context.Context, target a2aclient.Target, taskID string) error
	ResolveAgentCard(ctx context.Context, target a2aclient.Target) (*a2a.AgentCard, error)
	IsReady(target a2aclient.Target) bool
	QuoteAdmission(target a2aclient.Target, maxCost uint64) a2aclient.QuoteVerdict
}

type pollWaitFunc func(context.Context, time.Duration) error

// delegationAuditResult is the terminal, content-free audit outcome for one resolved target.
// Keeping it separate from ordinary diagnostic logs gives operators a stable evidence schema.
type delegationAuditResult struct {
	outcome          string
	terminalStage    string
	terminalReason   string
	duration         time.Duration
	dedupVerdict     auditDedupVerdict
	rateLimitVerdict auditRateLimitVerdict
	a2aAttempted     bool
	a2aUserID        string
	contextID        string
	taskID           string
	replyEventID     id.EventID
	canceledBy       string   // room member who canceled a long task (#98); empty otherwise
	activated        []string // A2A extensions the remote echoed as activated (#114); empty for local
	mediaIn          int      // files forwarded from the room to the agent (#115)
	mediaOut         int      // agent artifact files posted into the room (#115)
	mediaRejected    int      // files withheld in either direction by the media policy (#115)
}

// Bridge orchestrates the @mention -> A2A -> reply flow for one appservice.
type Bridge struct {
	cfg      config.Config
	as       *appservice.AppService
	agents   *AgentMap
	client   a2aClient
	store    state.Store
	log      *slog.Logger
	auditLog *slog.Logger

	mentionRe          *regexp.Regexp
	dispatcher         *dispatcher
	senderLimits       *limiters
	roomLimits         *limiters
	noticeSenderLimits *limiters
	noticeRoomLimits   *limiters
	pollInitial        time.Duration
	pollMax            time.Duration
	pollWait           pollWaitFunc
	profiles           *profileStore
	profileWriter      ghostProfileWriter
	inflight           *inflightRegistry
	openTasks          *openTaskRegistry   // input-required delegations awaiting a reply (#116)
	media              mediaPolicy         // MIME/size gate for files in both directions (#115)
	stagingRooms       map[string]struct{} // rooms where stage:dev agents may be invoked (#128)
	tracer             trace.Tracer
	watchWG            sync.WaitGroup
	watchCancel        context.CancelFunc
	agentConfigMu      sync.RWMutex

	runCtx context.Context // process lifetime; delegations run under it, not the handler ctx
}

// New builds a Bridge. The mention regex is the plaintext-body fallback used when a client
// does not populate the typed m.mentions field; it captures an optional ":server" suffix so
// federated look-alikes can be rejected (SPEC §4 F6).
func New(cfg config.Config, as *appservice.AppService, agents *AgentMap, client a2aClient, store state.Store, log *slog.Logger) *Bridge {
	mentionRe := regexp.MustCompile(
		`@(` + regexp.QuoteMeta(cfg.GhostPrefix) + `[a-zA-Z0-9._=\-]+)(:[a-zA-Z0-9.\-]+(?::\d+)?)?`,
	)
	bridge := &Bridge{
		cfg:                cfg,
		as:                 as,
		agents:             agents,
		client:             client,
		store:              store,
		log:                log,
		auditLog:           log.With("log_stream", delegationAuditStream),
		mentionRe:          mentionRe,
		dispatcher:         newDispatcher(cfg.Concurrency, cfg.RoomQueueCapacity, cfg.GlobalQueueCapacity),
		senderLimits:       newLimiters(cfg.SenderRatePerMinute, cfg.SenderRateBurst, cfg.RateLimitBucketCapacity),
		roomLimits:         newLimiters(cfg.RoomRatePerMinute, cfg.RoomRateBurst, cfg.RateLimitBucketCapacity),
		noticeSenderLimits: newLimiters(cfg.SenderRatePerMinute, cfg.SenderRateBurst, cfg.RateLimitBucketCapacity),
		noticeRoomLimits:   newLimiters(cfg.RoomRatePerMinute, cfg.RoomRateBurst, cfg.RateLimitBucketCapacity),
		pollInitial:        pollInitial,
		pollMax:            pollMax,
		pollWait:           waitForPoll,
		inflight:           newInflightRegistry(),
		openTasks:          newOpenTaskRegistry(),
		media:              newMediaPolicy(cfg),
		stagingRooms:       stagingRoomSet(cfg.StagingRooms),
		tracer:             otel.Tracer(tracerName),
	}
	bridge.profiles = newProfileStore(agents.Entries())
	bridge.profileWriter = &matrixProfileWriter{as: as, log: log}
	return bridge
}

// stagingRoomSet indexes the configured staging room IDs for O(1) membership checks (#128).
func stagingRoomSet(rooms []string) map[string]struct{} {
	set := make(map[string]struct{}, len(rooms))
	for _, room := range rooms {
		set[room] = struct{}{}
	}
	return set
}

// isStagingRoom reports whether roomID is a configured staging room where stage:dev agents run.
func (b *Bridge) isStagingRoom(roomID id.RoomID) bool {
	_, ok := b.stagingRooms[roomID.String()]
	return ok
}

func waitForPoll(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Start binds the bridge to the process lifetime context under which delegations run. Remote
// targets are verified synchronously so readiness can never expose an untrusted destination.
func (b *Bridge) Start(ctx context.Context) error {
	b.runCtx = ctx
	watchCtx, watchCancel := context.WithCancel(ctx)
	if b.profileWriter != nil {
		prepareCtx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout)
		if err := b.profileWriter.Prepare(prepareCtx); err != nil {
			b.log.Warn("prepare Matrix profile synchronization; descriptions remain available through !agents", "err", err)
		}
		cancel()
	}
	if err := b.syncProfilesChecked(ctx, b.agents.Entries(), false); err != nil {
		watchCancel()
		return fmt.Errorf("verify configured remote agents: %w", err)
	}
	b.watchCancel = watchCancel
	b.watchWG.Add(1)
	go b.watchAgents(watchCtx)
	return nil
}

// Stop waits for in-flight delegations to finish (graceful shutdown).
func (b *Bridge) Stop() {
	if b.watchCancel != nil {
		b.watchCancel()
	}
	b.dispatcher.Wait()
	b.watchWG.Wait()
}

// HandleMessage is the appservice event handler for m.room.message events. It only classifies
// and enqueues (SPEC §4 F3): the A2A round trip happens on the dispatcher's worker pool, with
// per-room FIFO ordering and a global concurrency cap.
func (b *Bridge) HandleMessage(ctx context.Context, evt *event.Event) {
	if b.isOwnUser(evt.Sender) {
		return // ignore our own bot/ghost messages — otherwise replies would loop
	}
	msg := evt.Content.AsMessage()
	if msg == nil {
		return
	}
	// m.notice is bot output by Matrix convention (our ghosts reply with it); never treating it as a
	// delegating message breaks agent-to-agent reply loops (SPEC §4 F8). A media message that mentions
	// an agent IS a delegation carrying an inbound file (#115); every other non-text type is ignored.
	switch {
	case msg.MsgType == event.MsgText:
		// A threaded reply answering a paused agent question resumes that task instead of starting a
		// new delegation (#116); an ordinary message falls through to the mention path below.
		if b.handleThreadContinuation(ctx, evt, msg) {
			return
		}
		if isAgentDirectoryCommand(msg.Body) {
			if b.markEventProcessed(ctx, evt) != dedupVerdictDuplicate {
				b.handleAgentDirectory(ctx, evt)
			}
			return
		}
	case !msg.MsgType.IsMedia():
		return
	}
	targets := b.resolveTargets(evt, msg)
	if len(targets.allowed) == 0 && len(targets.deniedBridged) == 0 {
		return
	}
	dedupStarted := time.Now()
	dedupVerdict := b.markEventProcessed(ctx, evt)
	if dedupVerdict == dedupVerdictDuplicate {
		duration := time.Since(dedupStarted)
		for _, localpart := range append(targets.allowed, targets.deniedBridged...) {
			ref := targets.refs[localpart]
			b.logDelegationAudit(evt, ref, localpart, targets.sender, delegationAuditResult{
				outcome:          outcomeDeduplicated,
				terminalStage:    "dedup",
				terminalReason:   "duplicate_delivery",
				duration:         duration,
				dedupVerdict:     dedupVerdictDuplicate,
				rateLimitVerdict: rateLimitVerdictNotChecked,
			})
		}
		return
	}
	prompt := b.stripMentions(msg.Body)
	for _, localpart := range targets.allowed {
		b.enqueueResolvedTarget(evt, localpart, prompt, targets.refs[localpart], targets.sender, dedupVerdict)
	}
	for _, localpart := range targets.deniedBridged {
		b.enqueueResolvedTarget(evt, localpart, prompt, targets.refs[localpart], targets.sender, dedupVerdict)
	}
}

func (b *Bridge) enqueueResolvedTarget(
	evt *event.Event,
	localpart, prompt string,
	ref *AgentRef,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	result := b.dispatcher.Enqueue(
		b.runCtx,
		evt.RoomID,
		func(ctx context.Context) {
			b.dispatchResolvedTarget(ctx, evt, localpart, prompt, ref, sender, dedupVerdict)
		},
		func() {
			b.recordShutdownTarget(evt, ref, localpart, sender, dedupVerdict, "shutdown_queued_dropped")
		},
	)
	if result == enqueueAccepted {
		return
	}
	if result == enqueueStopped {
		b.recordShutdownTarget(evt, ref, localpart, sender, dedupVerdict, "shutdown_enqueue_rejected")
		return
	}
	if result != enqueueRoomFull && result != enqueueGlobalFull {
		return
	}
	delegationsTotal.WithLabelValues(localpart, outcomeQueueFull).Inc()
	b.log.Warn(
		"rejecting delegation because dispatcher capacity is exhausted",
		"ghost", localpart,
		"room", evt.RoomID,
		"reason", result.terminalReason(),
	)
	b.logDelegationAudit(evt, ref, localpart, sender, delegationAuditResult{
		outcome:          outcomeQueueFull,
		terminalStage:    "queue",
		terminalReason:   result.terminalReason(),
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
	})
}

// recordShutdownTarget is the terminal record for a resolved target that never started. Jobs
// already running produce their normal terminal audit after the runtime context is cancelled.
func (b *Bridge) recordShutdownTarget(
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
	reason string,
) {
	delegationsTotal.WithLabelValues(localpart, outcomeShutdown).Inc()
	b.log.Warn(
		"delegation terminated before dispatch during shutdown",
		"ghost", localpart,
		"room", evt.RoomID,
		"reason", reason,
	)
	b.logDelegationAudit(evt, ref, localpart, sender, delegationAuditResult{
		outcome:          outcomeShutdown,
		terminalStage:    "queue",
		terminalReason:   reason,
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
	})
}

func (b *Bridge) markEventProcessed(ctx context.Context, evt *event.Event) auditDedupVerdict {
	// At-least-once transaction delivery collapses to effectively-once invocation (SPEC §4 F4).
	// On store failure we proceed anyway: a rare duplicate beats dropping the delegation.
	first, err := b.store.MarkEventProcessed(ctx, evt.ID.String())
	if err != nil {
		b.log.Error("event dedup check failed, proceeding", "event", evt.ID, "err", err)
		return dedupVerdictCheckError
	} else if !first {
		dedupSkipsTotal.Inc()
		b.log.Info("skipping already-processed event", "event", evt.ID)
		return dedupVerdictDuplicate
	}
	return dedupVerdictAccepted
}

// HandleMembership auto-accepts invites addressed to the bridge's own users (the bot and MAPPED
// agent ghosts). This is what activates a room: Synapse only pushes room traffic to the
// appservice once one of its namespaced users is a member, so "invite @agent-x, then @mention
// it" hinges on the invite being accepted. Invites for unmapped agent-like users are ignored
// (the allowlist is the agents map — SPEC §4 D6).
func (b *Bridge) HandleMembership(ctx context.Context, evt *event.Event) {
	content := evt.Content.AsMember()
	if content == nil || content.Membership != event.MembershipInvite || evt.StateKey == nil {
		return
	}
	target := id.UserID(*evt.StateKey)
	if target.Homeserver() != b.cfg.ServerName {
		return
	}
	localpart := target.Localpart()
	if localpart != b.as.Registration.SenderLocalpart {
		if !strings.HasPrefix(localpart, b.cfg.GhostPrefix) {
			return
		}
		if _, ok := b.agents.Lookup(localpart); !ok {
			b.log.Warn("ignoring invite for unmapped ghost", "ghost", localpart, "room", evt.RoomID)
			return
		}
	}
	intent := b.as.Intent(target)
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure invited user registered", "user", target, "err", err)
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("join invited room", "user", target, "room", evt.RoomID, "err", err)
		return
	}
	b.log.Info("accepted room invite", "user", target, "room", evt.RoomID)
}

// handleThreadContinuation routes a threaded reply answering a paused agent question (#116) back
// into its task. It returns true when the message was consumed as a continuation — answered, refused
// as a wrong sender, or de-duplicated — and false for an ordinary message that follows the mention
// path. GetThreadParent tolerates a nil m.relates_to.
func (b *Bridge) handleThreadContinuation(ctx context.Context, evt *event.Event, msg *event.MessageEventContent) bool {
	root := msg.RelatesTo.GetThreadParent()
	if root == "" {
		return false
	}
	room, owner, ok := b.openTasks.owner(root)
	if !ok || evt.RoomID != room {
		// Not a reply under a paused task's placeholder in this room. Re-binding the room stops a
		// reply threaded under a foreign-room placeholder ID from resuming a task elsewhere.
		return false
	}
	// Only the original delegating sender may answer. A wrong-sender reply is refused once and never
	// consumes the pending answer slot, so the owner can still respond.
	if evt.Sender != owner {
		if b.markEventProcessed(ctx, evt) != dedupVerdictDuplicate {
			b.postThreadContinuationDenied(ctx, evt)
		}
		return true
	}
	if b.markEventProcessed(ctx, evt) == dedupVerdictDuplicate {
		return true
	}
	open, claimed := b.openTasks.claim(root)
	if !claimed {
		return true // expired or resumed between the owner check and the claim
	}
	msg.RemoveReplyFallback()
	answer := strings.TrimSpace(msg.Body)
	if answer == "" {
		answer = "(the sender replied with no text)"
	}
	b.enqueueContinuation(evt, open, answer)
	return true
}

// postThreadContinuationDenied tells a non-owner, once and content-free, that only the original
// requester may answer a pending agent question, on the notice rate-limit plane (D7).
func (b *Bridge) postThreadContinuationDenied(ctx context.Context, evt *event.Event) {
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.allowNotice(sender, evt.RoomID, "input-continuation") {
		return
	}
	intent := b.as.BotIntent()
	if intent == nil {
		return
	}
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("register bot for continuation denial", "err", err)
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("join bot for continuation denial", "room", evt.RoomID, "err", err)
		return
	}
	b.postReply(ctx, intent, evt, "⚠️ only the person who started this task can answer the agent's question.")
}

// enqueueContinuation schedules a resumed task on the per-room dispatcher so it keeps FIFO order and
// the concurrency cap. A full queue or a stopping dispatcher cannot resume it, so its placeholder is
// edited to an honest dropped notice with a terminal audit.
func (b *Bridge) enqueueContinuation(evt *event.Event, open *openTask, answer string) {
	result := b.dispatcher.Enqueue(
		b.runCtx,
		evt.RoomID,
		func(ctx context.Context) { b.continueOpenTask(ctx, evt, open, answer) },
		func() { b.abandonContinuation(evt, open, "shutdown_queued_dropped", outcomeShutdown) },
	)
	switch result {
	case enqueueAccepted:
		return
	case enqueueStopped:
		b.abandonContinuation(evt, open, "shutdown_enqueue_rejected", outcomeShutdown)
	default:
		b.abandonContinuation(evt, open, result.terminalReason(), outcomeQueueFull)
	}
}

// abandonContinuation drops a resumed task that never dispatched (queue full or shutdown), editing
// its placeholder and auditing the terminal reason against the answering reply.
func (b *Bridge) abandonContinuation(evt *event.Event, open *openTask, reason, outcome string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.runCtx), b.cfg.RequestTimeout)
	defer cancel()
	intent := b.as.Intent(id.NewUserID(open.localpart, b.cfg.ServerName))
	b.editReply(ctx, intent, open.origin.RoomID, open.placeholder,
		fmt.Sprintf("⚠️ could not resume agent %q's task — please start again.", open.localpart))
	delegationsTotal.WithLabelValues(open.localpart, outcome).Inc()
	b.logDelegationAudit(evt, open.ref, open.localpart, b.agents.IdentifySender(evt.Sender), delegationAuditResult{
		outcome:          outcome,
		terminalStage:    "queue",
		terminalReason:   reason,
		dedupVerdict:     dedupVerdictAccepted,
		rateLimitVerdict: rateLimitVerdictNotChecked,
		taskID:           open.taskID,
		contextID:        open.contextID,
		replyEventID:     open.placeholder,
	})
}

// continueOpenTask resumes a paused task with the sender's answer. It re-validates the mapping,
// sender policy, remote trust, and rate limits at resume time (config may have changed while paused),
// then message/sends the answer with the same taskID+contextID and follows the reused placeholder to
// a terminal state (or another pause). A rate-limited answer re-registers the task so it can retry.
func (b *Bridge) continueOpenTask(ctx context.Context, reply *event.Event, open *openTask, answer string) {
	inflightDelegations.Inc()
	defer inflightDelegations.Dec()
	started := time.Now()

	currentSender, ref, ok := b.agents.SnapshotSenderTarget(open.origin.Sender, open.localpart)
	if !ok || !ref.SameTarget(open.ref) {
		b.finishContinuation(ctx, reply, open, currentSender, started, "admission", "agent_mapping_changed", "the agent it was waiting on changed")
		return
	}
	if !ref.AllowsSender(currentSender, b.cfg.ServerName) {
		b.finishContinuation(ctx, reply, open, currentSender, started, "admission", "sender_policy_rejected", "you are no longer allowed to invoke this agent")
		return
	}
	// Re-apply the stage (#128) and cost (#142) admission gates: a resumed turn is a fresh
	// invocation, so a stage or price change while paused must still fail closed.
	if ref.IsDev() && !b.isStagingRoom(reply.RoomID) {
		b.finishContinuation(ctx, reply, open, currentSender, started, "admission", "stage_policy_rejected", "the agent is now confined to staging rooms")
		return
	}
	if ref.Target().IsRemote() && (b.client == nil || !b.client.IsReady(ref.Target())) {
		b.finishContinuation(ctx, reply, open, currentSender, started, "agent_card", "agent_card_untrusted", "trust in the agent changed")
		return
	}
	if ref.Target().IsRemote() && ref.MaxCost() > 0 && b.client != nil {
		switch b.client.QuoteAdmission(ref.Target(), ref.MaxCost()) {
		case a2aclient.QuoteOverBudget, a2aclient.QuoteMissing:
			b.finishContinuation(ctx, reply, open, currentSender, started, "admission", "quote_over_budget", "the agent's price now exceeds the configured budget")
			return
		}
	}
	if !b.senderLimits.Allow(currentSender.rateLimitKey(open.localpart)) || !b.roomLimits.Allow(reply.RoomID.String()) {
		// Re-register (fresh budget) so a rate-limited answer is retried, not lost.
		b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })
		intent := b.as.Intent(id.NewUserID(open.localpart, b.cfg.ServerName))
		if b.allowNotice(currentSender, reply.RoomID, open.localpart) {
			b.postReply(ctx, intent, reply, rateLimitedText)
		}
		delegationsTotal.WithLabelValues(open.localpart, outcomeRateLimited).Inc()
		b.logDelegationAudit(reply, ref, open.localpart, currentSender, delegationAuditResult{
			outcome: outcomeRateLimited, terminalStage: "admission", terminalReason: "rate_limit_rejected",
			a2aAttempted: false, dedupVerdict: dedupVerdictAccepted, rateLimitVerdict: rateLimitVerdictRejected,
			taskID: open.taskID, contextID: open.contextID, replyEventID: open.placeholder, duration: time.Since(started),
		})
		return
	}

	intent := b.as.Intent(id.NewUserID(open.localpart, b.cfg.ServerName))
	audit := delegationAuditResult{
		outcome: outcomeError, terminalStage: "message_send", terminalReason: "dispatch_failed",
		a2aAttempted: true, a2aUserID: reply.Sender.String(), contextID: open.contextID, taskID: open.taskID,
		replyEventID: open.placeholder, dedupVerdict: dedupVerdictAccepted, rateLimitVerdict: rateLimitVerdictAllowed,
	}
	defer func() {
		audit.duration = time.Since(started)
		b.logDelegationAudit(reply, ref, open.localpart, currentSender, audit)
	}()

	a2aCtx := a2aclient.WithUser(ctx, reply.Sender.String())
	cancelDelegation := func() {}
	if ref.Timeout() > 0 {
		a2aCtx, cancelDelegation = context.WithTimeout(a2aCtx, ref.Timeout())
	}
	defer cancelDelegation()
	callCtx, cancel := context.WithTimeout(a2aCtx, b.cfg.RequestTimeout)
	res, err := b.client.Continue(callCtx, ref.Target(), provenancePrompt(reply, answer), open.contextID, open.taskID)
	cancel()
	if err != nil {
		if errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
			delegationsTotal.WithLabelValues(open.localpart, outcomeDenied).Inc()
			audit.outcome = outcomeDenied
			audit.terminalStage = "agent_card"
			audit.terminalReason = "agent_card_untrusted"
			audit.a2aAttempted = false
			b.editReply(ctx, intent, reply.RoomID, open.placeholder, fmt.Sprintf("⚠️ lost trust in agent %q — the task was stopped.", open.localpart))
			return
		}
		delegationsTotal.WithLabelValues(open.localpart, outcomeError).Inc()
		audit.terminalReason = "a2a_call_failed"
		b.log.Error("a2a continuation failed", "agent", ref.Path(), "room", reply.RoomID, "err", err)
		b.editReply(ctx, intent, reply.RoomID, open.placeholder, fmt.Sprintf("⚠️ could not reach agent %q — see the bridge logs.", open.localpart))
		return
	}
	audit.terminalStage = "message_result"
	audit.contextID = orDefault(res.ContextID, open.contextID)
	audit.taskID = orDefault(res.TaskID, open.taskID)
	if res.ContextID != "" {
		if err := b.store.SetContext(ctx, reply.RoomID.String(), open.localpart, res.ContextID); err != nil {
			b.log.Error("store context", "room", reply.RoomID, "ghost", open.localpart, "err", err)
		}
	}
	if !res.Terminal {
		terminalAudit := b.awaitTask(ctx, a2aCtx, intent, reply, ref, open.localpart, res, open.placeholder)
		terminalAudit.dedupVerdict = audit.dedupVerdict
		terminalAudit.rateLimitVerdict = audit.rateLimitVerdict
		audit = terminalAudit
		return
	}
	if res.Failed {
		delegationsTotal.WithLabelValues(open.localpart, outcomeFailed).Inc()
		audit.outcome = outcomeFailed
		audit.terminalReason = "agent_failed"
		b.editReply(ctx, intent, reply.RoomID, open.placeholder, fmt.Sprintf("⚠️ agent %q could not complete the task — see the bridge logs.", open.localpart))
		return
	}
	delegationsTotal.WithLabelValues(open.localpart, outcomeOK).Inc()
	audit.outcome = outcomeOK
	audit.terminalReason = "completed"
	_, out, rejected := b.deliverReply(ctx, intent, reply, open.placeholder, open.localpart, ref, res)
	audit.mediaOut = out
	audit.mediaRejected += rejected
	b.log.Info("resumed and completed delegation", "ghost", open.localpart, "agent", ref.Path(), "room", reply.RoomID)
}

// finishContinuation edits the placeholder and audits a fail-closed resume refusal. Every refusal
// is a denial that never reached A2A, so the outcome and a2a_attempted are fixed; stage and reason
// name which admission gate rejected the resume.
func (b *Bridge) finishContinuation(ctx context.Context, reply *event.Event, open *openTask, sender senderIdentity, started time.Time, stage, reason, roomMsg string) {
	intent := b.as.Intent(id.NewUserID(open.localpart, b.cfg.ServerName))
	b.editReply(ctx, intent, open.origin.RoomID, open.placeholder, fmt.Sprintf("⚠️ could not resume the task — %s.", roomMsg))
	delegationsTotal.WithLabelValues(open.localpart, outcomeDenied).Inc()
	b.logDelegationAudit(reply, open.ref, open.localpart, sender, delegationAuditResult{
		outcome: outcomeDenied, terminalStage: stage, terminalReason: reason, a2aAttempted: false,
		dedupVerdict: dedupVerdictAccepted, rateLimitVerdict: rateLimitVerdictNotChecked,
		taskID: open.taskID, contextID: open.contextID, replyEventID: open.placeholder, duration: time.Since(started),
	})
}

func (b *Bridge) dispatchResolvedTarget(
	ctx context.Context,
	evt *event.Event,
	localpart, prompt string,
	boundRef *AgentRef,
	queuedSender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	currentSender, ref, ok := b.agents.SnapshotSenderTarget(evt.Sender, localpart)
	sender := revalidateSender(queuedSender, currentSender)
	if !ok || !ref.SameTarget(boundRef) {
		b.refuseQueuedTarget(evt, boundRef, localpart, sender, dedupVerdict, "agent_mapping_changed")
		return
	}
	if !ref.AllowsSender(sender, b.cfg.ServerName) {
		if sender.isBridged() {
			b.rejectBridgedSender(ctx, evt, ref, localpart, sender, dedupVerdict)
			return
		}
		b.refuseQueuedTarget(evt, ref, localpart, sender, dedupVerdict, "sender_policy_rejected")
		return
	}
	// Stage admission (#128): a stage:dev agent is confined to configured staging rooms. Elsewhere
	// it is refused fail-closed with a distinct reason, before any trust, cost, limiter, or A2A step.
	if ref.IsDev() && !b.isStagingRoom(evt.RoomID) {
		b.refuseStagedTarget(ctx, evt, ref, localpart, sender, dedupVerdict)
		return
	}
	if ref.Target().IsRemote() && (b.client == nil || !b.client.IsReady(ref.Target())) {
		b.refuseUntrustedTarget(evt, ref, localpart, sender, dedupVerdict)
		return
	}
	// Cost admission (#142): a configured per-remote maxCost refuses a delegation whose verified
	// skill quote is over budget or missing, before spending a limiter token or dispatching A2A.
	if ref.Target().IsRemote() && ref.MaxCost() > 0 && b.client != nil {
		switch b.client.QuoteAdmission(ref.Target(), ref.MaxCost()) {
		case a2aclient.QuoteOverBudget, a2aclient.QuoteMissing:
			b.refuseOverBudget(evt, ref, localpart, sender, dedupVerdict)
			return
		}
	}
	b.dispatchWithDedupVerdict(ctx, evt, ref, localpart, prompt, sender, dedupVerdict)
}

// refuseOverBudget fails a delegation closed because the remote's verified skill quote exceeds the
// configured maxCost (or is missing). It is an economic refusal, distinct from a trust failure: the
// card is trusted, but the price is not, so it never spends a limiter token or reaches A2A (#142).
func (b *Bridge) refuseOverBudget(
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
	b.log.Warn(
		"refusing delegation whose remote skill quote exceeds the configured budget",
		"ghost", localpart,
		"agent", ref.Path(),
		"max_cost", ref.MaxCost(),
	)
	b.logDelegationAudit(evt, ref, localpart, sender, delegationAuditResult{
		outcome:          outcomeDenied,
		terminalStage:    "admission",
		terminalReason:   "quote_over_budget",
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
		a2aAttempted:     false,
	})
}

// revalidateSender applies the current origin map at dispatch time. Once a queued event is
// bridge-derived, its kind/network attribution is immutable: rule removal or relabeling cannot
// downgrade policy or corrupt its audit and rate-limit identity.
func revalidateSender(queued, current senderIdentity) senderIdentity {
	if queued.mxid == current.mxid && queued.isBridged() && queued.origin != current.origin {
		return queued
	}
	return current
}

func (b *Bridge) refuseQueuedTarget(
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
	reason string,
) {
	delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
	b.log.Warn(
		"refusing queued delegation after agent configuration changed",
		"sender", evt.Sender,
		"ghost", localpart,
		"reason", reason,
	)
	b.logDelegationAudit(evt, ref, localpart, sender, delegationAuditResult{
		outcome:          outcomeDenied,
		terminalStage:    "admission",
		terminalReason:   reason,
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
	})
}

func (b *Bridge) refuseUntrustedTarget(
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
	b.log.Warn(
		"refusing delegation to quarantined remote agent",
		"ghost", localpart,
		"agent", ref.Path(),
	)
	b.logDelegationAudit(evt, ref, localpart, sender, delegationAuditResult{
		outcome:          outcomeDenied,
		terminalStage:    "agent_card",
		terminalReason:   "agent_card_untrusted",
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
		a2aAttempted:     false,
	})
}

func (b *Bridge) dispatchWithDedupVerdict(
	ctx context.Context,
	evt *event.Event,
	ref *AgentRef,
	localpart, prompt string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	inflightDelegations.Inc()
	defer inflightDelegations.Dec()
	auditStarted := time.Now()
	ctx, span := b.tracer.Start(
		ctx,
		"fgentic.delegation",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(
			attribute.String("matrix.room_id", evt.RoomID.String()),
			attribute.String("matrix.event_id", evt.ID.String()),
			attribute.String("matrix.sender", evt.Sender.String()),
			attribute.String("fgentic.sender_origin_kind", string(sender.origin.kind)),
			attribute.String("fgentic.sender_origin_network", sender.origin.network),
			attribute.String("fgentic.ghost", localpart),
			attribute.String("a2a.agent_path", ref.Path()),
			attribute.String("fgentic.dedup_verdict", string(dedupVerdict)),
			attribute.Bool("fgentic.dedup_skipped", dedupVerdict == dedupVerdictDuplicate),
		),
	)
	span.AddEvent("queue.dequeued")
	audit := delegationAuditResult{
		outcome:          outcomeError,
		terminalStage:    "dispatch",
		terminalReason:   "dispatch_failed",
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
	}
	defer func() {
		audit.duration = time.Since(auditStarted)
		span.SetAttributes(
			attribute.String("fgentic.outcome", audit.outcome),
			attribute.String("fgentic.terminal_stage", audit.terminalStage),
			attribute.String("fgentic.terminal_reason", audit.terminalReason),
			attribute.String("fgentic.rate_limit_verdict", string(audit.rateLimitVerdict)),
			attribute.Bool("fgentic.rate_limited", audit.outcome == outcomeRateLimited),
		)
		if audit.outcome != outcomeOK {
			span.SetStatus(codes.Error, audit.outcome)
		}
		span.End()
		b.logDelegationAudit(evt, ref, localpart, sender, audit)
	}()

	ghost := id.NewUserID(localpart, b.cfg.ServerName)
	intent := b.as.Intent(ghost)
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure ghost registered", "ghost", ghost, "err", err)
		audit.terminalStage = "matrix_register"
		audit.terminalReason = "matrix_registration_failed"
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("ensure ghost joined", "ghost", ghost, "room", evt.RoomID, "err", err)
		audit.terminalStage = "matrix_join"
		audit.terminalReason = "matrix_join_failed"
		return
	}

	// LLM-spend guards (SPEC §4 F7): per (sender, agent) and per room.
	if !b.senderLimits.Allow(sender.rateLimitKey(localpart)) || !b.roomLimits.Allow(evt.RoomID.String()) {
		delegationsTotal.WithLabelValues(localpart, outcomeRateLimited).Inc()
		b.log.Warn("rate limited", "sender", evt.Sender, "ghost", localpart, "room", evt.RoomID)
		audit.outcome = outcomeRateLimited
		audit.terminalStage = "admission"
		audit.terminalReason = "rate_limit_rejected"
		audit.rateLimitVerdict = rateLimitVerdictRejected
		if b.allowNotice(sender, evt.RoomID, localpart) {
			audit.replyEventID = b.postReply(ctx, intent, evt, rateLimitedText)
		}
		return
	}
	audit.rateLimitVerdict = rateLimitVerdictAllowed

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

	// Resolve any file the mention carries into A2A parts (#115). A referenced file that fails policy
	// fails the whole delegation closed: post a bounded notice and never spend the A2A call.
	inboundFiles, inRejected, mediaOK := b.collectInboundMedia(ctx, intent, evt, ref)
	if !mediaOK {
		delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
		audit.outcome = outcomeDenied
		audit.terminalStage = "media_admission"
		audit.terminalReason = "media_input_rejected"
		audit.mediaRejected = inRejected
		b.log.Warn("refusing delegation: attached file failed media policy", "ghost", localpart, "room", evt.RoomID)
		if b.allowNotice(sender, evt.RoomID, localpart) {
			audit.replyEventID = b.postReply(ctx, intent, evt, mediaInputDeniedText)
		}
		return
	}
	audit.mediaIn = len(inboundFiles)

	audit.terminalStage = "message_send"
	audit.a2aAttempted = true
	audit.a2aUserID = evt.Sender.String()
	audit.contextID = contextID

	a2aCtx := a2aclient.WithUser(ctx, evt.Sender.String())
	cancelDelegation := func() {}
	if ref.Timeout() > 0 {
		a2aCtx, cancelDelegation = context.WithTimeout(a2aCtx, ref.Timeout())
	}
	defer cancelDelegation()
	callCtx, cancel := context.WithTimeout(a2aCtx, b.cfg.RequestTimeout)
	callStarted := time.Now()
	span.AddEvent("a2a.message.send")
	res, err := b.client.Call(callCtx, ref.Target(), provenancePrompt(evt, prompt), contextID, inboundFiles)
	cancel()
	a2aLatency.WithLabelValues(localpart).Observe(time.Since(callStarted).Seconds())
	if err != nil {
		// Error values can contain remote response bodies. Preserve only a bounded event name;
		// the deferred audit attributes carry the reviewed machine-readable failure reason.
		span.AddEvent(traceEventA2AMessageSendError)
		if errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
			delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
			b.log.Warn("refusing delegation after remote agent trust changed", "agent", ref.Path(), "room", evt.RoomID)
			audit.outcome = outcomeDenied
			audit.terminalStage = "agent_card"
			audit.terminalReason = "agent_card_untrusted"
			audit.a2aAttempted = false
			return
		}
		delegationsTotal.WithLabelValues(localpart, outcomeError).Inc()
		b.log.Error("a2a call failed", "agent", ref.Path(), "room", evt.RoomID, "err", err)
		// Deliberately generic: internal endpoints/errors must not leak into rooms (SPEC §6).
		audit.terminalReason = "a2a_call_failed"
		audit.replyEventID = b.postReply(ctx, intent, evt, fmt.Sprintf("⚠️ could not reach agent %q — see the bridge logs.", localpart))
		return
	}
	span.AddEvent("a2a.message.result")
	audit.terminalStage = "message_result"
	audit.contextID = orDefault(res.ContextID, contextID)
	audit.taskID = res.TaskID
	audit.activated = res.ActivatedExtensions // extension set the remote echoed on message/send (#114)
	if res.ContextID != "" {
		if err := b.store.SetContext(ctx, evt.RoomID.String(), localpart, res.ContextID); err != nil {
			b.log.Error("store context", "room", evt.RoomID, "ghost", localpart, "err", err)
		}
	}

	if !res.Terminal {
		terminalAudit := b.awaitTask(ctx, a2aCtx, intent, evt, ref, localpart, res, "")
		terminalAudit.contextID = orDefault(terminalAudit.contextID, contextID)
		terminalAudit.dedupVerdict = audit.dedupVerdict
		terminalAudit.rateLimitVerdict = audit.rateLimitVerdict
		terminalAudit.activated = res.ActivatedExtensions // negotiated once on message/send, not per poll
		terminalAudit.mediaIn = audit.mediaIn             // inbound files were forwarded on the initial send
		audit = terminalAudit
		return
	}
	if res.Failed {
		delegationsTotal.WithLabelValues(localpart, outcomeFailed).Inc()
		// Agent-side failure detail stays in the logs — rooms get a generic notice (SPEC §6).
		b.log.Error("agent task failed", "ghost", localpart, "agent", ref.Path(), "room", evt.RoomID, "detail", res.Text)
		audit.outcome = outcomeFailed
		audit.terminalReason = "agent_failed"
		audit.replyEventID = b.postReply(ctx, intent, evt, fmt.Sprintf("⚠️ agent %q could not complete the task — see the bridge logs.", localpart))
		return
	}
	delegationsTotal.WithLabelValues(localpart, outcomeOK).Inc()
	audit.outcome = outcomeOK
	audit.terminalReason = "completed"
	replyID, out, rejected := b.deliverReply(ctx, intent, evt, "", localpart, ref, res)
	audit.replyEventID = replyID
	audit.mediaOut = out
	audit.mediaRejected += rejected
	b.log.Info("delegated to agent", "ghost", localpart, "agent", ref.Path(), "room", evt.RoomID)
}

func (b *Bridge) rejectBridgedSender(
	ctx context.Context,
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	started := time.Now()
	audit := delegationAuditResult{
		outcome:          outcomeDenied,
		terminalStage:    "admission",
		terminalReason:   "sender_policy_rejected",
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
	}
	defer func() {
		audit.duration = time.Since(started)
		b.logDelegationAudit(evt, ref, localpart, sender, audit)
	}()
	if !b.allowNotice(sender, evt.RoomID, localpart) {
		delegationsTotal.WithLabelValues(localpart, outcomeRateLimited).Inc()
		audit.outcome = outcomeRateLimited
		audit.terminalReason = "denial_notice_rate_limit_rejected"
		audit.rateLimitVerdict = rateLimitVerdictRejected
		return
	}
	audit.rateLimitVerdict = rateLimitVerdictAllowed

	ghost := id.NewUserID(localpart, b.cfg.ServerName)
	intent := b.as.Intent(ghost)
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure denied-notice ghost registered", "ghost", ghost, "err", err)
		audit.outcome = outcomeError
		audit.terminalStage = "matrix_register"
		audit.terminalReason = "matrix_registration_failed"
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("ensure denied-notice ghost joined", "ghost", ghost, "room", evt.RoomID, "err", err)
		audit.outcome = outcomeError
		audit.terminalStage = "matrix_join"
		audit.terminalReason = "matrix_join_failed"
		return
	}
	delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
	audit.replyEventID = b.postReply(ctx, intent, evt, policyDeniedText)
	b.log.Warn(
		"bridged sender not allowed to invoke agent",
		"sender", evt.Sender,
		"sender_origin_network", sender.origin.network,
		"ghost", localpart,
		"room", evt.RoomID,
	)
}

// refuseStagedTarget fails a stage:dev agent closed when it is invoked outside a configured staging
// room (#128). It is a blast-radius boundary distinct from the sender allowlist: the sender may be
// allowed, but the room is not a staging room. It posts one bounded policy notice and never spends a
// limiter token or reaches A2A. Promoting the agent to stage:prod (a one-line agents.yaml reload)
// removes the restriction with no pod restart.
func (b *Bridge) refuseStagedTarget(
	ctx context.Context,
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	dedupVerdict auditDedupVerdict,
) {
	started := time.Now()
	audit := delegationAuditResult{
		outcome:          outcomeDenied,
		terminalStage:    "admission",
		terminalReason:   "stage_policy_rejected",
		dedupVerdict:     dedupVerdict,
		rateLimitVerdict: rateLimitVerdictNotChecked,
	}
	defer func() {
		audit.duration = time.Since(started)
		b.logDelegationAudit(evt, ref, localpart, sender, audit)
	}()
	if !b.allowNotice(sender, evt.RoomID, localpart) {
		delegationsTotal.WithLabelValues(localpart, outcomeRateLimited).Inc()
		audit.outcome = outcomeRateLimited
		audit.terminalReason = "denial_notice_rate_limit_rejected"
		audit.rateLimitVerdict = rateLimitVerdictRejected
		return
	}
	audit.rateLimitVerdict = rateLimitVerdictAllowed

	ghost := id.NewUserID(localpart, b.cfg.ServerName)
	intent := b.as.Intent(ghost)
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure stage-denied ghost registered", "ghost", ghost, "err", err)
		audit.outcome = outcomeError
		audit.terminalStage = "matrix_register"
		audit.terminalReason = "matrix_registration_failed"
		return
	}
	if err := intent.EnsureJoined(ctx, evt.RoomID); err != nil {
		b.log.Error("ensure stage-denied ghost joined", "ghost", ghost, "room", evt.RoomID, "err", err)
		audit.outcome = outcomeError
		audit.terminalStage = "matrix_join"
		audit.terminalReason = "matrix_join_failed"
		return
	}
	delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
	audit.replyEventID = b.postReply(ctx, intent, evt, stageDeniedText)
	b.log.Warn(
		"staging agent invoked outside a staging room",
		"sender", evt.Sender,
		"ghost", localpart,
		"room", evt.RoomID,
	)
}

// allowNotice bounds bridge-generated Matrix responses independently from invocation admission.
// Exhaustion is silent: replying with another rate-limit notice would itself be amplification.
func (b *Bridge) allowNotice(sender senderIdentity, roomID id.RoomID, scope string) bool {
	return b.noticeSenderLimits.Allow(sender.rateLimitKey(scope)) &&
		b.noticeRoomLimits.Allow(roomID.String())
}

// awaitTask handles a long-running task (SPEC §6): post a working placeholder, poll tasks/get
// with backoff until the task is terminal or TaskTimeout elapses, then edit the placeholder
// into the final answer (Matrix edits are the open-standard substitute for streaming).
func (b *Bridge) awaitTask(
	ctx context.Context,
	a2aCtx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	res a2aclient.Result,
	placeholder id.EventID,
) delegationAuditResult {
	// Keep the root event unambiguously non-terminal. Provider status text is untrusted working
	// state and belongs in the progress thread; using it as the root lets observers mistake an
	// in-flight task for a completed reply before the terminal edit arrives. A resumed task (#116)
	// passes its existing placeholder so the Q&A stays in one thread.
	if placeholder == "" {
		placeholder = b.postReply(ctx, intent, evt, workingText)
	}
	audit := delegationAuditResult{
		outcome:          outcomeError,
		terminalStage:    "task_poll",
		terminalReason:   "task_poll_failed",
		dedupVerdict:     dedupVerdictAccepted,
		rateLimitVerdict: rateLimitVerdictAllowed,
		a2aAttempted:     true,
		a2aUserID:        evt.Sender.String(),
		contextID:        res.ContextID,
		taskID:           res.TaskID,
		replyEventID:     placeholder,
	}

	// The first result can already be paused (the agent needs input or auth before doing any work).
	// Handle it before the polling machinery so the question is not also surfaced as working progress.
	if res.AuthRequired {
		return b.finishAuthRequired(ctx, intent, evt, localpart, placeholder, audit)
	}
	if res.InputRequired && placeholder != "" {
		return b.pauseForInput(ctx, intent, evt, ref, localpart, placeholder, res, audit)
	}

	pollCtx, cancel := context.WithTimeout(a2aCtx, b.cfg.TaskTimeout)
	defer cancel()

	// Register the running task so a room member can cancel it by reacting to the placeholder (#98).
	// A missing placeholder (its post failed) leaves nothing to react to, so there is nothing to track.
	var task *inflightTask
	progress := taskProgress{max: b.cfg.MaxTaskProgressPosts}
	if placeholder != "" {
		task = &inflightTask{
			room:           evt.RoomID,
			placeholder:    placeholder,
			taskID:         res.TaskID,
			originalSender: evt.Sender,
			target:         ref.Target(),
			cancelPoll:     cancel,
		}
		b.inflight.register(task)
		defer b.inflight.unregister(placeholder)
		progress.root = placeholder // thread working-state updates under the placeholder (#118)
		b.surface(ctx, intent, evt.RoomID, &progress, res.Text)
		if b.cfg.PinInFlightTasks {
			b.pinPlaceholder(ctx, intent, evt.RoomID, placeholder)
			// Unpin on any terminal state, on a fresh bounded context so a canceled/shutdown ctx
			// still clears the pin (best-effort — a lingering pin is cosmetic, not a correctness bug).
			defer func() {
				unpinCtx, cancelUnpin := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.RequestTimeout)
				defer cancelUnpin()
				b.unpinPlaceholder(unpinCtx, intent, evt.RoomID, placeholder)
			}()
		}
	}

	delay, pollErrors := b.pollInitial, 0
	for {
		if err := b.pollWait(pollCtx, delay); err != nil {
			if who := taskCanceler(task); who != "" {
				return b.finishCanceled(ctx, a2aCtx, intent, evt, ref, localpart, placeholder, res.TaskID, who, audit)
			}
			trace.SpanFromContext(ctx).AddEvent(traceEventA2ATaskTimeout)
			delegationsTotal.WithLabelValues(localpart, outcomeTimeout).Inc()
			audit.outcome = outcomeTimeout
			audit.terminalReason = "task_timeout"
			b.editReply(ctx, intent, evt.RoomID, placeholder,
				fmt.Sprintf("⚠️ agent %q did not finish within %s.", localpart, agentRequestTimeout(ref, b.cfg.TaskTimeout)))
			return audit
		}
		if delay *= 2; delay > b.pollMax {
			delay = b.pollMax
		}

		trace.SpanFromContext(ctx).AddEvent("a2a.task.poll")
		polled, err := b.client.PollTask(pollCtx, ref.Target(), res.TaskID)
		if err != nil {
			if who := taskCanceler(task); who != "" {
				return b.finishCanceled(ctx, a2aCtx, intent, evt, ref, localpart, placeholder, res.TaskID, who, audit)
			}
			trace.SpanFromContext(ctx).AddEvent(traceEventA2ATaskPollError)
			if errors.Is(err, a2aclient.ErrRemoteTargetUntrusted) {
				delegationsTotal.WithLabelValues(localpart, outcomeDenied).Inc()
				audit.outcome = outcomeDenied
				audit.terminalStage = "agent_card"
				audit.terminalReason = "agent_card_untrusted"
				b.log.Warn("stopping task polling after remote agent trust changed", "task", res.TaskID, "agent", ref.Path())
				b.editReply(ctx, intent, evt.RoomID, placeholder,
					fmt.Sprintf("⚠️ lost trust in agent %q while waiting for its task — see the bridge logs.", localpart))
				return audit
			}
			if pollErrors++; pollErrors < pollErrorBudget {
				b.log.Warn("tasks/get failed, retrying", "task", res.TaskID, "err", err)
				continue
			}
			delegationsTotal.WithLabelValues(localpart, outcomeLost).Inc()
			audit.outcome = outcomeLost
			audit.terminalReason = "task_poll_failed"
			b.log.Error("tasks/get failed", "task", res.TaskID, "agent", ref.Path(), "err", err)
			b.editReply(ctx, intent, evt.RoomID, placeholder,
				fmt.Sprintf("⚠️ lost track of agent %q's task — see the bridge logs.", localpart))
			return audit
		}
		pollErrors = 0
		if polled.AuthRequired {
			return b.finishAuthRequired(ctx, intent, evt, localpart, placeholder, audit)
		}
		if polled.InputRequired && placeholder != "" {
			return b.pauseForInput(ctx, intent, evt, ref, localpart, placeholder, polled, audit)
		}
		if polled.Terminal {
			audit.terminalStage = "task_result"
			audit.contextID = orDefault(polled.ContextID, res.ContextID)
			audit.taskID = orDefault(polled.TaskID, res.TaskID)
			if polled.Failed {
				delegationsTotal.WithLabelValues(localpart, outcomeFailed).Inc()
				audit.outcome = outcomeFailed
				audit.terminalReason = "agent_failed"
				b.log.Error("agent task failed", "ghost", localpart, "agent", ref.Path(), "room", evt.RoomID, "detail", polled.Text)
				b.editReply(ctx, intent, evt.RoomID, placeholder,
					fmt.Sprintf("⚠️ agent %q could not complete the task — see the bridge logs.", localpart))
				return audit
			}
			delegationsTotal.WithLabelValues(localpart, outcomeOK).Inc()
			audit.outcome = outcomeOK
			audit.terminalReason = "completed"
			_, out, rejected := b.deliverReply(ctx, intent, evt, placeholder, localpart, ref, polled)
			audit.mediaOut = out
			audit.mediaRejected += rejected
			b.log.Info("delegated to agent (long task)", "ghost", localpart, "agent", ref.Path(), "room", evt.RoomID)
			return audit
		}
		// Still working: surface a bounded, deduplicated progress update in the placeholder thread (#118).
		if task != nil {
			b.surface(ctx, intent, evt.RoomID, &progress, polled.Text)
		}
	}
}

// pauseForInput surfaces the agent's question in the placeholder thread and registers the task as
// open, releasing the dispatcher worker while the original sender composes a reply (#116) — so a
// paused task never holds a room's FIFO. The task resumes when that sender replies in the thread
// (continueOpenTask) or is dropped when InputWaitTimeout elapses (expireOpenTask). The audit is a
// non-terminal `input_required` outcome linked to the eventual resume by taskID + contextID.
func (b *Bridge) pauseForInput(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	placeholder id.EventID,
	res a2aclient.Result,
	audit delegationAuditResult,
) delegationAuditResult {
	question := orDefault(strings.TrimSpace(res.Text), "The agent needs more information to continue.")
	b.editReply(ctx, intent, evt.RoomID, placeholder,
		fmt.Sprintf("❓ %s\n\n(reply in this thread within %s to continue)", question, b.cfg.InputWaitTimeout))
	open := &openTask{
		origin:      evt,
		placeholder: placeholder,
		localpart:   localpart,
		ref:         ref,
		taskID:      orDefault(res.TaskID, audit.taskID),
		contextID:   orDefault(res.ContextID, audit.contextID),
		sender:      b.agents.IdentifySender(evt.Sender),
	}
	b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })

	delegationsTotal.WithLabelValues(localpart, outcomeInputRequired).Inc()
	audit.outcome = outcomeInputRequired
	audit.terminalStage = "task_input"
	audit.terminalReason = "awaiting_input"
	audit.taskID = open.taskID
	audit.contextID = open.contextID
	audit.replyEventID = placeholder
	b.log.Info("delegation paused awaiting a threaded reply",
		"ghost", localpart, "room", evt.RoomID, "task", open.taskID)
	return audit
}

// finishAuthRequired terminates a task that entered TASK_STATE_AUTH_REQUIRED. The bridge does not
// forward caller credentials to agents (docs/security.md), so it stops honestly instead of relaying
// anything or pausing for a human to supply a secret in a room.
func (b *Bridge) finishAuthRequired(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	localpart string,
	placeholder id.EventID,
	audit delegationAuditResult,
) delegationAuditResult {
	delegationsTotal.WithLabelValues(localpart, outcomeFailed).Inc()
	audit.outcome = outcomeFailed
	audit.terminalStage = "task_auth"
	audit.terminalReason = "auth_required_not_forwarded"
	b.editReply(ctx, intent, evt.RoomID, placeholder,
		fmt.Sprintf("⚠️ agent %q needs authorization the platform does not forward — the task was stopped.", localpart))
	b.log.Warn("stopping task: agent requires unforwarded authorization",
		"ghost", localpart, "room", evt.RoomID)
	return audit
}

// expireOpenTask drops a paused task whose InputWaitTimeout elapsed with no reply, editing the
// placeholder into an honest stale notice and emitting a terminal timeout audit. The claim makes
// expiry and an answering reply mutually exclusive.
func (b *Bridge) expireOpenTask(open *openTask) {
	if _, ok := b.openTasks.claim(open.placeholder); !ok {
		return // already resumed or dropped
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(b.runCtx), b.cfg.RequestTimeout)
	defer cancel()
	intent := b.as.Intent(id.NewUserID(open.localpart, b.cfg.ServerName))
	b.editReply(ctx, intent, open.origin.RoomID, open.placeholder,
		fmt.Sprintf("⌛ agent %q got no reply within %s — the task was dropped.", open.localpart, b.cfg.InputWaitTimeout))
	delegationsTotal.WithLabelValues(open.localpart, outcomeTimeout).Inc()
	b.logDelegationAudit(open.origin, open.ref, open.localpart, open.sender, delegationAuditResult{
		outcome:          outcomeTimeout,
		terminalStage:    "task_input",
		terminalReason:   "input_wait_timeout",
		a2aAttempted:     true,
		a2aUserID:        open.origin.Sender.String(),
		taskID:           open.taskID,
		contextID:        open.contextID,
		replyEventID:     open.placeholder,
		dedupVerdict:     dedupVerdictAccepted,
		rateLimitVerdict: rateLimitVerdictAllowed,
	})
	b.log.Info("dropped paused delegation after input wait timeout",
		"ghost", open.localpart, "room", open.origin.RoomID, "task", open.taskID)
}

// taskCanceler reports the room member who canceled a tracked long task, or empty when the task is
// untracked (no placeholder) or still running. It disambiguates a room-initiated cancel from an
// ordinary poll timeout or shutdown, which share the same context-cancellation signal.
func taskCanceler(task *inflightTask) id.UserID {
	if task == nil {
		return ""
	}
	return task.canceler()
}

// finishCanceled completes a delegation that a room member canceled (#98): ask the agent to stop
// (best-effort tasks/cancel, so token burn halts at the source), edit the placeholder into a
// content-free "canceled by" notice, and audit who canceled. The poll context is already dead, so
// the agent-side cancel runs on a fresh deadline off the still-live delegation context, which keeps
// the A2A user attribution and any per-remote ceiling.
func (b *Bridge) finishCanceled(
	ctx, a2aCtx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	placeholder id.EventID,
	taskID string,
	canceledBy id.UserID,
	audit delegationAuditResult,
) delegationAuditResult {
	trace.SpanFromContext(ctx).AddEvent("a2a.task.cancel")
	cancelCtx, cancel := context.WithTimeout(a2aCtx, b.cfg.RequestTimeout)
	if err := b.client.CancelTask(cancelCtx, ref.Target(), taskID); err != nil {
		// Best-effort: the room cancel is honored regardless, but a failed agent-side stop means
		// the agent may keep working, so it is worth a warning.
		b.log.Warn("agent-side task cancel failed", "task", taskID, "agent", ref.Path(), "err", err)
	}
	cancel()

	delegationsTotal.WithLabelValues(localpart, outcomeCanceled).Inc()
	audit.outcome = outcomeCanceled
	audit.terminalStage = "task_cancel"
	audit.terminalReason = "canceled_by_room"
	audit.canceledBy = canceledBy.String()
	b.editReply(ctx, intent, evt.RoomID, placeholder, fmt.Sprintf("🛑 canceled by %s.", canceledBy))
	b.log.Info("canceled long task from room",
		"ghost", localpart, "agent", ref.Path(), "room", evt.RoomID, "canceled_by", canceledBy)
	return audit
}

// logDelegationAudit emits exactly one terminal record per resolved target. Message and prompt
// content are deliberately absent: Matrix remains the source of record for content, while this
// record provides the structured joins needed to follow identity and usage across systems.
func (b *Bridge) logDelegationAudit(
	evt *event.Event,
	ref *AgentRef,
	localpart string,
	sender senderIdentity,
	result delegationAuditResult,
) {
	b.auditLog.Info(
		"delegation audit",
		"audit_schema", delegationAuditSchema,
		"sender_mxid", evt.Sender.String(),
		"sender_homeserver", evt.Sender.Homeserver(),
		"sender_origin_kind", string(sender.origin.kind),
		"sender_origin_network", sender.origin.network,
		"matrix_event_id", evt.ID.String(),
		"matrix_origin_server_ts", evt.Timestamp,
		"room_id", evt.RoomID.String(),
		"reply_event_id", result.replyEventID.String(),
		"ghost", localpart,
		"ghost_mxid", id.NewUserID(localpart, b.cfg.ServerName).String(),
		"agent_path", ref.Path(),
		"a2a_attempted", result.a2aAttempted,
		"a2a_user_id", result.a2aUserID,
		"a2a_context_id", result.contextID,
		"a2a_task_id", result.taskID,
		"outcome", result.outcome,
		"terminal_stage", result.terminalStage,
		"terminal_reason", result.terminalReason,
		"canceled_by", result.canceledBy,
		"media_in", result.mediaIn,
		"media_out", result.mediaOut,
		"media_rejected", result.mediaRejected,
		"a2a_activated_extensions", strings.Join(result.activated, ","),
		"duration_ms", result.duration.Milliseconds(),
		"dedup_verdict", string(result.dedupVerdict),
		"rate_limit_verdict", string(result.rateLimitVerdict),
	)
}

// postReply sends text into the room as the ghost, as an m.notice reply to the original message
// (notice, so other bots/agents ignore it by convention — SPEC §4 F8). Returns the event ID.
func (b *Bridge) postReply(ctx context.Context, intent *appservice.IntentAPI, evt *event.Event, text string) id.EventID {
	trace.SpanFromContext(ctx).AddEvent("matrix.reply.post")
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	content.SetReply(evt) // m.relates_to reply pointing at the human's original message
	resp, err := intent.SendMessageEvent(ctx, evt.RoomID, event.EventMessage, automatedContent(content))
	if err != nil {
		b.log.Error("post reply", "room", evt.RoomID, "err", err)
		return ""
	}
	return resp.EventID
}

// editReply replaces a previously-posted reply (m.replace); falls back to logging when the
// placeholder was never posted.
func (b *Bridge) editReply(ctx context.Context, intent *appservice.IntentAPI, roomID id.RoomID, target id.EventID, text string) {
	trace.SpanFromContext(ctx).AddEvent("matrix.reply.edit")
	if target == "" {
		b.log.Error("no placeholder to edit", "room", roomID, "text", text)
		return
	}
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: text}
	content.SetEdit(target)
	if _, err := intent.SendMessageEvent(ctx, roomID, event.EventMessage, automatedContent(content)); err != nil {
		b.log.Error("edit reply", "room", roomID, "err", err)
	}
}

// automatedContent tags bridge-authored message content with the MSC3955 m.automated mixin. The
// mixin is additive raw content merged alongside the parsed m.notice (mautrix merges Raw under
// Parsed), so mixin-unaware clients render the reply exactly as before. For m.replace edits the
// marker is mirrored into m.new_content as well, so edit-aware clients keep it once they apply the
// replacement — the top-level fallback still carries it for edit-unaware bots.
func automatedContent(content *event.MessageEventContent) *event.Content {
	raw := map[string]any{automatedMixinKey: true}
	if content.NewContent != nil {
		raw[newContentKey] = map[string]any{automatedMixinKey: true}
	}
	return &event.Content{Parsed: content, Raw: raw}
}

// resolveTargets returns the mapped agent ghost local-parts a message addresses, from the typed
// m.mentions field first (MSC3952), then a plaintext-body fallback. Only local ghosts survive
// (a federated @agent-x:other.org must never resolve to the local agent — SPEC §4 F6), and only
// when the sender passes the agent's policy.
func (b *Bridge) resolveTargets(evt *event.Event, msg *event.MessageEventContent) targetResolution {
	seen := make(map[string]struct{})
	result := targetResolution{
		sender: b.agents.IdentifySender(evt.Sender),
		refs:   make(map[string]*AgentRef),
	}
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
		result.refs[localpart] = ref
		if !ref.AllowsSender(result.sender, b.cfg.ServerName) {
			b.log.Warn(
				"sender not allowed to invoke agent",
				"sender", evt.Sender,
				"sender_origin_network", result.sender.origin.network,
				"ghost", localpart,
				"room", evt.RoomID,
			)
			if result.sender.isBridged() {
				if _, dup := seen[localpart]; !dup {
					seen[localpart] = struct{}{}
					result.deniedBridged = append(result.deniedBridged, localpart)
				}
			}
			return
		}
		if _, dup := seen[localpart]; dup {
			return
		}
		seen[localpart] = struct{}{}
		result.allowed = append(result.allowed, localpart)
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
	return result
}

// stripMentions removes "@agent-*[:server]" tokens so the agent receives a clean task prompt.
func (b *Bridge) stripMentions(body string) string {
	stripped := strings.TrimSpace(b.mentionRe.ReplaceAllString(body, ""))
	if stripped == "" {
		return body // the message was only a mention — send it verbatim rather than nothing
	}
	return stripped
}

// provenancePrompt gives agents bridge-derived attribution separately from the untrusted room
// text. Quoting the identifiers keeps the envelope single-line even if a future Matrix parser
// accepts unusual input; the content remains untrusted and may itself imitate these delimiters.
func provenancePrompt(evt *event.Event, content string) string {
	return fmt.Sprintf(
		"%s\nsender_mxid: %q\nsender_homeserver: %q\nroom_id: %q\n%s\n%s\n%s\n%s",
		provenanceStart,
		evt.Sender.String(),
		evt.Sender.Homeserver(),
		evt.RoomID.String(),
		provenanceEnd,
		contentStart,
		content,
		contentEnd,
	)
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
