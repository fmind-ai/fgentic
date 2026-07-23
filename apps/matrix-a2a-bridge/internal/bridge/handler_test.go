package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	ownServer         = "fgentic.fmind.ai"
	wireContractAgent = "/api/a2a/kagent/k8s-agent"
)

type wireExecutorFunc func(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error]

func (fn wireExecutorFunc) Execute(ctx context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return fn(ctx, execCtx)
}

func (wireExecutorFunc) Cancel(context.Context, *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
	return func(func(a2a.Event, error) bool) {}
}

type transientGetStore struct {
	taskstore.Store
	mu             sync.Mutex
	getFailures    int
	terminalUpdate chan struct{}
	terminalOnce   sync.Once
}

type markErrorStore struct {
	state.Store
}

func (*markErrorStore) MarkEventProcessed(context.Context, string) (bool, error) {
	return false, errors.New("scripted dedup store failure")
}

func (s *transientGetStore) failNextGets(count int) {
	s.mu.Lock()
	s.getFailures = count
	s.mu.Unlock()
}

func (s *transientGetStore) Get(ctx context.Context, taskID a2a.TaskID) (*taskstore.StoredTask, error) {
	s.mu.Lock()
	if s.getFailures > 0 {
		s.getFailures--
		s.mu.Unlock()
		return nil, errors.New("scripted transient GetTask failure")
	}
	s.mu.Unlock()
	return s.Store.Get(ctx, taskID)
}

func (s *transientGetStore) Update(ctx context.Context, update *taskstore.UpdateRequest) (taskstore.TaskVersion, error) {
	version, err := s.Store.Update(ctx, update)
	if err == nil && update.Task.Status.State.Terminal() && s.terminalUpdate != nil {
		s.terminalOnce.Do(func() { close(s.terminalUpdate) })
	}
	return version, err
}

func newWireA2AClient(t *testing.T, executor a2asrv.AgentExecutor, store taskstore.Store) *a2aclient.Client {
	t.Helper()

	handler := a2asrv.NewJSONRPCHandler(a2asrv.NewHandler(executor, a2asrv.WithTaskStore(store)))
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	card := &a2a.AgentCard{
		Name:                "Bridge wire fixture",
		Description:         "In-process A2A bridge contract fixture",
		Version:             "test",
		SupportedInterfaces: []*a2a.AgentInterface{a2a.NewAgentInterface(server.URL+wireContractAgent, a2a.TransportProtocolJSONRPC)},
		DefaultInputModes:   []string{"text/plain"},
		DefaultOutputModes:  []string{"text/plain"},
		Capabilities:        a2a.AgentCapabilities{},
		Skills:              []a2a.AgentSkill{},
	}
	mux.Handle(wireContractAgent+a2asrv.WellKnownAgentCardPath, a2asrv.NewStaticAgentCardHandler(card))
	mux.Handle(wireContractAgent, handler)

	return a2aclient.New(server.URL, "", slog.Default())
}

