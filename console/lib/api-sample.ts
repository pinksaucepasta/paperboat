// Test support: a value that satisfies a documented operation's response, for fixtures and the validator's own tests.
import { operationSchema, sharedSchema, type problem } from "@/lib/api-validate"

export type Schema = Parameters<typeof problem>[0]

/** A value that satisfies a schema: required fields only, the first choice of every oneOf. */
export function sample(schema: Schema, depth = 0): unknown {
  if (schema.$ref) return sample(sharedSchema(schema.$ref), depth)
  if (schema.allOf) {
    const parts = schema.allOf.map((part) => sample(part, depth)).filter((part) => part && typeof part === "object")
    return Object.assign({}, sample({ ...schema, allOf: undefined }, depth), ...parts)
  }
  const choice = schema.oneOf?.[0] ?? schema.anyOf?.[0]
  if (choice) return sample(choice, depth)
  const type = [schema.type].flat().find((t) => t !== "null")
  if (schema.type === "null") return null
  if (type === "string") return "x"
  if (type === "integer") return 1
  if (type === "number") return 1.5
  if (type === "boolean") return true
  if (type === "array") return depth < 6 && schema.items ? [sample(schema.items, depth + 1)] : []
  if (type === "object" || schema.properties) {
    const out: Record<string, unknown> = {}
    for (const name of schema.required ?? []) if (schema.properties?.[name] && depth < 6) out[name] = sample(schema.properties[name], depth + 1)
    return out
  }
  return "x"
}

/** What `GET /v1/…` answers, with every required field present; override what a test cares about. */
export const sampleFor = (operation: string, overrides: Record<string, unknown> = {}) => ({
  ...(sample(operationSchema(operation)) as Record<string, unknown>),
  ...overrides,
})
