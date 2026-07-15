package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestReplayEventPreservesCanonicalMatrixEvent(t *testing.T) {
	const (
		roomID        = "!room:integration.test"
		eventID       = "$event:integration.test"
		sender        = "@user:integration.test"
		transactionID = "canonical-redelivery"
		originTS      = int64(1_700_000_123_456)
	)
	content := json.RawMessage(`{"body":"exact replay","custom":{"keep":true},"msgtype":"m.text"}`)
	received := make(chan matrixEvent, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/event/"):
			if r.Header.Get("Authorization") != "Bearer matrix-token" {
				http.Error(w, "missing Matrix token", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"content":          content,
				"event_id":         eventID,
				"origin_server_ts": originTS,
				"sender":           sender,
				"type":             "m.room.message",
			})
		case r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/transactions/"):
			if r.Header.Get("Authorization") != "Bearer homeserver-token" {
				http.Error(w, "missing homeserver token", http.StatusUnauthorized)
				return
			}
			var transaction struct {
				Events []matrixEvent `json:"events"`
			}
			if err := json.NewDecoder(r.Body).Decode(&transaction); err != nil || len(transaction.Events) != 1 {
				http.Error(w, "invalid appservice transaction", http.StatusBadRequest)
				return
			}
			received <- transaction.Events[0]
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	f := fixture{
		matrixURL: server.URL,
		bridgeURL: server.URL,
		hsToken:   "homeserver-token",
		http:      server.Client(),
	}
	if err := f.replayEvent(t.Context(), "matrix-token", transactionID, roomID, eventID); err != nil {
		t.Fatalf("replayEvent() error = %v", err)
	}
	evt := <-received
	if evt.EventID != eventID || evt.RoomID != roomID || evt.Sender != sender ||
		evt.Type != "m.room.message" || evt.OriginServerTS != originTS || string(evt.Content) != string(content) {
		t.Fatalf("replayed event = %+v, content=%s", evt, evt.Content)
	}
}

func TestReplayEventRejectsMissingOrMismatchedIdentity(t *testing.T) {
	const (
		roomID  = "!room:integration.test"
		eventID = "$event:integration.test"
	)
	tests := []struct {
		name    string
		event   matrixEvent
		wantErr string
	}{
		{
			name: "missing event ID",
			event: matrixEvent{
				Content:        json.RawMessage(`{"body":"replay","msgtype":"m.text"}`),
				OriginServerTS: 1,
				Sender:         "@user:integration.test",
				Type:           "m.room.message",
			},
			wantErr: "event_id is empty",
		},
		{
			name: "mismatched event ID",
			event: matrixEvent{
				Content:        json.RawMessage(`{"body":"replay","msgtype":"m.text"}`),
				EventID:        "$other:integration.test",
				OriginServerTS: 1,
				Sender:         "@user:integration.test",
				Type:           "m.room.message",
			},
			wantErr: "does not match",
		},
		{
			name: "mismatched room ID",
			event: matrixEvent{
				Content:        json.RawMessage(`{"body":"replay","msgtype":"m.text"}`),
				EventID:        eventID,
				OriginServerTS: 1,
				RoomID:         "!other:integration.test",
				Sender:         "@user:integration.test",
				Type:           "m.room.message",
			},
			wantErr: "room_id",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var bridgeCalls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(w).Encode(tt.event)
					return
				}
				bridgeCalls.Add(1)
				_, _ = w.Write([]byte(`{}`))
			}))
			defer server.Close()

			f := fixture{
				matrixURL: server.URL,
				bridgeURL: server.URL,
				hsToken:   "homeserver-token",
				http:      server.Client(),
			}
			err := f.replayEvent(t.Context(), "matrix-token", "redelivery", roomID, eventID)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("replayEvent() error = %v, want %q", err, tt.wantErr)
			}
			if calls := bridgeCalls.Load(); calls != 0 {
				t.Fatalf("bridge calls = %d, want 0", calls)
			}
		})
	}
}