func testBridge(t *testing.T) *Bridge {
	t.Helper()
	agents, err := LoadAgents(writeTemp(t, `bridgedOrigins:
  slack: ["@slack_*:fgentic.fmind.ai"]
agents:
  agent-k8s: {namespace: kagent, name: k8s-agent, allowedRooms: ["!room:fgentic.fmind.ai"]}
  agent-locked:
    namespace: kagent
    name: locked-agent
    allowedRooms: ["!room:fgentic.fmind.ai"]
    allowedSenders: ["@admin:fgentic.fmind.ai"]
  agent-slack:
    namespace: kagent
    name: slack-agent
    allowedRooms: ["!room:fgentic.fmind.ai"]
    allowedSenders: ["@slack_*:fgentic.fmind.ai"]
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", AccessManagerMXID: "@alice:" + ownServer, Concurrency: 1,
		RoomQueueCapacity: 32, GlobalQueueCapacity: 256,
		SenderRatePerMinute: 60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
		RateLimitBucketCapacity:  testRateLimitBucketCapacity,
		RequestTimeout:           time.Second,
		AgentsReloadInterval:     time.Hour,
		AgentCardRefreshInterval: time.Hour,
		InputWaitTimeout:         time.Minute,
	}
	stateStore := mautrix.NewMemoryStateStore().(appservice.StateStore)
	for _, ghost := range []string{"agent-k8s", "agent-locked", "agent-slack"} {
		if err := stateStore.SetMembership(
			t.Context(), "!room:"+ownServer, id.NewUserID(ghost, ownServer), event.MembershipJoin,
		); err != nil {
			t.Fatalf("seed %s membership: %v", ghost, err)
		}
	}
	as := &appservice.AppService{
		Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"},
		StateStore:   stateStore,
	}
	b := New(cfg, as, agents, nil, state.NewMemory(), slog.Default())
	b.runCtx = t.Context() // delegations run under the process context; canceled when the test ends
	return b
}

func joinGhostForTest(t *testing.T, b *Bridge, roomID id.RoomID, ghost string) {
	t.Helper()
	if err := b.as.StateStore.SetMembership(
		t.Context(), roomID, id.NewUserID(ghost, ownServer), event.MembershipJoin,
	); err != nil {
		t.Fatalf("seed %s membership: %v", ghost, err)
	}
}

func msgEvent(sender id.UserID, body string, mentions ...id.UserID) (*event.Event, *event.MessageEventContent) {
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	if len(mentions) > 0 {
		content.Mentions = &event.Mentions{UserIDs: mentions}
	}
	evt := &event.Event{Sender: sender, RoomID: "!room:fgentic.fmind.ai"}
	return evt, content
}

func TestResolveTargets(t *testing.T) {
	tests := []struct {
		name        string
		sender      id.UserID
		body        string
		mentions    []id.UserID
		wantAllowed []string
		wantDenied  []string
	}{
		{
			name:        "typed mention",
			sender:      id.NewUserID("alice", ownServer),
			body:        "please check the pods",
			mentions:    []id.UserID{id.NewUserID("agent-k8s", ownServer)},
			wantAllowed: []string{"agent-k8s"},
		},
		{
			name:     "typed foreign homeserver rejected",
			sender:   id.NewUserID("alice", ownServer),
			body:     "hi",
			mentions: []id.UserID{id.NewUserID("agent-k8s", "evil.example")},
		},
		{
			name:        "plaintext fallback",
			sender:      id.NewUserID("alice", ownServer),
			body:        "@agent-k8s why is pod X down?",
			wantAllowed: []string{"agent-k8s"},
		},
		{
			name:   "plaintext foreign homeserver rejected",
			sender: id.NewUserID("alice", ownServer),
			body:   "@agent-k8s:evil.example do things",
		},
		{
			name:     "sender policy denied",
			sender:   id.NewUserID("alice", ownServer),
			body:     "restricted",
			mentions: []id.UserID{id.NewUserID("agent-locked", ownServer)},
		},
		{
			name:        "sender policy allowed",
			sender:      id.NewUserID("admin", ownServer),
			body:        "restricted",
			mentions:    []id.UserID{id.NewUserID("agent-locked", ownServer)},
			wantAllowed: []string{"agent-locked"},
		},
		{
			name:        "typed and plaintext duplicates",
			sender:      id.NewUserID("alice", ownServer),
			body:        "@agent-k8s and again @agent-k8s",
			mentions:    []id.UserID{id.NewUserID("agent-k8s", ownServer)},
			wantAllowed: []string{"agent-k8s"},
		},
		{
			name:   "mixed homeservers retain only local mention",
			sender: id.NewUserID("alice", ownServer),
			body:   "hi",
			mentions: []id.UserID{
				id.NewUserID("agent-k8s", "evil.example"),
				id.NewUserID("agent-k8s", ownServer),
			},
			wantAllowed: []string{"agent-k8s"},
		},
		{
			name:     "unknown local agent rejected",
			sender:   id.NewUserID("alice", ownServer),
			body:     "hi",
			mentions: []id.UserID{id.NewUserID("agent-unknown", ownServer)},
		},
		{
			name:       "bridged sender denied without explicit allowlist",
			sender:     id.NewUserID("slack_U123", ownServer),
			body:       "restricted",
			mentions:   []id.UserID{id.NewUserID("agent-k8s", ownServer)},
			wantDenied: []string{"agent-k8s"},
		},
		{
			name:        "bridged sender allowed by explicit namespace",
			sender:      id.NewUserID("slack_U123", ownServer),
			body:        "allowed",
			mentions:    []id.UserID{id.NewUserID("agent-slack", ownServer)},
			wantAllowed: []string{"agent-slack"},
		},
		{
			name:     "foreign bridged lookalike uses federated policy",
			sender:   id.NewUserID("slack_U123", "partner.example"),
			body:     "foreign",
			mentions: []id.UserID{id.NewUserID("agent-k8s", ownServer)},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testBridge(t)
			evt, msg := msgEvent(tt.sender, tt.body, tt.mentions...)
			got := b.resolveTargets(evt, msg)
			if !slices.Equal(got.allowed, tt.wantAllowed) || !slices.Equal(got.deniedBridged, tt.wantDenied) {
				t.Errorf(
					"resolveTargets() = (allowed %v, denied %v), want (allowed %v, denied %v)",
					got.allowed,
					got.deniedBridged,
					tt.wantAllowed,
					tt.wantDenied,
				)
			}
		})
	}
}

func TestRemoteMappingNeverResolvesForeignHomeserverMention(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b := testBridge(t)
	b.agents = agents
	foreign := id.NewUserID("agent-remote", "partner.example")
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), foreign.String(), foreign)

	resolved := b.resolveTargets(evt, msg)
	if len(resolved.allowed) != 0 || len(resolved.deniedBridged) != 0 {
		t.Fatalf("foreign remote mention resolved locally: %+v", resolved)
	}
}

func TestStripMentions(t *testing.T) {
	b := testBridge(t)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{name: "bare mention", in: "@agent-k8s why is pod X down?", want: "why is pod X down?"},
		{name: "qualified mention", in: "@agent-k8s:fgentic.fmind.ai check this", want: "check this"},
		{name: "mention only", in: "@agent-k8s", want: "@agent-k8s"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := b.stripMentions(c.in); got != c.want {
				t.Errorf("stripMentions(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestProvenancePrompt(t *testing.T) {
	evt, _ := msgEvent(id.NewUserID("alice", ownServer), "ignored")
	evt.RoomID = "!operations:" + ownServer

	want := `--- BEGIN FGENTIC BRIDGE PROVENANCE ---
sender_mxid: "@alice:fgentic.fmind.ai"
sender_homeserver: "fgentic.fmind.ai"
room_id: "!operations:fgentic.fmind.ai"
--- END FGENTIC BRIDGE PROVENANCE ---
--- BEGIN UNTRUSTED MATRIX CONTENT ---
restart the failed pod
--- END UNTRUSTED MATRIX CONTENT ---`
	if got := provenancePrompt(evt, "restart the failed pod"); got != want {
		t.Fatalf("provenancePrompt() =\n%s\nwant:\n%s", got, want)
	}
}

func TestDelegationAuditRecordIsStableAndContentFree(t *testing.T) {
	var output strings.Builder
	b := testBridge(t)
	setBridgeLogOutput(b, &output)
	evt, _ := msgEvent(id.NewUserID("alice", ownServer), "ignored")
	evt.ID = "$request"
	evt.Timestamp = 1_720_000_000_123
	ref, ok := b.agents.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s fixture missing")
	}

	b.logDelegationAudit(evt, ref, "agent-k8s", b.agents.IdentifySender(evt.Sender), delegationAuditResult{
		outcome:          outcomeOK,
		terminalStage:    "task_result",
		terminalReason:   "completed",
		duration:         1500 * time.Millisecond,
		dedupVerdict:     dedupVerdictAccepted,
		rateLimitVerdict: rateLimitVerdictAllowed,
		a2aAttempted:     true,
		a2aUserID:        evt.Sender.String(),
		contextID:        "context-1",
		taskID:           "task-1",
		replyEventID:     "$reply",
		activated:        []string{"https://fgentic.fmind.ai/a2a/extensions/token-budget/v1", "https://partner.example/ext/v1"},
	})

	records := auditRecords(t, output.String())
	if len(records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(records))
	}
	record := records[0]
	want := map[string]any{
		"msg":                      "delegation audit",
		"log_stream":               delegationAuditStream,
		"audit_schema":             delegationAuditSchema,
		"sender_mxid":              "@alice:" + ownServer,
		"sender_homeserver":        ownServer,
		"sender_origin_kind":       string(senderOriginMatrix),
		"sender_origin_network":    matrixOriginNetwork,
		"matrix_event_id":          "$request",
		"matrix_origin_server_ts":  float64(evt.Timestamp),
		"room_id":                  evt.RoomID.String(),
		"reply_event_id":           "$reply",
		"ghost":                    "agent-k8s",
		"ghost_mxid":               "@agent-k8s:" + ownServer,
		"agent_path":               "/api/a2a/kagent/k8s-agent",
		"agent_version":            ref.AgentVersion(),
		"agent_contract_sha256":    "",
		"a2a_attempted":            true,
		"a2a_user_id":              "@alice:" + ownServer,
		"a2a_context_id":           "context-1",
		"a2a_task_id":              "task-1",
		"outcome":                  outcomeOK,
		"terminal_stage":           "task_result",
		"terminal_reason":          "completed",
		"a2a_activated_extensions": "https://fgentic.fmind.ai/a2a/extensions/token-budget/v1,https://partner.example/ext/v1",
		"duration_ms":              float64(1500),
		"dedup_verdict":            string(dedupVerdictAccepted),
		"rate_limit_verdict":       string(rateLimitVerdictAllowed),
		"secret_scan_action":       "",
		"secret_match_count":       float64(0),
		"secret_rule_classes":      "",
	}
	for key, value := range want {
		if got := record[key]; got != value {
			t.Errorf("audit field %q = %#v, want %#v", key, got, value)
		}
	}
	for _, forbidden := range []string{"content", "message", "prompt", "text"} {
		if _, ok := record[forbidden]; ok {
			t.Errorf("audit record contains forbidden content field %q", forbidden)
		}
	}
}

func TestBridgedSenderPolicyDenialPostsNoticeAndAudit(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.runCtx = t.Context()
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.roomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	evt.Sender = id.NewUserID("slack_U123", ownServer)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	deniedMetric := delegationsTotal.WithLabelValues("agent-k8s", outcomeDenied)
	deniedBefore := counterValue(t, deniedMetric)

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("denied bridged sender made %d A2A calls", client.callCount)
	}
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Error("policy denial consumed the authorized sender invocation budget")
	}
	if !b.roomLimits.Allow(evt.RoomID.String()) {
		t.Error("policy denial consumed the authorized room invocation budget")
	}
	if got := counterValue(t, deniedMetric); got != deniedBefore+1 {
		t.Errorf("denied delegation metric = %v, want %v", got, deniedBefore+1)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorSenderPolicy, "agent-k8s", 0) || events[0].MsgType != event.MsgNotice {
		t.Fatalf("denied bridged sender Matrix events = %#v, want one policy notice", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("denied bridged sender audit records = %d, want 1", len(audits))
	}
	audit := audits[0]
	for key, want := range map[string]any{
		"sender_origin_kind":    string(senderOriginBridge),
		"sender_origin_network": "slack",
		"outcome":               outcomeDenied,
		"terminal_stage":        "admission",
		"terminal_reason":       "sender_policy_rejected",
		"a2a_attempted":         false,
		"rate_limit_verdict":    string(rateLimitVerdictAllowed),
	} {
		if got := audit[key]; got != want {
			t.Errorf("denied audit field %q = %#v, want %#v", key, got, want)
		}
	}
}

func TestBridgedSenderPolicyDenialUsesSeparateBoundedNoticeLimits(t *testing.T) {
	tests := []struct {
		name    string
		exhaust func(t *testing.T, b *Bridge, sender senderIdentity, room id.RoomID)
	}{
		{
			name: "sender denial bucket",
			exhaust: func(t *testing.T, b *Bridge, sender senderIdentity, _ id.RoomID) {
				t.Helper()
				b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
				if !b.noticeSenderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
					t.Fatal("failed to consume sender denial limiter fixture token")
				}
			},
		},
		{
			name: "room denial bucket",
			exhaust: func(t *testing.T, b *Bridge, _ senderIdentity, room id.RoomID) {
				t.Helper()
				b.noticeRoomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
				if !b.noticeRoomLimits.Allow(room.String()) {
					t.Fatal("failed to consume room denial limiter fixture token")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedA2AClient{}
			b, _, evt, _, recorder := pollingHarness(t, client)
			b.runCtx = t.Context()
			evt.Sender = id.NewUserID("slack_U123", ownServer)
			_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
			evt.Content = event.Content{Parsed: msg}
			sender := b.agents.IdentifySender(evt.Sender)
			tt.exhaust(t, b, sender, evt.RoomID)
			var output strings.Builder
			setBridgeLogOutput(b, &output)
			rateLimitedMetric := delegationsTotal.WithLabelValues("agent-k8s", outcomeRateLimited)
			rateLimitedBefore := counterValue(t, rateLimitedMetric)

			b.HandleMessage(t.Context(), evt)
			b.dispatcher.Wait()

			if client.callCount != 0 {
				t.Fatalf("rate-limited denied sender made %d A2A calls", client.callCount)
			}
			if events := recorder.snapshot(); len(events) != 0 {
				t.Fatalf("exhausted denial bucket amplified Matrix replies: %#v", events)
			}
			if got := counterValue(t, rateLimitedMetric); got != rateLimitedBefore+1 {
				t.Errorf("rate-limited denied metric = %v, want %v", got, rateLimitedBefore+1)
			}
			audits := auditRecords(t, output.String())
			if len(audits) != 1 ||
				audits[0]["sender_origin_network"] != "slack" ||
				audits[0]["outcome"] != outcomeRateLimited ||
				audits[0]["terminal_reason"] != "denial_notice_rate_limit_rejected" ||
				audits[0]["rate_limit_verdict"] != string(rateLimitVerdictRejected) {
				t.Fatalf("rate-limited denied sender audit = %#v", audits)
			}
		})
	}
}

func TestExplicitlyAllowedBridgedSenderDelegatesWithOriginAudit(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "done", Terminal: true}}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.agents = loadSlackAllowedAgent(t)
	b.runCtx = t.Context()
	evt.Sender = id.NewUserID("slack_U123", ownServer)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 1 {
		t.Fatalf("allowed bridged sender A2A calls = %d, want 1", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != "done" {
		t.Fatalf("allowed bridged sender Matrix events = %#v, want one agent reply", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("allowed bridged sender audit records = %d, want 1", len(audits))
	}
	if audits[0]["sender_origin_kind"] != string(senderOriginBridge) ||
		audits[0]["sender_origin_network"] != "slack" ||
		audits[0]["outcome"] != outcomeOK {
		t.Fatalf("allowed bridged sender audit attribution = %#v", audits[0])
	}
}

func TestQueuedDelegationReauthorizesAfterAgentReload(t *testing.T) {
	tests := []struct {
		name           string
		reloadedConfig string
		wantReason     string
	}{
		{
			name: "sender grant removed",
			reloadedConfig: `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["!room:fgentic.fmind.ai"]
    allowedSenders: ["@admin:fgentic.fmind.ai"]
`,
			wantReason: "sender_policy_rejected",
		},
		{
			name: "agent target remapped",
			reloadedConfig: `agents:
  agent-k8s:
    namespace: kagent
    name: replacement-agent
    allowedRooms: ["!room:fgentic.fmind.ai"]
`,
			wantReason: "agent_mapping_changed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
			b, _, evt, _, recorder := pollingHarness(t, client)
			b.runCtx = t.Context()
			b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
			b.roomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
			_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
			evt.Content = event.Content{Parsed: msg}
			var output strings.Builder
			setBridgeLogOutput(b, &output)

			unblock := blockDispatcher(t, b, evt.RoomID)

			b.HandleMessage(t.Context(), evt)
			reloaded, err := LoadAgents(writeTemp(t, tt.reloadedConfig))
			if err != nil {
				t.Fatalf("LoadAgents reloaded policy: %v", err)
			}
			b.agents.Replace(reloaded)
			unblock()
			b.dispatcher.Wait()

			if client.callCount != 0 {
				t.Fatalf("stale queued target made %d A2A calls", client.callCount)
			}
			if events := recorder.snapshot(); len(events) != 0 {
				t.Fatalf("stale queued target emitted Matrix replies: %#v", events)
			}
			sender := b.agents.IdentifySender(evt.Sender)
			if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
				t.Error("stale queued target consumed the sender invocation budget")
			}
			if !b.roomLimits.Allow(evt.RoomID.String()) {
				t.Error("stale queued target consumed the room invocation budget")
			}
			audits := auditRecords(t, output.String())
			if len(audits) != 1 ||
				audits[0]["outcome"] != outcomeDenied ||
				audits[0]["terminal_reason"] != tt.wantReason ||
				audits[0]["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) ||
				audits[0]["a2a_attempted"] != false {
				t.Fatalf("stale queued target audit = %#v", audits)
			}
		})
	}
}

func TestQueuedBridgedSenderCannotBeDowngradedByOriginReload(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.agents = loadSlackAllowedAgent(t)
	b.runCtx = t.Context()
	evt.Sender = id.NewUserID("slack_U123", ownServer)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	unblock := blockDispatcher(t, b, evt.RoomID)

	b.HandleMessage(t.Context(), evt)
	reloaded, err := LoadAgents(writeTemp(t, `agents:
  agent-k8s: {namespace: kagent, name: k8s-agent, allowedRooms: ["!room:fgentic.fmind.ai"]}
`))
	if err != nil {
		t.Fatalf("LoadAgents reloaded origin policy: %v", err)
	}
	b.agents.Replace(reloaded)
	unblock()
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("downgraded queued bridge origin made %d A2A calls", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorSenderPolicy, "agent-k8s", 0) {
		t.Fatalf("downgraded queued bridge origin Matrix events = %#v, want policy notice", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 ||
		audits[0]["sender_origin_kind"] != string(senderOriginBridge) ||
		audits[0]["sender_origin_network"] != "slack" ||
		audits[0]["outcome"] != outcomeDenied ||
		audits[0]["terminal_reason"] != "sender_policy_rejected" ||
		audits[0]["a2a_attempted"] != false {
		t.Fatalf("downgraded queued bridge origin audit = %#v", audits)
	}
}

func TestQueuedBridgedSenderKeepsOriginAcrossNetworkRelabel(t *testing.T) {
	load := func(t *testing.T, network string) *AgentMap {
		t.Helper()
		agents, err := LoadAgents(writeTemp(t, fmt.Sprintf(`bridgedOrigins:
  %s: ["@slack_*:fgentic.fmind.ai"]
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["!room:fgentic.fmind.ai"]
    allowedSenders: ["@slack_*:fgentic.fmind.ai"]
`, network)))
		if err != nil {
			t.Fatalf("LoadAgents %s origin: %v", network, err)
		}
		return agents
	}

	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "done", Terminal: true}}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.agents = load(t, "slack")
	b.runCtx = t.Context()
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	evt.Sender = id.NewUserID("slack_U123", ownServer)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	unblock := blockDispatcher(t, b, evt.RoomID)

	b.HandleMessage(t.Context(), evt)
	b.agents.Replace(load(t, "telegram"))
	current := b.agents.IdentifySender(evt.Sender)
	if current.origin.network != "telegram" {
		t.Fatalf("current origin network = %q, want telegram fixture", current.origin.network)
	}
	if !b.senderLimits.Allow(current.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to consume relabeled telegram limiter fixture token")
	}
	unblock()
	b.dispatcher.Wait()

	if client.callCount != 1 {
		t.Fatalf("origin-relabelled queued sender A2A calls = %d, want 1 using bound Slack attribution", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != "done" {
		t.Fatalf("origin-relabelled queued sender Matrix events = %#v", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 ||
		audits[0]["sender_origin_kind"] != string(senderOriginBridge) ||
		audits[0]["sender_origin_network"] != "slack" ||
		audits[0]["outcome"] != outcomeOK {
		t.Fatalf("origin-relabelled queued sender audit = %#v", audits)
	}
}

func blockDispatcher(t *testing.T, b *Bridge, roomID id.RoomID) func() {
	t.Helper()
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		unblock()
		b.dispatcher.Wait()
	})
	if got := b.dispatcher.Enqueue(t.Context(), roomID, func(context.Context) {
		close(started)
		<-release
	}, nil); got != enqueueAccepted {
		t.Fatalf("enqueue dispatcher barrier = %v, want accepted", got)
	}
	<-started
	return unblock
}

func TestBridgedSenderDispatchUsesOriginAwareRateLimitKey(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.agents = loadSlackAllowedAgent(t)
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	evt.Sender = id.NewUserID("slack_U123", ownServer)
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to consume bridged sender limiter fixture token")
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	rateLimitedMetric := delegationsTotal.WithLabelValues("agent-k8s", outcomeRateLimited)
	rateLimitedBefore := counterValue(t, rateLimitedMetric)
	ref, ok := b.agents.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s fixture missing")
	}

	b.dispatchWithDedupVerdict(
		t.Context(),
		evt,
		ref,
		"agent-k8s",
		"inspect the pod",
		b.agents.IdentifySender(evt.Sender),
		dedupVerdictAccepted,
	)

	if client.callCount != 0 {
		t.Fatalf("rate-limited bridged dispatch made %d A2A calls", client.callCount)
	}
	if got := counterValue(t, rateLimitedMetric); got != rateLimitedBefore+1 {
		t.Errorf("rate-limited delegation metric = %v, want %v", got, rateLimitedBefore+1)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorRateLimit, "", 0) {
		t.Fatalf("rate-limited bridged dispatch Matrix events = %#v", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 ||
		audits[0]["sender_origin_network"] != "slack" ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictRejected) {
		t.Fatalf("rate-limited bridged dispatch audit = %#v", audits)
	}
}

func TestAllowedBridgedRateLimitNoticesAreBounded(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.agents = loadSlackAllowedAgent(t)
	b.runCtx = t.Context()
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeRoomLimits = newLimiters(60, 10, testRateLimitBucketCapacity)
	evt.Sender = id.NewUserID("slack_U123", ownServer)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Fatal("failed to consume bridged invocation limiter fixture token")
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	for i := range 10 {
		evt.ID = id.EventID(fmt.Sprintf("$allowed-rate-%d", i))
		b.HandleMessage(t.Context(), evt)
		b.dispatcher.Wait()
	}

	if client.callCount != 0 {
		t.Fatalf("rate-limited bridged flood made %d A2A calls", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorRateLimit, "", 0) {
		t.Fatalf("rate-limited bridged flood events = %#v, want one bounded notice", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 10 {
		t.Fatalf("rate-limited bridged flood audits = %d, want 10", len(audits))
	}
	for i, audit := range audits {
		if audit["outcome"] != outcomeRateLimited || audit["sender_origin_network"] != "slack" {
			t.Errorf("rate-limited bridged flood audit %d = %#v", i, audit)
		}
		if i > 0 && audit["reply_event_id"] != "" {
			t.Errorf("suppressed rate-limit audit %d has reply event %q", i, audit["reply_event_id"])
		}
	}
}

func TestDispatcherOverflowFailsClosedBeforeAdmission(t *testing.T) {
	tests := []struct {
		name           string
		roomCapacity   int
		globalCapacity int
		blockRoom      func(id.RoomID) id.RoomID
		wantReason     string
	}{
		{
			name:           "per-room capacity",
			roomCapacity:   1,
			globalCapacity: 10,
			blockRoom:      func(room id.RoomID) id.RoomID { return room },
			wantReason:     "queue_room_capacity_rejected",
		},
		{
			name:           "global capacity",
			roomCapacity:   10,
			globalCapacity: 1,
			blockRoom:      func(id.RoomID) id.RoomID { return "!queue-block:" + ownServer },
			wantReason:     "queue_global_capacity_rejected",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
			b, _, evt, _, recorder := pollingHarness(t, client)
			b.runCtx = t.Context()
			b.dispatcher = newDispatcher(1, tt.roomCapacity, tt.globalCapacity)
			b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
			b.roomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
			_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
			evt.Content = event.Content{Parsed: msg}
			var output strings.Builder
			setBridgeLogOutput(b, &output)
			queueFullMetric := delegationsTotal.WithLabelValues("agent-k8s", outcomeQueueFull)
			queueFullBefore := counterValue(t, queueFullMetric)
			unblock := blockDispatcher(t, b, tt.blockRoom(evt.RoomID))

			b.HandleMessage(t.Context(), evt)

			if client.callCount != 0 {
				t.Fatalf("queue overflow made %d A2A calls", client.callCount)
			}
			if events := recorder.snapshot(); len(events) != 1 ||
				events[0].Body != failureMessage(tt.wantReason, "agent-k8s", 0) {
				t.Fatalf("queue overflow Matrix replies = %#v, want one bounded failure notice", events)
			}
			if got := counterValue(t, queueFullMetric); got != queueFullBefore+1 {
				t.Errorf("queue overflow metric = %v, want %v", got, queueFullBefore+1)
			}
			sender := b.agents.IdentifySender(evt.Sender)
			if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
				t.Error("queue overflow consumed the sender invocation budget")
			}
			if !b.roomLimits.Allow(evt.RoomID.String()) {
				t.Error("queue overflow consumed the room invocation budget")
			}
			audits := auditRecords(t, output.String())
			if len(audits) != 1 ||
				audits[0]["outcome"] != outcomeQueueFull ||
				audits[0]["terminal_stage"] != "queue" ||
				audits[0]["terminal_reason"] != tt.wantReason ||
				audits[0]["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) ||
				audits[0]["a2a_attempted"] != false {
				t.Fatalf("queue overflow audit = %#v", audits)
			}
			unblock()
			b.dispatcher.Wait()
		})
	}
}

func TestDispatcherShutdownRaceEmitsTerminalAudit(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, evt, _, recorder := pollingHarness(t, client)
	shutdownCtx, cancel := context.WithCancel(t.Context())
	cancel()
	b.runCtx = shutdownCtx
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.roomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	shutdownMetric := delegationsTotal.WithLabelValues("agent-k8s", outcomeShutdown)
	shutdownBefore := counterValue(t, shutdownMetric)

	b.HandleMessage(t.Context(), evt)

	if client.callCount != 0 {
		t.Fatalf("shutdown-raced event made %d A2A calls", client.callCount)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("shutdown-raced event emitted Matrix replies: %#v", events)
	}
	if got := counterValue(t, shutdownMetric); got != shutdownBefore+1 {
		t.Errorf("shutdown rejection metric = %v, want %v", got, shutdownBefore+1)
	}
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Error("shutdown-raced event consumed the sender invocation budget")
	}
	if !b.roomLimits.Allow(evt.RoomID.String()) {
		t.Error("shutdown-raced event consumed the room invocation budget")
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 ||
		audits[0]["outcome"] != outcomeShutdown ||
		audits[0]["terminal_stage"] != "queue" ||
		audits[0]["terminal_reason"] != "shutdown_enqueue_rejected" ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) ||
		audits[0]["a2a_attempted"] != false {
		t.Fatalf("shutdown-raced event audit = %#v", audits)
	}
}

func TestDispatcherShutdownDropsAcceptedQueuedTargetWithTerminalAudit(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "must not run", Terminal: true}}
	b, _, evt, _, recorder := pollingHarness(t, client)
	runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
	b.runCtx = runtimeCtx
	b.dispatcher = newDispatcher(1, 10, 10)
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.roomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	shutdownMetric := delegationsTotal.WithLabelValues("agent-k8s", outcomeShutdown)
	shutdownBefore := counterValue(t, shutdownMetric)

	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(func() {
		cancelRuntime()
		unblock()
		b.dispatcher.Wait()
	})
	if got := b.dispatcher.Enqueue(runtimeCtx, evt.RoomID, func(context.Context) {
		close(started)
		<-release
	}, nil); got != enqueueAccepted {
		t.Fatalf("blocker Enqueue = %v, want accepted", got)
	}
	<-started

	b.HandleMessage(t.Context(), evt)
	cancelRuntime()
	unblock()
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("shutdown-dropped queued event made %d A2A calls", client.callCount)
	}
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("shutdown-dropped queued event emitted Matrix replies: %#v", events)
	}
	if got := counterValue(t, shutdownMetric); got != shutdownBefore+1 {
		t.Errorf("shutdown drop metric = %v, want %v", got, shutdownBefore+1)
	}
	sender := b.agents.IdentifySender(evt.Sender)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Error("shutdown-dropped queued event consumed the sender invocation budget")
	}
	if !b.roomLimits.Allow(evt.RoomID.String()) {
		t.Error("shutdown-dropped queued event consumed the room invocation budget")
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 ||
		audits[0]["outcome"] != outcomeShutdown ||
		audits[0]["terminal_stage"] != "queue" ||
		audits[0]["terminal_reason"] != "shutdown_queued_dropped" ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) ||
		audits[0]["a2a_attempted"] != false {
		t.Fatalf("shutdown-dropped queued event audit = %#v", audits)
	}
}

func loadSlackAllowedAgent(t *testing.T) *AgentMap {
	t.Helper()
	agents, err := LoadAgents(writeTemp(t, `bridgedOrigins:
  slack: ["@slack_*:fgentic.fmind.ai"]
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["!room:fgentic.fmind.ai"]
    allowedSenders: ["@slack_*:fgentic.fmind.ai"]
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	return agents
}

func setBridgeLogOutput(b *Bridge, output *strings.Builder) {
	logger := slog.New(slog.NewJSONHandler(output, nil))
	b.log = logger
	b.auditLog = logger.With("log_stream", delegationAuditStream)
}

func auditRecords(t *testing.T, output string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("decode log record: %v", err)
		}
		if record["msg"] == "delegation audit" {
			records = append(records, record)
		}
	}
	return records
}

