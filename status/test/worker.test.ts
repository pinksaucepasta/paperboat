// Integration: the real Worker code on a local D1 (wrangler's getPlatformProxy), time driven by hand.
import { after, before, test } from "node:test"
import assert from "node:assert/strict"
import { mkdtempSync, readFileSync } from "node:fs"
import { tmpdir } from "node:os"
import { join } from "node:path"
import { getPlatformProxy } from "wrangler"
import { handle } from "../lib/api.ts"
import { BACKOFF_MS, MAX_ABANDONED, deliverDue, validUrl } from "../lib/webhook.ts"
import { tick } from "../lib/tick.ts"
import { sign } from "../lib/probe.ts"
import { admitProbe, buildSnapshot, catalog, evaluate, minute, readSnapshot, saveReport, type Reported } from "../lib/store.ts"
import { uptimeBar, uptimePercent } from "../lib/uptime.ts"
import type { Env } from "../lib/types.ts"

let db: D1Database
let dispose: () => Promise<void>
const env = () => ({ DB: db, PROBE_KEYS: JSON.stringify({ oracle: "k-oracle", anvit: "k-anvit" }), ADMIN_TOKEN: "admin-secret" }) as Env
const ctx = { waitUntil() {} }
const call = (request: unknown, e: Env, c = ctx) => handle(request as Request, e, c)
const T0 = minute(Date.UTC(2026, 10, 18, 12, 0, 0))

before(async () => {
  const p = await getPlatformProxy<{ DB: D1Database }>({ configPath: new URL("../dev/wrangler.jsonc", import.meta.url).pathname, persist: { path: mkdtempSync(join(tmpdir(), "status-")) } })
  db = p.env.DB
  dispose = p.dispose
  for (const stmt of readFileSync(new URL("../schema.sql", import.meta.url), "utf8").replace(/^--.*$/gm, "").split(";").map((s) => s.trim()).filter(Boolean)) await db.prepare(stmt).run()
})
after(async () => dispose?.())

const allUnits = catalog.components.flatMap((c) => c.units)
/** every unit healthy except `bad` */
const report = (vantage: string, bad: string[] = [], ms = 100): Reported =>
  Object.fromEntries(allUnits.filter((u) => u.vantages.includes(vantage)).map((u) => [u.id, [bad.includes(u.id) ? 0 : 1, ms] as [number, number]]))
async function minuteOf(i: number, badBy: Record<string, string[]> = {}, silent: string[] = []) {
  const now = T0 + i * 60_000 + 5_000
  for (const v of catalog.vantages) if (!silent.includes(v)) await saveReport(db, v, now, now, true, "test", report(v, badBy[v] ?? []))
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
  const stored = await db.prepare("SELECT COUNT(*) AS n FROM reports WHERE bucket >= ?").bind(T0 + 24 * 60_000).first<{ n: number }>()
  assert.equal(stored!.n, 0)
})

test("a vantage whose own internet is down sends a heartbeat but its results are discarded", async () => {
  const now = T0 + 40 * 60_000
  await saveReport(db, "anvit", now, now, false, "test", report("anvit", ["api_health"]))
  const row = await db.prepare("SELECT net_ok, results FROM reports WHERE vantage = 'anvit' AND bucket = ?").bind(minute(now)).first<{ net_ok: number; results: string }>()
  assert.deepEqual(row, { net_ok: 0, results: "{}" })
})

async function signed(vantage: string, key: string, body: object, tsOffset = 0) {
  const text = JSON.stringify({ vantage, ts: Date.now() + tsOffset, net_ok: true, results: [{ unit: "api_health", ok: true, ms: 12 }], ...body })
  const ts = JSON.parse(text).ts
  return new Request("https://status.test/api/probe", { method: "POST", body: text, headers: { "x-status-signature": await sign(key, `${vantage}\n${ts}\n${text}`) } })
}

