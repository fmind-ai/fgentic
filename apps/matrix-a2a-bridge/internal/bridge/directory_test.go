package bridge

import (
	"context"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestAgentDirectoryListsOnlyAgentsVisibleToSenderWithoutA2A(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	b.profiles.set("agent-k8s", agentProfile{
		DisplayName: "Kubernetes Specialist",
		Description: "Diagnoses cluster health from the live AgentCard.",
		AgentPath:   "/api/a2a/kagent/k8s-agent",
		Status:      profileStatusLive,
	})
	b.profiles.set("agent-locked", agentProfile{
		DisplayName: "Restricted Specialist",
		Description: "Handles restricted operations.",
		AgentPath:   "/api/a2a/kagent/locked-agent",
		Status:      profileStatusCached,
	})

	alice := directoryEvent("$agents-alice", id.NewUserID("alice", ownServer), evt.RoomID)
	b.HandleMessage(t.Context(), alice)
	admin := directoryEvent("$agents-admin", id.NewUserID("admin", ownServer), evt.RoomID)
	b.HandleMessage(t.Context(), admin)
	// Appservice transaction retries must not emit another directory reply.
	b.HandleMessage(t.Context(), admin)

	events := recorder.snapshot()
	if len(events) != 2 {
		t.Fatalf("Matrix events = %d, want Alice and admin directory replies", len(events))
	}
	assertDirectoryNotice(t, events[0], alice.ID)
	if !strings.Contains(events[0].Body, "Kubernetes Specialist") ||
		!strings.Contains(events[0].Body, "@agent-k8s:"+ownServer) ||
		!strings.Contains(events[0].Body, "AgentCard live") ||
		!strings.Contains(events[0].Body, "Diagnoses cluster health from the live AgentCard.") {
		t.Errorf("Alice directory missing live agent metadata:\n%s", events[0].Body)
	}
	if strings.Contains(events[0].Body, "@agent-locked:") || strings.Contains(events[0].Body, "restricted operations") {
		t.Errorf("Alice directory leaked a denied agent:\n%s", events[0].Body)
	}

	assertDirectoryNotice(t, events[1], admin.ID)
	if !strings.Contains(events[1].Body, "@agent-k8s:"+ownServer) ||
		!strings.Contains(events[1].Body, "@agent-locked:"+ownServer) ||
		!strings.Contains(events[1].Body, "AgentCard cached (refresh failed)") {
		t.Errorf("admin directory did not list both permitted agents:\n%s", events[1].Body)
	}
	if client.callCount != 0 || len(client.pollPaths) != 0 || len(client.cardPaths) != 0 {
		t.Fatalf("!agents touched A2A: calls=%d polls=%v cards=%v", client.callCount, client.pollPaths, client.cardPaths)
	}
}

func TestAgentDirectoryUsesDedicatedNoticeLimits(t *testing.T) {
	tests := []struct {
		name               string
		noticeSenderLimits *limiters
		noticeRoomLimits   *limiters
		senders            []id.UserID
	}{
		{
			name:               "sender",
			noticeSenderLimits: newLimiters(1, 1, testRateLimitBucketCapacity),
			noticeRoomLimits:   newLimiters(60, 10, testRateLimitBucketCapacity),
			senders:            []id.UserID{id.NewUserID("alice", ownServer), id.NewUserID("alice", ownServer)},
		},
		{
			name:               "room",
			noticeSenderLimits: newLimiters(60, 10, testRateLimitBucketCapacity),
			noticeRoomLimits:   newLimiters(1, 1, testRateLimitBucketCapacity),
			senders:            []id.UserID{id.NewUserID("alice", ownServer), id.NewUserID("bob", ownServer)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &scriptedA2AClient{}
			b, _, evt, _, recorder := pollingHarness(t, client)
			prepareDirectoryBot(t, b, evt.RoomID)
			b.noticeSenderLimits = tt.noticeSenderLimits
			b.noticeRoomLimits = tt.noticeRoomLimits
			for i, sender := range tt.senders {
				b.HandleMessage(t.Context(), directoryEvent(id.EventID("$rate-"+tt.name+string(rune('0'+i))), sender, evt.RoomID))
			}
			events := recorder.snapshot()
			if len(events) != 1 {
				t.Fatalf("Matrix events = %d, want one directory then silent suppression", len(events))
			}
			if events[0].Body == failureMessage(errorRateLimit, "", 0) {
				t.Fatalf("directory response used amplifying rate-limit text: %q", events[0].Body)
			}
			if client.callCount != 0 {
				t.Fatalf("rate-limited !agents made %d A2A calls", client.callCount)
			}
		})
	}
}

func TestDeniedBridgedAgentDirectoryFloodPreservesInvocationBudgets(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	agents, err := LoadAgents(writeTemp(t, `bridgedOrigins:
  slack: ["@slack_*:fgentic.fmind.ai"]
agents:
  agent-k8s: {namespace: kagent, name: k8s-agent}
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b.agents = agents
	b.senderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.roomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeSenderLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	b.noticeRoomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	senderID := id.NewUserID("slack_U123", ownServer)

	for i := range 10 {
		b.HandleMessage(
			t.Context(),
			directoryEvent(id.EventID("$agents-slack-"+string(rune('0'+i))), senderID, evt.RoomID),
		)
	}

	events := recorder.snapshot()
	if len(events) != 1 || !strings.Contains(events[0].Body, "No mapped agents are available") {
		t.Fatalf("denied bridged directory flood events = %#v, want one bounded response", events)
	}
	if events[0].Body == failureMessage(errorRateLimit, "", 0) {
		t.Fatalf("denied bridged directory flood emitted rate-limit amplification: %q", events[0].Body)
	}
	sender := b.agents.IdentifySender(senderID)
	if !b.senderLimits.Allow(sender.rateLimitKey("agent-k8s")) {
		t.Error("denied bridged directory flood consumed the sender invocation budget")
	}
	if !b.roomLimits.Allow(evt.RoomID.String()) {
		t.Error("denied bridged directory flood consumed the room invocation budget")
	}
	if client.callCount != 0 {
		t.Fatalf("denied bridged directory flood made %d A2A calls", client.callCount)
	}
}

func TestAgentDirectoryReportsNoVisibleMappings(t *testing.T) {
	b := testBridge(t)
	sender := id.NewUserID("mallory", "partner.example")
	body := b.agentDirectoryText(sender)
	if !strings.Contains(body, "No mapped agents are available") || strings.Contains(body, "@agent-") {
		t.Fatalf("directory for denied federated sender = %q", body)
	}
}

func TestAgentDirectoryHidesRemoteTargetWhenVerifiedFreshnessExpires(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &scriptedA2AClient{remoteReady: false}
	b := testBridge(t)
	b.agents = agents
	b.client = client
	b.profiles = newProfileStore(agents.Entries())
	ref, _ := agents.Lookup("agent-remote")
	profile := fallbackProfile(ref)
	profile.Status = profileStatusLive
	b.profiles.set("agent-remote", profile)
	sender := id.NewUserID("alice", ownServer)

	if body := b.agentDirectoryText(sender); strings.Contains(body, "@agent-remote:") {
		t.Fatalf("directory advertised unready remote target: %s", body)
	}
	client.remoteReady = true
	if body := b.agentDirectoryText(sender); !strings.Contains(body, "@agent-remote:"+ownServer) {
		t.Fatalf("directory omitted ready remote target: %s", body)
	}
}

func TestAgentDirectoryForBridgedSenderListsOnlyExplicitMappings(t *testing.T) {
	b := testBridge(t)
	body := b.agentDirectoryText(id.NewUserID("slack_U123", ownServer))
	if !strings.Contains(body, "@agent-slack:"+ownServer) {
		t.Fatalf("bridged sender directory omitted explicitly allowed mapping: %q", body)
	}
	for _, denied := range []string{"@agent-k8s:", "@agent-locked:"} {
		if strings.Contains(body, denied) {
			t.Errorf("bridged sender directory leaked denied mapping %q: %q", denied, body)
		}
	}
}

func directoryEvent(eventID id.EventID, sender id.UserID, roomID id.RoomID) *event.Event {
	return &event.Event{
		ID:     eventID,
		Sender: sender,
		RoomID: roomID,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    "!agents",
		}},
	}
}

func prepareDirectoryBot(t *testing.T, b *Bridge, roomID id.RoomID) {
	t.Helper()
	bot := b.as.BotIntent()
	bot.Registered = true
	if err := b.as.StateStore.SetMembership(context.Background(), roomID, bot.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
}

func assertDirectoryNotice(t *testing.T, content event.MessageEventContent, replyTo id.EventID) {
	t.Helper()
	if content.MsgType != event.MsgNotice {
		t.Errorf("directory msgtype = %q, want m.notice", content.MsgType)
	}
	if got := content.RelatesTo.GetReplyTo(); got != replyTo {
		t.Errorf("directory reply target = %q, want %q", got, replyTo)
	}
}
