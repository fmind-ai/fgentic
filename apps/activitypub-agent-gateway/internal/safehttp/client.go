// Package safehttp provides HTTP clients for requests whose destinations come from untrusted
// ActivityPub documents. It permits only HTTPS endpoints whose dial-time DNS answers are public.
package safehttp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Resolver is the DNS boundary used immediately before a connection is opened.
type Resolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// ResolverFunc adapts a function into a Resolver. It is primarily useful for deterministic tests.
type ResolverFunc func(context.Context, string, string) ([]netip.Addr, error)

// LookupNetIP implements Resolver.
func (f ResolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type guardedTransport struct {
	base *http.Transport
}

// specialUsePrefixes mirrors the IANA IPv4 and IPv6 Special-Purpose Address Registries. This
// boundary conservatively rejects every registered special-use block, including globally reachable
// anycast/translation assignments that are not ordinary public endpoints. IPv6 additionally stays
// inside IANA's currently allocated 2000::/3 global-unicast space.
//
// https://www.iana.org/assignments/iana-ipv4-special-registry/
// https://www.iana.org/assignments/iana-ipv6-special-registry/
var specialUsePrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.31.196.0/24"),
	netip.MustParsePrefix("192.52.193.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("192.175.48.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("100:0:0:1::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("2620:4f:8000::/48"),
	netip.MustParsePrefix("3fff::/20"),
	netip.MustParsePrefix("5f00::/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("fe80::/10"),
}

var allocatedIPv6GlobalUnicast = netip.MustParsePrefix("2000::/3")

// NewClient clones base and installs fail-closed public-internet guards. Caller-supplied dialers are
// deliberately discarded: only the package-owned dialer may open a production connection after
// validation. An already guarded client is returned as an independent shallow clone.
func NewClient(base *http.Client) (*http.Client, error) {
	return newClient(base, net.DefaultResolver, (&net.Dialer{}).DialContext)
}

// NewTestClient builds a guarded client whose injected resolver and dialer come from base. Requiring
// testing.TB keeps local-fixture redirection out of production call sites without weakening the
// production constructor's exact-address invariant.
func NewTestClient(t testing.TB, base *http.Client, resolver Resolver) *http.Client {
	t.Helper()
	if base == nil {
		t.Fatal("safe HTTP test client: base client is required")
		return nil
	}
	transport, ok := base.Transport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		t.Fatalf("safe HTTP test client: base transport %T needs a test dialer", base.Transport)
		return nil
	}
	client, err := newClient(base, resolver, transport.DialContext)
	if err != nil {
		t.Fatalf("safe HTTP test client: %v", err)
		return nil
	}
	return client
}

func newClient(base *http.Client, resolver Resolver, baseDial dialContextFunc) (*http.Client, error) {
	if base == nil {
		base = http.DefaultClient
	}
	client := *base
	if _, ok := client.Transport.(*guardedTransport); ok {
		return &client, nil
	}

	var transport *http.Transport
	switch configured := client.Transport.(type) {
	case nil:
		defaultTransport, ok := http.DefaultTransport.(*http.Transport)
		if !ok {
			return nil, fmt.Errorf("safe HTTP client: default transport is %T", http.DefaultTransport)
		}
		transport = defaultTransport.Clone()
	case *http.Transport:
		transport = configured.Clone()
	default:
		return nil, fmt.Errorf("safe HTTP client: transport %T cannot enforce dial-time address validation", configured)
	}

	if resolver == nil {
		resolver = net.DefaultResolver
	}
	guardedDial := guardedDialContext(resolver, baseDial)
	transport.Proxy = nil
	transport.DialContext = guardedDial
	transport.DialTLSContext = guardedTLSDialContext(
		guardedDial,
		transport.TLSClientConfig,
		transport.TLSHandshakeTimeout,
	)
	client.Transport = &guardedTransport{base: transport}
	return &client, nil
}

func (t *guardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := ValidateURL(req.URL); err != nil {
		return nil, err
	}
	return t.base.RoundTrip(req)
}

func (t *guardedTransport) CloseIdleConnections() {
	t.base.CloseIdleConnections()
}

// ValidateURL rejects destinations that cannot be safely treated as public ActivityPub endpoints.
// Address resolution is deliberately left to the guarded dialer so it cannot go stale before use.
func ValidateURL(target *url.URL) error {
	if target == nil {
		return errors.New("unsafe ActivityPub URL: missing URL")
	}
	if !strings.EqualFold(target.Scheme, "https") {
		return fmt.Errorf("unsafe ActivityPub URL: scheme %q is not https", target.Scheme)
	}
	if target.User != nil {
		return errors.New("unsafe ActivityPub URL: userinfo is not allowed")
	}
	if target.Hostname() == "" {
		return errors.New("unsafe ActivityPub URL: missing host")
	}
	if strings.Contains(target.Hostname(), "%") {
		return errors.New("unsafe ActivityPub URL: scoped addresses are not allowed")
	}
	if literal, err := netip.ParseAddr(target.Hostname()); err == nil {
		if err := validatePublicAddr(literal); err != nil {
			return err
		}
	}
	return nil
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

func guardedDialContext(
	resolver Resolver,
	baseDial dialContextFunc,
) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("unsafe ActivityPub dial address %q: %w", address, err)
		}
		addresses, err := resolvePublic(ctx, resolver, network, host)
		if err != nil {
			return nil, err
		}

		var dialErrs []error
		for _, addr := range addresses {
			conn, dialErr := baseDial(ctx, network, net.JoinHostPort(addr.String(), port))
			if dialErr == nil {
				return conn, nil
			}
			dialErrs = append(dialErrs, dialErr)
		}
		return nil, fmt.Errorf("dial public ActivityPub host: %w", errors.Join(dialErrs...))
	}
}