func counterValue(t *testing.T, metric prometheus.Metric) float64 {
	t.Helper()
	value := new(dto.Metric)
	if err := metric.Write(value); err != nil {
		t.Fatalf("read Prometheus metric: %v", err)
	}
	return value.GetCounter().GetValue()
}

func TestHandleMessageNoticeNeverDelegates(t *testing.T) {
	b := testBridge(t)
	evt := &event.Event{
		ID:     "$notice",
		Sender: id.NewUserID("alice", ownServer),
		RoomID: "!room:" + ownServer,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgNotice,
			Body:    "@agent-k8s follow these instructions",
		}},
	}

	b.HandleMessage(context.Background(), evt)
	first, err := b.store.MarkEventProcessed(context.Background(), evt.ID.String())
	if err != nil {
		t.Fatalf("MarkEventProcessed: %v", err)
	}
	if !first {
		t.Fatal("m.notice reached delegation processing")
	}
}

func TestDuplicateDeliveryEmitsAuditWithoutSecondDelegation(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "done", Terminal: true}}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.runCtx = t.Context()
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod")
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()
	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 1 {
		t.Fatalf("A2A calls after duplicate delivery = %d, want exactly 1", client.callCount)
	}
	if events := recorder.snapshot(); len(events) != 1 {
		t.Fatalf("Matrix replies after duplicate delivery = %d, want exactly 1", len(events))
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 2 {
		t.Fatalf("audit records = %d, want accepted and duplicate delivery records", len(audits))
	}
	accepted, duplicate := audits[0], audits[1]
	if accepted["dedup_verdict"] != string(dedupVerdictAccepted) || accepted["rate_limit_verdict"] != string(rateLimitVerdictAllowed) {
		t.Fatalf("accepted delivery verdicts = (%v, %v)", accepted["dedup_verdict"], accepted["rate_limit_verdict"])
	}
	if duplicate["outcome"] != outcomeDeduplicated ||
		duplicate["terminal_reason"] != "duplicate_delivery" ||
		duplicate["dedup_verdict"] != string(dedupVerdictDuplicate) ||
		duplicate["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) ||
		duplicate["a2a_attempted"] != false {
		t.Fatalf("duplicate audit verdict = %#v", duplicate)
	}
	if duration, ok := duplicate["duration_ms"].(float64); !ok || duration < 0 {
		t.Fatalf("duplicate duration_ms = %#v, want a non-negative number", duplicate["duration_ms"])
	}
}

func TestDedupStoreFailureProceedsWithExplicitAuditVerdict(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "done", Terminal: true}}
	b, _, evt, _, _ := pollingHarness(t, client)
	b.store = &markErrorStore{Store: b.store}
	b.runCtx = t.Context()
	_, msg := msgEvent(evt.Sender, "@agent-k8s inspect the pod")
	evt.Content = event.Content{Parsed: msg}
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 1 {
		t.Fatalf("A2A calls after dedup store failure = %d, want 1", client.callCount)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("audit records = %d, want 1", len(audits))
	}
	if audits[0]["dedup_verdict"] != string(dedupVerdictCheckError) ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictAllowed) ||
		audits[0]["outcome"] != outcomeOK {
		t.Fatalf("dedup store failure audit verdict = %#v", audits[0])
	}
}

