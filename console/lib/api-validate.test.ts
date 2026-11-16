import { afterEach, describe, expect, test } from "bun:test"
import { api, ApiError, checked } from "@/lib/api"
import { checkResponse, documentedOperations, operationFor, operationSchema, problem, sharedSchema } from "@/lib/api-validate"

import { sample, type Schema } from "@/lib/api-sample"

/** The first required field of the object a schema describes, with its schema; undefined for other shapes. */
function firstRequired(schema: Schema): [string, Schema] | undefined {
  if (schema.$ref) return firstRequired(sharedSchema(schema.$ref))
  for (const part of schema.allOf ?? []) {
    const found = firstRequired(part)
    if (found) return found
  }
  const name = schema.required?.find((n) => schema.properties?.[n])
  return name ? [name, schema.properties![name]] : undefined
}

describe("the validator", () => {
  const schema: Schema = {
    type: "object",
    required: ["id", "items"],
    properties: { id: { type: "string" }, count: { type: "integer" }, items: { type: "array", items: { type: "object", required: ["alias"], properties: { alias: { type: "string" } } } } },
  }
  test("ignores fields it does not know", () => {
    expect(problem(schema, { id: "a", items: [{ alias: "x", brand_new: { deep: 1 } }], extra: true })).toBeUndefined()
  })
  test("names a missing required field", () => {
    expect(problem(schema, { items: [] })).toBe("data.id is missing")
    expect(problem(schema, { id: "a", items: [{}] })).toBe("data.items[0].alias is missing")
  })
  test("names a wrong type", () => {
    expect(problem(schema, { id: 5, items: [] })).toBe("data.id should be string, got number")
    expect(problem(schema, { id: "a", count: 1.5, items: [] })).toBe("data.count should be integer, got number")
    expect(problem(schema, { id: "a", items: null })).toBe("data.items should be array, got null")
  })
  test("null is fine only where the schema allows it", () => {
    expect(problem({ type: ["string", "null"] }, null)).toBeUndefined()
    expect(problem({ type: "string" }, null)).toBe("data should be string, got null")
  })
})

describe("every documented operation", () => {
  const operations = documentedOperations()
  test("the console reads a meaningful number of them", () => expect(operations.length).toBeGreaterThan(100))

  test("accepts an answer of the documented shape, with an extra field", () => {
    for (const key of operations) {
      const value = sample(operationSchema(key))
      expect([key, problem(operationSchema(key), value)]).toEqual([key, undefined])
      if (value && typeof value === "object" && !Array.isArray(value))
        expect([key, problem(operationSchema(key), { ...value, field_from_a_newer_server: { x: 1 } })]).toEqual([key, undefined])
    }
  })

  test("refuses a wrong-typed or a missing required field, naming it", () => {
    let tried = 0
    for (const key of operations) {
      const schema = operationSchema(key)
      const required = firstRequired(schema)
      if (!required) continue
      const [name, fieldSchema] = required
      const value = sample(schema) as Record<string, unknown>
      const wrongType = [fieldSchema.type].flat()[0] === "string" ? 7 : "not what was documented"
      const named = (message?: string) => message?.includes(`data.${name}`) ?? false
      if ([fieldSchema.type].flat().length === 1 && fieldSchema.type) expect([key, named(problem(schema, { ...value, [name]: wrongType }))]).toEqual([key, true])
      const without = { ...value }
      delete without[name]
      expect([key, problem(schema, without)]).toEqual([key, `data.${name} is missing`])
      tried++
    }
    expect(tried).toBeGreaterThan(100)
  })
})

describe("operations", () => {
  test("a path finds its documented operation, literal segments before parameters", () => {
    expect(operationFor("GET", "machines?limit=200")).toBe("GET /v1/machines")
    expect(operationFor("GET", "machines/summary?workspace=personal")).toBe("GET /v1/machines/summary")
    expect(operationFor("GET", "machines/machine_1")).toBe("GET /v1/machines/{machine_id}")
    expect(operationFor("GET", "no/such/path")).toBeUndefined()
  })
  test("a tunnel rename answered 202 with its operation is a documented answer, not an unreadable one", () => {
    const operation = sample(sharedSchema("PreviewTunnelOperation"))
    expect(checkResponse("PATCH", "tunnels/tunnel_1", operation)).toBeUndefined()
    expect(checkResponse("PATCH", "tunnels/tunnel_1", sample(sharedSchema("Tunnel")))).toBeUndefined()
    expect(checkResponse("PATCH", "tunnels/tunnel_1", { id: 1 })).toContain("matches none of the shapes")
  })
  test("an undocumented path is not checked", () => expect(checkResponse("GET", "no/such/path", "anything")).toBeUndefined())
})

describe("api() at the boundary", () => {
  const realFetch = globalThis.fetch
  afterEach(() => (globalThis.fetch = realFetch))
  const answer = (data: unknown) => (globalThis.fetch = (() => Promise.resolve(Response.json({ data }))) as unknown as typeof fetch)
  const me = { id: "u1", email: "a@example.test", display_name: "A", status: "active", role: "user", workos_subject: "user_1" }

  test("returns the data of a right answer and ignores extra fields", async () => {
    answer({ ...me, a_field_added_later: [1, 2] })
    expect(((await api("me")) as { id: string }).id).toBe("u1")
  })
  test("a wrong-typed field fails with invalid_response naming the operation and field", async () => {
    answer({ ...me, id: 42 })
    const error = (await api("me").catch((e) => e)) as ApiError
    expect(error).toBeInstanceOf(ApiError)
    expect(error.code).toBe("invalid_response")
    expect(error.message).toContain("GET /v1/me: data.id should be string, got number")
  })
  test("a missing required field fails the same way", async () => {
    answer({ ...me, id: undefined })
    const error = (await api("me").catch((e) => e)) as ApiError
    expect(error.code).toBe("invalid_response")
    expect(error.message).toContain("data.id is missing")
  })
  test("checked() is the same boundary for readers that do not use api()", () => {
    expect(() => checked("GET", "me", { ...me, id: 1 })).toThrow("GET /v1/me: data.id should be string, got number")
    expect(checked("GET", "me", me)).toBe(me)
  })
})
