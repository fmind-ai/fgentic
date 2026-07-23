package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultLoadRooms         = 10
	defaultLoadMessages      = 10
	defaultConcurrencyCap    = 16
	defaultMaxHeapBytes      = 64 << 20
	defaultMaxTransactionAck = 10 * time.Second
	loadMetricSampleInterval = 100 * time.Millisecond
	loadPollInterval         = 250 * time.Millisecond
	loadReplyQuietPeriod     = 3 * time.Second
	maxLoadMentions          = 200
)

type loadConfig struct {
	Rooms             int
	MessagesPerRoom   int
	ConcurrencyCap    int
	MaxHeapBytes      uint64
	MaxTransactionAck time.Duration
}

type loadMessage struct {
	RoomIndex int
	Sequence  int
	RoomID    string
	EventID   string
	Content   messageContent
}

type stubRequestRecord struct {
	Room     int `json:"room"`
	Sequence int `json:"sequence"`
}

type stubStats struct {
	Active             int                 `json:"active"`
	CardTampered       bool                `json:"card_tampered"`
	DelayMillis        int64               `json:"delay_millis"`
	HoldEnabled        bool                `json:"hold_enabled"`
	MaxActive          int                 `json:"max_active"`
	RemoteCardRequests int                 `json:"remote_card_requests"`
	RemoteRequests     int                 `json:"remote_requests"`
	RemoteUserID       string              `json:"remote_user_id"`
	TokenBudgetValid   bool                `json:"token_budget_valid"`
	TotalRequests      int                 `json:"total_requests"`
	TotalStarted       int                 `json:"total_started"`
	TotalCompleted     int                 `json:"total_completed"`
	LongStarted        int                 `json:"long_started"`
	LongCompleted      int                 `json:"long_completed"`
	InputStarted       int                 `json:"input_started"`
	InputContinued     int                 `json:"input_continued"`
	CancelRequests     int                 `json:"cancel_requests"`
	Starts             []stubRequestRecord `json:"starts"`
	Completions        []stubRequestRecord `json:"completions"`
}

type bridgeMetrics struct {
	HeapAlloc        float64
	HeapInUse        float64
	HeapSys          float64
	QueueDepth       float64
	InFlight         float64
	DedupSkips       float64
	ProcessStartTime float64
}

type metricProfile struct {
	Baseline bridgeMetrics
	Peak     bridgeMetrics
	Final    bridgeMetrics
}

type metricSampler struct {
	fixture fixture
	cancel  context.CancelFunc
	done    chan struct{}

	mu      sync.Mutex
	profile metricProfile
	err     error
}

func (f fixture) runLoad(ctx context.Context) error {
	cfg, err := loadConfigFromEnv()
	if err != nil {
		return err
	}
	startedAt := time.Now()
	sess, err := f.register(ctx)
	if err != nil {
		return err
	}
	ghost := "@" + ghostLocalpart + ":" + f.server
	rooms, err := f.createLoadRooms(ctx, sess.AccessToken, ghost, cfg.Rooms)
	if err != nil {
		return err
	}

	sampler, err := startMetricSampler(ctx, f)
	if err != nil {
		return err
	}
	samplerFinished := false
	defer func() {
		if !samplerFinished {
			sampler.abort()
		}
	}()

	messages, sendDuration, err := f.sendLoadMessages(ctx, sess.AccessToken, ghost, rooms, cfg.MessagesPerRoom)
	if err != nil {
		return err
	}
	if err := f.waitUntilLoadEnqueued(ctx, len(messages)); err != nil {
		return err
	}
	transactionAck, err := f.replayLoadTransaction(ctx, sess.AccessToken, messages)
	if err != nil {
		return err
	}
	if transactionAck > cfg.MaxTransactionAck {
		return fmt.Errorf("appservice transaction acknowledgement took %s, limit %s", transactionAck, cfg.MaxTransactionAck)
	}

	if err := f.waitForLoadReplies(ctx, sess.AccessToken, ghost, messages); err != nil {
		return err
	}
	if err := wait(ctx, loadReplyQuietPeriod); err != nil {
		return err
	}
	if err := f.assertLoadReplies(ctx, sess.AccessToken, ghost, messages); err != nil {
		return fmt.Errorf("exactly-once reply check after quiet period: %w", err)
	}

	stats, finalMetrics, err := f.waitForLoadDrain(ctx, len(messages))
	if err != nil {
		return err
	}
	profile, err := sampler.finish(ctx)
	samplerFinished = true
	if err != nil {
		return err
	}
	profile.Final = finalMetrics
	if err := assertLoadInvariants(cfg, stats, profile, len(messages)); err != nil {
		return err
	}

	slogLoadResult(cfg, stats, profile, len(messages), sendDuration, transactionAck, time.Since(startedAt))
	return nil
}

