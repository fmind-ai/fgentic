package a2aclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/a2aproject/a2a-go/v2/a2a"
	sdk "github.com/a2aproject/a2a-go/v2/a2aclient"
	"github.com/gowebpki/jcs"
)

const (
	remoteAgentCardPath = "/.well-known/agent-card.json"
	maxAgentCardBytes   = 1 << 20
)

// ErrRemoteTargetUntrusted marks a remote target that cannot be delegated to because no
// currently verified signed AgentCard is installed. Call and PollTask never fetch remote trust
// material implicitly: startup and periodic profile refresh own that network boundary.
var ErrRemoteTargetUntrusted = errors.New("remote A2A target has no verified AgentCard")

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

	resp, err := c.remoteHTTPClient.Do(req)
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
			return nil, c.quarantineRemote(target, "server returned 304 without a verified cached card")
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
			trustErr := c.quarantineRemote(target, "card was withdrawn with HTTP %d", resp.StatusCode)
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
		return nil, c.quarantineRemote(target, "card exceeds %d bytes", maxAgentCardBytes)
	}
	if !isJSONMediaType(resp.Header.Get("Content-Type")) {
		if err := resp.Body.Close(); err != nil {
			return nil, fmt.Errorf("close non-JSON remote AgentCard response %s: %w", cardURL, err)
		}
		return nil, c.quarantineRemote(target, "card response is not JSON")
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
		return nil, c.quarantineRemote(target, "card exceeds %d bytes", maxAgentCardBytes)
	}

	card, err := verifyRemoteAgentCard(raw, target)
	if err != nil {
		return nil, c.quarantineRemote(target, "%v", err)
	}
	generation := nextGeneration(previous.generation)
	client, err := buildSDKClient(ctx, card, c.remoteSDKHTTPClient(target.ID(), generation), true)
	if err != nil {
		return nil, c.quarantineRemote(target, "build client from verified card: %v", err)
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

func (c *Client) quarantineRemote(target Target, format string, args ...any) error {
	reason := fmt.Sprintf(format, args...)
	c.mu.Lock()
	c.cache[target.ID()] = cachedTarget{generation: nextGeneration(c.cache[target.ID()].generation)}
	c.mu.Unlock()
	c.log.Warn("quarantined remote a2a agent", "target", target.String(), "reason", reason)
	return fmt.Errorf("%w: %s: %s", ErrRemoteTargetUntrusted, target.String(), reason)
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
	payload, signatures, err := canonicalSignedCardPayload(raw)
	if err != nil {
		return nil, err
	}

	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	var card a2a.AgentCard
	if err := decoder.Decode(&card); err != nil {
		return nil, fmt.Errorf("card JSON does not match the A2A schema")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("card has trailing JSON data")
	}
	if err := validateRemoteCardContract(&card, target); err != nil {
		return nil, err
	}
	if err := verifyCardSignatures(payload, signatures, target); err != nil {
		return nil, err
	}
	return &card, nil
}

func canonicalSignedCardPayload(raw []byte) ([]byte, []a2a.AgentCardSignature, error) {
	// Decode before invoking the recursive JCS implementation so encoding/json's bounded
	// nesting check rejects adversarially deep input. JCS then catches duplicate object keys
	// before the typed decoder and signature verifier can observe a different document.
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var document map[string]any
	if err := decoder.Decode(&document); err != nil {
		return nil, nil, fmt.Errorf("card is not valid JSON")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return nil, nil, fmt.Errorf("card has trailing JSON data")
	}
	if _, err := jcs.Transform(raw); err != nil {
		return nil, nil, fmt.Errorf("card is not valid canonicalizable I-JSON")
	}
	if err := validateRequiredCardJSON(document); err != nil {
		return nil, nil, err
	}

	rawSignatures, ok := document["signatures"]
	if !ok {
		return nil, nil, fmt.Errorf("card is unsigned")
	}
	signatureJSON, err := json.Marshal(rawSignatures)
	if err != nil {
		return nil, nil, fmt.Errorf("encode card signatures: %w", err)
	}
	var signatures []a2a.AgentCardSignature
	if err := json.Unmarshal(signatureJSON, &signatures); err != nil {
		return nil, nil, fmt.Errorf("card signatures do not match the A2A schema")
	}
	if len(signatures) == 0 {
		return nil, nil, fmt.Errorf("card is unsigned")
	}
	delete(document, "signatures")
	normalizeCardDefaults(document)
	unsignedJSON, err := json.Marshal(document)
	if err != nil {
		return nil, nil, fmt.Errorf("encode unsigned card: %w", err)
	}
	payload, err := jcs.Transform(unsignedJSON)
	if err != nil {
		return nil, nil, fmt.Errorf("canonicalize unsigned card: %w", err)
	}
	return payload, signatures, nil
}

func validateRequiredCardJSON(document map[string]any) error {
	for _, field := range []string{
		"name",
		"description",
		"supportedInterfaces",
		"version",
		"capabilities",
		"defaultInputModes",
		"defaultOutputModes",
		"skills",
	} {
		if _, exists := document[field]; !exists {
			return fmt.Errorf("card is missing a required A2A field")
		}
	}
	provider, ok := document["provider"].(map[string]any)
	if !ok {
		return fmt.Errorf("card provider is missing")
	}
	if _, exists := provider["url"]; !exists {
		return fmt.Errorf("card provider is missing a required A2A field")
	}
	if _, exists := provider["organization"]; !exists {
		return fmt.Errorf("card provider is missing a required A2A field")
	}

	interfaces, ok := document["supportedInterfaces"].([]any)
	if !ok || len(interfaces) == 0 {
		return fmt.Errorf("card has no supported interfaces")
	}
	for _, value := range interfaces {
		agentInterface, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("card contains an invalid interface")
		}
		for _, field := range []string{"url", "protocolBinding", "protocolVersion"} {
			if _, exists := agentInterface[field]; !exists {
				return fmt.Errorf("card interface is missing a required A2A field")
			}
		}
	}

	for _, field := range []string{"defaultInputModes", "defaultOutputModes"} {
		values, ok := document[field].([]any)
		if !ok || len(values) == 0 {
			return fmt.Errorf("card must advertise default input and output modes")
		}
	}
	skills, ok := document["skills"].([]any)
	if !ok || len(skills) == 0 {
		return fmt.Errorf("card must advertise at least one skill")
	}
	for _, value := range skills {
		skill, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("card contains an invalid skill")
		}
		for _, field := range []string{"id", "name", "description", "tags"} {
			if _, exists := skill[field]; !exists {
				return fmt.Errorf("card skill is missing a required A2A field")
			}
		}
		tags, ok := skill["tags"].([]any)
		if !ok || len(tags) == 0 {
			return fmt.Errorf("card skill must advertise at least one tag")
		}
	}
	return nil
}

func expectJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return fmt.Errorf("trailing data: %w", err)
	}
	return nil
}

// normalizeCardDefaults applies the protobuf presence rules required by A2A v1.0 before JCS.
// Values explicitly present for proto optional fields and all REQUIRED fields are retained;
// defaults for ordinary scalar, repeated, and map fields are removed.
func normalizeCardDefaults(document map[string]any) {
	cleanJSONObject(document)
}

func cleanJSONObject(document map[string]any) {
	deleteEmptyField(document, "securitySchemes")
	deleteEmptyField(document, "securityRequirements")
	if securitySchemes, ok := document["securitySchemes"].(map[string]any); ok {
		normalizeSecuritySchemes(securitySchemes)
	}
	if securityRequirements, ok := document["securityRequirements"].([]any); ok {
		normalizeSecurityRequirements(securityRequirements)
	}

	capabilities, _ := document["capabilities"].(map[string]any)
	if capabilities != nil {
		deleteEmptyField(capabilities, "extensions")
		if extensions, ok := capabilities["extensions"].([]any); ok {
			for _, value := range extensions {
				extension, _ := value.(map[string]any)
				if extension == nil {
					continue
				}
				deleteDefaultScalar(extension, "uri")
				deleteDefaultScalar(extension, "description")
				deleteDefaultScalar(extension, "required")
				// Params is a protobuf Struct: a nonempty subtree is extension-owned data,
				// so nested false/empty values must never be interpreted as proto defaults.
				deleteEmptyField(extension, "params")
			}
		}
	}

	if interfaces, ok := document["supportedInterfaces"].([]any); ok {
		for _, value := range interfaces {
			agentInterface, _ := value.(map[string]any)
			if agentInterface != nil {
				deleteDefaultScalar(agentInterface, "tenant")
			}
		}
	}

	if skills, ok := document["skills"].([]any); ok {
		for _, value := range skills {
			skill, _ := value.(map[string]any)
			if skill == nil {
				continue
			}
			deleteEmptyField(skill, "examples")
			deleteEmptyField(skill, "inputModes")
			deleteEmptyField(skill, "outputModes")
			deleteEmptyField(skill, "securityRequirements")
			if securityRequirements, ok := skill["securityRequirements"].([]any); ok {
				normalizeSecurityRequirements(securityRequirements)
			}
		}
	}
}

