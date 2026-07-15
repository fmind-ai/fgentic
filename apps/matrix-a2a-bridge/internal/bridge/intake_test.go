package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

func TestDurableIntakeGateWaitsForEveryOutstandingConsumer(t *testing.T) {
	var gate durableIntakeGate
	releaseFirst := gate.hold()
	releaseSecond := gate.hold()

	waitStarted := make(chan struct{})
	waitDone := make(chan error, 1)
	go func() {
		close(waitStarted)
		waitDone <- gate.wait(t.Context())
	}()
	<-waitStarted
	releaseFirst()
	releaseFirst() // Releases are idempotent because HTTP cleanup can run during panic unwinding.
	select {
	case err := <-waitDone:
		t.Fatalf("gate opened with one consumer still outstanding: %v", err)
	default:
	}
	releaseSecond()
	select {
	case err := <-waitDone:
		if err != nil {
			t.Fatalf("wait after all consumers returned: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("gate remained closed after every consumer returned")
	}

	releaseCanceled := gate.hold()
	canceledCtx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := gate.wait(canceledCtx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait error = %v, want context canceled", err)
	}
	releaseCanceled()
	if err := gate.wait(t.Context()); err != nil {
		t.Fatalf("open gate wait: %v", err)
	}
}

func TestDelegationsFromTransactionPreservesEventAndTargetOrder(t *testing.T) {
	b := testBridge(t)
	body := transactionBody(
		t,
		transactionEvent("$first", "@slack_alice:"+ownServer, "@agent-k8s @agent-slack inspect"),
		transactionEvent("$second", "@alice:"+ownServer, "@agent-k8s retry"),
	)

	jobs, err := b.delegationsFromTransaction(body)
	if err != nil {
		t.Fatalf("delegationsFromTransaction: %v", err)
	}
	if len(jobs) != 3 {
		t.Fatalf("jobs = %d, want 3", len(jobs))
	}
	want := []struct {
		eventID, ghost, originKind, prompt string
	}{
		{"$first", "agent-slack", "bridge", "inspect"},
		{"$first", "agent-k8s", "bridge", "inspect"},
		{"$second", "agent-k8s", "matrix", "retry"},
	}
	for index, expected := range want {
		job := jobs[index]
		if job.MatrixEventID != expected.eventID || job.GhostLocalpart != expected.ghost ||
			job.SenderOriginKind != expected.originKind || job.Prompt != expected.prompt {
			t.Errorf("job %d = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
				index, job.MatrixEventID, job.GhostLocalpart, job.SenderOriginKind, job.Prompt,
				expected.eventID, expected.ghost, expected.originKind, expected.prompt)
		}
		if job.TargetFingerprint == "" || job.GhostMXID == "" || len(job.Payload) == 0 {
			t.Errorf("job %d omitted immutable routing or recovery evidence: %+v", index, job)
		}
		var payload durablePayload
		if err := json.Unmarshal(job.Payload, &payload); err != nil {
			t.Errorf("job %d payload envelope is invalid: %v", index, err)
			continue
		}
		var recovered event.Event
		if err := json.Unmarshal(payload.Event, &recovered); err != nil {
			t.Errorf("job %d payload is not a recoverable Matrix event: %v", index, err)
		} else if recovered.ID.String() != expected.eventID {
			t.Errorf("job %d recovered event = %q, want %q", index, recovered.ID, expected.eventID)
		}
	}
}

func TestDelegationsFromTransactionIgnoresNonDelegationTraffic(t *testing.T) {
	b := testBridge(t)
	stateKey := "@alice:" + ownServer
	body := transactionBody(
		t,
		transactionEvent("$ordinary", "@alice:"+ownServer, "hello room"),
		transactionEvent("$directory", "@alice:"+ownServer, "!agents"),
		transactionEvent("$own", "@agent-k8s:"+ownServer, "@agent-k8s loop"),
		map[string]any{
			"event_id": "$notice", "room_id": "!room:" + ownServer,
			"sender": "@alice:" + ownServer, "type": "m.room.message",
			"origin_server_ts": int64(1),
			"content":          map[string]any{"msgtype": "m.notice", "body": "@agent-k8s ignored"},
		},
		map[string]any{
			"event_id": "$state", "room_id": "!room:" + ownServer,
			"sender": "@alice:" + ownServer, "type": "m.room.member", "state_key": stateKey,
			"origin_server_ts": int64(1), "content": map[string]any{"membership": "join"},
		},
	)

	jobs, err := b.delegationsFromTransaction(body)
	if err != nil {
		t.Fatalf("delegationsFromTransaction: %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("non-delegation jobs = %d, want 0", len(jobs))
	}
}

func TestDelegationsFromTransactionIgnoresMalformedMessageWithoutPoisoningTransaction(t *testing.T) {
	b := testBridge(t)
	body := transactionBody(
		t,
		map[string]any{
			"event_id": "$bad", "room_id": "!room:" + ownServer,
			"sender": "@alice:" + ownServer, "type": "m.room.message",
			"origin_server_ts": int64(1),
			"content":          map[string]any{"msgtype": []string{"not", "a", "string"}, "body": "ordinary"},
		},
		transactionEvent("$valid-after-bad", "@alice:"+ownServer, "@agent-k8s inspect"),
	)

	jobs, err := b.delegationsFromTransaction(body)
	if err != nil || len(jobs) != 1 || jobs[0].MatrixEventID != "$valid-after-bad" {
		t.Fatalf("mixed malformed transaction = (%+v, %v), want later valid delegation", jobs, err)
	}
	if _, err := b.delegationsFromTransaction([]byte(`{"events":`)); err == nil {
		t.Fatal("malformed transaction JSON was accepted")
	}
}

func TestDelegationsFromTransactionRetainsOnlyMinimalRecoveryEvent(t *testing.T) {
	b := testBridge(t)
	body := transactionBody(t, map[string]any{
		"event_id": "$minimal", "room_id": "!room:" + ownServer,
		"sender": "@alice:" + ownServer, "type": "m.room.message", "origin_server_ts": int64(7),
		"unsigned": map[string]any{"transaction_id": "secret-client-transaction"},
		"content": map[string]any{
			"msgtype": "m.text", "body": "@agent-k8s private prompt sentinel",
			"format": "org.matrix.custom.html", "formatted_body": "<b>private formatted sentinel</b>",
			"m.mentions":            map[string]any{"user_ids": []string{"@agent-k8s:" + ownServer}},
			"com.example.untrusted": "arbitrary extension sentinel",
		},
	})

	jobs, err := b.delegationsFromTransaction(body)
	if err != nil || len(jobs) != 1 {
		t.Fatalf("minimal recovery jobs = (%+v, %v)", jobs, err)
	}
	if jobs[0].Prompt != "private prompt sentinel" {
		t.Fatalf("dedicated prompt = %q", jobs[0].Prompt)
	}
	stored := string(jobs[0].Payload)
	for _, forbidden := range []string{
		"private prompt sentinel", "private formatted sentinel", "secret-client-transaction",
		"arbitrary extension sentinel", "m.mentions",
	} {
		if strings.Contains(stored, forbidden) {
			t.Errorf("recovery payload retained %q: %s", forbidden, stored)
		}
	}
}

func TestAdmitAppserviceTransactionCapacityDenialAuditsTerminalOutcome(t *testing.T) {
	for _, test := range []struct {
		name           string
		firstRoom      string
		roomCapacity   int
		globalCapacity int
		wantReason     string
	}{
		{
			name: "room capacity", firstRoom: "!room:" + ownServer,
			roomCapacity: 1, globalCapacity: 10, wantReason: state.QueueRoomCapacityRejected,
		},
		{
			name: "global capacity", firstRoom: "!other:" + ownServer,
			roomCapacity: 10, globalCapacity: 1, wantReason: state.QueueGlobalCapacityRejected,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := testBridge(t)
			b.cfg.RoomQueueCapacity = test.roomCapacity
			b.cfg.GlobalQueueCapacity = test.globalCapacity
			var output strings.Builder
			setBridgeLogOutput(b, &output)

			first := transactionEvent("$capacity-first", "@alice:"+ownServer, "@agent-k8s first private sentinel")
			first["room_id"] = test.firstRoom
			firstResult, err := b.AdmitAppserviceTransaction(
				t.Context(), "txn-capacity-first", transactionBody(t, first),
			)
			if err != nil || len(firstResult.InsertedJobIDs) != 1 {
				t.Fatalf("first admission = (%+v, %v), want one pending job", firstResult, err)
			}
			before := counterValue(t, delegationsTotal.WithLabelValues("agent-k8s", outcomeQueueFull))
			result, err := b.AdmitAppserviceTransaction(
				t.Context(),
				"txn-capacity-overflow",
				transactionBody(t, transactionEvent(
					"$capacity-overflow", "@alice:"+ownServer, "@agent-k8s second private sentinel",
				)),
			)
			if err != nil || len(result.CapacityDenied) != 1 ||
				result.CapacityDenied[0].Reason != test.wantReason {
				t.Fatalf("overflow admission = (%+v, %v), want one %q denial", result, err, test.wantReason)
			}
			if got := counterValue(t, delegationsTotal.WithLabelValues("agent-k8s", outcomeQueueFull)); got != before+1 {
				t.Fatalf("queue-full metric = %v, want %v", got, before+1)
			}

			audits := auditRecords(t, output.String())
			if len(audits) != 1 ||
				audits[0]["outcome"] != outcomeQueueFull ||
				audits[0]["terminal_stage"] != "queue" ||
				audits[0]["terminal_reason"] != test.wantReason ||
				audits[0]["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) ||
				audits[0]["target_fingerprint"] == "" ||
				audits[0]["a2a_attempted"] != false {
				t.Fatalf("capacity audit = %#v", audits)
			}
			for _, sentinel := range []string{"first private sentinel", "second private sentinel"} {
				if strings.Contains(output.String(), sentinel) {
					t.Fatalf("capacity logs retained private content %q", sentinel)
				}
			}

			denied, found, err := b.store.Job(t.Context(), result.CapacityDenied[0].JobID)
			if err != nil || !found || denied.State != state.StateDenied ||
				denied.ErrorCode != test.wantReason || denied.Prompt != "" ||
				len(denied.Payload) != 0 || denied.ResultText != "" || denied.TerminalAt.IsZero() {
				t.Fatalf("capacity tombstone = (%+v, %v, %v)", denied, found, err)
			}
		})
	}
}

func TestAdmitAppserviceTransactionChangedReplayAuditsContentFreeTamperEvidence(t *testing.T) {
	b := testBridge(t)
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	original := transactionBody(t, transactionEvent(
		"$tamper", "@alice:"+ownServer, "@agent-k8s original private sentinel",
	))
	if _, err := b.AdmitAppserviceTransaction(t.Context(), "txn-tamper", original); err != nil {
		t.Fatalf("first admission: %v", err)
	}
	output.Reset()
	changed := transactionBody(t, transactionEvent(
		"$tamper", "@alice:"+ownServer, "@agent-k8s changed private sentinel",
	))
	if _, err := b.AdmitAppserviceTransaction(t.Context(), "txn-tamper", changed); !errors.Is(err, state.ErrTransactionHashConflict) {
		t.Fatalf("changed replay error = %v, want transaction hash conflict", err)
	}

	var record map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(output.String())), &record); err != nil {
		t.Fatalf("decode transaction conflict audit: %v", err)
	}
	if record["msg"] != "appservice transaction conflict" ||
		record["log_stream"] != delegationAuditStream ||
		record["audit_schema"] != "fgentic.appservice_transaction.v1" ||
		record["transaction_id"] != "txn-tamper" ||
		record["outcome"] != "rejected" ||
		record["terminal_reason"] != "transaction_hash_conflict" {
		t.Fatalf("transaction conflict audit = %#v", record)
	}
	for _, field := range []string{"stored_body_sha256", "received_body_sha256"} {
		if value, ok := record[field].(string); !ok || len(value) != 64 {
			t.Errorf("transaction conflict %s = %#v, want SHA-256 hex", field, record[field])
		}
	}
	if record["stored_body_sha256"] == record["received_body_sha256"] {
		t.Fatal("changed replay audit recorded identical body hashes")
	}
	for _, sentinel := range []string{"original private sentinel", "changed private sentinel"} {
		if strings.Contains(output.String(), sentinel) {
			t.Fatalf("transaction conflict audit retained private content %q", sentinel)
		}
	}
}

func transactionEvent(eventID, sender, body string) map[string]any {
	return map[string]any{
		"event_id": eventID, "room_id": "!room:" + ownServer,
		"sender": sender, "type": "m.room.message", "origin_server_ts": int64(1),
		"content": map[string]any{"msgtype": "m.text", "body": body},
	}
}

func transactionBody(t *testing.T, events ...map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"events": events})
	if err != nil {
		t.Fatalf("marshal transaction: %v", err)
	}
	return body
}
