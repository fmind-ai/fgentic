package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	// modelOutageNotice is the exact §6.1 failure-catalog copy for errorA2APreflightRetry addressed to
	// the local integration ghost (internal/bridge/failure_message.go). A mismatch means the bridge
	// took a different terminal path (a warm-cache ambiguous send, or a request-timeout hang) — the
	// drill must surface that rather than pass.
	//
	// Runtime reality (#466): a delegation whose backend is unreachable resolves the AgentCard on every
	// attempt, and the failure classification depends on HOW the backend is down. Scaling the backend
	// to zero yields an endpointless Service that HANGS to REQUEST_TIMEOUT (context.DeadlineExceeded ->
	// errorRequestTimeout, a single terminal attempt), not a retry loop. This drill instead injects a
	// fast connection failure at the fault proxy: the cold-cache card-resolution GET fails before any
	// send is attempted, so it is neither DeadlineExceeded nor ambiguous, and the bridge retries with
	// capped backoff up to DELEGATION_MAX_ATTEMPTS before this dead-letter notice.
	modelOutageNotice = "⚠️ Agent \"agent-integration\"'s request could not be recovered after " +
		"repeated failures. Start a new request if the work is still needed."
	// Outcome labels mirror internal/bridge/metrics.go; the driver cannot import that package.
	outcomeDeadLabel = "dead"
	outcomeOKLabel   = "ok"
)

// runModelOutage proves the dependency-outage contract for a failed model backend (#466): while the
// A2A/model backend is failing, the user receives the bounded §6.1 catalog notice (never a silent
// drop or a forever placeholder), no model spend is amplified, and a later mention succeeds once the
// backend returns. The driver arms/disarms the fault directly through the fault proxy's control
// endpoint, so the outage is fast and deterministic without any pod scaling handshake.
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
	// Startup AgentCard verification: the ghost's Matrix display name is derived from the local card
	// (resolved through the fault proxy while it still forwards), so a synced name proves the backend
	// was reachable and trusted before the induced outage.
	if err := f.waitForDisplayName(ctx, sess.AccessToken, ghost, ghostDisplayName); err != nil {
		return err
	}

	slog.Info("model backend outage armed", "model_outage_phase", "await_outage")
	if err := f.armFaultRefuse(ctx); err != nil {
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
	if err := f.disarmFault(ctx); err != nil {
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
	// Exactly one A2A execution reached the backend across the whole drill (the recovery mention). The
	// bounded retries during the outage were refused before the upstream, so no model spend was
	// amplified while the delegation failed.
	stats, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if stats.TotalRequests != 1 || stats.TotalStarted != 0 {
		return fmt.Errorf(
			"model backend executions total=%d started=%d, want exactly one recovery request and no held starts",
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

func (f fixture) armFaultRefuse(ctx context.Context) error {
	status, body, err := f.request(ctx, http.MethodPost, f.faultURL+"/arm/a2a-refuse", "", nil)
	if err != nil {
		return fmt.Errorf("arm a2a-refuse fault: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("arm a2a-refuse fault: status %d: %s", status, body)
	}
	return nil
}

func (f fixture) disarmFault(ctx context.Context) error {
	status, body, err := f.request(ctx, http.MethodPost, f.faultURL+"/disarm", "", nil)
	if err != nil {
		return fmt.Errorf("disarm fault: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("disarm fault: status %d: %s", status, body)
	}
	return nil
}
