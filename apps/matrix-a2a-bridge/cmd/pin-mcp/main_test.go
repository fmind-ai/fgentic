package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fmind/matrix-a2a-bridge/internal/mcppin"
)

func TestUpdateCheckAndVerify(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(testTool("lookup", "reviewed description"), nil)
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return server },
		nil,
	))
	t.Cleanup(httpServer.Close)

	pinPath := filepath.Join(t.TempDir(), "nested", "surface.pin.json")
	updateArgs := []string{
		"update",
		"--name", "fixture",
		"--endpoint", httpServer.URL,
		"--output", pinPath,
		"--image", "registry.example/server@sha256:" + strings.Repeat("a", 64),
		"--command", "/server",
		"--argument=--read-only",
		"--discovery-url", "http://fixture.tools:8084/mcp",
		"--discovery-protocol", "STREAMABLE_HTTP",
		"--backend-host", "fixture.tools.svc.cluster.local",
		"--backend-port", "8084",
		"--backend-path", "/mcp",
		"--backend-protocol", "StreamableHTTP",
	}
	if err := run(updateArgs); err != nil {
		t.Fatalf("run(update) error = %v", err)
	}
	if err := run([]string{"check", "--pin", pinPath}); err != nil {
		t.Fatalf("run(check) error = %v", err)
	}

	content, err := os.ReadFile(pinPath)
	if err != nil {
		t.Fatalf("read generated pin: %v", err)
	}
	if bytes.Contains(content, []byte(httpServer.URL)) {
		t.Fatalf("generated pin leaked collection endpoint %q", httpServer.URL)
	}
	file, err := mcppin.Parse(content)
	if err != nil {
		t.Fatalf("parse generated pin: %v", err)
	}
	if len(file.Servers) != 1 || len(file.Servers[0].Tools.Entries) != 1 {
		t.Fatalf("generated pin surface = %+v, want one server with one tool", file.Servers)
	}

	verifyArgs := []string{
		"verify", "--name", "fixture", "--endpoint", httpServer.URL, "--pin", pinPath,
	}
	if err := run(verifyArgs); err != nil {
		t.Fatalf("run(verify) error = %v", err)
	}

	server.AddTool(testTool("lookup", "reviewed descriptioN"), nil)
	var stderr bytes.Buffer
	if exitCode := execute(verifyArgs, &stderr); exitCode != 1 {
		t.Fatalf("execute(verify drift) exit code = %d, want 1", exitCode)
	}
	for _, want := range []string{
		"MCP surface drift",
		`changed $.servers["fixture"].tools["lookup"].description`,
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("execute(verify drift) stderr = %q, want %q", stderr.String(), want)
		}
	}
}

func TestExecuteRejectsUnknownCommand(t *testing.T) {
	t.Parallel()

	var stderr bytes.Buffer
	if exitCode := execute([]string{"unknown"}, &stderr); exitCode != 1 {
		t.Fatalf("execute(unknown) exit code = %d, want 1", exitCode)
	}
	if !strings.Contains(stderr.String(), "expected check, update, or verify") {
		t.Fatalf("execute(unknown) stderr = %q", stderr.String())
	}
}

func testTool(name, description string) *mcp.Tool {
	return &mcp.Tool{
		Name:        name,
		Description: description,
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string", "description": "lookup query"},
			},
		},
	}
}
