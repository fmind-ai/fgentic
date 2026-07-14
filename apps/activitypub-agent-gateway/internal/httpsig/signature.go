package httpsig

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// parsedSignature is a scheme-agnostic view of an inbound signature: the key, algorithm, ordered
// covered components (lowercased, unquoted), the raw signature bytes, and — for RFC 9421 — the
// verbatim signature params needed to reconstruct the @signature-params line.
type parsedSignature struct {
	scheme          string
	keyID           string
	algorithm       string
	components      []string
	signature       []byte
	created         time.Time
	signatureParams string
}

// covers reports whether name (lowercased, unquoted) is a signed component.
func (s *parsedSignature) covers(name string) bool {
	for _, c := range s.components {
		if c == name {
			return true
		}
	}
	return false
}

// signingString reconstructs the exact bytes the signer signed, per the active scheme.
func (s *parsedSignature) signingString(req *http.Request) (string, error) {
	if s.scheme == "rfc9421" {
		return s.signingStringRFC9421(req)
	}
	return s.signingStringCavage(req)
}

// --- Cavage draft (draft-cavage-http-signatures), the format Mastodon still emits ---

func parseCavage(header string) (*parsedSignature, error) {
	params, err := splitSignatureParams(header)
	if err != nil {
		return nil, err
	}
	sig := &parsedSignature{scheme: "cavage"}
	sig.keyID = params["keyid"]
	sig.algorithm = params["algorithm"]
	if headers, ok := params["headers"]; ok && headers != "" {
		for _, h := range strings.Fields(headers) {
			sig.components = append(sig.components, strings.ToLower(h))
		}
	} else {
		sig.components = []string{"date"} // draft default
	}
	if created, ok := params["created"]; ok {
		if unix, convErr := strconv.ParseInt(created, 10, 64); convErr == nil {
			sig.created = time.Unix(unix, 0)
		}
	}
	raw, ok := params["signature"]
	if !ok || raw == "" {
		return nil, fmt.Errorf("%w: missing signature value", ErrMalformedSignature)
	}
	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not valid base64", ErrMalformedSignature)
	}
	sig.signature = decoded
	if sig.keyID == "" {
		return nil, fmt.Errorf("%w: missing keyId", ErrMalformedSignature)
	}
	return sig, nil
}

func (s *parsedSignature) signingStringCavage(req *http.Request) (string, error) {
	lines := make([]string, 0, len(s.components))
	for _, c := range s.components {
		switch c {
		case "(request-target)":
			lines = append(lines, "(request-target): "+strings.ToLower(req.Method)+" "+req.URL.RequestURI())
		case "(created)":
			if s.created.IsZero() {
				return "", fmt.Errorf("%w: (created) covered but absent", ErrMalformedSignature)
			}
			lines = append(lines, "(created): "+strconv.FormatInt(s.created.Unix(), 10))
		case "host":
			lines = append(lines, "host: "+req.Host)
		default:
			lines = append(lines, c+": "+strings.TrimSpace(req.Header.Get(c)))
		}
	}
	return strings.Join(lines, "\n"), nil
}

// splitSignatureParams parses a comma-separated list of key="value" (or key=value) pairs,
// respecting quotes, into a lowercased-key map.
func splitSignatureParams(header string) (map[string]string, error) {
	out := make(map[string]string)
	for _, part := range splitOutsideQuotes(header, ',') {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, value, ok := strings.Cut(part, "=")
		if !ok {
			return nil, fmt.Errorf("%w: malformed parameter %q", ErrMalformedSignature, part)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)
		value = strings.TrimPrefix(value, "\"")
		value = strings.TrimSuffix(value, "\"")
		out[key] = value
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: empty signature header", ErrMalformedSignature)
	}
	return out, nil
}

