package bridge

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

type recordingSessionPurger struct {
	mu     sync.Mutex
	calls  []purgeCall
	err    error
	called chan struct{}
}

type purgeCall struct {
	contextID string
	owners    []string
}

func (p *recordingSessionPurger) Purge(_ context.Context, contextID string, owners []string) error {
	p.mu.Lock()
	p.calls = append(p.calls, purgeCall{contextID: contextID, owners: slices.Clone(owners)})
	p.mu.Unlock()
	if p.called != nil {
		select {
		case p.called <- struct{}{}:
		default:
		}
	}
	return p.err
}

func (p *recordingSessionPurger) snapshot() []purgeCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.calls)
}

func TestForgetCommandPurgesEveryOwnerBeforeReset(t *testing.T) {
	b, _, evt, _, recorder := pollingHarness(t, &scriptedA2AClient{})
	purger := &recordingSessionPurger{}
	b.SetSessionPurger(purger)
	prepareDirectoryBot(t, b, evt.RoomID)
	if err := b.store.SetContext(t.Context(), evt.RoomID.String(), "agent-k8s", "ctx-1", "@alice:"+ownServer); err != nil {
		t.Fatal(err)
	}
	if err := b.store.AddContextOwner(t.Context(), evt.RoomID.String(), "agent-k8s", "ctx-1", "@bob:"+ownServer); err != nil {
		t.Fatal(err)
	}
	evt.ID = "$forget"
	evt.Content.Parsed = &event.MessageEventContent{MsgType: event.MsgText, Body: "!forget k8s"}

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	calls := purger.snapshot()
	wantOwners := []string{"@alice:" + ownServer, "@bob:" + ownServer}
	if len(calls) != 1 || calls[0].contextID != "ctx-1" || !slices.Equal(calls[0].owners, wantOwners) {
		t.Fatalf("purge calls = %#v, want ctx-1 owners %v", calls, wantOwners)
	}
	if contextID, err := b.store.Context(t.Context(), evt.RoomID.String(), "agent-k8s"); err != nil || contextID != "" {
		t.Fatalf("stored context after forget = %q, %v", contextID, err)
	}
	if !messageBodyContains(recorder, "Matrix room history and federated copies were not deleted") {
		t.Fatalf("forget confirmation did not state Matrix/federation limit: %#v", recorder.snapshot())
	}
}

func TestForgetKeepsContextWhenBackendDeletionFails(t *testing.T) {
	b, _, evt, _, _ := pollingHarness(t, &scriptedA2AClient{})
	b.SetSessionPurger(&recordingSessionPurger{err: errors.New("unavailable")})
	if err := b.store.SetContext(t.Context(), evt.RoomID.String(), "agent-k8s", "ctx-1", evt.Sender.String()); err != nil {
		t.Fatal(err)
	}
	message := b.forgetConversation(t.Context(), evt.RoomID.String(), "agent-k8s")
	if !strings.Contains(message, "kept the context") {
		t.Fatalf("forget failure message = %q", message)
	}
	if contextID, _ := b.store.Context(t.Context(), evt.RoomID.String(), "agent-k8s"); contextID != "ctx-1" {
		t.Fatalf("stored context after failed purge = %q, want ctx-1", contextID)
	}
}

type incompleteOwnerStore struct {
	state.Store
	conversation state.Conversation
}

func (s *incompleteOwnerStore) Conversation(context.Context, string, string) (state.Conversation, bool, error) {
	return s.conversation, true, nil
}

func TestForgetRejectsPreGovernanceOwnerInventory(t *testing.T) {
	b, _, evt, _, _ := pollingHarness(t, &scriptedA2AClient{})
	if err := b.store.SetContext(t.Context(), evt.RoomID.String(), "agent-k8s", "ctx-legacy", evt.Sender.String()); err != nil {
		t.Fatal(err)
	}
	conversation, found, err := b.store.Conversation(t.Context(), evt.RoomID.String(), "agent-k8s")
	if err != nil || !found {
		t.Fatalf("Conversation() = %#v, %v, %v", conversation, found, err)
	}
	conversation.OwnersComplete = false
	b.store = &incompleteOwnerStore{Store: b.store, conversation: conversation}
	purger := &recordingSessionPurger{}
	b.SetSessionPurger(purger)

	message := b.forgetConversation(t.Context(), evt.RoomID.String(), "agent-k8s")
	if !strings.Contains(message, "predates owner tracking") {
		t.Fatalf("forget legacy message = %q", message)
	}
	if calls := purger.snapshot(); len(calls) != 0 {
		t.Fatalf("legacy purge calls = %#v, want none", calls)
	}
}

func TestConversationRetentionSweepPurgesExpiredLocalContext(t *testing.T) {
	b, _, evt, _, _ := pollingHarness(t, &scriptedA2AClient{})
	agents, err := LoadAgents(writeTemp(t, `schemaVersion: 1
agents:
  agent-k8s:
    namespace: kagent
    name: k8s-agent
    maxSessionAge: 1ns
`))
	if err != nil {
		t.Fatal(err)
	}
	b.agents = agents
	b.cfg.ConversationSweepInterval = time.Hour
	purger := &recordingSessionPurger{called: make(chan struct{}, 1)}
	b.SetSessionPurger(purger)
	if err := b.store.SetContext(t.Context(), evt.RoomID.String(), "agent-k8s", "ctx-expired", evt.Sender.String()); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	b.watchWG.Add(1)
	go b.runConversationRetention(ctx)
	select {
	case <-purger.called:
	case <-time.After(time.Second):
		t.Fatal("retention sweep did not purge expired context")
	}
	cancel()
	b.watchWG.Wait()
	if contextID, _ := b.store.Context(t.Context(), evt.RoomID.String(), "agent-k8s"); contextID != "" {
		t.Fatalf("stored context after retention = %q", contextID)
	}
}

func TestRemoteMaxSessionAgeIsRejected(t *testing.T) {
	yaml := strings.Replace(validRemoteAgentsYAML, "    timeout: 12s\n", "    timeout: 12s\n    maxSessionAge: 24h\n", 1)
	if _, err := LoadAgents(writeTemp(t, yaml)); err == nil || !strings.Contains(err.Error(), "only valid for a local target") {
		t.Fatalf("LoadAgents() error = %v", err)
	}
}
