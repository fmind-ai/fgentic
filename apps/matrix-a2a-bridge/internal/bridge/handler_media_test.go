package bridge

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
)

// uploadedBlob is one media upload the ghost performed against the fake content repository.
type uploadedBlob struct {
	contentType string
	body        []byte
}

// mediaFixture backs the Matrix content repository + event fetch for media tests: it records what the
// bridge uploaded and serves seeded downloads and replied-to events.
type mediaFixture struct {
	mu        sync.Mutex
	uploads   []uploadedBlob
	downloads map[string][]byte     // mediaID -> bytes
	events    map[id.EventID][]byte // eventID -> raw event JSON for GetEvent
	nextMXC   int
}

func (f *mediaFixture) seedDownload(mediaID string, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.downloads[mediaID] = data
}

func (f *mediaFixture) seedMediaEvent(eventID id.EventID, roomID id.RoomID, mime, filename, mxcID string, size int) {
	raw, _ := json.Marshal(map[string]any{
		"type":     "m.room.message",
		"event_id": eventID.String(),
		"room_id":  roomID.String(),
		"sender":   "@alice:" + ownServer,
		"content": map[string]any{
			"msgtype":  "m.file",
			"body":     filename,
			"filename": filename,
			"url":      "mxc://" + ownServer + "/" + mxcID,
			"info":     map[string]any{"mimetype": mime, "size": size},
		},
	})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events[eventID] = raw
}

func (f *mediaFixture) uploadSnapshot() []uploadedBlob {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uploadedBlob(nil), f.uploads...)
}

// mediaHarness builds a bridge wired to a Matrix server that also serves media upload/download and
// event fetch, with the media policy enabled. It mirrors pollingHarness's appservice + membership so
// ghost registration/join short-circuit off the state store.
func mediaHarness(t *testing.T, client a2aClient) (*Bridge, *event.Event, *matrixRecorder, *mediaFixture) {
	t.Helper()
	recorder := &matrixRecorder{}
	fixture := &mediaFixture{downloads: map[string][]byte{}, events: map[id.EventID][]byte{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		path := req.URL.Path
		switch {
		case req.Method == http.MethodPost && strings.Contains(path, "/upload"):
			body, _ := io.ReadAll(req.Body)
			fixture.mu.Lock()
			fixture.nextMXC++
			mxc := fixture.nextMXC
			fixture.uploads = append(fixture.uploads, uploadedBlob{contentType: req.Header.Get("Content-Type"), body: body})
			fixture.mu.Unlock()
			writeJSON(t, w, map[string]string{"content_uri": "mxc://" + ownServer + "/up" + strconv.Itoa(mxc)})
		case req.Method == http.MethodGet && strings.Contains(path, "/media/download/"):
			seg := path[strings.LastIndex(path, "/")+1:]
			fixture.mu.Lock()
			data, ok := fixture.downloads[seg]
			fixture.mu.Unlock()
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write(data)
		case req.Method == http.MethodGet && strings.Contains(path, "/event/"):
			seg := id.EventID(path[strings.LastIndex(path, "/")+1:])
			fixture.mu.Lock()
			raw, ok := fixture.events[seg]
			fixture.mu.Unlock()
			if !ok {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		case req.Method == http.MethodPut && strings.Contains(path, "/send/m.room.message/"):
			body, _ := io.ReadAll(req.Body)
			var content event.MessageEventContent
			if err := json.Unmarshal(body, &content); err != nil {
				t.Errorf("decode Matrix event: %v", err)
				http.Error(w, "invalid", http.StatusBadRequest)
				return
			}
			writeJSON(t, w, map[string]id.EventID{"event_id": recorder.append(content, body)})
		default:
			// Housekeeping (typing, registration, presence): succeed quietly so intent bookkeeping
			// never fails a media test on an unrelated call.
			_, _ = w.Write([]byte("{}"))
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
	b.media = newMediaPolicy(mediaTestConfig())

	intent := as.Intent(id.NewUserID("agent-k8s", ownServer))
	intent.Registered = true
	if err := as.StateStore.SetMembership(t.Context(), "!room:"+ownServer, intent.UserID, event.MembershipJoin); err != nil {
		t.Fatalf("SetMembership: %v", err)
	}
	evt, _ := msgEvent(id.NewUserID("alice", ownServer), "@agent-k8s inspect the pod", id.NewUserID("agent-k8s", ownServer))
	evt.ID = "$original"
	evt.Type = event.EventMessage
	return b, evt, recorder, fixture
}

func mediaTestConfig() config.Config {
	return config.Config{
		MediaMIMEAllowlist: []string{"text/csv", "application/json", "image/png"},
		MediaMaxBytes:      1024,
		MediaMaxTotalBytes: 4096,
	}
}

func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// --- Outbound: agent artifacts -> Matrix media ---

func TestDeliverReplyPostsAllowedArtifactAsMedia(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text:     "here is your report",
		Terminal: true,
		Files:    []a2aclient.ResultFile{{Name: "report.csv", MIMEType: "text/csv", Bytes: []byte("a,b\n1,2")}},
	}}
	b, evt, recorder, fixture := mediaHarness(t, client)
	ref, _ := b.agents.Lookup("agent-k8s")

	b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "make a report", b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

	if ups := fixture.uploadSnapshot(); len(ups) != 1 || string(ups[0].body) != "a,b\n1,2" || ups[0].contentType != "text/csv" {
		t.Fatalf("uploads = %+v, want one text/csv upload", ups)
	}
	events := recorder.snapshot()
	var notice, file *event.MessageEventContent
	for i := range events {
		switch events[i].MsgType {
		case event.MsgNotice:
			notice = &events[i]
		case event.MsgFile:
			file = &events[i]
		}
	}
	if notice == nil || !strings.Contains(notice.Body, "here is your report") {
		t.Fatalf("missing text notice: %+v", events)
	}
	if file == nil || file.FileName != "report.csv" || file.URL == "" || file.Info == nil || file.Info.MimeType != "text/csv" {
		t.Fatalf("missing/incorrect m.file event: %+v", events)
	}
}

func TestDeliverReplyWithholdsDisallowedArtifact(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text:     "done",
		Terminal: true,
		Files: []a2aclient.ResultFile{
			{Name: "ok.csv", MIMEType: "text/csv", Bytes: []byte("x")},
			{Name: "evil.html", MIMEType: "text/html", Bytes: []byte("<script>")},
		},
	}}
	b, evt, recorder, fixture := mediaHarness(t, client)
	ref, _ := b.agents.Lookup("agent-k8s")

	b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "go", b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

	if ups := fixture.uploadSnapshot(); len(ups) != 1 {
		t.Fatalf("uploads = %d, want only the allowed csv", len(ups))
	}
	if !messageBodyContains(recorder, "1 attached file(s) withheld") || !messageBodyContains(recorder, "disallowed_type") {
		t.Fatalf("reply missing withheld notice: %#v", recorder.snapshot())
	}
}

