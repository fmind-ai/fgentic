package httpsig

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// PinnedResolver resolves a signer's public key from an operator-provided, out-of-band pin for a
// closed set of actor URIs, and delegates every other actor to a fallback resolver unchanged.
//
// It exists because the inbound signature border must fetch a signer's key from its keyId URL, but
// that fetch is deliberately constrained to public HTTPS by the #320 SSRF guard (safehttp): an
// operator-pinned peer that is only reachable in-cluster (a private/loopback address) or otherwise
// not SSRF-fetchable therefore cannot be verified by fetch alone. A pin supplies that peer's key
// out of band, mirroring the platform's existing pinned-remote-agent identity model ("explicitly
// configured remote agents require a currently verified pinned Signed AgentCard"). See ADR 0021 and
// docs/fediverse.md §3.
//
// Security invariant: pinning STRICTLY REDUCES SSRF surface. For a pinned actor no network request
// is made at all; for any unpinned actor the fallback (the guarded HTTPS resolver) runs exactly as
// before. A pin can only ADD a trusted key an operator placed by hand — it can never redirect,
// widen, or relax the guarded path.
type PinnedResolver struct {
	// pins maps an exact actor URI (the keyId with its #fragment stripped) to its resolved key. The
	// resolved Owner is always that same actor URI, so the border's owner==activity-actor binding
	// still holds and a pinned key can never speak for a different actor.
	pins     map[string]PublicKey
	fallback KeyResolver
}

// pinnedKeyFile is the on-disk JSON shape: an exact actor URI -> PKIX/SPKI public-key PEM map. The
// keys are actor URIs (not keyIds) because the border binds the resolved owner to the activity actor.
type pinnedKeyFile struct {
	Pins map[string]string `json:"pins"`
}

// NewPinnedResolver loads the pinned-key map from path and wraps fallback. It fails closed: an
// unreadable file, malformed JSON, an empty map, a blank actor URI, an actor URI carrying a URL
// fragment (which could never equal a fragment-stripped keyId, so it would silently never match),
// or an unparseable/empty PEM all return an error rather than a resolver that would fall through to
// the network for a key the operator believed was pinned.
func NewPinnedResolver(path string, fallback KeyResolver) (*PinnedResolver, error) {
	if fallback == nil {
		return nil, fmt.Errorf("pinned key resolver: a fallback resolver is required")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read pinned keys %s: %w", path, err)
	}
	var file pinnedKeyFile
	if err := json.Unmarshal(data, &file); err != nil {
		return nil, fmt.Errorf("decode pinned keys %s: %w", path, err)
	}
	if len(file.Pins) == 0 {
		return nil, fmt.Errorf("pinned keys %s: no pins configured", path)
	}
	pins := make(map[string]PublicKey, len(file.Pins))
	for actor, pem := range file.Pins {
		if strings.TrimSpace(actor) == "" {
			return nil, fmt.Errorf("pinned keys %s: empty actor URI", path)
		}
		// A keyId is matched after stripping its fragment, so an actor URI that itself carries a
		// fragment could never match and must be rejected rather than silently ignored.
		if strings.Contains(actor, "#") {
			return nil, fmt.Errorf("pinned keys %s: actor URI %q must not contain a fragment", path, actor)
		}
		key, err := ParsePublicKeyPEM(pem)
		if err != nil {
			return nil, fmt.Errorf("pinned keys %s: actor %q: %w", path, actor, err)
		}
		pins[actor] = PublicKey{Key: key, Owner: actor}
	}
	return &PinnedResolver{pins: pins, fallback: fallback}, nil
}

// Count reports how many actors are pinned (for a startup log line).
func (r *PinnedResolver) Count() int { return len(r.pins) }

// Resolve returns the pinned key for keyID's actor with no network call, or delegates to the
// guarded fallback for any unpinned actor. The actor is the keyId with its #fragment stripped, and
// the match is exact — no prefix or substring can select a pin.
func (r *PinnedResolver) Resolve(ctx context.Context, keyID string) (PublicKey, error) {
	actor, _, _ := strings.Cut(keyID, "#")
	if pinned, ok := r.pins[actor]; ok {
		return pinned, nil
	}
	return r.fallback.Resolve(ctx, keyID)
}
