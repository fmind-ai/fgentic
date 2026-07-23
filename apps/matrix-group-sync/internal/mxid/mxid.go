// Package mxid forms and validates full Matrix user IDs. Every identity decision in the reconciler
// goes through here so the platform's federation-safety rule is enforced in exactly one place: a
// principal is NEVER matched by localpart alone — the full `@localpart:server` is formed and the
// server is checked against the local homeserver (D6, docs/adr/0009). A local IdP group therefore
// can only ever assert membership for a LOCAL MXID, never a partner user's.
package mxid

import (
	"fmt"
	"strings"
)

// localpartChars is the Matrix user-localpart grammar (historical/common form): lowercase letters,
// digits, and `._=/+-`. `matrix_localpart` is an administrator-managed IdP attribute, so an invalid
// value is a directory-integrity fault and fails closed (no grant) rather than being coerced.
const localpartChars = "abcdefghijklmnopqrstuvwxyz0123456789._=/+-"

// ValidateLocalpart rejects an empty or malformed localpart. The reconciler treats a member whose
// `matrix_localpart` fails this check as ungrantable (fail closed), never guessing an identity.
func ValidateLocalpart(localpart string) error {
	if localpart == "" {
		return fmt.Errorf("localpart must not be empty")
	}
	if localpart != strings.TrimSpace(localpart) {
		return fmt.Errorf("localpart %q must not have surrounding whitespace", localpart)
	}
	for _, r := range localpart {
		if !strings.ContainsRune(localpartChars, r) {
			return fmt.Errorf("localpart %q contains invalid character %q", localpart, r)
		}
	}
	return nil
}

// Format builds the full local MXID `@<localpart>:<serverName>` after validating the localpart.
func Format(localpart, serverName string) (string, error) {
	if err := ValidateLocalpart(localpart); err != nil {
		return "", err
	}
	if serverName == "" {
		return "", fmt.Errorf("serverName must not be empty")
	}
	return "@" + localpart + ":" + serverName, nil
}

// Parse splits a full MXID into its localpart and server, rejecting a bare localpart.
func Parse(mxid string) (localpart, server string, err error) {
	if !strings.HasPrefix(mxid, "@") {
		return "", "", fmt.Errorf("%q is not a full MXID (missing '@')", mxid)
	}
	local, srv, ok := strings.Cut(mxid[1:], ":")
	if !ok || local == "" || srv == "" {
		return "", "", fmt.Errorf("%q is not a full MXID '@localpart:server'", mxid)
	}
	return local, srv, nil
}

// IsLocal reports whether mxid is a well-formed full MXID whose server equals serverName. A
// malformed MXID or any partner server returns false: the reconciler never asserts a local IdP
// group over a remote user, and never mistakes a partner user for a local one.
func IsLocal(mxid, serverName string) bool {
	_, server, err := Parse(mxid)
	if err != nil {
		return false
	}
	return server == serverName
}

// IsLocalGhost reports whether mxid is a local, bridge-owned agent ghost (`@<prefix>*:<server>`).
// The reconciler never invites or revokes a ghost: agents are placed in rooms by the bridge, and a
// remote MXID sharing the prefix is deliberately NOT treated as a ghost (server is checked too).
func IsLocalGhost(mxid, prefix, serverName string) bool {
	local, server, err := Parse(mxid)
	if err != nil {
		return false
	}
	return server == serverName && strings.HasPrefix(local, prefix)
}
