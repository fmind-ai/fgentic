package replyscan

import (
	"strings"
	"testing"
)

func TestSanitizeMasksKnownCredentialFormats(t *testing.T) {
	// Every value below is a synthetic, structurally-valid but non-live example credential. Each is
	// assembled from fragments at runtime (prefix + body) so no full token literal ever appears in the
	// committed source — a secret-scanner's own fixtures must not trip gitleaks or GitHub push
	// protection, and no fragment alone matches a provider detector.
	cases := []struct {
		name   string
		secret string
		class  string
	}{
		{"aws access key id", "AKIA" + "IOSFODNN7EXAMPLE", "aws-access-key-id"},
		{"github token", "ghp_" + "1234567890abcdefghijklmnopqrstuvwxyz", "github-token"},
		{"gitlab pat", "glpat-" + "ABCDEFGHIJ1234567890", "gitlab-token"},
		{"slack token", "xoxb-" + "1234567890-abcdefghijklmnop", "slack-token"},
		{"google api key", "AIza" + "SyA1234567890abcdefghijklmnopqrstuv", "google-api-key"},
		{"stripe key", "sk_live_" + "0123456789abcdefABCDEFGH", "stripe-key"},
		{"npm token", "npm_" + "1234567890abcdefghijklmnopqrstuvwxyz", "npm-token"},
		{"jwt", "eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0." + "dozjgNryP4J3jVmNHl0w5N", "jwt"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text := "here is the value " + tc.secret + " keep it safe"
			got := Sanitize(text)
			if got.Count != 1 {
				t.Fatalf("Count = %d, want 1", got.Count)
			}
			if strings.Contains(got.Masked, tc.secret) {
				t.Fatalf("masked output still contains the secret: %q", got.Masked)
			}
			if len(got.Classes) != 1 || got.Classes[0] != tc.class {
				t.Fatalf("Classes = %v, want [%s]", got.Classes, tc.class)
			}
			if !strings.Contains(got.Masked, redactionPlaceholder(tc.class)) {
				t.Fatalf("masked output missing placeholder: %q", got.Masked)
			}
		})
	}
}

func TestSanitizeMasksPrivateKeyBlock(t *testing.T) {
	text := "-----BEGIN " + "RSA PRIVATE KEY-----\nMIIEvQ...\n-----END RSA PRIVATE KEY-----"
	got := Sanitize(text)
	if got.Count == 0 {
		t.Fatal("expected the private-key header to be detected")
	}
	if strings.Contains(got.Masked, "BEGIN RSA PRIVATE KEY") {
		t.Fatalf("private-key header not masked: %q", got.Masked)
	}
}

func TestSanitizeMasksConnectionStringPassword(t *testing.T) {
	text := "connect via postgres://svc:sup3rs3cret@db.internal:5432/app"
	got := Sanitize(text)
	if got.Count != 1 {
		t.Fatalf("Count = %d, want 1", got.Count)
	}
	if strings.Contains(got.Masked, "sup3rs3cret") {
		t.Fatalf("connection-string password leaked: %q", got.Masked)
	}
	if !strings.Contains(got.Masked, "db.internal") {
		t.Fatalf("host should survive redaction: %q", got.Masked)
	}
}

func TestSanitizeCleanReplyIsByteIdentical(t *testing.T) {
	text := "The pod is healthy; restart it with kubectl rollout restart deploy/api."
	got := Sanitize(text)
	if !got.Clean() {
		t.Fatalf("clean text flagged: %+v", got)
	}
	if got.Masked != text {
		t.Fatalf("clean text was altered: %q != %q", got.Masked, text)
	}
	if got.Classes != nil {
		t.Fatalf("Classes should be nil for a clean reply, got %v", got.Classes)
	}
}

func TestSanitizeDeduplicatesAndSortsClasses(t *testing.T) {
	text := "ghp_" + "1234567890abcdefghijklmnopqrstuvwxyz and " +
		"AKIA" + "IOSFODNN7EXAMPLE and ghp_" + "zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"
	got := Sanitize(text)
	if got.Count != 3 {
		t.Fatalf("Count = %d, want 3", got.Count)
	}
	if len(got.Classes) != 2 || got.Classes[0] != "aws-access-key-id" || got.Classes[1] != "github-token" {
		t.Fatalf("Classes = %v, want [aws-access-key-id github-token]", got.Classes)
	}
}

func TestSanitizeAllAggregatesFragments(t *testing.T) {
	fragments := []string{
		"summary text",
		"ghp_" + "1234567890abcdefghijklmnopqrstuvwxyz",
		"https://user:" + "hunter2xyz@host/path",
	}
	masked, summary := SanitizeAll(fragments)
	if summary.Count != 2 {
		t.Fatalf("Count = %d, want 2", summary.Count)
	}
	if masked[0] != "summary text" {
		t.Fatalf("clean fragment altered: %q", masked[0])
	}
	if strings.Contains(masked[1], "ghp_") || strings.Contains(masked[2], "hunter2xyz") {
		t.Fatalf("secret leaked through SanitizeAll: %v", masked)
	}
	if len(summary.Classes) != 2 {
		t.Fatalf("Classes = %v, want two classes", summary.Classes)
	}
}

func TestTransparencyNotice(t *testing.T) {
	if TransparencyNotice(Result{}) != "" {
		t.Fatal("clean result must yield no notice")
	}
	notice := TransparencyNotice(Result{Count: 2, Classes: []string{"github-token", "jwt"}})
	if !strings.Contains(notice, "2 possible credential") ||
		!strings.Contains(notice, "github-token") || !strings.Contains(notice, "jwt") {
		t.Fatalf("unexpected notice: %q", notice)
	}
}
