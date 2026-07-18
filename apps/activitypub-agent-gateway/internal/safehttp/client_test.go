package safehttp

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"sync/atomic"
	"testing"
)

const testPublicHost = "federation.example.com"

var testPublicAddr = netip.MustParseAddr("93.184.216.34")

func TestClientRejectsUnsafeURLsBeforeDial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		url  string
	}{
		{name: "plain HTTP", url: "http://93.184.216.34/inbox"},
		{name: "userinfo", url: "https://user@93.184.216.34/inbox"},
		{name: "IPv4 unspecified", url: "https://0.0.0.0/inbox"},
		{name: "IPv4 loopback", url: "https://127.0.0.1/inbox"},
		{name: "IPv4 private 10", url: "https://10.0.0.1/inbox"},
		{name: "IPv4 private 172", url: "https://172.16.0.1/inbox"},
		{name: "IPv4 private 192", url: "https://192.168.0.1/inbox"},
		{name: "IPv4 shared carrier space", url: "https://100.64.0.1/inbox"},
		{name: "IPv4 benchmark space", url: "https://198.18.0.1/inbox"},
		{name: "IPv4 documentation space", url: "https://192.0.2.1/inbox"},
		{name: "cloud metadata link-local", url: "https://169.254.169.254/latest/meta-data"},
		{name: "IPv6 unspecified", url: "https://[::]/inbox"},
		{name: "IPv6 loopback", url: "https://[::1]/inbox"},
		{name: "IPv6 private", url: "https://[fd00::1]/inbox"},
		{name: "IPv6 link-local", url: "https://[fe80::1]/inbox"},
		{name: "IPv6 documentation space", url: "https://[2001:db8::1]/inbox"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var dials atomic.Int64
			base := &http.Client{Transport: &http.Transport{DialContext: func(
				context.Context,
				string,
				string,
			) (net.Conn, error) {
				dials.Add(1)
				return nil, fmt.Errorf("unexpected dial")
			}}}
			client, err := NewClient(base, ResolverFunc(publicResolution))
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if _, err := client.Get(tt.url); err == nil {
				t.Fatal("unsafe URL must be rejected")
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("dials = %d, want 0", got)
			}
		})
	}
}

func TestClientRejectsPrivateAndMixedDNSBeforeDial(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		addresses []netip.Addr
	}{
		{name: "loopback", addresses: []netip.Addr{netip.MustParseAddr("127.0.0.1")}},
		{name: "private", addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}},
		{name: "link-local", addresses: []netip.Addr{netip.MustParseAddr("169.254.169.254")}},
		{name: "mixed", addresses: []netip.Addr{testPublicAddr, netip.MustParseAddr("192.168.1.1")}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var dials atomic.Int64
			base := &http.Client{Transport: &http.Transport{DialContext: func(
				context.Context,
				string,
				string,
			) (net.Conn, error) {
				dials.Add(1)
				return nil, fmt.Errorf("unexpected dial")
			}}}
			resolver := ResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
				return tt.addresses, nil
			})
			client, err := NewClient(base, resolver)
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}
			if _, err := client.Get("https://" + testPublicHost + "/actor"); err == nil {
				t.Fatal("unsafe DNS answer must be rejected")
			}
			if got := dials.Load(); got != 0 {
				t.Fatalf("dials = %d, want 0", got)
			}
		})
	}
}

func TestClientRevalidatesDNSAtEveryDial(t *testing.T) {
	t.Parallel()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	base := srv.Client()
	transport := base.Transport.(*http.Transport).Clone()
	transport.DisableKeepAlives = true
	var dials atomic.Int64
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		dials.Add(1)
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	base.Transport = transport

	var lookups atomic.Int64
	resolver := ResolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		if lookups.Add(1) == 1 {
			return []netip.Addr{testPublicAddr}, nil
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	client, err := NewClient(base, resolver)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	resp, err := client.Get("https://" + testPublicHost + "/actor")
	if err != nil {
		t.Fatalf("first GET: %v", err)
	}
	_ = resp.Body.Close()
	if _, err := client.Get("https://" + testPublicHost + "/actor"); err == nil {
		t.Fatal("rebound private address must be rejected")
	}
	if got := lookups.Load(); got != 2 {
		t.Fatalf("lookups = %d, want 2", got)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want only the public first dial", got)
	}
}

func TestClientGuardsRedirectDestinations(t *testing.T) {
	t.Parallel()
	var hits atomic.Int64
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Location", "https://127.0.0.1/private")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()

	client := mappedClient(t, srv, ResolverFunc(publicResolution))
	if _, err := client.Get("https://" + testPublicHost + "/actor"); err == nil {
		t.Fatal("private redirect must be rejected")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("public server hits = %d, want 1", got)
	}
}

func TestClientRejectsUnsupportedTransport(t *testing.T) {
	t.Parallel()
	client, err := NewClient(&http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return nil, nil
	})}, nil)
	if err == nil || client != nil {
		t.Fatalf("NewClient = (%v, %v), want nil error result", client, err)
	}
}

func mappedClient(t *testing.T, srv *httptest.Server, resolver Resolver) *http.Client {
	t.Helper()
	base := srv.Client()
	transport := base.Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, srv.Listener.Addr().String())
	}
	base.Transport = transport
	client, err := NewClient(base, resolver)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func publicResolution(context.Context, string, string) ([]netip.Addr, error) {
	return []netip.Addr{testPublicAddr}, nil
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
