package apgateway

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// gatewayWithAgents builds a gateway serving exactly the agents in the given agents.yaml body.
func gatewayWithAgents(t *testing.T, agentsYAML string) *Gateway {
	t.Helper()
	registry, err := LoadRegistry(writeAgents(t, agentsYAML), "agent-")
	if err != nil {
		t.Fatalf("LoadRegistry: %v", err)
	}
	g, err := New("https://fgentic.localhost", "fgentic.localhost", registry, &fakeDelegator{}, prometheus.NewRegistry(), slog.Default())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return g
}

func TestNodeInfoDiscoveryPointsToSchema(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	rec := do(t, g, http.MethodGet, "/.well-known/nodeinfo", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var doc struct {
		Links []struct{ Rel, Href string } `json:"links"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Links) != 1 || doc.Links[0].Rel != nodeInfoSchema21 {
		t.Fatalf("links = %+v", doc.Links)
	}
	if doc.Links[0].Href != "https://fgentic.localhost/nodeinfo/2.1" {
		t.Errorf("href = %q", doc.Links[0].Href)
	}
}

func TestNodeInfoAdvertisesAllowlistAndProtocols(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{}) // validAgents: agent-docs-qa + agent-scribe
	rec := do(t, g, http.MethodGet, "/nodeinfo/2.1", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["openRegistrations"] != false {
		t.Errorf("openRegistrations = %v, want false", doc["openRegistrations"])
	}
	meta := doc["metadata"].(map[string]any)
	agents := meta["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("agents = %d, want 2", len(agents))
	}
	usage := doc["usage"].(map[string]any)["users"].(map[string]any)
	if usage["total"] != float64(2) {
		t.Errorf("usage.users.total = %v", usage["total"])
	}
	impls := toStringSet(meta["implements"].([]any))
	for _, want := range []string{"A2A", "FEP-8b32", "FEP-844e"} {
		if !impls[want] {
			t.Errorf("implements missing %q (got %v)", want, meta["implements"])
		}
	}
	// The agent entries name only allowlisted ghosts and carry resolvable actor/card pointers.
	first := agents[0].(map[string]any)
	if first["actor"] == "" || first["agentCard"] == "" {
		t.Errorf("agent entry missing pointers: %+v", first)
	}
}

func TestNodeInfoTracksAllowlistRemoval(t *testing.T) {
	single := `schemaVersion: 1
agents:
  agent-docs-qa:
    namespace: kagent
    name: docs-qa
    description: Answers docs questions.
`
	g := gatewayWithAgents(t, single)
	rec := do(t, g, http.MethodGet, "/nodeinfo/2.1", "")
	var doc map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	agents := doc["metadata"].(map[string]any)["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agents = %d, want 1 (removing an agent removes it from NodeInfo)", len(agents))
	}
	if agents[0].(map[string]any)["name"] != "agent-docs-qa" {
		t.Errorf("agent = %v", agents[0])
	}
}

func TestInstanceActorResolves(t *testing.T) {
	g := newTestGateway(t, &fakeDelegator{})
	rec := do(t, g, http.MethodGet, "/ap/instance", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d", rec.Code)
	}
	var actor map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &actor); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if actor["type"] != "Application" {
		t.Errorf("type = %v, want Application (FEP-2677 instance actor)", actor["type"])
	}
	if actor["id"] != "https://fgentic.localhost/ap/instance" {
		t.Errorf("id = %v", actor["id"])
	}
	impls, ok := actor["implements"].([]any)
	if !ok || len(impls) == 0 {
		t.Fatalf("implements = %v", actor["implements"])
	}
	names := map[string]bool{}
	for _, i := range impls {
		names[i.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"ActivityPub", "A2A", "FEP-8b32", "FEP-844e"} {
		if !names[want] {
			t.Errorf("instance actor implements missing %q", want)
		}
	}
}

func toStringSet(items []any) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			set[s] = true
		}
	}
	return set
}
