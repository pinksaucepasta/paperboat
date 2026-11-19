package configsync

import _ "embed"

// Schema is the config sync file's JSON Schema (2020-12), generated from SourceConfig (see schema_gen.go)
// and printed by `pb config sync schema`.
//
//go:embed schema.json
var Schema []byte
