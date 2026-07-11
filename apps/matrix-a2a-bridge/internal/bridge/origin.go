package bridge

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"maunium.net/go/mautrix/id"
)

const matrixOriginNetwork = "matrix"

var (
	originNetworkRE   = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	originLocalpartRE = regexp.MustCompile(`^[0-9A-Za-z._=/+\-]+$`)
)

type senderOriginKind string

const (
	senderOriginMatrix senderOriginKind = "matrix"
	senderOriginBridge senderOriginKind = "bridge"
)

// senderOrigin is bounded attribution derived only from configured full-MXID namespaces. It
// deliberately excludes remote-network user and tenant IDs so it is safe for stable audit fields.
type senderOrigin struct {
	kind    senderOriginKind
	network string
}

type senderIdentity struct {
	mxid   id.UserID
	origin senderOrigin
}

func matrixSender(mxid id.UserID) senderIdentity {
	return senderIdentity{
		mxid: mxid,
		origin: senderOrigin{
			kind:    senderOriginMatrix,
			network: matrixOriginNetwork,
		},
	}
}

func (s senderIdentity) isBridged() bool {
	return s.origin.kind == senderOriginBridge
}

// rateLimitKey retains the existing native/federated key and namespaces only bridge-derived
// identities. The complete MXID preserves the documented per-sender budget; the origin prefix
// makes bridged attribution explicit without adding a high-cardinality metric label.
func (s senderIdentity) rateLimitKey(agent string) string {
	base := s.mxid.String() + "|" + agent
	if !s.isBridged() {
		return base
	}
	return "bridge:" + s.origin.network + "|" + base
}

type bridgedOriginRule struct {
	origin      senderOrigin
	pattern     string
	localPrefix string
	homeserver  string
	re          *regexp.Regexp
}

func compileBridgedOrigins(config map[string][]string) ([]bridgedOriginRule, error) {
	networks := make([]string, 0, len(config))
	for network := range config {
		networks = append(networks, network)
	}
	sort.Strings(networks)

	rules := make([]bridgedOriginRule, 0, len(config))
	for _, network := range networks {
		if !originNetworkRE.MatchString(network) || network == matrixOriginNetwork {
			return nil, fmt.Errorf(
				"bridged origin network %q must match %s and must not be %q",
				network,
				originNetworkRE,
				matrixOriginNetwork,
			)
		}
		patterns := config[network]
		if len(patterns) == 0 {
			return nil, fmt.Errorf("bridged origin network %q defines no MXID namespaces", network)
		}
		sort.Strings(patterns)
		for _, pattern := range patterns {
			rule, err := compileBridgedOriginRule(network, pattern)
			if err != nil {
				return nil, err
			}
			for _, existing := range rules {
				if bridgedOriginRulesOverlap(existing, rule) {
					return nil, fmt.Errorf(
						"bridged origin MXID namespaces %q (%s) and %q (%s) overlap",
						existing.pattern,
						existing.origin.network,
						rule.pattern,
						rule.origin.network,
					)
				}
			}
			rules = append(rules, rule)
		}
	}
	return rules, nil
}

// compileBridgedOriginRule accepts one deliberately narrow namespace form:
// @<literal-prefix>*:<exact-server>. This is expressive enough for appservice-owned bridge
// users, anchored over a full MXID, and makes overlap detection exact rather than heuristic.
func compileBridgedOriginRule(network, pattern string) (bridgedOriginRule, error) {
	localGlob, homeserver, ok := strings.Cut(strings.TrimPrefix(pattern, "@"), ":")
	if !strings.HasPrefix(pattern, "@") || !ok || homeserver == "" {
		return bridgedOriginRule{}, fmt.Errorf(
			"bridged origin %q namespace %q must be a full MXID glob",
			network,
			pattern,
		)
	}
	if strings.Count(localGlob, "*") != 1 || !strings.HasSuffix(localGlob, "*") {
		return bridgedOriginRule{}, fmt.Errorf(
			"bridged origin %q namespace %q must end its localpart with exactly one '*'",
			network,
			pattern,
		)
	}
	localPrefix := strings.TrimSuffix(localGlob, "*")
	if localPrefix == "" || !originLocalpartRE.MatchString(localPrefix) {
		return bridgedOriginRule{}, fmt.Errorf(
			"bridged origin %q namespace %q has an invalid literal localpart prefix",
			network,
			pattern,
		)
	}
	sample := id.NewUserID(localPrefix+"sample", homeserver)
	if _, _, err := sample.ParseAndValidateRelaxed(); err != nil {
		return bridgedOriginRule{}, fmt.Errorf(
			"bridged origin %q namespace %q has an invalid homeserver: %w",
			network,
			pattern,
			err,
		)
	}

	return bridgedOriginRule{
		origin: senderOrigin{
			kind:    senderOriginBridge,
			network: network,
		},
		pattern:     pattern,
		localPrefix: localPrefix,
		homeserver:  homeserver,
		re: regexp.MustCompile(
			"^@" + regexp.QuoteMeta(localPrefix) + ".*:" + regexp.QuoteMeta(homeserver) + "$",
		),
	}, nil
}

func bridgedOriginRulesOverlap(a, b bridgedOriginRule) bool {
	return a.homeserver == b.homeserver &&
		(strings.HasPrefix(a.localPrefix, b.localPrefix) || strings.HasPrefix(b.localPrefix, a.localPrefix))
}
