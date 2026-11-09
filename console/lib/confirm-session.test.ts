import { afterEach, expect, test } from "bun:test"

const { confirmSession } = await import("@/lib/api")
const realFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = realFetch
})

test("reads failing together share one /me check", async () => {
  let calls = 0
  globalThis.fetch = (async () => {
    calls++
    await new Promise((resolve) => setTimeout(resolve, 10))
    return new Response(null, { status: 200 })
  }) as unknown as typeof fetch
  await Promise.all([confirmSession(), confirmSession(), confirmSession()])
  expect(calls).toBe(1)
  // Once settled, a later 401 checks again.
  await confirmSession()
  expect(calls).toBe(2)
})
