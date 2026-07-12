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

// a2aUser stamps the asserted AP actor onto the context so the A2A client forwards it as the
// end-user identity (X-User-Id) for kagent session/audit attribution.
func a2aUser(ctx context.Context, actor string) context.Context {
	return a2a.WithUser(ctx, actor)
}