func loadConfigFromEnv() (loadConfig, error) {
	rooms, err := envPositiveInt("LOAD_ROOMS", defaultLoadRooms)
	if err != nil {
		return loadConfig{}, err
	}
	messages, err := envPositiveInt("LOAD_MESSAGES_PER_ROOM", defaultLoadMessages)
	if err != nil {
		return loadConfig{}, err
	}
	concurrency, err := envPositiveInt("LOAD_CONCURRENCY_CAP", defaultConcurrencyCap)
	if err != nil {
		return loadConfig{}, err
	}
	maxHeap, err := envPositiveUint64("LOAD_MAX_HEAP_BYTES", defaultMaxHeapBytes)
	if err != nil {
		return loadConfig{}, err
	}
	maxAck, err := envDuration("LOAD_MAX_TRANSACTION_ACK", defaultMaxTransactionAck)
	if err != nil {
		return loadConfig{}, err
	}
	if rooms*messages > maxLoadMentions {
		return loadConfig{}, fmt.Errorf("load mentions %d exceed resource-safe limit %d", rooms*messages, maxLoadMentions)
	}
	return loadConfig{
		Rooms:             rooms,
		MessagesPerRoom:   messages,
		ConcurrencyCap:    concurrency,
		MaxHeapBytes:      maxHeap,
		MaxTransactionAck: maxAck,
	}, nil
}

func (f fixture) createLoadRooms(ctx context.Context, token, ghost string, count int) ([]string, error) {
	rooms := make([]string, 0, count)
	for roomIndex := range count {
		roomID, err := f.createRoom(ctx, token, fmt.Sprintf("load-%d", roomIndex))
		if err != nil {
			return nil, fmt.Errorf("create load room %d: %w", roomIndex, err)
		}
		if err := f.invite(ctx, token, roomID, ghost); err != nil {
			return nil, fmt.Errorf("invite ghost to load room %d: %w", roomIndex, err)
		}
		if err := f.waitForJoin(ctx, token, roomID, ghost); err != nil {
			return nil, fmt.Errorf("wait for ghost in load room %d: %w", roomIndex, err)
		}
		rooms = append(rooms, roomID)
	}
	return rooms, nil
}

func (f fixture) sendLoadMessages(
	ctx context.Context,
	token, ghost string,
	rooms []string,
	messagesPerRoom int,
) ([]loadMessage, time.Duration, error) {
	total := len(rooms) * messagesPerRoom
	messages := make([]loadMessage, total)
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	start := make(chan struct{})
	errCh := make(chan error, len(rooms))
	var wg sync.WaitGroup

	for roomIndex, roomID := range rooms {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case <-start:
			case <-sendCtx.Done():
				return
			}
			for sequence := range messagesPerRoom {
				content := messageContent{
					Body:     fmt.Sprintf("%s load room=%02d seq=%02d", ghost, roomIndex, sequence),
					Mentions: mentions{UserIDs: []string{ghost}},
					MsgType:  "m.text",
				}
				transactionID := fmt.Sprintf("load-%02d-%02d", roomIndex, sequence)
				eventID, err := f.sendMessageTxn(sendCtx, token, roomID, transactionID, content)
				if err != nil {
					errCh <- fmt.Errorf("send room %d sequence %d: %w", roomIndex, sequence, err)
					cancel()
					return
				}
				messages[roomIndex*messagesPerRoom+sequence] = loadMessage{
					RoomIndex: roomIndex,
					Sequence:  sequence,
					RoomID:    roomID,
					EventID:   eventID,
					Content:   content,
				}
			}
		}()
	}

	startedAt := time.Now()
	close(start)
	wg.Wait()
	close(errCh)
	if err := <-errCh; err != nil {
		return nil, 0, err
	}
	return messages, time.Since(startedAt), nil
}

