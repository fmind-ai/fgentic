package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/a2aproject/a2a-go/v2/a2a"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/agentcardjws"
)

const secretPromptSentinel = "SECRET-PROMPT-SENTINEL"

func TestSignAndSilentVerifyCommands(t *testing.T) {
	directory := t.TempDir()
	cardPath := filepath.Join(directory, "unsigned.json")
	outputPath := filepath.Join(directory, "bundle.json")
	if err := os.WriteFile(cardPath, commandTestCard(t), 0o600); err != nil {
		t.Fatalf("WriteFile card: %v", err)
	}
	privateKeyPEM := commandTestPrivateKeyPEM(t)
	if err := run([]string{
		"sign",
		"--input", cardPath,
		"--private-key", "-",
		"--key-id", "federation-card-key",
		"--output", outputPath,
	}, bytes.NewReader(privateKeyPEM)); err != nil {
		t.Fatalf("run sign: %v", err)
	}
	info, err := os.Stat(outputPath)
	if err != nil {
		t.Fatalf("Stat bundle: %v", err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("bundle mode = %o, want 644", info.Mode().Perm())
	}
	encoded, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("ReadFile bundle: %v", err)
	}
	var bundle agentcardjws.Bundle
	if err := json.Unmarshal(encoded, &bundle); err != nil {
		t.Fatalf("Unmarshal bundle: %v", err)
	}
	signedPath := filepath.Join(directory, "signed.json")
	publicKeyPath := filepath.Join(directory, "public-jwk.json")
	if err := os.WriteFile(signedPath, bundle.AgentCard, 0o600); err != nil {
		t.Fatalf("WriteFile signed card: %v", err)
	}
	if err := os.WriteFile(publicKeyPath, bundle.PublicJWK, 0o600); err != nil {
		t.Fatalf("WriteFile public JWK: %v", err)
	}
	verifyArgs := []string{
		"verify",
		"--input", signedPath,
		"--public-key", publicKeyPath,
		"--key-id", "federation-card-key",
	}
	var stderr bytes.Buffer
	if exitCode := execute(verifyArgs, strings.NewReader("unused"), &stderr); exitCode != 0 {
		t.Fatalf("execute verify exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("successful verify emitted stderr: %q", stderr.String())
	}

	var tampered map[string]any
	if err := json.Unmarshal(bundle.AgentCard, &tampered); err != nil {
		t.Fatalf("Unmarshal signed card: %v", err)
	}
	tampered["name"] = "tampered"
	tamperedJSON, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("Marshal tampered card: %v", err)
	}
	if err := os.WriteFile(signedPath, tamperedJSON, 0o600); err != nil {
		t.Fatalf("WriteFile tampered card: %v", err)
	}
	stderr.Reset()
	if exitCode := execute(verifyArgs, strings.NewReader("unused"), &stderr); exitCode == 0 {
		t.Fatal("execute verify accepted a tampered card")
	}
	if strings.Contains(stderr.String(), secretPromptSentinel) {
		t.Fatalf("verify error exposed card content: %s", stderr.String())
	}
}

// signCardForTest signs the shared fixture card under keyID and writes the signed card and its public
// JWK to the temp directory, returning their paths — the tool's own sign path builds the bundle exactly
// as #920's tests do, so overlap/revocation verification is exercised against real ES256 signatures.
func signCardForTest(t *testing.T, directory, keyID string) (signedPath, publicKeyPath string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	bundle, err := agentcardjws.Sign(commandTestCard(t), key, keyID)
	if err != nil {
		t.Fatalf("Sign %q: %v", keyID, err)
	}
	signedPath = filepath.Join(directory, keyID+".signed.json")
	publicKeyPath = filepath.Join(directory, keyID+".public-jwk.json")
	if err := os.WriteFile(signedPath, bundle.AgentCard, 0o600); err != nil {
		t.Fatalf("WriteFile signed card: %v", err)
	}
	if err := os.WriteFile(publicKeyPath, bundle.PublicJWK, 0o600); err != nil {
		t.Fatalf("WriteFile public JWK: %v", err)
	}
	return signedPath, publicKeyPath
}

