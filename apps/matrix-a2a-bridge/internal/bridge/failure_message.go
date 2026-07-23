package bridge

import (
	"context"
	"fmt"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	errorEmptyReply     = "empty_reply"
	errorRequestTimeout = "request_timeout"
)

// failureMessage maps stable terminal reasons to bounded, actionable room copy. It never accepts
// provider error text, so an upstream response body or internal endpoint cannot cross into Matrix.
func failureMessage(reason, agent string, timeout time.Duration) string {
	switch reason {
	case state.QueueRoomCapacityRejected:
		return "⚠️ This room already has as many agent requests as it can safely queue. Please try again in a moment."
	case state.QueueGlobalCapacityRejected:
		return "⚠️ The agent service is handling its maximum safe queue. Please try again in a moment."
	case errorRateLimit:
		return "⚠️ This agent has reached a request limit. Please wait a moment before trying again."
	case errorRoomTokenBudget:
		return "⚠️ This room has reached its token budget for the current period, so the request was not sent. It will reset automatically; ask an operator to review the budget if you need more."
	case errorQuoteOverBudget:
		return "⚠️ This agent is over its configured budget limit, so the request was not sent. Ask an operator to review the limit."
	case errorAgentUntrusted:
		return fmt.Sprintf("⚠️ Agent %q could not be verified, so the request was not sent. Ask an operator to review its trust configuration.", agent)
	case errorSenderPolicy:
		return fmt.Sprintf("⚠️ You are not allowed to invoke agent %q. Ask an operator to review its sender policy.", agent)
	case errorStagePolicy:
		return fmt.Sprintf("⚠️ Agent %q is a staging build and can only run in its designated staging room.", agent)
	case errorMediaDenied:
		return "⚠️ The attached file was refused by the media policy. Remove it or use an allowed file type, then try again."
	case errorRequestTimeout:
		return fmt.Sprintf("⚠️ Agent %q did not answer before the request deadline. Try again once; if it continues, ask an operator to check availability.", agent)
	case errorTaskTimeout:
		if timeout > 0 {
			return fmt.Sprintf("⚠️ Agent %q did not finish within %s. Start a new request if the work is still needed.", agent, timeout)
		}
		return fmt.Sprintf("⚠️ Agent %q did not finish before the task deadline. Start a new request if the work is still needed.", agent)
	case errorAuthRequired:
		return fmt.Sprintf("⚠️ Agent %q needs authorization that the platform does not forward. The task was stopped; ask an operator for an approved access path.", agent)
	case errorEmptyReply:
		return fmt.Sprintf("⚠️ Agent %q completed without a reply. Try rephrasing the request; if it repeats, ask an operator to check the agent.", agent)
	case errorA2AAckAmbiguous:
		return fmt.Sprintf("⚠️ Agent %q may have received this request, but its acknowledgement was lost. The bridge did not resend it; check the agent before retrying.", agent)
	case errorInputRequired:
		return fmt.Sprintf("⚠️ Agent %q needs more input, but this request cannot continue safely. Start a new request with the missing details.", agent)
	case errorTaskInvalid:
		return fmt.Sprintf("⚠️ Agent %q returned invalid task information. Start a new request; if it repeats, ask an operator to check the agent.", agent)
	case errorAgentFailed, errorTaskFailed:
		return fmt.Sprintf("⚠️ Agent %q could not complete the task. Try once more or ask an operator to check the agent.", agent)
	case errorTaskPoll:
		return fmt.Sprintf("⚠️ The bridge lost track of agent %q's task. Start a new request if the work is still needed.", agent)
	case errorA2APreflightRetry:
		return fmt.Sprintf("⚠️ Agent %q's request could not be recovered after repeated failures. Start a new request if the work is still needed.", agent)
	default:
		return fmt.Sprintf("⚠️ could not reach agent %q — see the bridge logs.", agent)
	}
}

// postFailureReply applies the independent notice budget before projecting a failure. Exhaustion is
// intentionally silent: a rate-limit response must not create another response-amplification path.
func (b *Bridge) postFailureReply(
	ctx context.Context,
	intent *appservice.IntentAPI,
	evt *event.Event,
	sender senderIdentity,
	agent, reason string,
	timeout time.Duration,
) id.EventID {
	if !b.allowNotice(sender, evt.RoomID, agent) {
		return ""
	}
	return b.postReply(ctx, intent, evt, failureMessage(reason, agent, timeout))
}

// postFailureForTarget is the pre-dispatch variant used before a ghost intent has been prepared.
// It spends notice capacity before registration and never joins a ghost as a side effect.
func (b *Bridge) postFailureForTarget(
	ctx context.Context,
	evt *event.Event,
	sender senderIdentity,
	agent, reason string,
) {
	if !b.allowNotice(sender, evt.RoomID, agent) {
		return
	}
	if b.as == nil || b.as.StateStore == nil {
		return
	}
	intent := b.as.Intent(id.NewUserID(agent, b.cfg.ServerName))
	if intent == nil || intent.Client == nil {
		return
	}
	if err := intent.EnsureRegistered(ctx); err != nil {
		b.log.Error("ensure failure-notice ghost registered", "ghost", agent, "err", err)
		return
	}
	b.postReply(ctx, intent, evt, failureMessage(reason, agent, 0))
}

// editFailureReply applies the same notice budget to a terminal edit of a working placeholder.
// The caller keeps the original placeholder ID in its audit even when the edit is suppressed.
func (b *Bridge) editFailureReply(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	placeholder id.EventID,
	sender senderIdentity,
	agent, reason string,
	timeout time.Duration,
) {
	if !b.allowNotice(sender, roomID, agent) {
		return
	}
	b.editReply(ctx, intent, roomID, placeholder, failureMessage(reason, agent, timeout))
}
