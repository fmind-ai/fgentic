package mcppin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"net/url"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	clientTimeout        = 30 * time.Second
	maxCollectionEntries = 4096
	maxCollectionBytes   = 16 << 20
	maxItemBytes         = 1 << 20
	maxHTTPResponseBytes = maxCollectionBytes + maxItemBytes
)

var errHTTPResponseTooLarge = errors.New("MCP HTTP response exceeds configured limit")

// Collect fetches the complete typed surface advertised by one Streamable HTTP MCP server.
func Collect(ctx context.Context, endpoint string) (Surface, error) {
	parsed, err := url.ParseRequestURI(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return Surface{}, fmt.Errorf("invalid MCP endpoint %q", endpoint)
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return Surface{}, fmt.Errorf("MCP endpoint must not contain credentials or a fragment")
	}
	ctx, cancel := context.WithTimeout(ctx, clientTimeout)
	defer cancel()

	transport := &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			Timeout: clientTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
			Transport: responseLimitTransport{
				base:     http.DefaultTransport,
				maxBytes: maxHTTPResponseBytes,
			},
		},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	return collectTransport(ctx, transport)
}

type responseLimitTransport struct {
	base     http.RoundTripper
	maxBytes int64
}

func (transport responseLimitTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := transport.base.RoundTrip(request)
	if err != nil {
		return nil, err
	}
	response.Body = &maxBytesReadCloser{
		body:      response.Body,
		remaining: transport.maxBytes,
	}
	return response, nil
}

type maxBytesReadCloser struct {
	body      io.ReadCloser
	remaining int64
}

func (reader *maxBytesReadCloser) Read(buffer []byte) (int, error) {
	if len(buffer) == 0 {
		return reader.body.Read(buffer)
	}
	if reader.remaining < 0 {
		return 0, errHTTPResponseTooLarge
	}
	if int64(len(buffer)) > reader.remaining+1 {
		buffer = buffer[:reader.remaining+1]
	}
	read, err := reader.body.Read(buffer)
	if int64(read) <= reader.remaining {
		reader.remaining -= int64(read)
		return read, err
	}
	read = int(reader.remaining)
	reader.remaining = -1
	return read, errHTTPResponseTooLarge
}

func (reader *maxBytesReadCloser) Close() error {
	return reader.body.Close()
}

func collectTransport(ctx context.Context, transport mcp.Transport) (_ Surface, returnErr error) {
	client := mcp.NewClient(
		&mcp.Implementation{Name: "fgentic-mcp-pin", Version: "v1"},
		&mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}},
	)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		return Surface{}, fmt.Errorf("initialize MCP server: %w", err)
	}
	defer func() {
		if closeErr := session.Close(); closeErr != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("close MCP session: %w", closeErr))
		}
	}()

	initial := session.InitializeResult()
	if initial == nil || initial.Capabilities == nil {
		return Surface{}, fmt.Errorf("MCP initialize response has no capabilities")
	}
	initialize, err := json.Marshal(initial)
	if err != nil {
		return Surface{}, fmt.Errorf("encode MCP initialize response: %w", err)
	}

	capabilities := initial.Capabilities
	surface := Surface{
		Initialize:         initialize,
		ToolsSupported:     capabilities.Tools != nil,
		PromptsSupported:   capabilities.Prompts != nil,
		ResourcesSupported: capabilities.Resources != nil,
	}
	if surface.ToolsSupported {
		surface.Tools, err = collectJSON(session.Tools(ctx, nil))
		if err != nil {
			return Surface{}, fmt.Errorf("list MCP tools: %w", err)
		}
	}
	if surface.PromptsSupported {
		surface.Prompts, err = collectJSON(session.Prompts(ctx, nil))
		if err != nil {
			return Surface{}, fmt.Errorf("list MCP prompts: %w", err)
		}
	}
	if surface.ResourcesSupported {
		surface.Resources, err = collectJSON(session.Resources(ctx, nil))
		if err != nil {
			return Surface{}, fmt.Errorf("list MCP resources: %w", err)
		}
		surface.ResourceTemplates, err = collectJSON(session.ResourceTemplates(ctx, nil))
		switch {
		case err == nil:
			surface.ResourceTemplatesSupported = true
		case isMethodNotFound(err):
		default:
			return Surface{}, fmt.Errorf("list MCP resource templates: %w", err)
		}
	}
	return surface, nil
}

func collectJSON[T any](sequence iter.Seq2[*T, error]) ([]json.RawMessage, error) {
	items := make([]json.RawMessage, 0)
	totalBytes := 0
	for item, err := range sequence {
		if err != nil {
			return nil, err
		}
		if item == nil {
			return nil, fmt.Errorf("MCP list contained a null item")
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			return nil, fmt.Errorf("encode MCP list item: %w", err)
		}
		if len(encoded) > maxItemBytes {
			return nil, fmt.Errorf("MCP list item exceeds %d bytes", maxItemBytes)
		}
		if len(items) >= maxCollectionEntries {
			return nil, fmt.Errorf("MCP collection exceeds %d entries", maxCollectionEntries)
		}
		totalBytes += len(encoded)
		if totalBytes > maxCollectionBytes {
			return nil, fmt.Errorf("MCP collection exceeds %d bytes", maxCollectionBytes)
		}
		items = append(items, encoded)
	}
	return items, nil
}

func isMethodNotFound(err error) bool {
	var rpcErr *jsonrpc.Error
	return errors.As(err, &rpcErr) && rpcErr.Code == jsonrpc.CodeMethodNotFound
}
