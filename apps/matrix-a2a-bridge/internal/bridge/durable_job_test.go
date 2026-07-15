package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

type lostAdmissionAcknowledgementStore struct {
	state.Store
}

func (s *lostAdmissionAcknowledgementStore) RecordAdmission(
	ctx context.Context,
	request state.AdmissionRequest,
) error {
	if err := s.Store.RecordAdmission(ctx, request); err != nil {
		return err
	}
	return errors.New("injected lost admission acknowledgement")
}

func TestDurableJobDeliversTerminalResultAndClearsContent(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text: "durable answer", ContextID: "context-after", Terminal: true,
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-terminal")

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered {
		t.Fatalf("state = %q, want delivered", stored.State)
	}
	if stored.Prompt != "" || len(stored.Payload) != 0 || stored.ResultText != "" {
		t.Fatalf("terminal content was retained: prompt=%q payload=%d result=%q",
			stored.Prompt, len(stored.Payload), stored.ResultText)
	}
	if stored.MatrixReplyEventID == "" || stored.TerminalAt.IsZero() {
		t.Fatalf("terminal Matrix evidence missing: %+v", stored)
	}
	if client.callCount != 1 || len(client.callMessageIDs) != 1 || client.callMessageIDs[0] != job.A2AMessageID {
		t.Fatalf("A2A calls/IDs = %d/%v, want one %q", client.callCount, client.callMessageIDs, job.A2AMessageID)
	}
	if events := recorder.snapshot(); len(events) != 1 || events[0].Body != "durable answer" {
		t.Fatalf("Matrix events = %+v, want one durable answer", events)
	}
	contextID, err := b.store.Context(t.Context(), job.RoomID, job.GhostLocalpart)
	if err != nil || contextID != "context-after" {
		t.Fatalf("conversation context = (%q, %v), want context-after", contextID, err)
	}
}

func TestDurableTerminalAuditPreservesContentFreeProtocolEvidence(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text:                "report ready",
		Terminal:            true,
		ActivatedExtensions: []string{"https://example.test/a2a/extension/v1"},
		Files: []a2aclient.ResultFile{
			{Name: "report.csv", MIMEType: "text/csv", Bytes: []byte("a,b\n1,2")},
		},
	}}
	b, _, _, fixture := mediaHarness(t, client)
	configureDurableTestBridge(b)
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	job := admitAndClaimDurableJob(t, b, "$durable-audit")

	b.executeDurableJob(t.Context(), job)
	if uploads := fixture.uploadSnapshot(); len(uploads) != 1 {
		t.Fatalf("durable artifact uploads = %d, want one", len(uploads))
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("durable audits = %#v, want one", audits)
	}
	audit := audits[0]
	if audit["outcome"] != outcomeOK || audit["rate_limit_verdict"] != string(rateLimitVerdictAllowed) ||
		audit["media_in"] != float64(0) || audit["media_out"] != float64(1) ||
		audit["media_rejected"] != float64(0) ||
		audit["a2a_activated_extensions"] != "https://example.test/a2a/extension/v1" {
		t.Fatalf("durable audit evidence = %#v", audit)
	}
	if strings.Contains(output.String(), "report ready") || strings.Contains(output.String(), "a,b") {
		t.Fatalf("durable audit leaked result content: %s", output.String())
	}
}

func TestDurableRateLimitAuditRecordsPersistedRejection(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.senderLimits = newLimiters(1, 1, 1)
	sender := matrixSender(id.NewUserID("alice", ownServer))
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to exhaust sender limiter fixture")
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	job := admitAndClaimDurableJob(t, b, "$durable-rate-limit")
	before := counterValue(t, delegationsTotal.WithLabelValues(job.GhostLocalpart, outcomeRateLimited))

	b.executeDurableJob(t.Context(), job)
	if client.callCount != 0 {
		t.Fatalf("rate-limited durable job reached A2A %d times", client.callCount)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["outcome"] != outcomeRateLimited ||
		audits[0]["terminal_stage"] != "admission" ||
		audits[0]["terminal_reason"] != errorRateLimit ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictRejected) ||
		audits[0]["a2a_attempted"] != false {
		t.Fatalf("rate-limit durable audit = %#v", audits)
	}
	if got := counterValue(t, delegationsTotal.WithLabelValues(job.GhostLocalpart, outcomeRateLimited)); got != before+1 {
		t.Fatalf("rate-limit delegation metric = %v, want %v", got, before+1)
	}
}

