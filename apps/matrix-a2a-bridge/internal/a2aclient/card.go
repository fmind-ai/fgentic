package a2aclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	sdk "github.com/a2aproject/a2a-go/v2/a2aclient"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
)

const (
	remoteAgentCardPath = "/.well-known/agent-card.json"
	maxAgentCardBytes   = 1 << 20
)

// ErrRemoteTargetUntrusted marks a remote target that cannot be delegated to because no
// currently verified signed AgentCard is installed. Call and PollTask never fetch remote trust
// material implicitly: startup and periodic profile refresh own that network boundary.
var ErrRemoteTargetUntrusted = errors.New("remote A2A target has no verified AgentCard")

// ErrRemoteKeyRevoked re-exports the AgentCard revoked-key-ID signal (#352) so the bridge can attribute
// a distinct audit reason when a card was offered only under a retired signing key ID.
var ErrRemoteKeyRevoked = agentcardjws.ErrRevokedKeyID

// ErrRemoteExtensionUnsupported marks a verified card that declares a `required: true` A2A
// extension the bridge is not configured to activate. It wraps ErrRemoteTargetUntrusted so every
// fail-closed path still quarantines the target, while giving the audit a distinct terminal reason
// (a negotiation gap, not a signature or identity mismatch — docs/bridge.md §6).
var ErrRemoteExtensionUnsupported = fmt.Errorf("%w: required A2A extension is not activated", ErrRemoteTargetUntrusted)

// ErrRemoteMutualTLSRequired marks a verified card that declares an A2A mTLS security scheme while
// its mapping configured no client certificate. It wraps ErrRemoteTargetUntrusted so the target is
// quarantined fail-closed, with a distinct terminal reason (a transport-auth gap, not a signature or
// identity mismatch — #244).
var ErrRemoteMutualTLSRequired = fmt.Errorf("%w: card requires mTLS but no client certificate is configured", ErrRemoteTargetUntrusted)

// cardRequiresMutualTLS reports whether a verified AgentCard declares an A2A mutual-TLS security
// scheme in its securitySchemes map.
func cardRequiresMutualTLS(card *a2a.AgentCard) bool {
	for _, scheme := range card.SecuritySchemes {
		if _, ok := scheme.(a2a.MutualTLSSecurityScheme); ok {
			return true
		}
	}
	return false
}

type cachedTarget struct {
	client       *sdk.Client
	card         *a2a.AgentCard
	etag         string
	lastModified string
	ready        bool
	generation   uint64
}

