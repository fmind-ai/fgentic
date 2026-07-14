package apgateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	vocab "github.com/go-ap/activitypub"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
)

// maxAlertsBytes bounds an inbound Alertmanager webhook payload.
const maxAlertsBytes = 1 << 20

// statusReservation is the token count debited from an agent's per-window status budget per Note, so
// a flapping alert cannot spam followers (cost/noise control, issue #219).
const statusReservation = 1

// statusEnabled reports whether the follow-to-subscribe status feed is active.
func (g *Gateway) statusEnabled() bool { return g.statusLimiter != nil && g.deliverer != nil }

// agentFollowerKey namespaces an agent's followers in the shared store, distinct from group ids.
func agentFollowerKey(ghost string) string { return "agent\x00" + ghost }

// maybeHandleAgentSubscription handles a Follow/Undo to an agent (subscription management) and
// reports whether it consumed the request. A Create falls through to the delegation path.
func (g *Gateway) maybeHandleAgentSubscription(w http.ResponseWriter, r *http.Request, ghost string, body []byte) bool {
	item, err := vocab.UnmarshalJSON(body)
	if err != nil {
		return false
	}
	activity, err := vocab.ToActivity(item)
	if err != nil {
		return false
	}
	actorIRI := activity.Actor.GetLink()
	switch activity.GetType() {
	case vocab.FollowType:
		if actorIRI == "" {
			http.Error(w, "follow has no actor", http.StatusBadRequest)
			return true
		}
		// Transport border (F3 allowlist + F4 signature) gates who may subscribe.
		if g.border != nil {
			if d := g.border.VerifyTransport(r.Context(), r, body, string(actorIRI)); !d.Allowed {
				g.metrics.rejected.WithLabelValues("follow_border_" + d.Reason).Inc()
				http.Error(w, "forbidden", http.StatusForbidden)
				return true
			}
		}
		g.agentFollow(r.Context(), ghost, string(actorIRI), activity)
		w.WriteHeader(http.StatusAccepted)
		return true
	case vocab.UndoType:
		if actorIRI != "" {
			g.followers.remove(agentFollowerKey(ghost), string(actorIRI))
		}
		w.WriteHeader(http.StatusAccepted)
		return true
	default:
		return false
	}
}

// agentFollow records a subscriber and delivers a signed Accept to its inbox.
func (g *Gateway) agentFollow(ctx context.Context, ghost, actorURI string, activity *vocab.Activity) {
	inbox, err := g.fetchInbox(ctx, actorURI)
	if err != nil {
		g.log.Warn("agent follow: cannot resolve follower inbox", "ghost", ghost, "error", err.Error())
		return
	}
	g.followers.add(agentFollowerKey(ghost), actorURI, inbox)

	actor := string(g.actorID(ghost))
	accept := map[string]any{
		"@context": integrity.ActivityStreamsContext,
		"id":       fmt.Sprintf("%s/activities/%d", actor, g.store.next()),
		"type":     "Accept",
		"actor":    actor,
		"object":   activityRef(activity, actorURI),
		"to":       []any{actorURI},
	}
	if raw, err := json.Marshal(accept); err == nil {
		if err := g.deliverer.Deliver(ctx, inbox, actor, raw); err != nil {
			g.log.Warn("agent follow: Accept delivery failed", "ghost", ghost, "error", err.Error())
		}
	}
}

// alertPayload is the subset of the Alertmanager webhook we consume.
type alertPayload struct {
	Alerts []struct {
		Status      string            `json:"status"`
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
	} `json:"alerts"`
}

// AlertsHandler receives Alertmanager webhooks (mounted on the INTERNAL metrics server, never the
// public AP surface) and publishes each firing alert as a signed status Note for the agent named by
// its `agent` label. Alerts without an allowlisted `agent` label are ignored. No outbound webhook is
// introduced: subscribers pull via standard AP Follow (issue #219).
func (g *Gateway) AlertsHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxAlertsBytes))
		if err != nil {
			http.Error(w, "cannot read body", http.StatusBadRequest)
			return
		}
		var payload alertPayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "cannot decode alert payload", http.StatusBadRequest)
			return
		}
		for _, alert := range payload.Alerts {
			if alert.Status != "firing" {
				continue
			}
			ghost := alert.Labels["agent"]
			if _, served := g.registry.Lookup(ghost); !served {
				continue
			}
			g.publishStatus(r.Context(), ghost, statusSummary(alert.Labels, alert.Annotations))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// statusSummary renders a content-free-ish, human-readable status line from an alert.
func statusSummary(labels, annotations map[string]string) string {
	name := labels["alertname"]
	if name == "" {
		name = "alert"
	}
	summary := annotations["summary"]
	if summary == "" {
		summary = annotations["description"]
	}
	if summary == "" {
		return name + " firing"
	}
	return name + ": " + summary
}

// publishStatus mints a public, FEP-8b32-signed status Note authored by an agent, stores it in the
// agent's outbox, and fans it out to the agent's followers — rate-limited so a flapping alert cannot
// spam. A rate-limited status is dropped (logged), never queued unboundedly.
func (g *Gateway) publishStatus(ctx context.Context, ghost, text string) {
	if !g.statusLimiter.Reserve(ghost, ghost, statusReservation, g.statusPool(), g.statusPool()) {
		g.metrics.rejected.WithLabelValues("status_rate_limited").Inc()
		g.log.Warn("status feed rate-limited", "ghost", ghost)
		return
	}
	actor := string(g.actorID(ghost))
	seq := g.store.next()
	note := map[string]any{
		"id":           fmt.Sprintf("%s/objects/%d", actor, seq),
		"type":         "Note",
		"attributedTo": actor,
		"content":      text,
		"to":           []any{publicCollection},
	}
	create := map[string]any{
		"@context": []any{integrity.ActivityStreamsContext, integrity.DataIntegrityContext},
		"id":       fmt.Sprintf("%s/activities/%d", actor, seq),
		"type":     "Create",
		"actor":    actor,
		"object":   note,
		"to":       []any{publicCollection},
	}
	if g.signer != nil {
		if err := g.signer.SignActivity(create, actor); err != nil {
			g.log.Warn("status note signing failed", "ghost", ghost, "error", err.Error())
		}
	}
	raw, err := json.Marshal(create)
	if err != nil {
		return
	}
	g.store.append(ghost, vocab.IRI(create["id"].(string)), raw)
	if inboxes := g.followers.inboxes(agentFollowerKey(ghost), ""); len(inboxes) > 0 {
		g.deliverer.Fanout(ctx, inboxes, actor, raw)
	}
}

// statusPool is the per-agent, per-window status budget (max Notes per window).
func (g *Gateway) statusPool() uint64 { return g.statusMaxPerWindow }
