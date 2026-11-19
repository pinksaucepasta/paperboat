package recipe

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pinksaucepasta/paperboat/shared/schemagen/schematest"
	"github.com/tailscale/hujson"
)

// schema.json is generated from the Go types (go test . -run TestSchemaIsGenerated -update).
func TestSchemaIsGenerated(t *testing.T) {
	schematest.Generated(t, "schema.json", Recipe{}, SchemaOptions())
}

func TestSchemaIsDocumented(t *testing.T) { schematest.Documented(t, Schema) }

// A duration pattern accepts exactly the durations ParseDuration accepts within its limits.
func TestDurationPatternMatchesTheParser(t *testing.T) {
	for _, c := range []struct{ from, to time.Duration }{{time.Minute, MaxExpires}, {MinOffline, MaxOffline}, {MinLifetime, MaxLifetime}, {time.Minute, MaxTTL}} {
		pattern := regexp.MustCompile(DurationPattern(c.from, c.to))
		for _, unit := range []string{"m", "h", "d"} {
			for n := 0; n <= 100000; n++ {
				text := strconv.Itoa(n) + unit
				d, ok := ParseDuration(text)
				want := ok && d >= c.from && d <= c.to
				if pattern.MatchString(text) != want {
					t.Fatalf("%s in [%v, %v]: pattern %v, parser %v", text, c.from, c.to, !want, want)
				}
			}
		}
	}
}

// The schema refuses what Parse refuses, with the server's message, and accepts what it accepts.
func TestSchemaAgreesWithParse(t *testing.T) {
	valid := []string{
		`{}`,
		`{"$schema": "https://example.test/schemas/recipe.json", "tags": ["ci", "a.b_c"], "expires": "365d", "max_machines": 20, // comment
		  "remove_after_offline": "30m", "machine_lifetime": "8h",
		  "reach": [{"dst": ["tag:ci"], "allow": ["exec", "files"]}, {"dst": ["machine:db"], "allow": ["private_access"], "ports": [5432, "8000-8100"]}],
		  "env": {"vaults": ["canary-env"], "shell": false}, "config": {"repository": "dotfiles"},
		  "preview": [{"port": 3000, "ttl": "2h", "private": true, "host_header": "localhost:3000", "domain": "p.example.test"}],
		  "tunnel": [{"name": "dash", "port": 8123, "duration": "7d"}, {"from": "http://localhost:80", "team": true}],}`,
	}
	invalid := []struct{ doc, want string }{
		{`{"tag": ["x"]}`, `unknown key "tag". Did you mean "tags"?`},
		{`{"tags": ["Bad Tag"]}`, `is not valid`},
		{`{"expires": "400d"}`, `expires must be at most 365d`},
		{`{"expires": "0m"}`, `expires must be a duration`},
		{`{"expires": 90}`, `expires must be a duration`},
		{`{"max_machines": 0}`, `max_machines must be a whole number`},
		{`{"max_machines": "20"}`, `max_machines must be a whole number`},
		{`{"remove_after_offline": "1m"}`, `remove_after_offline must be a duration`},
		{`{"machine_lifetime": "366d"}`, `machine_lifetime must be a duration`},
		{`{"reach": [{"dst": ["tag:ci"]}]}`, `a reach entry needs "allow"`},
		{`{"reach": [{"allow": ["exec"]}]}`, `a reach entry needs "dst"`},
		{`{"reach": [{"dst": ["tag:ci"], "allow": ["ssh"]}]}`, `allow has unknown action "ssh". Did you mean "managed_ssh"?`},
		{`{"reach": [{"dst": ["tag:ci"], "allow": ["exec"], "ports": [22]}]}`, `ports apply only when allow is exactly`},
		{`{"reach": [{"dst": ["tag:ci"], "allow": ["private_access"], "ports": [70000]}]}`, `ports is a list of ports or ranges`},
		{`{"env": {}}`, `"env" needs "vaults"`},
		{`{"env": {"vaults": ["Bad_Vault"]}}`, `is not valid`},
		{`{"env": {"vaults": ["a"], "shell": "yes"}}`, `shell must be true or false`},
		{`{"config": {}}`, `"config" needs "repository"`},
		{`{"preview": [{"ttl": "2h"}]}`, `a preview needs "port"`},
		{`{"preview": [{"port": 70000}]}`, `port must be a port number`},
		{`{"preview": [{"port": 80, "ttl": "25h"}]}`, `ttl must be a duration`},
		{`{"tunnel": [{"port": 1, "from": "http://x"}]}`, `a tunnel needs "port" or "from", not both`},
		{`{"tunnel": [{}]}`, `a tunnel needs "port" or "from", not both`},
		{`{"tunnel": [{"port": 1, "name": "Bad Name"}]}`, `a tunnel name is lowercase letters`},
		{`{"preview": [{"port": 80, "extra": 1}]}`, `unknown key "extra"`},
	}
	for _, doc := range valid {
		if problems := checkAgainstSchema(t, doc); problems != nil {
			t.Errorf("schema refuses a valid recipe: %v\n%s", problems, doc)
		}
		if _, err := Parse("ci.jsonc", []byte(doc)); err != nil {
			t.Errorf("Parse refuses the example: %v\n%s", err, doc)
		}
	}
	for _, c := range invalid {
		if problems := checkAgainstSchema(t, c.doc); problems == nil {
			t.Errorf("schema accepts %s, which the server refuses with %q", c.doc, c.want)
		}
		if _, err := Parse("ci.jsonc", []byte(c.doc)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: server says %v, want %q", c.doc, err, c.want)
		}
	}
}

// The schema's top-level examples are valid recipes.
func TestSchemaExamplesParse(t *testing.T) {
	var s struct{ Examples []json.RawMessage }
	if err := json.Unmarshal(Schema, &s); err != nil || len(s.Examples) == 0 {
		t.Fatal(err, len(s.Examples))
	}
	for _, e := range s.Examples {
		if _, err := Parse("x.jsonc", e); err != nil {
			t.Errorf("%s: %v", e, err)
		}
	}
}

func checkAgainstSchema(t *testing.T, doc string) []string {
	t.Helper()
	standard, err := hujson.Standardize([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return schematest.Validate(t, Schema, standard)
}
