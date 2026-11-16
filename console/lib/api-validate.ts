import generated from "@/lib/api-responses.generated.json"

/**
 * Checks what the control plane answers against its OpenAPI document (generated into api-responses.generated.json by
 * `bun run api:responses`). Unknown fields are ignored; a wrong type or a missing required field is a named problem.
 */
type Schema = {
  $ref?: string
  type?: string | string[]
  properties?: Record<string, Schema>
  required?: string[]
  items?: Schema
  additionalProperties?: Schema | boolean
  oneOf?: Schema[]
  anyOf?: Schema[]
  allOf?: Schema[]
}

const defs = generated.defs as Record<string, Schema>
const ops = generated.ops as Record<string, Schema>

const kind = (value: unknown) => (value === null ? "null" : Array.isArray(value) ? "array" : typeof value)

const matches = (type: string, value: unknown) =>
  type === "integer" ? Number.isInteger(value) : type === "number" ? typeof value === "number" : kind(value) === type

/** The first thing wrong with `value` against `schema`, or undefined; `at` names where, like `items[0].alias`. */
export function problem(schema: Schema, value: unknown, at = "data"): string | undefined {
  if (schema.$ref) return problem(defs[schema.$ref], value, at)
  for (const part of schema.allOf ?? []) {
    const found = problem(part, value, at)
    if (found) return found
  }
  const choices = schema.oneOf ?? schema.anyOf
  if (choices && !choices.some((choice) => !problem(choice, value, at))) return `${at} matches none of the shapes Paperboat documents`

  const types = schema.type === undefined ? [] : Array.isArray(schema.type) ? schema.type : [schema.type]
  if (types.length && !types.some((type) => matches(type, value))) return `${at} should be ${types.join(" or ")}, got ${kind(value)}`

  if (Array.isArray(value) && schema.items) {
    for (let i = 0; i < value.length; i++) {
      const found = problem(schema.items, value[i], `${at}[${i}]`)
      if (found) return found
    }
  }
  if (kind(value) === "object" && (schema.properties || schema.required || typeof schema.additionalProperties === "object")) {
    const object = value as Record<string, unknown>
    for (const name of schema.required ?? []) if (object[name] === undefined) return `${at}.${name} is missing`
    for (const [name, child] of Object.entries(object)) {
      const own = schema.properties?.[name]
      const rule = own ?? (typeof schema.additionalProperties === "object" ? schema.additionalProperties : undefined)
      // An optional field may be absent, never wrong: undefined is skipped above, null follows its schema.
      const found = rule && child !== undefined ? problem(rule, child, `${at}.${name}`) : undefined
      if (found) return found
    }
  }
  return undefined
}

const templates = Object.keys(ops).map((key) => {
  const [method, path] = key.split(" ")
  return { key, method, segments: path.split("/") }
})

/** The documented operation for a request: `machines/abc` is `GET /v1/machines/{machine_id}`; literal segments win. */
export function operationFor(method: string, path: string): string | undefined {
  const segments = `/v1/${path.split("?")[0]}`.split("/")
  let best: { key: string; literals: number } | undefined
  for (const template of templates) {
    if (template.method !== method || template.segments.length !== segments.length) continue
    let literals = 0
    const fits = template.segments.every((segment, i) => {
      if (segment.startsWith("{")) return segments[i] !== ""
      literals++
      return segment === segments[i]
    })
    if (fits && (!best || literals > best.literals)) best = { key: template.key, literals }
  }
  return best?.key
}

/** What is wrong with a response's `data`, naming the operation and field; undefined when fine or undocumented. */
export function checkResponse(method: string, path: string, data: unknown): string | undefined {
  const key = operationFor(method, path)
  const found = key ? problem(ops[key], data) : undefined
  return found && `${key}: ${found}`
}

export const documentedOperations = () => Object.keys(ops)
export const operationSchema = (key: string) => ops[key]
export const sharedSchema = (name: string) => defs[name]
