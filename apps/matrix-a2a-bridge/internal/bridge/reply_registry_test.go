package bridge

import (
	"testing"

	"maunium.net/go/mautrix/id"
)

func TestAgentReplyRegistryIsBoundedAndRoomScoped(t *testing.T) {
	registry := newAgentReplyRegistry(2)
	room := id.RoomID("!room:example.test")
	registry.record(agentReplyRef{room: room, event: "$one", ghost: "agent-one"})
	registry.record(agentReplyRef{room: room, event: "$two", ghost: "agent-two"})
	registry.record(agentReplyRef{room: room, event: "$three", ghost: "agent-three"})

	if _, ok := registry.lookup("$one", room); ok {
		t.Fatal("oldest reply survived bounded eviction")
	}
	if _, ok := registry.lookup("$two", "!other:example.test"); ok {
		t.Fatal("reply resolved across rooms")
	}
	if reply, ok := registry.lookup("$three", room); !ok || reply.ghost != "agent-three" {
		t.Fatalf("newest reply = (%+v, %v), want agent-three", reply, ok)
	}
}
