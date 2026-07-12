package apgateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

func TestActorAdvertisesServiceAndBot(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	rec := do(t, g, http.MethodGet, "/ap/agents/agent-docs-qa", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var actor map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &actor); err != nil {
		t.Fatalf("unmarshal actor: %v", err)
	}
	if actor["type"] != "Service" {
		t.Errorf("type = %v, want Service (honest machine typing)", actor["type"])
	}
	if actor["bot"] != true {
		t.Errorf("bot = %v, want true", actor["bot"])
	}
}

// gatewayWithLog builds a gateway logging to buf so audit records can be asserted.
func gatewayWithLog(t *testing.T, del Delegator, buf *bytes.Buffer) *Gateway {
	t.Helper()
	registry, err := LoadRegistry(writeAgents(t, validAgents), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	log := slog.New(slog.NewJSONHandler(buf, nil))
	g, err := New("https://fgentic.localhost", "fgentic.localhost", registry, del, prometheus.NewRegistry(), log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestDelegationEmitsAuditWithFullActorURI(t *testing.T) {
	var buf bytes.Buffer
	del := &fakeDelegator{reply: "hi"}
	g := gatewayWithLog(t, del, &buf)

	rec := do(t, g, http.MethodPost, "/ap/agents/agent-docs-qa/inbox", createNote)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("code = %d", rec.Code)
	}

	audit := findAudit(t, &buf)
	if audit["a2a_user_id"] != "https://mastodon.example/users/bob" {
		t.Errorf("a2a_user_id = %v, want the full un-truncated actor URI", audit["a2a_user_id"])
	}
	if audit["origin_kind"] != "activitypub" {
		t.Errorf("origin_kind = %v", audit["origin_kind"])
	}
	if audit["origin_network"] != "mastodon.example" {
		t.Errorf("origin_network = %v", audit["origin_network"])
	}
	if audit["outcome"] != "ok" {
		t.Errorf("outcome = %v", audit["outcome"])
	}
	// Origin fields never replace the authoritative URI: the network is a strict host substring of it.
	if audit["origin_network"] == audit["a2a_user_id"] {
		t.Errorf("origin must not stand in for the full actor URI")
	}
}

// findAudit returns the parsed "delegation audit" log record.
func findAudit(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	for _, line := range bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n")) {
		var rec map[string]any
		if err := json.Unmarshal(line, &rec); err != nil {
			continue
		}
		if rec["msg"] == "delegation audit" {
			return rec
		}
	}
	t.Fatalf("no delegation audit record in log:\n%s", buf.String())
	return nil
}
