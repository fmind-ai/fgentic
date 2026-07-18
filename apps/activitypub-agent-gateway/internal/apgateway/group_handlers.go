package apgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	vocab "github.com/go-ap/activitypub"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/activitystate"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
)

// publicCollection is the AS2 special IRI addressing "everyone" — Announce fan-out is public.
const publicCollection = "https://www.w3.org/ns/activitystreams#Public"

// groupActorID returns the stable public IRI of a collaboration Group actor.
func (g *Gateway) groupActorID(id string) vocab.IRI {
	return vocab.IRI(fmt.Sprintf("%s/ap/groups/%s", g.baseURL, id))
}

// buildGroupActor renders a collaboration room as an ActivityPub Group actor, publishing the
// RSA HTTP-Signature key so followers can verify the group's Announce/Accept deliveries.
func (g *Gateway) buildGroupActor(id string, ref GroupRef) map[string]any {
	actorID := string(g.groupActorID(id))
	doc := map[string]any{
		"@context":          []any{integrity.ActivityStreamsContext, "https://w3id.org/security/v1"},
		"id":                actorID,
		"type":              "Group",
		"preferredUsername": id,
		"name":              ref.Name,
		"summary":           ref.Description,
		"inbox":             actorID + "/inbox",
		"outbox":            actorID + "/outbox",
		"followers":         actorID + "/followers",
		"url":               actorID,
	}
	if g.httpPublicKeyPEM != "" {
		doc["publicKey"] = map[string]any{
			"id":           actorID + "#main-key",
			"owner":        actorID,
			"publicKeyPem": g.httpPublicKeyPEM,
		}
	}
	return doc
}

