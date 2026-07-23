package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
)

// errResultSentinel is an opaque failure used to force A2A error and task-timeout branches.
var errResultSentinel = errors.New("result-metadata sentinel failure")

// resultBlock extracts the ai.fgentic.a2a block from a raw wire event, failing when it is absent.
func resultBlock(t *testing.T, raw map[string]any) map[string]any {
	t.Helper()
	block, ok := raw[resultMetadataKey].(map[string]any)
	if !ok {
		t.Fatalf("event %#v carries no %s block", raw, resultMetadataKey)
	}
	return block
}

// assertWellFormedBlock checks the block is the exact, content-free schema: only the five stable
// fields, the pinned version, the full ghost MXID, and the audit-stream budget-evidence pointer.
func assertWellFormedBlock(t *testing.T, block map[string]any, wantOutcome, wantTaskID string) {
	t.Helper()
	if v, ok := block["v"].(float64); !ok || int(v) != resultMetadataVersion {
		t.Fatalf("block version = %v, want %d", block["v"], resultMetadataVersion)
	}
	if got := block["outcome"]; got != wantOutcome {
		t.Fatalf("block outcome = %v, want %q", got, wantOutcome)
	}
	if got := block["agent"]; got != "@agent-k8s:"+ownServer {
		t.Fatalf("block agent = %v, want full ghost MXID @agent-k8s:%s", got, ownServer)
	}
	if got := block["budget_evidence"]; got != delegationAuditSchema {
		t.Fatalf("block budget_evidence = %v, want audit-stream pointer %q", got, delegationAuditSchema)
	}
	// task_id is omitempty: present iff a task existed.
	if wantTaskID == "" {
		if _, present := block["task_id"]; present {
			t.Fatalf("block carries task_id %v, want it omitted for a pre-A2A refusal", block["task_id"])
		}
	} else if got := block["task_id"]; got != wantTaskID {
		t.Fatalf("block task_id = %v, want %q", got, wantTaskID)
	}
	// Content-freeness: only the fixed field set, so no room/reply text can ride along.
	allowed := map[string]struct{}{"v": {}, "outcome": {}, "agent": {}, "task_id": {}, "budget_evidence": {}}
	for key := range block {
		if _, ok := allowed[key]; !ok {
			t.Fatalf("block carries unexpected field %q (%#v); the schema must stay content-free", key, block)
		}
	}
}

func TestTerminalSuccessReplyCarriesResultMetadata(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "the pod is healthy", TaskID: "task-42", Terminal: true}}
	b, _, evt, ref, recorder := pollingHarness(t, client)

	b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "inspect the pod",
		b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("Matrix events = %d, want one terminal reply", len(events))
	}
	// D8: the reply stays a plain m.notice; the block is additive metadata, not a new message type.
	if events[0].MsgType != event.MsgNotice {
		t.Fatalf("terminal reply msgtype = %q, want m.notice unchanged", events[0].MsgType)
	}
	if events[0].Body != "the pod is healthy" {
		t.Fatalf("terminal reply body = %q, want the agent answer unchanged", events[0].Body)
	}
	raw := recorder.rawSnapshot(t)
	if raw[0][automatedMixinKey] != true {
		t.Fatalf("terminal reply lost the automated mixin: %#v", raw[0])
	}
	assertWellFormedBlock(t, resultBlock(t, raw[0]), outcomeOK, "task-42")
	// The block must not smuggle the reply text (content-freeness on the wire).
	blockJSON, _ := json.Marshal(raw[0][resultMetadataKey])
	if strings.Contains(string(blockJSON), "pod is healthy") {
		t.Fatalf("block leaked reply text: %s", blockJSON)
	}
}

