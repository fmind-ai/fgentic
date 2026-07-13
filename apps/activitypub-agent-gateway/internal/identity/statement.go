package identity

import (
	"crypto/ecdsa"
	"errors"
	"fmt"
	"strings"
	"time"
)

// StatementType is the FEP-c390 identity-proof object type.
const StatementType = "VerifiableIdentityStatement"

// Data-integrity contexts the statement carries so `proof`, `subject`, and `alsoKnownAs` are defined
// terms for a verifier.
const (
	activityStreamsContext = "https://www.w3.org/ns/activitystreams"
	dataIntegrityContext   = "https://w3id.org/security/data-integrity/v1"
)

// ErrBindingMismatch means the actor's did:key does not equal the AgentCard's published key — the
// two federation faces are NOT the same principal, so trust must fail closed.
var ErrBindingMismatch = errors.New("identity: actor did:key does not match the AgentCard key")

// verificationMethod is the standard did:key verification method: <did>#<multibase-key>.
func verificationMethod(did string) string {
	return did + "#" + strings.TrimPrefix(did, "did:key:")
}

// BuildStatement mints a FEP-c390 VerifiableIdentityStatement asserting that did (controlled by priv)
// also-knows-as the actor URI, signed by the did's key. A remote verifier confirms the fediverse
// actor and the A2A endpoint share ONE sovereign key.
func BuildStatement(priv *ecdsa.PrivateKey, did, actorURI string, created time.Time) (map[string]any, error) {
	statement := map[string]any{
		"@context":    []any{activityStreamsContext, dataIntegrityContext},
		"type":        StatementType,
		"subject":     did,
		"alsoKnownAs": actorURI,
	}
	if err := signProof(statement, priv, verificationMethod(did), created); err != nil {
		return nil, err
	}
	return statement, nil
}

// VerifyStatement checks a VerifiableIdentityStatement: the embedded proof must validate under the
// did:key named as its subject, the proof's verificationMethod must reference that same did, and the
// statement must bind exactly the expected actor. It returns the verified did.
func VerifyStatement(statement map[string]any, expectedActor string) (did string, pub *ecdsa.PublicKey, err error) {
	if statement["type"] != StatementType {
		return "", nil, fmt.Errorf("%w: not a VerifiableIdentityStatement", ErrMalformedProof)
	}
	subject, _ := statement["subject"].(string)
	if subject == "" {
		return "", nil, fmt.Errorf("%w: missing subject did", ErrMalformedProof)
	}
	alsoKnownAs, _ := statement["alsoKnownAs"].(string)
	if alsoKnownAs != expectedActor {
		return "", nil, fmt.Errorf("%w: statement binds %q, not %q", ErrMalformedProof, alsoKnownAs, expectedActor)
	}
	proof, ok := statement["proof"].(map[string]any)
	if !ok {
		return "", nil, ErrNoProof
	}
	if vm, _ := proof["verificationMethod"].(string); !strings.HasPrefix(vm, subject+"#") {
		return "", nil, fmt.Errorf("%w: verificationMethod does not reference the subject did", ErrMalformedProof)
	}
	pub, err = DecodeP256DIDKey(subject)
	if err != nil {
		return "", nil, err
	}
	if err := verifyProof(statement, pub); err != nil {
		return "", nil, err
	}
	return subject, pub, nil
}

// VerifyBinding is the full bidirectional FEP-c390 check at trust-establishment: the actor's
// statement must verify AND its did:key must equal the AgentCard's published P-256 JWK. It fails
// closed on a missing statement, an invalid proof, or a key mismatch, returning the verified did.
func VerifyBinding(statement, cardJWK map[string]any, expectedActor string) (string, error) {
	did, actorPub, err := VerifyStatement(statement, expectedActor)
	if err != nil {
		return "", err
	}
	cardPub, err := JWKToPublicKey(cardJWK)
	if err != nil {
		return "", err
	}
	if !actorPub.Equal(cardPub) {
		return "", ErrBindingMismatch
	}
	return did, nil
}
