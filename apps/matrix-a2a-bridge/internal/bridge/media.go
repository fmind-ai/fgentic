package bridge

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/fmind/matrix-a2a-bridge/internal/config"
)

// mediaReject is a bounded, content-free reason a file was refused by the media policy (#115). It is
// safe to surface in a room notice and in the delegation audit: it names the failing rule, never the
// file's bytes, name, or provenance.
type mediaReject string

const (
	mediaRejectDisallowedType  mediaReject = "disallowed_type"
	mediaRejectTooLarge        mediaReject = "too_large"
	mediaRejectBudgetExhausted mediaReject = "delegation_budget_exhausted"
	mediaRejectEmpty           mediaReject = "empty"
	mediaRejectEncrypted       mediaReject = "encrypted_not_supported"
	// mediaRejectRemoteOptOut marks bytes withheld because the remote mapping did not opt in with
	// allowMedia: true — the org boundary stays closed to files in both directions by default (#115).
	mediaRejectRemoteOptOut mediaReject = "remote_media_not_allowed"
	// mediaRejectTooMany marks files dropped because one delegation exceeded the per-reply file count,
	// a flood guard independent of the byte budget (D7).
	mediaRejectTooMany mediaReject = "too_many"

	// maxMediaFilesPerDelegation caps how many files one delegation may move in a single direction, so
	// a burst of tiny artifacts cannot spray a room with events even while under the byte budget.
	maxMediaFilesPerDelegation = 8

	// maxFilenameRunes bounds a sanitized filename so an agent- or room-supplied name cannot bloat a
	// Matrix event or smuggle control characters into a client's rendering.
	maxFilenameRunes = 120
)

// mediaPolicy is the deployment's file gate (#115): an exact-match MIME allowlist plus a per-file
// and per-delegation byte cap. It is the single authority for whether any file — inbound from a
// room or outbound from an agent — may cross the bridge, so both directions fail closed identically.
// A zero-value (or empty-allowlist) policy disables the media path entirely.
type mediaPolicy struct {
	allowed  map[string]struct{}
	maxBytes int64
	maxTotal int64
}

// newMediaPolicy builds the gate from validated config. The allowlist is normalized to lower case so
// matching is case-insensitive without allocating on every check.
func newMediaPolicy(cfg config.Config) mediaPolicy {
	allowed := make(map[string]struct{}, len(cfg.MediaMIMEAllowlist))
	for _, mime := range cfg.MediaMIMEAllowlist {
		if norm := normalizeMIME(mime); norm != "" {
			allowed[norm] = struct{}{}
		}
	}
	return mediaPolicy{allowed: allowed, maxBytes: cfg.MediaMaxBytes, maxTotal: cfg.MediaMaxTotalBytes}
}

// enabled reports whether any file may cross the bridge. An operator disables the whole media path by
// leaving the MIME allowlist empty.
func (p mediaPolicy) enabled() bool {
	return len(p.allowed) > 0 && p.maxBytes > 0
}

// allows reports whether a MIME type is on the exact allowlist. Parameters (e.g. "; charset=utf-8")
// and case are ignored so "text/csv" and "text/csv; charset=utf-8" are treated identically.
func (p mediaPolicy) allows(mime string) bool {
	if !p.enabled() {
		return false
	}
	_, ok := p.allowed[normalizeMIME(mime)]
	return ok
}

// sniffsAsHTML reports whether the actual bytes are detected as HTML, regardless of the declared MIME
// type. The allowlist matches only the self-declared type, which is untrusted; this is a targeted
// defense-in-depth against smuggling an HTML/stored-XSS payload past the allowlist by mislabeling it
// (#115). None of the allowlisted document/image types sniff as text/html, so it rejects a disguised
// HTML file in either direction without false-positives on real CSV/JSON/PDF/PNG. It does not attempt
// to verify every declared type against its content (unreliable for text formats); non-HTML
// mislabeling remains a documented residual risk handled by treating downloaded files as untrusted.
func sniffsAsHTML(data []byte) bool {
	return strings.HasPrefix(http.DetectContentType(data), "text/html")
}

// normalizeMIME lower-cases a media type and drops any parameters after the first ';', trimming
// surrounding whitespace. It returns "" for a value with no type/subtype so a blank never matches.
func normalizeMIME(mime string) string {
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	mime = strings.ToLower(strings.TrimSpace(mime))
	if !strings.Contains(mime, "/") {
		return ""
	}
	return mime
}

