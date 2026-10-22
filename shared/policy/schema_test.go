package policy

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

type schemaNode struct {
	Properties map[string]json.RawMessage `json:"properties"`
	Defs       map[string]schemaNode      `json:"$defs"`
	Enum       []string                   `json:"enum"`
	Required   []string                   `json:"required"`
	Additional *bool                      `json:"additionalProperties"`
}

func keys(m map[string]json.RawMessage) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// The schema and the parser must accept the same fields and actions.
func TestSchemaMatchesTheParser(t *testing.T) {
	var s schemaNode
	if err := json.Unmarshal(Schema, &s); err != nil {
		t.Fatal(err)
	}
	if s.Additional == nil || *s.Additional {
		t.Fatal("the top level must refuse unknown keys")
	}
	if got := keys(s.Properties); !reflect.DeepEqual(got, []string{"$schema", "allow_private_access", "groups", "rules", "tags", "tests"}) {
		t.Fatalf("top-level keys %q", got)
	}
	if got := keys(s.Defs["rule"].Properties); !reflect.DeepEqual(got, []string{"allow", "deny", "dst", "note", "ports", "src"}) {
		t.Fatalf("rule keys %q", got)
	}
	if got := keys(s.Defs["test"].Properties); !reflect.DeepEqual(got, []string{"allow", "deny", "dst", "src"}) {
		t.Fatalf("test keys %q", got)
	}
	want := append([]string{"*"}, MachineActions...)
	want = append(want, ENVActions...)
	if !reflect.DeepEqual(s.Defs["action"].Enum, want) {
		t.Fatalf("actions %q, want %q", s.Defs["action"].Enum, want)
	}
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
	}
}