test("probe writes need a valid signature for the vantage", async () => {
  assert.equal((await call(await signed("oracle", "k-oracle", {}), env(), ctx)).status, 200)
  assert.equal((await call(await signed("oracle", "wrong", {}), env(), ctx)).status, 401)
  assert.equal((await call(await signed("anvit", "k-oracle", {}), env(), ctx)).status, 401)
  assert.equal((await call(await signed("oracle", "k-oracle", {}, -600_000), env(), ctx)).status, 401) // replayed
  assert.equal((await call(await signed("cloudflare", "k-oracle", {}), env(), ctx)).status, 401) // the Worker's own vantage cannot be written from outside
  const body = await signed("oracle", "k-oracle", {})
  const tampered = new Request(body.url, { method: "POST", headers: body.headers, body: (await body.text()).replace('"ok":true', '"ok":false') })
  assert.equal((await call(tampered, env(), ctx)).status, 401)
})

test("public status is delayed when the last decision is older than 3 minutes, never green", async () => {
  const stale = Date.now() - 200_000
  await saveReport(db, "oracle", stale, stale, true, "t", report("oracle"))
  await evaluate(db, stale)
  await buildSnapshot(db, Date.now())
  const res = (await (await call(new Request("https://status.test/api/status"), env())).json()) as { delayed: boolean }
  assert.equal(res.delayed, true)
  await evaluate(db, Date.now())
  await buildSnapshot(db, Date.now())
  const fresh = (await (await call(new Request("https://status.test/api/status"), env())).json()) as { delayed: boolean }
  assert.equal(fresh.delayed, false)
})

const admin = (path: string, body?: object, token = "admin-secret") =>
  call(new Request(`https://status.test${path}`, { method: body ? "POST" : "GET", headers: { authorization: `Bearer ${token}`, "content-type": "application/json" }, body: body && JSON.stringify(body) }), env(), ctx)

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
  const feed = await (await call(new Request("https://status.test/api/feed.xml"), { ...env(), PUBLIC_ORIGIN: "https://status.test" }, ctx)).text()
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
  const res = await call(new Request("https://status.test/api/subscribe", { method: "POST", body: JSON.stringify({ email: "a@example.test" }) }), env(), ctx)
  assert.equal(res.status, 503)
})

test("catalog: unit ids are unique and every unit's vantages are declared", () => {
  const ids = allUnits.map((u) => u.id)
  assert.equal(new Set(ids).size, ids.length)
  for (const u of allUnits) for (const v of u.vantages) assert.ok(catalog.vantages.includes(v), `${u.id}: ${v}`)
})

test("an admin write refreshes incident text but never makes old decisions look fresh", async () => {
  const old = Date.now() - 600_000
  await evaluate(db, old)
  await buildSnapshot(db, old)
  const r = await admin("/api/admin/incidents", { title: "Noted", body: "Looking.", components: ["api"] })
  assert.equal(r.status, 201)
  const res = (await (await call(new Request("https://status.test/api/status"), env())).json()) as { delayed: boolean; at: number }
  assert.equal(res.at, old)
  assert.equal(res.delayed, true)
})

test("freshness is judged on the unrounded receipt time", async () => {
  const base = minute(Date.UTC(2026, 10, 19, 3, 0, 0))
  for (let i = 4; i >= 0; i--) for (const v of ["oracle", "anvit"]) await saveReport(db, v, base - i * 60_000 + 59_000, base - i * 60_000 + 59_000, true, "t", report(v))
  await evaluate(db, base + 59_000 + 170_000) // 170 s after the last receipt (a floored timestamp would read 229 s)
  const comps = (await buildSnapshot(db, base + 229_000)).public.components
  assert.equal(comps.find((c) => c.id === "api")!.state, "operational")
  await evaluate(db, base + 59_000 + 200_000)
  assert.equal((await buildSnapshot(db, base + 260_000)).public.components.find((c) => c.id === "api")!.state, "delayed")
})

test("probe posts: a replay is refused and a second post within 20 s is rate limited", async () => {
  await db.prepare("DELETE FROM reports WHERE vantage = 'anvit'").run()
  const first = await signed("anvit", "k-anvit", {})
  const copy = first.clone()
  assert.equal((await call(first, env())).status, 200)
  assert.equal((await call(copy, env())).status, 409) // the same signed body again
  const quick = await signed("anvit", "k-anvit", { ts: Date.now() + 1000 })
  assert.equal((await call(quick, env())).status, 429)
})

