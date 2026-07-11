// Command driver exercises the real Matrix client/appservice wire path in the kind fixture.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultMatrixURL     = "http://synapse:8008"
	defaultBridgeURL     = "http://bridge:29331"
	defaultMetricsURL    = "http://bridge:9090/metrics"
	defaultStubURL       = "http://a2a-stub:8080"
	defaultServer        = "integration.test"
	defaultHSToken       = "integration-homeserver-token"
	username             = "integration-user"
	password             = "integration-password"
	ghostLocalpart       = "agent-integration"
	remoteGhostLocalpart = "agent-remote"
	replyText            = "integration reply"
	remoteReplyText      = "remote integration reply"
)

type fixture struct {
	matrixURL  string
	bridgeURL  string
	metricsURL string
	stubURL    string
	server     string
	hsToken    string
	http       *http.Client
}

type session struct {
	AccessToken string `json:"access_token"`
	UserID      string `json:"user_id"`
}

type matrixEvent struct {
	Content        json.RawMessage `json:"content"`
	EventID        string          `json:"event_id"`
	OriginServerTS int64           `json:"origin_server_ts"`
	RoomID         string          `json:"room_id"`
	Sender         string          `json:"sender"`
	Type           string          `json:"type"`
}

type messageContent struct {
	Body      string   `json:"body"`
	Mentions  mentions `json:"m.mentions,omitempty"`
	MsgType   string   `json:"msgtype"`
	RelatesTo struct {
		InReplyTo struct {
			EventID string `json:"event_id"`
		} `json:"m.in_reply_to"`
	} `json:"m.relates_to"`
}

type mentions struct {
	UserIDs []string `json:"user_ids"`
}

