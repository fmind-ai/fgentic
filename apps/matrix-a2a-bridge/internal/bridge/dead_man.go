package bridge

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/appservice"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

const (
	delayedEventsFeature      = "org.matrix.msc4140"
	delayedEventsUnstablePath = "org.matrix.msc4140"
	deadManTransactionStage   = "deadman"
	deadManNoticeText         = "⚠️ this task's bridge stopped responding; treat the placeholder as stale."
	minimumDeadManSwitchDelay = 2 * time.Minute
)

type deadManClient interface {
	Supported(context.Context) (bool, error)
	Schedule(context.Context, *appservice.IntentAPI, id.RoomID, id.EventID, string, time.Duration) (id.DelayID, error)
	Restart(context.Context, *appservice.IntentAPI, id.DelayID) error
	Cancel(context.Context, *appservice.IntentAPI, id.DelayID) error
}

type matrixDeadManClient struct {
	as *appservice.AppService
}

func (c *matrixDeadManClient) Supported(ctx context.Context) (bool, error) {
	versions, err := c.as.BotClient().Versions(ctx)
	if err != nil {
		return false, fmt.Errorf("query Matrix versions: %w", err)
	}
	return versions.UnstableFeatures[delayedEventsFeature], nil
}

func (c *matrixDeadManClient) Schedule(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	placeholder id.EventID,
	txnID string,
	delay time.Duration,
) (id.DelayID, error) {
	content := &event.MessageEventContent{MsgType: event.MsgNotice, Body: deadManNoticeText}
	content.GetRelatesTo().SetReplyTo(placeholder)
	resp, err := intent.SendMessageEvent(
		ctx,
		roomID,
		event.EventMessage,
		automatedContent(content),
		mautrix.ReqSendEvent{TransactionID: txnID, UnstableDelay: delay},
	)
	if err != nil {
		return "", fmt.Errorf("schedule delayed stale-task notice: %w", err)
	}
	if resp == nil || resp.UnstableDelayID == "" {
		return "", errors.New("schedule delayed stale-task notice: homeserver omitted delay_id")
	}
	return resp.UnstableDelayID, nil
}

func (c *matrixDeadManClient) Restart(
	ctx context.Context,
	intent *appservice.IntentAPI,
	delayID id.DelayID,
) error {
	return c.update(ctx, intent, delayID, event.DelayActionRestart)
}

func (c *matrixDeadManClient) Cancel(
	ctx context.Context,
	intent *appservice.IntentAPI,
	delayID id.DelayID,
) error {
	err := c.update(ctx, intent, delayID, event.DelayActionCancel)
	if errors.Is(err, mautrix.MNotFound) {
		return nil // already fired or finalised; cancellation is idempotent for bridge cleanup
	}
	return err
}

// update owns the only unstable management path. Synapse 1.155.0 accepts action-suffixed POSTs;
// when MSC4140 stabilizes, this is the single endpoint boundary that must change.
func (c *matrixDeadManClient) update(
	ctx context.Context,
	intent *appservice.IntentAPI,
	delayID id.DelayID,
	action event.DelayAction,
) error {
	url := intent.BuildClientURL("unstable", delayedEventsUnstablePath, "delayed_events", delayID, action)
	if _, err := intent.MakeRequest(ctx, http.MethodPost, url, struct{}{}, nil); err != nil {
		return fmt.Errorf("%s delayed stale-task notice: %w", action, err)
	}
	return nil
}

type armedDeadMan struct {
	delayID     id.DelayID
	lastRestart time.Time
}

func (b *Bridge) probeDeadMan(ctx context.Context) {
	if b.cfg.DeadManSwitchDelay == 0 {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, b.cfg.RequestTimeout)
	defer cancel()
	supported, err := b.deadMan.Supported(probeCtx)
	if err != nil {
		b.log.Warn("Matrix delayed-event probe failed; task dead-man switch disabled", "reason", "probe_failed")
		return
	}
	if !supported {
		b.log.Info("Matrix delayed events unsupported; task dead-man switch disabled")
		return
	}
	b.deadManEnabled = true
	b.log.Info("Matrix delayed-event task dead-man switch enabled", "delay", b.cfg.DeadManSwitchDelay)
}

func (b *Bridge) armDeadMan(
	ctx context.Context,
	intent *appservice.IntentAPI,
	roomID id.RoomID,
	placeholder id.EventID,
	taskID string,
) *armedDeadMan {
	if !b.deadManEnabled || placeholder == "" {
		return nil
	}
	jobID := state.JobIDFor(placeholder.String(), intent.UserID.String())
	txnID := state.MatrixTransactionIDFor(jobID, deadManTransactionStage)
	delayID, err := b.deadMan.Schedule(ctx, intent, roomID, placeholder, txnID, b.cfg.DeadManSwitchDelay)
	if err != nil {
		b.log.Warn("schedule task dead-man switch", "task", taskID, "room", roomID, "reason", "matrix_delayed_event_failed")
		return nil
	}
	return &armedDeadMan{delayID: delayID, lastRestart: b.deadManNow()}
}