func TestIsOwnUser(t *testing.T) {
	b := testBridge(t)
	cases := []struct {
		name   string
		sender id.UserID
		want   bool
	}{
		{name: "bridge bot", sender: id.NewUserID("a2a-bridge", ownServer), want: true},
		{name: "local ghost", sender: id.NewUserID("agent-k8s", ownServer), want: true},
		{name: "local human", sender: id.NewUserID("alice", ownServer), want: false},
		{name: "foreign ghost", sender: id.NewUserID("agent-k8s", "partner.example"), want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := b.isOwnUser(c.sender); got != c.want {
				t.Errorf("isOwnUser(%s) = %v, want %v", c.sender, got, c.want)
			}
		})
	}
}

func membershipEvent(target string, membership event.Membership) *event.Event {
	return &event.Event{
		Sender:   id.NewUserID("alice", ownServer),
		RoomID:   "!room:fgentic.fmind.ai",
		StateKey: &target,
		Content:  event.Content{Parsed: &event.MemberEventContent{Membership: membership}},
	}
}

// Invites that must NOT be accepted never touch the homeserver (the test AppService has no
// client — reaching Intent would panic): unmapped ghosts, foreign homeservers, regular users,
// and non-invite membership changes.
func TestHandleMembership_IgnoresNonEligibleInvites(t *testing.T) {
	b := testBridge(t)
	for name, evt := range map[string]*event.Event{
		"unmapped ghost":     membershipEvent("@agent-unknown:"+ownServer, event.MembershipInvite),
		"foreign homeserver": membershipEvent("@agent-k8s:evil.example", event.MembershipInvite),
		"regular user":       membershipEvent("@alice:"+ownServer, event.MembershipInvite),
		"join not invite":    membershipEvent("@agent-k8s:"+ownServer, event.MembershipJoin),
		"missing state key": {
			Sender: id.NewUserID("alice", ownServer), RoomID: "!room:fgentic.fmind.ai",
			Content: event.Content{Parsed: &event.MemberEventContent{Membership: event.MembershipInvite}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			b.HandleMembership(t.Context(), evt) // must be a no-op
		})
	}
}

type scriptedPoll struct {
	result a2aclient.Result
	err    error
}

type scriptedA2AClient struct {
	callResult     a2aclient.Result
	callErr        error
	callText       string
	callCount      int
	callMessageIDs []string
	resumeCount    int
	card           *a2a.AgentCard
	cardErr        error
	cardPaths      []string
	polls          []scriptedPoll
	pollPaths      []string
	pollTasks      []string
	cancelPaths    []string
	cancelTasks    []string
	cancelErr      error
	remoteReady    bool
	quoteVerdict   a2aclient.QuoteVerdict // zero value QuoteNotApplicable admits by default

	continueResult    a2aclient.Result // returned by Continue (#116)
	continueErr       error
	continueCount     int
	continueText      string
	continueTaskID    string
	continueContextID string

	callFiles []a2aclient.InboundFile // inbound files forwarded on the last Call (#115)
}

func (c *scriptedA2AClient) ResolveAgentCard(_ context.Context, target a2aclient.Target) (*a2a.AgentCard, error) {
	c.cardPaths = append(c.cardPaths, target.String())
	if c.card == nil && c.cardErr == nil {
		return nil, errors.New("unexpected agent card resolution")
	}
	return c.card, c.cardErr
}

func (c *scriptedA2AClient) Call(_ context.Context, _ a2aclient.Target, text, _ string, files []a2aclient.InboundFile) (a2aclient.Result, error) {
	c.callCount++
	c.callText = text
	c.callFiles = files
	return c.callResult, c.callErr
}

func (c *scriptedA2AClient) CallWithMessageID(
	ctx context.Context,
	target a2aclient.Target,
	messageID, text, contextID string,
	files []a2aclient.InboundFile,
) (a2aclient.Result, error) {
	c.callMessageIDs = append(c.callMessageIDs, messageID)
	return c.Call(ctx, target, text, contextID, files)
}

func (c *scriptedA2AClient) ResumeTask(ctx context.Context, target a2aclient.Target, taskID string) (a2aclient.Result, error) {
	c.resumeCount++
	return c.PollTask(ctx, target, taskID)
}

