package bridge

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestAgentDirectoryListsOnlyAgentsVisibleToSenderWithoutA2A(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	joinGhostForTest(t, b, evt.RoomID, "agent-locked")
	b.profiles.set("agent-k8s", agentProfile{
		DisplayName: "Kubernetes Specialist",
		Description: "Diagnoses cluster health from the live AgentCard.",
		Skills:      []string{"Inspect workloads", "Explain failures"},
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
		!strings.Contains(events[0].Body, "Diagnoses cluster health from the live AgentCard.") ||
		!strings.Contains(events[0].Body, "skills: Inspect workloads, Explain failures") {
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

func TestAgentDirectoryDetailUsesCachedSkillsWithoutA2A(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, evt, _, recorder := pollingHarness(t, client)
	prepareDirectoryBot(t, b, evt.RoomID)
	b.profiles.set("agent-k8s", agentProfile{
		DisplayName: "Kubernetes Specialist",
		Description: "Diagnoses cluster health.",
		Skills: []string{
			"Inspect workloads", "Explain failures", "Review events", "Check capacity", "Trace ownership",
			"Compare rollout", "Summarize health", "Suggest recovery", "Inspect policies", "Review limits",
			"Hidden overflow skill",
		},
		AgentPath: "/api/a2a/kagent/k8s-agent",
		Status:    profileStatusLive,
	})

	detail := directoryEventWithBody(
		"$agents-detail", id.NewUserID("alice", ownServer), evt.RoomID, "!agents k8s",
	)
	b.HandleMessage(t.Context(), detail)
	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("detail events = %d, want one", len(events))
	}
	body := events[0].Body
	for _, want := range []string{
		"Kubernetes Specialist", "@agent-k8s:" + ownServer, "Type: local", "AgentCard live",
		"Diagnoses cluster health.", "Inspect workloads", "(+1 more)",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("detail missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "Hidden overflow skill") {
		t.Fatalf("detail exceeded skill bound:\n%s", body)
	}
	raw := recorder.rawSnapshot(t)
	if len(raw) != 1 || raw[0][automatedMixinKey] != true {
		t.Fatalf("detail missing %s mixin: %#v", automatedMixinKey, raw)
	}
	if client.callCount != 0 || len(client.pollPaths) != 0 || len(client.cardPaths) != 0 {
		t.Fatalf("detail touched A2A: calls=%d polls=%v cards=%v", client.callCount, client.pollPaths, client.cardPaths)
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
	body := b.agentDirectoryText(t.Context(), sender, "!room:"+ownServer)
	if !strings.Contains(body, "No mapped agents are available") || strings.Contains(body, "@agent-") {
		t.Fatalf("directory for denied federated sender = %q", body)
	}
}

func TestAgentDirectoryMarksRemoteUnavailableWhenVerifiedFreshnessExpires(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	client := &scriptedA2AClient{remoteReady: false}
	b := testBridge(t)
	b.agents = agents
	b.client = client
	b.profiles = newProfileStore(agents.Entries())
	joinGhostForTest(t, b, "!room:"+ownServer, "agent-remote")
	ref, _ := agents.Lookup("agent-remote")
	profile := fallbackProfile(ref)
	profile.Status = profileStatusLive
	profile.Description = "Stale signed purpose must be hidden."
	profile.Skills = []string{"Stale signed capability"}
	b.profiles.set("agent-remote", profile)
	sender := id.NewUserID("alice", ownServer)

	if body := b.agentDirectoryText(t.Context(), sender, "!room:"+ownServer); !containsAll(body, "@agent-remote:"+ownServer, "remote", "unavailable", "capabilities hidden") ||
		strings.Contains(body, "Stale signed purpose") || strings.Contains(body, "Stale signed capability") {
		t.Fatalf("directory unavailable remote entry = %s", body)
	}
	if body := b.agentDirectoryDetailText(t.Context(), sender, "!room:"+ownServer, "remote"); !containsAll(body, "Type: remote", "unavailable", "Capabilities: hidden") ||
		strings.Contains(body, "Stale signed capability") {
		t.Fatalf("detail unavailable remote entry = %s", body)
	}
	client.remoteReady = true
	if body := b.agentDirectoryText(t.Context(), sender, "!room:"+ownServer); !containsAll(
		body, "@agent-remote:"+ownServer, "remote", "Stale signed purpose", "Stale signed capability",
	) {
		t.Fatalf("directory ready remote entry = %s", body)
	}
	if body := b.agentDirectoryDetailText(t.Context(), sender, "!room:"+ownServer, "agent-remote"); !containsAll(
		body, "Type: remote", "AgentCard live", "Stale signed purpose", "Stale signed capability",
	) {
		t.Fatalf("detail ready remote entry = %s", body)
	}
}

func TestAgentDirectoryDetailRejectsForeignOrDeniedLookup(t *testing.T) {
	b := testBridge(t)
	sender := id.NewUserID("alice", ownServer)
	for _, query := range []string{"@agent-k8s:partner.example", "agent-locked", "missing"} {
		body := b.agentDirectoryDetailText(t.Context(), sender, "!room:"+ownServer, query)
		if !containsAll(body, "No invocable agent named", "Run !agents") || strings.Contains(body, "Restricted Specialist") {
			t.Errorf("detail for %q = %q", query, body)
		}
	}
}

func TestAgentDirectorySummaryBoundsAgentsAndMetadata(t *testing.T) {
	var agentsYAML strings.Builder
	agentsYAML.WriteString("agents:\n")
	for index := range 25 {
		fmt.Fprintf(
			&agentsYAML,
			"  agent-gallery-%02d: {namespace: kagent, name: gallery-%02d, allowedRooms: [\"!room:%s\"]}\n",
			index,
			index,
			ownServer,
		)
	}
	agents, err := LoadAgents(writeTemp(t, agentsYAML.String()))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b := testBridge(t)
	b.agents = agents
	b.profiles = newProfileStore(agents.Entries())
	for _, entry := range agents.Entries() {
		joinGhostForTest(t, b, "!room:"+ownServer, entry.Ghost)
		b.profiles.set(entry.Ghost, agentProfile{
			DisplayName: entry.Ghost,
			Description: strings.Repeat("long description ", 40),
			Skills:      []string{"one", "two", "three", "four"},
			Status:      profileStatusLive,
		})
	}
	body := b.agentDirectoryText(t.Context(), id.NewUserID("alice", ownServer), "!room:"+ownServer)
	if !strings.Contains(body, "… 5 more authorized agent(s)") {
		t.Fatalf("bounded directory omitted overflow summary:\n%s", body)
	}
	if strings.Contains(body, "agent-gallery-24") || len([]rune(body)) > 12_000 {
		t.Fatalf("directory output is not bounded: runes=%d", len([]rune(body)))
	}
}

func TestAgentDirectoryForBridgedSenderListsOnlyExplicitMappings(t *testing.T) {
	b := testBridge(t)
	body := b.agentDirectoryText(t.Context(), id.NewUserID("slack_U123", ownServer), "!room:"+ownServer)
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
	return directoryEventWithBody(eventID, sender, roomID, agentDirectoryCommand)
}

func directoryEventWithBody(eventID id.EventID, sender id.UserID, roomID id.RoomID, body string) *event.Event {
	return &event.Event{
		ID:     eventID,
		Sender: sender,
		RoomID: roomID,
		Content: event.Content{Parsed: &event.MessageEventContent{
			MsgType: event.MsgText,
			Body:    body,
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
