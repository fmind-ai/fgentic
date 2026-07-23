package a2aclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2a"
)

const (
	brokerTransportA2A         = "a2a"
	brokerTransportActivityPub = "activitypub"
	maxBrokerResponseBytes     = 1 << 20
)

type fediverseBroker struct {
	baseURL string
	token   string
	client  *http.Client
}

type fediverseBrokerRequest struct {
	Handle   string `json:"handle"`
	Identity struct {
		ActorID            string `json:"actorId"`
		VerificationMethod string `json:"verificationMethod"`
		PublicKeyMultibase string `json:"publicKeyMultibase"`
		ProofMaxAgeSeconds int64  `json:"proofMaxAgeSeconds"`
	} `json:"activityPubIdentity"`
	Sender    string `json:"sender,omitempty"`
	MessageID string `json:"messageId,omitempty"`
	Text      string `json:"text,omitempty"`
	ContextID string `json:"contextId,omitempty"`
}

type fediverseResolution struct {
	Transport   string `json:"transport"`
	ActorID     string `json:"actorId"`
	Name        string `json:"name,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Inbox       string `json:"inbox,omitempty"`
	A2AEndpoint string `json:"a2aEndpoint,omitempty"`
	AgentCard   string `json:"agentCard,omitempty"`
	ActivityID  string `json:"activityId,omitempty"`
}

// UseFediverseBroker configures the ClusterIP-only broker used by acct mappings. The local
// agentgateway credential is not part of this client and can never cross the boundary.
func (c *Client) UseFediverseBroker(baseURL, token string) error {
	normalized, err := NormalizeRemoteURL(baseURL)
	if err != nil {
		return fmt.Errorf("configure fediverse broker URL: %w", err)
	}
	parsed, err := url.Parse(normalized)
	if err != nil {
		return fmt.Errorf("configure fediverse broker URL: %w", err)
	}
	if !isAllowedCleartextHost(parsed.Hostname()) {
		return fmt.Errorf("configure fediverse broker URL: host must be loopback or a Kubernetes service")
	}
	if token == "" || strings.TrimSpace(token) != token {
		return fmt.Errorf("configure fediverse broker token: must be non-empty without surrounding whitespace")
	}
	c.fediverseBroker = &fediverseBroker{
		baseURL: normalized,
		token:   token,
		client: &http.Client{
			Transport:     &userTransport{base: http.DefaultTransport},
			CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	return nil
}

func (c *Client) resolveFediverseAgentCard(ctx context.Context, target Target) (*a2a.AgentCard, error) {
	if c.fediverseBroker == nil {
		return nil, c.quarantineRemote(target, errors.New("fediverse broker is not configured"))
	}
	var resolved fediverseResolution
	if err := c.callFediverseBroker(ctx, "/internal/v1/fediverse/resolve", brokerRequest(target), &resolved); err != nil {
		return nil, c.quarantineRemote(target, fmt.Errorf("resolve acct mapping: %w", err))
	}
	switch resolved.Transport {
	case brokerTransportA2A:
		remote, err := target.resolvedRemote(resolved.A2AEndpoint, resolved.AgentCard)
		if err != nil {
			return nil, c.quarantineRemote(target, fmt.Errorf("validate discovered A2A route: %w", err))
		}
		return c.resolveRemoteAgentCardAs(ctx, remote, target)
	case brokerTransportActivityPub:
		if resolved.ActorID != target.activityPubIdentity.ActorID || resolved.Inbox == "" {
			return nil, c.quarantineRemote(target, errors.New("broker returned an unpinned ActivityPub actor"))
		}
		card := &a2a.AgentCard{Name: target.expectedName, Description: resolved.Summary}
		if strings.TrimSpace(resolved.Name) != "" {
			card.Name = resolved.Name
		}
		c.mu.Lock()
		generation := nextGeneration(c.cache[target.ID()].generation)
		c.remoteGeneration(target.ID()).Store(generation)
		c.cache[target.ID()] = cachedTarget{card: card, ready: true, generation: generation, fediverse: &resolved}
		c.mu.Unlock()
		c.log.Info("verified ActivityPub agent fallback", "target", target.String(), "actor", resolved.ActorID)
		return cloneAgentCard(card)
	default:
		return nil, c.quarantineRemote(target, fmt.Errorf("broker returned unsupported transport %q", resolved.Transport))
	}
}

func (c *Client) sendActivityPub(
	ctx context.Context,
	target Target,
	resolved *fediverseResolution,
	messageID, text, contextID, taskID string,
	files []InboundFile,
) (Result, error) {
	if taskID != "" {
		return Result{}, fmt.Errorf("ActivityPub fallback does not support A2A task continuation")
	}
	if len(files) != 0 {
		return Result{}, fmt.Errorf("ActivityPub fallback does not support Matrix media")
	}
	if c.fediverseBroker == nil || resolved == nil {
		return Result{}, fmt.Errorf("call fediverse target %s: %w", target.String(), ErrRemoteTargetUntrusted)
	}
	req := brokerRequest(target)
	req.Sender, _ = ctx.Value(userKey{}).(string)
	req.MessageID = messageID
	req.Text = text
	req.ContextID = contextID
	var delivered fediverseResolution
	err := c.callFediverseBroker(ctx, "/internal/v1/fediverse/delegate", req, &delivered)
	if err != nil {
		var statusErr *brokerStatusError
		if errors.As(err, &statusErr) && statusErr.status == http.StatusConflict {
			return Result{}, c.quarantineRemote(target, errors.New("remote actor now advertises A2A; refresh required"))
		}
		if errors.As(err, &statusErr) && statusErr.status < http.StatusInternalServerError {
			return Result{}, fmt.Errorf("fediverse broker rejected delegation: %w", err)
		}
		return Result{}, &AmbiguousSendError{messageID: messageID, target: target.String(), cause: err}
	}
	if delivered.Transport != brokerTransportActivityPub || delivered.ActorID != target.activityPubIdentity.ActorID || delivered.ActivityID == "" {
		return Result{}, &AmbiguousSendError{messageID: messageID, target: target.String(), cause: errors.New("broker returned invalid delivery acknowledgement")}
	}
	return Result{
		Text:      "Delegation delivered over ActivityPub; the remote reply will arrive asynchronously.",
		ContextID: contextID,
		Terminal:  true,
	}, nil
}

func brokerRequest(target Target) fediverseBrokerRequest {
	req := fediverseBrokerRequest{Handle: target.String()}
	req.Identity.ActorID = target.activityPubIdentity.ActorID
	req.Identity.VerificationMethod = target.activityPubIdentity.VerificationMethod
	req.Identity.PublicKeyMultibase = target.activityPubIdentity.PublicKeyMultibase
	req.Identity.ProofMaxAgeSeconds = int64(target.activityPubIdentity.ProofMaxAge / time.Second)
	return req
}

type brokerStatusError struct{ status int }

func (e *brokerStatusError) Error() string {
	return fmt.Sprintf("fediverse broker returned HTTP %d", e.status)
}

func (c *Client) callFediverseBroker(ctx context.Context, path string, payload fediverseBrokerRequest, dst *fediverseResolution) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encode broker request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.fediverseBroker.baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("build broker request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.fediverseBroker.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.fediverseBroker.client.Do(req)
	if err != nil {
		return fmt.Errorf("call fediverse broker: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
		return &brokerStatusError{status: resp.StatusCode}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBrokerResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read fediverse broker response: %w", err)
	}
	if len(body) > maxBrokerResponseBytes {
		return fmt.Errorf("fediverse broker response exceeds %d bytes", maxBrokerResponseBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("decode fediverse broker response: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return fmt.Errorf("decode fediverse broker response trailing data: %w", err)
	}
	return nil
}