func TestDurableAdmissionLostAcknowledgementDoesNotRefundInvocationBudget(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	fixedNow := time.Now()
	b.senderLimits = newLimitersWithClock(60, 1, 10, func() time.Time { return fixedNow })
	b.roomLimits = newLimitersWithClock(60, 1, 10, func() time.Time { return fixedNow })
	b.store = &lostAdmissionAcknowledgementStore{Store: b.store}
	job := admitAndClaimDurableJob(t, b, "$durable-admission-ack-lost")

	if err := b.executePendingJob(t.Context(), &job); err == nil {
		t.Fatal("lost admission acknowledgement unexpectedly succeeded")
	}
	stored := loadDurableJob(t, b, job.JobID)
	if !stored.AdmissionChecked || !stored.AdmissionAllowed || stored.AdmissionReason != "" {
		t.Fatalf("persisted admission = (%v, %v, %q), want allowed",
			stored.AdmissionChecked, stored.AdmissionAllowed, stored.AdmissionReason)
	}
	if client.callCount != 0 {
		t.Fatalf("lost admission acknowledgement reached A2A %d times", client.callCount)
	}
	sender := matrixSender(id.NewUserID("alice", ownServer))
	if b.senderLimits.Allow(sender.rateLimitKey(job.GhostLocalpart)) {
		t.Fatal("lost admission acknowledgement refunded the sender invocation token")
	}
	if b.roomLimits.Allow(job.RoomID) {
		t.Fatal("lost admission acknowledgement refunded the room invocation token")
	}
}

func TestDurableJobMakesLostA2AAcknowledgementTerminallyAmbiguous(t *testing.T) {
	client := &scriptedA2AClient{callErr: errors.Join(
		a2aclient.ErrSendAcknowledgementAmbiguous,
		errors.New("provider response was lost"),
	)}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-ambiguous")

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateAmbiguous || stored.ErrorCode != errorA2AAckAmbiguous {
		t.Fatalf("terminal outcome = (%q, %q), want (ambiguous, %q)",
			stored.State, stored.ErrorCode, errorA2AAckAmbiguous)
	}
	if client.callCount != 1 {
		t.Fatalf("A2A calls = %d, want exactly one", client.callCount)
	}
	if events := recorder.snapshot(); len(events) != 1 || !containsAll(events[0].Body, "may have received", "did not resend") {
		t.Fatalf("ambiguity notice = %+v", events)
	}
	if _, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: time.Now().Add(time.Hour), LeaseDuration: time.Minute,
	}); err != nil || found {
		t.Fatalf("terminal ambiguous job was reclaimable: found=%v err=%v", found, err)
	}
}

func TestDurableAmbiguitySurvivesExhaustedMatrixOutbox(t *testing.T) {
	client := &scriptedA2AClient{callErr: a2aclient.ErrSendAcknowledgementAmbiguous}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.DelegationMaxAttempts = 1
	b.as.HTTPClient = &http.Client{Transport: bridgeRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("Matrix response unavailable")
	})}
	job := admitAndClaimDurableJob(t, b, "$durable-ambiguous-matrix-failure")

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateAmbiguous || stored.ErrorCode != errorA2AAckAmbiguous {
		t.Fatalf("outbox-exhausted ambiguity = (%q, %q), want (ambiguous, %q)",
			stored.State, stored.ErrorCode, errorA2AAckAmbiguous)
	}
	if client.callCount != 1 {
		t.Fatalf("outbox failure caused %d A2A calls, want one", client.callCount)
	}
}