func (f fixture) waitUntilLoadEnqueued(ctx context.Context, total int) error {
	for {
		stats, err := f.fetchStubStats(ctx)
		if err != nil {
			return err
		}
		metrics, err := f.fetchBridgeMetrics(ctx)
		if err != nil {
			return err
		}
		processed := stats.TotalStarted + int(metrics.QueueDepth)
		if processed >= total {
			return nil
		}
		if err := wait(ctx, loadPollInterval); err != nil {
			return fmt.Errorf("wait for %d mentions to enter dispatcher (last count %d): %w", total, processed, err)
		}
	}
}

func (f fixture) replayLoadTransaction(ctx context.Context, token string, messages []loadMessage) (time.Duration, error) {
	events := make([]matrixEvent, 0, len(messages))
	for _, message := range messages {
		evt, err := f.roomEvent(ctx, token, message.RoomID, message.EventID)
		if err != nil {
			return 0, fmt.Errorf("load canonical replay event %s: %w", message.EventID, err)
		}
		events = append(events, evt)
	}
	startedAt := time.Now()
	err := f.pushAppserviceTransaction(ctx, "load-redelivery", events)
	duration := time.Since(startedAt)
	if err != nil {
		return duration, fmt.Errorf("replay load appservice transaction: %w", err)
	}
	return duration, nil
}

func (f fixture) waitForLoadReplies(
	ctx context.Context,
	token, ghost string,
	messages []loadMessage,
) error {
	for {
		delivered, err := f.loadReplyCount(ctx, token, ghost, messages)
		if err != nil {
			return err
		}
		if delivered == len(messages) {
			return nil
		}
		if err := wait(ctx, loadPollInterval); err != nil {
			return fmt.Errorf("wait for load replies (%d/%d delivered): %w", delivered, len(messages), err)
		}
	}
}

func (f fixture) assertLoadReplies(ctx context.Context, token, ghost string, messages []loadMessage) error {
	delivered, err := f.loadReplyCount(ctx, token, ghost, messages)
	if err != nil {
		return err
	}
	if delivered != len(messages) {
		return fmt.Errorf("load replies = %d, want %d", delivered, len(messages))
	}
	return nil
}

func (f fixture) loadReplyCount(
	ctx context.Context,
	token, ghost string,
	messages []loadMessage,
) (int, error) {
	expectedByRoom := make(map[string]map[string]string)
	for _, message := range messages {
		room := expectedByRoom[message.RoomID]
		if room == nil {
			room = make(map[string]string)
			expectedByRoom[message.RoomID] = room
		}
		room[message.EventID] = fmt.Sprintf("load reply room=%02d seq=%02d", message.RoomIndex, message.Sequence)
	}

	delivered := 0
	for roomID, expected := range expectedByRoom {
		events, err := f.roomMessages(ctx, token, roomID)
		if err != nil {
			return 0, err
		}
		counts := make(map[string]int, len(expected))
		for _, event := range events {
			if event.Type != "m.room.message" || event.Sender != ghost {
				continue
			}
			var content messageContent
			if err := json.Unmarshal(event.Content, &content); err != nil {
				return 0, fmt.Errorf("decode ghost reply %s: %w", event.EventID, err)
			}
			if content.MsgType != "m.notice" {
				return 0, fmt.Errorf("ghost event %s has msgtype %q, want m.notice", event.EventID, content.MsgType)
			}
			replyTo := content.RelatesTo.InReplyTo.EventID
			wantBody, ok := expected[replyTo]
			if !ok {
				return 0, fmt.Errorf("ghost event %s replies to unexpected event %q", event.EventID, replyTo)
			}
			if content.Body != wantBody {
				return 0, fmt.Errorf("ghost reply to %s = %q, want %q", replyTo, content.Body, wantBody)
			}
			counts[replyTo]++
		}
		for eventID := range expected {
			if counts[eventID] > 1 {
				return 0, fmt.Errorf("event %s received %d replies, want exactly one", eventID, counts[eventID])
			}
			if counts[eventID] == 1 {
				delivered++
			}
		}
	}
	return delivered, nil
}

