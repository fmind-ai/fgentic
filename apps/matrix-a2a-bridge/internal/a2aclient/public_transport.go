package a2aclient

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

type publicResolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type dialContextFunc func(context.Context, string, string) (net.Conn, error)

// specialUsePrefixes mirrors the IANA IPv4 and IPv6 Special-Purpose Address Registries. A
// discovery-derived route is untrusted routing data even after its AgentCard proves identity, so
// every DNS answer must remain an ordinary public endpoint at the instant a connection is opened.
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

func publicDialContext(resolver publicResolver, dial dialContextFunc) dialContextFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, fmt.Errorf("split discovered remote address %q: %w", address, err)
		}
		lookupNetwork := "ip"
		switch network {
		case "tcp4":
			lookupNetwork = "ip4"
		case "tcp6":
			lookupNetwork = "ip6"
		case "tcp":
		default:
			return nil, fmt.Errorf("discovered remote network %q is not TCP", network)
		}
		addresses, err := resolver.LookupNetIP(ctx, lookupNetwork, host)
		if err != nil {
			return nil, fmt.Errorf("resolve discovered remote host %q: %w", host, err)
		}
		if len(addresses) == 0 {
			return nil, fmt.Errorf("resolve discovered remote host %q: no addresses", host)
		}
		for _, addr := range addresses {
			if err := validatePublicAddr(addr); err != nil {
				return nil, fmt.Errorf("resolve discovered remote host %q: %w", host, err)
			}
		}

		dialErrs := make([]error, 0, len(addresses))
		for _, addr := range addresses {
			conn, dialErr := dial(ctx, network, net.JoinHostPort(addr.Unmap().String(), port))
			if dialErr == nil {
				return conn, nil
			}
			dialErrs = append(dialErrs, dialErr)
		}
		return nil, fmt.Errorf("dial discovered remote host %q: %w", host, errors.Join(dialErrs...))
	}
}

func validatePublicAddr(addr netip.Addr) error {
	if addr.Is4In6() {
		return fmt.Errorf("address %s is IPv4-mapped IPv6", addr)
	}
	addr = addr.Unmap()
	if !addr.IsValid() || !addr.IsGlobalUnicast() || addr.IsUnspecified() || addr.IsLoopback() ||
		addr.IsPrivate() || addr.IsLinkLocalUnicast() || addr.IsLinkLocalMulticast() ||
		addr.IsMulticast() {
		return fmt.Errorf("address %s is not public", addr)
	}
	if addr.Is6() && !allocatedIPv6GlobalUnicast.Contains(addr) {
		return fmt.Errorf("address %s is not allocated global unicast", addr)
	}
	for _, prefix := range specialUsePrefixes {
		if prefix.Contains(addr) {
			return fmt.Errorf("address %s is special-use", addr)
		}
	}
	return nil
}
