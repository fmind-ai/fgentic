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

type deadManRecordErrorStore struct {
	state.Store
	err error
}

type deadManRetryErrorStore struct {
	state.Store
	err error
}

func (s *deadManRecordErrorStore) RecordDeadMan(context.Context, state.DeadManRequest) error {
	return s.err
}

func (s *deadManRetryErrorStore) ScheduleRetry(context.Context, state.RetryRequest) error {
	return s.err
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
	if reply, ok := b.replies.lookup(id.EventID(stored.MatrixReplyEventID), id.RoomID(job.RoomID)); !ok || reply.ghost != job.GhostLocalpart {
		t.Fatalf("durable quality-reaction reply = (%+v, %v), want %s", reply, ok, job.GhostLocalpart)
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

func TestDurableFailureNoticeSuppressionPreservesTerminalReason(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	sender := matrixSender(id.NewUserID("alice", ownServer))
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) ||
		!b.noticeSenderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to exhaust invocation and notice limiter fixtures")
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	job := admitAndClaimDurableJob(t, b, "$durable-rate-limit-notice-suppressed")

	b.executeDurableJob(t.Context(), job)
	if client.callCount != 0 {
		t.Fatalf("suppressed rate-limit failure reached A2A %d times", client.callCount)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("exhausted failure-notice bucket emitted Matrix events: %#v", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["outcome"] != outcomeRateLimited ||
		audits[0]["terminal_reason"] != errorRateLimit ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictRejected) {
		t.Fatalf("suppressed failure audit = %#v", audits)
	}
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDenied || stored.ErrorCode != errorRateLimit || stored.ResultText != "" {
		t.Fatalf("suppressed failure evidence = %+v", stored)
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
	originalRef, ok := b.agents.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s fixture missing")
	}
	payload.AgentVersion = originalRef.AgentVersion()
	payload.AgentContract = originalRef.AgentContractSHA256()
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
		audits[0]["target_fingerprint"] != job.TargetFingerprint ||
		audits[0]["agent_version"] != payload.AgentVersion {
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
	deadMan := &fakeDeadManClient{supported: true}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	b.pollMax = time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-task")

	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	if waiting.State != state.StateAwaitingTask || waiting.A2ATaskID != "task-known" ||
		waiting.MatrixPlaceholderEventID == "" || waiting.MatrixDeadManDelayID == "" ||
		waiting.LeaseOwner != "" || waiting.AttemptCount != 0 {
		t.Fatalf("awaiting task evidence = %+v", waiting)
	}
	if len(deadMan.schedules) != 1 || len(deadMan.restarts) != 0 || len(deadMan.cancels) != 0 {
		t.Fatalf("initial durable dead-man calls = schedules %+v restarts %v cancels %v",
			deadMan.schedules, deadMan.restarts, deadMan.cancels)
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
	if len(deadMan.restarts) != 1 || len(deadMan.cancels) != 1 ||
		deadMan.restarts[0] != id.DelayID(waiting.MatrixDeadManDelayID) ||
		deadMan.cancels[0] != id.DelayID(waiting.MatrixDeadManDelayID) {
		t.Fatalf("terminal durable dead-man calls = restarts %v cancels %v", deadMan.restarts, deadMan.cancels)
	}
	events := recorder.snapshot()
	if len(events) != 2 || events[0].Body != workingText || events[1].Body != "* finished" {
		t.Fatalf("task Matrix events = %+v", events)
	}
	if got := events[1].RelatesTo.GetReplaceID(); got != placeholder {
		t.Fatalf("terminal edit target = %q, want placeholder %q", got, placeholder)
	}
}

func TestDurableDeadManRefreshFailureSchedulesPersistedShortRetry(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-refresh-retry", ContextID: "context-refresh-retry"},
		polls: []scriptedPoll{
			{result: a2aclient.Result{TaskID: "task-refresh-retry"}},
			{result: a2aclient.Result{TaskID: "task-refresh-retry"}},
			{result: a2aclient.Result{TaskID: "task-refresh-retry"}},
			{result: a2aclient.Result{TaskID: "task-refresh-retry"}},
			{result: a2aclient.Result{TaskID: "task-refresh-retry", Text: "finished", Terminal: true}},
		},
	}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	deadMan := &fakeDeadManClient{
		supported:   true,
		restartErrs: []error{nil, errors.New("homeserver unavailable"), nil},
	}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	b.cfg.RequestTimeout = 5 * time.Second
	b.pollInitial = 30 * time.Second
	b.pollMax = 30 * time.Second
	job := admitAndClaimDurableJob(t, b, "$durable-dead-man-refresh-retry")

	b.executeDurableJob(t.Context(), job)
	for poll := 0; poll < 4; poll++ {
		waiting := loadDurableJob(t, b, job.JobID)
		claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
			Owner: "poll-worker", Now: waiting.NextAttemptAt, LeaseDuration: time.Minute,
		})
		if err != nil || !found {
			t.Fatalf("claim poll %d = (%v, %v)", poll, found, err)
		}
		b.executeDurableJob(t.Context(), claimed)
	}

	retrying := loadDurableJob(t, b, job.JobID)
	if retrying.ErrorCode != errorDeadManRefresh ||
		retrying.NextAttemptAt.Sub(retrying.UpdatedAt) != 5*time.Second {
		t.Fatalf("failed refresh retry = code %q delay %s, want %q/5s",
			retrying.ErrorCode, retrying.NextAttemptAt.Sub(retrying.UpdatedAt), errorDeadManRefresh)
	}
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "refresh-retry", Now: retrying.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim refresh retry = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || len(deadMan.restarts) != 3 || len(deadMan.cancels) != 1 {
		t.Fatalf("refresh recovery = state %s restarts %v cancels %v, want delivered/3/1",
			stored.State, deadMan.restarts, deadMan.cancels)
	}
}

func TestDurableDeadManPersistenceFailureCancelsScheduledEvent(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-record-failure", ContextID: "context-record-failure",
	}}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.store = &deadManRecordErrorStore{Store: b.store, err: errors.New("database unavailable")}
	deadMan := &fakeDeadManClient{supported: true}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-dead-man-record-failure")

	b.executeDurableJob(t.Context(), job)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.MatrixDeadManDelayID != "" {
		t.Fatalf("failed persistence retained delay ID %q", stored.MatrixDeadManDelayID)
	}
	if len(deadMan.schedules) != 1 || len(deadMan.cancels) != 1 ||
		deadMan.cancels[0] != id.DelayID("delay-"+deadMan.schedules[0].txnID) {
		t.Fatalf("dead-man rollback calls = schedules %+v cancels %v", deadMan.schedules, deadMan.cancels)
	}
}

