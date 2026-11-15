import { afterEach, beforeEach, expect, test } from "bun:test"

process.env.NEXT_PUBLIC_POSTHOG_KEY = "phc_test"
process.env.NEXT_PUBLIC_POSTHOG_HOST = "https://analytics.example.test"
const { track } = await import("@/lib/analytics")

const sent: { url: string; body: Record<string, unknown> }[] = []
const realFetch = globalThis.fetch
const global = globalThis as { localStorage?: unknown }

beforeEach(() => {
  sent.length = 0
  globalThis.fetch = (async (url: URL, init: RequestInit) => {
    sent.push({ url: String(url), body: JSON.parse(String(init.body)) })
    return new Response("{}")
  }) as unknown as typeof fetch
})
afterEach(() => {
  globalThis.fetch = realFetch
  delete global.localStorage
  Object.defineProperty(navigator, "doNotTrack", { value: null, configurable: true })
})

test("sends the event with the account id to the configured host", () => {
  track("acct_1", "preview_created", { access: "public" })
  expect(sent).toHaveLength(1)
  expect(sent[0].url).toBe("https://analytics.example.test/i/v0/e/")
  expect(sent[0].body).toMatchObject({ api_key: "phc_test", event: "preview_created", distinct_id: "acct_1" })
})

test("sends nothing with Do Not Track", () => {
  Object.defineProperty(navigator, "doNotTrack", { value: "1", configurable: true })
  track("acct_1", "preview_created")
  expect(sent).toHaveLength(0)
})

test("sends nothing when the Settings switch is off", () => {
  global.localStorage = { getItem: (key: string) => (key === "pb-analytics-off" ? "1" : null) }
  track("acct_1", "preview_created")
  expect(sent).toHaveLength(0)
})

test("a failing request does not throw", () => {
  globalThis.fetch = (() => Promise.reject(new Error("blocked"))) as unknown as typeof fetch
  expect(() => track("acct_1", "preview_created")).not.toThrow()
})