func splitOutsideQuotes(s string, sep rune) []string {
	var parts []string
	var b strings.Builder
	inQuotes := false
	for _, r := range s {
		switch {
		case r == '"':
			inQuotes = !inQuotes
			b.WriteRune(r)
		case r == sep && !inQuotes:
			parts = append(parts, b.String())
			b.Reset()
		default:
			b.WriteRune(r)
		}
	}
	parts = append(parts, b.String())
	return parts
}

// --- RFC 9421 (HTTP Message Signatures) ---

func parseRFC9421(header http.Header) (*parsedSignature, error) {
	input := header.Get("Signature-Input")
	label, params, err := firstDictionaryMember(input)
	if err != nil {
		return nil, err
	}
	sig := &parsedSignature{scheme: "rfc9421", signatureParams: params}

	openIdx := strings.Index(params, "(")
	closeIdx := strings.Index(params, ")")
	if openIdx != 0 || closeIdx < 0 {
		return nil, fmt.Errorf("%w: signature-input must start with a component list", ErrMalformedSignature)
	}
	for _, item := range strings.Fields(params[openIdx+1 : closeIdx]) {
		component := strings.ToLower(strings.Trim(item, "\""))
		if component != "" {
			sig.components = append(sig.components, component)
		}
	}
	for _, kv := range splitOutsideQuotes(params[closeIdx+1:], ';') {
		kv = strings.TrimSpace(kv)
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.Trim(strings.TrimSpace(value), "\"")
		switch key {
		case "keyid":
			sig.keyID = value
		case "alg":
			sig.algorithm = value
		case "created":
			if unix, convErr := strconv.ParseInt(value, 10, 64); convErr == nil {
				sig.created = time.Unix(unix, 0)
			}
		}
	}
	if sig.keyID == "" {
		return nil, fmt.Errorf("%w: missing keyid", ErrMalformedSignature)
	}

	sigLabel, sigValue, err := firstDictionaryMember(header.Get("Signature"))
	if err != nil {
		return nil, err
	}
	if sigLabel != label {
		return nil, fmt.Errorf("%w: signature label mismatch", ErrMalformedSignature)
	}
	encoded := strings.Trim(strings.TrimSpace(sigValue), ":")
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not valid base64", ErrMalformedSignature)
	}
	sig.signature = decoded
	return sig, nil
}

// firstDictionaryMember extracts the first `label=value` member of a structured-field dictionary
// and returns the label and its verbatim value. This handles the common single-signature case.
func firstDictionaryMember(field string) (label, value string, err error) {
	field = strings.TrimSpace(field)
	if field == "" {
		return "", "", fmt.Errorf("%w: empty signature dictionary", ErrMalformedSignature)
	}
	member := splitOutsideQuotes(field, ',')[0]
	label, value, ok := strings.Cut(strings.TrimSpace(member), "=")
	if !ok {
		return "", "", fmt.Errorf("%w: malformed signature dictionary", ErrMalformedSignature)
	}
	return strings.TrimSpace(label), strings.TrimSpace(value), nil
}

func (s *parsedSignature) signingStringRFC9421(req *http.Request) (string, error) {
	lines := make([]string, 0, len(s.components)+1)
	for _, c := range s.components {
		var value string
		switch c {
		case "@method":
			value = strings.ToUpper(req.Method)
		case "@target-uri":
			value = "https://" + req.Host + req.URL.RequestURI()
		case "@authority":
			value = strings.ToLower(req.Host)
		case "@path":
			value = req.URL.EscapedPath()
		case "@query":
			value = "?" + req.URL.RawQuery
		default:
			if strings.HasPrefix(c, "@") {
				return "", fmt.Errorf("%w: unsupported derived component %q", ErrMalformedSignature, c)
			}
			value = strings.TrimSpace(req.Header.Get(c))
		}
		lines = append(lines, "\""+c+"\": "+value)
	}
	lines = append(lines, "\"@signature-params\": "+s.signatureParams)
	return strings.Join(lines, "\n"), nil
}
