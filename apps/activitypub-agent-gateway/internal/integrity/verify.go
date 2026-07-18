package integrity

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/safehttp"
)

// maxKeyDocBytes bounds an untrusted actor/key document fetch.
const maxKeyDocBytes = 1 << 20 // 1 MiB

// ErrKeyResolution means the proof's verificationMethod could not be resolved to a usable Ed25519
// key (fetch failed, no matching assertionMethod, or a malformed publicKeyMultibase).
var ErrKeyResolution = errors.New("integrity: cannot resolve verification key")

// ResolvedKey is a verified public key and the actor that controls it.
type ResolvedKey struct {
	Key        ed25519.PublicKey
	Controller string
}

// KeyResolver fetches the Ed25519 key named by a proof's verificationMethod. The returned key is
// untrusted trust material: the border still binds Controller to the activity actor.
type KeyResolver interface {
	Resolve(ctx context.Context, verificationMethod string) (ResolvedKey, error)
}

// Verifier verifies an inbound activity's object integrity proof end to end: resolve the key named
// by the proof, then check the eddsa-jcs-2022 signature over the canonicalized document.
type Verifier struct {
	resolver KeyResolver
}

// NewVerifier builds a Verifier over a key resolver.
func NewVerifier(resolver KeyResolver) *Verifier {
	return &Verifier{resolver: resolver}
}

// VerifyDocument verifies doc's embedded proof and returns the key controller. It fails closed on a
// missing, malformed, unsupported, or invalid proof, and on an unresolvable key.
func (v *Verifier) VerifyDocument(ctx context.Context, doc map[string]any) (controller string, err error) {
	proof, ok := doc["proof"].(map[string]any)
	if !ok {
		if _, present := doc["proof"]; present {
			return "", ErrMalformedProof
		}
		return "", ErrNoProof
	}
	vm, _ := proof["verificationMethod"].(string)
	if vm == "" {
		return "", fmt.Errorf("%w: missing verificationMethod", ErrMalformedProof)
	}
	resolved, err := v.resolver.Resolve(ctx, vm)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrKeyResolution, err)
	}
	if _, err := Verify(doc, resolved.Key); err != nil {
		return "", err
	}
	return resolved.Controller, nil
}

// HTTPKeyResolver dereferences a verificationMethod to its controller's actor document and extracts
// the matching Multikey (FEP-8b32 assertionMethod / publicKeyMultibase shape).
type HTTPKeyResolver struct {
	client    *http.Client
	clientErr error
}

// NewHTTPKeyResolver returns a resolver using a guarded clone of client. The clone permits only
// public HTTPS destinations and revalidates DNS at dial time; the in-cluster egress NetworkPolicy
// remains a second, independent control.
func NewHTTPKeyResolver(client *http.Client) *HTTPKeyResolver {
	guarded, err := safehttp.NewClient(client)
	return &HTTPKeyResolver{client: guarded, clientErr: err}
}

// multikey is one FEP-8b32 verification method (Multikey type, publicKeyMultibase encoding).
type multikey struct {
	ID                 string `json:"id"`
	Type               string `json:"type"`
	Controller         string `json:"controller"`
	PublicKeyMultibase string `json:"publicKeyMultibase"`
}

// keyDoc is the subset of an actor (or standalone key) document carrying assertion methods.
type keyDoc struct {
	ID              string          `json:"id"`
	AssertionMethod json.RawMessage `json:"assertionMethod"`
}

// Resolve fetches the actor document addressed by verificationMethod (its fragment stripped) and
// returns the Ed25519 key whose id matches, plus its controller.
func (r *HTTPKeyResolver) Resolve(ctx context.Context, verificationMethod string) (ResolvedKey, error) {
	if r.clientErr != nil {
		return ResolvedKey{}, fmt.Errorf("configure key resolver: %w", r.clientErr)
	}
	docURL, _, _ := strings.Cut(verificationMethod, "#")
	if docURL == "" {
		return ResolvedKey{}, fmt.Errorf("empty verificationMethod")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, docURL, nil)
	if err != nil {
		return ResolvedKey{}, fmt.Errorf("build key request: %w", err)
	}
	if err := safehttp.ValidateURL(req.URL); err != nil {
		return ResolvedKey{}, fmt.Errorf("validate key URL: %w", err)
	}
	req.Header.Set("Accept", "application/activity+json")
	resp, err := r.client.Do(req)
	if err != nil {
		return ResolvedKey{}, fmt.Errorf("fetch %s: %w", docURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ResolvedKey{}, fmt.Errorf("fetch %s: status %d", docURL, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxKeyDocBytes))
	if err != nil {
		return ResolvedKey{}, fmt.Errorf("read %s: %w", docURL, err)
	}
	return selectKey(body, verificationMethod, docURL)
}

// selectKey parses an actor document and returns the Multikey matching verificationMethod. When the
// document declares exactly one assertion method and no explicit id match is possible, that sole key
// is used (some servers omit key ids on inline single-key actors).
func selectKey(body []byte, verificationMethod, docURL string) (ResolvedKey, error) {
	var doc keyDoc
	if err := json.Unmarshal(body, &doc); err != nil {
		return ResolvedKey{}, fmt.Errorf("decode %s: %w", docURL, err)
	}
	keys, err := parseAssertionMethods(doc.AssertionMethod)
	if err != nil {
		return ResolvedKey{}, fmt.Errorf("assertionMethod in %s: %w", docURL, err)
	}
	if len(keys) == 0 {
		return ResolvedKey{}, fmt.Errorf("%s declares no assertionMethod", docURL)
	}

	var chosen *multikey
	for i := range keys {
		if keys[i].ID == verificationMethod {
			chosen = &keys[i]
			break
		}
	}
	if chosen == nil {
		if len(keys) != 1 {
			return ResolvedKey{}, fmt.Errorf("no assertionMethod in %s matches %s", docURL, verificationMethod)
		}
		chosen = &keys[0]
	}

	pub, err := DecodePublicKeyMultibase(chosen.PublicKeyMultibase)
	if err != nil {
		return ResolvedKey{}, err
	}
	controller := chosen.Controller
	if controller == "" {
		controller = doc.ID
	}
	if controller == "" {
		return ResolvedKey{}, fmt.Errorf("%s key has no controller", docURL)
	}
	return ResolvedKey{Key: pub, Controller: controller}, nil
}

// parseAssertionMethods accepts assertionMethod as a single Multikey object or an array of them.
// String references (keys defined elsewhere in the document) are not dereferenced here.
func parseAssertionMethods(raw json.RawMessage) ([]multikey, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var array []multikey
	if err := json.Unmarshal(raw, &array); err == nil {
		return array, nil
	}
	var single multikey
	if err := json.Unmarshal(raw, &single); err != nil {
		return nil, err
	}
	return []multikey{single}, nil
}
