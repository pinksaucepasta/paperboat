import { afterEach, expect, test } from "bun:test"
import { drainInstallation, liveOf, resumeInstallation, switchEdgesStoppingLive, type Installation } from "@/lib/network"
import { ApiError } from "@/lib/api"

const realFetch = globalThis.fetch
afterEach(() => {
  globalThis.fetch = realFetch
})

const region = (id: string) => ({ id, name: id, status: "ready" })
const choice = { kind: "self_hosted" as const, primaryRegion: "r1", fallbackRegion: "r2" }
const json = (status: number, body: unknown, headers: Record<string, string> = {}) =>
  new Response(JSON.stringify(body), { status, headers: { "content-type": "application/json", ...headers } })
const tunnel = (desired: string) => ({
  schema: "paperboat.preview-tunnel/v1", kind: "tunnel", id: "t1", account_id: "u", name: "n", desired_state: desired,
  access_mode: "public", etag: '"1"', generation: 1, stable_endpoint_id: "e", stable_endpoint: "https://t.example.test",
  created_by_host_id: "m", created_by_actor_id: "u", summary_code: "pending", created_at: "2026-11-18T12:00:00Z", updated_at: "2026-11-18T12:00:00Z",
})
const refusal = (items: unknown[], count = items.length) =>
  json(409, { error: { code: "previews_running", message: "m", details: { count, items } } })

test("a refusal names what runs; any other error does not", () => {
  const items = [{ kind: "preview" as const, id: "p1", name: "https://a.example.test" }]
  const error = new ApiError(409, "previews_running", "m", { count: 1, items })
  expect(liveOf(error)).toEqual({ count: 1, items })
  expect(liveOf(new ApiError(409, "other", "m", { count: 1, items }))).toBeUndefined()
  expect(liveOf(new Error("x"))).toBeUndefined()
})

test("switching stops what the server names, each its own way, and asks again until it accepts", async () => {
  const calls: string[] = []
  const asked: string[][] = [
    [JSON.stringify({ kind: "preview", id: "p1", name: "n" }), JSON.stringify({ kind: "tunnel", id: "t1", name: "n" })],
    [JSON.stringify({ kind: "preview", id: "p2", name: "n" })],
  ]
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === "string" ? input : input instanceof URL ? input.href : input.url, "http://console.test")
    const call = `${init?.method ?? "GET"} ${url.pathname.replace("/api/v1/", "")}`
    calls.push(call)
    if (call === "PUT edges/pair") {
      const next = asked.shift()
      return next ? refusal(next.map((item) => JSON.parse(item)), 3) : json(200, { data: { kind: "self_hosted", primary: region("r1"), fallback: region("r2") } })
    }
    if (call.startsWith("GET previews/")) return json(200, { data: {} }, { etag: '"1"' })
    if (call === "GET tunnels/t1") return json(200, { data: tunnel("active") })
    if (call === "POST tunnels/t1/pause") return json(200, { data: tunnel("paused") })
    return json(200, { data: {} })
  }) as typeof fetch
  const { pair, stopped } = await switchEdgesStoppingLive("personal", choice)
  expect(pair.primary.id).toBe("r1")
  expect(stopped.map((item) => item.id)).toEqual(["p1", "t1", "p2"])
  expect(calls.filter((call) => call.startsWith("DELETE previews/")).sort()).toEqual(["DELETE previews/p1", "DELETE previews/p2"])
  expect(calls).toContain("POST tunnels/t1/pause")
  expect(calls.filter((call) => call === "PUT edges/pair").length).toBe(3)
})

test("a refusal that names nothing stops nothing and is shown as it is", async () => {
  globalThis.fetch = (async () => refusal([], 2)) as unknown as typeof fetch
  await expect(switchEdgesStoppingLive("personal", choice)).rejects.toMatchObject({ code: "previews_running" })
})

test("the pair is saved as regions, and drain and resume name the installation and the move", async () => {
  const sent: string[] = []
  globalThis.fetch = (async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(typeof input === "string" ? input : input instanceof URL ? input.href : input.url, "http://console.test")
    sent.push(`${init?.method} ${url.pathname.replace("/api/v1/", "")} ${init?.body ?? ""}`)
    if (url.pathname.includes("selfhost/installations"))
      return json(200, {
        data: {
          installation_id: "inst/1", node_id: "n", endpoint_host: "h.example.test", name: "n", region: "home", capability: "tunnel",
          ready: false, trust_notice: "", scope_kind: "account", scope_id: "u", enrollment_state: "enrolled",
        },
      })
    return json(200, { data: { kind: "self_hosted", primary: region("r1"), fallback: region("r2") } })
  }) as typeof fetch
  await switchEdgesStoppingLive("personal", choice)
  const installation = { installation_id: "inst/1" } as Installation
  await drainInstallation(installation, true)
  await drainInstallation(installation, false)
  await resumeInstallation(installation)
  expect(sent).toEqual([
    'PUT edges/pair {"kind":"self_hosted","primary_region":"r1","fallback_region":"r2"}',
    'POST selfhost/installations/inst%2F1/drain {"move":true}',
    'POST selfhost/installations/inst%2F1/drain {"move":false}',
    "POST selfhost/installations/inst%2F1/resume {}",
  ])
})