func (f fixture) waitForLoadDrain(ctx context.Context, total int) (stubStats, bridgeMetrics, error) {
	for {
		stats, err := f.fetchStubStats(ctx)
		if err != nil {
			return stubStats{}, bridgeMetrics{}, err
		}
		metrics, err := f.fetchBridgeMetrics(ctx)
		if err != nil {
			return stubStats{}, bridgeMetrics{}, err
		}
		if stats.TotalStarted > total || stats.TotalCompleted > total {
			return stubStats{}, bridgeMetrics{}, fmt.Errorf(
				"A2A executions exceeded mention count: started=%d completed=%d mentions=%d",
				stats.TotalStarted,
				stats.TotalCompleted,
				total,
			)
		}
		if stats.TotalStarted == total && stats.TotalCompleted == total && stats.Active == 0 &&
			metrics.QueueDepth == 0 && metrics.InFlight == 0 && metrics.DedupSkips >= float64(total) {
			return stats, metrics, nil
		}
		if err := wait(ctx, loadPollInterval); err != nil {
			return stubStats{}, bridgeMetrics{}, fmt.Errorf(
				"wait for load drain (started=%d completed=%d active=%d queue=%.0f inflight=%.0f dedup=%.0f): %w",
				stats.TotalStarted,
				stats.TotalCompleted,
				stats.Active,
				metrics.QueueDepth,
				metrics.InFlight,
				metrics.DedupSkips,
				err,
			)
		}
	}
}

func assertLoadInvariants(cfg loadConfig, stats stubStats, profile metricProfile, total int) error {
	if stats.DelayMillis < 2000 || stats.DelayMillis > 5000 {
		return fmt.Errorf("A2A stub delay = %dms, want 2000-5000ms", stats.DelayMillis)
	}
	expectedMaxActive := min(cfg.Rooms, cfg.ConcurrencyCap)
	if stats.MaxActive != expectedMaxActive {
		return fmt.Errorf("peak A2A concurrency = %d, want %d", stats.MaxActive, expectedMaxActive)
	}
	if stats.MaxActive > cfg.ConcurrencyCap || profile.Peak.InFlight > float64(cfg.ConcurrencyCap) {
		return fmt.Errorf(
			"global concurrency exceeded cap %d: stub=%d bridge_metric=%.0f",
			cfg.ConcurrencyCap,
			stats.MaxActive,
			profile.Peak.InFlight,
		)
	}
	if err := assertPerRoomFIFO("start", stats.Starts, cfg.Rooms, cfg.MessagesPerRoom); err != nil {
		return err
	}
	if err := assertPerRoomFIFO("completion", stats.Completions, cfg.Rooms, cfg.MessagesPerRoom); err != nil {
		return err
	}
	if profile.Peak.QueueDepth <= 0 || profile.Peak.QueueDepth > float64(total) {
		return fmt.Errorf("peak queue depth = %.0f, want within 1-%d", profile.Peak.QueueDepth, total)
	}
	if profile.Final.QueueDepth != 0 || profile.Final.InFlight != 0 {
		return fmt.Errorf(
			"dispatcher did not drain: queue=%.0f inflight=%.0f",
			profile.Final.QueueDepth,
			profile.Final.InFlight,
		)
	}
	if profile.Peak.HeapAlloc > float64(cfg.MaxHeapBytes) || profile.Peak.HeapInUse > float64(cfg.MaxHeapBytes) {
		return fmt.Errorf(
			"bridge heap exceeded %d bytes: alloc=%.0f inuse=%.0f",
			cfg.MaxHeapBytes,
			profile.Peak.HeapAlloc,
			profile.Peak.HeapInUse,
		)
	}
	return nil
}

