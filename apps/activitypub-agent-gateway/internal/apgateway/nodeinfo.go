package apgateway

import (
	"encoding/json"
	"net/http"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
)

// Instance-level self-description (issue #216): the Fediverse twin of /.well-known/matrix/server.
// One fetch tells a remote org which agents this sovereign instance exposes and which open protocols
// it implements — sourced from the agents.yaml allowlist so an unlisted agent is never announced.
const (
	// softwareName/softwareVersion identify this instance in NodeInfo. The version tracks the chart
	// appVersion; it is descriptive metadata, not a security boundary.
	softwareName    = "fgentic-activitypub-agent-gateway"
	softwareVersion = "0.1.0"

	// nodeInfoSchema21 is the NodeInfo 2.1 schema rel (FEP-0151, 2025 ed.).
	nodeInfoSchema21 = "http://nodeinfo.diaspora.software/ns/schema/2.1"
)

// implementedProtocol is the FEP-844e { href, name } shape advertised at the instance level.
type implementedProtocol struct {
	Href string `json:"href"`
	Name string `json:"name"`
}

// implementedProtocols is the open-standard protocol stack this instance advertises. Every href is a
// canonical, stable identifier for the protocol or FEP.
func implementedProtocols() []implementedProtocol {
	return []implementedProtocol{
		{Href: integrity.ActivityStreamsContext, Name: "ActivityPub"},
		{Href: "https://a2a-protocol.org/", Name: "A2A"},
		{Href: "https://w3id.org/fep/8b32", Name: "FEP-8b32"},
		{Href: "https://w3id.org/fep/844e", Name: "FEP-844e"},
	}
}

// handleNodeInfoDiscovery serves the /.well-known/nodeinfo pointer to the 2.1 schema document.
func (g *Gateway) handleNodeInfoDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"links": []any{
			map[string]any{"rel": nodeInfoSchema21, "href": g.baseURL + "/nodeinfo/2.1"},
		},
	}
	writeJSON(w, doc)
}

// handleNodeInfo serves the NodeInfo 2.1 document, advertising the exposed agents/skills (sourced
// live from the allowlist) and the implemented protocols. openRegistrations is false: agents are
// added by GitOps to agents.yaml, never self-service.
func (g *Gateway) handleNodeInfo(w http.ResponseWriter, _ *http.Request) {
	ghosts := g.registry.Ghosts()
	agents := make([]map[string]any, 0, len(ghosts))
	for _, ghost := range ghosts {
		ref, ok := g.registry.Lookup(ghost)
		if !ok {
			continue
		}
		agents = append(agents, map[string]any{
			"handle":    g.handle(ghost),
			"name":      ghost,
			"summary":   ref.Description,
			"actor":     string(g.actorID(ghost)),
			"agentCard": g.a2aCardURL(ghost),
		})
	}

	protocols := implementedProtocols()
	protocolNames := make([]string, len(protocols))
	for i, p := range protocols {
		protocolNames[i] = p.Name
	}

	doc := map[string]any{
		"version":           "2.1",
		"software":          map[string]any{"name": softwareName, "version": softwareVersion},
		"protocols":         []string{"activitypub"},
		"services":          map[string]any{"inbound": []string{}, "outbound": []string{}},
		"openRegistrations": false,
		"usage":             map[string]any{"users": map[string]any{"total": len(agents)}},
		"metadata": map[string]any{
			"implements": protocolNames,
			"agents":     agents,
		},
	}
	writeJSON(w, doc)
}

// handleInstanceActor serves the FEP-2677 instance Application actor: a machine-typed self-
// description of the whole instance (not a single agent), advertising the implemented protocols.
func (g *Gateway) handleInstanceActor(w http.ResponseWriter, _ *http.Request) {
	id := g.baseURL + "/ap/instance"
	actor := map[string]any{
		"@context":          []any{integrity.ActivityStreamsContext},
		"id":                id,
		"type":              "Application",
		"preferredUsername": g.serverName,
		"name":              "Fgentic agent instance",
		"summary":           "Sovereign ActivityPub↔A2A agent gateway; agents are exposed from a GitOps allowlist.",
		"inbox":             id + "/inbox",
		"outbox":            id + "/outbox",
		"url":               id,
		"implements":        implementedProtocols(),
	}
	data, err := json.Marshal(actor)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// writeJSON marshals a document and writes it as application/json.
func writeJSON(w http.ResponseWriter, doc any) {
	data, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}