func TestVerifyBackwardCompatibleSingleKey(t *testing.T) {
	directory := t.TempDir()
	signedPath, publicKeyPath := signCardForTest(t, directory, "old-key")
	var stderr bytes.Buffer
	exitCode := execute([]string{
		"verify",
		"--input", signedPath,
		"--public-key", publicKeyPath,
		"--key-id", "old-key",
	}, strings.NewReader("unused"), &stderr)
	if exitCode != 0 {
		t.Fatalf("single-key verify exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("successful verify emitted stderr: %q", stderr.String())
	}
}

func TestVerifyOverlapAcceptsEitherPinnedKey(t *testing.T) {
	directory := t.TempDir()
	oldSignedPath, oldPublicKeyPath := signCardForTest(t, directory, "old-key")
	newSignedPath, newPublicKeyPath := signCardForTest(t, directory, "new-key")
	// During the overlap window both the retiring key and its replacement are pinned; a card signed under
	// EITHER kid must verify. The card under the non-primary key exercises the additional-key path.
	for _, testCase := range []struct {
		name       string
		signedPath string
	}{
		{name: "card under primary key", signedPath: oldSignedPath},
		{name: "card under additional key", signedPath: newSignedPath},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			err := run([]string{
				"verify",
				"--input", testCase.signedPath,
				"--public-key", oldPublicKeyPath,
				"--key-id", "old-key",
				"--additional-key", "new-key=" + newPublicKeyPath,
			}, strings.NewReader("unused"))
			if err != nil {
				t.Fatalf("overlap verify: %v", err)
			}
		})
	}
}

func TestVerifyRevocationFailsClosedButNewKeySucceeds(t *testing.T) {
	directory := t.TempDir()
	oldSignedPath, _ := signCardForTest(t, directory, "old-key")
	newSignedPath, newPublicKeyPath := signCardForTest(t, directory, "new-key")
	// After promotion, the retired kid is dropped from the pin set and revoked; a card offered only under
	// it is refused with the distinct revoked reason, while a card under the promoted key still verifies.
	err := run([]string{
		"verify",
		"--input", oldSignedPath,
		"--public-key", newPublicKeyPath,
		"--key-id", "new-key",
		"--revoked-key-id", "old-key",
	}, strings.NewReader("unused"))
	if err == nil {
		t.Fatal("verify accepted a card signed only under a revoked key ID")
	}
	if !errors.Is(err, agentcardjws.ErrRevokedKeyID) {
		t.Fatalf("revoked card error = %v, want ErrRevokedKeyID", err)
	}
	if err := run([]string{
		"verify",
		"--input", newSignedPath,
		"--public-key", newPublicKeyPath,
		"--key-id", "new-key",
		"--revoked-key-id", "old-key",
	}, strings.NewReader("unused")); err != nil {
		t.Fatalf("promoted-key verify: %v", err)
	}
}

func TestVerifyRejectsPinnedAndRevokedKeyID(t *testing.T) {
	directory := t.TempDir()
	signedPath, publicKeyPath := signCardForTest(t, directory, "old-key")
	// A kid can never be both pinned and revoked: VerifySet fails closed on the contradiction.
	err := run([]string{
		"verify",
		"--input", signedPath,
		"--public-key", publicKeyPath,
		"--key-id", "old-key",
		"--revoked-key-id", "old-key",
	}, strings.NewReader("unused"))
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("pinned-and-revoked verify error = %v", err)
	}
}

func TestVerifyUnrevokedTamperedCardFails(t *testing.T) {
	directory := t.TempDir()
	signedPath, publicKeyPath := signCardForTest(t, directory, "new-key")
	signed, err := os.ReadFile(signedPath)
	if err != nil {
		t.Fatalf("ReadFile signed card: %v", err)
	}
	var tampered map[string]any
	if err := json.Unmarshal(signed, &tampered); err != nil {
		t.Fatalf("Unmarshal signed card: %v", err)
	}
	tampered["name"] = "tampered"
	tamperedJSON, err := json.Marshal(tampered)
	if err != nil {
		t.Fatalf("Marshal tampered card: %v", err)
	}
	if err := os.WriteFile(signedPath, tamperedJSON, 0o600); err != nil {
		t.Fatalf("WriteFile tampered card: %v", err)
	}
	// A tampered card under a pinned, non-revoked kid must still fail — overlap must not widen acceptance.
	err = run([]string{
		"verify",
		"--input", signedPath,
		"--public-key", publicKeyPath,
		"--key-id", "new-key",
	}, strings.NewReader("unused"))
	if err == nil {
		t.Fatal("verify accepted a tampered card")
	}
	if errors.Is(err, agentcardjws.ErrRevokedKeyID) {
		t.Fatalf("tampered card misreported as revoked: %v", err)
	}
}

