package bridge

import (
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/replyscan"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

// awsExampleKey is AWS's own documentation example access-key id — a structurally valid but non-live
// credential. It is assembled from fragments so no full token literal appears in the committed
// source and neither gitleaks nor GitHub push protection flags a secret-scanner's own fixture.
const awsExampleKey = "AKIA" + "IOSFODNN7EXAMPLE"

// admitAndClaimDurableJobFor admits and claims a durable job for a specific sender, mention body, and
// event id so a test can exercise the local, bridged, or remote reply-scan posture.
func admitAndClaimDurableJobFor(t *testing.T, b *Bridge, eventID, sender, body string) state.Job {
	t.Helper()
	txn := transactionBody(t, transactionEvent(eventID, sender, body))
	result, err := b.AdmitAppserviceTransaction(t.Context(), "txn-"+eventID, txn)
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

func TestReplyScanBlockWithholdsSecretReply(t *testing.T) {
	secret := "your key is " + awsExampleKey + " keep it safe"
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: secret, Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ReplyScanMode = "block"
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	job := admitAndClaimDurableJob(t, b, "$secret-block")
	b.executeDurableJob(t.Context(), job)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDenied {
		t.Fatalf("state = %q, want denied", stored.State)
	}
	if stored.ResultText != "" || len(stored.Payload) != 0 {
		t.Fatalf("withheld reply retained content: result=%q payload=%d", stored.ResultText, len(stored.Payload))
	}
	events := recorder.snapshot()
	if len(events) != 1 || !strings.Contains(events[0].Body, "reply withheld") {
		t.Fatalf("posted events = %+v, want one withheld notice", events)
	}
	assertNoSecretLeak(t, recorder, output.String())

	audit := singleAudit(t, output.String())
	if audit["outcome"] != outcomeDenied || audit["terminal_reason"] != errorSecretInReply {
		t.Fatalf("audit outcome/reason = %v/%v, want denied/secret_in_reply",
			audit["outcome"], audit["terminal_reason"])
	}
	if audit["secret_scan_action"] != "block" || audit["secret_match_count"] != float64(1) {
		t.Fatalf("audit scan fields = %v/%v, want block/1", audit["secret_scan_action"], audit["secret_match_count"])
	}
	if audit["a2a_attempted"] != true {
		t.Fatal("block audit must record a2a_attempted=true (the reply scan runs after A2A)")
	}
	if client.callCount != 1 {
		t.Fatalf("A2A calls = %d, want 1", client.callCount)
	}
}

func TestReplyScanRedactMasksSecretInDataBlock(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text:     "here is the config",
		Data:     []string{`{"aws_key":"` + awsExampleKey + `"}`},
		Terminal: true,
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ReplyScanMode = "redact"
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	job := admitAndClaimDurableJob(t, b, "$secret-redact")
	b.executeDurableJob(t.Context(), job)

	stored := loadDurableJob(t, b, job.JobID)
	if stored.State != state.StateDelivered {
		t.Fatalf("state = %q, want delivered", stored.State)
	}
	events := recorder.snapshot()
	if len(events) != 1 || !strings.Contains(events[0].Body, "here is the config") {
		t.Fatalf("posted events = %+v, want one masked reply", events)
	}
	if !strings.Contains(events[0].Body, "‹redacted:aws-access-key-id›") {
		t.Fatalf("redacted reply missing placeholder: %q", events[0].Body)
	}
	assertNoSecretLeak(t, recorder, output.String())

	audit := singleAudit(t, output.String())
	if audit["outcome"] != outcomeOK || audit["secret_scan_action"] != "redact" {
		t.Fatalf("audit outcome/action = %v/%v, want ok/redact", audit["outcome"], audit["secret_scan_action"])
	}
	if audit["secret_rule_classes"] != "aws-access-key-id" {
		t.Fatalf("audit rule classes = %v, want aws-access-key-id", audit["secret_rule_classes"])
	}
}

func TestReplyScanAnnotateMasksAndAppendsNotice(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text: "the token is " + awsExampleKey, Terminal: true,
	}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ReplyScanMode = "annotate"
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	job := admitAndClaimDurableJob(t, b, "$secret-annotate")
	b.executeDurableJob(t.Context(), job)

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("posted events = %+v, want one", events)
	}
	if !strings.Contains(events[0].Body, "‹redacted:aws-access-key-id›") ||
		!strings.Contains(events[0].Body, "possible credential(s) detected and masked") {
		t.Fatalf("annotate reply missing mask or notice: %q", events[0].Body)
	}
	assertNoSecretLeak(t, recorder, output.String())

	audit := singleAudit(t, output.String())
	if audit["outcome"] != outcomeOK || audit["secret_scan_action"] != "annotate" {
		t.Fatalf("audit outcome/action = %v/%v, want ok/annotate", audit["outcome"], audit["secret_scan_action"])
	}
}