func TestDurableDeadManCancellationRetriesAfterTerminalProjection(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-cancel-retry", ContextID: "context-cancel-retry"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			Text: "finished", TaskID: "task-cancel-retry", ContextID: "context-final", Terminal: true,
		}}},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	deadMan := &fakeDeadManClient{supported: true, cancelErr: errors.New("homeserver unavailable")}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-dead-man-cancel-retry")

	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "terminal", Now: waiting.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim known task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)
	retrying := loadDurableJob(t, b, job.JobID)
	if retrying.State != state.StateReplyPending || retrying.AttemptCount != 1 || retrying.LeaseOwner != "" ||
		retrying.MatrixEditEventID == "" {
		t.Fatalf("cancel retry evidence = %+v", retrying)
	}
	if events := recorder.snapshot(); len(events) != 2 {
		t.Fatalf("events after accepted terminal projection = %+v", events)
	}

	deadMan.cancelErr = nil
	claimed, found, err = b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: retrying.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("reclaim terminal projection = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || stored.AttemptCount != 0 {
		t.Fatalf("recovered terminal state = %+v", stored)
	}
	if len(deadMan.cancels) != 2 || deadMan.cancels[0] != deadMan.cancels[1] {
		t.Fatalf("dead-man cancellation attempts = %v, want two identical IDs", deadMan.cancels)
	}
	if events := recorder.snapshot(); len(events) != 2 {
		t.Fatalf("cancellation retry re-projected terminal event: %+v", events)
	}
}

