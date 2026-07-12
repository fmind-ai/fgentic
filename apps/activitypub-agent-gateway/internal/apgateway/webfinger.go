package apgateway

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/fmind/activitypub-agent-gateway/internal/a2a"
)

// jrd is the WebFinger JSON Resource Descriptor (RFC 7033) returned for an agent handle.
type jrd struct {
	Subject string    `json:"subject"`
	Links   []jrdLink `json:"links"`
}

type jrdLink struct {
	Rel  string `json:"rel"`
	Type string `json:"type,omitempty"`
	Href string `json:"href,omitempty"`
}

func writeJRD(w http.ResponseWriter, doc jrd) {
	data, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/jrd+json; charset=utf-8")
	_, _ = w.Write(data)
}

// OriginKindActivityPub is the bounded origin kind stamped on every AP-transport delegation, the
// parallel of the bridge's origin.kind for external-appservice senders (docs/audit.md).
const OriginKindActivityPub = "activitypub"

// a2aAttribution stamps the FULL asserted AP actor URI plus its bounded origin (kind=activitypub,
// network=signing domain) onto the context so the A2A client forwards them. The actor URI is
// authoritative and never shortened; origin is additive audit metadata (docs/audit.md).
func a2aAttribution(ctx context.Context, actorURI, network string) context.Context {
	return a2a.WithAttribution(ctx, actorURI, a2a.Origin{Kind: OriginKindActivityPub, Network: network})
}
