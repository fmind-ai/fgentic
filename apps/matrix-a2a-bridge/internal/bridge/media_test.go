package bridge

import (
	"strings"
	"testing"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/config"
)

func testMediaPolicy() mediaPolicy {
	return newMediaPolicy(config.Config{
		MediaMIMEAllowlist: []string{"text/csv", "application/json", "image/png"},
		MediaMaxBytes:      100,
		MediaMaxTotalBytes: 250,
	})
}

func TestMediaPolicyEnabledSwitch(t *testing.T) {
	if (mediaPolicy{}).enabled() {
		t.Fatal("zero-value policy must be disabled")
	}
	empty := newMediaPolicy(config.Config{MediaMaxBytes: 100})
	if empty.enabled() {
		t.Fatal("empty allowlist must disable the media path")
	}
	if !testMediaPolicy().enabled() {
		t.Fatal("configured policy must be enabled")
	}
}

func TestMediaPolicyAllowsNormalizesMIME(t *testing.T) {
	p := testMediaPolicy()
	for _, mime := range []string{"text/csv", "TEXT/CSV", "text/csv; charset=utf-8", "  text/csv  "} {
		if !p.allows(mime) {
			t.Errorf("allows(%q) = false, want true", mime)
		}
	}
	for _, mime := range []string{"text/html", "application/xml", "", "text", "image/svg+xml"} {
		if p.allows(mime) {
			t.Errorf("allows(%q) = true, want false", mime)
		}
	}
}

func TestMediaBudgetAdmitOrdersReasons(t *testing.T) {
	tests := []struct {
		name string
		mime string
		size int64
		want mediaReject
	}{
		{"disallowed", "text/html", 10, mediaRejectDisallowedType},
		{"empty", "text/csv", 0, mediaRejectEmpty},
		{"too_large", "text/csv", 101, mediaRejectTooLarge},
		{"ok", "text/csv", 100, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := testMediaPolicy().newBudget()
			reason, ok := b.admit(tt.mime, tt.size)
			if tt.want == "" {
				if !ok {
					t.Fatalf("admit rejected a valid file: %s", reason)
				}
				return
			}
			if ok || reason != tt.want {
				t.Fatalf("admit = (%q, %v), want (%q, false)", reason, ok, tt.want)
			}
		})
	}
}

func TestMediaBudgetEnforcesDelegationTotal(t *testing.T) {
	b := testMediaPolicy().newBudget() // per-file 100, total 250
	if _, ok := b.admit("text/csv", 100); !ok {
		t.Fatal("first 100-byte file must be admitted")
	}
	if _, ok := b.admit("text/csv", 100); !ok {
		t.Fatal("second 100-byte file must be admitted")
	}
	reason, ok := b.admit("text/csv", 100) // 300 > 250
	if ok || reason != mediaRejectBudgetExhausted {
		t.Fatalf("third file = (%q, %v), want delegation budget exhaustion", reason, ok)
	}
}

func TestMediaBudgetEnforcesFileCount(t *testing.T) {
	// A generous byte budget still caps the file count so tiny files cannot flood a room.
	p := newMediaPolicy(config.Config{
		MediaMIMEAllowlist: []string{"text/plain"},
		MediaMaxBytes:      10,
		MediaMaxTotalBytes: 100000,
	})
	b := p.newBudget()
	for i := 0; i < maxMediaFilesPerDelegation; i++ {
		if _, ok := b.admit("text/plain", 1); !ok {
			t.Fatalf("file %d must be admitted", i)
		}
	}
	reason, ok := b.admit("text/plain", 1)
	if ok || reason != mediaRejectTooMany {
		t.Fatalf("overflow file = (%q, %v), want too_many", reason, ok)
	}
}

func TestMediaPrecheckIgnoresUnknownDeclaredSize(t *testing.T) {
	p := testMediaPolicy()
	if !p.precheck("text/csv", 0) {
		t.Fatal("precheck must pass an allowlisted type with unknown declared size")
	}
	if p.precheck("text/csv", 101) {
		t.Fatal("precheck must reject an oversized declared size before download")
	}
	if p.precheck("text/html", 1) {
		t.Fatal("precheck must reject a disallowed type")
	}
}

func TestSanitizeFilename(t *testing.T) {
	tests := []struct {
		name, mime, want string
	}{
		{"report.csv", "text/csv", "report.csv"},
		{"../../etc/passwd", "text/plain", "....etcpasswd"},
		{"a\nb\tc", "text/plain", "abc"},
		{"", "text/csv", "file.csv"},
		{"   ", "application/json", "file.json"},
		{"", "application/octet-stream", "file"},
	}
	for _, tt := range tests {
		if got := sanitizeFilename(tt.name, tt.mime); got != tt.want {
			t.Errorf("sanitizeFilename(%q, %q) = %q, want %q", tt.name, tt.mime, got, tt.want)
		}
	}
	long := strings.Repeat("x", maxFilenameRunes+50)
	if got := sanitizeFilename(long, "text/plain"); len([]rune(got)) != maxFilenameRunes {
		t.Errorf("long filename not bounded: got %d runes", len([]rune(got)))
	}
}

func TestWithheldNotice(t *testing.T) {
	if withheldNotice(nil) != "" {
		t.Fatal("empty rejects must yield no notice")
	}
	notice := withheldNotice(map[mediaReject]int{mediaRejectDisallowedType: 2, mediaRejectTooLarge: 1})
	if !strings.Contains(notice, "3 attached file(s) withheld") {
		t.Errorf("notice missing total: %q", notice)
	}
	if !strings.Contains(notice, "2 disallowed_type") || !strings.Contains(notice, "1 too_large") {
		t.Errorf("notice missing per-reason counts: %q", notice)
	}
	// disallowed_type is ordered before too_large deterministically.
	if strings.Index(notice, "disallowed_type") > strings.Index(notice, "too_large") {
		t.Errorf("notice reasons out of deterministic order: %q", notice)
	}
}