func (c *scriptedA2AClient) Continue(_ context.Context, _ a2aclient.Target, text, contextID, taskID string) (a2aclient.Result, error) {
	c.continueCount++
	c.continueText = text
	c.continueTaskID = taskID
	c.continueContextID = contextID
	return c.continueResult, c.continueErr
}

func (c *scriptedA2AClient) ContinueWithMessageID(
	ctx context.Context,
	target a2aclient.Target,
	messageID, text, contextID, taskID string,
) (a2aclient.Result, error) {
	c.callMessageIDs = append(c.callMessageIDs, messageID)
	return c.Continue(ctx, target, text, contextID, taskID)
}

func (c *scriptedA2AClient) PollTask(_ context.Context, target a2aclient.Target, taskID string) (a2aclient.Result, error) {
	c.pollPaths = append(c.pollPaths, target.String())
	c.pollTasks = append(c.pollTasks, taskID)
	if len(c.polls) == 0 {
		return a2aclient.Result{}, errors.New("unexpected GetTask")
	}
	next := c.polls[0]
	c.polls = c.polls[1:]
	return next.result, next.err
}

func (c *scriptedA2AClient) CancelTask(_ context.Context, target a2aclient.Target, taskID string) error {
	c.cancelPaths = append(c.cancelPaths, target.String())
	c.cancelTasks = append(c.cancelTasks, taskID)
	return c.cancelErr
}

func (c *scriptedA2AClient) IsReady(target a2aclient.Target) bool {
	return !target.IsRemote() || c.remoteReady
}

func (c *scriptedA2AClient) QuoteAdmission(_ a2aclient.Target, _ uint64) a2aclient.QuoteVerdict {
	return c.quoteVerdict
}

type matrixRecorder struct {
	mu                  sync.Mutex
	events              []event.MessageEventContent
	raw                 [][]byte // verbatim request bodies, so tests can inspect mixins the struct drops
	allowMembershipWire bool
	membershipRequests  []string
}

func (r *matrixRecorder) append(content event.MessageEventContent, raw []byte) id.EventID {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, content)
	r.raw = append(r.raw, slices.Clone(raw))
	return id.EventID(fmt.Sprintf("$reply-%d", len(r.events)))
}

func (r *matrixRecorder) snapshot() []event.MessageEventContent {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events)
}

func (r *matrixRecorder) enableMembershipWire() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.allowMembershipWire = true
}

func (r *matrixRecorder) recordMembershipRequest(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.allowMembershipWire {
		return false
	}
	r.membershipRequests = append(r.membershipRequests, path)
	return true
}

func (r *matrixRecorder) membershipSnapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.membershipRequests)
}

// rawSnapshot returns each sent event body parsed as a generic map, exposing raw content blocks
// (like the MSC3955 mixin) that the typed MessageEventContent decode silently discards.
func (r *matrixRecorder) rawSnapshot(t *testing.T) []map[string]any {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]map[string]any, len(r.raw))
	for i, body := range r.raw {
		if err := json.Unmarshal(body, &out[i]); err != nil {
			t.Fatalf("decode raw Matrix event %d: %v", i, err)
		}
	}
	return out
}

func pollingHarness(
	t *testing.T,
	client a2aClient,
) (*Bridge, *appservice.IntentAPI, *event.Event, *AgentRef, *matrixRecorder) {
	t.Helper()

	recorder := &matrixRecorder{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/register"):
			if !recorder.recordMembershipRequest(req.URL.Path) {
				t.Errorf("unexpected Matrix registration request: %s", req.URL.Path)
				http.NotFound(w, req)
				return
			}
			_, _ = w.Write([]byte("{}"))
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/join"):
			if !recorder.recordMembershipRequest(req.URL.Path) {
				t.Errorf("unexpected Matrix join request: %s", req.URL.Path)
				http.NotFound(w, req)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]id.RoomID{"room_id": "!room:" + ownServer}); err != nil {
				t.Errorf("encode Matrix join response: %v", err)
			}
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/send/m.room.message/"):
			body, err := io.ReadAll(req.Body)
			if err != nil {
				t.Errorf("read Matrix event body: %v", err)
				http.Error(w, "unreadable event", http.StatusBadRequest)
				return
			}
			var content event.MessageEventContent
			if err := json.Unmarshal(body, &content); err != nil {
				t.Errorf("decode Matrix event: %v", err)
				http.Error(w, "invalid event", http.StatusBadRequest)
				return
			}
			if err := json.NewEncoder(w).Encode(map[string]id.EventID{"event_id": recorder.append(content, body)}); err != nil {
				t.Errorf("encode Matrix response: %v", err)
			}
		case req.Method == http.MethodPut && strings.Contains(req.URL.Path, "/typing/"):
			if _, err := w.Write([]byte("{}")); err != nil {
				t.Errorf("write typing response: %v", err)
			}
		default:
			t.Errorf("unexpected Matrix request: %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(server.Close)

	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     &appservice.Registration{AppToken: "test-token", SenderLocalpart: "a2a-bridge"},
		HomeserverDomain: ownServer,
		HomeserverURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}
	as.HTTPClient = server.Client()
	as.DefaultHTTPRetries = 0

	b := testBridge(t)
	b.as = as
	b.client = client
	b.cfg.RequestTimeout = time.Second
	b.cfg.TaskTimeout = time.Minute

	evt, _ := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s inspect the pod")
	evt.ID = "$original"
	ref, ok := b.agents.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s fixture missing")
	}
	intent := as.Intent(id.NewUserID("agent-k8s", ownServer))
	intent.Registered = true
	if err := as.StateStore.SetMembership(t.Context(), evt.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	return b, intent, evt, ref, recorder
}

func TestDispatchUsesFallbackForEmptyTerminalMessage(t *testing.T) {
	received := make(chan *a2a.Message, 1)
	executor := wireExecutorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			received <- execCtx.Message
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx), nil)
		}
	})
	client := newWireA2AClient(t, executor, taskstore.NewInMemory(nil))
	b, _, evt, ref, recorder := pollingHarness(t, client)
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.dispatchWithDedupVerdict(
		t.Context(),
		evt,
		ref,
		"agent-k8s",
		"inspect the pod",
		b.agents.IdentifySender(evt.Sender),
		dedupVerdictAccepted,
	)

	var message *a2a.Message
	select {
	case message = <-received:
	default:
		t.Fatal("wire executor received no message")
	}
	var prompt strings.Builder
	for _, part := range message.Parts {
		prompt.WriteString(part.Text())
	}
	if !strings.Contains(prompt.String(), contentStart+"\ninspect the pod\n"+contentEnd) {
		t.Fatalf("wire prompt does not preserve the provenance envelope:\n%s", prompt.String())
	}
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("Matrix events = %d, want one reply", len(events))
	}
	if got := events[0].Body; got != failureMessage(errorEmptyReply, "agent-k8s", 0) {
		t.Fatalf("reply body = %q, want empty-reply failure", got)
	}
	if got := events[0].RelatesTo.GetReplyTo(); got != evt.ID {
		t.Fatalf("reply target = %q, want %q", got, evt.ID)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("terminal audit records = %d, want exactly 1", len(audits))
	}
	if audits[0]["outcome"] != outcomeFailed || audits[0]["terminal_stage"] != "message_result" {
		t.Fatalf("terminal audit outcome = (%v, %v), want (failed, message_result)", audits[0]["outcome"], audits[0]["terminal_stage"])
	}
	if audits[0]["terminal_reason"] != errorEmptyReply ||
		audits[0]["dedup_verdict"] != string(dedupVerdictAccepted) ||
		audits[0]["rate_limit_verdict"] != string(rateLimitVerdictAllowed) {
		t.Fatalf("terminal audit verdicts = (%v, %v, %v)", audits[0]["terminal_reason"], audits[0]["dedup_verdict"], audits[0]["rate_limit_verdict"])
	}
	if duration, ok := audits[0]["duration_ms"].(float64); !ok || duration < 0 {
		t.Fatalf("duration_ms = %#v, want a non-negative number", audits[0]["duration_ms"])
	}
}

func TestRateLimitedDispatchEmitsExplicitAuditVerdict(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, ref, recorder := pollingHarness(t, client)
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	if !b.senderLimits.Allow(evt.Sender.String() + "|agent-k8s") {
		t.Fatal("failed to consume sender limiter fixture token")
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.dispatchWithDedupVerdict(
		t.Context(),
		evt,
		ref,
		"agent-k8s",
		"inspect the pod",
		b.agents.IdentifySender(evt.Sender),
		dedupVerdictAccepted,
	)

	if client.callCount != 0 {
		t.Fatalf("rate-limited dispatch made %d A2A calls", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorRateLimit, "", 0) {
		t.Fatalf("rate-limit Matrix replies = %#v, want one standard notice", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("rate-limit audit records = %d, want 1", len(audits))
	}
	audit := audits[0]
	if audit["outcome"] != outcomeRateLimited ||
		audit["terminal_reason"] != "rate_limit_rejected" ||
		audit["dedup_verdict"] != string(dedupVerdictAccepted) ||
		audit["rate_limit_verdict"] != string(rateLimitVerdictRejected) ||
		audit["a2a_attempted"] != false {
		t.Fatalf("rate-limit audit verdict = %#v", audit)
	}
}

func TestAwaitTaskPollsWithCappedBackoffAndEmptyReplyFallback(t *testing.T) {
	client := &scriptedA2AClient{polls: []scriptedPoll{
		{err: errors.New("temporary GetTask failure")},
		{result: a2aclient.Result{TaskID: "task-1"}},
		{result: a2aclient.Result{TaskID: "task-1", Terminal: true}},
	}}
	b, intent, evt, ref, recorder := pollingHarness(t, client)
	b.pollInitial = 5 * time.Second
	b.pollMax = 8 * time.Second
	var waits []time.Duration
	b.pollWait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, delay)
		return nil
	}

	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-k8s", a2aclient.Result{
		Text:   "preparing",
		TaskID: "task-1",
	}, "")

	if want := []time.Duration{5 * time.Second, 8 * time.Second, 8 * time.Second}; !slices.Equal(waits, want) {
		t.Fatalf("poll waits = %v, want %v", waits, want)
	}
	if len(client.pollTasks) != 3 {
		t.Fatalf("PollTask calls = %d, want 3", len(client.pollTasks))
	}
	for i := range client.pollTasks {
		if client.pollTasks[i] != "task-1" || client.pollPaths[i] != ref.Path() {
			t.Errorf("PollTask %d = (%q, %q), want (%q, task-1)", i+1, client.pollPaths[i], client.pollTasks[i], ref.Path())
		}
	}
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("Matrix events = %d, want placeholder and edit", len(events))
	}
	if events[0].Body != workingText {
		t.Fatalf("placeholder body = %q, want %q", events[0].Body, workingText)
	}
	assertEdit(t, events[1], failureMessage(errorEmptyReply, "agent-k8s", 0))
	if audit.outcome != outcomeFailed || audit.terminalStage != "task_result" ||
		audit.terminalReason != errorEmptyReply || audit.taskID != "task-1" {
		t.Fatalf("long-task audit = %+v, want empty-reply failure for task-1", audit)
	}
}

