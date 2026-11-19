package recipe

import _ "embed"

// Schema is the recipe file's JSON Schema (2020-12), generated from the types in recipe.go
// (see schema_gen.go) and printed by `pb recipe schema`.
//
//go:embed schema.json
var Schema []byte
