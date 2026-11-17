import { afterEach, expect, test } from "bun:test"
import { ApiError, api, post } from "@/lib/api"
import { sectionsFor } from "@/lib/sections"

/** The ApiError a request is expected to fail with. */
const failure = (request: Promise<unknown>) =>
  request.then(
    () => {
      throw new Error("expected the request to fail")
    },
    (e: unknown) => e as ApiError,
  )

const realFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = realFetch
})

function answer(status: number, body: unknown, headers: Record<string, string> = {}) {
  globalThis.fetch = (async () => new Response(JSON.stringify(body), { status, headers })) as unknown as typeof fetch
}

test("an error carries the support reference from the envelope", async () => {
  answer(503, { error: { code: "operator_unavailable", message: "Unavailable.", request_id: "req_1", support_reference: "support_abc" } })
  const error = await failure(api("/v1/admin/overview"))
  expect(error).toBeInstanceOf(ApiError)
  expect(error.status).toBe(503)
  expect(error.code).toBe("operator_unavailable")
  expect(error.reference).toBe("support_abc")
})

test("an error without an envelope still carries the response header reference", async () => {
  globalThis.fetch = (async () => new Response("<html>bad gateway</html>", { status: 502, headers: { "request-id": "req_header" } })) as unknown as typeof fetch
  const error = await failure(api("/v1/admin/overview"))
  expect(error.status).toBe(502)
  expect(error.reference).toBe("req_header")
})

test("success returns the data member", async () => {
  answer(200, { data: { total: 3 } })
  expect(await api<{ total: number }>("/v1/admin/accounts")).toEqual({ total: 3 })
})

test("a request that never arrives is a network error the page can retry", async () => {
  globalThis.fetch = (async () => {
    throw new TypeError("Failed to fetch")
  }) as unknown as typeof fetch
  const error = await failure(api("/v1/admin/overview"))
  expect(error.code).toBe("network")
})

test("sign-in failures keep their reference", async () => {
  answer(401, { error: { code: "invalid_credentials", message: "Operator sign-in failed.", request_id: "req_2" } })
  const error = await failure(post("/v1/operator/login", { email: "a", password: "b" }))
  expect(error.status).toBe(401)
  expect(error.reference).toBe("req_2")
})

test("each role sees only the pages it may read", () => {
  const titles = (role: "admin" | "support" | "billing" | "operations") => sectionsFor(role).map((s) => s.href)
  expect(titles("admin")).toContain("/audit")
  expect(titles("support")).not.toContain("/audit")
  expect(titles("support")).not.toContain("/infrastructure")
  expect(titles("operations")).toEqual(["/machines", "/capacity", "/infrastructure"])
  expect(titles("billing")).toContain("/billing")
})
