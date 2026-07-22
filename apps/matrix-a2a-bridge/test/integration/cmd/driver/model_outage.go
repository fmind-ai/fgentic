package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	modelOutagePollInterval = 200 * time.Millisecond
	modelOutageReachTimeout = 90 * time.Second
	// modelOutageNotice is the exact §6.1 failure-catalog copy for errorA2APreflightRetry addressed to
	// the local integration ghost (internal/bridge/failure_message.go). A mismatch means the bridge
	// took a different terminal path (e.g. a warm-cache ambiguous send or a request-timeout), which
	// this drill must surface rather than pass.
	modelOutageNotice = "⚠️ Agent \"agent-integration\"'s request could not be recovered after " +
		"repeated failures. Start a new request if the work is still needed."
	// Outcome labels mirror internal/bridge/metrics.go; the driver cannot import that package.
	outcomeDeadLabel = "dead"
	outcomeOKLabel   = "ok"
)

// runModelOutage proves the dependency-outage contract for a failed model backend (#466): with the
// A2A/model backend scaled to zero mid-delegation, the user receives the bounded §6.1 catalog notice
// (never a silent drop or a forever placeholder), no model spend is amplified, and a later mention
// succeeds once the backend returns. run.sh performs the scale-down/up when it observes the phase
// logs below; the driver detects the backend's real reachability so the failing send resolves its
// cold local AgentCard against a down backend — the retryable preflight path, not a warm-cache send.
func (f fixture) runModelOutage(ctx context.Context) error {
	startedAt := time.Now()
	sess, err := f.register(ctx)
	if err != nil {
		return err
	}
	ghost := "@" + ghostLocalpart + ":" + f.server
	roomID, err := f.createRoom(ctx, sess.AccessToken)
	if err != nil {
		return err
	}
	if err := f.invite(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	if err := f.waitForJoin(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	// Startup AgentCard verification: the ghost's Matrix display name is derived from the local card,
	// so a synced name proves the backend was reachable and trusted before the induced outage.
	if err := f.waitForDisplayName(ctx, sess.AccessToken, ghost, ghostDisplayName); err != nil {
		return err
	}

	slog.Info("model backend outage armed", "model_outage_phase", "await_outage")
	if err := f.waitForStubUnreachable(ctx); err != nil {
		return err
	}

	failEventID, err := f.sendMessageTxn(ctx, sess.AccessToken, roomID, "model-outage-mention",
		messageContent{
			Body:     ghost + " summarize the outage runbook",
			Mentions: mentions{UserIDs: []string{ghost}},
			MsgType:  "m.text",
		})
	if err != nil {
		return err
	}
	// The user must see the bounded §6.1 catalog notice — never a silent drop or a forever placeholder.
	if err := f.waitForReply(ctx, sess.AccessToken, roomID, ghost, failEventID, modelOutageNotice); err != nil {
		return fmt.Errorf("await model-outage catalog notice: %w", err)
	}
	// Bounded terminal: exactly one dead-letter outcome. run.sh independently counts the content-free
	// preflight-retry log lines to prove the attempt count equalled DELEGATION_MAX_ATTEMPTS.
	if err := f.requireDelegationMetric(ctx, ghostLocalpart, outcomeDeadLabel, 1); err != nil {
		return err
	}

	slog.Info("model backend recovery armed", "model_outage_phase", "await_recovery")
	if err := f.waitForStubReachable(ctx); err != nil {
		return err
	}
	okEventID, err := f.sendMessageTxn(ctx, sess.AccessToken, roomID, "model-outage-recovery",
		messageContent{
			Body:     ghost + " confirm the backend recovered",
			Mentions: mentions{UserIDs: []string{ghost}},
			MsgType:  "m.text",
		})
	if err != nil {
		return err
	}
	if err := f.waitForReply(ctx, sess.AccessToken, roomID, ghost, okEventID, replyText); err != nil {
		return fmt.Errorf("await post-recovery reply: %w", err)
	}
	if err := f.requireDelegationMetric(ctx, ghostLocalpart, outcomeOKLabel, 1); err != nil {
		return err
	}
	// The restored backend is a fresh pod with fresh counters: exactly one A2A execution reached it
	// (the recovery mention). The bounded retries during the outage never connected, so no model spend
	// was amplified while the delegation failed.
	stats, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if stats.TotalRequests != 1 || stats.TotalStarted != 0 {
		return fmt.Errorf(
			"model backend executions total=%d started=%d after recovery, want exactly one recovery request and no held starts",
			stats.TotalRequests, stats.TotalStarted,
		)
	}

	slog.Info(
		"bridge model-outage scenario passed",
		"model_outage_phase", "passed",
		"catalog_notice", true,
		"dead_outcome", 1,
		"recovered_outcome", 1,
		"backend_requests_total", stats.TotalRequests,
		"scenario_duration_ms", time.Since(startedAt).Milliseconds(),
	)
	return nil
}

func (f fixture) stubHealthy(ctx context.Context) bool {
	status, _, err := f.request(ctx, http.MethodGet, f.stubURL+"/healthz", "", nil)
	return err == nil && status == http.StatusNoContent
}

func (f fixture) waitForStubUnreachable(ctx context.Context) error {
	deadline := time.Now().Add(modelOutageReachTimeout)
	for {
		if !f.stubHealthy(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("model backend still reachable after the scale-down window")
		}
		if err := wait(ctx, modelOutagePollInterval); err != nil {
			return fmt.Errorf("wait for model backend outage: %w", err)
		}
	}
}

func (f fixture) waitForStubReachable(ctx context.Context) error {
	deadline := time.Now().Add(modelOutageReachTimeout)
	for {
		if f.stubHealthy(ctx) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("model backend did not recover within the window")
		}
		if err := wait(ctx, modelOutagePollInterval); err != nil {
			return fmt.Errorf("wait for model backend recovery: %w", err)
		}
	}
}