func assertPerRoomFIFO(stage string, records []stubRequestRecord, rooms, messagesPerRoom int) error {
	byRoom := make([][]int, rooms)
	for _, record := range records {
		if record.Room < 0 || record.Room >= rooms {
			return fmt.Errorf("%s record has invalid room %d", stage, record.Room)
		}
		byRoom[record.Room] = append(byRoom[record.Room], record.Sequence)
	}
	for room, sequences := range byRoom {
		if len(sequences) != messagesPerRoom {
			return fmt.Errorf("room %d %s count = %d, want %d", room, stage, len(sequences), messagesPerRoom)
		}
		for sequence, got := range sequences {
			if got != sequence {
				return fmt.Errorf("room %d %s order[%d] = %d", room, stage, sequence, got)
			}
		}
	}
	return nil
}

func (f fixture) fetchStubStats(ctx context.Context) (stubStats, error) {
	status, body, err := f.request(ctx, http.MethodGet, f.stubURL+"/stats", "", nil)
	if err != nil {
		return stubStats{}, fmt.Errorf("read A2A stub stats: %w", err)
	}
	if status != http.StatusOK {
		return stubStats{}, fmt.Errorf("read A2A stub stats: status %d: %s", status, body)
	}
	var stats stubStats
	if err := json.Unmarshal(body, &stats); err != nil {
		return stubStats{}, fmt.Errorf("decode A2A stub stats: %w", err)
	}
	return stats, nil
}

func (f fixture) fetchBridgeMetrics(ctx context.Context) (bridgeMetrics, error) {
	status, body, err := f.request(ctx, http.MethodGet, f.metricsURL, "", nil)
	if err != nil {
		return bridgeMetrics{}, fmt.Errorf("read bridge metrics: %w", err)
	}
	if status != http.StatusOK {
		return bridgeMetrics{}, fmt.Errorf("read bridge metrics: status %d: %s", status, body)
	}
	return parseBridgeMetrics(body)
}

func parseBridgeMetrics(body []byte) (bridgeMetrics, error) {
	values := make(map[string]float64)
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || strings.HasPrefix(fields[0], "#") || strings.Contains(fields[0], "{") {
			continue
		}
		switch fields[0] {
		case "go_memstats_heap_alloc_bytes",
			"go_memstats_heap_inuse_bytes",
			"go_memstats_heap_sys_bytes",
			"fgentic_queue_depth",
			"fgentic_inflight_delegations",
			"fgentic_dedup_skips_total",
			"process_start_time_seconds":
			value, err := strconv.ParseFloat(fields[1], 64)
			if err != nil {
				return bridgeMetrics{}, fmt.Errorf("parse metric %s: %w", fields[0], err)
			}
			values[fields[0]] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return bridgeMetrics{}, fmt.Errorf("scan bridge metrics: %w", err)
	}
	for _, name := range []string{
		"go_memstats_heap_alloc_bytes",
		"go_memstats_heap_inuse_bytes",
		"go_memstats_heap_sys_bytes",
		"fgentic_queue_depth",
		"fgentic_inflight_delegations",
		"fgentic_dedup_skips_total",
		"process_start_time_seconds",
	} {
		if _, ok := values[name]; !ok {
			return bridgeMetrics{}, fmt.Errorf("bridge metric %s is missing", name)
		}
	}
	return bridgeMetrics{
		HeapAlloc:        values["go_memstats_heap_alloc_bytes"],
		HeapInUse:        values["go_memstats_heap_inuse_bytes"],
		HeapSys:          values["go_memstats_heap_sys_bytes"],
		QueueDepth:       values["fgentic_queue_depth"],
		InFlight:         values["fgentic_inflight_delegations"],
		DedupSkips:       values["fgentic_dedup_skips_total"],
		ProcessStartTime: values["process_start_time_seconds"],
	}, nil
}

