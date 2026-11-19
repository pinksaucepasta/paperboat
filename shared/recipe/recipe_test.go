package recipe

import (
	"errors"
	"strings"
	"testing"
)

func TestParseExamples(t *testing.T) {
	ci := `// CI runner
{
  "tags": ["ci"],
  "max_machines": 20,
  "remove_after_offline": "30m",
  "env": {"vaults": ["canary-env"]},
  "preview": [{"port": 3000, "ttl": "2h"},], // trailing comma
}`
	r, err := Parse("ci.jsonc", []byte(ci))
	if err != nil {
		t.Fatal(err)
	}
	if r.Expires != "90d" || r.MaxMachines != 20 || r.Env == nil || !r.Env.Shell || r.Preview[0].TTL != "2h" || r.OfflineAfter().Minutes() != 30 {
		t.Fatalf("recipe = %+v", r)
	}
	again, err := Parse("ci.jsonc", r.Canonical())
	if err != nil || string(again.Canonical()) != string(r.Canonical()) {
		t.Fatalf("canonical round trip: %v\n%s\n%s", err, r.Canonical(), again.Canonical())
	}
	agent := `{
  "$schema": "https://example.test/schemas/recipe.json",
  "max_machines": 1,
  "remove_after_offline": "10m",
  "machine_lifetime": "8h",
  "reach": [{"dst": ["tag:ci"], "allow": ["exec", "managed_ssh", "files"]}],
  "tunnel": [{"port": 8123, "private": true}]
}`
	if r, err := Parse("agent.jsonc", []byte(agent)); err != nil || len(r.Reach) != 1 || r.Lifetime().Hours() != 8 || r.Tunnel[0].Port != 8123 {
		t.Fatalf("agent = %+v, %v", r, err)
	}
}

// Parse reads the caller's bytes without changing them (the CLI sends the file as it was written).
func TestParseLeavesTheSourceAlone(t *testing.T) {
	src := []byte("// CI\n{\"tags\": [\"ci\",], /* c */}\n")
	before := string(src)
	if _, err := Parse("ci.jsonc", src); err != nil || string(src) != before {
		t.Fatalf("err=%v source now %q", err, src)
	}
}

func TestParseErrors(t *testing.T) {
	for _, c := range []struct{ src, want string }{
		{"{\"tags\": [\"ci\"],\n\"max_machines\":\n}", `ci.jsonc:3:1:`},
		{"{\"tags\": [\"ci\"],\n\n  \"tag\": [\"x\"]}", `ci.jsonc:3:3: unknown key "tag". Did you mean "tags"?`},
		{"{\"envs\": {\"vaults\": [\"a\"]}}", `ci.jsonc:1:2: unknown key "envs". Did you mean "env"?`},
		{"{\"tags\": [],\n\"max_machines\": \"20\"}", `ci.jsonc:2:17: max_machines must be a whole number, like "max_machines": 20.`},
		{"{\"remove_after_offline\": \"1m\"}", `ci.jsonc:1:26: remove_after_offline must be a duration like "30m", "12h" or "7d" (5m to 30d).`},
		{"{\"expires\": \"400d\"}", `ci.jsonc:1:2: expires must be at most 365d. Rotate the recipe to extend it.`},
		{"{\"preview\": [{\"ttl\": \"2h\"}]}", `ci.jsonc:1:14: a preview needs "port", like "port": 3000.`},
		{"{\"tunnel\": [{\"port\": 1, \"from\": \"http://x\"}]}", `a tunnel needs "port" or "from", not both.`},
		{"{\"reach\": [{\"dst\": [\"tag:ci\"],\n\"allow\": [\"ssh\"]}]}", `ci.jsonc:2:10: allow has unknown action "ssh". Did you mean "managed_ssh"? Actions: pb policy schema`},
		{`{"env": {"shell": false}}`, `"env" needs "vaults"`},
		{`{"reach": [{"dst": ["tag:ci"], "allow": ["exec"], "ports": [22]}]}`, `ports apply only when allow is exactly`},
		{`{"tags": ["a"], "tags": ["b"]}`, `ci.jsonc:1:17: "tags" appears more than once.`},
		{`{"$schema": 1}`, `"$schema" must be a string`},
		{`{"tags": ["a"]`, `ci.jsonc:1:15:`},
		{`[]`, `a recipe is a JSON object`},
	} {
		_, err := Parse("ci.jsonc", []byte(c.src))
		var errs Errors
		if !errors.As(err, &errs) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: got %v, want %q", c.src, err, c.want)
		}
	}
	if _, err := NameFromFile("CI.jsonc"); err == nil {
		t.Fatal("uppercase name accepted")
	}
	if _, err := NameFromFile("ci.toml"); err == nil || !strings.Contains(err.Error(), "ends in .jsonc") {
		t.Fatalf("toml accepted: %v", err)
	}
	if n, err := NameFromFile("/x/ci.jsonc"); n != "ci" || err != nil {
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