test("cron: a malformed CHECKS secret does not stop the decision, and a retried minute counts once", async () => {
  const e = { ...env(), NET_CHECK_URL: "http://127.0.0.1:9/", CHECKS: "{not json" } as Env
  const t = Date.UTC(2026, 10, 20, 4, 0, 10)
  await tick(e, t)
  const count = async () => (await db.prepare("SELECT SUM(up + degraded + down + delayed) AS n FROM uptime_day WHERE day = '2026-11-20' AND component = 'api'").first<{ n: number | null }>())?.n ?? 0
  const once = await count()
  await tick(e, t + 20_000) // the same minute again (a cron retry)
  assert.equal(await count(), once)
  assert.equal(once, 1)
  await tick(e, t + 70_000)
  assert.equal(await count(), 2)
})

test("delayed minutes are their own bucket: not in the percentage, shown as unknown on a mostly unmeasured day", async () => {
  const t = Date.UTC(2026, 10, 21, 5, 0, 0)
  for (let i = 0; i < 12; i++) await evaluate(db, t + i * 60_000) // nothing reports: every minute is delayed
  const day = (await db.prepare("SELECT up, degraded, down, delayed FROM uptime_day WHERE day = '2026-11-21' AND component = 'relays'").first<{ up: number; degraded: number; down: number; delayed: number }>())!
  assert.equal(day.delayed, 12)
  assert.equal(day.up + day.down, 0)
  assert.equal(uptimeBar({ day: "d", ...day }), "unknown")
  assert.equal(uptimePercent([{ day: "d", ...day }]), null)
  assert.equal(uptimePercent([{ day: "d", up: 1400, degraded: 40, down: 0, delayed: 500 }]), 99.17)
  assert.equal(uptimePercent([{ day: "d", up: 120, degraded: 0, down: 0, delayed: 0 }]), null) // under three hours measured: no percentage
  assert.equal(uptimePercent([{ day: "d", up: 600, degraded: 0, down: 0, delayed: 0 }]), 100) // a few hours: the tracked period
})

test("subscribe: the daily quota is spent only by a confirmation mail that was sent", async () => {
  const mail = { ...env(), MAIL_ENDPOINT: "https://mail.example.test/emails", MAIL_KEY: "k", MAIL_FROM: "Status <s@example.test>", PUBLIC_ORIGIN: "https://status.test" } as Env
  const quota = async () => Number((await db.prepare("SELECT v FROM kv WHERE k LIKE 'sub:%'").first<{ v: string }>())?.v ?? 0)
  const post = (email: string) => call(new Request("https://status.test/api/subscribe", { method: "POST", body: JSON.stringify({ email }) }), mail)
  const realFetch = globalThis.fetch
  try {
    globalThis.fetch = (async () => new Response("down", { status: 503 })) as typeof fetch
    assert.equal((await post("one@example.test")).status, 502)
    assert.equal(await quota(), 0)
    globalThis.fetch = (async () => new Response("{}", { status: 200 })) as typeof fetch
    assert.equal((await post("two@example.test")).status, 200)
    assert.equal(await quota(), 1)
  } finally { globalThis.fetch = realFetch }
})

test("a snapshot build that started earlier never overwrites one that started later", async () => {
  const t = Date.now()
  const r = await admin("/api/admin/incidents", { title: "Newer", body: "Newest incident.", components: ["api"] })
  assert.equal(r.status, 201)
  await buildSnapshot(db, t, t + 10_000) // a newer build, with the incident
  await db.prepare("DELETE FROM incidents WHERE title = 'Newer'").run()
  await buildSnapshot(db, t, t + 5_000) // an older build that read after the delete must not win
  assert.ok((await readSnapshot(db))!.public.incidents.some((i) => i.title === "Newer"))
  await buildSnapshot(db, t, t + 20_000) // a later one replaces it
  assert.ok(!(await readSnapshot(db))!.public.incidents.some((i) => i.title === "Newer"))
})