func TestReplyScanCleanReplyDeliveredUnchanged(t *testing.T) {
	clean := "The pod is healthy. Restart it with kubectl rollout restart."
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: clean, Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ReplyScanMode = "block" // strictest base; a clean reply must still pass through untouched
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	job := admitAndClaimDurableJob(t, b, "$clean")
	b.executeDurableJob(t.Context(), job)

	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != clean {
		t.Fatalf("clean reply altered: %+v", events)
	}
	audit := singleAudit(t, output.String())
	if audit["outcome"] != outcomeOK || audit["secret_scan_action"] != "" || audit["secret_match_count"] != float64(0) {
		t.Fatalf("clean audit scan fields not empty: %v/%v", audit["secret_scan_action"], audit["secret_match_count"])
	}
}

func TestReplyScanDisabledDeliversReplyUnchanged(t *testing.T) {
	secret := "key " + awsExampleKey
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: secret, Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ReplyScanMode = "off"
	b.cfg.ReplyScanFederatedMode = "off"

	job := admitAndClaimDurableJob(t, b, "$scan-off")
	b.executeDurableJob(t.Context(), job)

	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != secret {
		t.Fatalf("disabled scan altered the reply: %+v", events)
	}
}

func TestReplyScanFederationExposureEscalatesToBlock(t *testing.T) {
	// A bridged (federation-exposed) sender must take the stricter federated posture even though the
	// same-org base posture is only annotate.
	secret := "leaked " + awsExampleKey
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: secret, Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	configureDurableTestBridge(b)
	b.cfg.ReplyScanMode = "annotate"
	b.cfg.ReplyScanFederatedMode = "block"
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	// Pre-register the agent-slack ghost the same way pollingHarness does for agent-k8s so the
	// terminal notice projects without an unmocked registration/join round trip.
	slackIntent := b.as.Intent(id.NewUserID("agent-slack", ownServer))
	slackIntent.Registered = true
	if err := b.as.StateStore.SetMembership(
		t.Context(), id.RoomID("!room:"+ownServer), slackIntent.UserID, event.MembershipJoin,
	); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	job := admitAndClaimDurableJobFor(t, b, "$fed-block", "@slack_U1:"+ownServer, "@agent-slack inspect")
	b.executeDurableJob(t.Context(), job)

	events := recorder.snapshot()
	if len(events) != 1 || !strings.Contains(events[0].Body, "reply withheld") {
		t.Fatalf("federation-exposed reply not withheld: %+v", events)
	}
	assertNoSecretLeak(t, recorder, output.String())
	audit := singleAudit(t, output.String())
	if audit["terminal_reason"] != errorSecretInReply || audit["secret_scan_action"] != "block" {
		t.Fatalf("federated audit = %v/%v, want secret_in_reply/block",
			audit["terminal_reason"], audit["secret_scan_action"])
	}
}

func TestReplyScanModeSelection(t *testing.T) {
	b := testBridge(t)
	b.cfg.ReplyScanMode = "annotate"
	b.cfg.ReplyScanFederatedMode = "block"
	localRef, _ := b.agents.Lookup("agent-k8s")

	cases := []struct {
		name   string
		sender senderIdentity
		ref    *AgentRef
		want   replyscan.Mode
	}{
		{"local matrix sender, local target", matrixSender(id.NewUserID("alice", ownServer)), localRef, replyscan.ModeAnnotate},
		{"remote homeserver sender", matrixSender(id.NewUserID("alice", "partner.example")), localRef, replyscan.ModeBlock},
		{"bridged sender", b.agents.IdentifySender(id.NewUserID("slack_U1", ownServer)), localRef, replyscan.ModeBlock},
		{"nil ref local sender", matrixSender(id.NewUserID("alice", ownServer)), nil, replyscan.ModeAnnotate},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.replyScanMode(tc.sender, tc.ref); got != tc.want {
				t.Fatalf("replyScanMode = %v, want %v", got, tc.want)
			}
		})
	}
}

// assertNoSecretLeak fails if the example credential appears in any posted Matrix event or any
// log/audit line — the core invariant of #343: the matched value never enters the room or the log.
func assertNoSecretLeak(t *testing.T, recorder *matrixRecorder, logOutput string) {
	t.Helper()
	for _, evt := range recorder.snapshot() {
		if strings.Contains(evt.Body, awsExampleKey) {
			t.Fatalf("secret leaked into a posted Matrix event: %q", evt.Body)
		}
	}
	if strings.Contains(logOutput, awsExampleKey) {
		t.Fatal("secret leaked into the log/audit stream")
	}
}

func singleAudit(t *testing.T, output string) map[string]any {
	t.Helper()
	records := auditRecords(t, output)
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(records))
	}
	return records[0]
}
