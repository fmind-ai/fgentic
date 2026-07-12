// Package apgateway is the ActivityPub agent gateway's HTTP surface: it serves each exposed
// platform agent as an AP Service actor (WebFinger + actor document), turns inbound Create(Note)
// mentions into governed A2A delegations through agentgateway, and publishes the reply as a
// Create(Note) in the agent's outbox.
//
// Inbound AP content is UNTRUSTED (prompt injection is threat #1, docs/security.md): this package
// lands only the actor surface. The HTTP-Signature/allowlist border and object-integrity twin
// controls (docs/fediverse.md §3) gate real public exposure and are landed by later federation
// work. Until then a mention is delegated only when the note actually names the routed agent, so
// automation cannot fan a single delivery into unbounded LLM spend.
package apgateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"
	"github.com/prometheus/client_golang/prometheus"
)

// maxInboxBytes bounds an untrusted inbound activity body.
const maxInboxBytes = 1 << 20 // 1 MiB

// Delegator sends a text prompt to a local kagent agent over A2A and returns its reply text. The
// gateway depends on this narrow interface so the wire client and tests stay decoupled.
type Delegator interface {
	Call(ctx context.Context, namespace, name, text, contextID string) (string, error)
}

// Gateway serves the ActivityPub surface for a registry of agents.
type Gateway struct {
	baseURL    string // public scheme+host, no trailing slash
	serverName string // fediverse handle domain
	registry   *Registry
	delegator  Delegator
	store      *outboxStore
	metrics    *metrics
	log        *slog.Logger
	now        func() time.Time
	border     *Border // federation policy border; nil disables enforcement (local-only dev/tests)
}

// UseBorder installs the federation policy border. When set, every inbound activity must pass
// signature verification, actor-key binding, and the allowlist before any A2A delegation.
func (g *Gateway) UseBorder(b *Border) { g.border = b }

// New builds a Gateway. baseURL is the public scheme+host every actor URL is built from; serverName
// is the acct: handle domain. reg (a prometheus.Registerer) receives the gateway's counters.
func New(baseURL, serverName string, registry *Registry, delegator Delegator, reg prometheus.Registerer, log *slog.Logger) (*Gateway, error) {
	if baseURL == "" || serverName == "" {
		return nil, fmt.Errorf("gateway: baseURL and serverName are required")
	}
	if registry == nil || delegator == nil {
		return nil, fmt.Errorf("gateway: registry and delegator are required")
	}
	return &Gateway{
		baseURL:    strings.TrimRight(baseURL, "/"),
		serverName: serverName,
		registry:   registry,
		delegator:  delegator,
		store:      newOutboxStore(),
		metrics:    newMetrics(reg),
		log:        log,
		now:        func() time.Time { return time.Now().UTC() },
	}, nil
}

// Handler wires the public AP routes plus health probes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/webfinger", g.handleWebFinger)
	mux.HandleFunc("GET /ap/agents/{ghost}", g.handleActor)
	mux.HandleFunc("POST /ap/agents/{ghost}/inbox", g.handleInbox)
	mux.HandleFunc("GET /ap/agents/{ghost}/outbox", g.handleOutbox)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	return mux
}

