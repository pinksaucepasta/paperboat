import { afterEach, beforeEach, describe, expect, mock, test } from "bun:test"

mock.module("server-only", () => ({}))
mock.module("next/headers", () => ({ cookies: async () => new Map([["pb-workspace", { value: "personal" }]]) }))
mock.module("next/server", () => ({ connection: async () => {} }))
process.env.PAPERBOAT_SERVER_URL = "http://control.example.test"
const { prefetchMachinesPage } = await import("@/lib/server/prefetch")
const { api, machinesQuery } = await import("@/lib/api")
const { validatePolicy } = await import("@/lib/policy")
const { cursorPage } = await import("@/lib/cursor-pages")

const realFetch = globalThis.fetch
let aborted: string[] = []

/** A server that answers /me at once and never answers anything else until the caller gives up. */
beforeEach(() => {
  aborted = []
  globalThis.fetch = ((input: string | URL, init?: RequestInit) => {
    const url = String(input)
    if (url.endsWith("/v1/me")) return Promise.resolve(Response.json({ data: { id: "u1" } }))
    return new Promise((_, reject) =>
      init?.signal?.addEventListener("abort", () => {
        aborted.push(url)
        reject(init.signal!.reason)
      }),
    )
  }) as typeof fetch
})
afterEach(() => {
  globalThis.fetch = realFetch
})

describe("a hung endpoint", () => {
  test("the machines page prefetch ends within one timeout, keeps what answered, aborts the rest", async () => {
    const started = Date.now()
    const { state } = await prefetchMachinesPage()
    expect(Date.now() - started).toBeLessThan(4_000)
    expect(JSON.stringify(state)).toContain("u1")
    expect(aborted.length).toBe(3)
  })

  test("a browser read stops when its query is cancelled", async () => {
    const caller = new AbortController()
    const read = api("machines/overview", { signal: caller.signal })
    caller.abort()
    expect(read).rejects.toMatchObject({ name: "AbortError" })
    expect(aborted).toHaveLength(1)
  })

  test("a paged query's every page request stops when the query is cancelled", async () => {
    const caller = new AbortController()
    const run = machinesQuery("personal").queryFn as (context: { signal: AbortSignal }) => Promise<unknown>
    const paged = run({ signal: caller.signal })
    const settled = paged.catch((error: Error) => error.name)
    caller.abort()
    expect(await settled).toBe("AbortError")
    expect(aborted).toHaveLength(1)
  })

  test("a cursor-paged list stops when its query is cancelled", async () => {
    const caller = new AbortController()
    const settled = cursorPage("machines", { workspace: "personal" }, 1, caller.signal).catch((e: Error) => e.name)
    caller.abort()
    expect(await settled).toBe("AbortError")
    expect(aborted).toHaveLength(1)
  })

  test("a POST-style read (policy validation) stops when its query is cancelled", async () => {
    const caller = new AbortController()
    const settled = validatePolicy("personal", "{}", caller.signal).catch((e: Error) => e.name)
    caller.abort()
    expect(await settled).toBe("AbortError")
    expect(aborted).toHaveLength(1)
  })
})
