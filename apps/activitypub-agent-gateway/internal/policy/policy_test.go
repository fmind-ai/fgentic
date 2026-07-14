package policy

import "testing"

const validPolicy = `{
  "version": 1,
  "allowed_domains": ["mastodon.example", "gts.example"],
  "allowed_actors": ["https://other.example/users/trusted"]
}`

func TestParseAndAllows(t *testing.T) {
	p, err := Parse([]byte(validPolicy))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	allow := []string{
		"https://mastodon.example/users/bob",
		"https://GTS.example/users/alice", // host match is case-insensitive
		"https://other.example/users/trusted",
	}
	for _, a := range allow {
		if !p.Allows(a) {
			t.Errorf("Allows(%q) = false, want true", a)
		}
	}
	deny := []string{
		"https://evil.example/users/mallory",
		"https://other.example/users/someone-else", // domain not allowed, only the exact actor is
		"not a url",
		"",
	}
	for _, d := range deny {
		if p.Allows(d) {
			t.Errorf("Allows(%q) = true, want false", d)
		}
	}
	if p.Version() != 1 {
		t.Errorf("Version = %d", p.Version())
	}
	if p.Digest() != "v1/d2/a1" {
		t.Errorf("Digest = %q", p.Digest())
	}
}

func TestParseEmptyPolicyDeniesAll(t *testing.T) {
	p, err := Parse([]byte(`{"version":1}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if p.Allows("https://mastodon.example/users/bob") {
		t.Errorf("empty policy must deny everything")
	}
}

func TestParseRejects(t *testing.T) {
	cases := map[string]string{
		"unknown field":    `{"version":1,"bogus":true}`,
		"bad version":      `{"version":2}`,
		"trailing content": `{"version":1} extra`,
		"not json":         `{`,
		"dup domain":       `{"version":1,"allowed_domains":["a.example","a.example"]}`,
		"domain scheme":    `{"version":1,"allowed_domains":["https://a.example"]}`,
		"domain port":      `{"version":1,"allowed_domains":["a.example:443"]}`,
		"domain path":      `{"version":1,"allowed_domains":["a.example/users"]}`,
		"actor http":       `{"version":1,"allowed_actors":["http://a.example/u"]}`,
		"actor relative":   `{"version":1,"allowed_actors":["/users/x"]}`,
		"dup actor":        `{"version":1,"allowed_actors":["https://a.example/u","https://a.example/u"]}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse([]byte(body)); err == nil {
				t.Errorf("expected parse error")
			}
		})
	}
}
