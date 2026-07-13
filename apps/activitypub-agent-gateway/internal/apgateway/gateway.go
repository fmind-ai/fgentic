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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/fmind/activitypub-agent-gateway/internal/delivery"
	"github.com/fmind/activitypub-agent-gateway/internal/integrity"
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
	border     *Border           // federation policy border; nil disables enforcement (local-only dev/tests)
	signer     *integrity.Signer // FEP-8b32 object-integrity signer; nil serves replies without a proof
	a2aBaseURL string            // public base of the advertised A2A endpoint (defaults to baseURL)

	// Group collaboration (issue #217); all nil unless UseGroups is called.
	groups      *GroupRegistry
	followers   *followerStore
	deliverer   *delivery.Deliverer
	groupClient *http.Client
}

// UseGroups enables the ActivityPub Group collaboration surface: designated collaboration rooms are
// exposed as Group actors that remote actors can follow and post to, with Announce fan-out and
// governed @agent routing (issue #217). deliverer signs outbound Accept/Announce; client fetches a
// follower's inbox. Requires a signer (for the actor's publicKey) and a border (F3/F4/F5).
func (g *Gateway) UseGroups(registry *GroupRegistry, deliverer *delivery.Deliverer, client *http.Client) {
	g.groups = registry
	g.followers = newFollowerStore()
	g.deliverer = deliverer
	g.groupClient = client
}

// SetA2APublicBase overrides the base URL the published AgentCard and the actor's `implements`
// advertise for A2A delegation (issue #215). It defaults to the AP public base; production points it
// at the gated federation A2A host (a2a.<domain>).
func (g *Gateway) SetA2APublicBase(baseURL string) {
	if baseURL != "" {
		g.a2aBaseURL = strings.TrimRight(baseURL, "/")
	}
}

// UseBorder installs the federation policy border. When set, every inbound activity must pass
// signature verification, actor-key binding, and the allowlist before any A2A delegation.
func (g *Gateway) UseBorder(b *Border) { g.border = b }

// UseSigner enables FEP-8b32 object integrity proofs: outbound replies carry an eddsa-jcs-2022
// DataIntegrityProof and each actor publishes the signer's Multikey under its assertionMethod, so a
// remote verifier can confirm the agent authored a reply even after relaying (docs/fediverse.md §3).
func (g *Gateway) UseSigner(s *integrity.Signer) { g.signer = s }

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
		a2aBaseURL: strings.TrimRight(baseURL, "/"),
	}, nil
}

// Handler wires the public AP routes plus health probes.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/webfinger", g.handleWebFinger)
	mux.HandleFunc("GET /.well-known/nodeinfo", g.handleNodeInfoDiscovery)
	mux.HandleFunc("GET /nodeinfo/2.1", g.handleNodeInfo)
	mux.HandleFunc("GET /ap/instance", g.handleInstanceActor)
	mux.HandleFunc("GET /ap/agents/{ghost}", g.handleActor)
	mux.HandleFunc("POST /ap/agents/{ghost}/inbox", g.handleInbox)
	mux.HandleFunc("GET /ap/agents/{ghost}/outbox", g.handleOutbox)
	mux.HandleFunc("GET /ap/agents/{ghost}/activities/{seq}", g.handleActivity)
	mux.HandleFunc("GET /ap/agents/{ghost}/agent-card.json", g.handleAgentCard)
	mux.HandleFunc("GET /ap/groups/{group}", g.handleGroupActor)
	mux.HandleFunc("POST /ap/groups/{group}/inbox", g.handleGroupInbox)
	mux.HandleFunc("GET /ap/groups/{group}/outbox", g.handleGroupOutbox)
	mux.HandleFunc("GET /ap/groups/{group}/followers", g.handleGroupFollowers)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	return mux
}

