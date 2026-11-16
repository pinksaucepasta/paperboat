// Writes lib/api-responses.generated.json from ../server/docs/openapi.json: the shape of the `data` of every successful JSON
// response a browser session may read, with only what the console checks (types, required fields, items, refs). The console
// validates each answer against it (lib/api-validate.ts). Run `bun run api:responses`; `bun run api:check` fails when stale.
import { readFileSync, writeFileSync } from "node:fs"

const doc = JSON.parse(readFileSync(new URL("../../server/docs/openapi.json", import.meta.url), "utf8"))
const keep = ["type", "properties", "required", "items", "additionalProperties", "oneOf", "anyOf", "allOf"]
const refName = (ref) => ref.replace("#/components/schemas/", "")
const used = new Set()

/** The checked part of a schema. Enums, formats, limits and descriptions are left out: new server values must not break a page. */
function slim(schema) {
  if (schema.$ref) {
    used.add(refName(schema.$ref))
    return { $ref: refName(schema.$ref) }
  }
  const out = {}
  for (const key of keep) {
    const value = schema[key]
    if (value === undefined) continue
    if (key === "properties") out[key] = Object.fromEntries(Object.entries(value).map(([name, child]) => [name, slim(child)]))
    else if (key === "items") out[key] = slim(value)
    else if (key === "additionalProperties") out[key] = typeof value === "object" ? slim(value) : true
    else if (["oneOf", "anyOf", "allOf"].includes(key)) out[key] = value.map(slim)
    else out[key] = value
  }
  // if/then parts slim to nothing; they would only be noise.
  if (out.allOf) out.allOf = out.allOf.filter((part) => Object.keys(part).length)
  if (out.allOf && !out.allOf.length) delete out.allOf
  return out
}

const ops = {}
for (const [path, item] of Object.entries(doc.paths)) {
  for (const [method, op] of Object.entries(item)) {
    if (!["get", "post", "put", "patch"].includes(method)) continue
    // Operator and machine-credential endpoints are not called from a browser session.
    const security = op.security ?? doc.security
    if (!security.some((option) => "cookieSession" in option)) continue
    // A mutation documented as 200/201 with a resource and 202 with its operation answers with either: accept each shape.
    const shapes = ["200", "201", "202"]
      .map((code) => op.responses?.[code]?.content?.["application/json"]?.schema)
      .filter(Boolean)
      .map((schema) => (schema.$ref ? doc.components.schemas[refName(schema.$ref)] : schema)?.properties?.data)
      // A response documented only as { data: any } says nothing to check.
      .filter((data) => data && Object.keys(data).some((key) => keep.includes(key) || key === "$ref"))
    const distinct = [...new Map(shapes.map((data) => [JSON.stringify(data), data])).values()]
    if (!distinct.length) continue
    const data = distinct.length === 1 ? distinct[0] : { oneOf: distinct }
    ops[`${method.toUpperCase()} ${path}`] = slim(data)
  }
}

const defs = {}
const queue = [...used]
while (queue.length) {
  const name = queue.shift()
  if (defs[name]) continue
  const before = new Set(used)
  defs[name] = slim(doc.components.schemas[name])
  for (const next of used) if (!before.has(next)) queue.push(next)
}
writeFileSync(new URL("../lib/api-responses.generated.json", import.meta.url), JSON.stringify({ defs, ops }) + "\n")
console.log(`api-responses: ${Object.keys(ops).length} operations, ${Object.keys(defs).length} shared schemas`)
