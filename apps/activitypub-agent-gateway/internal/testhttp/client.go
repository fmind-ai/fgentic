// Package testhttp maps public HTTPS hostnames to local TLS fixtures while retaining safehttp's
// production URL and dial-time address validation. It is intended only for tests.
package testhttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sort"
	"testing"

	"github.com/fmind-ai/activitypub-agent-gateway/internal/safehttp"
)

// Client returns a guarded client that resolves each map key to a synthetic public address and
// connects that address to the corresponding local TLS server.
func Client(t testing.TB, servers map[string]*httptest.Server) *http.Client {
	t.Helper()
	if len(servers) == 0 {
		t.Fatal("testhttp: at least one server is required")
	}

	hosts := make([]string, 0, len(servers))
	for host := range servers {
		hosts = append(hosts, host)
	}
	sort.Strings(hosts)

	addresses := make(map[string]netip.Addr, len(hosts))
	listeners := make(map[netip.Addr]string, len(hosts))
	for i, host := range hosts {
		addr := netip.AddrFrom4([4]byte{93, 184, 216, byte(34 + i)})
		addresses[host] = addr
		listeners[addr] = servers[host].Listener.Addr().String()
	}

	base := servers[hosts[0]].Client()
	transport := base.Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		addr, err := netip.ParseAddr(host)
		if err != nil {
			return nil, err
		}
		listener, ok := listeners[addr.Unmap()]
		if !ok {
			return nil, fmt.Errorf("testhttp: no listener for %s", addr)
		}
		return (&net.Dialer{}).DialContext(ctx, network, listener)
	}
	base.Transport = transport

	resolver := safehttp.ResolverFunc(func(
		_ context.Context,
		_, host string,
	) ([]netip.Addr, error) {
		addr, ok := addresses[host]
		if !ok {
			return nil, fmt.Errorf("testhttp: no public fixture for %s", host)
		}
		return []netip.Addr{addr}, nil
	})
	client, err := safehttp.NewClient(base, resolver)
	if err != nil {
		t.Fatalf("testhttp: build guarded client: %v", err)
	}
	return client
}

// URL returns the public HTTPS origin for a test server host.
func URL(host string) string {
	return "https://" + host
}
