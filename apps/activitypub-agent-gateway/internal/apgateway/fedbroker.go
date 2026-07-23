package apgateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/gowebpki/jcs"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/safehttp"
)

const (
	maxBrokerBodyBytes    = 1 << 20
	maxDiscoveryBodyBytes = 1 << 20
	brokerTransportA2A    = "a2a"
	brokerTransportAP     = "activitypub"
	a2aImplementationName = "A2A"
	activityJSONMediaType = "application/activity+json"
)

var (
	acctLocalRE  = regexp.MustCompile(`^[A-Za-z0-9._~-]+$`)
	acctDomainRE = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?$`)
)

// FediverseIdentity is the operator-pinned FEP-8b32 identity for one acct mapping. The actor and
// verification method are exact URLs; the Multikey is public trust material, never a secret.
type FediverseIdentity struct {
	ActorID            string `json:"actorId"`
	VerificationMethod string `json:"verificationMethod"`
	PublicKeyMultibase string `json:"publicKeyMultibase"`
	ProofMaxAgeSeconds int64  `json:"proofMaxAgeSeconds"`
}

type fedBrokerRequest struct {
	Handle    string            `json:"handle"`
	Identity  FediverseIdentity `json:"activityPubIdentity"`
	Sender    string            `json:"sender,omitempty"`
	MessageID string            `json:"messageId,omitempty"`
	Text      string            `json:"text,omitempty"`
	ContextID string            `json:"contextId,omitempty"`
}

// FediverseResolution is the broker's bounded discovery result. A2A endpoints remain untrusted
// until the Matrix bridge verifies the pinned Signed AgentCard; AP fallback has already passed the
// exact FEP-8b32 pin before this value is returned.
type FediverseResolution struct {
	Transport   string `json:"transport"`
	ActorID     string `json:"actorId"`
	Name        string `json:"name,omitempty"`
	Summary     string `json:"summary,omitempty"`
	Inbox       string `json:"inbox,omitempty"`
	A2AEndpoint string `json:"a2aEndpoint,omitempty"`
	AgentCard   string `json:"agentCard,omitempty"`
	ActivityID  string `json:"activityId,omitempty"`
}

type webFingerDocument struct {
	Subject string `json:"subject"`
	Links   []struct {
		Rel  string `json:"rel"`
		Type string `json:"type"`
		Href string `json:"href"`
	} `json:"links"`
}

type remoteActorDocument struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Name       string `json:"name"`
	Summary    string `json:"summary"`
	Inbox      string `json:"inbox"`
	Implements []struct {
		Name      string `json:"name"`
		Href      string `json:"href"`
		AgentCard string `json:"agentCard"`
	} `json:"implements"`
}

// UseFediverseBroker enables the private Matrix-to-Fediverse broker. It deliberately rides the
// internal metrics listener: the public Gateway API route exposes only /ap and well-known paths.
func (g *Gateway) UseFediverseBroker(token string, client *http.Client) error {
	if token == "" || strings.TrimSpace(token) != token {
		return fmt.Errorf("gateway: fediverse broker token must be non-empty without surrounding whitespace")
	}
	if g.signer == nil || g.deliverer == nil {
		return fmt.Errorf("gateway: fediverse broker requires object and HTTP-signature delivery keys")
	}
	guarded, err := safehttp.NewClient(client)
	if err != nil {
		return fmt.Errorf("gateway: configure fediverse discovery client: %w", err)
	}
	g.fedBrokerToken = token
	g.fedBrokerClient = guarded
	return nil
}

// FediverseBrokerHandler returns the authenticated internal broker routes.
func (g *Gateway) FediverseBrokerHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /internal/v1/fediverse/resolve", g.handleFediverseResolve)
	mux.HandleFunc("POST /internal/v1/fediverse/delegate", g.handleFediverseDelegate)
	return mux
}

func (g *Gateway) handleFediverseResolve(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeBroker(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	req, err := decodeBrokerRequest(w, r)
	if err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	resolved, err := g.resolveFediverse(r.Context(), req.Handle, req.Identity)
	if err != nil {
		g.log.Warn("fediverse broker resolution rejected", "reason", "discovery_or_trust")
		http.Error(w, "remote identity unavailable", http.StatusBadGateway)
		return
	}
	writeJSON(w, resolved)
}

func (g *Gateway) handleFediverseDelegate(w http.ResponseWriter, r *http.Request) {
	if !g.authorizeBroker(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	req, err := decodeBrokerRequest(w, r)
	if err != nil || req.Sender == "" || req.MessageID == "" || req.Text == "" || len(req.MessageID) > 256 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	resolved, err := g.resolveFediverse(r.Context(), req.Handle, req.Identity)
	if err != nil {
		g.log.Warn("fediverse broker delegation rejected", "reason", "discovery_or_trust")
		http.Error(w, "remote identity unavailable", http.StatusBadGateway)
		return
	}
	if resolved.Transport != brokerTransportAP {
		// A newly-advertised A2A transport must win. The bridge refreshes discovery and performs its
		// Signed AgentCard verification instead of silently downgrading to ActivityPub.
		writeJSONStatus(w, resolved, http.StatusConflict)
		return
	}
	activityID, raw, err := g.marshalFediverseDelegation(req, resolved)
	if err != nil {
		http.Error(w, "cannot prepare delegation", http.StatusInternalServerError)
		return
	}
	if err := g.deliverer.Deliver(r.Context(), resolved.Inbox, g.baseURL+"/ap/instance", raw); err != nil {
		g.log.Warn("fediverse broker delivery failed", "actor", resolved.ActorID, "error", err)
		http.Error(w, "remote delivery failed", http.StatusBadGateway)
		return
	}
	resolved.ActivityID = activityID
	writeJSON(w, resolved)
}

func (g *Gateway) authorizeBroker(r *http.Request) bool {
	provided, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || g.fedBrokerToken == "" || len(provided) != len(g.fedBrokerToken) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(provided), []byte(g.fedBrokerToken)) == 1
}

func decodeBrokerRequest(w http.ResponseWriter, r *http.Request) (fedBrokerRequest, error) {
	var req fedBrokerRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBrokerBodyBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return req, err
	}
	return req, nil
}

func (g *Gateway) resolveFediverse(ctx context.Context, handle string, pin FediverseIdentity) (FediverseResolution, error) {
	canonical, domain, err := parseAcctHandle(handle)
	if err != nil {
		return FediverseResolution{}, err
	}
	if err := validateFediversePin(pin); err != nil {
		return FediverseResolution{}, err
	}
	wfURL := &url.URL{Scheme: "https", Host: domain, Path: "/.well-known/webfinger"}
	query := wfURL.Query()
	query.Set("resource", canonical)
	wfURL.RawQuery = query.Encode()
	var wf webFingerDocument
	if _, err := g.fetchDiscoveryJSON(ctx, wfURL.String(), "application/jrd+json", &wf); err != nil {
		return FediverseResolution{}, fmt.Errorf("fetch WebFinger: %w", err)
	}
	if wf.Subject != canonical {
		return FediverseResolution{}, fmt.Errorf("WebFinger subject does not match requested handle")
	}
	actorURL := ""
	for _, link := range wf.Links {
		if link.Rel == "self" && (link.Type == activityJSONMediaType || link.Type == "application/ld+json") {
			if actorURL != "" {
				return FediverseResolution{}, fmt.Errorf("WebFinger declares multiple actor links")
			}
			actorURL = link.Href
		}
	}
	if actorURL == "" || actorURL != pin.ActorID {
		return FediverseResolution{}, fmt.Errorf("WebFinger actor does not match pinned actor")
	}
	var actor remoteActorDocument
	raw, err := g.fetchDiscoveryJSON(ctx, actorURL, activityJSONMediaType, &actor)
	if err != nil {
		return FediverseResolution{}, fmt.Errorf("fetch actor: %w", err)
	}
	if actor.ID != pin.ActorID || (actor.Type != "Service" && actor.Type != "Application") {
		return FediverseResolution{}, fmt.Errorf("actor identity or type is invalid")
	}
	resolution := FediverseResolution{ActorID: actor.ID, Name: actor.Name, Summary: actor.Summary}
	for _, implementation := range actor.Implements {
		if implementation.Name != a2aImplementationName {
			continue
		}
		if resolution.A2AEndpoint != "" {
			return FediverseResolution{}, fmt.Errorf("actor declares multiple A2A implementations")
		}
		if err := validatePublicHTTPS(implementation.Href); err != nil {
			return FediverseResolution{}, fmt.Errorf("A2A endpoint: %w", err)
		}
		if err := validatePublicHTTPS(implementation.AgentCard); err != nil {
			return FediverseResolution{}, fmt.Errorf("AgentCard URL: %w", err)
		}
		resolution.Transport = brokerTransportA2A
		resolution.A2AEndpoint = implementation.Href
		resolution.AgentCard = implementation.AgentCard
	}
	if resolution.Transport == brokerTransportA2A {
		return resolution, nil
	}
	if err := validatePublicHTTPS(actor.Inbox); err != nil {
		return FediverseResolution{}, fmt.Errorf("actor inbox: %w", err)
	}
	if err := verifyPinnedActor(raw, pin, g.now()); err != nil {
		return FediverseResolution{}, err
	}
	resolution.Transport = brokerTransportAP
	resolution.Inbox = actor.Inbox
	return resolution, nil
}

func (g *Gateway) fetchDiscoveryJSON(ctx context.Context, rawURL, accept string, dst any) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if err := safehttp.ValidateURL(req.URL); err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	resp, err := g.fedBrokerClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBodyBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDiscoveryBodyBytes {
		return nil, fmt.Errorf("response exceeds %d bytes", maxDiscoveryBodyBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dst); err != nil {
		return nil, err
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	return body, nil
}

func verifyPinnedActor(raw []byte, pin FediverseIdentity, now time.Time) error {
	if _, err := jcs.Transform(raw); err != nil {
		return fmt.Errorf("actor is not canonicalizable I-JSON")
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return fmt.Errorf("decode actor proof: %w", err)
	}
	proof, ok := doc["proof"].(map[string]any)
	if !ok || proof["verificationMethod"] != pin.VerificationMethod {
		return fmt.Errorf("actor proof does not use pinned verification method")
	}
	createdRaw, _ := proof["created"].(string)
	created, err := time.Parse(time.RFC3339, createdRaw)
	if err != nil {
		return fmt.Errorf("actor proof has an invalid created timestamp")
	}
	if created.After(now.Add(5 * time.Minute)) {
		return fmt.Errorf("actor proof was created in the future")
	}
	if now.Sub(created) > time.Duration(pin.ProofMaxAgeSeconds)*time.Second {
		return fmt.Errorf("actor proof has expired")
	}
	pub, err := integrity.DecodePublicKeyMultibase(pin.PublicKeyMultibase)
	if err != nil {
		return fmt.Errorf("pinned actor key: %w", err)
	}
	if _, err := integrity.Verify(doc, pub); err != nil {
		return fmt.Errorf("verify pinned actor proof: %w", err)
	}
	return nil
}

func validateFediversePin(pin FediverseIdentity) error {
	if err := validatePublicHTTPS(pin.ActorID); err != nil {
		return fmt.Errorf("pinned actor ID: %w", err)
	}
	if !strings.HasPrefix(pin.VerificationMethod, pin.ActorID+"#") {
		return fmt.Errorf("pinned verification method must be an actor fragment")
	}
	if _, err := integrity.DecodePublicKeyMultibase(pin.PublicKeyMultibase); err != nil {
		return fmt.Errorf("pinned actor key: %w", err)
	}
	if pin.ProofMaxAgeSeconds <= 0 || pin.ProofMaxAgeSeconds > int64((30*24*time.Hour)/time.Second) {
		return fmt.Errorf("pinned actor proof max age must be between 1 second and 30 days")
	}
	return nil
}

func parseAcctHandle(raw string) (canonical, domain string, err error) {
	rest, ok := strings.CutPrefix(raw, "acct:")
	if !ok {
		return "", "", fmt.Errorf("handle must start with the acct scheme")
	}
	local, host, ok := strings.Cut(rest, "@")
	if !ok || !acctLocalRE.MatchString(local) || !acctDomainRE.MatchString(host) {
		return "", "", fmt.Errorf("handle is invalid")
	}
	host = strings.ToLower(host)
	if !validAcctDomain(host) {
		return "", "", fmt.Errorf("handle domain must be fully qualified")
	}
	return "acct:" + local + "@" + host, host, nil
}

func validAcctDomain(domain string) bool {
	if len(domain) > 253 || !strings.Contains(domain, ".") {
		return false
	}
	for label := range strings.SplitSeq(domain, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, char := range label {
			if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
				return false
			}
		}
	}
	return true
}

func validatePublicHTTPS(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return err
	}
	return safehttp.ValidateURL(parsed)
}

func (g *Gateway) marshalFediverseDelegation(req fedBrokerRequest, resolved FediverseResolution) (string, []byte, error) {
	sum := sha256.Sum256([]byte(req.Handle + "\x00" + req.MessageID))
	id := hex.EncodeToString(sum[:])
	actorID := g.baseURL + "/ap/instance"
	activityID := actorID + "/activities/delegations/" + id
	noteID := actorID + "/notes/delegations/" + id
	content := html.EscapeString("Matrix sender " + req.Sender + ": " + req.Text)
	doc := map[string]any{
		"@context": []any{integrity.ActivityStreamsContext, integrity.DataIntegrityContext},
		"id":       activityID,
		"type":     "Create",
		"actor":    actorID,
		"to":       []any{resolved.ActorID},
		"object": map[string]any{
			"id":           noteID,
			"type":         "Note",
			"attributedTo": actorID,
			"content":      content,
			"to":           []any{resolved.ActorID},
			"tag": []any{map[string]any{
				"type": "Mention",
				"href": resolved.ActorID,
				"name": req.Handle,
			}},
		},
	}
	if req.ContextID != "" {
		doc["context"] = actorID + "/contexts/" + url.PathEscape(req.ContextID)
	}
	if err := g.signer.SignActivity(doc, actorID); err != nil {
		return "", nil, fmt.Errorf("sign fediverse delegation: %w", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", nil, fmt.Errorf("encode fediverse delegation: %w", err)
	}
	return activityID, raw, nil
}

func writeJSONStatus(w http.ResponseWriter, doc any, status int) {
	data, err := json.Marshal(doc)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}
