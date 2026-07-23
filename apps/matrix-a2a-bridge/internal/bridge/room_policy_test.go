package bridge

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func TestRoomAdmissionRequiresExactBindingAndCurrentMembership(t *testing.T) {
	b := testBridge(t)
	ref, ok := b.agents.Lookup("agent-k8s")
	if !ok {
		t.Fatal("agent-k8s fixture missing")
	}
	ctx := t.Context()
	boundRoom := id.RoomID("!room:" + ownServer)
	if reason := b.roomAdmission(ctx, ref, "agent-k8s", boundRoom); reason != "" {
		t.Fatalf("bound joined room denied: %s", reason)
	}
	if reason := b.roomAdmission(ctx, ref, "agent-k8s", "!unbound:"+ownServer); reason != errorRoomBinding {
		t.Fatalf("unbound room reason = %q, want %q", reason, errorRoomBinding)
	}
	if err := b.as.StateStore.SetMembership(
		ctx, boundRoom, id.NewUserID("agent-k8s", ownServer), event.MembershipLeave,
	); err != nil {
		t.Fatalf("remove ghost membership: %v", err)
	}
	if reason := b.roomAdmission(ctx, ref, "agent-k8s", boundRoom); reason != errorGhostMembership {
		t.Fatalf("absent membership reason = %q, want %q", reason, errorGhostMembership)
	}
}

func TestMessageCannotAmbientJoinGhost(t *testing.T) {
	client := &scriptedA2AClient{}
	b := testBridge(t)
	b.client = client
	b.runCtx = t.Context()
	ghost := id.NewUserID("agent-k8s", ownServer)
	room := id.RoomID("!room:" + ownServer)
	if err := b.as.StateStore.SetMembership(t.Context(), room, ghost, event.MembershipLeave); err != nil {
		t.Fatalf("seed absent membership: %v", err)
	}
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	evt, msg := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s do not join", ghost)
	evt.ID = "$ambient-join-probe"
	evt.Content = event.Content{Parsed: msg}

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("absent ghost reached A2A %d time(s)", client.callCount)
	}
	member, err := b.as.StateStore.GetMember(t.Context(), room, ghost)
	if err != nil || member.Membership != event.MembershipLeave {
		t.Fatalf("message changed ghost membership = (%+v, %v)", member, err)
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["terminal_stage"] != "room_authorization" ||
		audits[0]["terminal_reason"] != errorGhostMembership || audits[0]["a2a_attempted"] != false {
		t.Fatalf("ambient-join denial audit = %#v", audits)
	}
	if strings.Contains(output.String(), "do not join") {
		t.Fatal("room denial audit leaked the event body")
	}
}

func TestMatrixProjectionNeverUsesIntentAmbientJoin(t *testing.T) {
	b, intent, evt, _, recorder := pollingHarness(t, &scriptedA2AClient{})
	if err := b.as.StateStore.SetMembership(
		t.Context(), evt.RoomID, intent.UserID, event.MembershipLeave,
	); err != nil {
		t.Fatalf("seed absent projection membership: %v", err)
	}
	if _, err := sendMessageEvent(
		t.Context(), intent, evt.RoomID, event.EventMessage,
		&event.MessageEventContent{MsgType: event.MsgNotice, Body: "fixed refusal"},
	); err != nil {
		t.Fatalf("direct projection fixture: %v", err)
	}
	if membershipRequests := recorder.membershipSnapshot(); len(membershipRequests) != 0 {
		t.Fatalf("Matrix projection made ambient membership requests: %v", membershipRequests)
	}
}

