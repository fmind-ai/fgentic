package bridge

import (
	"strings"
	"testing"
	"time"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

func TestFailureMessageCatalogIsDistinctActionableAndContentFree(t *testing.T) {
	reasons := []string{
		state.QueueRoomCapacityRejected,
		state.QueueGlobalCapacityRejected,
		errorRateLimit,
		errorQuoteOverBudget,
		errorAgentUntrusted,
		errorSenderPolicy,
		errorStagePolicy,
		errorMediaDenied,
		errorRequestTimeout,
		errorTaskTimeout,
		errorAuthRequired,
		errorEmptyReply,
	}
	seen := make(map[string]string, len(reasons))
	for _, reason := range reasons {
		message := failureMessage(reason, "agent-docs-qa", 2*time.Minute)
		if len(message) == 0 || len(message) > 320 {
			t.Errorf("message for %q has unsafe length %d: %q", reason, len(message), message)
		}
		for _, leaked := range []string{
			"private sentinel", "http://", "https://", "/api/", "bridge logs", "internal endpoint",
		} {
			if strings.Contains(message, leaked) {
				t.Errorf("message for %q leaked %q: %q", reason, leaked, message)
			}
		}
		if previous, duplicate := seen[message]; duplicate {
			t.Errorf("reasons %q and %q share indistinguishable copy %q", previous, reason, message)
		}
		seen[message] = reason
	}
}

func TestFailureMessageDoesNotInterpolateUntrustedErrors(t *testing.T) {
	const secret = "private sentinel from https://internal.example/api/agents"
	message := failureMessage("unknown: "+secret, "agent-docs-qa", 0)
	if strings.Contains(message, secret) || strings.Contains(message, "internal.example") {
		t.Fatalf("unknown failure copy contains terminal input: %q", message)
	}
	const want = `⚠️ could not reach agent "agent-docs-qa" — see the bridge logs.`
	if message != want {
		t.Fatalf("unknown failure copy = %q, want existing generic fallback %q", message, want)
	}
}

func TestFailureNoticesUseSharedBoundedAutomatedPlane(t *testing.T) {
	b, intent, evt, _, recorder := pollingHarness(t, nil)
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeRoomLimits = newLimiters(60, 10, testRateLimitBucketCapacity)
	sender := b.agents.IdentifySender(evt.Sender)

	if replyID := b.postFailureReply(
		t.Context(), intent, evt, sender, "agent-k8s", errorRateLimit, 0, outcomeRateLimited, "",
	); replyID == "" {
		t.Fatal("first failure notice was unexpectedly suppressed")
	}
	if replyID := b.postFailureReply(
		t.Context(), intent, evt, sender, "agent-k8s", errorRateLimit, 0, outcomeRateLimited, "",
	); replyID != "" {
		t.Fatalf("second failure notice bypassed exhausted sender bucket: %q", replyID)
	}

	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorRateLimit, "agent-k8s", 0) {
		t.Fatalf("bounded failure notices = %#v", events)
	}
	raw := recorder.rawSnapshot(t)
	if len(raw) != 1 || raw[0][automatedMixinKey] != true {
		t.Fatalf("failure notice missing %s: %#v", automatedMixinKey, raw)
	}
}
