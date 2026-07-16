package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRequestHash(t *testing.T) {
	input := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(
		input,
		[]byte(`{"jsonrpc":"2.0","id":"request-1","method":"SendMessage"}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	var stdout bytes.Buffer
	if err := run([]string{"request-hash", "--input", input}, &stdout); err != nil {
		t.Fatalf("run request-hash: %v", err)
	}
	if !strings.HasPrefix(stdout.String(), "sha256:") || len(strings.TrimSpace(stdout.String())) != 71 {
		t.Fatalf("request-hash output = %q", stdout.String())
	}
}

func TestRunRequestHashRejectsUnsafeInteger(t *testing.T) {
	input := filepath.Join(t.TempDir(), "request.json")
	if err := os.WriteFile(
		input,
		[]byte(`{"jsonrpc":"2.0","id":9007199254740993}`),
		0o600,
	); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := run([]string{"request-hash", "--input", input}, &bytes.Buffer{}); err == nil {
		t.Fatal("run request-hash accepted an unsafe integer")
	}
}