func TestTerminalFailureRepliesCarryMatchingOutcome(t *testing.T) {
	tests := []struct {
		name        string
		client      *scriptedA2AClient
		wantOutcome string
		wantTaskID  string
	}{
		{
			name:        "agent reported failure",
			client:      &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-fail", Terminal: true, Failed: true}},
			wantOutcome: outcomeFailed,
			wantTaskID:  "task-fail",
		},
		{
			name:        "empty terminal reply",
			client:      &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-empty", Terminal: true}},
			wantOutcome: outcomeFailed,
			wantTaskID:  "task-empty",
		},
		{
			name:        "a2a transport error before any task",
			client:      &scriptedA2AClient{callErr: errResultSentinel},
			wantOutcome: outcomeError,
			wantTaskID:  "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, _, evt, ref, recorder := pollingHarness(t, tc.client)

			b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "inspect the pod",
				b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

			events := recorder.snapshot()
			if len(events) != 1 {
				t.Fatalf("Matrix events = %d, want one terminal failure notice", len(events))
			}
			if events[0].MsgType != event.MsgNotice {
				t.Fatalf("failure notice msgtype = %q, want m.notice", events[0].MsgType)
			}
			raw := recorder.rawSnapshot(t)
			assertWellFormedBlock(t, resultBlock(t, raw[0]), tc.wantOutcome, tc.wantTaskID)
		})
	}
}

// TestNonTerminalNoticeCarriesNoResultMetadata drives a long task to timeout: the working
// placeholder (a non-terminal notice) must carry NO result block, while the terminal timeout edit
// that replaces it MUST — the block describes only a terminal outcome.
func TestNonTerminalNoticeCarriesNoResultMetadata(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-long", Terminal: false}}
	b, _, evt, ref, recorder := pollingHarness(t, client)
	b.pollWait = func(context.Context, time.Duration) error { return errResultSentinel } // force the task-timeout branch

	b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "inspect the pod",
		b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

	raw := recorder.rawSnapshot(t)
	if len(raw) != 2 {
		t.Fatalf("Matrix events = %d, want placeholder + terminal edit", len(raw))
	}
	if _, present := raw[0][resultMetadataKey]; present {
		t.Fatalf("non-terminal working placeholder carries a result block: %#v", raw[0])
	}
	// The terminal edit (m.new_content) still carries the block with the timeout outcome.
	assertWellFormedBlock(t, resultBlock(t, raw[1]), outcomeTimeout, "task-long")
}

// TestResultMetadataDoesNotMakeNoticeActionable is the D8 guard: a ghost-authored m.notice carrying
// the ai.fgentic.a2a block (even one mentioning another agent) must never be treated as a delegation.
// The block is machine-readable metadata, not an instruction, so bridge intake keeps ignoring it.
func TestResultMetadataDoesNotMakeNoticeActionable(t *testing.T) {
	tests := []struct {
		name   string
		sender id.UserID
	}{
		{name: "own ghost notice", sender: id.NewUserID("agent-k8s", ownServer)},
		{name: "other user notice", sender: id.NewUserID("mallory", ownServer)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
			b, _, _, _, recorder := pollingHarness(t, client)
			block := b.newResultMetadata("agent-k8s", outcomeOK, "task-42")

			notice := &event.MessageEventContent{
				MsgType:  event.MsgNotice,
				Body:     "done @agent-slack take it from here",
				Mentions: &event.Mentions{UserIDs: []id.UserID{id.NewUserID("agent-slack", ownServer)}},
			}
			evt := &event.Event{
				Sender:  tc.sender,
				RoomID:  id.RoomID("!room:" + ownServer),
				Type:    event.EventMessage,
				ID:      "$agent-notice",
				Content: event.Content{Parsed: notice, Raw: map[string]any{resultMetadataKey: block}},
			}

			b.HandleMessage(t.Context(), evt)

			if client.callCount != 0 {
				t.Fatalf("bridge delegated from an m.notice carrying a result block: callCount = %d (D8 violated)", client.callCount)
			}
			if got := recorder.snapshot(); len(got) != 0 {
				t.Fatalf("bridge emitted %d events for a non-delegating notice: %#v", len(got), got)
			}
		})
	}
}

