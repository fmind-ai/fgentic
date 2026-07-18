package apgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	vocab "github.com/go-ap/activitypub"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/safehttp"
)

// maxActorDocBytes bounds an untrusted follower actor-document fetch.
const maxActorDocBytes = 1 << 20

// fetchInbox resolves a remote actor's inbox URL from its actor document, so the group can deliver a
// signed Accept there. The document is untrusted input; only the inbox field is read.
func (g *Gateway) fetchInbox(ctx context.Context, actorURI string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, actorURI, nil)
	if err != nil {
		return "", fmt.Errorf("build actor request: %w", err)
	}
	if err := safehttp.ValidateURL(req.URL); err != nil {
		return "", fmt.Errorf("validate actor URL: %w", err)
	}
	req.Header.Set("Accept", "application/activity+json")
	resp, err := g.groupClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch actor %s: %w", actorURI, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch actor %s: status %d", actorURI, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxActorDocBytes))
	if err != nil {
		return "", fmt.Errorf("read actor %s: %w", actorURI, err)
	}
	var doc struct {
		Inbox string `json:"inbox"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", fmt.Errorf("decode actor %s: %w", actorURI, err)
	}
	if doc.Inbox == "" {
		return "", fmt.Errorf("actor %s declares no inbox", actorURI)
	}
	inboxURL, err := url.Parse(doc.Inbox)
	if err != nil {
		return "", fmt.Errorf("parse actor %s inbox: %w", actorURI, err)
	}
	if err := safehttp.ValidateURL(inboxURL); err != nil {
		return "", fmt.Errorf("validate actor %s inbox: %w", actorURI, err)
	}
	return doc.Inbox, nil
}

// activityRef returns a reference to a Follow activity for use as an Accept's object: its id when
// present, else an inline echo the follower can correlate.
func activityRef(activity *vocab.Activity, actorURI string) any {
	if id := activity.GetLink(); id != "" {
		return string(id)
	}
	return map[string]any{"type": "Follow", "actor": actorURI}
}

// noteToMap serializes a parsed Note back to a generic map for inlining in a group Announce.
func noteToMap(note *vocab.Object) map[string]any {
	data, err := vocab.MarshalJSON(note)
	if err != nil {
		return map[string]any{"type": "Note", "id": string(note.ID)}
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]any{"type": "Note", "id": string(note.ID)}
	}
	return m
}

// containsMention reports whether content plainly names @<ghost>, requiring a boundary after the
// ghost so @agent-docs-qa does not match @agent-docs.
func containsMention(content, ghost string) bool {
	needle := "@" + ghost
	idx := strings.Index(content, needle)
	if idx < 0 {
		return false
	}
	after := idx + len(needle)
	return after >= len(content) || !isHandleChar(content[after])
}

// isHandleChar reports whether b can continue a ghost localpart (letters, digits, '-', '_').
func isHandleChar(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	case b == '-', b == '_':
		return true
	default:
		return false
	}
}
