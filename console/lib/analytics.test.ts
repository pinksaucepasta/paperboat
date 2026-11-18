import { afterEach, beforeEach, expect, test } from "bun:test"

const store = new Map<string, string>()
Object.assign(globalThis, {
  location: { host: "console.example.test", origin: "https://console.example.test" },
  sessionStorage: { getItem: (k: string) => store.get(k) ?? null, setItem: (k: string, v: string) => store.set(k, v) },
})
process.env.NEXT_PUBLIC_POSTHOG_KEY = "phc_test"
process.env.NEXT_PUBLIC_POSTHOG_HOST = "https://analytics.example.test"
const { client, pageleave, pageview, routePath, track } = await import("@/lib/analytics")

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
  expect((sent[0].body.properties as Record<string, unknown>).$geoip_disable).toBeUndefined()
})

test("every event carries the app, the session and the client class, never a query string", () => {
  pageview("acct_1", "/machines/[machineId]", "https://www.example.test/pricing?utm_source=x")
  track("acct_1", "preview_created")
  const [view, event] = sent.map((s) => s.body.properties as Record<string, string>)
  expect(sent[0].body.event).toBe("$pageview")
  expect(view).toMatchObject({ app: "console", $pathname: "/machines/[machineId]", $current_url: "https://console.example.test/machines/[machineId]", $referring_domain: "www.example.test" })
  expect(view.$session_id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
  expect(event.$session_id).toBe(view.$session_id)
  expect(event.$pathname).toBe("/machines/[machineId]")
  expect(Object.keys(view).filter((k) => /geoip|email|query|search/i.test(k))).toEqual([])
  pageleave("acct_1")
  expect(sent[2].body.event).toBe("$pageleave")
})

test("route templates hide ids and names but keep the tab", () => {
  expect(routePath("/machines/m_123/terminals", { machineId: "m_123", tab: "terminals" })).toBe("/machines/[machineId]/terminals")
  expect(routePath("/settings/recipes/my-recipe/edit", { name: "my-recipe" })).toBe("/settings/recipes/[name]/edit")
  // A name that is a prefix of a static segment, or has characters the URL encodes, still never leaks.
  expect(routePath("/settings/recipes/recipe/edit", { name: "recipe" })).not.toContain("recipe/edit")
  expect(routePath("/settings/recipes/my%20recipe/edit", { name: "my recipe" })).toBe("/settings/recipes/[name]/edit")
  expect(routePath("/machines/m1/terminals", { machineId: "m1", tab: "terminals" })).toBe("/machines/[machineId]/terminals")
})

test("browser, OS and device class come from the user agent", () => {
  expect(client("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 Chrome/130 Safari/537.36")).toEqual({ $browser: "Chrome", $os: "Linux", $device_type: "Desktop" })
  expect(client("Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit Version/17 Mobile Safari/604")).toEqual({ $browser: "Safari", $os: "iOS", $device_type: "Mobile" })
  expect(client("Mozilla/5.0 (Windows NT 10.0) Chrome/130 Safari/537 Edg/130").$browser).toBe("Microsoft Edge")
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