func (c *Client) resolveRemoteAgentCard(ctx context.Context, target Target) (*a2a.AgentCard, error) {
	refreshLock := c.refreshLock(target.ID())
	refreshLock.Lock()
	defer refreshLock.Unlock()

	c.mu.RLock()
	previous := c.cache[target.ID()]
	c.mu.RUnlock()

	cardURL := strings.TrimRight(target.String(), "/") + remoteAgentCardPath
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cardURL, nil)
	if err != nil {
		return nil, fmt.Errorf("build remote AgentCard request for %s: %w", target.String(), err)
	}
	req.Header.Set("Accept", "application/a2a+json, application/json")
	if previous.ready {
		if previous.etag != "" {
			req.Header.Set("If-None-Match", previous.etag)
		}
		if previous.lastModified != "" {
			req.Header.Set("If-Modified-Since", previous.lastModified)
		}
	}

	// The card is fetched over the same endpoint the delegation dials, so it must present the same
	// per-target client certificate when mTLS is configured (#244).
	cardClient := &http.Client{Transport: c.remoteUserTransport(target), CheckRedirect: c.remoteHTTPClient.CheckRedirect}
	resp, err := cardClient.Do(req)
	if err != nil {
		// Availability failures do not revoke a previously verified identity. The explicit
		// periodic refresh policy will retry; trust failures still quarantine immediately.
		return nil, fmt.Errorf("fetch remote AgentCard %s: %w", cardURL, err)
	}

	if resp.StatusCode == http.StatusNotModified {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close remote AgentCard response %s: %w", cardURL, err)
		}
		if !previous.ready || previous.card == nil || previous.client == nil {
			return nil, c.quarantineRemote(target, errors.New("server returned 304 without a verified cached card"))
		}
		if etag := resp.Header.Get("ETag"); etag != "" {
			previous.etag = etag
		}
		if lastModified := resp.Header.Get("Last-Modified"); lastModified != "" {
			previous.lastModified = lastModified
		}
		c.mu.Lock()
		c.cache[target.ID()] = previous
		c.mu.Unlock()
		return cloneAgentCard(previous.card)
	}
	if resp.StatusCode != http.StatusOK {
		closeErr := resp.Body.Close()
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			trustErr := c.quarantineRemote(target, fmt.Errorf("card was withdrawn with HTTP %d", resp.StatusCode))
			if closeErr != nil {
				return nil, errors.Join(trustErr, fmt.Errorf("close withdrawn remote AgentCard response: %w", closeErr))
			}
			return nil, trustErr
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close remote AgentCard response %s after HTTP %d: %w", cardURL, resp.StatusCode, closeErr)
		}
		return nil, fmt.Errorf("fetch remote AgentCard %s: HTTP %d", cardURL, resp.StatusCode)
	}

	if resp.ContentLength > maxAgentCardBytes {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close oversized remote AgentCard response %s: %w", cardURL, err)
		}
		return nil, c.quarantineRemote(target, fmt.Errorf("card exceeds %d bytes", maxAgentCardBytes))
	}
	if !isJSONMediaType(resp.Header.Get("Content-Type")) {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close non-JSON remote AgentCard response %s: %w", cardURL, err)
		}
		return nil, c.quarantineRemote(target, errors.New("card response is not JSON"))
	}

	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxAgentCardBytes+1))
	closeErr := resp.Body.Close()
	if readErr != nil {
		return nil, fmt.Errorf("read remote AgentCard %s: %w", cardURL, readErr)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("close remote AgentCard response %s: %w", cardURL, closeErr)
	}
	if len(raw) > maxAgentCardBytes {
		return nil, c.quarantineRemote(target, fmt.Errorf("card exceeds %d bytes", maxAgentCardBytes))
	}

	card, err := verifyRemoteAgentCard(raw, target)
	if err != nil {
		// err may wrap ErrRemoteExtensionUnsupported; quarantineRemote preserves it for the audit.
		return nil, c.quarantineRemote(target, err)
	}
	// A card that requires mTLS but whose mapping configured no client certificate is refused: the
	// partner demands transport auth the bridge cannot present, and trusting it anyway would be a
	// silent downgrade of the declared scheme (#244).
	if cardRequiresMutualTLS(card) && !target.requiresClientCert() {
		return nil, c.quarantineRemote(target, ErrRemoteMutualTLSRequired)
	}
	generation := nextGeneration(previous.generation)
	generationState := c.remoteGeneration(target.ID())
	client, err := buildSDKClient(
		ctx,
		card,
		c.remoteSDKHTTPClient(target, generationState, generation),
		target.ActivatedExtensions(),
	)
	if err != nil {
		return nil, c.quarantineRemote(target, fmt.Errorf("build client from verified card: %w", err))
	}

	installed := cachedTarget{
		client:       client,
		card:         card,
		etag:         resp.Header.Get("ETag"),
		lastModified: resp.Header.Get("Last-Modified"),
		ready:        true,
		generation:   generation,
	}
	c.mu.Lock()
	generationState.Store(generation)
	c.cache[target.ID()] = installed
	c.mu.Unlock()
	c.log.Info(
		"verified remote a2a agent",
		"target", target.String(),
		"card_name", card.Name,
		"card_organization", card.Provider.Org,
		"card_key_id", target.expectedKeyID,
	)
	return cloneAgentCard(card)
}

