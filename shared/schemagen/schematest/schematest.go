// Package schematest holds the checks every generated schema file shares; only tests import it.
package schematest

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/schemagen"
	"github.com/xeipuuv/gojsonschema"
)

var update = flag.Bool("update", false, "rewrite the generated schema file")

// Generated fails when file is not what schemagen makes from the types in root; -update rewrites it.
func Generated(t *testing.T, file string, root any, o schemagen.Options) []byte {
	t.Helper()
	want, err := schemagen.Generate(root, o)
	if err != nil {
		t.Fatal(err)
	}
	if *update {
		if err := os.WriteFile(file, want, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs from the schema generated from the Go types; run: go test . -run %s -update", file, t.Name())
	}
	return got
}

// Validate returns the problems the schema finds in doc (plain JSON), or nil when doc is valid.
func Validate(t *testing.T, schema, doc []byte) []string {
	t.Helper()
	var s map[string]any
	if err := json.Unmarshal(schema, &s); err != nil {
		t.Fatal(err)
	}
	delete(s, "$schema") // the validator knows drafts 4 to 7; the keywords used here are in all of them
	result, err := gojsonschema.Validate(gojsonschema.NewGoLoader(s), gojsonschema.NewBytesLoader(doc))
	if err != nil {
		t.Fatalf("validate %s: %v", doc, err)
	}
	var out []string
	for _, e := range result.Errors() {
		out = append(out, e.String())
	}
	return out
}

// Documented fails for a property or definition without a description, or without an example that
// the schema itself accepts.
func Documented(t *testing.T, schema []byte) {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(schema, &root); err != nil {
		t.Fatal(err)
	}
	check := func(where string, node map[string]any) {
		if d, _ := node["description"].(string); strings.TrimSpace(d) == "" {
			t.Errorf("%s has no description", where)
		}
		examples, _ := node["examples"].([]any)
		if len(examples) == 0 {
			t.Errorf("%s has no examples", where)
		}
		for _, example := range examples {
			doc, _ := json.Marshal(example)
			own := map[string]any{"$defs": root["$defs"]}
			for k, v := range node {
				own[k] = v
			}
			if bad := Validate(t, mustJSON(own), doc); bad != nil {
				t.Errorf("%s: example %s is not valid: %v", where, doc, bad)
			}
		}
	}
	var walk func(where string, node any)
	walk = func(where string, node any) {
		switch n := node.(type) {
		case map[string]any:
			if props, ok := n["properties"].(map[string]any); ok {
				for name, p := range props {
					if p, ok := p.(map[string]any); ok {
						check(where+"/"+name, p)
					}
					walk(where+"/"+name, p)
				}
			}
			for k, v := range n {
				switch k {
				case "properties", "examples", "default", "const", "enum", "if", "then", "else", "anyOf", "oneOf", "allOf":
				default:
					walk(where+"/"+k, v)
				}
			}
		case []any:
			for _, v := range n {
				walk(where, v)
			}
		}
	}
	walk("", root)
	if defs, ok := root["$defs"].(map[string]any); ok {
		for name, d := range defs {
			if d, ok := d.(map[string]any); ok {
				if _, isObject := d["properties"]; !isObject { // an object def is documented by Extra and its properties
					check("$defs/"+name, d)
				} else if desc, _ := d["description"].(string); desc == "" {
					t.Errorf("$defs/%s has no description", name)
				}
			}
		}
	}
	check("(file)", root)
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