// handleWebFinger resolves acct:agent-<name>@<serverName> to the agent's actor. Federation-ready:
// it matches the FULL handle (localpart AND host), never the localpart alone (docs/fediverse.md §6).
func (g *Gateway) handleWebFinger(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	ghost, ok := g.parseHandle(resource)
	if !ok {
		g.metrics.rejected.WithLabelValues("webfinger_bad_resource").Inc()
		http.Error(w, "resource must be acct:<ghost>@"+g.serverName, http.StatusBadRequest)
		return
	}
	if _, served := g.registry.Lookup(ghost); !served {
		g.metrics.rejected.WithLabelValues("webfinger_unknown_agent").Inc()
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	writeJRD(w, jrd{
		Subject: g.handle(ghost),
		Links: []jrdLink{{
			Rel:  "self",
			Type: "application/activity+json",
			Href: string(g.actorID(ghost)),
		}},
	})
}

// parseHandle extracts the ghost from acct:<ghost>@<serverName>, rejecting any other host.
func (g *Gateway) parseHandle(resource string) (string, bool) {
	rest, ok := strings.CutPrefix(resource, "acct:")
	if !ok {
		return "", false
	}
	local, host, ok := strings.Cut(rest, "@")
	if !ok || local == "" {
		return "", false
	}
	if !strings.EqualFold(host, g.serverName) {
		return "", false
	}
	return local, true
}

// handleActor serves a ghost's Service actor document.
func (g *Gateway) handleActor(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	ref, served := g.registry.Lookup(ghost)
	if !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	g.writeAP(w, g.buildActor(ghost, ref))
}

// handleOutbox serves a ghost's published replies as an OrderedCollection (newest-first).
func (g *Gateway) handleOutbox(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	if _, served := g.registry.Lookup(ghost); !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	items := g.store.items(ghost)
	oc := vocab.OrderedCollectionNew(g.outboxID(ghost))
	oc.TotalItems = uint(len(items))
	for _, activity := range items {
		oc.OrderedItems = append(oc.OrderedItems, activity)
	}
	g.writeAP(w, oc)
}

// handleInbox turns a Create(Note) mention into one A2A delegation and publishes the reply.
func (g *Gateway) handleInbox(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	ref, served := g.registry.Lookup(ghost)
	if !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInboxBytes))
	if err != nil {
		g.metrics.rejected.WithLabelValues("inbox_read").Inc()
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	activity, note, err := parseCreateNote(body)
	if err != nil {
		g.metrics.inbound.WithLabelValues(ghost, "unparseable").Inc()
		g.metrics.rejected.WithLabelValues("inbox_parse").Inc()
		http.Error(w, "expected an inline Create(Note): "+err.Error(), http.StatusBadRequest)
		return
	}
	g.metrics.inbound.WithLabelValues(ghost, "create").Inc()

	actorIRI := activity.Actor.GetLink()
	if actorIRI == "" {
		g.metrics.rejected.WithLabelValues("inbox_no_actor").Inc()
		http.Error(w, "activity has no actor", http.StatusBadRequest)
		return
	}
	content := nlvText(note.Content)
	if content == "" {
		g.metrics.rejected.WithLabelValues("inbox_empty_content").Inc()
		http.Error(w, "note has no content", http.StatusBadRequest)
		return
	}

	// Federation policy border: an inbound activity must carry a valid signature from the claimed
	// actor's key, and that actor must be on the git-reloadable allowlist — enforced BEFORE any
	// A2A call so an unsigned or off-allowlist remote never reaches an agent (docs/fediverse.md §3).
	if g.border != nil {
		decision := g.border.Authorize(r.Context(), r, body, string(actorIRI))
		if !decision.Allowed {
			g.metrics.rejected.WithLabelValues("border_" + decision.Reason).Inc()
			// Content-free evidence: reason + policy digest, never the actor URI or note content.
			g.log.Warn("federation border denied inbound", "ghost", ghost, "reason", decision.Reason, "policy", decision.Digest)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	// Only delegate when the note actually names this agent. A no-op accept (202) keeps a stray
	// or relayed delivery from spending an LLM invocation.
	if !g.mentions(ghost, note) {
		g.metrics.delegations.WithLabelValues(ghost, "not_mentioned").Inc()
		w.WriteHeader(http.StatusAccepted)
		return
	}

	contextID := deriveContextID(ghost, string(actorIRI), threadRoot(note))
	ctx := a2aUser(r.Context(), string(actorIRI))
	reply, err := g.delegator.Call(ctx, ref.Namespace, ref.Name, content, contextID)
	if err != nil {
		g.metrics.delegations.WithLabelValues(ghost, "error").Inc()
		g.log.Error("a2a delegation failed", "ghost", ghost, "actor", actorIRI, "error", err)
		http.Error(w, "delegation failed", http.StatusBadGateway)
		return
	}
	g.metrics.delegations.WithLabelValues(ghost, "ok").Inc()

	created := g.publishReply(ghost, actorIRI, note.ID, reply)
	w.Header().Set("Location", string(created.ID))
	w.WriteHeader(http.StatusAccepted)
}

// publishReply mints and stores a Create(Note) reply attributed to the ghost's actor, inReplyTo
// the triggering object. Per-reply bot/automated labeling beyond Service-actor typing lands with
// the attribution twin (docs/fediverse.md §3).
func (g *Gateway) publishReply(ghost string, actorIRI, inReplyTo vocab.IRI, text string) *vocab.Create {
	seq := g.store.next()
	actor := g.actorID(ghost)
	now := g.now()

	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = vocab.IRI(fmt.Sprintf("%s/objects/%d", actor, seq))
	note.AttributedTo = actor
	note.Content = vocab.NaturalLanguageValuesNew(vocab.DefaultLangRef(text))
	note.To = vocab.ItemCollection{actorIRI}
	note.Published = now
	if inReplyTo != "" {
		note.InReplyTo = inReplyTo
	}

	create := vocab.CreateNew(vocab.IRI(fmt.Sprintf("%s/activities/%d", actor, seq)), note)
	create.Actor = actor
	create.To = vocab.ItemCollection{actorIRI}
	create.Published = now

	g.store.append(ghost, create)
	return create
}

// mentions reports whether the note names this ghost, via an AP Mention tag targeting its actor
// or a plaintext @<ghost> in the content.
func (g *Gateway) mentions(ghost string, note *vocab.Object) bool {
	actor := string(g.actorID(ghost))
	for _, tag := range note.Tag {
		if link, err := vocab.ToLink(tag); err == nil && string(link.Href) == actor {
			return true
		}
	}
	return strings.Contains(nlvText(note.Content), "@"+ghost)
}

// nlvText returns the trimmed first natural-language value of an AS2 field. The collection-level
// String() renders an empty value as "[]", so the first element is read directly.
func nlvText(n vocab.NaturalLanguageValues) string {
	return strings.TrimSpace(n.First().String())
}

// writeAP marshals an ActivityStreams object and writes it with the canonical AP content type.
func (g *Gateway) writeAP(w http.ResponseWriter, item vocab.Item) {
	data, err := vocab.MarshalJSON(item)
	if err != nil {
		g.log.Error("marshal activitypub object", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// parseCreateNote extracts the Create activity and its inline Note object from an inbound body.
func parseCreateNote(body []byte) (*vocab.Activity, *vocab.Object, error) {
	item, err := vocab.UnmarshalJSON(body)
	if err != nil {
		return nil, nil, fmt.Errorf("decode: %w", err)
	}
	activity, err := vocab.ToActivity(item)
	if err != nil {
		return nil, nil, fmt.Errorf("not an activity: %w", err)
	}
	if activity.Type != vocab.CreateType {
		return nil, nil, fmt.Errorf("activity type %q is not Create", activity.Type)
	}
	if activity.Object == nil {
		return nil, nil, errors.New("activity has no object")
	}
	note, err := vocab.ToObject(activity.Object)
	if err != nil {
		return nil, nil, fmt.Errorf("object is not inline: %w", err)
	}
	if note.Type != vocab.NoteType {
		return nil, nil, fmt.Errorf("object type %q is not Note", note.Type)
	}
	return activity, note, nil
}

// threadRoot returns the conversation anchor for context threading: the note it replies to, or
// the note itself when it starts a thread.
func threadRoot(note *vocab.Object) string {
	if note.InReplyTo != nil {
		if root := note.InReplyTo.GetLink(); root != "" {
			return string(root)
		}
	}
	return string(note.ID)
}

// deriveContextID is a stable, opaque A2A contextId for a (ghost, actor, thread) triple. The ghost
// is part of the key, so a contextId is NEVER reused across agents (docs/bridge.md §threading).
func deriveContextID(ghost, actor, threadRoot string) string {
	sum := sha256.Sum256([]byte(ghost + "\x00" + actor + "\x00" + threadRoot))
	return hex.EncodeToString(sum[:])
}
