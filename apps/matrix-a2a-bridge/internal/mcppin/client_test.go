package mcppin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"iter"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCollectTransportFollowsPagesAndCapabilities(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(
		&mcp.Implementation{Name: "fixture", Version: "1"},
		&mcp.ServerOptions{PageSize: 1, Instructions: "Use reviewed tools only."},
	)
	for _, name := range []string{"second", "first"} {
		server.AddTool(&mcp.Tool{
			Name:        name,
			Description: name + " tool",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value": map[string]any{"type": "string", "description": name + " value"},
				},
			},
			Meta: mcp.Meta{"review": name},
		}, nil)
		server.AddPrompt(&mcp.Prompt{Name: name, Description: name + " prompt"}, nil)
		server.AddResource(&mcp.Resource{Name: name, URI: "test://" + name}, nil)
		server.AddResourceTemplate(&mcp.ResourceTemplate{
			Name:        name,
			URITemplate: "test://" + name + "/{id}",
		}, nil)
	}

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect fixture server: %v", err)
	}
	t.Cleanup(func() {
		if err := serverSession.Close(); err != nil {
			t.Errorf("close fixture server: %v", err)
		}
	})

	surface, err := collectTransport(context.Background(), clientTransport)
	if err != nil {
		t.Fatalf("collectTransport() error = %v", err)
	}
	if !surface.ToolsSupported || !surface.PromptsSupported ||
		!surface.ResourcesSupported || !surface.ResourceTemplatesSupported {
		t.Fatalf("collection support = %+v, want every fixture collection supported", surface)
	}
	var initialize map[string]any
	if err := json.Unmarshal(surface.Initialize, &initialize); err != nil {
		t.Fatalf("decode collected initialize response: %v", err)
	}
	if got := initialize["instructions"]; got != "Use reviewed tools only." {
		t.Fatalf("initialize instructions = %v, want reviewed instructions", got)
	}
	if _, ok := initialize["serverInfo"].(map[string]any); !ok {
		t.Fatalf("initialize response omitted server identity: %s", surface.Initialize)
	}
	for kind, items := range map[string][]json.RawMessage{
		"tools": surface.Tools, "prompts": surface.Prompts,
		"resources": surface.Resources, "resource templates": surface.ResourceTemplates,
	} {
		if len(items) != 2 {
			t.Errorf("%s item count = %d, want 2", kind, len(items))
		}
	}

	var tool map[string]any
	if err := json.Unmarshal(surface.Tools[0], &tool); err != nil {
		t.Fatalf("decode collected tool: %v", err)
	}
	if _, ok := tool["inputSchema"].(map[string]any); !ok {
		t.Fatalf("collected tool omitted its complete input schema: %s", surface.Tools[0])
	}
	if _, ok := tool["_meta"].(map[string]any); !ok {
		t.Fatalf("collected tool omitted its metadata: %s", surface.Tools[0])
	}
}

func TestCollectTransportRecordsUnsupportedCollections(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "empty", Version: "1"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect empty server: %v", err)
	}
	t.Cleanup(func() {
		if err := serverSession.Close(); err != nil {
			t.Errorf("close empty server: %v", err)
		}
	})

	surface, err := collectTransport(context.Background(), clientTransport)
	if err != nil {
		t.Fatalf("collectTransport() error = %v", err)
	}
	if surface.ToolsSupported || surface.PromptsSupported ||
		surface.ResourcesSupported || surface.ResourceTemplatesSupported {
		t.Fatalf("empty server unexpectedly advertised a collection: %+v", surface)
	}
}

func TestCollectTransportAllowsUnsupportedResourceTemplates(t *testing.T) {
	t.Parallel()

	server := mcp.NewServer(&mcp.Implementation{Name: "resources-only", Version: "1"}, nil)
	server.AddResource(&mcp.Resource{Name: "guide", URI: "test://guide"}, nil)
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, request mcp.Request) (mcp.Result, error) {
			if method == "resources/templates/list" {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "not supported"}
			}
			return next(ctx, method, request)
		}
	})

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := server.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect resources-only server: %v", err)
	}
	t.Cleanup(func() {
		if err := serverSession.Close(); err != nil {
			t.Errorf("close resources-only server: %v", err)
		}
	})

	surface, err := collectTransport(context.Background(), clientTransport)
	if err != nil {
		t.Fatalf("collectTransport() error = %v", err)
	}
	if !surface.ResourcesSupported || surface.ResourceTemplatesSupported {
		t.Fatalf("resource support = %+v, want resources supported and templates unsupported", surface)
	}
	if len(surface.Resources) != 1 || len(surface.ResourceTemplates) != 0 {
		t.Fatalf("resource collections = %d, %d; want 1, 0", len(surface.Resources), len(surface.ResourceTemplates))
	}
}

func TestCollectRejectsInvalidEndpoint(t *testing.T) {
	t.Parallel()

	for _, endpoint := range []string{"", "localhost:8080/mcp", "file:///tmp/mcp", "https://user@example.test/mcp"} {
		if _, err := Collect(context.Background(), endpoint); err == nil {
			t.Errorf("Collect(%q) unexpectedly succeeded", endpoint)
		}
	}
}

func TestCollectRejectsRedirectedEndpoint(t *testing.T) {
	t.Parallel()

	var targetHit atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		targetHit.Store(true)
	}))
	t.Cleanup(target.Close)
	redirect := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(redirect.Close)

	if _, err := Collect(context.Background(), redirect.URL); err == nil {
		t.Fatal("Collect(redirect) unexpectedly succeeded")
	}
	if targetHit.Load() {
		t.Fatal("Collect followed an unpinned endpoint redirect")
	}
}

func TestCollectJSONBoundsUntrustedMetadata(t *testing.T) {
	t.Parallel()

	sequence := func(yield func(*mcp.Tool, error) bool) {
		yield(&mcp.Tool{
			Name:        "oversized",
			Description: strings.Repeat("x", maxItemBytes),
			InputSchema: map[string]any{"type": "object"},
		}, nil)
	}
	if _, err := collectJSON(iter.Seq2[*mcp.Tool, error](sequence)); err == nil ||
		!strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("collectJSON() error = %v, want size bound", err)
	}
}

func TestMaxBytesReadCloserBoundsBeforeProtocolDecode(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		content string
		limit   int64
		want    string
		wantErr error
	}{
		{name: "exact", content: "1234", limit: 4, want: "1234"},
		{name: "exceeded", content: "12345", limit: 4, want: "1234", wantErr: errHTTPResponseTooLarge},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			reader := &maxBytesReadCloser{
				body:      io.NopCloser(strings.NewReader(test.content)),
				remaining: test.limit,
			}
			content, err := io.ReadAll(reader)
			if string(content) != test.want || !errors.Is(err, test.wantErr) {
				t.Fatalf("ReadAll() = %q, %v; want %q, %v", content, err, test.want, test.wantErr)
			}
		})
	}
}
