package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTestClientCert generates a self-signed client certificate and writes its cert and key PEM to
// temp files, returning their paths — enough material to exercise the mTLS config loader (#244).
func writeTestClientCert(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fgentic-bridge"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "client.crt")
	keyFile = filepath.Join(dir, "client.key")
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	return certFile, keyFile
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func remoteYAMLWithMTLS(certFile, keyFile string) string {
	block := fmt.Sprintf("    mtls:\n      clientCertFile: %s\n      clientKeyFile: %s\n", certFile, keyFile)
	return strings.Replace(validRemoteAgentsYAML, "    tokenBudget: 8192\n", "    tokenBudget: 8192\n"+block, 1)
}

func TestLoadAgentsRemoteMTLS(t *testing.T) {
	certFile, keyFile := writeTestClientCert(t)
	agents, err := LoadAgents(writeTemp(t, remoteYAMLWithMTLS(certFile, keyFile)))
	if err != nil {
		t.Fatalf("LoadAgents: %v", err)
	}
	ref, ok := agents.Lookup("agent-remote")
	if !ok {
		t.Fatal("agent-remote not found")
	}
	if !ref.Target().IsRemote() {
		t.Fatal("mtls target should be remote")
	}
	// The mTLS material folds into the target ID, so the mapping re-keys versus a plain remote and a
	// queued delegation re-verifies the card under the new transport auth.
	base, _ := LoadAgents(writeTemp(t, validRemoteAgentsYAML))
	baseRef, _ := base.Lookup("agent-remote")
	if ref.MappingID() == baseRef.MappingID() {
		t.Fatal("mtls did not change the mapping ID")
	}
}

func TestLoadAgentsRejectsLocalMTLS(t *testing.T) {
	local := "agents:\n  agent-k8s:\n    namespace: kagent\n    name: k8s-agent\n    mtls:\n      clientCertFile: /x\n      clientKeyFile: /y\n"
	if _, err := LoadAgents(writeTemp(t, local)); err == nil || !strings.Contains(err.Error(), "only valid for a url target") {
		t.Fatalf("LoadAgents local+mtls err = %v", err)
	}
}

func TestLoadAgentsRejectsIncompleteMTLS(t *testing.T) {
	certFile, _ := writeTestClientCert(t)
	// clientKeyFile omitted.
	incomplete := strings.Replace(validRemoteAgentsYAML, "    tokenBudget: 8192\n",
		"    tokenBudget: 8192\n    mtls:\n      clientCertFile: "+certFile+"\n", 1)
	if _, err := LoadAgents(writeTemp(t, incomplete)); err == nil || !strings.Contains(err.Error(), "clientCertFile and clientKeyFile") {
		t.Fatalf("LoadAgents incomplete mtls err = %v", err)
	}
}

func TestLoadAgentsRejectsMissingMTLSFile(t *testing.T) {
	missing := strings.Replace(validRemoteAgentsYAML, "    tokenBudget: 8192\n",
		"    tokenBudget: 8192\n    mtls:\n      clientCertFile: /nonexistent/client.crt\n      clientKeyFile: /nonexistent/client.key\n", 1)
	if _, err := LoadAgents(writeTemp(t, missing)); err == nil || !strings.Contains(err.Error(), "read mtls clientCertFile") {
		t.Fatalf("LoadAgents missing mtls file err = %v", err)
	}
}