func TestAwaitTaskRetriesTransientRealWireFailureWithCappedBackoff(t *testing.T) {
	terminalUpdate := make(chan struct{})
	store := &transientGetStore{
		Store:          taskstore.NewInMemory(nil),
		terminalUpdate: terminalUpdate,
	}
	release := make(chan struct{})
	var releaseOnce sync.Once
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })

	executor := wireExecutorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		return func(yield func(a2a.Event, error) bool) {
			task := a2a.NewSubmittedTask(execCtx, execCtx.Message)
			if !yield(task, nil) {
				return
			}
			<-release
			yield(
				a2a.NewStatusUpdateEvent(
					task,
					a2a.TaskStateCompleted,
					a2a.NewMessage(a2a.MessageRoleAgent, a2a.NewTextPart("finished over the wire")),
				),
				nil,
			)
		}
	})
	client := newWireA2AClient(t, executor, store)
	target, err := a2aclient.NewLocalTarget(wireContractAgent)
	if err != nil {
		t.Fatalf("NewLocalTarget: %v", err)
	}
	working, err := client.Call(t.Context(), target, "long task", "", nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if working.Terminal || working.TaskID == "" {
		t.Fatalf("Call result = %+v, want a non-terminal task", working)
	}

	// Arm the fault only after SendMessage has created the task, so the first GetTask
	// crosses the JSON-RPC wire as a transient protocol error.
	store.failNextGets(1)
	b, intent, evt, ref, recorder := pollingHarness(t, client)
	b.pollInitial = 5 * time.Second
	b.pollMax = 8 * time.Second
	var waits []time.Duration
	b.pollWait = func(ctx context.Context, delay time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		waits = append(waits, delay)
		if len(waits) == 3 {
			releaseOnce.Do(func() { close(release) })
			select {
			case <-terminalUpdate:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}

	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-k8s", working, "")

	if want := []time.Duration{5 * time.Second, 8 * time.Second, 8 * time.Second}; !slices.Equal(waits, want) {
		t.Fatalf("poll waits = %v, want %v", waits, want)
	}
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("Matrix events = %d, want placeholder and edit", len(events))
	}
	assertEdit(t, events[1], "finished over the wire")
	if audit.outcome != outcomeOK || audit.terminalStage != "task_result" || audit.taskID != working.TaskID {
		t.Fatalf("long-task audit = %+v, want completed task_result for %s", audit, working.TaskID)
	}
}

func TestAwaitTaskTimeoutIsDeterministic(t *testing.T) {
	client := &scriptedA2AClient{}
	b, intent, evt, ref, recorder := pollingHarness(t, client)
	b.cfg.TaskTimeout = 0
	b.pollWait = func(ctx context.Context, _ time.Duration) error {
		<-ctx.Done()
		return ctx.Err()
	}

	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-k8s", a2aclient.Result{TaskID: "task-timeout"}, "")

	if len(client.pollTasks) != 0 {
		t.Fatalf("PollTask calls = %d, want none after timeout", len(client.pollTasks))
	}
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("Matrix events = %d, want placeholder and timeout edit", len(events))
	}
	if events[0].Body != workingText {
		t.Fatalf("placeholder body = %q, want %q", events[0].Body, workingText)
	}
	assertEdit(t, events[1], failureMessage(errorTaskTimeout, "agent-k8s", 0))
	if audit.outcome != outcomeTimeout || audit.terminalStage != "task_poll" || audit.taskID != "task-timeout" {
		t.Fatalf("timeout audit = %+v, want timeout task_poll for task-timeout", audit)
	}
}