func TestDurableDeadManCancellationSurvivesDisabledStartupProbe(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-probe-disabled", ContextID: "context-probe-disabled"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			Text: "finished", TaskID: "task-probe-disabled", ContextID: "context-final", Terminal: true,
		}}},
	}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	deadMan := &fakeDeadManClient{supported: true}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-dead-man-probe-disabled")
	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	if waiting.MatrixDeadManDelayID == "" {
		t.Fatal("awaiting task has no persisted delayed-event ID")
	}

	// A restart may fail its capability probe or disable new scheduling. Persisted Synapse timers
	// remain cleanup obligations regardless of the current scheduling capability.
	b.deadManEnabled = false
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: waiting.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim known task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered {
		t.Fatalf("probe-disabled terminal state = %s", stored.State)
	}
	if len(deadMan.cancels) != 1 || deadMan.cancels[0] != id.DelayID(waiting.MatrixDeadManDelayID) {
		t.Fatalf("probe-disabled cleanup cancels = %v", deadMan.cancels)
	}
}

func TestDurableDeadManCancellationIgnoresDeliveryAttemptLimit(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-cancel-exhausted", ContextID: "context-cancel-exhausted"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			Text: "finished", TaskID: "task-cancel-exhausted", ContextID: "context-final", Terminal: true,
		}}},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	deadMan := &fakeDeadManClient{supported: true, cancelErr: errors.New("homeserver unavailable")}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-dead-man-cancel-exhausted")
	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	b.cfg.DelegationMaxAttempts = 1
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "terminal", Now: waiting.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim known task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	retrying := loadDurableJob(t, b, job.JobID)
	if retrying.State != state.StateReplyPending || retrying.AttemptCount != 1 ||
		retrying.MatrixEditEventID == "" || retrying.MatrixDeadManDelayID == "" {
		t.Fatalf("cleanup retry after delivery-attempt exhaustion = %+v", retrying)
	}
	if events := recorder.snapshot(); len(events) != 2 {
		t.Fatalf("terminal events = %+v", events)
	}

	deadMan.cancelErr = nil
	claimed, found, err = b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "cleanup", Now: retrying.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim cleanup retry = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)
	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || stored.MatrixEditEventID == "" ||
		stored.MatrixDeadManDelayID == "" {
		t.Fatalf("terminal Matrix evidence after cleanup = %+v", stored)
	}
	if len(deadMan.cancels) != 2 || deadMan.cancels[0] != deadMan.cancels[1] {
		t.Fatalf("dead-man cleanup attempts = %v, want two identical IDs", deadMan.cancels)
	}
}

func TestDurableDeadManCancellationRetryPersistenceFailureStaysNonTerminal(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-cleanup-store", ContextID: "context-cleanup-store"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			Text: "finished", TaskID: "task-cleanup-store", ContextID: "context-final", Terminal: true,
		}}},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	deadMan := &fakeDeadManClient{supported: true, cancelErr: errors.New("homeserver unavailable")}
	b.deadMan = deadMan
	b.deadManEnabled = true
	b.cfg.DeadManSwitchDelay = 2 * time.Minute
	job := admitAndClaimDurableJob(t, b, "$durable-dead-man-cleanup-store")
	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	b.cfg.DelegationMaxAttempts = 1
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "terminal", Now: waiting.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim known task = (%v, %v)", found, err)
	}
	b.store = &deadManRetryErrorStore{Store: b.store, err: errors.New("database unavailable")}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateReplyPending || !stored.TerminalAt.IsZero() ||
		stored.MatrixEditEventID == "" || stored.MatrixDeadManDelayID == "" ||
		stored.LeaseOwner != "terminal" {
		t.Fatalf("cleanup retry persistence failure = %+v", stored)
	}
	if events := recorder.snapshot(); len(events) != 2 {
		t.Fatalf("terminal events = %+v", events)
	}
}

func TestDurableKnownTaskKeepsPollingAcrossContractOnlyChange(t *testing.T) {
	const originalContract = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{Text: "working", TaskID: "task-known", ContextID: "context-known"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			Text: "finished", TaskID: "task-known", ContextID: "context-final", Terminal: true,
		}}},
	}
	b, _, _, _, _ := pollingHarness(t, client)
	configureDurableTestBridge(b)
	original, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    agentContractSHA256: `+originalContract+`