// handleGroupActor serves a Group actor document.
func (g *Gateway) handleGroupActor(w http.ResponseWriter, r *http.Request) {
	if g.groups == nil {
		http.Error(w, "groups disabled", http.StatusNotFound)
		return
	}
	id := r.PathValue("group")
	ref, served := g.groups.Lookup(id)
	if !served {
		http.Error(w, "no such group", http.StatusNotFound)
		return
	}
	data, err := json.Marshal(g.buildGroupActor(id, ref))
	if err != nil {
		g.log.Error("marshal group actor", "group", id, "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	_, _ = w.Write(data)
}

// handleGroupOutbox serves a group's published Announces (newest-first), reusing the outbox store
// keyed by group id.
func (g *Gateway) handleGroupOutbox(w http.ResponseWriter, r *http.Request) {
	if g.groups == nil {
		http.Error(w, "groups disabled", http.StatusNotFound)
		return
	}
	id := r.PathValue("group")
	if _, served := g.groups.Lookup(id); !served {
		http.Error(w, "no such group", http.StatusNotFound)
		return
	}
	items := g.store.items(id)
	ordered := make([]json.RawMessage, len(items))
	for i, it := range items {
		ordered[i] = it.raw
	}
	writeJSON(w, map[string]any{
		"@context":     integrity.ActivityStreamsContext,
		"id":           string(g.groupActorID(id)) + "/outbox",
		"type":         "OrderedCollection",
		"totalItems":   len(items),
		"orderedItems": ordered,
	})
}

// handleGroupFollowers serves the follower count as an OrderedCollection. Follower actor URIs are
// deliberately withheld (content-free) so the public collection cannot be scraped for a member list.
func (g *Gateway) handleGroupFollowers(w http.ResponseWriter, r *http.Request) {
	if g.groups == nil {
		http.Error(w, "groups disabled", http.StatusNotFound)
		return
	}
	id := r.PathValue("group")
	if _, served := g.groups.Lookup(id); !served {
		http.Error(w, "no such group", http.StatusNotFound)
		return
	}
	writeJSON(w, map[string]any{
		"@context":     integrity.ActivityStreamsContext,
		"id":           string(g.groupActorID(id)) + "/followers",
		"type":         "OrderedCollection",
		"totalItems":   g.followers.count(id),
		"orderedItems": []any{},
	})
}

// handleGroupInbox processes an inbound activity to a Group. ALL inbound group traffic passes the
// transport border (signature + allowlist) first; a Create additionally reserves budget per mentioned
// agent before any A2A call. Off-allowlist or over-budget traffic is dropped (issue #217).
func (g *Gateway) handleGroupInbox(w http.ResponseWriter, r *http.Request) {
	if g.groups == nil {
		http.Error(w, "groups disabled", http.StatusNotFound)
		return
	}
	id := r.PathValue("group")
	if _, served := g.groups.Lookup(id); !served {
		http.Error(w, "no such group", http.StatusNotFound)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxInboxBytes))
	if err != nil {
		http.Error(w, "cannot read body", http.StatusBadRequest)
		return
	}
	item, err := vocab.UnmarshalJSON(body)
	if err != nil {
		http.Error(w, "cannot decode activity", http.StatusBadRequest)
		return
	}
	activity, err := vocab.ToActivity(item)
	if err != nil {
		http.Error(w, "not an activity", http.StatusBadRequest)
		return
	}
	actorIRI := activity.Actor.GetLink()
	if actorIRI == "" {
		http.Error(w, "activity has no actor", http.StatusBadRequest)
		return
	}

	// Transport border on ALL inbound group traffic (F3 allowlist + F4 signature).
	if g.border != nil {
		if d := g.border.VerifyTransport(r.Context(), r, body, string(actorIRI)); !d.Allowed {
			g.metrics.rejected.WithLabelValues("group_border_" + d.Reason).Inc()
			g.log.Warn("group border denied inbound", "group", id, "reason", d.Reason, "policy", d.Digest)
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
	}

	switch activity.GetType() {
	case vocab.FollowType:
		g.groupFollow(r.Context(), id, string(actorIRI), activity)
	case vocab.UndoType:
		g.followers.remove(id, string(actorIRI))
	case vocab.CreateType:
		note, err := vocab.ToObject(activity.Object)
		if err != nil || note.GetType() != vocab.NoteType {
			http.Error(w, "expected an inline Create(Note)", http.StatusBadRequest)
			return
		}
		if err := validateActivityID(activity.ID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		job := activitystate.Job{
			ActivityID: string(activity.ID),
			Route:      activitystate.RouteGroup,
			Target:     id,
			ActorURI:   string(actorIRI),
			Body:       body,
		}
		g.acceptActivity(w, r, job)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// groupFollow records a follower and delivers a signed Accept(Follow) to its inbox so the remote
// subscription completes. The follower's inbox is resolved from its actor document.
func (g *Gateway) groupFollow(ctx context.Context, id, actorURI string, activity *vocab.Activity) {
	inbox, err := g.fetchInbox(ctx, actorURI)
	if err != nil {
		g.log.Warn("group follow: cannot resolve follower inbox", "group", id, "error", err.Error())
		return
	}
	g.followers.add(id, actorURI, inbox)

	groupActor := string(g.groupActorID(id))
	accept := map[string]any{
		"@context": integrity.ActivityStreamsContext,
		"id":       fmt.Sprintf("%s/activities/%d", groupActor, g.store.next()),
		"type":     "Accept",
		"actor":    groupActor,
		"object":   activityRef(activity, actorURI),
		"to":       []any{actorURI},
	}
	raw, err := json.Marshal(accept)
	if err != nil {
		return
	}
	if err := g.deliverer.Deliver(ctx, inbox, groupActor, raw); err != nil {
		g.log.Warn("group follow: Accept delivery failed", "group", id, "error", err.Error())
	}
}

// groupCreate fans out an inbound post as an Announce to followers (the guppe model) and routes any
// @agent mention through the governed A2A path, publishing each reply back into the group.
func (g *Gateway) groupCreate(ctx context.Context, id, actorURI string, note *vocab.Object) {
	groupActor := string(g.groupActorID(id))

	// Fan out the member's post to all other followers.
	g.announce(ctx, id, noteToMap(note), actorURI)

	// Route mentions through the border's budget gate, then A2A.
	for _, ghost := range g.mentionedGhosts(note) {
		ref, ok := g.registry.Lookup(ghost)
		if !ok {
			continue
		}
		if !g.reserveBudget(ghost, actorURI) {
			continue
		}
		content := nlvText(note.Content)
		contextID := deriveContextID(ghost, actorURI, groupActor+"\x00"+string(note.ID))
		callCtx := a2aAttribution(ctx, actorURI, actorDomain(actorURI))
		reply, err := g.delegator.Call(callCtx, ref.Namespace, ref.Name, content, contextID)
		if err != nil {
			g.metrics.delegations.WithLabelValues(ghost, "error").Inc()
			g.log.Error("group a2a delegation failed", "group", id, "ghost", ghost, "error", err)
			continue
		}
		g.metrics.delegations.WithLabelValues(ghost, "ok").Inc()
		g.announce(ctx, id, g.buildAgentReplyNote(ghost, note, reply), "")
	}
}

// announce wraps an object in a group Announce, stores it in the group outbox, and fans it out to
// followers (excluding the original author). The Announce is signed on delivery.
func (g *Gateway) announce(ctx context.Context, id string, object map[string]any, excludeAuthor string) {
	groupActor := string(g.groupActorID(id))
	seq := g.store.next()
	announce := map[string]any{
		"@context": integrity.ActivityStreamsContext,
		"id":       fmt.Sprintf("%s/activities/%d", groupActor, seq),
		"type":     "Announce",
		"actor":    groupActor,
		"object":   object,
		"to":       []any{publicCollection, groupActor + "/followers"},
	}
	raw, err := json.Marshal(announce)
	if err != nil {
		return
	}
	g.store.append(id, vocab.IRI(announce["id"].(string)), raw)
	if inboxes := g.followers.inboxes(id, excludeAuthor); len(inboxes) > 0 {
		g.deliverer.Fanout(ctx, inboxes, groupActor, raw)
	}
}

// buildAgentReplyNote mints a Note authored by the mentioned agent, replying to the triggering note,
// signed with a FEP-8b32 proof so followers can verify a sovereign agent authored it.
func (g *Gateway) buildAgentReplyNote(ghost string, inReplyTo *vocab.Object, text string) map[string]any {
	actor := string(g.actorID(ghost))
	seq := g.store.next()
	note := map[string]any{
		"id":           fmt.Sprintf("%s/objects/%d", actor, seq),
		"type":         "Note",
		"attributedTo": actor,
		"content":      text,
		"inReplyTo":    string(inReplyTo.ID),
		"to":           []any{publicCollection},
	}
	if g.signer != nil {
		note["@context"] = []any{integrity.ActivityStreamsContext, integrity.DataIntegrityContext}
		if err := g.signer.SignActivity(note, actor); err != nil {
			delete(note, "@context")
			g.log.Warn("group reply: signing failed", "ghost", ghost, "error", err.Error())
		}
	}
	return note
}

// mentionedGhosts returns every allowlisted ghost named by a note, via an AP Mention tag or a
// plaintext @<ghost>. It never matches a remote actor as a local target.
func (g *Gateway) mentionedGhosts(note *vocab.Object) []string {
	seen := make(map[string]struct{})
	var ghosts []string
	add := func(ghost string) {
		if _, served := g.registry.Lookup(ghost); !served {
			return
		}
		if _, dup := seen[ghost]; dup {
			return
		}
		seen[ghost] = struct{}{}
		ghosts = append(ghosts, ghost)
	}
	for _, tag := range note.Tag {
		if link, err := vocab.ToLink(tag); err == nil {
			for _, ghost := range g.registry.Ghosts() {
				if string(link.Href) == string(g.actorID(ghost)) {
					add(ghost)
				}
			}
		}
	}
	content := nlvText(note.Content)
	for _, ghost := range g.registry.Ghosts() {
		if containsMention(content, ghost) {
			add(ghost)
		}
	}
	return ghosts
}