test("an unreadable report row is skipped and the decision still runs", async () => {
  const t = Date.UTC(2026, 10, 22, 6, 0, 0)
  await db.prepare("INSERT INTO reports (vantage, bucket, ts, probe_ts, net_ok, build, results) VALUES ('oracle', ?, ?, 1, 1, 'x', '{not json')").bind(minute(t), t).run()
  await evaluate(db, t + 30_000)
  assert.equal(Number((await db.prepare("SELECT v FROM kv WHERE k = 'evaluated_at'").first<{ v: string }>())!.v), t + 30_000)
})

test("replay: an exact copy is refused, but a clock that ran ahead and came back does not lock the probe out", async () => {
  const t = Date.UTC(2026, 10, 22, 7, 0, 0)
  assert.equal(await admitProbe(db, "oracle", t, t + 90_000), "ok") // the probe's clock is 90 s fast
  await saveReport(db, "oracle", t, t + 90_000, true, "x", {})
  assert.equal(await admitProbe(db, "oracle", t + 30_000, t + 90_000), "replayed") // the same signed body again
  assert.equal(await admitProbe(db, "oracle", t + 60_000, t + 60_000), "ok") // corrected clock: older signed time, new data
})

test("two evaluations of the same minute at once count the uptime once", async () => {
  const t = Date.UTC(2026, 10, 23, 8, 0, 10)
  await Promise.all([evaluate(db, t), evaluate(db, t + 5000)])
  const n = (await db.prepare("SELECT SUM(up + degraded + down + delayed) AS n FROM uptime_day WHERE day = '2026-11-23' AND component = 'api'").first<{ n: number }>())!.n
  assert.equal(n, 1)
})

test("a unit never seen before is not green on its first healthy sample", async () => {
  await db.prepare("DELETE FROM unit_state WHERE unit = 'api_read'").run()
  const t = Date.UTC(2026, 10, 24, 9, 0, 0)
  for (const v of ["oracle", "anvit"]) await saveReport(db, v, t + 5_000, t + 5_000, true, "t", report(v))
  await evaluate(db, t + 10_000)
  assert.equal((await db.prepare("SELECT state FROM unit_state WHERE unit = 'api_read'").first<{ state: string }>())!.state, "delayed")
})

test("subscribe: a failed send does not make the retry look like 'already sent'", async () => {
  const mail = { ...env(), MAIL_ENDPOINT: "https://mail.example.test/emails", MAIL_KEY: "k", MAIL_FROM: "S <s@example.test>", PUBLIC_ORIGIN: "https://status.test" } as Env
  const post = () => call(new Request("https://status.test/api/subscribe", { method: "POST", body: JSON.stringify({ email: "retry@example.test" }) }), mail)
  const real = globalThis.fetch
  const sent: string[] = []
  try {
    globalThis.fetch = (async () => new Response("down", { status: 503 })) as typeof fetch
    assert.equal((await post()).status, 502)
    globalThis.fetch = (async (_u: unknown, init: RequestInit) => { sent.push(String(init.body)); return new Response("{}") }) as typeof fetch
    assert.equal((await post()).status, 200)
    assert.equal(sent.length, 1) // the retry really sent the mail
  } finally { globalThis.fetch = real }
})

test("confirm and unsubscribe links only act on POST (a mailbox scanner's GET changes nothing)", async () => {
  await db.prepare("INSERT OR REPLACE INTO subscribers (email, token, confirmed, created, last_mail) VALUES ('c@example.test', 'tok123', 0, 1, 0)").run()
  const get = await call(new Request("https://status.test/api/confirm?t=tok123"), env())
  assert.match(await get.text(), /<form method="post"/)
  assert.equal((await db.prepare("SELECT confirmed FROM subscribers WHERE token = 'tok123'").first<{ confirmed: number }>())!.confirmed, 0)
  const form = new URLSearchParams({ t: "tok123" })
  const post = await call(new Request("https://status.test/api/confirm", { method: "POST", body: form }), env())
  assert.match(await post.text(), /You will get an email/)
  assert.equal((await db.prepare("SELECT confirmed FROM subscribers WHERE token = 'tok123'").first<{ confirmed: number }>())!.confirmed, 1)
})

