import { afterEach, expect, test } from "bun:test"

const { optionalMe } = await import("@/lib/api")
const realFetch = globalThis.fetch
const realWindow = globalThis.window
afterEach(() => {
  globalThis.fetch = realFetch
  globalThis.window = realWindow
})

// C2: a signed-out guest on /terminal/{id} was sent to sign-in by the analytics /me read.
test("a signed-out visitor is nobody, not sent to sign-in", async () => {
  const assigned: string[] = []
  globalThis.window = { location: { pathname: "/terminal/t", search: "", assign: (u: string) => assigned.push(u) } } as never
  globalThis.fetch = (async () => new Response(null, { status: 401 })) as unknown as typeof fetch
  expect(await optionalMe()).toBeNull()
  expect(assigned).toEqual([])
})

test("an outage is an error, not a signed-out visitor", async () => {
  globalThis.fetch = (async () => new Response(null, { status: 503 })) as unknown as typeof fetch
  await expect(optionalMe()).rejects.toThrow()
})