func TestResultMetadataIsContentFree(t *testing.T) {
	const roomText = "SECRET incident detail from the room"
	b := testBridge(t)
	meta := b.newResultMetadata("agent-k8s", outcomeOK, "task-42")

	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatalf("marshal block: %v", err)
	}
	if strings.Contains(string(encoded), roomText) || strings.Contains(string(encoded), "incident") {
		t.Fatalf("block must never carry room content: %s", encoded)
	}

	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal block: %v", err)
	}
	want := map[string]struct{}{"v": {}, "outcome": {}, "agent": {}, "task_id": {}, "budget_evidence": {}}
	if len(fields) != len(want) {
		t.Fatalf("block fields = %v, want exactly %d content-free fields", fields, len(want))
	}
	for key := range fields {
		if _, ok := want[key]; !ok {
			t.Fatalf("unexpected block field %q; the schema must stay a fixed, content-free set", key)
		}
	}
	if fields["agent"] != "@agent-k8s:"+ownServer {
		t.Fatalf("agent identity = %v, want full federation-safe MXID", fields["agent"])
	}
}

// --- Durable (production) path: EnableDurableIntake makes this the live terminal path, so the block
// must ride the durable projection, not only the legacy in-memory one. ---

func TestDurableTerminalSuccessCarriesResultMetadata(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text: "durable answer", TaskID: "task-durable", ContextID: "ctx-after", Terminal: true,
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-block-ok")

	b.executeDurableJob(t.Context(), job)

	stored := loadDurableJob(t, b, job.JobID)
	raw := recorder.rawSnapshot(t)
	if len(raw) != 1 {
		t.Fatalf("durable Matrix events = %d, want one terminal reply", len(raw))
	}
	if raw[0][automatedMixinKey] != true {
		t.Fatalf("durable reply lost the automated mixin: %#v", raw[0])
	}
	// The block reports the durable job's already-persisted outcome and A2A task id (not recomputed).
	assertWellFormedBlock(t, resultBlock(t, raw[0]), outcomeOK, stored.A2ATaskID)
	if stored.A2ATaskID != "task-durable" {
		t.Fatalf("persisted A2A task id = %q, want task-durable", stored.A2ATaskID)
	}
	blockJSON, _ := json.Marshal(raw[0][resultMetadataKey])
	if strings.Contains(string(blockJSON), "durable answer") {
		t.Fatalf("durable block leaked reply text: %s", blockJSON)
	}
}

func TestDurableTerminalFailureCarriesResultMetadata(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		TaskID: "task-durable-fail", Terminal: true, Failed: true,
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-block-fail")

	b.executeDurableJob(t.Context(), job)

	stored := loadDurableJob(t, b, job.JobID)
	raw := recorder.rawSnapshot(t)
	if len(raw) != 1 {
		t.Fatalf("durable Matrix events = %d, want one terminal failure notice", len(raw))
	}
	// block.task_id must equal the persisted A2ATaskID the projection actually reads — never invented.
	assertWellFormedBlock(t, resultBlock(t, raw[0]), outcomeFailed, stored.A2ATaskID)
}

// TestDurableNonTerminalPlaceholderCarriesNoResultMetadata drives a durable LONG task: its first
// result is non-terminal, so the durable working placeholder (and any progress) is projected without
// a result block. The block describes only a terminal outcome.
func TestDurableNonTerminalPlaceholderCarriesNoResultMetadata(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{TaskID: "task-durable-long", Terminal: false}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	job := admitAndClaimDurableJob(t, b, "$durable-placeholder")

	b.executeDurableJob(t.Context(), job)

	raw := recorder.rawSnapshot(t)
	if len(raw) == 0 {
		t.Fatal("durable long task projected no working placeholder")
	}
	for i, ev := range raw {
		if _, present := ev[resultMetadataKey]; present {
			t.Fatalf("non-terminal durable projection %d carries a result block: %#v", i, ev)
		}
	}
	// The placeholder is the ordinary working notice, unchanged.
	if raw[0]["body"] != workingText {
		t.Fatalf("durable placeholder body = %v, want %q", raw[0]["body"], workingText)
	}
}
