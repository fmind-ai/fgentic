package apgateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/activitystate"
)

const inboxPollInterval = 250 * time.Millisecond

const inboxPruneInterval = time.Minute

// RunActivityProcessor drains the durable inbox ledger until ctx is cancelled. Exactly one runner
// is allowed per Gateway; the chart's single replica and Postgres' atomic claim also keep one unique
// activity from creating concurrent delegation attempts.
func (g *Gateway) RunActivityProcessor(ctx context.Context) error {
	done, err := g.StartActivityProcessor(ctx)
	if err != nil {
		return err
	}
	return <-done
}

// StartActivityProcessor terminalizes interrupted work synchronously, then starts the durable drain.
// Returning only after setup guarantees the HTTP servers cannot accept work into an unstarted queue.
func (g *Gateway) StartActivityProcessor(ctx context.Context) (<-chan error, error) {
	if !g.inboxAsync.CompareAndSwap(false, true) {
		return nil, errors.New("gateway: activity processor already running")
	}
	if err := g.inboxState.FailRunning(ctx); err != nil {
		g.inboxAsync.Store(false)
		return nil, fmt.Errorf("start activity processor: %w", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- g.runActivityProcessor(ctx)
		close(done)
	}()
	return done, nil
}

func (g *Gateway) runActivityProcessor(ctx context.Context) error {
	ticker := time.NewTicker(inboxPollInterval)
	defer ticker.Stop()
	pruneTicker := time.NewTicker(inboxPruneInterval)
	defer pruneTicker.Stop()
	for {
		job, claimed, err := g.inboxState.Claim(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return fmt.Errorf("claim activity: %w", err)
		}
		if claimed {
			if err := g.processActivity(ctx, job); err != nil {
				return err
			}
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-g.inboxWake:
		case <-ticker.C:
		case <-pruneTicker.C:
			if err := g.inboxState.Prune(ctx); err != nil {
				return fmt.Errorf("prune activity outcomes: %w", err)
			}
		}
	}
}

func (g *Gateway) acceptActivity(w http.ResponseWriter, r *http.Request, job activitystate.Job) {
	record, inserted, err := g.inboxState.Enqueue(r.Context(), job)
	if err != nil {
		if errors.Is(err, activitystate.ErrConflict) {
			g.metrics.rejected.WithLabelValues("activity_id_conflict").Inc()
			http.Error(w, "activity id conflicts with prior delivery", http.StatusConflict)
			return
		}
		if errors.Is(err, activitystate.ErrCapacity) {
			g.metrics.rejected.WithLabelValues("activity_queue_full").Inc()
			w.Header().Set("Retry-After", "1")
			http.Error(w, "activity queue is full", http.StatusServiceUnavailable)
			return
		}
		g.log.Error("enqueue inbound activity", "route", job.Route, "target", job.Target, "error", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	if !inserted {
		g.metrics.delegations.WithLabelValues(job.Target, "duplicate").Inc()
		if g.inboxAsync.Load() {
			g.writeAsyncAccepted(w, record)
		} else {
			writeAcceptedRecord(w, record)
		}
		return
	}

	if g.inboxAsync.Load() {
		select {
		case g.inboxWake <- struct{}{}:
		default:
		}
		g.writeAsyncAccepted(w, record)
		return
	}

	// Tests and embedders that do not start the production runner retain deterministic inline
	// processing. The shipped command always starts RunActivityProcessor before serving HTTP.
	if err := g.processActivity(r.Context(), job); err != nil {
		g.log.Error("process inbound activity", "route", job.Route, "target", job.Target, "error", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	record, _, err = g.inboxState.Enqueue(r.Context(), job)
	if err != nil {
		g.log.Error("reload processed activity", "route", job.Route, "target", job.Target, "error", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	writeAcceptedRecord(w, record)
}

func (g *Gateway) rememberIgnored(ctx context.Context, activityID, target, actorURI string, body []byte) (activitystate.Record, error) {
	job := activitystate.Job{
		ActivityID: activityID,
		Route:      activitystate.RouteAgent,
		Target:     target,
		ActorURI:   actorURI,
		Body:       body,
	}
	record, _, err := g.inboxState.Ignore(ctx, job)
	return record, err
}

func writeAcceptedRecord(w http.ResponseWriter, record activitystate.Record) {
	if record.Location != "" {
		w.Header().Set("Location", record.Location)
	}
	w.WriteHeader(http.StatusAccepted)
}

func (g *Gateway) writeAsyncAccepted(w http.ResponseWriter, record activitystate.Record) {
	w.Header().Set("Location", g.baseURL+"/ap/inbox-status/"+record.StatusToken)
	w.WriteHeader(http.StatusAccepted)
}

// handleActivityStatus serves the durable cached outcome through an unguessable capability URL.
// Pending states and terminal non-reply outcomes are content-free; a successful direct delegation
// returns the exact persisted Activity bytes, including its object-integrity proof.
func (g *Gateway) handleActivityStatus(w http.ResponseWriter, r *http.Request) {
	record, err := g.inboxState.LookupStatus(r.Context(), r.PathValue("token"))
	if errors.Is(err, activitystate.ErrNotFound) {
		http.Error(w, "no such activity status", http.StatusNotFound)
		return
	}
	if err != nil {
		g.log.Error("lookup activity status", "error", err)
		http.Error(w, "temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if record.State == activitystate.StateSucceeded && len(record.Result) > 0 {
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(record.Result)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if record.State == activitystate.StatePending || record.State == activitystate.StateRunning {
		w.WriteHeader(http.StatusAccepted)
	}
	_ = json.NewEncoder(w).Encode(map[string]activitystate.State{"state": record.State})
}

func (g *Gateway) processActivity(ctx context.Context, job activitystate.Job) error {
	completion := g.executeActivity(ctx, job)
	completeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := g.inboxState.Complete(completeCtx, job.ActivityID, completion); err != nil {
		return fmt.Errorf("complete inbound activity %q as %s: %w", job.ActivityID, completion.State, err)
	}
	return nil
}

func (g *Gateway) executeActivity(ctx context.Context, job activitystate.Job) activitystate.Completion {
	switch job.Route {
	case activitystate.RouteAgent:
		return g.executeAgentActivity(ctx, job)
	case activitystate.RouteGroup:
		return g.executeGroupActivity(ctx, job)
	default:
		g.log.Error("unknown durable activity route", "route", job.Route)
		return activitystate.Completion{State: activitystate.StateFailed}
	}
}

func (g *Gateway) executeAgentActivity(ctx context.Context, job activitystate.Job) activitystate.Completion {
	activity, note, err := parseCreateNote(job.Body)
	if err != nil || string(activity.ID) != job.ActivityID || string(activity.Actor.GetLink()) != job.ActorURI {
		g.log.Error("durable agent activity changed after intake", "ghost", job.Target)
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	if !g.mentions(job.Target, note) {
		g.metrics.delegations.WithLabelValues(job.Target, "not_mentioned").Inc()
		return activitystate.Completion{State: activitystate.StateIgnored}
	}
	ref, served := g.registry.Lookup(job.Target)
	if !served {
		g.log.Warn("durable activity target was removed", "ghost", job.Target)
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	if !g.reserveBudget(job.Target, job.ActorURI) {
		return activitystate.Completion{State: activitystate.StateDenied}
	}

	contextID := deriveContextID(job.Target, job.ActorURI, threadRoot(note))
	network := actorDomain(job.ActorURI)
	callCtx := a2aAttribution(ctx, job.ActorURI, network)
	reply, err := g.delegator.Call(callCtx, ref.Namespace, ref.Name, nlvText(note.Content), contextID)
	if err != nil {
		g.metrics.delegations.WithLabelValues(job.Target, "error").Inc()
		g.auditDelegation(job.Target, job.ActorURI, network, contextID, "error")
		g.log.Error("a2a delegation failed", "ghost", job.Target, "error", err)
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	g.auditDelegation(job.Target, job.ActorURI, network, contextID, "ok")
	g.metrics.delegations.WithLabelValues(job.Target, "ok").Inc()

	replyID, raw, err := g.publishReply(job.ActivityID, job.Target, activity.Actor.GetLink(), note.ID, reply)
	if err != nil {
		g.log.Error("publish reply", "ghost", job.Target, "error", err)
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	return activitystate.Completion{State: activitystate.StateSucceeded, Location: string(replyID), Result: raw}
}

func (g *Gateway) executeGroupActivity(ctx context.Context, job activitystate.Job) activitystate.Completion {
	item, err := vocab.UnmarshalJSON(job.Body)
	if err != nil {
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	activity, err := vocab.ToActivity(item)
	if err != nil || string(activity.ID) != job.ActivityID || string(activity.Actor.GetLink()) != job.ActorURI {
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	note, err := vocab.ToObject(activity.Object)
	if err != nil || note.GetType() != vocab.NoteType {
		return activitystate.Completion{State: activitystate.StateFailed}
	}
	g.groupCreate(ctx, job.Target, job.ActorURI, note)
	return activitystate.Completion{State: activitystate.StateSucceeded}
}

func (g *Gateway) reserveBudget(target, actorURI string) bool {
	if g.border == nil {
		return true
	}
	decision := g.border.ReserveBudget(actorURI, "none")
	if decision.BudgetOutcome != "" {
		g.metrics.reservations.WithLabelValues(target, decision.BudgetOutcome).Inc()
	}
	if decision.Allowed {
		return true
	}
	g.metrics.rejected.WithLabelValues("activity_" + decision.Reason).Inc()
	g.log.Warn("federation budget denied durable activity", "target", target, "reason", decision.Reason, "policy", decision.Digest)
	return false
}

func validateActivityID(id vocab.IRI) error {
	if id == "" || len(id) > 4096 {
		return errors.New("activity id is required and must be at most 4096 bytes")
	}
	parsed, err := url.Parse(string(id))
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") || parsed.Host == "" || parsed.User != nil {
		return errors.New("activity id must be an absolute https URL without userinfo")
	}
	return nil
}
