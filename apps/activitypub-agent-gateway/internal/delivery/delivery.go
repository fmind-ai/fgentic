// Package delivery posts signed ActivityPub activities to remote inboxes. It is the OUTBOUND half of
// federation the inbound border already governs: a sovereign Group actor uses it to deliver Accept
// and Announce activities to its followers (issue #217). Every delivery prefers RFC 9421 and uses a
// per-server Cavage fallback so the receiving server can authenticate it; every activity that carries
// provenance is additionally FEP-8b32 object-signed upstream before it reaches here.
package delivery

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
)

const (
	// maxRespBytes bounds an untrusted inbox response body we read only to drain the connection.
	maxRespBytes = 64 << 10
	// maxRememberedServers prevents an attacker-controlled follower list from growing profile memory
	// without bound. Eviction only loses an optimization: the next delivery probes RFC 9421 again.
	maxRememberedServers = 4096
)

// Deliverer signs and posts activities to remote inboxes with one shared signing key. keyID is
// derived per sender actor (<actorID>#main-key), matching the `publicKey` each actor publishes.
type Deliverer struct {
	client   *http.Client
	priv     crypto.Signer
	now      func() time.Time
	log      *slog.Logger
	profiles profileMemory
}

// New builds a Deliverer. client should carry a sane timeout and, in cluster, an egress
// NetworkPolicy — outbound delivery is a distinct trust boundary.
func New(client *http.Client, priv crypto.Signer, log *slog.Logger) *Deliverer {
	if client == nil {
		client = http.DefaultClient
	}
	return &Deliverer{
		client: client,
		priv:   priv,
		now:    func() time.Time { return time.Now().UTC() },
		log:    log,
		profiles: profileMemory{
			byServer: make(map[string]httpsig.Profile),
		},
	}
}

// Deliver signs activity as senderActorID and POSTs it to inboxURL, returning an error on a non-2xx
// response. The body is the exact bytes signed (both the HTTP digest and any embedded object proof).
func (d *Deliverer) Deliver(ctx context.Context, inboxURL, senderActorID string, activity []byte) error {
	parsedURL, err := url.Parse(inboxURL)
	if err != nil {
		return fmt.Errorf("parse delivery inbox %q: %w", inboxURL, err)
	}
	if parsedURL.Host == "" {
		return fmt.Errorf("parse delivery inbox %q: missing authority", inboxURL)
	}
	if !strings.EqualFold(parsedURL.Scheme, "https") {
		return fmt.Errorf("parse delivery inbox %q: public federation requires https", inboxURL)
	}
	server := strings.ToLower(parsedURL.Host)
	first := d.profiles.get(server)
	status, err := d.deliver(ctx, inboxURL, senderActorID, activity, first)
	if err == nil {
		d.profiles.set(server, first)
		return nil
	}
	if status != http.StatusUnauthorized {
		return err
	}

	second := alternate(first)
	if _, retryErr := d.deliver(ctx, inboxURL, senderActorID, activity, second); retryErr != nil {
		return errors.Join(err, fmt.Errorf("%s fallback failed: %w", second, retryErr))
	}
	d.profiles.set(server, second)
	return nil
}

// PublicKey returns the transport key remote actors must discover as #main-key.
func (d *Deliverer) PublicKey() crypto.PublicKey {
	if d == nil || d.priv == nil {
		return nil
	}
	return d.priv.Public()
}

func (d *Deliverer) deliver(
	ctx context.Context,
	inboxURL string,
	senderActorID string,
	activity []byte,
	profile httpsig.Profile,
) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inboxURL, bytes.NewReader(activity))
	if err != nil {
		return 0, fmt.Errorf("build delivery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Accept", "application/activity+json")
	if err := httpsig.SignWithProfile(req, activity, senderActorID+"#main-key", d.priv, d.now(), profile); err != nil {
		return 0, fmt.Errorf("sign %s delivery: %w", profile, err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("deliver to %s with %s: %w", inboxURL, profile, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return resp.StatusCode, fmt.Errorf("deliver to %s with %s: status %d", inboxURL, profile, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

func alternate(profile httpsig.Profile) httpsig.Profile {
	if profile == httpsig.ProfileCavage {
		return httpsig.ProfileRFC9421
	}
	return httpsig.ProfileCavage
}

// profileMemory records one successful wire profile per remote authority. It is deliberately
// process-local: losing it on restart only repeats the bounded double-knock negotiation.
type profileMemory struct {
	mu       sync.Mutex
	byServer map[string]httpsig.Profile
	order    []string
}

func (m *profileMemory) get(server string) httpsig.Profile {
	m.mu.Lock()
	defer m.mu.Unlock()
	if profile, ok := m.byServer[server]; ok {
		return profile
	}
	return httpsig.ProfileRFC9421
}

func (m *profileMemory) set(server string, profile httpsig.Profile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.byServer[server]; !exists {
		if len(m.order) == maxRememberedServers {
			delete(m.byServer, m.order[0])
			m.order = m.order[1:]
		}
		m.order = append(m.order, server)
	}
	m.byServer[server] = profile
}

// Fanout delivers activity to every inbox, best-effort: one failing inbox does not stop the others.
// It returns the number of successful deliveries; failures are logged with a content-free reason.
func (d *Deliverer) Fanout(ctx context.Context, inboxes []string, senderActorID string, activity []byte) int {
	delivered := 0
	for _, inbox := range inboxes {
		if err := d.Deliver(ctx, inbox, senderActorID, activity); err != nil {
			if d.log != nil {
				d.log.Warn("fanout delivery failed", "sender", senderActorID, "error", err.Error())
			}
			continue
		}
		delivered++
	}
	return delivered
}
