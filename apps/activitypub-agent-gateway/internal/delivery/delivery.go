// Package delivery posts signed ActivityPub activities to remote inboxes. It is the OUTBOUND half of
// federation the inbound border already governs: a sovereign Group actor uses it to deliver Accept
// and Announce activities to its followers (issue #217). Every delivery is HTTP-Signature signed
// with the sender's Ed25519 key so the receiving server can authenticate it, and every activity that
// carries provenance is additionally FEP-8b32 object-signed upstream before it reaches here.
package delivery

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/httpsig"
)

// maxRespBytes bounds an untrusted inbox response body we read only to drain the connection.
const maxRespBytes = 64 << 10

// Deliverer signs and posts activities to remote inboxes with one shared Ed25519 key. keyID is
// derived per sender actor (<actorID>#main-key), matching the `publicKey` each actor publishes.
type Deliverer struct {
	client *http.Client
	priv   ed25519.PrivateKey
	now    func() time.Time
	log    *slog.Logger
}

// New builds a Deliverer. client should carry a sane timeout and, in cluster, an egress
// NetworkPolicy — outbound delivery is a distinct trust boundary.
func New(client *http.Client, priv ed25519.PrivateKey, log *slog.Logger) *Deliverer {
	if client == nil {
		client = http.DefaultClient
	}
	return &Deliverer{client: client, priv: priv, now: func() time.Time { return time.Now().UTC() }, log: log}
}

// Deliver signs activity as senderActorID and POSTs it to inboxURL, returning an error on a non-2xx
// response. The body is the exact bytes signed (both the HTTP digest and any embedded object proof).
func (d *Deliverer) Deliver(ctx context.Context, inboxURL, senderActorID string, activity []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, inboxURL, bytes.NewReader(activity))
	if err != nil {
		return fmt.Errorf("build delivery request: %w", err)
	}
	req.Header.Set("Content-Type", "application/activity+json")
	req.Header.Set("Accept", "application/activity+json")
	if err := httpsig.Sign(req, activity, senderActorID+"#main-key", d.priv, d.now()); err != nil {
		return fmt.Errorf("sign delivery: %w", err)
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return fmt.Errorf("deliver to %s: %w", inboxURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRespBytes))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("deliver to %s: status %d", inboxURL, resp.StatusCode)
	}
	return nil
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
