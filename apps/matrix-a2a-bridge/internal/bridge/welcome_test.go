package bridge

import (
	"strings"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestBotInvitePostsOneSenderFilteredWelcome(t *testing.T) {
	client := &scriptedA2AClient{}
	b, _, original, _, recorder := pollingHarness(t, client)
	b.cfg.WelcomeEnabled = true
	recorder.enableMembershipWire()
	invite := membershipEvent("@a2a-bridge:"+ownServer, event.MembershipInvite)
	invite.RoomID = original.RoomID

	b.HandleMembership(t.Context(), invite)
	b.HandleMembership(t.Context(), invite) // a replay or re-invite must not post again

	events := recorder.snapshot()
	if len(events) != 1 {
		t.Fatalf("welcome events = %d, want exactly one", len(events))
	}
	membershipRequests := recorder.membershipSnapshot()
	if len(membershipRequests) != 2 || !strings.HasSuffix(membershipRequests[0], "/register") ||
		!strings.HasSuffix(membershipRequests[1], "/join") {
		t.Fatalf("bot invite membership requests = %#v, want register then join", membershipRequests)
	}
	welcome := events[0]
	if welcome.MsgType != event.MsgNotice || welcome.RelatesTo.GetReplyTo() != "" {
		t.Fatalf("welcome = %+v, want standalone m.notice", welcome)
	}
	for _, want := range []string{
		"Welcome to this agent room", "@agent-k8s:" + ownServer, "!ask", "!agents", "!budget", "/ask", "/agents", "/budget",
	} {
		if !strings.Contains(welcome.Body, want) {
			t.Errorf("welcome missing %q:\n%s", want, welcome.Body)
		}
	}
	if strings.Contains(welcome.Body, "@agent-locked:") || strings.Contains(welcome.Body, "@agent-slack:") {
		t.Fatalf("welcome leaked a sender-denied agent:\n%s", welcome.Body)
	}
	raw := recorder.rawSnapshot(t)
	if len(raw) != 1 || raw[0][automatedMixinKey] != true {
		t.Fatalf("welcome missing %s mixin: %#v", automatedMixinKey, raw)
	}
	if client.callCount != 0 || len(client.pollPaths) != 0 || len(client.cardPaths) != 0 {
		t.Fatalf("welcome touched A2A: calls=%d polls=%v cards=%v", client.callCount, client.pollPaths, client.cardPaths)
	}
}

func TestBotJoinUsesPreviousFullSenderForWelcome(t *testing.T) {
	b, _, original, _, recorder := pollingHarness(t, &scriptedA2AClient{})
	b.cfg.WelcomeEnabled = true
	prepareDirectoryBot(t, b, original.RoomID)
	join := membershipEvent("@a2a-bridge:"+ownServer, event.MembershipJoin)
	join.RoomID = original.RoomID
	join.Sender = id.NewUserID("a2a-bridge", ownServer)
	join.Unsigned.PrevSender = id.NewUserID("alice", ownServer)

	b.HandleMembership(t.Context(), join)

	events := recorder.snapshot()
	if len(events) != 1 || !strings.Contains(events[0].Body, "@agent-k8s:"+ownServer) ||
		strings.Contains(events[0].Body, "@agent-locked:") {
		t.Fatalf("join-event welcome did not apply previous sender policy: %#v", events)
	}
}

func TestBotJoinWithoutPreviousSenderFailsClosed(t *testing.T) {
	b, _, original, _, recorder := pollingHarness(t, &scriptedA2AClient{})
	b.cfg.WelcomeEnabled = true
	prepareDirectoryBot(t, b, original.RoomID)
	join := membershipEvent("@a2a-bridge:"+ownServer, event.MembershipJoin)
	join.RoomID = original.RoomID
	join.Sender = id.NewUserID("a2a-bridge", ownServer)

	b.HandleMembership(t.Context(), join)

	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("join without a full previous sender emitted welcome: %#v", events)
	}
	first, err := b.store.MarkRoomWelcomed(t.Context(), original.RoomID.String())
	if err != nil || !first {
		t.Fatalf("failed-closed join persisted room marker = (%v, %v)", first, err)
	}
}

func TestRoomWelcomeUsesNoticePlaneAndMarksSuppression(t *testing.T) {
	b, _, original, _, recorder := pollingHarness(t, &scriptedA2AClient{})
	b.cfg.WelcomeEnabled = true
	prepareDirectoryBot(t, b, original.RoomID)
	// Keep sender capacity available so the welcome is suppressed specifically by the room bucket.
	b.noticeSenderLimits = newLimiters(60, 10, testRateLimitBucketCapacity)
	b.noticeRoomLimits = newLimiters(1, 1, testRateLimitBucketCapacity)
	sender := b.agents.IdentifySender(id.NewUserID("alice", ownServer))
	if !b.allowNotice(sender, original.RoomID, roomWelcomeNoticeScope) {
		t.Fatal("failed to exhaust welcome notice fixture")
	}

	invite := membershipEvent("@a2a-bridge:"+ownServer, event.MembershipInvite)
	invite.RoomID = original.RoomID
	b.HandleMembership(t.Context(), invite)
	if events := recorder.snapshot(); len(events) != 0 {
		t.Fatalf("exhausted notice plane emitted welcome: %#v", events)
	}
	first, err := b.store.MarkRoomWelcomed(t.Context(), original.RoomID.String())
	if err != nil || first {
		t.Fatalf("suppressed welcome marker = (%v, %v), want (false, nil)", first, err)
	}
}

func TestRoomWelcomeIsConfigGatedAndBotOwned(t *testing.T) {
	tests := []struct {
		name    string
		target  string
		enabled bool
	}{
		{name: "disabled bot", target: "@a2a-bridge:" + ownServer},
		{name: "mapped ghost", target: "@agent-k8s:" + ownServer, enabled: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b, _, original, _, recorder := pollingHarness(t, &scriptedA2AClient{})
			b.cfg.WelcomeEnabled = tt.enabled
			prepareDirectoryBot(t, b, original.RoomID)
			invite := membershipEvent(tt.target, event.MembershipInvite)
			invite.RoomID = original.RoomID
			b.HandleMembership(t.Context(), invite)
			if events := recorder.snapshot(); len(events) != 0 {
				t.Fatalf("unexpected welcome events: %#v", events)
			}
			first, err := b.store.MarkRoomWelcomed(t.Context(), original.RoomID.String())
			if err != nil || !first {
				t.Fatalf("non-welcome path persisted room marker = (%v, %v)", first, err)
			}
		})
	}
}

func TestRoomWelcomeTransactionIDIsStableAndRoomScoped(t *testing.T) {
	one := roomWelcomeTransactionID("!one:" + ownServer)
	if one != roomWelcomeTransactionID("!one:"+ownServer) {
		t.Fatal("same room produced different welcome transaction IDs")
	}
	if one == roomWelcomeTransactionID("!two:"+ownServer) {
		t.Fatal("different rooms produced the same welcome transaction ID")
	}
	if strings.ContainsAny(one, "!:/") || !strings.HasPrefix(one, "fgentic-room-welcome-") {
		t.Fatalf("unsafe welcome transaction ID %q", one)
	}
}