test("admin: a retried write with the same idempotency key creates one incident; mail failures are reported; no token means closed", async () => {
  const mail = { ...env(), MAIL_ENDPOINT: "https://mail.example.test/emails", MAIL_KEY: "k", MAIL_FROM: "S <s@example.test>", PUBLIC_ORIGIN: "https://status.test" } as Env
  await db.prepare("INSERT OR REPLACE INTO subscribers (email, token, confirmed, created, last_mail) VALUES ('m@example.test', 'tokm', 1, 1, 0)").run()
  const create = (headers: Record<string, string>) => call(new Request("https://status.test/api/admin/incidents", { method: "POST", headers: { authorization: "Bearer admin-secret", "content-type": "application/json", ...headers }, body: JSON.stringify({ title: "Once", body: "One incident.", components: ["api"] }) }), mail)
  const real = globalThis.fetch
  try {
    globalThis.fetch = (async () => new Response("down", { status: 500 })) as typeof fetch
    const first = (await (await create({ "idempotency-key": "key-0000001" })).json()) as { id: string; mail: { failed: number } }
    assert.ok(first.mail.failed >= 1) // every confirmed subscriber failed
    const again = (await (await create({ "idempotency-key": "key-0000001" })).json()) as { id: string; replayed?: boolean }
    assert.equal(again.id, first.id)
    assert.equal(again.replayed, true)
    assert.equal((await db.prepare("SELECT COUNT(*) AS n FROM incidents WHERE title = 'Once'").first<{ n: number }>())!.n, 1)
    assert.equal((await create({ "idempotency-key": "x" })).status, 400)
  } finally { globalThis.fetch = real }
  const closed = await call(new Request("https://status.test/api/admin/state", { headers: { authorization: "Bearer " } }), { ...env(), ADMIN_TOKEN: "" } as Env)
  assert.equal(closed.status, 503)
  const multiline = await admin("/api/admin/incidents", { title: "two\nlines", body: "x", components: ["api"] })
  assert.equal(multiline.status, 400)
})

// ---- webhooks
const hook = (path: string, body: object) => call(new Request(`https://status.test/api/webhook/${path}`, { method: "POST", body: JSON.stringify(body) }), env())
type Call = { url: string; init: RequestInit }
/** Replace fetch for the webhook endpoint "https://hooks.example.test/x": answers the challenge, then follows `mode`. */
function fakeEndpoint(mode: { ok: boolean }) {
  const calls: Call[] = []
  const real = globalThis.fetch
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    calls.push({ url: String(url), init })
    const body = JSON.parse(String(init.body))
    if (body.type === "verification") return new Response(JSON.stringify({ challenge: body.challenge }))
    return new Response("{}", { status: mode.ok ? 200 : 500 })
  }) as typeof fetch
  return { calls, restore: () => { globalThis.fetch = real } }
}

test("webhook urls: https on a named public host only", () => {
  assert.ok(validUrl("https://hooks.example.test/x"))
  for (const bad of ["http://hooks.example.test/x", "https://127.0.0.1/x", "https://localhost/x", "https://user:pw@hooks.example.test/x", "https://[::1]/x", "https://intranet/x", "ftp://a.test/x", 5, "x".repeat(600)]) assert.equal(validUrl(bad), null, String(bad))
})