func TestDurablePreparedRecoveryNeverResendsUnknownA2AOutcome(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-prepared-crash")
	if err := b.store.Transition(t.Context(), state.TransitionRequest{
		Lease: job.LeaseToken(), From: state.StatePending, To: state.StateA2APrepared, At: time.Now(),
	}); err != nil {
		t.Fatalf("prepare job: %v", err)
	}
	job.State = state.StateA2APrepared

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateAmbiguous {
		t.Fatalf("recovered prepared state = %q, want ambiguous", stored.State)
	}
	if client.callCount != 0 {
		t.Fatalf("recovered prepared job resent A2A %d times", client.callCount)
	}
}

func TestDurablePreparedPreflightRetryRebuildsInboundMedia(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "processed", Terminal: true}}
	b, _, _, fixture := mediaHarness(t, client)
	configureDurableTestBridge(b)
	data := []byte("h1,h2\n1,2")
	fixture.seedDownload("durable-input", data)
	body := transactionBody(t, map[string]any{
		"event_id": "$durable-media-retry", "room_id": "!room:" + ownServer,
		"sender": "@alice:" + ownServer, "type": "m.room.message", "origin_server_ts": int64(1),
		"content": map[string]any{
			"msgtype": "m.file", "body": "input.csv", "filename": "input.csv",
			"url":        "mxc://" + ownServer + "/durable-input",
			"info":       map[string]any{"mimetype": "text/csv", "size": len(data)},
			"m.mentions": map[string]any{"user_ids": []string{"@agent-k8s:" + ownServer}},
		},
	})
	result, err := b.AdmitAppserviceTransaction(t.Context(), "txn-durable-media-retry", body)
	if err != nil || len(result.InsertedJobIDs) != 1 {
		t.Fatalf("admit media job = (%+v, %v)", result, err)
	}
	job, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "worker", Now: time.Now(), LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim media job = (%v, %v)", found, err)
	}
	code := errorA2APreflightRetry
	if err := b.store.Transition(t.Context(), state.TransitionRequest{
		Lease: job.LeaseToken(), From: state.StatePending, To: state.StateA2APrepared, At: time.Now(),
		Patch: state.TransitionPatch{ErrorCode: &code},
	}); err != nil {
		t.Fatalf("prepare retryable media job: %v", err)
	}
	job.State = state.StateA2APrepared
	job.ErrorCode = code

	b.executeDurableJob(t.Context(), job)
	if stored := loadDurableJob(t, b, job.JobID); stored.State != state.StateDelivered {
		t.Fatalf("recovered media state = %q, want delivered", stored.State)
	}
	if client.callCount != 1 || len(client.callFiles) != 1 ||
		client.callFiles[0].Name != "input.csv" || string(client.callFiles[0].Bytes) != string(data) {
		t.Fatalf("recovered A2A media = calls %d files %+v", client.callCount, client.callFiles)
	}
}

func TestDurableReplyWithholdsArtifactsAfterMappingChange(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	job := admitAndClaimDurableJob(t, b, "$durable-mapping-change")
	var payload durablePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode admitted payload: %v", err)
	}
	payload.Result = &a2aclient.Result{
		Text: "completed", Terminal: true,
		Files: []a2aclient.ResultFile{{Name: "secret.csv", MIMEType: "text/csv", Bytes: []byte("secret")}},
	}
	payload.TerminalState = state.StateDelivered
	payload.Audit = durableTerminalAuditState{
		Outcome: outcomeOK, TerminalStage: "message_result", TerminalReason: "completed", A2AAttempted: true,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode persisted result: %v", err)
	}
	if err := b.store.Transition(t.Context(), state.TransitionRequest{
		Lease: job.LeaseToken(), From: state.StatePending, To: state.StateReplyPending, At: time.Now(),
		Patch: state.TransitionPatch{Payload: &encoded},
	}); err != nil {
		t.Fatalf("persist pending reply: %v", err)
	}
	job.State = state.StateReplyPending
	job.Payload = encoded
	replacement, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s: {namespace: kagent, name: replacement-agent}
