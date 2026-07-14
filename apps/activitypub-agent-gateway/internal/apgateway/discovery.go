package apgateway

import (
	"encoding/json"
	"net/http"

	"github.com/fmind/activitypub-agent-gateway/internal/a2a"
)

// a2aAgentCardRel is the novel cross-protocol WebFinger link relation that points from a fediverse
// handle to the agent's A2A AgentCard (issue #215). One WebFinger lookup therefore reveals BOTH the
// plain ActivityPub actor (rel=self) AND the richer A2A capability, so a remote org can choose the
// higher-fidelity A2A delegation instead of degraded Note-passing — with no proprietary directory.
const a2aAgentCardRel = "https://fgentic.fmind.ai/ns/a2a#agent-card"

// discoveryCard is a synthesized A2A AgentCard advertising an agent's identity and A2A endpoint for
// discovery. It is a self-description derived from the agents.yaml allowlist; the authoritative,
// full card (skills, exact capabilities) is served by the endpoint's own well-known path.
type discoveryCard struct {
	ProtocolVersion    string         `json:"protocolVersion"`
	Name               string         `json:"name"`
	Description        string         `json:"description"`
	URL                string         `json:"url"`
	PreferredTransport string         `json:"preferredTransport"`
	Version            string         `json:"version"`
	Capabilities       map[string]any `json:"capabilities"`
	DefaultInputModes  []string       `json:"defaultInputModes"`
	DefaultOutputModes []string       `json:"defaultOutputModes"`
	Skills             []any          `json:"skills"`
	// Identity cross-references the actor's FEP-c390 did:key and publishes the matching P-256 JWK, so
	// a verifier confirms this A2A face shares the sovereign key of the AP actor (issue #218). Omitted
	// when the identity anchor is disabled.
	Identity *cardIdentity `json:"identity,omitempty"`
}

// cardIdentity is the AgentCard side of the FEP-c390 binding.
type cardIdentity struct {
	DID          string         `json:"did"`
	PublicKeyJWK map[string]any `json:"publicKeyJwk"`
}

// a2aCardURL is the public URL of a ghost's published A2A AgentCard (served by this gateway).
func (g *Gateway) a2aCardURL(ghost string) string {
	return string(g.actorID(ghost)) + "/agent-card.json"
}

// a2aEndpoint is the public A2A delegation endpoint advertised for an agent. Reachability of this
// route stays governed by the federation profile (docs/fediverse.md §3); the card only advertises it.
func (g *Gateway) a2aEndpoint(ref AgentRef) string {
	return g.a2aBaseURL + "/api/a2a/" + ref.Namespace + "/" + ref.Name
}

// buildAgentCard synthesizes a ghost's A2A AgentCard from its allowlist entry. Streaming is false to
// match the bridge's deliberate non-streaming delegation model (docs/bridge.md §6).
func (g *Gateway) buildAgentCard(ghost string, ref AgentRef) discoveryCard {
	card := discoveryCard{
		ProtocolVersion:    a2a.ProtocolVersion,
		Name:               ghost,
		Description:        ref.Description,
		URL:                g.a2aEndpoint(ref),
		PreferredTransport: "JSONRPC",
		Version:            "1.0",
		Capabilities:       map[string]any{"streaming": false},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills:             []any{},
	}
	if g.identity != nil {
		if jwk, err := g.identity.JWK(); err == nil {
			card.Identity = &cardIdentity{DID: g.identity.DID(), PublicKeyJWK: jwk}
		}
	}
	return card
}

// handleAgentCard serves a ghost's synthesized A2A AgentCard. Only allowlisted agents are
// discoverable — an unmapped ghost 404s, exactly like the actor and inbox surfaces.
func (g *Gateway) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	ref, served := g.registry.Lookup(ghost)
	if !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	data, err := json.Marshal(g.buildAgentCard(ghost, ref))
	if err != nil {
		g.log.Error("marshal agent card", "ghost", ghost, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write(data)
}