func TestDispatchRefusesQuarantinedRemoteBeforeAdmission(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &scriptedA2AClient{remoteReady: false}
	b, _, evt, _, recorder := pollingHarness(t, client)
	b.agents = agents
	b.client = client
	b.profiles = newProfileStore(agents.Entries())
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	evt.Sender = id.NewUserID("alice", ownServer)
	evt.ID = "$remote-untrusted"
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote fixture missing")
	}
	sender := agents.IdentifySender(evt.Sender)
	intent := b.as.Intent(id.NewUserID("agent-remote", ownServer))
	intent.Registered = true
	if err := b.as.StateStore.SetMembership(t.Context(), evt.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	b.dispatchResolvedTarget(
		t.Context(), evt, "agent-remote", "inspect the pod", ref, sender, dedupVerdictAccepted,
	)

	if client.callCount != 0 {
		t.Fatalf("A2A calls = %d, want zero", client.callCount)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorAgentUntrusted, "agent-remote", 0) {
		t.Fatalf("remote refusal events = %#v, want one trust notice", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("delegation audits = %d, want one", len(audits))
	}
	audit := audits[0]
	if audit["outcome"] != outcomeDenied || audit["terminal_stage"] != "agent_card" ||
		audit["terminal_reason"] != "agent_card_untrusted" || audit["a2a_attempted"] != false ||
		audit["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) {
		t.Fatalf("remote refusal audit = %#v", audit)
	}
}

func TestDispatchRefusesRemoteOverBudgetQuoteBeforeAdmission(t *testing.T) {
	yaml := strings.Replace(validRemoteAgentsYAML, "    tokenBudget: 8192\n", "    tokenBudget: 8192\n    maxCost: 5\n", 1)
	agents, err := LoadAgents(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote fixture missing")
	}
	if ref.MaxCost() != 5 {
		t.Fatalf("MaxCost() = %d, want 5", ref.MaxCost())
	}
	for _, verdict := range []a2aclient.QuoteVerdict{a2aclient.QuoteOverBudget, a2aclient.QuoteMissing} {
		t.Run(fmt.Sprintf("verdict-%d", verdict), func(t *testing.T) {
			client := &scriptedA2AClient{remoteReady: true, quoteVerdict: verdict}
			b := testBridge(t)
			b.agents = agents
			b.client = client
			b.profiles = newProfileStore(agents.Entries())
			remoteGhost := id.NewUserID("agent-remote", ownServer)
			if err := b.as.StateStore.SetMembership(
				t.Context(), "!room:"+ownServer, remoteGhost, event.MembershipJoin,
			); err != nil {
				t.Fatalf("SetMembership: %v", err)
			}
			var output strings.Builder
			setBridgeLogOutput(b, &output)

			evt, _ := msgEvent(id.NewUserID("alice", ownServer), "@agent-remote inspect the pod")
			evt.ID = "$remote-over-budget"
			sender := agents.IdentifySender(evt.Sender)
			b.dispatchResolvedTarget(t.Context(), evt, "agent-remote", "inspect the pod", ref, sender, dedupVerdictAccepted)

			if client.callCount != 0 {
				t.Fatalf("A2A calls = %d, want zero (fail closed before dispatch)", client.callCount)
			}
			audits := auditRecords(t, output.String())
			if len(audits) != 1 {
				t.Fatalf("delegation audits = %d, want one", len(audits))
			}
			audit := audits[0]
			if audit["outcome"] != outcomeDenied || audit["terminal_stage"] != "admission" ||
				audit["terminal_reason"] != "quote_over_budget" || audit["a2a_attempted"] != false ||
				audit["rate_limit_verdict"] != string(rateLimitVerdictNotChecked) {
				t.Fatalf("over-budget refusal audit = %#v", audit)
			}
		})
	}
}

func TestDispatchEnforcesStagingRoomBoundary(t *testing.T) {
	stagingRoom := id.RoomID("!staging:" + ownServer)
	otherRoom := id.RoomID("!prod:" + ownServer)
	agents, err := LoadAgents(writeTemp(t, `agents:
  agent-dev: {namespace: kagent, name: dev-agent, stage: dev, allowedRooms: ["!staging:fgentic.fmind.ai", "!prod:fgentic.fmind.ai"]}
  agent-prod: {namespace: kagent, name: prod-agent, stage: prod, allowedRooms: ["!staging:fgentic.fmind.ai", "!prod:fgentic.fmind.ai"]}
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "ok", Terminal: true}}
	b, _, _, _, recorder := pollingHarness(t, client)
	b.agents = agents
	b.stagingRooms = map[string]struct{}{stagingRoom.String(): {}}
	for _, localpart := range []string{"agent-dev", "agent-prod"} {
		for _, room := range []id.RoomID{stagingRoom, otherRoom} {
			intent := b.as.Intent(id.NewUserID(localpart, ownServer))
			intent.Registered = true
			if err := b.as.StateStore.SetMembership(t.Context(), room, intent.UserID, event.MembershipJoin); err != nil {
				t.Fatalf("SetMembership: %v", err)
			}
		}
	}

	dispatch := func(localpart string, room id.RoomID, eventID string) map[string]any {
		var output strings.Builder
		setBridgeLogOutput(b, &output)
		ghost := id.NewUserID(localpart, ownServer)
		evt, _ := msgEvent(id.NewUserID("alice", ownServer), "@"+localpart+" do it", ghost)
		evt.ID = id.EventID(eventID)
		evt.RoomID = room
		ref, _ := agents.Lookup(localpart)
		b.dispatchResolvedTarget(t.Context(), evt, localpart, "do it", ref, agents.IdentifySender(evt.Sender), dedupVerdictAccepted)
		audits := auditRecords(t, output.String())
		if len(audits) != 1 {
			t.Fatalf("%s in %s: audits = %d, want 1", localpart, room, len(audits))
		}
		return audits[0]
	}

	// dev agent in a staging room: dispatches to A2A and completes.
	if audit := dispatch("agent-dev", stagingRoom, "$dev-staging"); audit["outcome"] != outcomeOK {
		t.Fatalf("dev-in-staging audit = %#v, want ok", audit)
	}
	if client.callCount != 1 {
		t.Fatalf("dev-in-staging A2A calls = %d, want 1", client.callCount)
	}
	// dev agent elsewhere: refused fail-closed, no A2A, distinct reason, one bounded notice.
	audit := dispatch("agent-dev", otherRoom, "$dev-other")
	if audit["outcome"] != outcomeDenied || audit["terminal_stage"] != "admission" ||
		audit["terminal_reason"] != "stage_policy_rejected" {
		t.Fatalf("dev-elsewhere audit = %#v, want denied stage_policy_rejected", audit)
	}
	if client.callCount != 1 {
		t.Fatalf("dev-elsewhere spent an A2A call: calls = %d", client.callCount)
	}
	// prod agent is unaffected by the staging boundary in any room.
	if audit := dispatch("agent-prod", otherRoom, "$prod-other"); audit["outcome"] != outcomeOK {
		t.Fatalf("prod-elsewhere audit = %#v, want ok", audit)
	}
	if client.callCount != 2 {
		t.Fatalf("prod A2A calls total = %d, want 2", client.callCount)
	}

	notices := 0
	for _, evt := range recorder.snapshot() {
		if evt.Body == failureMessage(errorStagePolicy, "agent-dev", 0) {
			notices++
		}
	}
	if notices != 1 {
		t.Fatalf("stage-denied notices = %d, want exactly one", notices)
	}
}

func messageBodyContains(r *matrixRecorder, substr string) bool {
	for _, e := range r.snapshot() {
		if strings.Contains(e.Body, substr) {
			return true
		}
	}
	return false
}

func threadReply(sender id.UserID, root id.EventID, body string) (*event.Event, *event.MessageEventContent) {
	msg := &event.MessageEventContent{
		MsgType:   event.MsgText,
		Body:      body,
		RelatesTo: &event.RelatesTo{Type: event.RelThread, EventID: root},
	}
	evt := &event.Event{Sender: sender, RoomID: "!room:" + ownServer, Content: event.Content{Parsed: msg}}
	return evt, msg
}

func TestAwaitTaskPausesOnInputRequired(t *testing.T) {
	b, intent, evt, ref, recorder := pollingHarness(t, &scriptedA2AClient{})
	res := a2aclient.Result{TaskID: "task-1", ContextID: "ctx-1", InputRequired: true, Text: "which namespace?"}
	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-k8s", res, "")

	if audit.outcome != outcomeInputRequired || audit.terminalStage != "task_input" ||
		audit.terminalReason != "awaiting_input" || audit.taskID != "task-1" {
		t.Fatalf("pause audit = %+v", audit)
	}
	_, owner, ok := b.openTasks.owner(audit.replyEventID)
	if !ok || owner != evt.Sender {
		t.Fatalf("open-task owner = %q (present=%v), want %s", owner, ok, evt.Sender)
	}
	if !messageBodyContains(recorder, "which namespace?") {
		t.Fatal("agent question not surfaced to the room")
	}
}

func TestAwaitTaskStopsOnAuthRequiredWithoutRelay(t *testing.T) {
	b, intent, evt, ref, recorder := pollingHarness(t, &scriptedA2AClient{})
	res := a2aclient.Result{TaskID: "task-2", AuthRequired: true}
	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-k8s", res, "")

	if audit.outcome != outcomeFailed || audit.terminalStage != "task_auth" ||
		audit.terminalReason != "auth_required_not_forwarded" {
		t.Fatalf("auth audit = %+v", audit)
	}
	if _, _, ok := b.openTasks.owner(audit.replyEventID); ok {
		t.Fatal("auth-required must not register a resumable open task (no credential relay)")
	}
	if !messageBodyContains(recorder, "does not forward") {
		t.Fatal("honest auth notice not posted")
	}
}

func TestThreadContinuationGatesToOriginalSender(t *testing.T) {
	b, _, origin, ref, recorder := pollingHarness(t, &scriptedA2AClient{continueResult: a2aclient.Result{Text: "ok", Terminal: true}})
	// The wrong-sender denial posts as the bot; pre-register it so the harness serves the notice.
	botIntent := b.as.BotIntent()
	botIntent.Registered = true
	if err := b.as.StateStore.SetMembership(t.Context(), origin.RoomID, botIntent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	placeholder := id.EventID("$placeholder")
	open := &openTask{
		origin: origin, placeholder: placeholder, localpart: "agent-k8s", ref: ref,
		taskID: "task-3", contextID: "ctx-3", sender: b.agents.IdentifySender(origin.Sender),
	}
	if err := b.store.SetContext(t.Context(), origin.RoomID.String(), open.localpart, open.contextID, origin.Sender.String()); err != nil {
		t.Fatal(err)
	}
	b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })

	// A wrong sender is refused and never consumes the pending answer slot.
	wrong, wrongMsg := threadReply(id.NewUserID("mallory", ownServer), placeholder, "wrong answer")
	wrong.ID = "$wrong"
	if !b.handleThreadContinuation(t.Context(), wrong, wrongMsg) {
		t.Fatal("wrong-sender thread reply not consumed as a continuation attempt")
	}
	if _, _, ok := b.openTasks.owner(placeholder); !ok {
		t.Fatal("wrong-sender reply consumed the open task")
	}
	if !messageBodyContains(recorder, "only the person who started") {
		t.Fatal("wrong-sender denial notice not posted")
	}

	// The original sender's reply claims the task (the resume itself is covered separately).
	right, rightMsg := threadReply(origin.Sender, placeholder, "kube-system")
	right.ID = "$answer"
	b.runCtx = t.Context()
	if !b.handleThreadContinuation(t.Context(), right, rightMsg) {
		t.Fatal("owner thread reply not consumed")
	}
	if _, _, ok := b.openTasks.owner(placeholder); ok {
		t.Fatal("owner reply did not claim the open task")
	}
	b.dispatcher.Wait()
}

func TestContinueOpenTaskResumesAndCompletes(t *testing.T) {
	client := &scriptedA2AClient{continueResult: a2aclient.Result{Text: "created the pod", Terminal: true, ContextID: "ctx-4"}}
	b, _, origin, ref, recorder := pollingHarness(t, client)
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	placeholder := id.EventID("$placeholder")
	open := &openTask{
		origin: origin, placeholder: placeholder, localpart: "agent-k8s", ref: ref,
		taskID: "task-4", contextID: "ctx-4", sender: b.agents.IdentifySender(origin.Sender),
	}
	if err := b.store.SetContext(t.Context(), origin.RoomID.String(), open.localpart, open.contextID, origin.Sender.String()); err != nil {
		t.Fatal(err)
	}
	b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })

	reply, _ := threadReply(origin.Sender, placeholder, "kube-system")
	reply.ID = "$answer"
	b.continueOpenTask(t.Context(), reply, open, "kube-system")

	if client.continueCount != 1 || client.continueTaskID != "task-4" || client.continueContextID != "ctx-4" {
		t.Fatalf("Continue = count %d taskID %q contextID %q", client.continueCount, client.continueTaskID, client.continueContextID)
	}
	// The answer lands as an m.replace edit of the same placeholder, one coherent thread.
	if !messageBodyContains(recorder, "created the pod") {
		t.Fatal("final answer not edited into the placeholder")
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["outcome"] != outcomeOK ||
		audits[0]["terminal_reason"] != "completed" || audits[0]["a2a_task_id"] != "task-4" {
		t.Fatalf("continuation audit = %#v", audits)
	}
}

func TestExpireOpenTaskDropsPausedTask(t *testing.T) {
	b, _, origin, ref, recorder := pollingHarness(t, &scriptedA2AClient{})
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	b.runCtx = t.Context()
	placeholder := id.EventID("$placeholder")
	open := &openTask{
		origin: origin, placeholder: placeholder, localpart: "agent-k8s", ref: ref,
		taskID: "task-5", contextID: "ctx-5", sender: b.agents.IdentifySender(origin.Sender),
	}
	b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })

	b.expireOpenTask(open)
	if _, _, ok := b.openTasks.owner(placeholder); ok {
		t.Fatal("expiry did not drop the open task")
	}
	if !messageBodyContains(recorder, "no reply within") {
		t.Fatal("stale notice not posted on expiry")
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["outcome"] != outcomeTimeout || audits[0]["terminal_reason"] != "input_wait_timeout" {
		t.Fatalf("expiry audit = %#v", audits)
	}
	// A second expiry is a no-op (the claim already fired).
	b.expireOpenTask(open)
	if got := auditRecords(t, output.String()); len(got) != 1 {
		t.Fatalf("double expiry emitted %d audits, want 1", len(got))
	}
}

func TestThreadContinuationRebindsRoom(t *testing.T) {
	b, _, origin, ref, _ := pollingHarness(t, &scriptedA2AClient{})
	placeholder := id.EventID("$ph-rebind")
	open := &openTask{
		origin: origin, placeholder: placeholder, localpart: "agent-k8s", ref: ref,
		taskID: "t", contextID: "c", sender: b.agents.IdentifySender(origin.Sender),
	}
	b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })

	// The owner replies threaded under the placeholder ID but from a different room: not a
	// continuation here, and it must not resume the task elsewhere.
	reply, replyMsg := threadReply(origin.Sender, placeholder, "answer")
	reply.RoomID = "!elsewhere:" + ownServer
	reply.ID = "$elsewhere"
	if b.handleThreadContinuation(t.Context(), reply, replyMsg) {
		t.Fatal("a cross-room reply was consumed as a continuation")
	}
	if _, _, ok := b.openTasks.owner(placeholder); !ok {
		t.Fatal("cross-room reply consumed the open task")
	}
}

func TestContinueOpenTaskReappliesCostGate(t *testing.T) {
	yaml := strings.Replace(validRemoteAgentsYAML, "    tokenBudget: 8192\n", "    tokenBudget: 8192\n    maxCost: 5\n", 1)
	agents, err := LoadAgents(writeTemp(t, yaml))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, _ := agents.Lookup("agent-remote")
	client := &scriptedA2AClient{remoteReady: true, quoteVerdict: a2aclient.QuoteOverBudget}
	b, _, origin, _, _ := pollingHarness(t, client)
	b.agents = agents
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	intent := b.as.Intent(id.NewUserID("agent-remote", ownServer))
	intent.Registered = true
	if err := b.as.StateStore.SetMembership(t.Context(), origin.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	open := &openTask{
		origin: origin, placeholder: "$ph-cost", localpart: "agent-remote", ref: ref,
		taskID: "t", contextID: "c", sender: b.agents.IdentifySender(origin.Sender),
	}
	b.openTasks.register(open, b.cfg.InputWaitTimeout, func() { b.expireOpenTask(open) })

	reply, _ := threadReply(origin.Sender, "$ph-cost", "the answer")
	reply.ID = "$answer"
	b.continueOpenTask(t.Context(), reply, open, "the answer")

	if client.continueCount != 0 {
		t.Fatalf("resume dispatched despite an over-budget quote: continueCount = %d", client.continueCount)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["outcome"] != outcomeDenied || audits[0]["terminal_reason"] != "quote_over_budget" {
		t.Fatalf("cost-gate audit = %#v", audits)
	}
}

func TestDispatchClassifiesTrustRevocationAtTransportBoundary(t *testing.T) {
	client := &scriptedA2AClient{
		callErr:     fmt.Errorf("remote transport refused request: %w", a2aclient.ErrRemoteTargetUntrusted),
		remoteReady: true,
	}
	b, _, evt, _, recorder := pollingHarness(t, client)
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b.agents = agents
	b.profiles = newProfileStore(agents.Entries())
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote fixture missing")
	}
	evt.ID = "$remote-trust-race"
	evt.Sender = id.NewUserID("alice", ownServer)
	intent := b.as.Intent(id.NewUserID("agent-remote", ownServer))
	intent.Registered = true
	if err := b.as.StateStore.SetMembership(t.Context(), evt.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.dispatchWithDedupVerdict(
		t.Context(), evt, ref, "agent-remote", "inspect the pod",
		agents.IdentifySender(evt.Sender), dedupVerdictAccepted,
	)

	if events := recorder.snapshot(); len(events) != 1 ||
		events[0].Body != failureMessage(errorAgentUntrusted, "agent-remote", 0) {
		t.Fatalf("Matrix events after transport trust refusal = %#v, want trust notice", events)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 {
		t.Fatalf("delegation audits = %d, want one", len(audits))
	}
	audit := audits[0]
	if audit["outcome"] != outcomeDenied || audit["terminal_stage"] != "agent_card" ||
		audit["terminal_reason"] != "agent_card_untrusted" || audit["a2a_attempted"] != false ||
		audit["rate_limit_verdict"] != string(rateLimitVerdictAllowed) {
		t.Fatalf("transport trust refusal audit = %#v", audit)
	}
}

func TestAwaitTaskStopsImmediatelyWhenRemoteTrustIsRevoked(t *testing.T) {
	client := &scriptedA2AClient{polls: []scriptedPoll{
		{err: fmt.Errorf("remote transport refused request: %w", a2aclient.ErrRemoteTargetUntrusted)},
		{result: a2aclient.Result{TaskID: "task-remote", Terminal: true, Text: "must not be observed"}},
	}}
	b, intent, evt, _, recorder := pollingHarness(t, client)
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b.agents = agents
	b.profiles = newProfileStore(agents.Entries())
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote fixture missing")
	}
	b.pollWait = func(context.Context, time.Duration) error { return nil }

	audit := b.awaitTask(t.Context(), t.Context(), intent, evt, ref, "agent-remote", a2aclient.Result{
		TaskID: "task-remote",
	}, "")

	if len(client.pollTasks) != 1 {
		t.Fatalf("PollTask calls = %d, want one", len(client.pollTasks))
	}
	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("Matrix events = %d, want placeholder and trust-refusal edit", len(events))
	}
	assertEdit(t, events[1], failureMessage(errorAgentUntrusted, "agent-remote", 0))
	if audit.outcome != outcomeDenied || audit.terminalStage != "agent_card" ||
		audit.terminalReason != "agent_card_untrusted" || !audit.a2aAttempted ||
		audit.rateLimitVerdict != rateLimitVerdictAllowed {
		t.Fatalf("task trust refusal audit = %+v", audit)
	}
}

type deadlineA2AClient struct {
	remaining time.Duration
}

func (c *deadlineA2AClient) Call(ctx context.Context, _ a2aclient.Target, _, _ string, _ []a2aclient.InboundFile) (a2aclient.Result, error) {
	deadline, ok := ctx.Deadline()
	if !ok {
		return a2aclient.Result{}, errors.New("A2A call has no deadline")
	}
	c.remaining = time.Until(deadline)
	<-ctx.Done()
	return a2aclient.Result{}, ctx.Err()
}

func (c *deadlineA2AClient) Continue(ctx context.Context, target a2aclient.Target, text, contextID, _ string) (a2aclient.Result, error) {
	return c.Call(ctx, target, text, contextID, nil)
}

func (*deadlineA2AClient) PollTask(context.Context, a2aclient.Target, string) (a2aclient.Result, error) {
	return a2aclient.Result{}, errors.New("unexpected A2A task poll")
}

func (*deadlineA2AClient) CancelTask(context.Context, a2aclient.Target, string) error {
	return errors.New("unexpected A2A task cancel")
}

func (*deadlineA2AClient) ResolveAgentCard(context.Context, a2aclient.Target) (*a2a.AgentCard, error) {
	return nil, errors.New("unexpected AgentCard resolution")
}

func (*deadlineA2AClient) IsReady(a2aclient.Target) bool { return true }

func (*deadlineA2AClient) QuoteAdmission(a2aclient.Target, uint64) a2aclient.QuoteVerdict {
	return a2aclient.QuoteNotApplicable
}

func TestRemoteTimeoutBoundsDelegationWithoutCancellingMatrixNotice(t *testing.T) {
	client := &deadlineA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	agents, err := LoadAgents(writeTemp(t, strings.Replace(validRemoteAgentsYAML, "timeout: 12s", "timeout: 10ms", 1)))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b.agents = agents
	b.profiles = newProfileStore(agents.Entries())
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote fixture missing")
	}
	evt.ID = "$remote-timeout"
	evt.Sender = id.NewUserID("alice", ownServer)
	intent := b.as.Intent(id.NewUserID("agent-remote", ownServer))
	intent.Registered = true
	if err := b.as.StateStore.SetMembership(t.Context(), evt.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	b.dispatchWithDedupVerdict(
		t.Context(), evt, ref, "agent-remote", "inspect the pod",
		agents.IdentifySender(evt.Sender), dedupVerdictAccepted,
	)

	if client.remaining <= 0 || client.remaining > 25*time.Millisecond {
		t.Fatalf("A2A deadline remaining = %s, want remote 10ms ceiling", client.remaining)
	}
	events := recorder.snapshot()
	if len(events) != 1 || events[0].Body != failureMessage(errorRequestTimeout, "agent-remote", b.cfg.RequestTimeout) {
		t.Fatalf("Matrix events after remote timeout = %#v, want one failure notice", events)
	}
}

func assertEdit(t *testing.T, content event.MessageEventContent, want string) {
	t.Helper()
	if content.RelatesTo == nil || content.RelatesTo.GetReplaceID() != "$reply-1" {
		t.Fatalf("edit target = %v, want $reply-1", content.RelatesTo)
	}
	if content.NewContent == nil || content.NewContent.Body != want {
		t.Fatalf("edit content = %+v, want body %q", content.NewContent, want)
	}
}

// TestAutomatedContentMixinGolden pins the exact wire bytes of an automated reply and edit. The
// mixin rides under the MSC3955 unstable prefix (org.matrix.msc1767.automated, per the MSC's own
// "Unstable prefix" section) alongside an untouched m.notice; any drift in reply content shape
// breaks these fixtures deterministically.
func TestAutomatedContentMixinGolden(t *testing.T) {
	reply := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "done"}
	reply.SetReply(&event.Event{ID: "$orig", Sender: id.NewUserID("alice", ownServer)})

	edit := &event.MessageEventContent{MsgType: event.MsgNotice, Body: "updated"}
	edit.SetEdit("$placeholder")

	tests := []struct {
		name    string
		content *event.MessageEventContent
		golden  string
	}{
		{
			name:    "reply",
			content: reply,
			golden:  `{"body":"done","m.mentions":{"user_ids":["@alice:fgentic.fmind.ai"]},"m.relates_to":{"m.in_reply_to":{"event_id":"$orig"}},"msgtype":"m.notice","org.matrix.msc1767.automated":true}`,
		},
		{
			name:    "edit",
			content: edit,
			golden:  `{"body":"* updated","m.mentions":{},"m.new_content":{"body":"updated","msgtype":"m.notice","org.matrix.msc1767.automated":true},"m.relates_to":{"event_id":"$placeholder","rel_type":"m.replace"},"msgtype":"m.notice","org.matrix.msc1767.automated":true}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(automatedContent(tc.content))
			if err != nil {
				t.Fatalf("marshal automated content: %v", err)
			}
			if string(got) != tc.golden {
				t.Fatalf("automated content mismatch:\n got: %s\nwant: %s", got, tc.golden)
			}
		})
	}
}

