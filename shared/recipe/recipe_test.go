package recipe

import (
	"errors"
	"strings"
	"testing"
)

func TestParseExamples(t *testing.T) {
	ci := `# CI runner
tags = ["ci"]
max_machines = 20
remove_after_offline = "30m"

[env]
vaults = ["canary-env"]

[[preview]]
port = 3000
ttl = "2h"
`
	r, err := Parse("ci.toml", []byte(ci))
	if err != nil {
		t.Fatal(err)
	}
	if r.Expires != "90d" || r.MaxMachines != 20 || r.Env == nil || !r.Env.Shell || r.Preview[0].TTL != "2h" || r.OfflineAfter().Minutes() != 30 {
		t.Fatalf("recipe = %+v", r)
	}
	again, err := ParseJSON("ci.json", r.Canonical())
	if err != nil || string(again.Canonical()) != string(r.Canonical()) {
		t.Fatalf("canonical round trip: %v\n%s\n%s", err, r.Canonical(), again.Canonical())
	}
	agent := `max_machines = 1
remove_after_offline = "10m"
machine_lifetime = "8h"

[[reach]]
dst = ["tag:ci"]
allow = ["exec", "managed_ssh", "files"]

[[tunnel]]
port = 8123
private = true
`
	if r, err := Parse("agent.toml", []byte(agent)); err != nil || len(r.Reach) != 1 || r.Lifetime().Hours() != 8 || r.Tunnel[0].Port != 8123 {
		t.Fatalf("agent = %+v, %v", r, err)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"tags = [\"ci\"]\nmax_machines =\n", `ci.toml:2:`},
		{"tags = [\"ci\"]\n\ntag = [\"x\"]\n", `ci.toml:3:1: unknown key "tag". Did you mean "tags"?`},
		{"[envs]\nvaults = [\"a\"]\n", `ci.toml:1:1: unknown section [envs]. Did you mean [env]?`},
		{"tags = []\nmax_machines = \"20\"\n", `ci.toml:2:16: max_machines must be a whole number, like max_machines = 20.`},
		{"remove_after_offline = \"1m\"\n", `ci.toml:1:24: remove_after_offline must be a duration like "30m", "12h" or "7d" (5m to 30d).`},
		{"expires = \"400d\"\n", `ci.toml:1:1: expires must be at most 365d. Rotate the recipe to extend it.`},
		{"[[preview]]\nttl = \"2h\"\n", `ci.toml:1:1: [[preview]] needs port, like port = 3000.`},
		{"[[tunnel]]\nport = 1\nfrom = \"http://x\"\n", `ci.toml:1:1: [[tunnel]] needs port or from, not both.`},
		{"[[reach]]\ndst = [\"tag:ci\"]\nallow = [\"ssh\"]\n", `ci.toml:3:9: allow has unknown action "ssh". Did you mean "managed_ssh"? Actions: pb policy schema`},
		{"[env]\nshell = false\n", `[env] needs vaults`},
		{"[[reach]]\ndst = [\"tag:ci\"]\nallow = [\"exec\"]\nports = [22]\n", `ports apply only when allow is exactly`},
	} {
		_, err := Parse("ci.toml", []byte(c.src))
		var errs Errors
		if !errors.As(err, &errs) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.src, err, c.want)
		}
	}
	if _, err := ParseJSON("r.json", []byte(`{"expires":"90d","extra":1}`)); err == nil || !strings.Contains(err.Error(), `/extra: unknown key "extra"`) {
		t.Fatalf("strict JSON: %v", err)
	}
	if _, err := NameFromFile("CI.toml"); err == nil {
		t.Fatal("uppercase name accepted")
	}
	if n, err := NameFromFile("/x/ci.toml"); n != "ci" || err != nil {
		t.Fatal(n, err)
	}
}

func TestToken(t *testing.T) {
	tok, err := NewToken("")
	if err != nil {
		t.Fatal(err)
	}
	s := tok.String()
	if len(s) != TokenLength || TokenLength != 67 || !strings.HasPrefix(s, "pbr_") {
		t.Fatalf("token %d chars", len(s))
	}
	back, err := ParseToken(" " + s + "\n")
	if err != nil || back.String() != s || string(back.Auth()) != string(tok.Auth()) {
		t.Fatalf("parse: %v", err)
	}
	if string(tok.Auth()) == string(tok.PrivateKey()) || len(tok.PublicKey()) != 32 || len(AuthHash(tok.Auth())) != 32 {
		t.Fatal("derived values")
	}
	var shape *ShapeError
	if _, err := ParseToken(s[:41]); !errors.As(err, &shape) || shape.Length != 41 {
		t.Fatalf("short token: %v", err)
	}
	if _, err := ParseToken(strings.ToUpper(s)); err == nil {
		t.Fatal("uppercase token accepted")
	}
}
