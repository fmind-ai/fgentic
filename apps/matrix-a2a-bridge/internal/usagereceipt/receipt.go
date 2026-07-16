// Package usagereceipt defines the content-free, seller-signed cross-organization usage receipt.
package usagereceipt

import (
	"bytes"
	"crypto/ecdsa"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gowebpki/jcs"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
)

// Schema is the immutable version discriminator inside each signed receipt.
const Schema = "fgentic.usage-receipt.v1"

const maxJCSSafeInteger = uint64(1<<53 - 1)

var (
	azpRE  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/-]{0,255}$`)
	hashRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// Receipt is the JCS-canonical assertion signed by the exporting organization. TokensConsumed is
// deliberately nullable until the gateway can attribute provider-reported usage per consumer.
type Receipt struct {
	Schema         string  `json:"schema"`
	AZP            string  `json:"azp"`
	TaskID         string  `json:"taskId"`
	ContextID      string  `json:"contextId"`
	RequestHash    string  `json:"requestHash"`
	TokensReserved uint64  `json:"tokensReserved"`
	TokensConsumed *uint64 `json:"tokensConsumed"`
	Timestamp      string  `json:"timestamp"`
	Outcome        string  `json:"outcome"`
	KeyID          string  `json:"keyId"`
}

// Signed is the receipt plus flattened protected JWS material. The receipt object, rather than its
// transport encoding, is canonicalized before signing and verification.
type Signed struct {
	Receipt   Receipt `json:"receipt"`
	Protected string  `json:"protected"`
	Signature string  `json:"signature"`
}

// New constructs and validates one receipt at a caller-supplied completion time.
func New(
	azp, taskID, contextID, requestHash string,
	tokensReserved uint64,
	completedAt time.Time,
	outcome, keyID string,
) (Receipt, error) {
	receipt := Receipt{
		Schema:         Schema,
		AZP:            azp,
		TaskID:         taskID,
		ContextID:      contextID,
		RequestHash:    requestHash,
		TokensReserved: tokensReserved,
		TokensConsumed: nil,
		Timestamp:      completedAt.UTC().Format(time.RFC3339Nano),
		Outcome:        outcome,
		KeyID:          keyID,
	}
	if err := receipt.Validate(); err != nil {
		return Receipt{}, err
	}
	return receipt, nil
}

// Validate enforces the bounded content-free receipt contract.
func (r Receipt) Validate() error {
	if r.Schema != Schema {
		return fmt.Errorf("receipt schema must be %q", Schema)
	}
	if !azpRE.MatchString(r.AZP) {
		return fmt.Errorf("receipt azp is invalid")
	}
	for name, value := range map[string]string{
		"taskId": r.TaskID, "contextId": r.ContextID, "outcome": r.Outcome,
	} {
		if !validIdentifier(value) {
			return fmt.Errorf("receipt %s is invalid", name)
		}
	}
	if !hashRE.MatchString(r.RequestHash) {
		return fmt.Errorf("receipt requestHash must be a sha256 identifier")
	}
	if !validTokenReservation(r.TokensReserved) {
		return fmt.Errorf(
			"receipt tokensReserved must be between 1 and %d",
			maxJCSSafeInteger,
		)
	}
	if r.TokensConsumed != nil {
		return fmt.Errorf("receipt tokensConsumed must remain null until per-consumer actuals exist")
	}
	timestamp, err := time.Parse(time.RFC3339Nano, r.Timestamp)
	if err != nil || timestamp.Location() != time.UTC {
		return fmt.Errorf("receipt timestamp must be RFC3339 UTC")
	}
	if r.KeyID == "" || len(r.KeyID) > 256 {
		return fmt.Errorf("receipt keyId is invalid")
	}
	return nil
}

func validTokenReservation(value uint64) bool {
	return value > 0 && value <= maxJCSSafeInteger
}

func validIdentifier(value string) bool {
	if value == "" || len(value) > 256 || !utf8.ValidString(value) || strings.TrimSpace(value) != value {
		return false
	}
	for _, char := range value {
		if unicode.IsControl(char) {
			return false
		}
	}
	return true
}

// Sign signs a validated receipt with ES256 over its RFC 8785 representation.
func Sign(receipt Receipt, key *ecdsa.PrivateKey) (Signed, error) {
	payload, err := canonicalReceipt(receipt)
	if err != nil {
		return Signed{}, err
	}
	signature, err := agentcardjws.SignCanonicalPayload(payload, key, receipt.KeyID)
	if err != nil {
		return Signed{}, fmt.Errorf("sign usage receipt: %w", err)
	}
	return Signed{
		Receipt: receipt, Protected: signature.Protected, Signature: signature.Signature,
	}, nil
}

// Verify validates the schema and pinned-key ES256 signature.
func Verify(signed Signed, key *ecdsa.PublicKey, expectedKeyID string) error {
	if signed.Receipt.KeyID != expectedKeyID {
		return fmt.Errorf("receipt keyId does not match pinned key ID")
	}
	payload, err := canonicalReceipt(signed.Receipt)
	if err != nil {
		return err
	}
	ok, err := agentcardjws.VerifyCanonicalPayload(payload, agentcardjws.Signature{
		Protected: signed.Protected,
		Signature: signed.Signature,
	}, key, expectedKeyID)
	if err != nil {
		return fmt.Errorf("verify usage receipt: %w", err)
	}
	if !ok {
		return fmt.Errorf("verify usage receipt: ES256 signature did not match")
	}
	return nil
}

// Parse strictly decodes a signed receipt and rejects extension fields outside the versioned schema.
func Parse(raw []byte) (Signed, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var signed Signed
	if err := decoder.Decode(&signed); err != nil {
		return Signed{}, fmt.Errorf("decode signed usage receipt: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Signed{}, fmt.Errorf("decode signed usage receipt: trailing data")
	}
	if err := signed.Receipt.Validate(); err != nil {
		return Signed{}, err
	}
	if signed.Protected == "" || signed.Signature == "" {
		return Signed{}, fmt.Errorf("signed usage receipt is missing JWS material")
	}
	return signed, nil
}

// Marshal emits one compact JSON object suitable for A2A metadata and JSONL archival.
func Marshal(signed Signed) ([]byte, error) {
	if err := signed.Receipt.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(signed)
	if err != nil {
		return nil, fmt.Errorf("encode signed usage receipt: %w", err)
	}
	return encoded, nil
}

func canonicalReceipt(receipt Receipt) ([]byte, error) {
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		return nil, fmt.Errorf("encode usage receipt payload: %w", err)
	}
	canonical, err := jcs.Transform(encoded)
	if err != nil {
		return nil, fmt.Errorf("canonicalize usage receipt payload: %w", err)
	}
	return canonical, nil
}