`))
	if err != nil {
		t.Fatalf("load replacement mapping: %v", err)
	}
	b.agents.Replace(replacement)

	b.executeDurableJob(t.Context(), job)
	if stored := loadDurableJob(t, b, job.JobID); stored.State != state.StateDelivered {
		t.Fatalf("mapping-changed reply state = %q, want delivered", stored.State)
	}
	events := recorder.snapshot()
	if len(events) != 1 || !containsAll(events[0].Body, "completed", "artifact", "withheld", "mapping changed") {
		t.Fatalf("mapping-changed projection = %+v", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["agent_path"] != "" ||
		audits[0]["target_fingerprint"] != job.TargetFingerprint {
		t.Fatalf("mapping-changed immutable audit = %#v", audits)
	}
}

func TestDurableKnownTaskResumesWithGetAndReusesPlaceholder(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{Text: "working", TaskID: "task-known", ContextID: "context-known"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			Text: "finished", TaskID: "task-known", ContextID: "context-final", Terminal: true,
		}}},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-task")

	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	if waiting.State != state.StateAwaitingTask || waiting.A2ATaskID != "task-known" ||
		waiting.MatrixPlaceholderEventID == "" || waiting.LeaseOwner != "" || waiting.AttemptCount != 0 {
		t.Fatalf("awaiting task evidence = %+v", waiting)
	}
	placeholder := id.EventID(waiting.MatrixPlaceholderEventID)
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: waiting.NextAttemptAt.Add(time.Millisecond), LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim known task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered {
		t.Fatalf("resumed task state = %q, want delivered", stored.State)
	}
	if client.callCount != 1 || client.resumeCount != 1 {
		t.Fatalf("SendMessage calls=%d GetTask calls=%d, want 1/1", client.callCount, client.resumeCount)
	}
	events := recorder.snapshot()
	if len(events) != 2 || events[0].Body != workingText || events[1].Body != "* finished" {
		t.Fatalf("task Matrix events = %+v", events)
	}
	if got := events[1].RelatesTo.GetReplaceID(); got != placeholder {
		t.Fatalf("terminal edit target = %q, want placeholder %q", got, placeholder)
	}
}

func TestDurableTaskPollingUsesPersistedExponentialCursor(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-cursor", ContextID: "context-cursor"},
		polls: []scriptedPoll{
			{result: a2aclient.Result{TaskID: "task-cursor", ContextID: "context-cursor"}},
			{result: a2aclient.Result{
				Text: "finished", TaskID: "task-cursor", ContextID: "context-cursor", Terminal: true,
			}},
		},
	}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.pollInitial = time.Second
	b.pollMax = 8 * time.Second
	job := admitAndClaimDurableJob(t, b, "$durable-poll-cursor")

	b.executeDurableJob(t.Context(), job)
	first := loadDurableJob(t, b, job.JobID)
	if first.PollCount != 1 || first.NextAttemptAt.Sub(first.UpdatedAt) != time.Second {
		t.Fatalf("first poll cursor = count %d delay %s, want 1/1s",
			first.PollCount, first.NextAttemptAt.Sub(first.UpdatedAt))
	}
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "poll-1", Now: first.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim first poll = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)
	second := loadDurableJob(t, b, job.JobID)
	if second.PollCount != 2 || second.NextAttemptAt.Sub(second.UpdatedAt) != 2*time.Second {
		t.Fatalf("second poll cursor = count %d delay %s, want 2/2s",
			second.PollCount, second.NextAttemptAt.Sub(second.UpdatedAt))
	}
	claimed, found, err = b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "poll-2", Now: second.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim second poll = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)
	if terminal := loadDurableJob(t, b, job.JobID); terminal.State != state.StateDelivered || terminal.PollCount != 0 {
		t.Fatalf("terminal poll cursor = state %s count %d, want delivered/0", terminal.State, terminal.PollCount)
	}
}

func TestDurableAwaitingTaskDenialEditsExistingPlaceholder(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-policy", ContextID: "context-policy"},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-task-policy-change")
	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	if waiting.State != state.StateAwaitingTask || waiting.MatrixPlaceholderEventID == "" {
		t.Fatalf("waiting task = %+v", waiting)
	}
	replacement, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s: {namespace: kagent, name: replacement-agent}
`))
	if err != nil {
		t.Fatalf("load replacement mapping: %v", err)
	}
	b.agents.Replace(replacement)
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: waiting.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim policy-changed task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDenied || stored.ErrorCode != errorAgentMappingChanged {
		t.Fatalf("policy-changed task = (%s, %s), want denied/%s",
			stored.State, stored.ErrorCode, errorAgentMappingChanged)
	}
	events := recorder.snapshot()
	if len(events) != 2 || events[0].Body != workingText ||
		!containsAll(events[1].Body, "could not safely continue", "agent-k8s") ||
		events[1].RelatesTo.GetReplaceID() != id.EventID(waiting.MatrixPlaceholderEventID) {
		t.Fatalf("policy-changed placeholder projection = %+v", events)
	}
}

