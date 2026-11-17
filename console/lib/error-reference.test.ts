import { afterEach, beforeEach, expect, mock, spyOn, test } from "bun:test"

mock.module("server-only", () => ({}))
process.env.PAPERBOAT_SERVER_URL = "http://control.example.test"
const { api, ApiError } = await import("@/lib/api")
const { errorReference, traceFrom } = await import("@/lib/error-reference")
const { GET } = await import("@/app/api/v1/[...path]/route")

const reference = "support_0b7c3a52-9d1e-4c8a-8f2b-3e5d7a9c1b64"
const realFetch = globalThis.fetch
let logged: string[] = []
const answer = (fn: (input: unknown, init?: RequestInit) => Promise<Response>) =>
  (globalThis.fetch = fn as unknown as typeof fetch)
type Failure = InstanceType<typeof ApiError>
beforeEach(() => {
  logged = []
  spyOn(console, "error").mockImplementation((...args: unknown[]) => void logged.push(args.map(String).join(" ")))
})
afterEach(() => {
  globalThis.fetch = realFetch
  mock.restore()
})

test("an error answer keeps the server's support reference and request id, from the body or the headers", async () => {
  answer(() =>
    Promise.resolve(
      Response.json(
        { error: { code: "internal_error", message: "x", support_reference: reference, request_id: "request_1" } },
        { status: 503 },
      ),
    ),
  )
  const fromBody = await api<never>("machines").catch((error) => error as Failure)
  expect(fromBody).toBeInstanceOf(ApiError)
  expect([fromBody.reference, fromBody.requestId, errorReference(fromBody)]).toEqual([
    reference,
    "request_1",
    reference,
  ])

  answer(() =>
    Promise.resolve(
      Response.json(
        { error: { code: "unavailable", message: "x" } },
        { status: 502, headers: { "support-reference": reference, "request-id": "request_2" } },
      ),
    ),
  )
  const fromHeaders = await api<never>("machines").catch((error) => error as Failure)
  expect([fromHeaders.reference, fromHeaders.requestId]).toEqual([reference, "request_2"])
})

test("a malformed id is dropped, never shown", () => {
  const trace = traceFrom(new Response(null, { headers: { "support-reference": "<script>" } }), { request_id: "a b" })
  expect(trace).toEqual({ reference: undefined, requestId: undefined })
})

test("an error no server answered gets one client reference, the same each time and logged", async () => {
  answer(() => Promise.reject(new TypeError("Failed to fetch")))
  const error = await api<never>("machines").catch((e) => e)
  const first = errorReference(error)
  expect(first).toMatch(/^client_[0-9a-f]{32}$/)
  expect(errorReference(error)).toBe(first)
  expect(logged.filter((line) => line.includes(first))).toHaveLength(1)
  expect(errorReference(new Error("another"))).not.toBe(first)
})

test("a render error from the server shows Next's digest", () => {
  expect(errorReference(Object.assign(new Error("x"), { digest: "1234567890" }))).toBe("1234567890")
})

test("the API relay sends one reference to the control plane, returns it, and logs a failure with the request id", async () => {
  let sent = ""
  answer((_, init) => {
    sent = new Headers(init?.headers).get("support-reference") ?? ""
    return Promise.resolve(
      Response.json(
        { error: { code: "internal_error", message: "x", support_reference: sent, request_id: "request_9" } },
        { status: 503, headers: { "support-reference": sent, "request-id": "request_9" } },
      ),
    )
  })
  const response = await GET(new Request("http://console.test/api/v1/machines/machine_1/inbox"), {
    params: Promise.resolve({ path: ["machines", "machine_1", "inbox"] }),
  })
  expect(sent).toMatch(/^support_[0-9a-f-]{36}$/)
  expect(response.headers.get("support-reference")).toBe(sent)
  const line = logged.find((entry) => entry.includes("console api failure")) ?? ""
  expect(line).toContain(sent)
  expect(line).toContain("request_9")
  expect(line).toContain("machines/:id/inbox") // ids are not logged

  answer(() => Promise.reject(new Error("connect refused")))
  const down = await GET(new Request("http://console.test/api/v1/machines"), {
    params: Promise.resolve({ path: ["machines"] }),
  })
  const body = (await down.json()) as { error: { support_reference: string } }
  expect(body.error.support_reference).toBe(down.headers.get("support-reference")!)
  expect(logged.some((entry) => entry.includes(body.error.support_reference))).toBe(true)
})
