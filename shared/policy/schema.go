package policy

import _ "embed"

// Schema is the policy file's JSON Schema (2020-12), served by the server at
// GET /v1/policy/schema and printed by `pb policy schema`.
//
//go:embed schema.json
var Schema []byte
