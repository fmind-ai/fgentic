package apgateway

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/budget"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/integrity"
	"github.com/fmind-ai/activitypub-agent-gateway/internal/policy"
)

// Border is the ActivityPub federation policy border: it verifies an inbound HTTP Signature, binds
// the signing key's owner to the activity's actor, and admits the request only if that actor is on
// the git-reloadable allowlist. It is the AP twin of the Synapse callback border (docs/fediverse.md
// §3). Every denial fails closed and yields content-free evidence — never an actor URI or content.
type Border struct {
	verifier  *httpsig.Verifier
	store     *policy.Store
	integrity *integrity.Verifier // optional FEP-8b32 object-integrity gate; nil skips it
	reserver  *budget.Reserver    // optional per-actor/domain token-budget reserver; nil skips it
	log       *slog.Logger
}

// NewBorder wires a verifier and a hot-reloadable policy store into the border.
func NewBorder(verifier *httpsig.Verifier, store *policy.Store, log *slog.Logger) *Border {
	return &Border{verifier: verifier, store: store, log: log}
}

// RequireObjectIntegrity adds a mandatory FEP-8b32 object-integrity check: after transport signature
// and allowlist, an inbound activity must ALSO carry a valid eddsa-jcs-2022 proof whose key controller
// is the activity actor. Missing/invalid proofs fail closed, so a relayed or tampered object cannot
// be laundered through a trusted actor even if the transport hop was signed (docs/fediverse.md §3).
func (b *Border) RequireObjectIntegrity(v *integrity.Verifier) { b.integrity = v }

// RequireBudget adds a per-actor/per-domain token-budget admission reservation: after signature,
// allowlist, and object integrity, an inbound delegation must reserve its token ceiling from the
// verified actor's and domain's git-configured pools, or it is rejected before any A2A/LLM call. An
// allowlisted domain with no configured budget is denied (deny-by-default). Reservation ≠ consumption.
func (b *Border) RequireBudget(r *budget.Reserver) { b.reserver = r }

// BudgetEnforced reports whether token-budget admission is active (drives the reservation metric).
func (b *Border) BudgetEnforced() bool { return b != nil && b.reserver != nil }

// BorderDecision is a content-free admission outcome for logs and metrics.
type BorderDecision struct {
	Allowed bool
	Reason  string
	Digest  string
	// BudgetOutcome is "reserved" when a token reservation was taken, "rejected" when the budget
	// gate denied admission, and "" when budget was not evaluated. It feeds the reservation metric
	// and is never a consumption figure.
	BudgetOutcome string
}

// Authorize runs the full border check for an inbound activity from actorURI. The request's raw
// body must be passed so the signed digest can be verified against it.
func (b *Border) Authorize(ctx context.Context, req *http.Request, body []byte, actorURI string) BorderDecision {
	decision := b.VerifyDelegation(ctx, req, body, actorURI)
	if !decision.Allowed {
		return decision
	}
	return b.ReserveBudget(actorURI, decision.Digest)
}

// VerifyDelegation runs every identity and policy check for delegating content except the budget
// reservation. Durable inbox intake calls this before inserting the activity ID, then the unique
// background job reserves budget exactly once.
func (b *Border) VerifyDelegation(ctx context.Context, req *http.Request, body []byte, actorURI string) BorderDecision {
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
	if !decision.Allowed {
		return BorderDecision{Allowed: false, Reason: decision.Reason, Digest: decision.Digest}
	}

	if o := b.VerifyObject(ctx, body, actorURI, decision.Digest); !o.Allowed {
		return o
	}
	return BorderDecision{Allowed: true, Reason: "allowlisted", Digest: decision.Digest}
}

// VerifyTransport runs ONLY the transport-hop checks — signature, actor-key binding, and the
// allowlist — with no object-integrity or budget step. It admits non-delegating activities such as a
// Group Follow, which must be allowlist-gated but neither carry a per-object proof nor spend budget.
func (b *Border) VerifyTransport(ctx context.Context, req *http.Request, body []byte, actorURI string) BorderDecision {
	result, err := b.verifier.Verify(ctx, req, body)
	if err != nil {
		return BorderDecision{Allowed: false, Reason: signatureReason(err), Digest: "none"}
	}
	if result.Owner != actorURI {
		return BorderDecision{Allowed: false, Reason: "actor_key_mismatch", Digest: "none"}
	}
	decision := b.store.Admit(result.Owner)
	return BorderDecision{Allowed: decision.Allowed, Reason: decision.Reason, Digest: decision.Digest}
}

// VerifyObject runs the optional FEP-8b32 object-integrity check on an already-transport-verified
// body, binding the proof's key controller to actorURI. It is a no-op (allow) when integrity is
// disabled. digest is the policy digest carried from the transport decision.
func (b *Border) VerifyObject(ctx context.Context, body []byte, actorURI, digest string) BorderDecision {
	if b.integrity == nil {
		return BorderDecision{Allowed: true, Reason: "allowlisted", Digest: digest}
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return BorderDecision{Allowed: false, Reason: "malformed_object", Digest: digest}
	}
	controller, err := b.integrity.VerifyDocument(ctx, doc)
	if err != nil {
		return BorderDecision{Allowed: false, Reason: objectIntegrityReason(err), Digest: digest}
	}
	if controller != actorURI {
		return BorderDecision{Allowed: false, Reason: "object_actor_mismatch", Digest: digest}
	}
	return BorderDecision{Allowed: true, Reason: "allowlisted", Digest: digest}
}

// ReserveBudget runs the optional per-actor/domain token-budget reservation for a delegation. It is a
// no-op (allow) when budgets are disabled; otherwise it denies deny-by-default (no budget) or over
// budget before any A2A call. A reservation gates admission and is never consumption (D8).
func (b *Border) ReserveBudget(actorURI, digest string) BorderDecision {
	if b.reserver == nil {
		return BorderDecision{Allowed: true, Reason: "allowlisted", Digest: digest}
	}
	grant, ok := b.store.Budget(actorURI)
	if !ok {
		return BorderDecision{Allowed: false, Reason: "no_budget", Digest: digest, BudgetOutcome: "rejected"}
	}
	if !b.reserver.Reserve(actorURI, actorDomain(actorURI), grant.Reservation, grant.ActorPool, grant.DomainPool) {
		return BorderDecision{Allowed: false, Reason: "over_budget", Digest: digest, BudgetOutcome: "rejected"}
	}
	return BorderDecision{Allowed: true, Reason: "allowlisted", Digest: digest, BudgetOutcome: "reserved"}
}

// actorDomain extracts the lowercased host of an actor URI for the per-domain budget key. A
// malformed URI yields "", which shares one bounded bucket rather than fanning out cardinality.
func actorDomain(actorURI string) string {
	parsed, err := url.Parse(actorURI)
	if err != nil {
		return ""
	}
	return strings.ToLower(parsed.Host)
}

// objectIntegrityReason maps an object-integrity failure to a stable, content-free reason label.
func objectIntegrityReason(err error) string {
	switch {
	case errors.Is(err, integrity.ErrNoProof):
		return "unsigned_object"
	case errors.Is(err, integrity.ErrUnsupportedProof):
		return "unsupported_object_proof"
	case errors.Is(err, integrity.ErrProofInvalid):
		return "bad_object_proof"
	case errors.Is(err, integrity.ErrKeyResolution):
		return "object_key_unresolved"
	default:
		return "malformed_object_proof"
	}
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
