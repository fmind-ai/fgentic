package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"
)

const (
	synapseRestartPollInterval = 200 * time.Millisecond
	synapseRestartReachTimeout = 300 * time.Second
	synapseRestartWorkingText  = "⏳ working on it…"
	synapseRestartLongReply    = "long reply room=99 seq=01"
	synapseRestartQuietPeriod  = 2 * time.Second
)

// runSynapseRestart proves the dependency-outage contract for a Synapse homeserver restart (#466):
// a long task is held mid-poll with a pending reply, run.sh performs a real rollout restart of the
// single-replica homeserver, and after it returns the bridge delivers exactly one reply. Replaying
// the original appservice transaction after recovery must dedup cleanly. The driver detects the
// homeserver's real down->up transition (Synapse uses a Recreate strategy, so the restart is an
// observable outage window) rather than sleeping, then releases the held task.
func (f fixture) runSynapseRestart(ctx context.Context) error {
	startedAt := time.Now()
	sess, err := f.register(ctx)
	if err != nil {
		return err
	}
	ghost := "@" + ghostLocalpart + ":" + f.server
	roomID, err := f.createRoom(ctx, sess.AccessToken, "synapse-restart")
	if err != nil {
		return err
	}
	if err := f.invite(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	if err := f.waitForJoin(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	if err := f.waitForDisplayName(ctx, sess.AccessToken, ghost, ghostDisplayName); err != nil {
		return err
	}

	mentionEventID, err := f.sendMessageTxn(ctx, sess.AccessToken, roomID, "synapse-restart-mention",
		messageContent{
			Body:     ghost + " long room=99 seq=01",
			Mentions: mentions{UserIDs: []string{ghost}},
			MsgType:  "m.text",
		})
	if err != nil {
		return err
	}
	// The held long task posts a working placeholder before it blocks on the release gate: the task is
	// now mid-poll with a reply pending, which is the state the restart must survive.
	placeholderID, err := f.waitForReplyEvent(
		ctx, sess.AccessToken, roomID, ghost, mentionEventID, synapseRestartWorkingText,
	)
	if err != nil {
		return fmt.Errorf("await long-task placeholder: %w", err)
	}

	slog.Info(
		"synapse restart armed with a task mid-poll",
		"synapse_restart_phase", "task_polling",
		"placeholder_event_id", placeholderID,
	)
	if err := f.waitForSynapseRestart(ctx); err != nil {
		return err
	}

	// The homeserver is back: release the held reply and require exactly one delivered edit.
	if err := f.releaseLongTask(ctx); err != nil {
		return err
	}
	if err := f.waitForSingleLongReply(ctx, sess.AccessToken, roomID, ghost, placeholderID); err != nil {
		return fmt.Errorf("await exactly-once reply after homeserver restart: %w", err)
	}
	// Appservice transaction replay must dedup: the same mention re-pushed produces no second reply.
	if err := f.replayEvent(ctx, sess.AccessToken, "synapse-restart-redelivery", roomID, mentionEventID); err != nil {
		return fmt.Errorf("replay mention after restart: %w", err)
	}
	quietDeadline := time.Now().Add(synapseRestartQuietPeriod)
	for time.Now().Before(quietDeadline) {
		count, err := f.countLongReplyEdits(ctx, sess.AccessToken, roomID, ghost, placeholderID)
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("long reply edits = %d after transaction replay, want exactly 1", count)
		}
		if err := wait(ctx, synapseRestartPollInterval); err != nil {
			return err
		}
	}

	slog.Info(
		"bridge synapse-restart scenario passed",
		"synapse_restart_phase", "passed",
		"placeholder_event_id", placeholderID,
		"reply_count", 1,
		"deduplicated_replay", true,
		"scenario_duration_ms", time.Since(startedAt).Milliseconds(),
	)
	return nil
}

// waitForSynapseRestart blocks until the homeserver has gone unreachable (the restart began) and then
// become reachable again (the replacement is ready). Requiring the down transition proves the restart
// actually happened before the held reply is released.
func (f fixture) waitForSynapseRestart(ctx context.Context) error {
	downDeadline := time.Now().Add(synapseRestartReachTimeout)
	for f.matrixReachable(ctx) {
		if time.Now().After(downDeadline) {
			return errors.New("synapse never became unreachable; the restart window was not observed")
		}
		if err := wait(ctx, synapseRestartPollInterval); err != nil {
			return fmt.Errorf("wait for homeserver downtime: %w", err)
		}
	}
	slog.Info("synapse restart observed downtime", "synapse_restart_phase", "homeserver_down")

	upDeadline := time.Now().Add(synapseRestartReachTimeout)
	for !f.matrixReachable(ctx) {
		if time.Now().After(upDeadline) {
			return errors.New("synapse did not become reachable again after the restart")
		}
		if err := wait(ctx, synapseRestartPollInterval); err != nil {
			return fmt.Errorf("wait for homeserver recovery: %w", err)
		}
	}
	slog.Info("synapse restart observed recovery", "synapse_restart_phase", "homeserver_up")
	return nil
}

func (f fixture) matrixReachable(ctx context.Context) bool {
	status, _, err := f.request(ctx, http.MethodGet, f.matrixURL+"/_matrix/client/versions", "", nil)
	return err == nil && status == http.StatusOK
}

func (f fixture) waitForSingleLongReply(ctx context.Context, token, roomID, ghost, placeholderID string) error {
	for {
		count, err := f.countLongReplyEdits(ctx, token, roomID, ghost, placeholderID)
		if err == nil && count == 1 {
			return nil
		}
		if err == nil && count > 1 {
			return fmt.Errorf("long reply edits = %d, want exactly 1", count)
		}
		if waitErr := wait(ctx, synapseRestartPollInterval); waitErr != nil {
			if err != nil {
				return errors.Join(err, waitErr)
			}
			return fmt.Errorf("wait for long reply edit: %w", waitErr)
		}
	}
}

func (f fixture) countLongReplyEdits(ctx context.Context, token, roomID, ghost, placeholderID string) (int, error) {
	events, err := f.roomMessages(ctx, token, roomID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, evt := range events {
		if evt.Type != "m.room.message" || evt.Sender != ghost {
			continue
		}
		var content struct {
			RelatesTo struct {
				RelType string `json:"rel_type"`
				EventID string `json:"event_id"`
			} `json:"m.relates_to"`
			NewContent struct {
				Body string `json:"body"`
			} `json:"m.new_content"`
		}
		if err := json.Unmarshal(evt.Content, &content); err != nil {
			return 0, fmt.Errorf("decode long reply edit %s: %w", evt.EventID, err)
		}
		if content.RelatesTo.RelType == "m.replace" && content.RelatesTo.EventID == placeholderID &&
			content.NewContent.Body == synapseRestartLongReply {
			count++
		}
	}
	return count, nil
}
