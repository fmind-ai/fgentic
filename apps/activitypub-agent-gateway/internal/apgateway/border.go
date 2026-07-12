package apgateway

import (
	"context"
	"errors"
	"log/slog"
	"net/http"

	"github.com/fmind/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind/activitypub-agent-gateway/internal/policy"
)

// Border is the ActivityPub federation policy border: it verifies an inbound HTTP Signature, binds
// the signing key's owner to the activity's actor, and admits the request only if that actor is on
// the git-reloadable allowlist. It is the AP twin of the Synapse callback border (docs/fediverse.md
// §3). Every denial fails closed and yields content-free evidence — never an actor URI or content.
type Border struct {
	verifier *httpsig.Verifier
	store    *policy.Store
	log      *slog.Logger
}

// NewBorder wires a verifier and a hot-reloadable policy store into the border.
func NewBorder(verifier *httpsig.Verifier, store *policy.Store, log *slog.Logger) *Border {
	return &Border{verifier: verifier, store: store, log: log}
}

// BorderDecision is a content-free admission outcome for logs and metrics.
type BorderDecision struct {
	Allowed bool
	Reason  string
	Digest  string
}

// Authorize runs the full border check for an inbound activity from actorURI. The request's raw
// body must be passed so the signed digest can be verified against it.
func (b *Border) Authorize(ctx context.Context, req *http.Request, body []byte, actorURI string) BorderDecision {
	result, err := b.verifier.Verify(ctx, req, body)
	if err != nil {
		return BorderDecision{Allowed: false, Reason: signatureReason(err), Digest: "none"}
	}
	// Bind the proven signer to the claimed author: a valid signature from key K only authorizes
	// activities whose actor is K's owner. Otherwise a signer could speak for anyone.
	if result.Owner != actorURI {
		return BorderDecision{Allowed: false, Reason: "actor_key_mismatch", Digest: "none"}
	}
	decision := b.store.Admit(result.Owner)
	return BorderDecision{Allowed: decision.Allowed, Reason: decision.Reason, Digest: decision.Digest}
}

// signatureReason maps a verification error to a stable, content-free reason label.
func signatureReason(err error) string {
	switch {
	case errors.Is(err, httpsig.ErrNoSignature):
		return "unsigned"
	case errors.Is(err, httpsig.ErrStale):
		return "stale_signature"
	case errors.Is(err, httpsig.ErrDigestMismatch):
		return "digest_mismatch"
	case errors.Is(err, httpsig.ErrKeyResolution):
		return "key_unresolved"
	case errors.Is(err, httpsig.ErrUnsupportedAlgorithm):
		return "unsupported_algorithm"
	case errors.Is(err, httpsig.ErrSignatureInvalid):
		return "bad_signature"
	default:
		return "malformed_signature"
	}
}