func TestDurableJobDeadLettersAfterBoundedConsecutiveFailures(t *testing.T) {
	client := &scriptedA2AClient{callErr: errors.New("local card preflight unavailable")}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.DelegationMaxAttempts = 2
	job := admitAndClaimDurableJob(t, b, "$durable-bounded-failure")

	b.executeDurableJob(t.Context(), job)
	retrying := loadDurableJob(t, b, job.JobID)
	if retrying.State != state.StateA2APrepared || retrying.AttemptCount != 1 ||
		retrying.ErrorCode != errorA2APreflightRetry || retrying.LeaseOwner != "" {
		t.Fatalf("first retry evidence = %+v", retrying)
	}
	reclaimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: retrying.NextAttemptAt.Add(time.Millisecond), LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("reclaim failed job = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), reclaimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDead || stored.ErrorCode != errorA2APreflightRetry || stored.AttemptCount != 0 {
		t.Fatalf("dead-letter evidence = %+v", stored)
	}
	if client.callCount != 2 {
		t.Fatalf("A2A attempts = %d, want bounded total 2", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || !containsAll(events[0].Body, "could not be recovered", "repeated failures") {
		t.Fatalf("dead-letter Matrix notice = %+v", events)
	}
}

func TestDurableJobKeepsWholeTaskDeadlineAcrossRecovery(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.TaskTimeout = time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-expired")
	var payload durablePayload
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("decode recovery payload: %v", err)
	}
	payload.Audit.A2AAttempted = true
	payload.Audit.A2AStartedAt = time.Now().Add(-2 * time.Minute).UnixMilli()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("encode recovery payload: %v", err)
	}
	if err := b.store.Transition(t.Context(), state.TransitionRequest{
		Lease: job.LeaseToken(), From: state.StatePending, To: state.StateA2APrepared, At: time.Now(),
		Patch: state.TransitionPatch{Payload: &encoded},
	}); err != nil {
		t.Fatalf("persist prepared task: %v", err)
	}
	taskID := "task-expired"
	if err := b.store.Transition(t.Context(), state.TransitionRequest{
		Lease: job.LeaseToken(), From: state.StateA2APrepared, To: state.StateAwaitingTask, At: time.Now(),
		Patch: state.TransitionPatch{A2ATaskID: &taskID},
	}); err != nil {
		t.Fatalf("persist awaiting task: %v", err)
	}
	job = loadDurableJob(t, b, job.JobID)

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || stored.ErrorCode != errorTaskTimeout {
		t.Fatalf("expired durable outcome = (%q, %q), want delivered/%s",
			stored.State, stored.ErrorCode, errorTaskTimeout)
	}
	if client.callCount != 0 {
		t.Fatalf("expired durable job reached A2A %d times", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || !containsAll(events[0].Body, "did not finish", "1m0s") {
		t.Fatalf("expired durable notice = %+v", events)
	}
}

func TestDurableTaskTimeoutStartsAtFirstA2AAttemptNotQueueAdmission(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "started after backlog", Terminal: true}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.TaskTimeout = time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-old-backlog")
	job.CreatedAt = time.Now().Add(-2 * time.Minute)

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || client.callCount != 1 {
		t.Fatalf("old queued job = state %s calls %d, want delivered/1", stored.State, client.callCount)
	}
}

func TestDurableJobTerminatesUnsupportedPausedTaskHonestly(t *testing.T) {
	tests := []struct {
		name      string
		result    a2aclient.Result
		errorCode string
		message   string
	}{
		{
			name: "input required",
			result: a2aclient.Result{
				TaskID: "task-input", ContextID: "context-input", InputRequired: true,
			},
			errorCode: errorInputRequired,
			message:   "needs more input",
		},
		{
			name: "authorization required",
			result: a2aclient.Result{
				TaskID: "task-auth", ContextID: "context-auth", AuthRequired: true,
			},
			errorCode: errorAuthRequired,
			message:   "authorization the platform does not forward",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedA2AClient{callResult: tt.result}
			b, _, _, _, recorder := pollingHarness(t, client)
			configureDurableTestBridge(b)
			job := admitAndClaimDurableJob(t, b, "$durable-paused-"+strings.ReplaceAll(tt.name, " ", "-"))

			b.executeDurableJob(t.Context(), job)
			stored := loadDurableJob(t, b, job.JobID)
			if stored.State != state.StateDelivered || stored.ErrorCode != tt.errorCode ||
				stored.A2ATaskID != tt.result.TaskID || stored.A2AContextID != tt.result.ContextID {
				t.Fatalf("terminal outcome = (%q, %q, %q, %q), want (delivered, %q, %q, %q)",
					stored.State, stored.ErrorCode, stored.A2ATaskID, stored.A2AContextID,
					tt.errorCode, tt.result.TaskID, tt.result.ContextID)
			}
			events := recorder.snapshot()
			if len(events) != 1 || !strings.Contains(events[0].Body, tt.message) {
				t.Fatalf("terminal notice = %+v, want %q", events, tt.message)
			}
		})
	}
}