// precheck reports whether a file's DECLARED type and size pass the policy before its bytes are
// fetched, so an oversized or disallowed inbound file is refused without ever downloading it. A
// declared size of 0 (unknown) is not rejected here — the real size is enforced by admit after the
// download. It does not touch the delegation budget.
func (p mediaPolicy) precheck(mime string, declaredSize int64) bool {
	return p.allows(mime) && declaredSize <= p.maxBytes
}

// mediaBudget tracks the summed accepted bytes and file count of one delegation in a single direction
// so the per-delegation total-byte and file-count caps hold across many files, independent of the
// per-file cap (#115, D7). It is not safe for concurrent use: one delegation admits its files
// sequentially.
type mediaBudget struct {
	policy mediaPolicy
	spent  int64
	count  int
}

func (p mediaPolicy) newBudget() *mediaBudget {
	return &mediaBudget{policy: p}
}

// admit applies the full policy to one file of the given MIME type and size and, on success, charges
// its bytes against the delegation total. It returns a content-free reject reason otherwise. A file
// is admitted only if the path is enabled, the type is allowlisted, it is non-empty and within the
// per-file cap, and it still fits the per-delegation budget — checked in that order so the reason is
// the most specific failing rule.
func (b *mediaBudget) admit(mime string, size int64) (mediaReject, bool) {
	if !b.policy.enabled() {
		return mediaRejectDisallowedType, false
	}
	if size <= 0 {
		return mediaRejectEmpty, false
	}
	if !b.policy.allows(mime) {
		return mediaRejectDisallowedType, false
	}
	if size > b.policy.maxBytes {
		return mediaRejectTooLarge, false
	}
	if b.count >= maxMediaFilesPerDelegation {
		return mediaRejectTooMany, false
	}
	if b.policy.maxTotal > 0 && b.spent+size > b.policy.maxTotal {
		return mediaRejectBudgetExhausted, false
	}
	b.spent += size
	b.count++
	return "", true
}

// withheldNotice renders a bounded, content-free suffix summarizing how many files a delegation
// dropped and why, for appending to the agent's room reply so a refusal is never silent (#115). It
// returns "" when nothing was withheld. Reasons are aggregated by count, never listing file names.
func withheldNotice(rejects map[mediaReject]int) string {
	if len(rejects) == 0 {
		return ""
	}
	total := 0
	for _, n := range rejects {
		total += n
	}
	// Deterministic order so the notice and its tests are stable.
	var parts []string
	for _, reason := range []mediaReject{
		mediaRejectDisallowedType, mediaRejectTooLarge, mediaRejectBudgetExhausted,
		mediaRejectTooMany, mediaRejectRemoteOptOut, mediaRejectEncrypted, mediaRejectEmpty,
	} {
		if n := rejects[reason]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, reason))
		}
	}
	return fmt.Sprintf("\n\n⚠️ %d attached file(s) withheld by media policy (%s).", total, strings.Join(parts, ", "))
}

// sanitizeFilename turns an untrusted agent- or room-supplied file name into a safe Matrix display
// name: control characters and path separators are dropped, length is bounded, and an empty result
// falls back to a neutral name with an extension guessed from the MIME type. It never trusts the
// input to be a well-formed or non-malicious file name.
func sanitizeFilename(name, mime string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '/' || r == '\\' {
			return -1
		}
		return r
	}, name)
	cleaned = strings.TrimSpace(cleaned)
	if runes := []rune(cleaned); len(runes) > maxFilenameRunes {
		cleaned = string(runes[:maxFilenameRunes])
	}
	if cleaned == "" {
		return "file" + extensionForMIME(mime)
	}
	return cleaned
}

// extensionForMIME maps a handful of allowlisted MIME types to a conventional extension for a
// fallback file name. It is best-effort cosmetic only — the MIME type, not the extension, governs
// policy — so an unknown type simply yields no extension.
func extensionForMIME(mime string) string {
	switch normalizeMIME(mime) {
	case "text/csv":
		return ".csv"
	case "text/plain":
		return ".txt"
	case "text/markdown":
		return ".md"
	case "application/json":
		return ".json"
	case "application/pdf":
		return ".pdf"
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	default:
		return ""
	}
}