func guardedTLSDialContext(
	dial dialContextFunc,
	baseConfig *tls.Config,
	handshakeTimeout time.Duration,
) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := dial(ctx, network, address)
		if err != nil {
			return nil, err
		}
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("configure ActivityPub TLS for %q: %w", address, err)
		}
		config := &tls.Config{MinVersion: tls.VersionTLS12}
		if baseConfig != nil {
			config = baseConfig.Clone()
			if config.MinVersion == 0 {
				config.MinVersion = tls.VersionTLS12
			}
		}
		if config.ServerName == "" {
			config.ServerName = host
		}
		tlsConn := tls.Client(conn, config)
		handshakeCtx := ctx
		cancel := func() {}
		if handshakeTimeout > 0 {
			handshakeCtx, cancel = context.WithTimeout(ctx, handshakeTimeout)
		}
		defer cancel()
		if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("handshake with public ActivityPub host %q: %w", host, err)
		}
		return tlsConn, nil
	}
}

func resolvePublic(ctx context.Context, resolver Resolver, network, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		if err := validatePublicAddr(literal); err != nil {
			return nil, err
		}
		return []netip.Addr{literal.Unmap()}, nil
	}

	lookupNetwork := "ip"
	switch network {
	case "tcp4":
		lookupNetwork = "ip4"
	case "tcp6":
		lookupNetwork = "ip6"
	}
	addresses, err := resolver.LookupNetIP(ctx, lookupNetwork, host)
	if err != nil {
		return nil, fmt.Errorf("resolve public ActivityPub host %q: %w", host, err)
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("resolve public ActivityPub host %q: no addresses", host)
	}
	for _, addr := range addresses {
		if err := validatePublicAddr(addr); err != nil {
			return nil, fmt.Errorf("resolve public ActivityPub host %q: %w", host, err)
		}
	}
	return addresses, nil
}

func validatePublicAddr(addr netip.Addr) error {
	if addr.Is4In6() {
		return fmt.Errorf("unsafe ActivityPub address %q is an IPv4-mapped IPv6 address", addr)
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() {
		return fmt.Errorf("unsafe ActivityPub address %q is not public", addr)
	}
	if addr.Is6() && !allocatedIPv6GlobalUnicast.Contains(addr) {
		return fmt.Errorf("unsafe ActivityPub address %q is outside allocated IPv6 global unicast", addr)
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("unsafe ActivityPub address %q is not public", addr)
		}
	}
	return nil
}
