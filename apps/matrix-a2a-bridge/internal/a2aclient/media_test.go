package a2aclient

import (
	"context"
	"iter"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"
	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/a2aproject/a2a-go/v2/a2asrv/taskstore"
)

func rawPart(name, mime string, data []byte) *a2a.Part {
	p := a2a.NewRawPart(data)
	p.Filename = name
	p.MediaType = mime
	return p
}

func TestExtractPartsBuckets(t *testing.T) {
	url := a2a.NewFileURLPart(a2a.URL("https://example.test/report"), "application/pdf")
	url.Filename = "report.pdf"
	parts := a2a.ContentParts{
		a2a.NewTextPart("summary text"),
		rawPart("data.csv", "text/csv", []byte("a,b\n1,2")),
		a2a.NewDataPart(map[string]any{"rows": 2}),
		url,
	}
	files, data, links := extractParts(parts)
	if len(files) != 1 || files[0].Name != "data.csv" || files[0].MIMEType != "text/csv" || string(files[0].Bytes) != "a,b\n1,2" {
		t.Fatalf("files = %+v", files)
	}
	if len(data) != 1 || data[0] != `{"rows":2}` {
		t.Fatalf("data = %+v", data)
	}
	if len(links) != 1 || links[0].URL != "https://example.test/report" || links[0].MIMEType != "application/pdf" {
		t.Fatalf("links = %+v", links)
	}
}

func TestTaskResultExtractsArtifactsOnlyWhenTerminal(t *testing.T) {
	artifact := &a2a.Artifact{Parts: a2a.ContentParts{
		a2a.NewTextPart("here is the file"),
		rawPart("out.csv", "text/csv", []byte("x")),
	}}

	done := &a2a.Task{
		ID: "t1", ContextID: "c1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateCompleted},
		Artifacts: []*a2a.Artifact{artifact},
	}
	r := taskResult(done)
	if !r.Terminal || len(r.Files) != 1 || r.Files[0].Name != "out.csv" {
		t.Fatalf("terminal task result files = %+v (terminal=%v)", r.Files, r.Terminal)
	}

	// A still-working task must not surface artifact files: they are only trustworthy once terminal.
	working := &a2a.Task{
		ID: "t1", ContextID: "c1",
		Status:    a2a.TaskStatus{State: a2a.TaskStateWorking},
		Artifacts: []*a2a.Artifact{artifact},
	}
	if wr := taskResult(working); len(wr.Files) != 0 {
		t.Fatalf("working task leaked artifact files: %+v", wr.Files)
	}
}

func TestCallAttachesInboundFilesAsRawParts(t *testing.T) {
	recorder := &contractRecorder{}
	client := contractServer(t, executorFunc(func(_ context.Context, execCtx *a2asrv.ExecutorContext) iter.Seq2[a2a.Event, error] {
		recorder.recordExecution(execCtx)
		return func(yield func(a2a.Event, error) bool) {
			yield(a2a.NewMessageForTask(a2a.MessageRoleAgent, execCtx), nil)
		}
	}), taskstore.NewInMemory(nil), recorder)

	files := []InboundFile{{Name: "in.csv", MIMEType: "text/csv", Bytes: []byte("h1,h2\n1,2")}}
	if _, err := client.Call(t.Context(), contractTarget(t), "process this", "", files); err != nil {
		t.Fatalf("Call: %v", err)
	}

	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	if len(recorder.messages) != 1 {
		t.Fatalf("executor saw %d messages, want 1", len(recorder.messages))
	}
	var sawText, sawFile bool
	for _, p := range recorder.messages[0].Parts {
		if p.Text() == "process this" {
			sawText = true
		}
		if raw := p.Raw(); len(raw) > 0 {
			sawFile = true
			if p.Filename != "in.csv" || p.MediaType != "text/csv" || string(raw) != "h1,h2\n1,2" {
				t.Fatalf("inbound raw part = (%q, %q, %q)", p.Filename, p.MediaType, string(raw))
			}
		}
	}
	if !sawText || !sawFile {
		t.Fatalf("message parts missing text=%v file=%v", sawText, sawFile)
	}
}