func (b *Bridge) restartDeadManOnPoll(
	ctx context.Context,
	intent *appservice.IntentAPI,
	armed *armedDeadMan,
	taskID string,
) {
	if armed == nil || b.deadManNow().Sub(armed.lastRestart) < b.deadManRefreshInterval() {
		return
	}
	armed.lastRestart = b.deadManNow() // bound retries even when Synapse rate-limits or is unavailable
	if err := b.deadMan.Restart(ctx, intent, armed.delayID); err != nil {
		b.log.Warn("restart task dead-man switch", "task", taskID, "reason", "matrix_delayed_event_failed")
	}
}

func (b *Bridge) cancelDeadMan(
	ctx context.Context,
	intent *appservice.IntentAPI,
	armed *armedDeadMan,
	taskID string,
) {
	if armed == nil {
		return
	}
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.RequestTimeout)
	defer cancel()
	if err := b.deadMan.Cancel(cancelCtx, intent, armed.delayID); err != nil {
		b.log.Warn("cancel task dead-man switch", "task", taskID, "reason", "matrix_delayed_event_failed")
	}
}

func (b *Bridge) deadManRefreshInterval() time.Duration {
	return b.cfg.DeadManSwitchDelay / 2
}

func (b *Bridge) ensureDurableDeadMan(
	ctx context.Context,
	job *state.Job,
	evt *event.Event,
) error {
	if !b.deadManEnabled || job.MatrixPlaceholderEventID == "" || job.MatrixDeadManDelayID != "" {
		return nil
	}
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	delayID, err := b.deadMan.Schedule(
		ctx,
		intent,
		evt.RoomID,
		id.EventID(job.MatrixPlaceholderEventID),
		state.MatrixTransactionIDFor(job.JobID, deadManTransactionStage),
		b.cfg.DeadManSwitchDelay,
	)
	if err != nil {
		b.log.Warn("schedule durable task dead-man switch", "job_id", job.JobID, "reason", "matrix_delayed_event_failed")
		return nil // optional enhancement: a Matrix feature outage never blocks A2A recovery
	}
	now := time.Now().UTC()
	if err := b.store.RecordDeadMan(ctx, state.DeadManRequest{
		Lease: job.LeaseToken(), At: now, DelayID: string(delayID),
	}); err != nil {
		recordErr := fmt.Errorf("record durable task dead-man switch: %w", err)
		if cancelErr := b.cancelDurableDeadManID(ctx, job, delayID); cancelErr != nil {
			return errors.Join(recordErr, fmt.Errorf("cancel unpersisted task dead-man switch: %w", cancelErr))
		}
		return recordErr
	}
	job.MatrixDeadManDelayID = string(delayID)
	return nil
}

func (b *Bridge) restartDurableDeadManOnPoll(ctx context.Context, job *state.Job) {
	if !b.deadManEnabled || job.MatrixDeadManDelayID == "" || !b.durableDeadManRefreshDue(*job) {
		return
	}
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	if err := b.deadMan.Restart(ctx, intent, id.DelayID(job.MatrixDeadManDelayID)); err != nil {
		b.log.Warn("restart durable task dead-man switch", "job_id", job.JobID, "reason", "matrix_delayed_event_failed")
	}
}

func (b *Bridge) durableDeadManRefreshDue(job state.Job) bool {
	if job.PollCount < 1 || b.pollMax <= 0 {
		return false
	}
	refresh := b.deadManRefreshInterval()
	polls := max(1, int((refresh+b.pollMax-1)/b.pollMax))
	return job.PollCount%polls == 0
}

func (b *Bridge) cancelDurableDeadMan(ctx context.Context, job *state.Job) error {
	if job.MatrixDeadManDelayID == "" {
		return nil
	}
	return b.cancelDurableDeadManID(ctx, job, id.DelayID(job.MatrixDeadManDelayID))
}

func (b *Bridge) cancelDurableDeadManID(ctx context.Context, job *state.Job, delayID id.DelayID) error {
	cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), b.cfg.RequestTimeout)
	defer cancel()
	intent := b.as.Intent(id.UserID(job.GhostMXID))
	if err := b.deadMan.Cancel(cancelCtx, intent, delayID); err != nil {
		return fmt.Errorf("cancel durable task dead-man switch: %w", err)
	}
	return nil
}
