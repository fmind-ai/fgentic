// Package sessioncontrol owns the narrow kagent controller boundary used for conversation forget.
package sessioncontrol

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

const maxErrorBody = 4096

// ErrOwnersUnknown prevents reset when the complete kagent owner set is unavailable.
var ErrOwnersUnknown = errors.New("conversation session owners are unknown")

// Client deletes kagent sessions through its pinned internal REST contract.
type Client struct {
	base *url.URL
	http *http.Client
}

// New validates an origin-only controller URL and returns a session control client.
func New(rawBase string, httpClient *http.Client) (*Client, error) {
	base, err := url.Parse(rawBase)
	if err != nil {
		return nil, fmt.Errorf("parse kagent API URL: %w", err)
	}
	if (base.Scheme != "http" && base.Scheme != "https") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return nil, fmt.Errorf("kagent API URL must be an http(s) origin without credentials, path, query, or fragment")
	}
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: base, http: httpClient}, nil
}

// Purge deletes every observed kagent owner row for contextID and verifies each is no longer
// readable. The pinned kagent API scopes sessions by both context ID and user ID.
func (c *Client) Purge(ctx context.Context, contextID string, owners []string) error {
	if contextID == "" {
		return fmt.Errorf("context ID must not be empty")
	}
	if len(owners) == 0 {
		return ErrOwnersUnknown
	}
	for _, owner := range owners {
		if owner == "" {
			return fmt.Errorf("session owner must not be empty")
		}
		if err := c.delete(ctx, contextID, owner); err != nil {
			return err
		}
		forgotten, err := c.isForgotten(ctx, contextID, owner)
		if err != nil {
			return err
		}
		if !forgotten {
			return fmt.Errorf("verify deleted kagent session %q for owner %q: session remains readable", contextID, owner)
		}
	}
	return nil
}

func (c *Client) delete(ctx context.Context, contextID, owner string) (returnedErr error) {
	request, err := c.request(ctx, http.MethodDelete, contextID, owner)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("delete kagent session %q: %w", contextID, err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && returnedErr == nil {
			returnedErr = fmt.Errorf("close kagent delete response: %w", closeErr)
		}
	}()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusNoContent {
		return responseError("delete", contextID, response)
	}
	return nil
}

func (c *Client) isForgotten(ctx context.Context, contextID, owner string) (forgotten bool, returnedErr error) {
	request, err := c.request(ctx, http.MethodGet, contextID, owner)
	if err != nil {
		return false, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return false, fmt.Errorf("verify kagent session %q deletion: %w", contextID, err)
	}
	defer func() {
		if closeErr := response.Body.Close(); closeErr != nil && returnedErr == nil {
			forgotten = false
			returnedErr = fmt.Errorf("close kagent verification response: %w", closeErr)
		}
	}()
	if response.StatusCode == http.StatusNotFound {
		return true, nil
	}
	if response.StatusCode != http.StatusOK {
		return false, responseError("verify", contextID, response)
	}
	return false, nil
}

func (c *Client) request(ctx context.Context, method, contextID, owner string) (*http.Request, error) {
	endpoint := *c.base
	// URL.Path is decoded form while RawPath preserves contextID as exactly one path segment.
	// Setting only Path would turn an embedded slash into another route segment; setting it to the
	// already escaped value would double-escape the percent sign when URL.String serializes it.
	endpoint.Path = "/api/sessions/" + contextID
	endpoint.RawPath = "/api/sessions/" + url.PathEscape(contextID)
	query := endpoint.Query()
	query.Set("user_id", owner)
	endpoint.RawQuery = query.Encode()
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build kagent session request: %w", err)
	}
	request.Header.Set("X-User-ID", owner)
	return request, nil
}

func responseError(operation, contextID string, response *http.Response) error {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxErrorBody))
	if err != nil {
		return fmt.Errorf("%s kagent session %q: status %d (read bounded response: %w)", operation, contextID, response.StatusCode, err)
	}
	return fmt.Errorf(
		"%s kagent session %q: status %d: %s",
		operation, contextID, response.StatusCode, strings.TrimSpace(string(body)),
	)
}