func main() {
	if err := run(); err != nil {
		slog.Error("bridge integration failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	timeout, err := envDuration("DRIVER_TIMEOUT", 75*time.Second)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	f := fixture{
		matrixURL:  strings.TrimRight(envOrDefault("MATRIX_URL", defaultMatrixURL), "/"),
		bridgeURL:  strings.TrimRight(envOrDefault("BRIDGE_URL", defaultBridgeURL), "/"),
		metricsURL: envOrDefault("BRIDGE_METRICS_URL", defaultMetricsURL),
		stubURL:    strings.TrimRight(envOrDefault("A2A_STUB_URL", defaultStubURL), "/"),
		server:     envOrDefault("MATRIX_SERVER_NAME", defaultServer),
		hsToken:    envOrDefault("BRIDGE_HS_TOKEN", defaultHSToken),
		http:       &http.Client{Timeout: 10 * time.Second},
	}
	switch scenario := envOrDefault("DRIVER_SCENARIO", "integration"); scenario {
	case "integration":
		return f.runBasic(ctx)
	case "load":
		return f.runLoad(ctx)
	default:
		return fmt.Errorf("unknown DRIVER_SCENARIO %q", scenario)
	}
}

func (f fixture) runBasic(ctx context.Context) error {
	sess, err := f.register(ctx)
	if err != nil {
		return err
	}
	roomID, err := f.createRoom(ctx, sess.AccessToken)
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

	content := messageContent{
		Body:     ghost + " prove the appservice wire path",
		Mentions: mentions{UserIDs: []string{ghost}},
		MsgType:  "m.text",
	}
	eventID, err := f.sendMessage(ctx, sess.AccessToken, roomID, content)
	if err != nil {
		return err
	}
	if err := f.waitForReplyCount(ctx, sess.AccessToken, roomID, ghost, eventID, replyText, 1); err != nil {
		return err
	}

	if err := f.replayEvent(ctx, roomID, sess.UserID, eventID, content); err != nil {
		return err
	}
	quietDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(quietDeadline) {
		if err := f.assertReplyCount(ctx, sess.AccessToken, roomID, ghost, eventID, replyText, 1); err != nil {
			return fmt.Errorf("deduplication after transaction replay: %w", err)
		}
		if err := wait(ctx, 250*time.Millisecond); err != nil {
			return err
		}
	}

	remoteGhost := "@" + remoteGhostLocalpart + ":" + f.server
	if err := f.invite(ctx, sess.AccessToken, roomID, remoteGhost); err != nil {
		return err
	}
	if err := f.waitForJoin(ctx, sess.AccessToken, roomID, remoteGhost); err != nil {
		return err
	}
	remoteContent := messageContent{
		Body:     remoteGhost + " prove the signed remote A2A path",
		Mentions: mentions{UserIDs: []string{remoteGhost}},
		MsgType:  "m.text",
	}
	remoteEventID, err := f.sendMessageTxn(
		ctx,
		sess.AccessToken,
		roomID,
		"integration-remote-valid",
		remoteContent,
	)
	if err != nil {
		return err
	}
	if err := f.waitForReplyCount(
		ctx,
		sess.AccessToken,
		roomID,
		remoteGhost,
		remoteEventID,
		remoteReplyText,
		1,
	); err != nil {
		return fmt.Errorf("signed remote round trip: %w", err)
	}

	beforeTamper, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	if beforeTamper.RemoteRequests < 1 {
		return fmt.Errorf("signed remote round trip made %d A2A requests, want at least 1", beforeTamper.RemoteRequests)
	}
	if !beforeTamper.TokenBudgetValid {
		return fmt.Errorf("signed remote round trip did not validate the configured token-budget extension")
	}
	if err := f.tamperRemoteCard(ctx); err != nil {
		return err
	}
	tamperedBaseline, err := f.fetchStubStats(ctx)
	if err != nil {
		return err
	}
	afterRefresh, err := f.waitForTamperedCardRefresh(ctx, tamperedBaseline.RemoteCardRequests)
	if err != nil {
		return err
	}

	tamperedContent := messageContent{
		Body:     remoteGhost + " this must fail closed after card tampering",
		Mentions: mentions{UserIDs: []string{remoteGhost}},
		MsgType:  "m.text",
	}
	tamperedEventID, err := f.sendMessageTxn(
		ctx,
		sess.AccessToken,
		roomID,
		"integration-remote-tampered",
		tamperedContent,
	)
	if err != nil {
		return err
	}
	if err := f.assertNoRemoteDispatch(ctx, afterRefresh.RemoteRequests, 4*time.Second); err != nil {
		return fmt.Errorf("tampered remote AgentCard: %w", err)
	}

	slog.Info(
		"bridge integration passed",
		"room_id", roomID,
		"mention_event_id", eventID,
		"reply", replyText,
		"deduplicated_replay", true,
		"remote_mention_event_id", remoteEventID,
		"remote_reply", remoteReplyText,
		"tampered_mention_event_id", tamperedEventID,
		"tampered_card_rejected_before_a2a", true,
	)
	return nil
}

func (f fixture) register(ctx context.Context) (session, error) {
	payload := map[string]any{
		"device_id": "integration",
		"password":  password,
		"username":  username,
	}
	status, body, err := f.request(ctx, http.MethodPost, f.matrixURL+"/_matrix/client/v3/register", "", payload)
	if err != nil {
		return session{}, fmt.Errorf("start Matrix registration: %w", err)
	}
	if status == http.StatusUnauthorized {
		var challenge struct {
			Session string `json:"session"`
		}
		if err := json.Unmarshal(body, &challenge); err != nil || challenge.Session == "" {
			return session{}, fmt.Errorf("decode Matrix registration challenge (%d): %s", status, body)
		}
		payload["auth"] = map[string]string{"session": challenge.Session, "type": "m.login.dummy"}
		status, body, err = f.request(ctx, http.MethodPost, f.matrixURL+"/_matrix/client/v3/register", "", payload)
		if err != nil {
			return session{}, fmt.Errorf("complete Matrix registration: %w", err)
		}
	}
	if status != http.StatusOK {
		return session{}, fmt.Errorf("register Matrix user: status %d: %s", status, body)
	}
	var sess session
	if err := json.Unmarshal(body, &sess); err != nil {
		return session{}, fmt.Errorf("decode Matrix registration: %w", err)
	}
	if sess.AccessToken == "" || sess.UserID == "" {
		return session{}, fmt.Errorf("matrix registration returned incomplete credentials")
	}
	return sess, nil
}

func (f fixture) createRoom(ctx context.Context, token string) (string, error) {
	status, body, err := f.request(
		ctx,
		http.MethodPost,
		f.matrixURL+"/_matrix/client/v3/createRoom",
		token,
		map[string]any{"name": "Bridge integration", "preset": "private_chat"},
	)
	if err != nil {
		return "", fmt.Errorf("create Matrix room: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("create Matrix room: status %d: %s", status, body)
	}
	var response struct {
		RoomID string `json:"room_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode Matrix room response: %w", err)
	}
	if response.RoomID == "" {
		return "", fmt.Errorf("decode Matrix room response: room_id is empty")
	}
	return response.RoomID, nil
}

func (f fixture) invite(ctx context.Context, token, roomID, userID string) error {
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/invite", f.matrixURL, pathSegment(roomID))
	status, body, err := f.request(ctx, http.MethodPost, endpoint, token, map[string]string{"user_id": userID})
	if err != nil {
		return fmt.Errorf("invite bridge ghost: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("invite bridge ghost: status %d: %s", status, body)
	}
	return nil
}

func (f fixture) waitForJoin(ctx context.Context, token, roomID, userID string) error {
	endpoint := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/state/m.room.member/%s",
		f.matrixURL,
		pathSegment(roomID),
		pathSegment(userID),
	)
	for {
		status, body, err := f.request(ctx, http.MethodGet, endpoint, token, nil)
		if err == nil && status == http.StatusOK {
			var member struct {
				Membership string `json:"membership"`
			}
			if json.Unmarshal(body, &member) == nil && member.Membership == "join" {
				return nil
			}
		}
		if err := wait(ctx, 250*time.Millisecond); err != nil {
			return fmt.Errorf("wait for bridge ghost %s to join room: %w", userID, err)
		}
	}
}

func (f fixture) sendMessage(ctx context.Context, token, roomID string, content messageContent) (string, error) {
	return f.sendMessageTxn(ctx, token, roomID, "integration-mention", content)
}

func (f fixture) sendMessageTxn(
	ctx context.Context,
	token, roomID, transactionID string,
	content messageContent,
) (string, error) {
	endpoint := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		f.matrixURL,
		pathSegment(roomID),
		pathSegment(transactionID),
	)
	status, body, err := f.request(ctx, http.MethodPut, endpoint, token, content)
	if err != nil {
		return "", fmt.Errorf("send Matrix mention: %w", err)
	}
	if status != http.StatusOK {
		return "", fmt.Errorf("send Matrix mention: status %d: %s", status, body)
	}
	var response struct {
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return "", fmt.Errorf("decode Matrix send response: %w", err)
	}
	if response.EventID == "" {
		return "", fmt.Errorf("decode Matrix send response: event_id is empty")
	}
	return response.EventID, nil
}

func (f fixture) replayEvent(
	ctx context.Context,
	roomID, sender, eventID string,
	content messageContent,
) error {
	encodedContent, err := json.Marshal(content)
	if err != nil {
		return fmt.Errorf("encode replayed Matrix event: %w", err)
	}
	event := matrixEvent{
		Content:        encodedContent,
		EventID:        eventID,
		OriginServerTS: time.Now().UnixMilli(),
		RoomID:         roomID,
		Sender:         sender,
		Type:           "m.room.message",
	}
	endpoint := f.bridgeURL + "/_matrix/app/v1/transactions/integration-redelivery"
	status, body, err := f.request(ctx, http.MethodPut, endpoint, f.hsToken, map[string]any{"events": []matrixEvent{event}})
	if err != nil {
		return fmt.Errorf("replay appservice transaction: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("replay appservice transaction: status %d: %s", status, body)
	}
	return nil
}

func (f fixture) waitForReplyCount(
	ctx context.Context,
	token, roomID, ghost, eventID, body string,
	want int,
) error {
	for {
		count, err := f.replyCount(ctx, token, roomID, ghost, eventID, body)
		if err == nil && count == want {
			return nil
		}
		if err == nil && count > want {
			return fmt.Errorf("matrix replies = %d, want %d", count, want)
		}
		if waitErr := wait(ctx, 250*time.Millisecond); waitErr != nil {
			if err != nil {
				return errors.Join(
					fmt.Errorf("last Matrix reply query: %w", err),
					fmt.Errorf("wait for Matrix reply: %w", waitErr),
				)
			}
			return fmt.Errorf("wait for Matrix reply: %w", waitErr)
		}
	}
}

func (f fixture) assertReplyCount(
	ctx context.Context,
	token, roomID, ghost, eventID, body string,
	want int,
) error {
	count, err := f.replyCount(ctx, token, roomID, ghost, eventID, body)
	if err != nil {
		return err
	}
	if count != want {
		return fmt.Errorf("matrix m.notice replies = %d, want %d", count, want)
	}
	return nil
}

func (f fixture) replyCount(ctx context.Context, token, roomID, ghost, eventID, body string) (int, error) {
	events, err := f.roomMessages(ctx, token, roomID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, event := range events {
		if event.Type != "m.room.message" || event.Sender != ghost {
			continue
		}
		var content messageContent
		if err := json.Unmarshal(event.Content, &content); err != nil {
			return 0, fmt.Errorf("decode Matrix message %s: %w", event.EventID, err)
		}
		if content.MsgType == "m.notice" && content.Body == body && content.RelatesTo.InReplyTo.EventID == eventID {
			count++
		}
	}
	return count, nil
}

func (f fixture) tamperRemoteCard(ctx context.Context) error {
	status, body, err := f.request(ctx, http.MethodPost, f.stubURL+"/control/tamper", "", nil)
	if err != nil {
		return fmt.Errorf("tamper remote AgentCard: %w", err)
	}
	if status != http.StatusNoContent {
		return fmt.Errorf("tamper remote AgentCard: status %d: %s", status, body)
	}
	return nil
}

func (f fixture) waitForTamperedCardRefresh(ctx context.Context, previousRequests int) (stubStats, error) {
	for {
		stats, statsErr := f.fetchStubStats(ctx)
		// Refreshes are synchronous. Seeing the next cycle fetch a second tampered card proves
		// the bridge completed verification and quarantined the first one.
		if statsErr == nil && stats.CardTampered && stats.RemoteCardRequests >= previousRequests+2 {
			return stats, nil
		}
		if waitErr := wait(ctx, 100*time.Millisecond); waitErr != nil {
			if statsErr != nil {
				return stubStats{}, errors.Join(
					fmt.Errorf("last A2A stub stats query: %w", statsErr),
					fmt.Errorf("wait for tampered AgentCard refresh: %w", waitErr),
				)
			}
			return stubStats{}, fmt.Errorf("wait for tampered AgentCard refresh: %w", waitErr)
		}
	}
}

func (f fixture) assertNoRemoteDispatch(ctx context.Context, expected int, duration time.Duration) error {
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		stats, err := f.fetchStubStats(ctx)
		if err != nil {
			return err
		}
		if stats.RemoteRequests != expected {
			return fmt.Errorf("remote A2A requests = %d, want unchanged at %d", stats.RemoteRequests, expected)
		}
		if err := wait(ctx, 100*time.Millisecond); err != nil {
			return err
		}
	}
	return nil
}

func (f fixture) roomMessages(ctx context.Context, token, roomID string) ([]matrixEvent, error) {
	endpoint := fmt.Sprintf(
		"%s/_matrix/client/v3/rooms/%s/messages?dir=b&limit=100",
		f.matrixURL,
		pathSegment(roomID),
	)
	status, body, err := f.request(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return nil, fmt.Errorf("read Matrix room messages: %w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("read Matrix room messages: status %d: %s", status, body)
	}
	var response struct {
		Chunk []matrixEvent `json:"chunk"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("decode Matrix room messages: %w", err)
	}
	return response.Chunk, nil
}

func (f fixture) request(
	ctx context.Context,
	method, endpoint, token string,
	payload any,
) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, fmt.Errorf("encode request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return 0, nil, fmt.Errorf("build request: %w", err)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	response, err := f.http.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("%s %s: %w", method, endpoint, err)
	}
	defer func() {
		if err := response.Body.Close(); err != nil {
			slog.Warn("close HTTP response body", "err", err)
		}
	}()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if err != nil {
		return 0, nil, fmt.Errorf("read %s %s response: %w", method, endpoint, err)
	}
	return response.StatusCode, responseBody, nil
}

func pathSegment(value string) string {
	return url.PathEscape(value)
}

func wait(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}

func envOrDefault(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(name)
	if raw == "" {
		return fallback, nil
	}
	value, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("parse %s %q: %w", name, raw, err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %s", name, value)
	}
	return value, nil
}
