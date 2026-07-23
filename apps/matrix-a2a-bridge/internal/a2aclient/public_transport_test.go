package a2aclient

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"sync/atomic"
	"testing"
)

type publicResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f publicResolverFunc) LookupNetIP(
	ctx context.Context,
	network, host string,
) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

func TestPublicDialContextRejectsSpecialUseAndMixedAnswersBeforeDial(t *testing.T) {
	tests := map[string][]netip.Addr{
		"private": {netip.MustParseAddr("10.0.0.1")},
		"loopback": {
			netip.MustParseAddr("127.0.0.1"),
		},
		"carrier-grade NAT": {
			netip.MustParseAddr("100.64.0.1"),
		},
		"IPv6 unique-local": {
			netip.MustParseAddr("fd00::1"),
		},
		"IPv4-mapped IPv6": {
			netip.MustParseAddr("::ffff:127.0.0.1"),
		},
		"multicast": {
			netip.MustParseAddr("224.0.0.1"),
		},
		"mixed public and private": {
			netip.MustParseAddr("93.184.216.34"),
			netip.MustParseAddr("192.168.1.1"),
		},
	}
	for name, addresses := range tests {
		t.Run(name, func(t *testing.T) {
			var dialed atomic.Bool
			resolver := publicResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				return addresses, nil
			})
			dial := publicDialContext(resolver, func(context.Context, string, string) (net.Conn, error) {
				dialed.Store(true)
				return nil, nil
			})
			if _, err := dial(t.Context(), "tcp", "peer.example.com:443"); err == nil {
				t.Fatal("public dial accepted a special-use DNS answer")
			}
			if dialed.Load() {
				t.Fatal("public dial opened a connection before rejecting all DNS answers")
			}
		})
	}
}

func TestPublicDialContextDialsValidatedAddress(t *testing.T) {
	resolver := publicResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
	})
	var dialedAddress string
	dial := publicDialContext(resolver, func(_ context.Context, _, address string) (net.Conn, error) {
		dialedAddress = address
		client, server := net.Pipe()
		t.Cleanup(func() { _ = server.Close() })
		return client, nil
	})
	conn, err := dial(t.Context(), "tcp", "peer.example.com:443")
	if err != nil {
		t.Fatalf("public dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if dialedAddress != "93.184.216.34:443" {
		t.Fatalf("dialed address = %q", dialedAddress)
	}
}

func TestResolvedFediverseRouteUsesPublicOnlyTransport(t *testing.T) {
	target := newFediverseTestTarget(t, "https://peer.example.com/users/agent")
	resolved, err := target.resolvedRemote(
		"https://a2a.example.com/agent",
		"https://a2a.example.com/card",
	)
	if err != nil {
		t.Fatalf("resolvedRemote: %v", err)
	}
	if !resolved.publicOnly {
		t.Fatal("discovery-derived route did not require public-only dialing")
	}

	client := New("http://local.invalid", "local-secret", nil)
	wrapped, ok := client.remoteUserTransport(resolved).(*userTransport)
	if !ok {
		t.Fatalf("public transport = %T", client.remoteUserTransport(resolved))
	}
	transport, ok := wrapped.base.(*http.Transport)
	if !ok {
		t.Fatalf("public base transport = %T", wrapped.base)
	}
	if transport.Proxy != nil || transport.DialContext == nil {
		t.Fatalf(
			"public transport has proxy=%t dialer=%t",
			transport.Proxy != nil,
			transport.DialContext != nil,
		)
	}
	if wrapped.apiKey != "" {
		t.Fatal("public transport carries the local gateway credential")
	}
}
