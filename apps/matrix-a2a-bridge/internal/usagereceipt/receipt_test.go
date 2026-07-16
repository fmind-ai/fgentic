package usagereceipt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestSignVerifyAndTamper(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	receipt, err := New(
		"org-b-a2a", "task-1", "context-1",
		"sha256:"+strings.Repeat("a", 64), 3000,
		time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC),
		"TASK_STATE_COMPLETED", "fgentic-org-a-receipts-v1",
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	signed, err := Sign(receipt, key)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if err := Verify(signed, &key.PublicKey, receipt.KeyID); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	encoded, err := Marshal(signed)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if !strings.Contains(string(encoded), `"tokensConsumed":null`) {
		t.Fatalf("signed receipt omitted honest null consumption: %s", encoded)
	}
	parsed, err := Parse(encoded)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := Verify(parsed, &key.PublicKey, receipt.KeyID); err != nil {
		t.Fatalf("Verify parsed: %v", err)
	}

	tampered := parsed
	tampered.Receipt.TokensReserved++
	if err := Verify(tampered, &key.PublicKey, receipt.KeyID); err == nil {
		t.Fatal("Verify accepted tampered reservation")
	}
}

func TestReceiptRejectsInventedConsumptionAndUnknownFields(t *testing.T) {
	consumed := uint64(1)
	receipt := Receipt{
		Schema: Schema, AZP: "org-b-a2a", TaskID: "task-1", ContextID: "context-1",
		RequestHash: "sha256:" + strings.Repeat("b", 64), TokensReserved: 1000,
		TokensConsumed: &consumed, Timestamp: "2026-07-16T08:30:00Z",
		Outcome: "TASK_STATE_COMPLETED", KeyID: "receipt-key",
	}
	if err := receipt.Validate(); err == nil {
		t.Fatal("Validate accepted invented token consumption")
	}

	raw := map[string]any{
		"receipt": map[string]any{
			"schema": Schema, "azp": "org-b-a2a", "taskId": "task-1", "contextId": "context-1",
			"requestHash": "sha256:" + strings.Repeat("b", 64), "tokensReserved": 1000,
			"tokensConsumed": nil, "timestamp": "2026-07-16T08:30:00Z",
			"outcome": "TASK_STATE_COMPLETED", "keyId": "receipt-key", "prompt": "must not exist",
		},
		"protected": "x", "signature": "y",
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("Marshal fixture: %v", err)
	}
	if _, err := Parse(encoded); err == nil {
		t.Fatal("Parse accepted an extension field that could carry content")
	}
}

func TestReceiptRejectsJCSUnsafeReservation(t *testing.T) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	receipt, err := New(
		"org-b-a2a", "task-1", "context-1",
		"sha256:"+strings.Repeat("c", 64), maxJCSSafeInteger,
		time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC),
		"TASK_STATE_COMPLETED", "receipt-key",
	)
	if err != nil {
		t.Fatalf("New max-safe receipt: %v", err)
	}
	signed, err := Sign(receipt, key)
	if err != nil {
		t.Fatalf("Sign max-safe receipt: %v", err)
	}
	tampered := signed
	tampered.Receipt.TokensReserved--
	if err := Verify(tampered, &key.PublicKey, receipt.KeyID); err == nil {
		t.Fatal("Verify accepted max-safe reservation tamper")
	}
	if _, err := New(
		"org-b-a2a", "task-1", "context-1",
		"sha256:"+strings.Repeat("c", 64), maxJCSSafeInteger+1,
		time.Date(2026, 7, 16, 8, 30, 0, 0, time.UTC),
		"TASK_STATE_COMPLETED", "receipt-key",
	); err == nil {
		t.Fatal("New accepted a reservation outside the JCS safe-integer range")
	}
}