func configureDurableTestBridge(b *Bridge) {
	b.cfg.DelegationMaxAttempts = 5
	b.cfg.DelegationRetryInitial = time.Millisecond
	b.cfg.DelegationRetryMax = time.Second
	b.cfg.TaskTimeout = time.Minute
	b.pollInitial = time.Millisecond
}

func admitAndClaimDurableJob(t *testing.T, b *Bridge, eventID string) state.Job {
	t.Helper()
	body := transactionBody(t, transactionEvent(eventID, "@alice:"+ownServer, "@agent-k8s inspect"))
	result, err := b.AdmitAppserviceTransaction(t.Context(), "txn-"+eventID, body)
	if err != nil {
		t.Fatalf("AdmitAppserviceTransaction: %v", err)
	}
	if len(result.InsertedJobIDs) != 1 {
		t.Fatalf("inserted jobs = %v, want one", result.InsertedJobIDs)
	}
	job, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "worker", Now: time.Now(), LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("Claim = (%v, %v)", found, err)
	}
	return job
}

func loadDurableJob(t *testing.T, b *Bridge, jobID string) state.Job {
	t.Helper()
	job, found, err := b.store.Job(t.Context(), jobID)
	if err != nil || !found {
		t.Fatalf("Job(%q) = (%v, %v)", jobID, found, err)
	}
	return job
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}

func TestDurableBackoffUsesConsecutiveFailureOrdinal(t *testing.T) {
	for _, test := range []struct {
		attempt int
		want    time.Duration
	}{
		{attempt: 1, want: time.Second},
		{attempt: 2, want: 2 * time.Second},
		{attempt: 3, want: 4 * time.Second},
		{attempt: 30, want: 30 * time.Second},
	} {
		if got := durableBackoff(time.Second, 30*time.Second, test.attempt); got != test.want {
			t.Errorf("durableBackoff attempt %d = %s, want %s", test.attempt, got, test.want)
		}
	}
}

type bridgeRoundTripFunc func(*http.Request) (*http.Response, error)

func (f bridgeRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}