func normalizeSecurityRequirements(requirements []any) {
	for _, value := range requirements {
		requirement, _ := value.(map[string]any)
		if requirement == nil {
			continue
		}
		if schemes, ok := requirement["schemes"].(map[string]any); ok {
			for _, scopesValue := range schemes {
				if scopes, ok := scopesValue.(map[string]any); ok {
					deleteEmptyField(scopes, "list")
				}
			}
		}
		deleteEmptyField(requirement, "schemes")
	}
}

func normalizeSecuritySchemes(schemes map[string]any) {
	for _, value := range schemes {
		wrapper, _ := value.(map[string]any)
		if wrapper == nil {
			continue
		}
		for schemeType, schemeValue := range wrapper {
			scheme, _ := schemeValue.(map[string]any)
			if scheme == nil {
				continue
			}
			switch schemeType {
			case "apiKeySecurityScheme":
				deleteDefaultScalar(scheme, "description")
			case "httpAuthSecurityScheme":
				deleteDefaultScalar(scheme, "description")
				deleteDefaultScalar(scheme, "bearerFormat")
			case "openIdConnectSecurityScheme", "mtlsSecurityScheme":
				deleteDefaultScalar(scheme, "description")
			case "oauth2SecurityScheme":
				normalizeOAuth2SecurityScheme(scheme)
			}
		}
	}
}

func normalizeOAuth2SecurityScheme(scheme map[string]any) {
	deleteDefaultScalar(scheme, "description")
	deleteDefaultScalar(scheme, "oauth2MetadataUrl")
	flows, _ := scheme["flows"].(map[string]any)
	for flowType, flowValue := range flows {
		flow, _ := flowValue.(map[string]any)
		if flow == nil {
			continue
		}
		deleteDefaultScalar(flow, "refreshUrl")
		switch flowType {
		case "authorizationCode":
			deleteDefaultScalar(flow, "pkceRequired")
		case "implicit":
			deleteDefaultScalar(flow, "authorizationUrl")
			deleteEmptyField(flow, "scopes")
		case "password":
			deleteDefaultScalar(flow, "tokenUrl")
			deleteEmptyField(flow, "scopes")
		}
	}
}

func deleteEmptyField(object map[string]any, key string) {
	value, exists := object[key]
	if !exists {
		return
	}
	switch typed := value.(type) {
	case nil:
		delete(object, key)
	case []any:
		if len(typed) == 0 {
			delete(object, key)
		}
	case map[string]any:
		if len(typed) == 0 {
			delete(object, key)
		}
	}
}