func startMetricSampler(ctx context.Context, f fixture) (*metricSampler, error) {
	baseline, err := f.fetchBridgeMetrics(ctx)
	if err != nil {
		return nil, err
	}
	sampleCtx, cancel := context.WithCancel(ctx)
	sampler := &metricSampler{
		fixture: f,
		cancel:  cancel,
		done:    make(chan struct{}),
		profile: metricProfile{Baseline: baseline, Peak: baseline},
	}
	go sampler.run(sampleCtx)
	return sampler, nil
}

func (s *metricSampler) run(ctx context.Context) {
	defer close(s.done)
	ticker := time.NewTicker(loadMetricSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			metrics, err := s.fixture.fetchBridgeMetrics(ctx)
			s.mu.Lock()
			if err != nil {
				if s.err == nil && ctx.Err() == nil {
					s.err = err
				}
			} else {
				s.profile.Peak = peakMetrics(s.profile.Peak, metrics)
			}
			s.mu.Unlock()
		}
	}
}

func (s *metricSampler) finish(ctx context.Context) (metricProfile, error) {
	s.cancel()
	<-s.done
	final, finalErr := s.fixture.fetchBridgeMetrics(ctx)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profile.Final = final
	if s.err != nil {
		return metricProfile{}, fmt.Errorf("sample bridge metrics: %w", s.err)
	}
	if finalErr != nil {
		return metricProfile{}, finalErr
	}
	return s.profile, nil
}

func (s *metricSampler) abort() {
	s.cancel()
	<-s.done
}

func peakMetrics(peak, current bridgeMetrics) bridgeMetrics {
	peak.HeapAlloc = max(peak.HeapAlloc, current.HeapAlloc)
	peak.HeapInUse = max(peak.HeapInUse, current.HeapInUse)
	peak.HeapSys = max(peak.HeapSys, current.HeapSys)
	peak.QueueDepth = max(peak.QueueDepth, current.QueueDepth)
	peak.InFlight = max(peak.InFlight, current.InFlight)
	peak.DedupSkips = max(peak.DedupSkips, current.DedupSkips)
	return peak
}

func envPositiveInt(name string, fallback int) (int, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, raw, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %d", name, value)
	}
	return value, nil
}

func envPositiveUint64(name string, fallback uint64) (uint64, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, raw, err)
	}
	if value == 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}

func slogLoadResult(
	cfg loadConfig,
	stats stubStats,
	profile metricProfile,
	mentions int,
	sendDuration, transactionAck, elapsed time.Duration,
) {
	slog.Info(
		"bridge load regression passed",
		"mentions", mentions,
		"rooms", cfg.Rooms,
		"messages_per_room", cfg.MessagesPerRoom,
		"stub_delay_ms", stats.DelayMillis,
		"concurrency_cap", cfg.ConcurrencyCap,
		"peak_a2a_concurrency", stats.MaxActive,
		"peak_bridge_inflight", int(profile.Peak.InFlight),
		"peak_queue_depth", int(profile.Peak.QueueDepth),
		"baseline_heap_alloc_bytes", int64(profile.Baseline.HeapAlloc),
		"peak_heap_alloc_bytes", int64(profile.Peak.HeapAlloc),
		"peak_heap_inuse_bytes", int64(profile.Peak.HeapInUse),
		"peak_heap_sys_bytes", int64(profile.Peak.HeapSys),
		"final_heap_alloc_bytes", int64(profile.Final.HeapAlloc),
		"dedup_skips", int(profile.Final.DedupSkips),
		"send_duration_ms", sendDuration.Milliseconds(),
		"transaction_ack_ms", transactionAck.Milliseconds(),
		"elapsed_ms", elapsed.Milliseconds(),
		"per_room_fifo", true,
		"exactly_once_replies", true,
	)
}
