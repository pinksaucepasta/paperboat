package configsync

import (
	"errors"
	"testing"

	"github.com/pinksaucepasta/paperboat/shared/schemagen/schematest"
	"github.com/tailscale/hujson"
)

// schema.json is generated from the Go types (go test . -run TestSchemaIsGenerated -update).
func TestSchemaIsGenerated(t *testing.T) {
	schematest.Generated(t, "schema.json", SourceConfig{}, SchemaOptions())
}

func TestSchemaIsDocumented(t *testing.T) { schematest.Documented(t, Schema) }

// The schema refuses what the reader refuses and accepts what it accepts. What a schema cannot say
// (a pattern that is not a valid glob, paths that contain one another, reserved Windows names) is
// checked by ParseSourceConfig only.
func TestSchemaAgreesWithTheReader(t *testing.T) {
	valid := []string{
		`{}`,
		"// only a comment\n{}",
		`{"version": 1, "enabled": true, "mode": "bidirectional", "automatic_updates": false,
		  "pull": {"repository_id": "dotfiles", "branch": "main"}, "push": {"repository_id": "dotfiles", "url": "https://git.example.test/d.git"},
		  "paths": {"shell/zshrc": {"local_path": "~/.zshrc", "kind": "file"}, "editor": {"local_path": "~/e", "kind": "directory", "include": ["*.json"], "exclude": []}},
		  "os": {"windows": {"paths": {"editor": {"local_path": "%APPDATA%/editor"}}}},
		  "machine": {"laptop-1": {"mode": "pull_only"}},}`,
	}
	invalid := []string{
		`{"unexpected": true}`, `{"Paths": {}}`, `{"version": 2}`, `{"version": 1.5}`, `{"mode": "sideways"}`, `{"mode": 3}`,
		`{"enabled": "true"}`, `{"paths": 3}`, `{"os": 3}`, `{"pull": 3}`, `{"pull": {}}`, `{"pull": {"repository_id": 3}}`, `{"pull": {"repository_id": "r", "x": 1}}`,
		`{"paths": {"a": {"include": 1}}}`, `{"paths": {"a": {"exclude": [1]}}}`, `{"paths": {"a": {"kind": "link"}}}`, `{"paths": {"a": {"local_path": ""}}}`,
		`{"paths": {"a": {"unknown": 1}}}`, `{"os": {"plan9": {}}}`, `{"os": {"linux": {"os": {}}}}`, `{"os": {"linux": {"machine": {}}}}`,
		`{"machine": {"LAPTOP": {}}}`, `{"machine": {"-laptop": {}}}`, `{"machine": {"laptop-": {}}}`, `{"machine": {"a b": {}}}`, `{"machine": {"laptop": {"os": {}}}}`, `{"machine": {"laptop": {"mode": 3}}}`,
	}
	for _, doc := range valid {
		if problems := checkAgainstSchema(t, doc); problems != nil {
			t.Errorf("schema refuses a valid file: %v\n%s", problems, doc)
		}
		if _, err := ParseSourceConfig([]byte(doc), DefaultSourceConfigLimits()); err != nil {
			t.Errorf("the reader refuses the example: %v\n%s", err, doc)
		}
	}
	for _, doc := range invalid {
		if problems := checkAgainstSchema(t, doc); problems == nil {
			t.Errorf("schema accepts %s, which the reader refuses", doc)
		}
		if _, err := ParseSourceConfig([]byte(doc), DefaultSourceConfigLimits()); !errors.Is(err, ErrSourceConfigInvalid) {
			t.Errorf("%s: the reader says %v, want ErrSourceConfigInvalid", doc, err)
		}
	}
}

func checkAgainstSchema(t *testing.T, doc string) []string {
	t.Helper()
	standard, err := hujson.Standardize([]byte(doc))
	if err != nil {
		standard = []byte("{}") // only comments: an empty file
	}
	return schematest.Validate(t, Schema, standard)
}