func deleteDefaultScalar(object map[string]any, key string) {
	value, exists := object[key]
	if !exists {
		return
	}
	if value == "" || value == false || value == nil {
		delete(object, key)
	}
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
		endpoint, err := normalizeRemoteURL(agentInterface.URL)
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
	foundTokenBudget := false
	for _, extension := range card.Capabilities.Extensions {
		if extension.URI == TokenBudgetExtensionURI {
			foundTokenBudget = true
			continue
		}
		if extension.Required {
			return fmt.Errorf("card requires an unsupported A2A extension")
		}
	}
	if foundTokenBudget {
		return nil
	}
	return fmt.Errorf("card does not advertise the required token-budget extension")
}

func verifyCardSignatures(payload []byte, signatures []a2a.AgentCardSignature, target Target) error {
	publicKey := target.es256PublicKey()
	if publicKey == nil {
		return fmt.Errorf("pinned ES256 public key is invalid")
	}
	for _, signature := range signatures {
		matches, err := verifyCardSignature(payload, signature, target.expectedKeyID, publicKey)
		if err != nil || !matches {
			continue
		}
		return nil
	}
	return fmt.Errorf("card has no valid ES256 signature for pinned key ID %q", target.expectedKeyID)
}

func verifyCardSignature(
	payload []byte,
	signature a2a.AgentCardSignature,
	expectedKeyID string,
	publicKey *ecdsa.PublicKey,
) (bool, error) {
	protectedJSON, err := base64.RawURLEncoding.Strict().DecodeString(signature.Protected)
	if err != nil {
		return false, fmt.Errorf("JWS protected header is not valid base64url")
	}
	decoder := json.NewDecoder(strings.NewReader(string(protectedJSON)))
	decoder.UseNumber()
	var protected map[string]any
	if err := decoder.Decode(&protected); err != nil {
		return false, fmt.Errorf("JWS protected header is not a JSON object")
	}
	if err := expectJSONEOF(decoder); err != nil {
		return false, fmt.Errorf("JWS protected header has trailing data")
	}
	if _, err := jcs.Transform(protectedJSON); err != nil {
		return false, fmt.Errorf("JWS protected header is not valid I-JSON")
	}
	if _, exists := signature.Header["crit"]; exists {
		return false, fmt.Errorf("JWS unprotected header contains crit")
	}
	if _, exists := signature.Header["b64"]; exists {
		return false, fmt.Errorf("JWS unprotected header contains b64")
	}
	for _, protectedName := range []string{"alg", "kid", "jku", "typ"} {
		if _, exists := signature.Header[protectedName]; exists {
			return false, fmt.Errorf("JWS parameter must be protected")
		}
	}
	for name := range signature.Header {
		if _, exists := protected[name]; exists {
			return false, fmt.Errorf("JWS protected and unprotected headers overlap")
		}
	}
	if _, exists := protected["crit"]; exists {
		return false, fmt.Errorf("JWS critical extensions are unsupported")
	}
	if _, exists := protected["b64"]; exists {
		return false, fmt.Errorf("JWS b64 mode is unsupported")
	}
	algorithm, ok := protected["alg"].(string)
	if !ok || algorithm != "ES256" {
		return false, fmt.Errorf("JWS alg is not ES256")
	}
	keyID, ok := protected["kid"].(string)
	if !ok || keyID != expectedKeyID {
		return false, fmt.Errorf("JWS kid does not match pinned key ID")
	}
	if typ, exists := protected["typ"]; exists {
		typString, ok := typ.(string)
		if !ok || typString != "JOSE" {
			return false, fmt.Errorf("JWS typ is not JOSE")
		}
	}

	signatureBytes, err := base64.RawURLEncoding.Strict().DecodeString(signature.Signature)
	if err != nil {
		return false, fmt.Errorf("JWS signature is not valid base64url")
	}
	if len(signatureBytes) != 64 {
		return false, fmt.Errorf("ES256 signature is %d bytes, want 64", len(signatureBytes))
	}
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(signature.Protected + "." + encodedPayload))
	r := signatureBytes[:32]
	s := signatureBytes[32:]
	if !ecdsa.Verify(publicKey, digest[:], new(big.Int).SetBytes(r), new(big.Int).SetBytes(s)) {
		return false, fmt.Errorf("ES256 signature verification failed")
	}
	return true, nil
}