// TestRepliesCarryAutomatedMixin proves the mixin is stamped on the real send path for both a
// posted reply and an m.replace edit, and that the edit keeps the marker inside m.new_content so
// edit-aware clients still see it after applying the replacement.
func TestRepliesCarryAutomatedMixin(t *testing.T) {
	b, intent, evt, _, recorder := pollingHarness(t, nil)

	replyID := b.postReply(t.Context(), intent, evt, "hello")
	if replyID == "" {
		t.Fatal("postReply returned no event id")
	}
	b.editReply(t.Context(), intent, evt.RoomID, replyID, "updated")

	raw := recorder.rawSnapshot(t)
	if len(raw) != 2 {
		t.Fatalf("sent events = %d, want post + edit", len(raw))
	}

	post := raw[0]
	if post["msgtype"] != string(event.MsgNotice) {
		t.Errorf("post msgtype = %v, want m.notice", post["msgtype"])
	}
	if post[automatedMixinKey] != true {
		t.Errorf("post reply missing %s mixin: %v", automatedMixinKey, post)
	}

	edit := raw[1]
	if edit["msgtype"] != string(event.MsgNotice) {
		t.Errorf("edit msgtype = %v, want m.notice", edit["msgtype"])
	}
	if edit[automatedMixinKey] != true {
		t.Errorf("edit missing top-level %s mixin: %v", automatedMixinKey, edit)
	}
	newContent, ok := edit[newContentKey].(map[string]any)
	if !ok {
		t.Fatalf("edit missing %s block: %v", newContentKey, edit)
	}
	if newContent[automatedMixinKey] != true {
		t.Errorf("edit replacement missing %s mixin: %v", automatedMixinKey, newContent)
	}
	if newContent["msgtype"] != string(event.MsgNotice) {
		t.Errorf("edit replacement msgtype = %v, want m.notice", newContent["msgtype"])
	}
}
