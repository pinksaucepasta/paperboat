package policy

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/schemagen/schematest"
)

// schema.json is generated from the Go types (go test . -run TestSchemaIsGenerated -update).
func TestSchemaIsGenerated(t *testing.T) {
	schematest.Generated(t, "schema.json", Document{}, SchemaOptions())
}

func TestSchemaIsDocumented(t *testing.T) { schematest.Documented(t, Schema) }

// The schema refuses what Validate refuses, with the server's code, and accepts what it accepts.
func TestSchemaAgreesWithTheServer(t *testing.T) {
	valid := []string{
		`{}`,
		`{"$schema": "https://example.test/schemas/policy.json", "allow_private_access": true}`,
		`{"groups": {"group:ops": ["user:ana@example.test"]}, "tags": {"tag:db": ["machine:db1"]},
		  "rules": [{"src": ["group:ops", "role:admin"], "dst": ["tag:db"], "allow": ["terminal", "exec"], "note": "x"},
		            {"src": ["*"], "dst": ["machine:db1"], "allow": ["private_access"], "ports": [5432, "8000-8100"]},
		            {"src": ["user:bo@example.test"], "dst": ["machine:*"], "deny": ["files"]}], // comment
		  "tests": [{"src": "user:ana@example.test", "dst": "machine:db1", "allow": ["exec"], "deny": ["files"]}],
		  "config_sync": [{"dst": ["tag:db", "machine:*"], "repository": "https://git.example.test/c.git", "branch": "main"}]}`,
	}
	invalid := []struct{ doc, code string }{
		{`{"extra": 1}`, "unknown_field"},
		{`{"rules": {}}`, "wrong_type"},
		{`{"rules": [{"dst": ["machine:*"], "allow": ["exec"]}]}`, "required"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"]}]}`, "allow_or_deny"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["exec"], "deny": ["files"]}]}`, "allow_or_deny"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["exec"], "extra": 1}]}`, "unknown_field"},
		{`{"rules": [{"src": ["bob"], "dst": ["machine:*"], "allow": ["exec"]}]}`, "invalid_subject"},
		{`{"rules": [{"src": ["*"], "dst": ["host:db1"], "allow": ["exec"]}]}`, "invalid_target"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["ssh"]}]}`, "unknown_action"},
		{`{"rules": [{"src": [], "dst": ["machine:*"], "allow": ["exec"]}]}`, "empty"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["exec"], "ports": [22]}]}`, "ports_need_private_access"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["private_access"], "ports": [70000]}]}`, "invalid_port"},
		{`{"rules": [{"src": ["*"], "dst": ["machine:*"], "allow": ["private_access"], "ports": ["9-x"]}]}`, "invalid_port"},
		{`{"groups": {"ops": []}}`, "invalid_name"},
		{`{"tags": {"tag:db": ["machine:*"]}}`, "not_allowed_in_tag"},
		{`{"tests": [{"src": "user:ana@example.test", "dst": "machine:db1"}]}`, "required"},
		{`{"config_sync": [{"dst": ["tag:db"]}]}`, "required"},
	}
	for _, doc := range valid {
		if problems := checkAgainstSchema(t, doc); problems != nil {
			t.Errorf("schema refuses a valid policy: %v\n%s", problems, doc)
		}
		if _, _, errs := Validate([]byte(doc), team(), Limits{}); len(errs) > 0 {
			t.Errorf("Validate refuses the example: %v\n%s", errs, doc)
		}
	}
	for _, c := range invalid {
		if problems := checkAgainstSchema(t, c.doc); problems == nil {
			t.Errorf("schema accepts %s, which the server refuses with %s", c.doc, c.code)
		}
		_, _, errs := Validate([]byte(c.doc), team(), Limits{})
		if !slices.ContainsFunc(errs, func(e Error) bool { return e.Code == c.code }) {
			t.Errorf("%s: server errors %v, want code %s", c.doc, errs, c.code)
		}
	}
}

func checkAgainstSchema(t *testing.T, doc string) []string {
	t.Helper()
	canonical, err := Canonical([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	return schematest.Validate(t, Schema, canonical)
}

// Every rule code the package can produce is documented in README.md.
func TestReadmeDocumentsEveryErrorCode(t *testing.T) {
	readme, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	var src []byte
	for _, f := range []string{"parse.go", "compile.go", "rules.go", "eval.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src = append(src, b...)
	}
	found := map[string]bool{}
	for _, m := range regexp.MustCompile(`add\([^,]+, "([a-z_]+)",|Code: "([a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		for _, code := range m[1:] {
			if code != "" {
				found[code] = true
			}
		}
	}
	found["unknown_preview"], found["unknown_tunnel"] = true, true
	for code := range found {
		if !strings.Contains(string(readme), "`"+code+"`") {
			t.Errorf("README.md does not document error code %q", code)
		}
	}
}

// The README examples are valid policies.
func TestReadmeExampleValidates(t *testing.T) {
	readme, _ := os.ReadFile("README.md")
	m := regexp.MustCompile("(?s)```jsonc\n(.*?)```").FindAllSubmatch(readme, -1)
	if len(m) == 0 {
		t.Fatal("no example")
	}
	inv := team()
	inv.Machines = append(inv.Machines, Machine{ID: "m-x", Alias: "web2"})
	for _, ex := range m {
		if _, _, errs := Validate(ex[1], inv, Limits{}); len(errs) > 0 {
			t.Errorf("example: %v\n%s", errs, ex[1])
		}
		if problems := checkAgainstSchema(t, string(ex[1])); problems != nil {
			t.Errorf("schema refuses the README example: %v\n%s", problems, ex[1])
		}
	}
}
