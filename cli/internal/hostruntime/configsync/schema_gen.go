package configsync

import (
	"encoding/json"
	"reflect"

	"github.com/pinksaucepasta/paperboat/shared/schemagen"
)

// SchemaOptions describe the config sync file (.paperboat/config-sync.jsonc in a repository, and the
// per-machine config-sync.jsonc) for schemagen; schema.json is generated from them
// (go test ./internal/hostruntime/configsync -run TestSchemaIsGenerated -update).
func SchemaOptions() schemagen.Options {
	return schemagen.Options{
		Title:       "Paperboat config sync file",
		Description: "Which files and directories config sync keeps in a Git repository, where they live on each machine, and which way they move. It is the shared file .paperboat/config-sync.jsonc in the repository, or one machine's own config-sync.jsonc (pb config sync path); the machine's file wins over the shared one. It may hold // comments and trailing commas (HuJSON); an empty file is valid.",
		Examples: []json.RawMessage{
			json.RawMessage(`{"version": 1, "mode": "pull_only", "paths": {"shell/zshrc": {"local_path": "~/.zshrc", "kind": "file"}, "editor": {"local_path": "~/.config/editor", "kind": "directory", "exclude": ["**/*.tmp"]}}}`),
		},
		Defs: map[string]any{
			"mode": map[string]any{"type": "string", "enum": []string{"pull_only", "push_only", "bidirectional"}, "description": "pull_only: this machine only receives changes. push_only: it only publishes them. bidirectional: both.", "examples": []string{"pull_only"}},
			"kind": map[string]any{"type": "string", "enum": []string{"file", "directory"}, "description": "Whether a path is a single file or a directory tree.", "examples": []string{"file", "directory"}},
		},
		Extra: map[reflect.Type]map[string]any{
			reflect.TypeOf(RepositoryTarget{}): {"description": "A registered config sync repository."},
			reflect.TypeOf(RuleConfig{}):       {"description": "How one path syncs. Leave a key out to inherit it from the os section or the shared file."},
		},
		Post: func(schema map[string]any) {
			// os and machine sections hold the same keys as the file, minus os and machine.
			defs := schema["$defs"].(map[string]any)
			section := map[string]any{}
			for k, v := range defs["sourceConfig"].(map[string]any) {
				section[k] = v
			}
			properties := map[string]any{}
			for k, v := range section["properties"].(map[string]any) {
				if k != "os" && k != "machine" {
					properties[k] = v
				}
			}
			section["properties"] = properties
			section["description"] = "One os or machine section: the keys of the file except os and machine."
			section["examples"] = []any{map[string]any{"mode": "pull_only"}}
			defs["section"] = section
			for _, key := range []string{"os", "machine"} {
				for _, root := range []map[string]any{schema, defs["sourceConfig"].(map[string]any)} {
					root["properties"].(map[string]any)[key].(map[string]any)["additionalProperties"] = map[string]any{"$ref": "#/$defs/section"}
				}
			}
		},
	}
}
