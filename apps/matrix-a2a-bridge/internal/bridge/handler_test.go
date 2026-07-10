package bridge

import (
	"log/slog"
	"testing"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind/matrix-a2a-bridge/internal/config"
	"github.com/fmind/matrix-a2a-bridge/internal/state"
)

const ownServer = "fgentic.fmind.ai"

func testBridge(t *testing.T) *Bridge {
	t.Helper()
	agents, err := LoadAgents(writeTemp(t, `agents:
  agent-k8s: {namespace: kagent, name: k8s-agent}
  agent-locked:
    namespace: kagent
    name: locked-agent
    allowedSenders: ["@admin:fgentic.fmind.ai"]
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	cfg := config.Config{
		ServerName: ownServer, GhostPrefix: "agent-", Concurrency: 1,
		SenderRatePerMinute: 60, SenderRateBurst: 10, RoomRatePerMinute: 60, RoomRateBurst: 10,
	}
	as := &appservice.AppService{Registration: &appservice.Registration{SenderLocalpart: "a2a-bridge"}}
	return New(cfg, as, agents, nil, state.NewMemory(), slog.Default())
}

func msgEvent(sender id.UserID, body string, mentions ...id.UserID) (*event.Event, *event.MessageEventContent) {
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	if len(mentions) > 0 {
		content.Mentions = &event.Mentions{UserIDs: mentions}
	}
	evt := &event.Event{Sender: sender, RoomID: "!room:fgentic.fmind.ai"}
	return evt, content
}

func TestResolveTargets_TypedMention(t *testing.T) {
	b := testBridge(t)
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "please check the pods",
		id.NewUserID("agent-k8s", ownServer))
	if got := b.resolveTargets(evt, msg); len(got) != 1 || got[0] != "agent-k8s" {
		t.Errorf("resolveTargets = %v, want [agent-k8s]", got)
	}
}

// SPEC §4 F6: a federated look-alike ghost must never resolve to the local agent.
func TestResolveTargets_RejectsForeignHomeserver(t *testing.T) {
	b := testBridge(t)
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "hi",
		id.NewUserID("agent-k8s", "evil.example"))
	if got := b.resolveTargets(evt, msg); len(got) != 0 {
		t.Errorf("foreign-homeserver mention resolved to %v, want none", got)
	}
}

func TestResolveTargets_PlaintextFallback(t *testing.T) {
	b := testBridge(t)
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s why is pod X down?")
	if got := b.resolveTargets(evt, msg); len(got) != 1 || got[0] != "agent-k8s" {
		t.Errorf("resolveTargets = %v, want [agent-k8s]", got)
	}
}

func TestResolveTargets_PlaintextForeignServerRejected(t *testing.T) {
	b := testBridge(t)
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s:evil.example do things")
	if got := b.resolveTargets(evt, msg); len(got) != 0 {
		t.Errorf("plaintext foreign mention resolved to %v, want none", got)
	}
}

func TestResolveTargets_SenderPolicyEnforced(t *testing.T) {
	b := testBridge(t)
	target := id.NewUserID("agent-locked", ownServer)

	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "restricted", target)
	if got := b.resolveTargets(evt, msg); len(got) != 0 {
		t.Errorf("unauthorized sender resolved to %v, want none", got)
	}
	evt, msg = msgEvent(id.NewUserID("admin", ownServer), "restricted", target)
	if got := b.resolveTargets(evt, msg); len(got) != 1 {
		t.Errorf("authorized sender resolved to %v, want [agent-locked]", got)
	}
}

func TestResolveTargets_Deduplicates(t *testing.T) {
	b := testBridge(t)
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s and again @agent-k8s",
		id.NewUserID("agent-k8s", ownServer))
	if got := b.resolveTargets(evt, msg); len(got) != 1 {
		t.Errorf("resolveTargets = %v, want a single deduplicated target", got)
	}
}

func TestStripMentions(t *testing.T) {
	b := testBridge(t)
	cases := []struct{ in, want string }{
		{"@agent-k8s why is pod X down?", "why is pod X down?"},
		{"@agent-k8s:fgentic.fmind.ai check this", "check this"},
		{"@agent-k8s", "@agent-k8s"}, // mention-only message goes through verbatim
	}
	for _, c := range cases {
		if got := b.stripMentions(c.in); got != c.want {
			t.Errorf("stripMentions(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestIsOwnUser(t *testing.T) {
	b := testBridge(t)
	cases := []struct {
		sender id.UserID
		want   bool
	}{
		{id.NewUserID("a2a-bridge", ownServer), true},
		{id.NewUserID("agent-k8s", ownServer), true},
		{id.NewUserID("alice", ownServer), false},
		{id.NewUserID("agent-k8s", "partner.example"), false}, // foreign ghost is not ours
	}
	for _, c := range cases {
		if got := b.isOwnUser(c.sender); got != c.want {
			t.Errorf("isOwnUser(%s) = %v, want %v", c.sender, got, c.want)
		}
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
		b.HandleMembership(t.Context(), evt) // must be a no-op
		_ = name
	}
}