func (c *Client) refreshLock(targetID string) *sync.Mutex {
	value, _ := c.refreshLocks.LoadOrStore(targetID, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// quarantineRemote drops any cached client for target and returns a trust error wrapping cause.
// The returned error always satisfies errors.Is(err, ErrRemoteTargetUntrusted); a cause that
// itself wraps a more specific sentinel (e.g. ErrRemoteExtensionUnsupported) stays inspectable so
// the audit can report a distinct terminal reason.
func (c *Client) quarantineRemote(target Target, cause error) error {
	c.mu.Lock()
	generation := nextGeneration(c.cache[target.ID()].generation)
	c.remoteGeneration(target.ID()).Store(generation)
	c.cache[target.ID()] = cachedTarget{generation: generation}
	c.mu.Unlock()
	c.log.Warn("quarantined remote a2a agent", "target", target.String(), "reason", cause.Error())
	return fmt.Errorf("%w: %s: %w", ErrRemoteTargetUntrusted, target.String(), cause)
}

func nextGeneration(current uint64) uint64 {
	next := current + 1
	if next == 0 {
		return 1
	}
	return next
}

func isJSONMediaType(raw string) bool {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return false
	}
	return mediaType == "application/json" ||
		mediaType == "application/a2a+json" ||
		(strings.HasPrefix(mediaType, "application/") && strings.HasSuffix(mediaType, "+json"))
}

func cloneAgentCard(card *a2a.AgentCard) (*a2a.AgentCard, error) {
	raw, err := json.Marshal(card)
	if err != nil {
		return nil, fmt.Errorf("clone AgentCard: marshal: %w", err)
	}
	var cloned a2a.AgentCard
	if err := json.Unmarshal(raw, &cloned); err != nil {
		return nil, fmt.Errorf("clone AgentCard: unmarshal: %w", err)
	}
	return &cloned, nil
}

func verifyRemoteAgentCard(raw []byte, target Target) (*a2a.AgentCard, error) {
	document, err := agentcardjws.Parse(raw)
	if err != nil {
		return nil, err
	}
	if signatures, present := document.Signatures(); !present || len(signatures) == 0 {
		return nil, fmt.Errorf("card is unsigned")
	}
	card, err := document.Card()
	if err != nil {
		return nil, err
	}
	if err := validateRemoteCardContract(card, target); err != nil {
		return nil, err
	}
	pinnedKeys := target.pinnedKeys()
	if len(pinnedKeys) == 0 {
		return nil, fmt.Errorf("pinned ES256 public key is invalid")
	}
	// Verify against the currently-valid pin set (rotation overlap) and the revoked list (#352): a card
	// under any pinned, non-revoked key is trusted; one under a retired key is refused.
	if err := agentcardjws.VerifySet(document, pinnedKeys, target.revoked()); err != nil {
		return nil, err
	}
	return card, nil
}

func validateRemoteCardContract(card *a2a.AgentCard, target Target) error {
	if card.Name != target.expectedName {
		return fmt.Errorf("card name does not match pinned identity")
	}
	if card.Provider == nil {
		return fmt.Errorf("card provider is missing")
	}
	if card.Provider.Org != target.expectedOrganization {
		return fmt.Errorf("card organization does not match pinned identity")
	}
	if strings.TrimSpace(card.Provider.URL) == "" {
		return fmt.Errorf("card provider URL is empty")
	}
	if card.Version == "" {
		return fmt.Errorf("card version is empty")
	}
	if len(card.DefaultInputModes) == 0 || len(card.DefaultOutputModes) == 0 {
		return fmt.Errorf("card must advertise default input and output modes")
	}
	if !slices.Contains(card.DefaultInputModes, "text/plain") || !slices.Contains(card.DefaultOutputModes, "text/plain") {
		return fmt.Errorf("card does not advertise text/plain input and output")
	}
	if len(card.SupportedInterfaces) == 0 {
		return fmt.Errorf("card has no supported interfaces")
	}
	seenSkillIDs := make(map[string]struct{}, len(card.Skills))
	for _, skill := range card.Skills {
		if strings.TrimSpace(skill.ID) == "" || strings.TrimSpace(skill.Name) == "" {
			return fmt.Errorf("card contains a skill with an empty ID or name")
		}
		if _, duplicate := seenSkillIDs[skill.ID]; duplicate {
			return fmt.Errorf("card contains duplicate skill IDs")
		}
		seenSkillIDs[skill.ID] = struct{}{}
	}
	matches := make([]*a2a.AgentInterface, 0, len(card.SupportedInterfaces))
	for _, agentInterface := range card.SupportedInterfaces {
		if agentInterface == nil {
			continue
		}
		// Tenant changes JSON-RPC parameters and REST paths in the SDK. It is not part of
		// the configured trust pin, so a signer must not be able to select it implicitly.
		if agentInterface.Tenant != "" {
			continue
		}
		if agentInterface.ProtocolVersion != a2a.ProtocolVersion("1.0") {
			continue
		}
		if agentInterface.ProtocolBinding != a2a.TransportProtocolJSONRPC &&
			agentInterface.ProtocolBinding != a2a.TransportProtocolHTTPJSON {
			continue
		}
		endpoint, err := NormalizeRemoteURL(agentInterface.URL)
		if err != nil {
			continue
		}
		if endpoint == target.String() {
			// Bind the SDK to the canonical configured endpoint rather than retaining any
			// alternate wire representation accepted by URL parsing.
			agentInterface.URL = target.String()
			matches = append(matches, agentInterface)
		}
	}
	if len(matches) == 0 {
		return fmt.Errorf("card has no A2A v1.0 interface matching the pinned endpoint")
	}
	// Never let SDK transport selection follow an unrelated endpoint merely because it was
	// covered by the same card signature.
	card.SupportedInterfaces = matches
	// Extension negotiation (docs/bridge.md §6): the token-budget base contract must be present,
	// and any card-declared `required: true` extension the bridge is not configured to activate
	// fails closed with a distinct, inspectable trust error.
	foundTokenBudget := false
	for _, extension := range card.Capabilities.Extensions {
		if extension.URI == TokenBudgetExtensionURI {
			foundTokenBudget = true
			continue
		}
		if extension.Required && !target.activatesExtension(extension.URI) {
			return fmt.Errorf("card requires unsupported A2A extension %q: %w", extension.URI, ErrRemoteExtensionUnsupported)
		}
	}
	if !foundTokenBudget {
		return fmt.Errorf("card does not advertise the required token-budget extension")
	}
	return nil
}