// handleWebFinger resolves acct:agent-<name>@<serverName> to the agent's actor. Federation-ready:
// it matches the FULL handle (localpart AND host), never the localpart alone (docs/fediverse.md §6).
func (g *Gateway) handleWebFinger(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")
	local, ok := g.parseHandle(resource)
	if !ok {
		g.metrics.rejected.WithLabelValues("webfinger_bad_resource").Inc()
		http.Error(w, "resource must be acct:<local>@"+g.serverName, http.StatusBadRequest)
		return
	}
	if _, served := g.registry.Lookup(local); !served {
		// A collaboration Group resolves to its Group actor (issue #217).
		if g.groups != nil {
			if _, isGroup := g.groups.Lookup(local); isGroup {
				writeJRD(w, jrd{
					Subject: g.handle(local),
					Links: []jrdLink{{
						Rel:  "self",
						Type: "application/activity+json",
						Href: string(g.groupActorID(local)),
					}},
				})
				return
			}
		}
		g.metrics.rejected.WithLabelValues("webfinger_unknown_agent").Inc()
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	ghost := local
	// One WebFinger resolution reveals BOTH the ActivityPub actor (rel=self) AND a resolvable
	// pointer to the richer A2A AgentCard — the novel cross-protocol discovery of issue #215.
	writeJRD(w, jrd{
		Subject: g.handle(ghost),
		Links: []jrdLink{
			{
				Rel:  "self",
				Type: "application/activity+json",
				Href: string(g.actorID(ghost)),
			},
			{
				Rel:  a2aAgentCardRel,
				Type: "application/json",
				Href: g.a2aCardURL(ghost),
			},
		},
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

// handleActor serves a ghost's Service actor document, including its FEP-8b32 assertionMethod
// Multikey when object-integrity signing is enabled so remote verifiers can resolve the signing key.
func (g *Gateway) handleActor(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	ref, served := g.registry.Lookup(ghost)
	if !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	data, err := g.marshalActor(ghost, ref)
	if err != nil {
		g.log.Error("marshal actor", "ghost", ghost, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// handleOutbox serves a ghost's published replies as an OrderedCollection (newest-first). The stored
// bytes are served verbatim so a signed activity's exact signed octets reach the verifier unperturbed.
func (g *Gateway) handleOutbox(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	if _, served := g.registry.Lookup(ghost); !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	items := g.store.items(ghost)
	ordered := make([]json.RawMessage, len(items))
	for i, it := range items {
		ordered[i] = it.raw
	}
	collection := map[string]any{
		"@context":     integrity.ActivityStreamsContext,
		"id":           string(g.outboxID(ghost)),
		"type":         "OrderedCollection",
		"totalItems":   len(items),
		"orderedItems": ordered,
	}
	data, err := json.Marshal(collection)
	if err != nil {
		g.log.Error("marshal outbox", "ghost", ghost, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// handleActivity dereferences a single published activity by its IRI, serving the exact signed bytes.
func (g *Gateway) handleActivity(w http.ResponseWriter, r *http.Request) {
	ghost := r.PathValue("ghost")
	if _, served := g.registry.Lookup(ghost); !served {
		http.Error(w, "no such agent", http.StatusNotFound)
		return
	}
	id := g.actorID(ghost) + vocab.IRI("/activities/"+r.PathValue("seq"))
	raw, ok := g.store.lookup(id)
	if !ok {
		http.Error(w, "no such activity", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(raw)
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
		// Reservation is admission accounting, not consumption: record the outcome (never a token
		// count, never a per-actor label) whenever the budget gate ran.
		if decision.BudgetOutcome != "" {
			g.metrics.reservations.WithLabelValues(ghost, decision.BudgetOutcome).Inc()
		}
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
	network := actorDomain(string(actorIRI))
	ctx := a2aAttribution(r.Context(), string(actorIRI), network)
	reply, err := g.delegator.Call(ctx, ref.Namespace, ref.Name, content, contextID)
	if err != nil {
		g.metrics.delegations.WithLabelValues(ghost, "error").Inc()
		g.auditDelegation(ghost, string(actorIRI), network, contextID, "error")
		g.log.Error("a2a delegation failed", "ghost", ghost, "actor", actorIRI, "error", err)
		http.Error(w, "delegation failed", http.StatusBadGateway)
		return
	}
	g.auditDelegation(ghost, string(actorIRI), network, contextID, "ok")
	g.metrics.delegations.WithLabelValues(ghost, "ok").Inc()

	activityID, err := g.publishReply(ghost, actorIRI, note.ID, reply)
	if err != nil {
		g.log.Error("publish reply", "ghost", ghost, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Location", string(activityID))
	w.WriteHeader(http.StatusAccepted)
}

// publishReply mints, signs, and stores a Create(Note) reply attributed to the ghost's actor,
// inReplyTo the triggering object, returning the activity IRI. Per-reply bot/automated labeling
// beyond Service-actor typing lands with the attribution twin (docs/fediverse.md §3).
func (g *Gateway) publishReply(ghost string, actorIRI, inReplyTo vocab.IRI, text string) (vocab.IRI, error) {
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

	activityID := vocab.IRI(fmt.Sprintf("%s/activities/%d", actor, seq))
	create := vocab.CreateNew(activityID, note)
	create.Actor = actor
	create.To = vocab.ItemCollection{actorIRI}
	create.Published = now

	raw, err := g.marshalReply(create, string(actor))
	if err != nil {
		return "", err
	}
	g.store.append(ghost, activityID, raw)
	return activityID, nil
}

// marshalReply serializes a reply activity, attaching a FEP-8b32 object integrity proof when signing
// is enabled. The signed document carries both the AS2 and data-integrity contexts so `proof` and
// `proofValue` are defined terms for a remote verifier.
func (g *Gateway) marshalReply(create *vocab.Create, actorID string) (json.RawMessage, error) {
	data, err := vocab.MarshalJSON(create)
	if err != nil {
		return nil, fmt.Errorf("marshal reply: %w", err)
	}
	if g.signer == nil {
		return data, nil
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode reply for signing: %w", err)
	}
	doc["@context"] = []any{integrity.ActivityStreamsContext, integrity.DataIntegrityContext}
	if err := g.signer.SignActivity(doc, actorID); err != nil {
		return nil, fmt.Errorf("sign reply: %w", err)
	}
	signed, err := json.Marshal(doc)
	if err != nil {
		return nil, fmt.Errorf("encode signed reply: %w", err)
	}
	return signed, nil
}

// mastodonContext defines the toot: namespace so the bot flag is a resolvable JSON-LD term.
const mastodonContext = "http://joinmastodon.org/ns#"

// marshalActor serializes a ghost's actor with honest machine labeling — ActivityStreams `Service`
// typing (buildActor) plus Mastodon's `bot: true` flag, so a Fediverse client cannot mistake an
// agent for a person (docs/audit.md, docs/fediverse.md §3) — and, when object-integrity signing is
// enabled, its FEP-8b32 assertionMethod Multikey and the security contexts.
func (g *Gateway) marshalActor(ghost string, ref AgentRef) ([]byte, error) {
	data, err := vocab.MarshalJSON(g.buildActor(ghost, ref))
	if err != nil {
		return nil, fmt.Errorf("marshal actor: %w", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("decode actor for attribution: %w", err)
	}

	// Honest machine labeling: Service type (from buildActor) + the explicit bot flag.
	doc["bot"] = true
	// FEP-844e cross-protocol advertisement: this actor also implements A2A at the endpoint below,
	// pointing to its published AgentCard so a remote org can discover the richer capability (#215).
	doc["implements"] = []any{map[string]any{
		"href":      g.a2aEndpoint(ref),
		"name":      "A2A",
		"agentCard": g.a2aCardURL(ghost),
	}}
	contexts := []any{
		integrity.ActivityStreamsContext,
		map[string]any{"toot": mastodonContext, "bot": "toot:bot"},
	}
	if g.signer != nil {
		actorID := string(g.actorID(ghost))
		contexts = append(contexts, integrity.DataIntegrityContext, "https://w3id.org/security/multikey/v1")
		doc["assertionMethod"] = []any{
			map[string]any{
				"id":                 g.signer.VerificationMethod(actorID),
				"type":               "Multikey",
				"controller":         actorID,
				"publicKeyMultibase": g.signer.PublicKeyMultibase(),
			},
		}
	}
	doc["@context"] = contexts
	return json.Marshal(doc)
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

// auditDelegation emits the AP-transport delegation evidence record (docs/audit.md): the FULL,
// un-truncated asserted actor URI plus its bounded origin. Origin fields ADD provenance and never
// replace or shorten the actor URI. It carries no note content and no model-token figure.
func (g *Gateway) auditDelegation(ghost, actorURI, network, contextID, outcome string) {
	g.log.Info(
		"delegation audit",
		"audit_schema", "fgentic.ap.delegation.v1",
		"log_stream", "audit",
		"ghost", ghost,
		"a2a_user_id", actorURI, // authoritative, never shortened
		"origin_kind", OriginKindActivityPub,
		"origin_network", network,
		"context_id", contextID,
		"outcome", outcome,
	)
}

// deriveContextID is a stable, opaque A2A contextId for a (ghost, actor, thread) triple. The ghost
// is part of the key, so a contextId is NEVER reused across agents (docs/bridge.md §threading).
func deriveContextID(ghost, actor, threadRoot string) string {
	sum := sha256.Sum256([]byte(ghost + "\x00" + actor + "\x00" + threadRoot))
	return hex.EncodeToString(sum[:])
}