func TestDeliverReplyRendersDataAndUntrustedLinks(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text:     "results:",
		Terminal: true,
		Data:     []string{`{"rows":2}`},
		Links:    []a2aclient.ResultLink{{Label: "source", URL: "https://example.test/x"}},
	}}
	b, evt, recorder, _ := mediaHarness(t, client)
	ref, _ := b.agents.Lookup("agent-k8s")

	b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "go", b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

	if !messageBodyContains(recorder, "```json") || !messageBodyContains(recorder, `{"rows":2}`) {
		t.Fatalf("reply missing data code block: %#v", recorder.snapshot())
	}
	if !messageBodyContains(recorder, "untrusted link (not fetched)") || !messageBodyContains(recorder, "https://example.test/x") {
		t.Fatalf("reply missing untrusted link: %#v", recorder.snapshot())
	}
}

// --- Inbound: Matrix media -> A2A part ---

func mediaMentionEvent(mime, filename, mxcID string, size int) *event.Event {
	content := &event.MessageEventContent{
		MsgType:  event.MsgFile,
		Body:     filename,
		FileName: filename,
		URL:      id.ContentURIString("mxc://" + ownServer + "/" + mxcID),
		Info:     &event.FileInfo{MimeType: mime, Size: size},
		Mentions: &event.Mentions{UserIDs: []id.UserID{id.NewUserID("agent-k8s", ownServer)}},
	}
	evt := &event.Event{
		Sender:  id.NewUserID("alice", ownServer),
		RoomID:  "!room:" + ownServer,
		Type:    event.EventMessage,
		Content: event.Content{Parsed: content},
	}
	evt.ID = "$file-mention"
	return evt
}

func TestInboundMediaForwardedToAgent(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "processed", Terminal: true}}
	b, _, _, fixture := mediaHarness(t, client)
	data := []byte("h1,h2\n1,2")
	fixture.seedDownload("csv1", data)
	evt := mediaMentionEvent("text/csv", "input.csv", "csv1", len(data))

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 1 {
		t.Fatalf("A2A calls = %d, want 1", client.callCount)
	}
	if len(client.callFiles) != 1 || client.callFiles[0].Name != "input.csv" || string(client.callFiles[0].Bytes) != string(data) {
		t.Fatalf("forwarded inbound files = %+v", client.callFiles)
	}
}