`))
	if err != nil {
		t.Fatalf("load original contract mapping: %v", err)
	}
	b.agents.Replace(original)
	job := admitAndClaimDurableJob(t, b, "$durable-task-contract-change")
	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	if waiting.State != state.StateAwaitingTask {
		t.Fatalf("awaiting task state = %s", waiting.State)
	}
	replacement, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    agentContractSHA256: `+strings.Repeat("a", 64)+`
`))
	if err != nil {
		t.Fatalf("load replacement contract mapping: %v", err)
	}
	b.agents.Replace(replacement)
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: waiting.NextAttemptAt.Add(time.Millisecond), LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim known task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || client.callCount != 1 || client.resumeCount != 1 {
		t.Fatalf("contract-changed known task = state %s, send/resume %d/%d", stored.State, client.callCount, client.resumeCount)
	}
}

func TestDurableTaskFailureEditsPlaceholderWhenNoticeBudgetIsExhausted(t *testing.T) {
	client := &scriptedA2AClient{
		callResult: a2aclient.Result{TaskID: "task-failed", ContextID: "context-failed"},
		polls: []scriptedPoll{{result: a2aclient.Result{
			TaskID: "task-failed", ContextID: "context-failed", Terminal: true, Failed: true,
		}}},
	}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-task-failed-notice-exhausted")
	b.executeDurableJob(t.Context(), job)
	waiting := loadDurableJob(t, b, job.JobID)
	if waiting.State != state.StateAwaitingTask || waiting.MatrixPlaceholderEventID == "" {
		t.Fatalf("waiting task = %+v", waiting)
	}
	sender := matrixSender(id.NewUserID("alice", ownServer))
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	if !b.noticeSenderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to exhaust notice limiter fixture")
	}
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: waiting.NextAttemptAt.Add(time.Millisecond), LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim failed task = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered || stored.ErrorCode != errorTaskFailed {
		t.Fatalf("failed task = (%s, %s), want delivered/%s", stored.State, stored.ErrorCode, errorTaskFailed)
	}
	events := recorder.snapshot()
	if len(events) != 2 || events[1].NewContent == nil ||
		events[1].NewContent.Body != failureMessage(errorTaskFailed, "agent-k8s", 0) ||
		events[1].RelatesTo.GetReplaceID() != id.EventID(waiting.MatrixPlaceholderEventID) {
		t.Fatalf("failed task placeholder projection = %+v", events)
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
	sender := matrixSender(id.NewUserID("alice", ownServer))
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	if !b.noticeSenderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to exhaust notice limiter fixture")
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
		events[1].NewContent == nil ||
		events[1].NewContent.Body != failureMessage(errorAgentMappingChanged, "agent-k8s", 0) ||
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

func TestDurablePreparedRetryRejectsContractOnlyChange(t *testing.T) {
	const originalContract = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	client := &scriptedA2AClient{callErr: errors.New("local card preflight unavailable")}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	original, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    agentContractSHA256: `+originalContract+`
`))
	if err != nil {
		t.Fatalf("load original contract mapping: %v", err)
	}
	b.agents.Replace(original)
	job := admitAndClaimDurableJob(t, b, "$durable-contract-change")

	b.executeDurableJob(t.Context(), job)
	retrying := loadDurableJob(t, b, job.JobID)
	if retrying.State != state.StateA2APrepared || retrying.ErrorCode != errorA2APreflightRetry {
		t.Fatalf("prepared retry = (%s, %s), want (%s, %s)", retrying.State, retrying.ErrorCode,
			state.StateA2APrepared, errorA2APreflightRetry)
	}
	replacement, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    agentContractSHA256: `+strings.Repeat("a", 64)+`
`))
	if err != nil {
		t.Fatalf("load replacement contract mapping: %v", err)
	}
	b.agents.Replace(replacement)
	claimed, found, err := b.store.Claim(t.Context(), state.ClaimRequest{
		Owner: "replacement", Now: retrying.NextAttemptAt, LeaseDuration: time.Minute,
	})
	if err != nil || !found {
		t.Fatalf("claim prepared retry = (%v, %v)", found, err)
	}
	b.executeDurableJob(t.Context(), claimed)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDenied || stored.ErrorCode != errorAgentMappingChanged {
		t.Fatalf("contract-changed retry = (%s, %s), want denied/%s",
			stored.State, stored.ErrorCode, errorAgentMappingChanged)
	}
	if client.callCount != 1 {
		t.Fatalf("contract-changed retry made %d A2A calls, want only the original preflight", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorAgentMappingChanged, "agent-k8s", 0) {
		t.Fatalf("contract-changed denial notice = %+v", events)
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
			message:   "needs authorization",
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
