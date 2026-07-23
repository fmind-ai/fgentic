package directory

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Keycloak is a narrowly-scoped, read-only IdP directory client. It uses the client-credentials
// grant to obtain a bearer token and reads ONLY the bound groups, their members, and each member's
// `matrix_localpart` attribute. It never mutates a user, never issues a token for a user, and holds
// no Synapse or MAS admin authority (docs/adr/0009). It is the production adapter; deterministic
// coverage exercises it against an httptest fixture, and the live-cluster read is the deferred
// acceptance path.
type Keycloak struct {
	baseURL      string
	realm        string
	clientID     string
	clientSecret string
	pageSize     int
	httpClient   *http.Client

	mu    sync.Mutex
	token string
	// tokenExp is when the cached bearer token expires; a small skew forces early refresh.
	tokenExp time.Time
}

// NewKeycloak builds a read-only directory client. The client secret is read from a mounted file by
// the caller and passed in, never taken from the process environment.
func NewKeycloak(baseURL, realm, clientID, clientSecret string, pageSize int, httpClient *http.Client) *Keycloak {
	return &Keycloak{
		baseURL:      strings.TrimRight(baseURL, "/"),
		realm:        realm,
		clientID:     clientID,
		clientSecret: clientSecret,
		pageSize:     pageSize,
		httpClient:   httpClient,
	}
}

// userRepresentation is the subset of the Keycloak user representation the reconciler trusts: the
// immutable id (the OIDC `sub`) and the administrator-managed attributes.
type userRepresentation struct {
	ID         string              `json:"id"`
	Attributes map[string][]string `json:"attributes"`
}

// Snapshot reads the current membership of exactly the requested groups. On ANY error it returns a
// snapshot with Complete=false so the reconciler retains last-known Matrix state and makes no grants
// or removals; it never returns a partial set as if it were authoritative.
func (k *Keycloak) Snapshot(ctx context.Context, groups []string) (Snapshot, error) {
	out := Snapshot{Groups: make(map[string][]Member, len(groups)), Complete: false}
	for _, group := range groups {
		members, err := k.groupMembers(ctx, group)
		if err != nil {
			return out, fmt.Errorf("read group %q: %w", group, err)
		}
		out.Groups[group] = members
	}
	out.Complete = true
	return out, nil
}

func (k *Keycloak) groupMembers(ctx context.Context, group string) ([]Member, error) {
	groupID, err := k.groupID(ctx, group)
	if err != nil {
		return nil, err
	}
	var members []Member
	for first := 0; ; first += k.pageSize {
		endpoint := fmt.Sprintf("%s/admin/realms/%s/groups/%s/members", k.baseURL, url.PathEscape(k.realm), url.PathEscape(groupID))
		q := url.Values{}
		q.Set("first", strconv.Itoa(first))
		q.Set("max", strconv.Itoa(k.pageSize))
		q.Set("briefRepresentation", "false") // include attributes so matrix_localpart is present
		var page []userRepresentation
		if err := k.getJSON(ctx, endpoint+"?"+q.Encode(), &page); err != nil {
			return nil, err
		}
		for _, u := range page {
			members = append(members, Member{Sub: u.ID, Localpart: firstAttr(u.Attributes, "matrix_localpart")})
		}
		if len(page) < k.pageSize {
			break // last (or only) page: a short page ends a complete traversal
		}
	}
	return members, nil
}

func (k *Keycloak) groupID(ctx context.Context, group string) (string, error) {
	// group-by-path takes the path without the leading slash; each segment is URL-escaped.
	segments := strings.Split(strings.TrimPrefix(group, "/"), "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	endpoint := fmt.Sprintf("%s/admin/realms/%s/group-by-path/%s", k.baseURL, url.PathEscape(k.realm), strings.Join(segments, "/"))
	var rep struct {
		ID string `json:"id"`
	}
	if err := k.getJSON(ctx, endpoint, &rep); err != nil {
		return "", err
	}
	if rep.ID == "" {
		return "", fmt.Errorf("group %q resolved to an empty id", group)
	}
	return rep.ID, nil
}

func (k *Keycloak) getJSON(ctx context.Context, endpoint string, into any) error {
	token, err := k.bearer(ctx)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("unexpected status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(resp.Body).Decode(into); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func (k *Keycloak) bearer(ctx context.Context) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.token != "" && time.Now().Before(k.tokenExp) {
		return k.token, nil
	}
	form := url.Values{}
	form.Set("grant_type", "client_credentials")
	form.Set("client_id", k.clientID)
	form.Set("client_secret", k.clientSecret)
	endpoint := fmt.Sprintf("%s/realms/%s/protocol/openid-connect/token", k.baseURL, url.PathEscape(k.realm))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("build token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := k.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("do token request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("token endpoint status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tok); err != nil {
		return "", fmt.Errorf("decode token: %w", err)
	}
	if tok.AccessToken == "" {
		return "", fmt.Errorf("token endpoint returned an empty access_token")
	}
	k.token = tok.AccessToken
	// Refresh 30s early; default to a short life when the server omits expires_in.
	ttl := time.Duration(tok.ExpiresIn) * time.Second
	if ttl <= 30*time.Second {
		ttl = 30 * time.Second
	}
	k.tokenExp = time.Now().Add(ttl - 30*time.Second)
	return k.token, nil
}

// firstAttr returns the first value of a single-valued Keycloak attribute, or "" when absent.
func firstAttr(attrs map[string][]string, key string) string {
	values := attrs[key]
	if len(values) == 0 {
		return ""
	}
	return strings.TrimSpace(values[0])
}