test("webhook subscribe needs the challenge answered; deliveries are signed; retries back off; success clears the queue", async () => {
  const mode = { ok: false }
  const fake = fakeEndpoint(mode)
  try {
    const bad = await hook("subscribe", { url: "http://hooks.example.test/x" })
    assert.equal(bad.status, 400)
    const sub = (await (await hook("subscribe", { url: "https://hooks.example.test/x" })).json()) as { id: string; secret: string; unsubscribe_token: string }
    assert.match(sub.secret, /^whsec_[0-9a-f]{48}$/)
    assert.equal((await hook("subscribe", { url: "https://hooks.example.test/x" })).status, 409)
    // an incident: the first delivery is tried at once and fails (endpoint down)
    const t = Date.now()
    const created = await admin("/api/admin/incidents", { title: "Hook test", body: "Testing webhooks.", components: ["api"] })
    assert.equal(created.status, 201)
    const delivery = fake.calls.filter((c) => JSON.parse(String(c.init.body)).type === "incident.created")
    assert.equal(delivery.length, 1)
    const sent = delivery[0]
    const headers = sent.init.headers as Record<string, string>
    const [tPart, vPart] = headers["x-paperboat-signature"].split(",")
    const stamp = tPart.slice(2)
    assert.equal(vPart.slice(3), await sign(sub.secret, `${stamp}.${String(sent.init.body)}`)) // the receiver can verify the body
    let row = (await db.prepare("SELECT attempts, next_at FROM webhook_queue").first<{ attempts: number; next_at: number }>())!
    assert.equal(row.attempts, 1)
    assert.ok(row.next_at >= t + BACKOFF_MS[0] - 5000 && row.next_at <= Date.now() + BACKOFF_MS[0])
    // not due yet: nothing is sent; due and still down: the next backoff step
    const before = fake.calls.length
    await deliverDue(env(), t + 30_000)
    assert.equal(fake.calls.length, before)
    await deliverDue(env(), row.next_at + 1)
    row = (await db.prepare("SELECT attempts, next_at FROM webhook_queue").first<{ attempts: number; next_at: number }>())!
    assert.equal(row.attempts, 2)
    // the endpoint recovers: the same delivery id arrives and the queue empties
    mode.ok = true
    const firstId = (fake.calls[before].init.headers as Record<string, string>)["x-paperboat-delivery"]
    await deliverDue(env(), row.next_at + 1)
    assert.equal((fake.calls[fake.calls.length - 1].init.headers as Record<string, string>)["x-paperboat-delivery"], firstId)
    assert.equal((await db.prepare("SELECT COUNT(*) AS n FROM webhook_queue").first<{ n: number }>())!.n, 0)
    // unsubscribe by token
    assert.equal((await hook("unsubscribe", { token: "0".repeat(32) })).status, 404)
    assert.equal((await hook("unsubscribe", { token: sub.unsubscribe_token })).status, 200)
    assert.equal((await db.prepare("SELECT COUNT(*) AS n FROM webhooks").first<{ n: number }>())!.n, 0)
  } finally { fake.restore() }
})

test("a webhook that never answers is given up on after the last backoff step and removed after repeated give-ups", async () => {
  const mode = { ok: false }
  const fake = fakeEndpoint(mode)
  try {
    await hook("subscribe", { url: "https://dead.example.test/x" })
    let now = Date.now()
    for (let event = 0; event < MAX_ABANDONED; event++) {
      await admin("/api/admin/incidents", { title: `Dead ${event}`, body: "x", components: ["api"] })
      for (let step = 0; step <= BACKOFF_MS.length; step++) { now += 13 * 3600_000; await deliverDue(env(), now) } // every retry becomes due
    }
    assert.equal((await db.prepare("SELECT COUNT(*) AS n FROM webhooks WHERE url LIKE '%dead.example.test%'").first<{ n: number }>())!.n, 0)
    assert.equal((await db.prepare("SELECT COUNT(*) AS n FROM webhook_queue").first<{ n: number }>())!.n, 0)
  } finally { fake.restore() }
})

test("a webhook endpoint that does not answer the challenge is refused and nothing is stored", async () => {
  const real = globalThis.fetch
  try {
    globalThis.fetch = (async () => new Response("no", { status: 200 })) as typeof fetch
    assert.equal((await hook("subscribe", { url: "https://silent.example.test/x" })).status, 422)
    assert.equal((await db.prepare("SELECT COUNT(*) AS n FROM webhooks WHERE url LIKE '%silent%'").first<{ n: number }>())!.n, 0)
  } finally { globalThis.fetch = real }
})
