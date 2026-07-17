// Package replyscan detects secret/credential material in an agent reply before the bridge
// projects it into a Matrix room. The reply->room boundary is the one control point only the
// bridge owns: the agent's answer transits the bridge before it is posted, so this is the last
// place to catch a secret (an API key, private key, or connection-string password) that the model
// or a tool inadvertently emitted. In a federated room this is load-bearing — a leaked credential
// replicated to a partner homeserver cannot be retracted (docs/federation.md §8).
//
// The package is deliberately self-contained: it composes a curated, high-precision ruleset over
// well-known public credential formats (the same structured, prefix-anchored approach gitleaks
// takes) rather than importing a secret-scanning engine or a new dependency, and it never surfaces
// the matched value. Callers receive only a masked rendering plus a bounded, content-free match
// count and rule-class set. No matched span, secret, or reply body ever leaves this package.
package replyscan

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// redactionPlaceholder is the bounded, value-free token substituted for a matched span. It names
// the rule class only (a fixed identifier), never any fragment of the detected secret.
func redactionPlaceholder(class string) string {
	return "‹redacted:" + class + "›"
}

// rule binds a fixed rule identifier and coarse class to a compiled, structured pattern. Every
// pattern is anchored on a distinctive prefix or delimiter so the detector stays high-precision:
// masking a false positive silently corrupts a legitimate reply, so we prefer misses over noise
// on unstructured text and rely on the annotate/block modes for the residual risk.
type rule struct {
	id    string
	class string
	re    *regexp.Regexp
}

// rules is the curated detector set. Patterns cover provider-prefixed tokens, PEM private-key
// blocks, JWTs, and URL-embedded connection-string passwords. Ordering is irrelevant: every rule
// runs and overlapping spans are merged by first-match-wins during masking.
var rules = []rule{
	{"aws-access-key-id", "aws-access-key-id", regexp.MustCompile(`\b(?:AKIA|ASIA|AGPA|AIDA|AROA|AIPA|ANPA|ANVA|AKIB)[0-9A-Z]{16}\b`)},
	{"github-token", "github-token", regexp.MustCompile(`\bgh[pousr]_[0-9A-Za-z]{36}\b`)},
	{"github-fine-grained-pat", "github-token", regexp.MustCompile(`\bgithub_pat_[0-9A-Za-z_]{82}\b`)},
	{"gitlab-pat", "gitlab-token", regexp.MustCompile(`\bglpat-[0-9A-Za-z_-]{20}\b`)},
	{"slack-token", "slack-token", regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{10,48}\b`)},
	{"google-api-key", "google-api-key", regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{"stripe-key", "stripe-key", regexp.MustCompile(`\b[sr]k_(?:live|test)_[0-9A-Za-z]{24,}\b`)},
	{"openai-key", "openai-key", regexp.MustCompile(`\bsk-(?:proj-)?[A-Za-z0-9]{32,}\b`)},
	{"npm-token", "npm-token", regexp.MustCompile(`\bnpm_[0-9A-Za-z]{36}\b`)},
	{"jwt", "jwt", regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`)},
	{"private-key", "private-key", regexp.MustCompile(`-----BEGIN (?:RSA |EC |DSA |OPENSSH |PGP )?PRIVATE KEY-----`)},
	{"connection-string-password", "connection-string-password", regexp.MustCompile(`://[^:@/\s]+:[^@/\s]{4,}@`)},
}

// span is a half-open [start,end) byte range with the class that matched it. Spans never leave the
// package; only counts and classes are exported.
type span struct {
	start int
	end   int
	class string
}

// Result is the bounded, content-free outcome of scanning one text. Masked is the reply with every
// detected span replaced by a value-free placeholder; when Count is zero Masked is byte-identical
// to the input, so a clean reply is delivered unchanged. Count and Classes carry only the number of
// masked spans and the sorted, de-duplicated set of matched rule classes — never any secret bytes.
type Result struct {
	Masked  string
	Count   int
	Classes []string
}

// Clean reports whether the scan found nothing, i.e. the text may be delivered unchanged.
func (r Result) Clean() bool { return r.Count == 0 }

// Sanitize scans text and returns a masked copy plus a content-free match summary. Overlapping
// matches (e.g. a JWT embedded in a larger structure) are merged first-match-wins so no placeholder
// nests inside another and the byte offsets stay valid while rewriting.
func Sanitize(text string) Result {
	if text == "" {
		return Result{Masked: text}
	}
	var spans []span
	for _, r := range rules {
		for _, loc := range r.re.FindAllStringIndex(text, -1) {
			spans = append(spans, span{start: loc[0], end: loc[1], class: r.class})
		}
	}
	if len(spans) == 0 {
		return Result{Masked: text}
	}
	// Sort by start, then by widest span, and keep only non-overlapping spans so masking rewrites
	// each region exactly once with a deterministic class.
	sort.Slice(spans, func(i, j int) bool {
		if spans[i].start != spans[j].start {
			return spans[i].start < spans[j].start
		}
		return spans[i].end > spans[j].end
	})
	var (
		masked  strings.Builder
		classes = map[string]struct{}{}
		cursor  int
		count   int
	)
	for _, s := range spans {
		if s.start < cursor {
			continue // covered by an already-masked span
		}
		masked.WriteString(text[cursor:s.start])
		masked.WriteString(redactionPlaceholder(s.class))
		classes[s.class] = struct{}{}
		cursor = s.end
		count++
	}
	masked.WriteString(text[cursor:])
	return Result{Masked: masked.String(), Count: count, Classes: sortedKeys(classes)}
}

// SanitizeAll scans several reply fragments (the reply text plus each rendered data block and link)
// as one logical body. It returns the sanitized fragments in order alongside the aggregate match
// count and merged class set, so a caller can rebuild the reply with every fragment masked while
// reporting a single bounded summary. A nil/empty input yields a clean, empty Result.
func SanitizeAll(fragments []string) (masked []string, summary Result) {
	masked = make([]string, len(fragments))
	classes := map[string]struct{}{}
	total := 0
	for i, fragment := range fragments {
		res := Sanitize(fragment)
		masked[i] = res.Masked
		total += res.Count
		for _, c := range res.Classes {
			classes[c] = struct{}{}
		}
	}
	return masked, Result{Count: total, Classes: sortedKeys(classes)}
}

func sortedKeys(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// TransparencyNotice renders the bounded, value-free line appended in annotate mode. It names only
// the count and the sorted rule classes — never a matched value — so a room learns a credential was
// caught and masked without the secret entering the timeline.
func TransparencyNotice(summary Result) string {
	if summary.Count == 0 {
		return ""
	}
	return fmt.Sprintf(
		"⚠️ %d possible credential(s) detected and masked (%s).",
		summary.Count, strings.Join(summary.Classes, ", "),
	)
}