func TestInboundDisallowedMediaRefusesDelegation(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "processed", Terminal: true}}
	b, _, recorder, fixture := mediaHarness(t, client)
	fixture.seedDownload("bad1", []byte("<script>"))
	evt := mediaMentionEvent("text/html", "evil.html", "bad1", 8)
	var output strings.Builder
	setBridgeLogOutput(b, &output)

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("disallowed inbound media reached A2A: %d calls", client.callCount)
	}
	if !messageBodyContains(recorder, "refused by the media policy") {
		t.Fatalf("missing refusal notice: %#v", recorder.snapshot())
	}
	audits := auditRecords(t, output.String())
	if len(audits) != 1 || audits[0]["outcome"] != outcomeDenied || audits[0]["terminal_reason"] != "media_input_rejected" {
		t.Fatalf("audit = %#v, want denied/media_input_rejected", audits)
	}
}

func TestInboundOversizedActualBytesRefused(t *testing.T) {
	// The declared info.size is small but the real blob exceeds MEDIA_MAX_BYTES (1024). The bounded
	// download must reject it rather than trust the attacker-controlled declared size.
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "ok", Terminal: true}}
	b, _, recorder, fixture := mediaHarness(t, client)
	fixture.seedDownload("big1", make([]byte, 4096))
	evt := mediaMentionEvent("text/csv", "small.csv", "big1", 10) // lies: declares 10 bytes

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("oversized inbound media reached A2A: %d calls", client.callCount)
	}
	if !messageBodyContains(recorder, "refused by the media policy") {
		t.Fatalf("missing refusal notice: %#v", recorder.snapshot())
	}
}

func TestInboundHTMLSniffRefused(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "ok", Terminal: true}}
	b, _, _, fixture := mediaHarness(t, client)
	fixture.seedDownload("html1", []byte("<html><body><script>alert(1)</script></body></html>"))
	evt := mediaMentionEvent("text/csv", "innocent.csv", "html1", 51) // mislabeled HTML

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 0 {
		t.Fatalf("HTML-sniffed inbound media reached A2A: %d calls", client.callCount)
	}
}

func TestOutboundHTMLSniffWithheld(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{
		Text:     "done",
		Terminal: true,
		Files:    []a2aclient.ResultFile{{Name: "report.csv", MIMEType: "text/csv", Bytes: []byte("<html><body>x</body></html>")}},
	}}
	b, evt, recorder, fixture := mediaHarness(t, client)
	ref, _ := b.agents.Lookup("agent-k8s")

	b.dispatchWithDedupVerdict(t.Context(), evt, ref, "agent-k8s", "go", b.agents.IdentifySender(evt.Sender), dedupVerdictAccepted)

	if ups := fixture.uploadSnapshot(); len(ups) != 0 {
		t.Fatalf("HTML-sniffed artifact was uploaded: %+v", ups)
	}
	if !messageBodyContains(recorder, "withheld") {
		t.Fatalf("missing withheld notice: %#v", recorder.snapshot())
	}
}

func TestInboundMediaViaReplyToFile(t *testing.T) {
	client := &scriptedA2AClient{callResult: a2aclient.Result{Text: "ok", Terminal: true}}
	b, _, _, fixture := mediaHarness(t, client)
	data := []byte("col\nval")
	fixture.seedDownload("csv2", data)
	fixture.seedMediaEvent("$parentfile", "!room:"+ownServer, "text/csv", "attached.csv", "csv2", len(data))

	// A text mention that replies to the earlier file event.
	content := &event.MessageEventContent{
		MsgType:  event.MsgText,
		Body:     "@agent-k8s summarize this",
		Mentions: &event.Mentions{UserIDs: []id.UserID{id.NewUserID("agent-k8s", ownServer)}},
		RelatesTo: &event.RelatesTo{
			InReplyTo: &event.InReplyTo{EventID: "$parentfile"},
		},
	}
	evt := &event.Event{Sender: id.NewUserID("alice", ownServer), RoomID: "!room:" + ownServer, Type: event.EventMessage, Content: event.Content{Parsed: content}}
	evt.ID = "$reply-mention"

	b.HandleMessage(t.Context(), evt)
	b.dispatcher.Wait()

	if client.callCount != 1 || len(client.callFiles) != 1 || client.callFiles[0].Name != "attached.csv" {
		t.Fatalf("reply-to-file inbound = calls %d files %+v", client.callCount, client.callFiles)
	}
}
