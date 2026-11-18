// Integration: the real Worker code on a local D1 (wrangler's getPlatformProxy), time driven by hand.
import { after, before, test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { getPlatformProxy } from "wrangler"
import worker from "../src/index.ts"
import { sign } from "../src/probe.ts"
import { buildSnapshot, catalog, evaluate, minute, readSnapshot, saveReport, type Reported } from "../src/store.ts"
import type { Env } from "../src/types.ts"

let db: D1Database
let dispose: () => Promise<void>
const env = () => ({ DB: db, PROBE_KEYS: JSON.stringify({ oracle: "k-oracle", anvit: "k-anvit" }), ADMIN_TOKEN: "admin-secret" }) as Env
const ctx = { waitUntil() {}, passThroughOnException() {} } as unknown as ExecutionContext
const T0 = minute(Date.UTC(2026, 10, 18, 12, 0, 0))

before(async () => {
  const p = await getPlatformProxy<{ DB: D1Database }>({ configPath: new URL("../wrangler.jsonc", import.meta.url).pathname, persist: { path: mkdtempSync(join(tmpdir(), "status-")) } })
  db = p.env.DB
  dispose = p.dispose
  for (const stmt of readFileSync(new URL("../schema.sql", import.meta.url), "utf8").split(";").map((s) => s.trim()).filter(Boolean)) await db.prepare(stmt).run()
})
after(async () => dispose?.())

const allUnits = catalog.components.flatMap((c) => c.units)
/** every unit healthy except `bad` */
const report = (vantage: string, bad: string[] = [], ms = 100): Reported =>
  Object.fromEntries(allUnits.filter((u) => u.vantages.includes(vantage)).map((u) => [u.id, [bad.includes(u.id) ? 0 : 1, ms] as [number, number]]))
async function minuteOf(i: number, badBy: Record<string, string[]> = {}, silent: string[] = []) {
  const now = T0 + i * 60_000 + 5_000
  for (const v of catalog.vantages) if (!silent.includes(v)) await saveReport(db, v, now, true, "test", report(v, badBy[v] ?? []))
  await evaluate(db, now)
  return (await buildSnapshot(db, now)).public.components
}
const state = (cs: Awaited<ReturnType<typeof minuteOf>>, id: string) => cs.find((c) => c.id === id)!.state

test("healthy for five minutes is operational everywhere", async () => {
  let cs: Awaited<ReturnType<typeof minuteOf>> = []
  for (let i = 0; i < 5; i++) cs = await minuteOf(i)
  assert.ok(cs.every((c) => c.state === "operational"), JSON.stringify(cs.map((c) => [c.id, c.state])))
})

test("one relay down on two vantages for three minutes: only the relays component, degraded (the other relay is fine); then recovery after five passes", async () => {
  const bad = { oracle: ["relay_india_1"], anvit: ["relay_india_1"] }
  let cs = await minuteOf(5, bad)
  cs = await minuteOf(6, bad)
  assert.equal(state(cs, "relays"), "operational") // two failures are not enough
  cs = await minuteOf(7, bad)
  assert.equal(state(cs, "relays"), "degraded")
  assert.equal(state(cs, "edges"), "operational")
  assert.equal(state(cs, "api"), "operational")
  for (let i = 8; i < 12; i++) cs = await minuteOf(i)
  assert.notEqual(state(cs, "relays"), "operational") // four passes: not yet
  cs = await minuteOf(12)
  assert.equal(state(cs, "relays"), "operational")
})

test("both relays down on two vantages is major; one vantage down alone is not", async () => {
  let cs = await minuteOf(13, { oracle: ["relay_india_1", "relay_india_2"] })
  cs = await minuteOf(14, { oracle: ["relay_india_1", "relay_india_2"] })
  cs = await minuteOf(15, { oracle: ["relay_india_1", "relay_india_2"] })
  assert.equal(state(cs, "relays"), "operational")
  const both = { oracle: ["relay_india_1", "relay_india_2"], anvit: ["relay_india_1", "relay_india_2"] }
  for (let i = 16; i < 19; i++) cs = await minuteOf(i, both)
  assert.equal(state(cs, "relays"), "major")
  for (let i = 19; i < 24; i++) cs = await minuteOf(i)
  assert.equal(state(cs, "relays"), "operational")
})

test("a silent probe host (no internet, results discarded) never turns anything red or green: data goes delayed after 3 minutes", async () => {
  let cs: Awaited<ReturnType<typeof minuteOf>> = []
  for (let i = 24; i < 30; i++) cs = await minuteOf(i, {}, ["oracle", "anvit", "cloudflare"])
  assert.ok(cs.every((c) => c.state === "delayed"))
  const stored = await db.prepare("SELECT COUNT(*) AS n FROM reports WHERE ts >= ?").bind(T0 + 24 * 60_000).first<{ n: number }>()
  assert.equal(stored!.n, 0)
})

test("a vantage whose own internet is down sends a heartbeat but its results are discarded", async () => {
  const now = T0 + 40 * 60_000
  await saveReport(db, "anvit", now, false, "test", report("anvit", ["api_health"]))
  const row = await db.prepare("SELECT net_ok, results FROM reports WHERE vantage = 'anvit' AND ts = ?").bind(minute(now)).first<{ net_ok: number; results: string }>()
  assert.deepEqual(row, { net_ok: 0, results: "{}" })
})

async function signed(vantage: string, key: string, body: object, tsOffset = 0) {
  const text = JSON.stringify({ vantage, ts: Date.now() + tsOffset, net_ok: true, results: [{ unit: "api_health", ok: true, ms: 12 }], ...body })
  const ts = JSON.parse(text).ts
  return new Request("https://status.test/api/probe", { method: "POST", body: text, headers: { "x-status-signature": await sign(key, `${vantage}\n${ts}\n${text}`) } })
}

test("probe writes need a valid signature for the vantage", async () => {
  assert.equal((await worker.fetch(await signed("oracle", "k-oracle", {}), env(), ctx)).status, 200)
  assert.equal((await worker.fetch(await signed("oracle", "wrong", {}), env(), ctx)).status, 401)
  assert.equal((await worker.fetch(await signed("anvit", "k-oracle", {}), env(), ctx)).status, 401)
  assert.equal((await worker.fetch(await signed("oracle", "k-oracle", {}, -600_000), env(), ctx)).status, 401) // replayed
  assert.equal((await worker.fetch(await signed("cloudflare", "k-oracle", {}), env(), ctx)).status, 401) // the Worker's own vantage cannot be written from outside
  const body = await signed("oracle", "k-oracle", {})
  const tampered = new Request(body.url, { method: "POST", headers: body.headers, body: (await body.text()).replace('"ok":true', '"ok":false') })
  assert.equal((await worker.fetch(tampered, env(), ctx)).status, 401)
})

test("public status is delayed when the snapshot is older than 3 minutes, never green", async () => {
  await buildSnapshot(db, Date.now() - 200_000)
  const res = (await (await worker.fetch(new Request("https://status.test/api/status"), env(), ctx)).json()) as { delayed: boolean }
  assert.equal(res.delayed, true)
  await buildSnapshot(db, Date.now())
  const fresh = (await (await worker.fetch(new Request("https://status.test/api/status"), env(), ctx)).json()) as { delayed: boolean }
  assert.equal(fresh.delayed, false)
})

const admin = (path: string, body?: object, token = "admin-secret") =>
  worker.fetch(new Request(`https://status.test${path}`, { method: body ? "POST" : "GET", headers: { authorization: `Bearer ${token}`, "content-type": "application/json" }, body: body && JSON.stringify(body) }), env(), ctx)

test("incidents: admin only, human text, updates, resolve, feed", async () => {
  assert.equal((await admin("/api/admin/state", undefined, "nope")).status, 401)
  assert.equal((await admin("/api/admin/incidents", { title: "x", body: "y", components: ["nope"] })).status, 400)
  const created = (await (await admin("/api/admin/incidents", { title: "Relay trouble", body: "We are looking into it.", components: ["relays"] })).json()) as { id: string; status: string }
  assert.equal(created.status, "investigating")
  const upd = await admin(`/api/admin/incidents/${created.id}/updates`, { status: "resolved", body: "Fixed." })
  assert.equal(upd.status, 200)
  assert.equal((await admin(`/api/admin/incidents/${created.id}/updates`, { status: "monitoring", body: "again" })).status, 409)
  const snap = (await readSnapshot(db))!
  assert.equal(snap.public.incidents[0].status, "resolved")
  assert.equal(snap.public.incidents[0].updates.length, 2)
  const feed = await (await worker.fetch(new Request("https://status.test/api/feed.xml"), { ...env(), PUBLIC_ORIGIN: "https://status.test" }, ctx)).text()
  assert.match(feed, /<title>\[resolved\] Relay trouble<\/title>/)
})

test("maintenance window hides downtime from the component and is not counted", async () => {
  const now = Date.now()
  const r = await admin("/api/admin/maintenance", { title: "Relay move", components: ["relays"], starts: now - 1000, ends: now + 3_600_000 })
  assert.equal(r.status, 201)
  const snap = await buildSnapshot(db, now)
  assert.equal(snap.public.components.find((c) => c.id === "relays")!.state, "maintenance")
  assert.equal((await admin("/api/admin/maintenance", { title: "bad", components: ["relays"], starts: now, ends: now - 1 })).status, 400)
})

test("subscribe is off without mail configured and never errors the page", async () => {
  const res = await worker.fetch(new Request("https://status.test/api/subscribe", { method: "POST", body: JSON.stringify({ email: "a@example.test" }) }), env(), ctx)
  assert.equal(res.status, 503)
})

test("catalog: unit ids are unique and every unit's vantages are declared", () => {
  const ids = allUnits.map((u) => u.id)
  assert.equal(new Set(ids).size, ids.length)
  for (const u of allUnits) for (const v of u.vantages) assert.ok(catalog.vantages.includes(v), `${u.id}: ${v}`)
})
