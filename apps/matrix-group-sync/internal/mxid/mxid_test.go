package mxid

import "testing"

func TestValidateLocalpart(t *testing.T) {
	valid := []string{"alice", "a.b_c", "user-1", "x=y", "a/b", "user+tag"}
	for _, v := range valid {
		if err := ValidateLocalpart(v); err != nil {
			t.Errorf("expected %q valid: %v", v, err)
		}
	}
	invalid := []string{"", " alice", "alice ", "Alice", "a b", "bad!", "a:b", "@a"}
	for _, v := range invalid {
		if err := ValidateLocalpart(v); err == nil {
			t.Errorf("expected %q invalid", v)
		}
	}
}

func TestFormat(t *testing.T) {
	got, err := Format("alice", "fgentic.localhost")
	if err != nil || got != "@alice:fgentic.localhost" {
		t.Fatalf("Format = %q, %v", got, err)
	}
	if _, err := Format("Bad!", "fgentic.localhost"); err == nil {
		t.Fatal("invalid localpart must error")
	}
	if _, err := Format("alice", ""); err == nil {
		t.Fatal("empty server must error")
	}
}

func TestParse(t *testing.T) {
	local, server, err := Parse("@alice:fgentic.localhost")
	if err != nil || local != "alice" || server != "fgentic.localhost" {
		t.Fatalf("Parse = %q %q %v", local, server, err)
	}
	for _, bad := range []string{"alice", "@alice", "@:server", "@alice:", "alice:server"} {
		if _, _, err := Parse(bad); err == nil {
			t.Errorf("expected %q to fail parsing", bad)
		}
	}
}

func TestIsLocal(t *testing.T) {
	if !IsLocal("@alice:fgentic.localhost", "fgentic.localhost") {
		t.Error("local MXID must be local")
	}
	if IsLocal("@alice:other.example", "fgentic.localhost") {
		t.Error("partner MXID must not be local (federation-safe)")
	}
	if IsLocal("alice", "fgentic.localhost") {
		t.Error("a bare localpart must never be treated as local")
	}
}

func TestIsLocalGhost(t *testing.T) {
	if !IsLocalGhost("@agent-k8s:fgentic.localhost", "agent-", "fgentic.localhost") {
		t.Error("local ghost must match")
	}
	if IsLocalGhost("@agent-k8s:other.example", "agent-", "fgentic.localhost") {
		t.Error("a remote MXID sharing the prefix must NOT be treated as a local ghost")
	}
	if IsLocalGhost("@alice:fgentic.localhost", "agent-", "fgentic.localhost") {
		t.Error("a human must not be a ghost")
	}
}