func TestGhostInviteRequiresAccessManagerAndBinding(t *testing.T) {
	agents, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["!managed:fgentic.fmind.ai"]
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b := testBridge(t)
	b.agents = agents
	var output strings.Builder
	setBridgeLogOutput(b, &output)
	target := "@agent-k8s:" + ownServer

	unauthorized := membershipEvent(target, event.MembershipInvite)
	unauthorized.ID = "$unauthorized-invite"
	unauthorized.RoomID = "!managed:" + ownServer
	unauthorized.Sender = id.NewUserID("bob", ownServer)
	b.HandleMembership(t.Context(), unauthorized)

	unbound := membershipEvent(target, event.MembershipInvite)
	unbound.ID = "$unbound-invite"
	unbound.RoomID = "!other:" + ownServer
	b.HandleMembership(t.Context(), unbound)

	member, err := b.as.StateStore.GetMember(t.Context(), unauthorized.RoomID, id.UserID(target))
	if err != nil || member.Membership != event.MembershipLeave {
		t.Fatalf("rejected invite changed membership = (%+v, %v)", member, err)
	}
	if !strings.Contains(output.String(), "invite_sender_rejected") ||
		!strings.Contains(output.String(), errorRoomBinding) {
		t.Fatalf("invite denial evidence = %s", output.String())
	}
	if strings.Contains(output.String(), "content") || strings.Contains(output.String(), "body") {
		t.Fatal("invite audit included a content field")
	}
}

func TestRoomAdmissionResolvesLocalBootstrapAlias(t *testing.T) {
	roomID := id.RoomID("!dynamic:" + ownServer)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || !strings.Contains(req.URL.Path, "/directory/room/") {
			t.Errorf("unexpected alias request: %s %s", req.Method, req.URL.Path)
			http.NotFound(w, req)
			return
		}
		if strings.Contains(req.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errcode": "M_NOT_FOUND",
				"error":   "Room alias not found",
			})
			return
		}
		if strings.Contains(req.URL.Path, "unavailable") {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"errcode": "M_UNKNOWN",
				"error":   "Alias directory unavailable",
			})
			return
		}
		if strings.Contains(req.URL.Path, "malformed") {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"room_id": roomID, "servers": []string{ownServer}})
	}))
	t.Cleanup(server.Close)
	as, err := appservice.CreateFull(appservice.CreateOpts{
		Registration:     &appservice.Registration{AppToken: "test", SenderLocalpart: "a2a-bridge"},
		HomeserverDomain: ownServer,
		HomeserverURL:    server.URL,
	})
	if err != nil {
		t.Fatalf("CreateFull: %v", err)
	}
	as.HTTPClient = server.Client()
	as.DefaultHTTPRetries = 0
	agents, err := LoadAgents(writeTemp(t, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["#missing:fgentic.fmind.ai", "#managed:fgentic.fmind.ai"]
`))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	b := testBridge(t)
	b.as = as
	b.agents = agents
	ghost := id.NewUserID("agent-k8s", ownServer)
	if err := as.StateStore.SetMembership(t.Context(), roomID, ghost, event.MembershipJoin); err != nil {
		t.Fatalf("seed membership: %v", err)
	}
	ref, _ := agents.Lookup("agent-k8s")
	if reason := b.roomAdmission(t.Context(), ref, "agent-k8s", roomID); reason != "" {
		t.Fatalf("resolved alias admission reason = %q", reason)
	}
	if reason := b.roomAdmission(t.Context(), ref, "agent-k8s", "!unbound:"+ownServer); reason != errorRoomBinding {
		t.Fatalf("missing and non-matching aliases reason = %q, want %q", reason, errorRoomBinding)
	}

	resolvedAgents, err := LoadAgents(writeTemp(t, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["#managed:fgentic.fmind.ai"]
`))
	if err != nil {
		t.Fatalf("LoadAgents resolved aliases: %v", err)
	}
	resolvedRef, _ := resolvedAgents.Lookup("agent-k8s")
	if reason := b.roomAdmission(t.Context(), resolvedRef, "agent-k8s", "!unbound:"+ownServer); reason != errorRoomBinding {
		t.Fatalf("resolved non-matching aliases reason = %q, want %q", reason, errorRoomBinding)
	}

	for _, alias := range []string{"unavailable", "malformed"} {
		t.Run(alias, func(t *testing.T) {
			unavailableAgents, err := LoadAgents(writeTemp(t, `agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    allowedRooms: ["#`+alias+`:fgentic.fmind.ai"]
`))
			if err != nil {
				t.Fatalf("LoadAgents %s alias: %v", alias, err)
			}
			unavailableRef, _ := unavailableAgents.Lookup("agent-k8s")
			if reason := b.roomAdmission(t.Context(), unavailableRef, "agent-k8s", "!unbound:"+ownServer); reason != errorRoomBindingUnavailable {
				t.Fatalf("%s alias reason = %q, want %q", alias, reason, errorRoomBindingUnavailable)
			}
		})
	}
}