func TestSignFailurePreservesExistingOutput(t *testing.T) {
	directory := t.TempDir()
	cardPath := filepath.Join(directory, "invalid.json")
	keyPath := filepath.Join(directory, "private.pem")
	outputPath := filepath.Join(directory, "bundle.json")
	if err := os.WriteFile(cardPath, []byte(`{"invalid":true}`), 0o600); err != nil {
		t.Fatalf("WriteFile card: %v", err)
	}
	if err := os.WriteFile(keyPath, commandTestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	original := []byte("existing-public-artifact\n")
	if err := os.WriteFile(outputPath, original, 0o644); err != nil {
		t.Fatalf("WriteFile output: %v", err)
	}
	err := run([]string{
		"sign",
		"--input", cardPath,
		"--private-key", keyPath,
		"--key-id", "card-key",
		"--output", outputPath,
	}, strings.NewReader("unused"))
	if err == nil {
		t.Fatal("sign accepted an invalid card")
	}
	got, readErr := os.ReadFile(outputPath)
	if readErr != nil {
		t.Fatalf("ReadFile output: %v", readErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("failed sign changed existing output to %q", got)
	}
	entries, readErr := os.ReadDir(directory)
	if readErr != nil {
		t.Fatalf("ReadDir: %v", readErr)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".sign-agent-card-") {
			t.Fatalf("temporary output remained after failure: %s", entry.Name())
		}
	}
}

func TestSignRejectsOutputAliasingInputs(t *testing.T) {
	directory := t.TempDir()
	cardPath := filepath.Join(directory, "card.json")
	keyPath := filepath.Join(directory, "private.pem")
	if err := os.WriteFile(cardPath, commandTestCard(t), 0o600); err != nil {
		t.Fatalf("WriteFile card: %v", err)
	}
	if err := os.WriteFile(keyPath, commandTestPrivateKeyPEM(t), 0o600); err != nil {
		t.Fatalf("WriteFile key: %v", err)
	}
	for _, outputPath := range []string{cardPath, keyPath} {
		err := run([]string{
			"sign",
			"--input", cardPath,
			"--private-key", keyPath,
			"--key-id", "card-key",
			"--output", outputPath,
		}, strings.NewReader("unused"))
		if err == nil || !strings.Contains(err.Error(), "must not replace") {
			t.Fatalf("alias output %q error = %v", outputPath, err)
		}
	}
}

func TestReadBoundedRejectsOversizedInput(t *testing.T) {
	if _, err := readBounded(strings.NewReader("12345"), 4, "fixture"); err == nil || !strings.Contains(err.Error(), "exceeds 4") {
		t.Fatalf("readBounded error = %v", err)
	}
}

func commandTestCard(t *testing.T) []byte {
	t.Helper()
	card := &a2a.AgentCard{
		Name:        "Federated docs fixture",
		Description: secretPromptSentinel,
		Provider: &a2a.AgentProvider{
			Org: "Fgentic org A",
			URL: "https://org-a.fgentic.localhost",
		},
		Version: "1.0.0",
		SupportedInterfaces: []*a2a.AgentInterface{
			a2a.NewAgentInterface("https://a2a.org-a.fgentic.localhost/docs", a2a.TransportProtocolJSONRPC),
		},
		Capabilities:       a2a.AgentCapabilities{},
		DefaultInputModes:  []string{"text/plain"},
		DefaultOutputModes: []string{"text/plain"},
		Skills: []a2a.AgentSkill{{
			ID:          "docs",
			Name:        "Docs",
			Description: "Answers documentation questions",
			Tags:        []string{"docs"},
		}},
	}
	raw, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("Marshal AgentCard: %v", err)
	}
	return raw
}

func commandTestPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey: %v", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
}
