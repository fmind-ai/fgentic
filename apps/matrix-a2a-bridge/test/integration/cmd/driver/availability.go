package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

const (
	availabilityReplyText    = "load reply room=99 seq=01"
	availabilityPollInterval = 100 * time.Millisecond
	availabilityQuietPeriod  = 3 * time.Second
)

func (f fixture) runAvailability(ctx context.Context) error {
	startedAt := time.Now()
	sess, err := f.register(ctx)
	if err != nil {
		return err
	}
	roomID, err := f.createRoom(ctx, sess.AccessToken, "availability")
	if err != nil {
		return err
	}
	ghost := "@" + ghostLocalpart + ":" + f.server
	if err := f.invite(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	if err := f.waitForJoin(ctx, sess.AccessToken, roomID, ghost); err != nil {
		return err
	}
	baselineMetrics, err := f.fetchBridgeMetrics(ctx)
	if err != nil {
		return fmt.Errorf("read original bridge process metrics: %w", err)
	}

	content := messageContent{
		Body:     ghost + " load room=99 seq=01",
		Mentions: mentions{UserIDs: []string{ghost}},
		MsgType:  "m.text",
	}
	eventID, err := f.sendMessageTxn(
		ctx,
		sess.AccessToken,
		roomID,
		"availability-mention",
		content,
	)
	if err != nil {
		return err
	}

	activeAt, err := f.waitForAvailabilityActive(ctx)
	if err != nil {
		return err
	}
	slog.Info(
		"bridge availability delegation active",
		"availability_phase", "active",
		"active_observed_unix_ms", activeAt.UnixMilli(),
		"a2a_started", 1,
	)

	if err := f.waitForReply(
		ctx,
		sess.AccessToken,
		roomID,
		ghost,
		eventID,
		availabilityReplyText,
	); err != nil {
		return fmt.Errorf("wait for reply during graceful bridge drain: %w", err)
	}
	replyObservedAt := time.Now()
	stats, err := f.waitForAvailabilityCompletion(ctx)
	if err != nil {
		return err
	}
	if err := assertAvailabilityStubStats(stats); err != nil {
		return err
	}

	replacementMetrics, replacementObservedAt, err := f.waitForReplacementProcess(
		ctx,
		baselineMetrics.ProcessStartTime,
	)
	if err != nil {
		return err
	}
	if err := assertReplacementMetrics(replacementMetrics, replacementMetrics.ProcessStartTime, 0); err != nil {
		return fmt.Errorf("replacement bridge before replay: %w", err)
	}
	// The process-start change proves this replay reaches a replacement process. Its only shared
	// dedup state with the drained pod is Postgres.
	if err := f.replayEvent(
		ctx,
		sess.AccessToken,
		"availability-redelivery",
		roomID,
		eventID,
	); err != nil {
		return fmt.Errorf("replay accepted event through replacement bridge: %w", err)
	}

	metrics, err := f.waitForAvailabilityDedup(ctx, replacementMetrics.ProcessStartTime)
	if err != nil {
		return err
	}
	quietDeadline := time.Now().Add(availabilityQuietPeriod)
	for time.Now().Before(quietDeadline) {
		if err := f.assertReplyCount(
			ctx,
			sess.AccessToken,
			roomID,
			ghost,
			eventID,
			availabilityReplyText,
			1,
		); err != nil {
			return fmt.Errorf("exactly-once reply after replacement replay: %w", err)
		}
		stats, err = f.fetchStubStats(ctx)
		if err != nil {
			return err
		}
		if err := assertAvailabilityStubStats(stats); err != nil {
			return fmt.Errorf("exactly-once A2A execution after replacement replay: %w", err)
		}
		metrics, err = f.fetchBridgeMetrics(ctx)
		if err != nil {
			return fmt.Errorf("read replacement bridge metrics during quiet period: %w", err)
		}
		if err := assertReplacementMetrics(metrics, replacementMetrics.ProcessStartTime, 1); err != nil {
			return fmt.Errorf("replacement bridge during quiet period: %w", err)
		}
		if err := wait(ctx, availabilityPollInterval); err != nil {
			return err
		}
	}

	slog.Info(
		"bridge availability scenario passed",
		"availability_phase", "passed",
		"active_observed_unix_ms", activeAt.UnixMilli(),
		"reply_observed_unix_ms", replyObservedAt.UnixMilli(),
		"delivery_gap_ms", replyObservedAt.Sub(activeAt).Milliseconds(),
		"replacement_observed_ms", replacementObservedAt.Sub(activeAt).Milliseconds(),
		"scenario_duration_ms", time.Since(startedAt).Milliseconds(),
		"old_process_start_time_seconds", baselineMetrics.ProcessStartTime,
		"new_process_start_time_seconds", replacementMetrics.ProcessStartTime,
		"a2a_started", stats.TotalStarted,
		"a2a_completed", stats.TotalCompleted,
		"reply_count", 1,
		"dedup_skips", int(metrics.DedupSkips),
		"deduplicated_replay", true,
	)
	return nil
}

func (f fixture) waitForAvailabilityActive(ctx context.Context) (time.Time, error) {
	for {
		stats, statsErr := f.fetchStubStats(ctx)
		if statsErr == nil {
			if stats.TotalStarted > 1 || stats.TotalCompleted > 0 {
				return time.Time{}, fmt.Errorf(
					"A2A call passed the disruption point: active=%d started=%d completed=%d",
					stats.Active,
					stats.TotalStarted,
					stats.TotalCompleted,
				)
			}
			if stats.Active == 1 && stats.TotalStarted == 1 {
				return time.Now(), nil
			}
		}
		if waitErr := wait(ctx, availabilityPollInterval); waitErr != nil {
			if statsErr != nil {
				return time.Time{}, errors.Join(
					fmt.Errorf("last A2A stub stats query: %w", statsErr),
					fmt.Errorf("wait for active A2A call: %w", waitErr),
				)
			}
			return time.Time{}, fmt.Errorf("wait for active A2A call: %w", waitErr)
		}
	}
}

func (f fixture) waitForAvailabilityCompletion(ctx context.Context) (stubStats, error) {
	for {
		stats, statsErr := f.fetchStubStats(ctx)
		if statsErr == nil && stats.Active == 0 && stats.TotalCompleted == 1 {
			return stats, nil
		}
		if statsErr == nil && (stats.TotalStarted > 1 || stats.TotalCompleted > 1) {
			return stubStats{}, fmt.Errorf(
				"A2A execution duplicated while bridge drained: started=%d completed=%d",
				stats.TotalStarted,
				stats.TotalCompleted,
			)
		}
		if waitErr := wait(ctx, availabilityPollInterval); waitErr != nil {
			if statsErr != nil {
				return stubStats{}, errors.Join(
					fmt.Errorf("last A2A stub stats query: %w", statsErr),
					fmt.Errorf("wait for A2A completion: %w", waitErr),
				)
			}
			return stubStats{}, fmt.Errorf("wait for A2A completion: %w", waitErr)
		}
	}
}

func (f fixture) waitForReplacementProcess(
	ctx context.Context,
	originalStartTime float64,
) (bridgeMetrics, time.Time, error) {
	for {
		metrics, metricsErr := f.fetchBridgeMetrics(ctx)
		if metricsErr == nil && metrics.ProcessStartTime > originalStartTime {
			return metrics, time.Now(), nil
		}
		if waitErr := wait(ctx, availabilityPollInterval); waitErr != nil {
			if metricsErr != nil {
				return bridgeMetrics{}, time.Time{}, errors.Join(
					fmt.Errorf("last replacement bridge metrics query: %w", metricsErr),
					fmt.Errorf("wait for replacement bridge process: %w", waitErr),
				)
			}
			return bridgeMetrics{}, time.Time{}, fmt.Errorf(
				"wait for replacement bridge process (start_time=%f, original=%f): %w",
				metrics.ProcessStartTime,
				originalStartTime,
				waitErr,
			)
		}
	}
}

func (f fixture) waitForAvailabilityDedup(
	ctx context.Context,
	replacementStartTime float64,
) (bridgeMetrics, error) {
	for {
		metrics, metricsErr := f.fetchBridgeMetrics(ctx)
		if metricsErr == nil && metrics.ProcessStartTime != replacementStartTime {
			return bridgeMetrics{}, fmt.Errorf(
				"bridge process changed again during replay: start_time=%f, want %f",
				metrics.ProcessStartTime,
				replacementStartTime,
			)
		}
		if metricsErr == nil && metrics.DedupSkips == 1 {
			return metrics, nil
		}
		if metricsErr == nil && metrics.DedupSkips > 1 {
			return bridgeMetrics{}, fmt.Errorf(
				"replacement bridge dedup skips = %.0f, want exactly 1",
				metrics.DedupSkips,
			)
		}
		if waitErr := wait(ctx, availabilityPollInterval); waitErr != nil {
			if metricsErr != nil {
				return bridgeMetrics{}, errors.Join(
					fmt.Errorf("last bridge metrics query: %w", metricsErr),
					fmt.Errorf("wait for replacement deduplication: %w", waitErr),
				)
			}
			return bridgeMetrics{}, fmt.Errorf(
				"wait for replacement deduplication (dedup_skips=%.0f): %w",
				metrics.DedupSkips,
				waitErr,
			)
		}
	}
}

func assertAvailabilityStubStats(stats stubStats) error {
	if stats.DelayMillis != 0 || !stats.HoldEnabled {
		return fmt.Errorf(
			"A2A disruption gate delay=%dms hold_enabled=%t, want 0ms/true",
			stats.DelayMillis,
			stats.HoldEnabled,
		)
	}
	if stats.Active != 0 || stats.TotalRequests != 1 || stats.TotalStarted != 1 || stats.TotalCompleted != 1 {
		return fmt.Errorf(
			"A2A executions active=%d requests=%d started=%d completed=%d, want 0/1/1/1",
			stats.Active,
			stats.TotalRequests,
			stats.TotalStarted,
			stats.TotalCompleted,
		)
	}
	want := stubRequestRecord{Room: 99, Sequence: 1}
	if len(stats.Starts) != 1 || stats.Starts[0] != want {
		return fmt.Errorf("A2A starts = %+v, want [%+v]", stats.Starts, want)
	}
	if len(stats.Completions) != 1 || stats.Completions[0] != want {
		return fmt.Errorf("A2A completions = %+v, want [%+v]", stats.Completions, want)
	}
	return nil
}

func assertReplacementMetrics(metrics bridgeMetrics, processStartTime, dedupSkips float64) error {
	if metrics.ProcessStartTime != processStartTime {
		return fmt.Errorf(
			"process start time = %f, want %f",
			metrics.ProcessStartTime,
			processStartTime,
		)
	}
	if metrics.DedupSkips != dedupSkips {
		return fmt.Errorf("dedup skips = %.0f, want %.0f", metrics.DedupSkips, dedupSkips)
	}
	return nil
}
