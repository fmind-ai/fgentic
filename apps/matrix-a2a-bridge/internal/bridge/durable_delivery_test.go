package bridge

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path"
	"sync"
	"testing"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

func TestDurableMatrixOutboxUsesStableDistinctTransactionIDs(t *testing.T) {
	type recorded struct {
		transactionID string
		content       event.MessageEventContent
	}
	var mu sync.Mutex
	var requests []recorded
	eventsByTransaction := make(map[string]id.EventID)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		transactionID := path.Base(request.URL.Path)
		var content event.MessageEventContent
		if err := json.NewDecoder(request.Body).Decode(&content); err != nil {
			http.Error(w, "invalid event", http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, recorded{transactionID: transactionID, content: content})
		eventID, ok := eventsByTransaction[transactionID]
		if !ok {
			eventID = id.EventID(fmt.Sprintf("$event-%d", len(eventsByTransaction)+1))
			eventsByTransaction[transactionID] = eventID
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]id.EventID{"event_id": eventID})
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
	b := testBridge(t)
	b.as = as
	intent := as.Intent(id.NewUserID("agent-k8s", ownServer))
	intent.Registered = true
	evt, _ := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s inspect")
	evt.ID = "$origin"
	evt.RoomID = "!room:" + ownServer
	if err := as.StateStore.SetMembership(t.Context(), evt.RoomID, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}

	jobID := state.JobIDFor(evt.ID.String(), intent.UserID.String())
	replyTxn := state.MatrixTransactionIDFor(jobID, "reply")
	placeholderTxn := state.MatrixTransactionIDFor(jobID, "placeholder")
	editTxn := state.MatrixTransactionIDFor(jobID, "edit")
	first, err := b.sendDurableNotice(t.Context(), intent, evt, "done", replyTxn)
	if err != nil {
		t.Fatalf("send durable reply: %v", err)
	}
	replayed, err := b.sendDurableNotice(t.Context(), intent, evt, "done", replyTxn)
	if err != nil {
		t.Fatalf("replay durable reply: %v", err)
	}
	if replayed != first {
		t.Fatalf("replayed event ID = %q, want %q", replayed, first)
	}
	placeholder, err := b.sendDurableNotice(t.Context(), intent, evt, workingText, placeholderTxn)
	if err != nil {
		t.Fatalf("send durable placeholder: %v", err)
	}
	if _, err := b.editDurableNotice(t.Context(), intent, evt.RoomID, placeholder, "done", editTxn); err != nil {
		t.Fatalf("edit durable placeholder: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 4 {
		t.Fatalf("Matrix requests = %d, want 4: %+v", len(requests), requests)
	}
	if requests[0].transactionID != replyTxn || requests[1].transactionID != replyTxn ||
		requests[2].transactionID != placeholderTxn || requests[3].transactionID != editTxn {
		t.Fatalf("transaction IDs = %q, %q, %q, %q", requests[0].transactionID,
			requests[1].transactionID, requests[2].transactionID, requests[3].transactionID)
	}
	if replyTxn == placeholderTxn || replyTxn == editTxn || placeholderTxn == editTxn {
		t.Fatal("durable outbox stages reused a transaction ID")
	}
	if got := requests[3].content.RelatesTo.GetReplaceID(); got != placeholder {
		t.Fatalf("edit target = %q, want %q", got, placeholder)
	}
	for index, request := range requests {
		if request.content.MsgType != event.MsgNotice {
			t.Errorf("request %d msgtype = %q, want m.notice", index, request.content.MsgType)
		}
	}
}
